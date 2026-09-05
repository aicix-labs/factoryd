// Package refresh brings the producer's workdir to the target branch (#35).
//
// Nothing else does. The producer holds no provider credential and may run
// without network, so it cannot fetch; factoryd holds the credential but
// never runs git in a directory the producer can write (§4.4). The crossing
// is therefore a bundle: factoryd fetches the target into its own clone,
// writes a git bundle where the producer can read it, and a helper running
// AS THE PRODUCER, in the producer's own repository, fetches from the bundle
// and resets to it. Every write into the producer's workdir is the
// producer's own; every network and credential operation is factoryd's.
//
// A refresh runs before a producer turn when no change of the producer's is
// in flight, and as the operator's `factoryd refresh`. A stale workdir is
// the silent case this exists for: the brief describes the merged tree, the
// producer sees an older one, declares nothing, and the halt that follows
// looks like a model ignoring its brief.
package refresh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/state"
	"github.com/aicix-labs/factoryd/internal/supervise"
)

// Deps are the three sides of the crossing. Each is faked in tests.
type Deps struct {
	// Fetch brings refspec into the submit repository over the transport.
	Fetch func(ctx context.Context, refspec string) error
	// Bundle writes ref from the submit repository to path and returns the
	// sha the ref names: what the workdir must end up at.
	Bundle func(ctx context.Context, path, ref string) (sha string, err error)
	// Apply, as the producer, fetches branch from the bundle into the
	// workdir and resets to it, returning the workdir's HEAD afterwards.
	Apply func(ctx context.Context, bundle, branch string) (sha string, err error)
	// Lookup reads one change at the provider, for Reconcile. Nil means
	// no reconciliation: the cycle record is taken as it stands.
	Lookup func(ctx context.Context, id scm.ChangeID) (scm.Change, error)
	// Ancestor reports whether sha is reachable from ref at the provider.
	// Reconcile needs it: "merged" alone does not say where (#49 review).
	Ancestor func(ctx context.Context, sha, ref string) (bool, error)
}

// Result is what a refresh did.
type Result struct {
	// SHA is the workdir's HEAD after the refresh, verified equal to the
	// bundle's tip.
	SHA string
	// Note reports a reconciliation that happened on the way (#43).
	Note string
}

// Ref is where the transport's fetch leaves the target branch.
func Ref(cfg *config.Config) string { return "refs/remotes/factoryd/" + cfg.TargetBranch }

// Dir is the handoff directory the bundle is written to: under the factory
// root beside inbox/ and outbox/, readable by the producer, written by
// factoryd. It is not the inbox: a file appearing there is a trigger.
func Dir(cfg *config.Config) string { return filepath.Join(cfg.Paths.Root, "refresh") }

// BundlePath is the bundle for the target branch.
func BundlePath(cfg *config.Config) string {
	return filepath.Join(Dir(cfg), cfg.TargetBranch+".bundle")
}

// Run refreshes the producer's workdir to the target branch and verifies
// the result by sha, not by the helper's exit code: a helper that exited
// zero with the workdir somewhere else is the silent case.
func Run(ctx context.Context, cfg *config.Config, deps Deps) (Result, error) {
	if deps.Fetch == nil || deps.Bundle == nil || deps.Apply == nil {
		return Result{}, errors.New("refresh: incomplete dependencies")
	}
	if err := deps.Fetch(ctx, "+refs/heads/"+cfg.TargetBranch+":"+Ref(cfg)); err != nil {
		return Result{}, fmt.Errorf("refresh: fetching %s: %w", cfg.TargetBranch, err)
	}
	if err := os.MkdirAll(Dir(cfg), 0o755); err != nil {
		return Result{}, fmt.Errorf("refresh: %w", err)
	}
	path := BundlePath(cfg)
	want, err := deps.Bundle(ctx, path, Ref(cfg))
	if err != nil {
		return Result{}, fmt.Errorf("refresh: bundling %s: %w", Ref(cfg), err)
	}
	want = strings.TrimSpace(want)
	if want == "" {
		return Result{}, fmt.Errorf("refresh: bundle of %s names no commit", Ref(cfg))
	}
	got, err := deps.Apply(ctx, path, cfg.TargetBranch)
	if err != nil {
		return Result{}, fmt.Errorf("refresh: applying the bundle as the producer: %w", err)
	}
	got = strings.TrimSpace(got)
	if got != want {
		return Result{}, fmt.Errorf("refresh: the workdir is at %s after the refresh, not %s", short(got), short(want))
	}
	return Result{SHA: got}, nil
}

