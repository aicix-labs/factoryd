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

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/state"
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

// InFlight reports whether the producer has a change that is not finished:
// its last submission has no merged verdict recorded. A refresh then would
// throw away the tree the producer is expected to iterate on. The reason is
// returned for the log and for `refresh` to print when it refuses.
func InFlight(st *state.State) (bool, string) {
	ls := st.LastSubmit
	if ls == nil {
		return false, ""
	}
	if v := st.LastVerdict; v != nil && v.ChangeID == ls.ChangeID && v.Kind == state.VerdictMerged {
		return false, ""
	}
	return true, fmt.Sprintf("change %s (%s), submitted %s, has no merged verdict", ls.ChangeID, ls.Branch, ls.At.Format("2006-01-02T15:04:05Z07:00"))
}
