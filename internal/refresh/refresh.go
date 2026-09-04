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
}

// Result is what a refresh did.
type Result struct {
	// SHA is the workdir's HEAD after the refresh, verified equal to the
	// bundle's tip.
	SHA string
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

// BeforeTurn is the producer supervisor's before-turn step. It refreshes
// only when Decide allows, records the new cycle at its base, and marks
// the cycle working before the turn runs -- so a turn that edits and fails
// before declaring leaves "working", and its retry keeps the edits.
func BeforeTurn(cfg *config.Config, mkDeps func(ctx context.Context) (Deps, error)) func(ctx context.Context, t supervise.Turn) (string, error) {
	return func(ctx context.Context, t supervise.Turn) (string, error) {
		st, err := state.Load(cfg.StatePath(), cfg.Name)
		if err != nil {
			return "", fmt.Errorf("refresh: %w", err)
		}
		run, why := Decide(st)
		msg := "workdir not refreshed: " + why
		if run {
			deps, err := mkDeps(ctx)
			if err != nil {
				return "", fmt.Errorf("refresh: %w", err)
			}
			r, err := Run(ctx, cfg, deps)
			if err != nil {
				return "", err
			}
			if _, err := state.Update(cfg.StatePath(), cfg.Name, func(st *state.State) error {
				st.SetCycle(state.CycleNew, time.Now()).Base = r.SHA
				return nil
			}); err != nil {
				return "", fmt.Errorf("refresh: recording the new cycle: %w", err)
			}
			msg = fmt.Sprintf("workdir refreshed to %s at %s", cfg.TargetBranch, short(r.SHA))
		}
		// The turn is about to run: from here the tree is the producer's.
		if _, err := state.Update(cfg.StatePath(), cfg.Name, func(st *state.State) error {
			if c := st.Cycle; c != nil && c.Phase == state.CycleNew {
				st.SetCycle(state.CycleWorking, time.Now())
			}
			return nil
		}); err != nil {
			return "", fmt.Errorf("refresh: recording the working cycle: %w", err)
		}
		return msg, nil
	}
}

// Force runs a refresh regardless of the cycle and starts a new one: the
// operator's explicit acknowledgement that whatever the tree held is not
// wanted. The previous phase is returned for the operator to see.
func Force(ctx context.Context, cfg *config.Config, deps Deps) (Result, string, error) {
	st, err := state.Load(cfg.StatePath(), cfg.Name)
	if err != nil {
		return Result{}, "", err
	}
	prev := state.CycleUnknown
	if st.Cycle != nil {
		prev = st.Cycle.Phase
	}
	r, err := Run(ctx, cfg, deps)
	if err != nil {
		return Result{}, prev, err
	}
	if _, err := state.Update(cfg.StatePath(), cfg.Name, func(st *state.State) error {
		st.SetCycle(state.CycleNew, time.Now()).Base = r.SHA
		return nil
	}); err != nil {
		return Result{}, prev, fmt.Errorf("refresh: recording the new cycle: %w", err)
	}
	return r, prev, nil
}
