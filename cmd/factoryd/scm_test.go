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
	state     scm.ChangeState
	closes    []string // reasons passed to Close
	closeErr  error
	afterOpen bool // the provider says closed but the re-read still shows open
}

func (d *closeDriver) Get(context.Context, scm.ChangeID) (scm.Change, error) {
	return scm.Change{ID: "48", State: d.state}, nil
}

func (d *closeDriver) Close(_ context.Context, _ scm.ChangeID, reason string) error {
	d.closes = append(d.closes, reason)
	if d.closeErr != nil {
		return d.closeErr
	}
	if !d.afterOpen {
		d.state = scm.StateClosed
	}
	return nil
}

// close retires only an open change, with a reason, and believes the
// provider only after re-reading the change (#36).
func TestScmCloseGuardsAndVerifies(t *testing.T) {
	out := &printer{}
	d := &closeDriver{state: scm.StateOpen}
	if rc := scmVerb(context.Background(), d, "close", []string{"48"}, out); rc != exitOK {
		t.Fatalf("rc=%d", rc)
	}
	if len(d.closes) != 1 || d.closes[0] != "superseded by a newer submission" {
		t.Fatalf("closes=%v", d.closes)
	}
	d = &closeDriver{state: scm.StateOpen}
	if rc := scmVerb(context.Background(), d, "close", []string{"48", "duplicate", "of", "50"}, out); rc != exitOK || d.closes[0] != "duplicate of 50" {
		t.Fatalf("rc=%d closes=%v", rc, d.closes)
	}

	for _, st := range []scm.ChangeState{scm.StateMerged, scm.StateClosed} {
		d := &closeDriver{state: st}
		if rc := scmVerb(context.Background(), d, "close", []string{"48"}, out); rc != exitError || len(d.closes) != 0 {
			t.Fatalf("%s: rc=%d closes=%v; only an open change is closed", st, rc, d.closes)
		}
	}
	d = &closeDriver{state: scm.StateOpen, afterOpen: true}
	if rc := scmVerb(context.Background(), d, "close", []string{"48"}, out); rc != exitError {
		t.Fatal("the provider's word was taken as proof the change closed")
	}
	d = &closeDriver{state: scm.StateOpen, closeErr: errors.New("403")}
	if rc := scmVerb(context.Background(), d, "close", []string{"48"}, out); rc != exitError {
		t.Fatal("a failed close exited 0")
	}
	if rc := scmVerb(context.Background(), &closeDriver{state: scm.StateOpen}, "close", nil, out); rc != exitError {
		t.Fatal("close with no id was accepted")
	}
}