func short(s string) string {
	if s == "" {
		return "(nothing)"
	}
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// Decide says whether a refresh may run now, from the cycle record alone.
// Only the start of a cycle qualifies: CycleNew (nothing has touched the
// tree), CycleFinished (its draft merged) and CycleClean (it produced no
// change). Working, submitting and open
// name the producer's work; unknown is unknown. Absence is never "no
// draft": a nil cycle is unknown too.
func Decide(st *state.State) (run bool, reason string) {
	c := st.Cycle
	if c == nil {
		return false, "no cycle record; unknown authorizes nothing (`factoryd refresh --force` starts a new cycle)"
	}
	switch c.Phase {
	case state.CycleNew, state.CycleFinished, state.CycleClean:
		return true, ""
	case state.CycleWorking:
		return false, "cycle is working: a turn has edited this tree and no draft has merged"
	case state.CycleSubmitting:
		return false, fmt.Sprintf("cycle is submitting %s (%s): a draft may exist that state does not name yet", c.Family, short(c.Digest))
	case state.CycleOpen:
		return false, fmt.Sprintf("cycle is open: draft %s on %s has not merged", c.ChangeID, c.Family)
	default:
		note := c.Note
		if note == "" {
			note = "`factoryd refresh --force` starts a new cycle"
		}
		return false, "cycle is " + c.Phase + ": " + note
	}
}

// Reconcile brings an open cycle up to date with the provider (#43): a
// change the reviewer signalled operator-gated and a human then merged
// left the cycle open forever, refusing every refresh and never telling
// the producer its work landed. Nothing watches the provider; this asks
// at the moment a refresh is being decided, and only about the cycle's
// own change. "Merged" alone is not enough (#49 review): a draft opened
// for the configured target can be retargeted and merged elsewhere, and
// finishing the cycle on that would reset the producer's tree to a branch
// the work never reached. So merged finishes the cycle only when the
// change's target IS the configured target and its head is an ancestor
// of it at the provider. Closed without a merge ends the cycle too (the
// draft is gone; its branch is still at the provider). Anything else, a
// read that fails included, leaves the record as it is and says why.
func Reconcile(ctx context.Context, cfg *config.Config, st *state.State, lookup func(context.Context, scm.ChangeID) (scm.Change, error), ancestor func(context.Context, string, string) (bool, error), now time.Time) (changed bool, note string) {
	c := st.Cycle
	if lookup == nil || c == nil || c.Phase != state.CycleOpen || c.ChangeID == "" {
		return false, ""
	}
	ch, err := lookup(ctx, scm.ChangeID(c.ChangeID))
	if err != nil {
		return false, fmt.Sprintf("cycle left open: could not read change %s at the provider (%v)", c.ChangeID, err)
	}
	switch ch.State {
	case scm.StateMerged:
		if ch.TargetBranch != cfg.TargetBranch {
			return false, fmt.Sprintf("cycle left open: change %s is merged into %q, not the configured target %q; its work did not land here", c.ChangeID, ch.TargetBranch, cfg.TargetBranch)
		}
		if ancestor == nil || ch.HeadSHA == "" {
			return false, fmt.Sprintf("cycle left open: change %s is merged but its landing on %s cannot be verified", c.ChangeID, cfg.TargetBranch)
		}
		on, err := ancestor(ctx, ch.HeadSHA, cfg.TargetBranch)
		if err != nil {
			return false, fmt.Sprintf("cycle left open: could not verify change %s on %s (%v)", c.ChangeID, cfg.TargetBranch, err)
		}
		if !on {
			return false, fmt.Sprintf("cycle left open: change %s says merged but its head %s is not on %s", c.ChangeID, short(ch.HeadSHA), cfg.TargetBranch)
		}
		st.SetCycle(state.CycleFinished, now).Note = "change " + c.ChangeID + " found merged into " + cfg.TargetBranch + " at the provider (reconciled at refresh time)"
		return true, "cycle finished: change " + c.ChangeID + " is merged into " + cfg.TargetBranch + " at the provider"
	case scm.StateClosed:
		st.SetCycle(state.CycleFinished, now).Note = "change " + c.ChangeID + " found closed without a merge at the provider (reconciled at refresh time)"
		return true, "cycle finished: change " + c.ChangeID + " was closed without a merge at the provider"
	}
	return false, ""
}

// ErrRefused is returned by Guarded when the cycle, re-read under the
// lock, does not allow a refresh.
var ErrRefused = errors.New("refresh refused")

// Guarded is the one way a refresh is decided and applied: inside a single
// state update, under the document's exclusive lock, which the producer's
// turn start takes too (BeforeTurn). The cycle is re-read there, the
// refresh runs while the lock is held, and the new cycle is recorded
// before it is released -- so an operator's non-forced refresh and a
// producer turn that starts between decision and apply cannot interleave:
// whichever holds the lock first wins, and the other sees its record. A
// decision taken outside the lock is a decision about a tree someone else
// may be editing by the time it is acted on (#41 review). With force the
// cycle is not consulted; the previous phase is returned for the operator.
func Guarded(ctx context.Context, cfg *config.Config, deps Deps, force bool) (Result, string, error) {
	var r Result
	var prev string
	var reconciled string
	_, err := state.Update(cfg.StatePath(), cfg.Name, func(st *state.State) error {
		prev = state.CycleUnknown
		if st.Cycle != nil {
			prev = st.Cycle.Phase
		}
		if changed, note := Reconcile(ctx, cfg, st, deps.Lookup, deps.Ancestor, time.Now()); changed || note != "" {
			reconciled = note
		}
		if !force {
			if run, why := Decide(st); !run {
				return fmt.Errorf("%w: %s", ErrRefused, why)
			}
		}
		var err error
		if r, err = Run(ctx, cfg, deps); err != nil {
			return err
		}
		st.SetCycle(state.CycleNew, time.Now()).Base = r.SHA
		return nil
	})
	if reconciled != "" && err == nil {
		r.Note = reconciled
	}
	return r, prev, err
}

// BeforeTurn is the producer supervisor's before-turn step. Under the
// same lock as Guarded, it decides, refreshes when the cycle allows,
// records the new cycle at its base, and marks the cycle working -- all
// before the lock is released and the turn runs. So a turn that edits and
// fails before declaring leaves "working", its retry keeps the edits, and
// an operator refresh racing the turn start sees "working" and refuses.
func BeforeTurn(cfg *config.Config, mkDeps func(ctx context.Context) (Deps, error)) func(ctx context.Context, t supervise.Turn) (string, error) {
	return func(ctx context.Context, t supervise.Turn) (string, error) {
		var msg string
		_, err := state.Update(cfg.StatePath(), cfg.Name, func(st *state.State) error {
			// An open cycle is checked against the provider once before
			// the decision (#43): the deps are built for that read, and
			// reused for the refresh if one follows.
			var deps Deps
			var built bool
			if c := st.Cycle; c != nil && c.Phase == state.CycleOpen {
				d, err := mkDeps(ctx)
				if err != nil {
					return err
				}
				deps, built = d, true
				if _, note := Reconcile(ctx, cfg, st, deps.Lookup, deps.Ancestor, time.Now()); note != "" {
					msg = note + "; "
				}
			}
			run, why := Decide(st)
			msg += "workdir not refreshed: " + why
			if run {
				if !built {
					d, err := mkDeps(ctx)
					if err != nil {
						return err
					}
					deps = d
				}
				r, err := Run(ctx, cfg, deps)
				if err != nil {
					return err
				}
				st.SetCycle(state.CycleNew, time.Now()).Base = r.SHA
				msg = strings.TrimSuffix(msg, "workdir not refreshed: "+why) + fmt.Sprintf("workdir refreshed to %s at %s", cfg.TargetBranch, short(r.SHA))
			}
			// The turn is about to run: from here the tree is the producer's.
			if c := st.Cycle; c != nil && c.Phase == state.CycleNew {
				st.SetCycle(state.CycleWorking, time.Now())
			}
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("refresh: %w", err)
		}
		return msg, nil
	}
}

// QueueStart decides, refreshes, and reserves a queued producer cycle in one
// state-locked operation. It is intentionally separate from BeforeTurn: a
// queued brief behind an open draft must not create a synthetic failed turn
// merely to learn that it has to wait. More importantly, returning ready from
// a read-only check would leave a gap in which another root-side operation
// could open the cycle before factoryd takes the brief. The successful path
// therefore refreshes and records CycleWorking before it releases the lock.
// An open draft is reconciled at this boundary, so an operator merge releases
// the next queued brief without a new manual wake.
func QueueStart(cfg *config.Config, mkDeps func(ctx context.Context) (Deps, error)) func(ctx context.Context) (bool, string, error) {
	return func(ctx context.Context) (bool, string, error) {
		started := false
		var note string
		_, err := state.Update(cfg.StatePath(), cfg.Name, func(st *state.State) error {
			// An open cycle may have landed since the previous turn. Build the
			// dependencies once and reuse them for the refresh after a proved
			// merge, matching BeforeTurn's locked sequence.
			var deps Deps
			var built bool
			if c := st.Cycle; c != nil && c.Phase == state.CycleOpen {
				d, err := mkDeps(ctx)
				if err != nil {
					return err
				}
				deps, built = d, true
				if _, n := Reconcile(ctx, cfg, st, deps.Lookup, deps.Ancestor, time.Now()); n != "" {
					note = n
				}
			}
			var why string
			canStart, why := Decide(st)
			if !canStart {
				if note != "" {
					note += "; "
				}
				note += "brief queue waiting: " + why
				return nil
			}
			if !built {
				d, err := mkDeps(ctx)
				if err != nil {
					return err
				}
				deps = d
			}
			r, err := Run(ctx, cfg, deps)
			if err != nil {
				return err
			}
			st.SetCycle(state.CycleNew, time.Now()).Base = r.SHA
			// This is the reservation: the queued file has not moved yet, but
			// root-side lifecycle operations now see a producer-owned tree.
			st.SetCycle(state.CycleWorking, time.Now())
			started = true
			if note != "" {
				note += "; "
			}
			note += fmt.Sprintf("workdir refreshed to %s at %s", cfg.TargetBranch, short(r.SHA))
			return nil
		})
		if err != nil {
			return false, "", fmt.Errorf("refresh: starting the brief queue: %w", err)
		}
		return started, note, nil
	}
}
