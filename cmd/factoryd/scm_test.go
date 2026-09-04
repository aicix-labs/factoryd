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

// close is an operator's acknowledged act: refused without --operator,
// refused for anything that is not an open draft, and believed only when
// the re-read says closed -- merged in the window is reported as merged,
// never as success (#36, #47 review).
func TestScmCloseIsAnOperatorActAndVerifiesClosed(t *testing.T) {
	out := &printer{}
	run := func(d *closeDriver, args ...string) int { return scmVerb(context.Background(), d, "close", args, out) }

	d := &closeDriver{state: scm.StateOpen}
	if rc := run(d, "48"); rc != exitError || len(d.closes) != 0 {
		t.Fatalf("without --operator: rc=%d closes=%v; the reviewer protocol must not close", rc, d.closes)
	}
	if rc := run(d, "--operator", "48"); rc != exitOK || len(d.closes) != 1 || d.closes[0] != "superseded by a newer submission" {
		t.Fatalf("rc=%d closes=%v", rc, d.closes)
	}
	d = &closeDriver{state: scm.StateOpen}
	if rc := run(d, "--operator", "48", "duplicate", "of", "50"); rc != exitOK || d.closes[0] != "duplicate of 50" {
		t.Fatalf("rc=%d closes=%v", rc, d.closes)
	}

	for _, st := range []scm.ChangeState{scm.StateMerged, scm.StateClosed} {
		d := &closeDriver{state: st}
		if rc := run(d, "--operator", "48"); rc != exitError || len(d.closes) != 0 {
			t.Fatalf("%s: rc=%d closes=%v; only an open change is closed", st, rc, d.closes)
		}
	}
	d = &closeDriver{state: scm.StateOpen, ready: true}
	if rc := run(d, "--operator", "48"); rc != exitError || len(d.closes) != 0 {
		t.Fatalf("ready change: rc=%d closes=%v; a change that left the producer's hands is not a superseded draft", rc, d.closes)
	}
	d = &closeDriver{state: scm.StateOpen, after: scm.StateOpen}
	if rc := run(d, "--operator", "48"); rc != exitError {
		t.Fatal("the provider's word was taken as proof the change closed")
	}
	d = &closeDriver{state: scm.StateOpen, after: scm.StateMerged}
	if rc := run(d, "--operator", "48"); rc != exitError {
		t.Fatal("a change merged in the window was reported as retired")
	}
	d = &closeDriver{state: scm.StateOpen, closeErr: errors.New("403")}
	if rc := run(d, "--operator", "48"); rc != exitError {
		t.Fatal("a failed close exited 0")
	}
	if rc := run(&closeDriver{state: scm.StateOpen}, "--operator"); rc != exitError {
		t.Fatal("close with no id was accepted")
	}
}
