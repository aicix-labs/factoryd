package main

import (
	"context"
	"errors"
	"testing"

	"github.com/aicix-labs/factoryd/internal/scm"
)

// closeDriver is the provider as the close verb sees it.
type closeDriver struct {
	scm.Driver
	name     string
	state    scm.ChangeState
	ready    bool     // the change is no longer a draft
	closes   []string // reasons passed to Close
	closeErr error
	// after is the state the re-read shows once Close returned: closed by
	// default; open when the provider lied; merged when another party
	// merged in the window the close cannot exclude.
	after scm.ChangeState
}

func (d *closeDriver) Get(context.Context, scm.ChangeID) (scm.Change, error) {
	return scm.Change{ID: "48", State: d.state, Draft: !d.ready}, nil
}

func (d *closeDriver) Close(_ context.Context, _ scm.ChangeID, reason string) error {
	d.closes = append(d.closes, reason)
	if d.closeErr != nil {
		return d.closeErr
	}
	d.state = scm.StateClosed
	if d.after != 0 {
		d.state = d.after
	}
	return nil
}

// close reaches Driver.Close only through the operator principal: the
// role's own driver -- the one the reviewer turn holds -- never receives
// the call, whatever arguments are passed (#36, #47 review). And it
// closes only an open draft, believing only a re-read that says closed.
func TestScmCloseRunsOnlyAsTheOperatorPrincipal(t *testing.T) {
	out := &printer{}
	role := &closeDriver{name: "reviewer", state: scm.StateOpen}
	t.Cleanup(func() {
		if len(role.closes) != 0 {
			t.Errorf("the role's driver received Close(%v); the reviewer closed", role.closes)
		}
	})
	noOperator := func(context.Context) (scm.Driver, error) {
		return nil, errors.New("credentials.operator is not configured")
	}
	// Without an operator principal nothing closes, whatever is passed.
	for _, args := range [][]string{{"48"}, {"--operator", "48"}, {"48", "--operator"}, {"48", "reason"}} {
		if rc := scmVerb(context.Background(), role, noOperator, "close", args, out); rc != exitError {
			t.Fatalf("args %v: rc=%d; without the operator credential close must refuse", args, rc)
		}
	}
	if rc := scmVerb(context.Background(), role, nil, "close", []string{"48"}, out); rc != exitError {
		t.Fatal("no operator seam at all, yet close proceeded")
	}

	op := &closeDriver{name: "operator", state: scm.StateOpen}
	asOperator := func(context.Context) (scm.Driver, error) { return op, nil }
	run := func(o *closeDriver, args ...string) int {
		op = o
		return scmVerb(context.Background(), role, asOperator, "close", args, out)
	}
	if rc := run(op, "48"); rc != exitOK || len(op.closes) != 1 || op.closes[0] != "superseded by a newer submission" {
		t.Fatalf("rc=%d closes=%v", rc, op.closes)
	}
	o := &closeDriver{state: scm.StateOpen}
	if rc := run(o, "48", "duplicate", "of", "50"); rc != exitOK || o.closes[0] != "duplicate of 50" {
		t.Fatalf("rc=%d closes=%v", rc, o.closes)
	}
	for _, st := range []scm.ChangeState{scm.StateMerged, scm.StateClosed} {
		o := &closeDriver{state: st}
		if rc := run(o, "48"); rc != exitError || len(o.closes) != 0 {
			t.Fatalf("%s: rc=%d closes=%v; only an open change is closed", st, rc, o.closes)
		}
	}
	o = &closeDriver{state: scm.StateOpen, ready: true}
	if rc := run(o, "48"); rc != exitError || len(o.closes) != 0 {
		t.Fatalf("ready change: rc=%d closes=%v; a change that left the producer's hands is not a superseded draft", rc, o.closes)
	}
	if rc := run(&closeDriver{state: scm.StateOpen, after: scm.StateOpen}, "48"); rc != exitError {
		t.Fatal("the provider's word was taken as proof the change closed")
	}
	if rc := run(&closeDriver{state: scm.StateOpen, after: scm.StateMerged}, "48"); rc != exitError {
		t.Fatal("a change merged in the window was reported as retired")
	}
	if rc := run(&closeDriver{state: scm.StateOpen, closeErr: errors.New("403")}, "48"); rc != exitError {
		t.Fatal("a failed close exited 0")
	}
	if rc := run(&closeDriver{state: scm.StateOpen}); rc != exitError {
		t.Fatal("close with no id was accepted")
	}
}
