package scm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestZeroMergeResultIsNotSuccess pins the property the whole type exists for.
func TestZeroMergeResultIsNotSuccess(t *testing.T) {
	var r MergeResult
	if r.Outcome == Merged {
		t.Fatal("the zero MergeResult is Merged")
	}
	if r.Outcome != MergeUnknown {
		t.Fatalf("zero outcome = %v, want MergeUnknown", r.Outcome)
	}
	if r.Verified {
		t.Fatal("the zero MergeResult claims to be verified")
	}
	// A zero result must not pass validation either, or a caller could build
	// one by forgetting to set anything and have it accepted downstream.
	if err := r.Validate(); err == nil {
		t.Fatal("the zero MergeResult validated")
	}
}

func TestMergeResultValidate(t *testing.T) {
	cases := []struct {
		name    string
		r       MergeResult
		wantErr bool
	}{
		{"merged with commit", MergeResult{Outcome: Merged, MergeCommit: "abc"}, false},
		{"merged without commit", MergeResult{Outcome: Merged}, true},
		{"refusal with reason", Refused(RefusedDraft, "it is a draft"), false},
		{"refusal without reason", MergeResult{Outcome: RefusedDraft}, true},
		{"refusal carrying a commit", MergeResult{Outcome: RefusedConflict, Reason: "x", MergeCommit: "abc"}, true},
		{"outcome off the end of the enum", MergeResult{Outcome: MergeOutcome(99), Reason: "x"}, true},
	}
	for _, c := range cases {
		err := c.r.Validate()
		if (err != nil) != c.wantErr {
			t.Errorf("%s: Validate() = %v, wantErr %v", c.name, err, c.wantErr)
		}
	}
}

// TestAllMergeOutcomesAreNamed catches an outcome added to the enum without a
// String case, which would otherwise surface in an operator-facing reason as
// "MergeOutcome(7)".
func TestAllMergeOutcomesAreNamed(t *testing.T) {
	seen := map[string]bool{}
	for _, o := range AllMergeOutcomes {
		s := o.String()
		if strings.HasPrefix(s, "MergeOutcome(") {
			t.Errorf("outcome %d has no name", int(o))
		}
		if seen[s] {
			t.Errorf("two outcomes share the name %q", s)
		}
		seen[s] = true
	}
	// AllMergeOutcomes must actually be all of them.
	for i := 0; ; i++ {
		o := MergeOutcome(i)
		if strings.HasPrefix(o.String(), "MergeOutcome(") {
			if i != len(AllMergeOutcomes) {
				t.Errorf("AllMergeOutcomes has %d entries but the enum runs to %d", len(AllMergeOutcomes), i)
			}
			break
		}
		if i > 100 {
			t.Fatal("enum did not terminate")
		}
	}
}

// ---------- MergeVerified ----------

type fakeDriver struct {
	merge       MergeResult
	mergeErr    error
	ancestor    bool
	ancestErr   error
	ancestorHit int
}

func (f *fakeDriver) Provider() string { return "fake" }
func (f *fakeDriver) Merge(context.Context, ChangeID, string) (MergeResult, error) {
	return f.merge, f.mergeErr
}
func (f *fakeDriver) IsAncestor(context.Context, string, string) (bool, error) {
	f.ancestorHit++
	return f.ancestor, f.ancestErr
}

func (f *fakeDriver) ListOpen(context.Context) ([]Change, error)                 { panic("unused") }
func (f *fakeDriver) Get(context.Context, ChangeID) (Change, error)              { panic("unused") }
func (f *fakeDriver) Diff(context.Context, ChangeID) ([]FileDiff, error)         { panic("unused") }
func (f *fakeDriver) Pipeline(context.Context, ChangeID) (PipelineStatus, error) { panic("unused") }
func (f *fakeDriver) Comment(context.Context, ChangeID, string) error            { panic("unused") }
func (f *fakeDriver) SetDraft(context.Context, ChangeID, bool) error             { panic("unused") }
func (f *fakeDriver) PostAudit(context.Context, ChangeID, string, Audit) error   { panic("unused") }
func (f *fakeDriver) Audits(context.Context, ChangeID, string) ([]Audit, error)  { panic("unused") }
func (f *fakeDriver) Whoami(context.Context) (Identity, error)                   { panic("unused") }

func TestMergeVerified(t *testing.T) {
	ctx := context.Background()
	merged := MergeResult{Outcome: Merged, MergeCommit: "deadbeef"}

	t.Run("verified merge", func(t *testing.T) {
		d := &fakeDriver{merge: merged, ancestor: true}
		r, err := MergeVerified(ctx, d, "1", "head", "main")
		if err != nil {
			t.Fatal(err)
		}
		if r.Outcome != Merged || !r.Verified {
			t.Fatalf("got %v verified=%v, want merged and verified", r.Outcome, r.Verified)
		}
	})

	t.Run("merge commit not on the target branch", func(t *testing.T) {
		d := &fakeDriver{merge: merged, ancestor: false}
		r, err := MergeVerified(ctx, d, "1", "head", "main")
		if err != nil {
			t.Fatal(err)
		}
		if r.Outcome != MergeUnknown {
			t.Fatalf("outcome = %v, want unknown: the API said merged, the branch says otherwise", r.Outcome)
		}
		if r.MergeCommit != "" || r.Verified {
			t.Fatalf("unverified result carries commit %q verified=%v", r.MergeCommit, r.Verified)
		}
		if !strings.Contains(r.Reason, "not an ancestor") {
			t.Fatalf("reason = %q, want it to say the commit is not an ancestor", r.Reason)
		}
	})

	t.Run("ancestry check itself fails", func(t *testing.T) {
		d := &fakeDriver{merge: merged, ancestErr: errors.New("network down")}
		r, err := MergeVerified(ctx, d, "1", "head", "main")
		if err != nil {
			t.Fatal(err)
		}
		// Cannot verify is not the same as verified. It is also not a refusal.
		if r.Outcome != MergeUnknown {
			t.Fatalf("outcome = %v, want unknown", r.Outcome)
		}
	})

	t.Run("a refusal is not verified", func(t *testing.T) {
		d := &fakeDriver{merge: Refused(RefusedDraft, "it is a draft")}
		r, err := MergeVerified(ctx, d, "1", "head", "main")
		if err != nil {
			t.Fatal(err)
		}
		if r.Outcome != RefusedDraft {
			t.Fatalf("outcome = %v, want refused-draft", r.Outcome)
		}
		if d.ancestorHit != 0 {
			t.Fatal("ancestry was checked for a merge that never happened")
		}
	})

	t.Run("a driver returning an invalid result is an error", func(t *testing.T) {
		d := &fakeDriver{merge: MergeResult{Outcome: Merged}} // Merged, no commit
		if _, err := MergeVerified(ctx, d, "1", "head", "main"); err == nil {
			t.Fatal("accepted a Merged result with no merge commit")
		}
	})

	t.Run("no target branch is an error, not a skipped check", func(t *testing.T) {
		d := &fakeDriver{merge: merged, ancestor: true}
		if _, err := MergeVerified(ctx, d, "1", "head", ""); err == nil {
			t.Fatal("verified a merge against an empty target branch")
		}
	})
}
