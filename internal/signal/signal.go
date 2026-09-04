package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/state"
)

// Exit codes. They are the contract of `factoryd signal`.
const (
	ExitDone    = 0
	ExitConfig  = 3 // config, identity, or the provider could not be asked
	ExitRefused = 5 // the gate refused: scope, audit, head moved, provider would not merge
	ExitUnknown = 6 // the provider claimed a merge the repository does not show; a human must look
)

// Error carries an exit code.
type Error struct {
	Exit int
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func refuse(format string, args ...any) error {
	return &Error{Exit: ExitRefused, Err: fmt.Errorf(format, args...)}
}

func failed(format string, args ...any) error {
	return &Error{Exit: ExitConfig, Err: fmt.Errorf(format, args...)}
}

// Request is one verdict.
type Request struct {
	ID      scm.ChangeID
	Kind    string // merged, changes-requested, operator-gated
	SHA     string // the head the verdict is about; "auto" means the head now
	Summary string
}

// Deps is what a signal needs beyond config.
type Deps struct {
	Driver   scm.Driver
	Reviewer scm.Identity
	Now      func() time.Time
	Log      io.Writer
}

// Result is what happened.
type Result struct {
	Verdict  state.Verdict
	Decision Decision
	// Audits are the audits found on the head, when the class required them.
	Audits []scm.Audit
	Path   string // the verdict file written
}

// Run records a verdict. For "merged" it is the merge gate: it classifies
// the change against the scope policy, requires a recorded audit on an
// escalate-class change, refuses an operator-only one, then merges with
// the expected head and verifies the merge by ancestry. Every verdict is
// written to outbox/<id>.json for the producer, recorded in the state
// document, and posted as a comment on the change. The summary must stand
// alone: the producer may be sandboxed and unable to read the comment.
func Run(ctx context.Context, cfg *config.Config, deps Deps, req Request) (Result, error) {
	now := time.Now
	if deps.Now != nil {
		now = deps.Now
	}
	log := deps.Log
	if log == nil {
		log = io.Discard
	}
	if !state.ValidVerdictKind(req.Kind) {
		return Result{}, failed("verdict %q is not one of merged, changes-requested, operator-gated", req.Kind)
	}
	if strings.TrimSpace(req.Summary) == "" {
		return Result{}, failed("summary is empty; a verdict the producer cannot act on is not a verdict")
	}
	if req.ID == "" {
		return Result{}, failed("change id is empty")
	}

	// 1. The change, now. The head the verdict is about must be the head
	// that exists: a verdict on a commit that has since been replaced is a
	// verdict on something the producer no longer has.
	change, err := deps.Driver.Get(ctx, req.ID)
	if err != nil {
		return Result{}, failed("reading change %s: %w", req.ID, err)
	}
	if change.State != scm.StateOpen {
		return Result{}, refuse("change %s is %s, not open", req.ID, change.State)
	}
	sha := req.SHA
	switch {
	case sha == "" || sha == "auto":
		sha = change.HeadSHA
	case sha != change.HeadSHA:
		return Result{}, refuse("change %s head is %s, not %s; the head moved since this verdict was formed", req.ID, change.HeadSHA, sha)
	}
	if sha == "" {
		return Result{}, failed("change %s reports no head sha", req.ID)
	}
	v := state.Verdict{ChangeID: string(req.ID), Kind: req.Kind, SHA: sha, Summary: req.Summary, At: now()}
	res := Result{Verdict: v}

	if req.Kind == state.VerdictMerged {
		// 2. Scope policy, on the diff of this head.
		diffs, err := deps.Driver.Diff(ctx, req.ID)
		if err != nil {
			return res, failed("reading the diff of %s: %w", req.ID, err)
		}
		if len(diffs) == 0 {
			return res, refuse("change %s has an empty diff; there is nothing to merge", req.ID)
		}
		res.Decision = Classify(&cfg.Scope, diffs)
		for _, r := range res.Decision.Reasons {
			fmt.Fprintf(log, "scope: %s\n", r)
		}
		switch res.Decision.Class {
		case OperatorOnly:
			// The verdict is not downgraded behind the reviewer's back: the
			// merge is refused, and the reviewer signals operator-gated
			// themselves, so the recorded verdict is always the one chosen.
			return res, refuse("scope policy makes %s operator-only; signal operator-gated instead:\n  %s", req.ID, strings.Join(res.Decision.Reasons, "\n  "))
		case Escalate:
			audits, err := deps.Driver.Audits(ctx, req.ID, sha)
			if err != nil {
				return res, failed("reading audits of %s at %s: %w", req.ID, sha, err)
			}
			res.Audits = audits
			if err := auditsClear(audits, sha); err != nil {
				return res, refuse("scope policy requires an adversarial audit of %s at %s: %v\n  %s", req.ID, sha, err, strings.Join(res.Decision.Reasons, "\n  "))
			}
			fmt.Fprintf(log, "audit: %d cleared on %s\n", len(audits), sha)
		}

		// 3. Ready, then merge with the expected head, then verify.
		if change.Draft {
			if err := deps.Driver.SetDraft(ctx, req.ID, false); err != nil {
				return res, failed("marking %s ready: %w", req.ID, err)
			}
		}
		mr, err := scm.MergeVerified(ctx, deps.Driver, req.ID, sha, cfg.TargetBranch)
		if err != nil {
			return res, failed("merging %s: %w", req.ID, err)
		}
		switch mr.Outcome {
		case scm.Merged:
			v.MergeCommit = mr.MergeCommit
			res.Verdict = v
		case scm.MergeUnknown:
			return res, &Error{Exit: ExitUnknown, Err: fmt.Errorf("merge of %s is UNKNOWN: %s", req.ID, mr.Reason)}
		default:
			return res, refuse("provider refused to merge %s: %s (%s)", req.ID, mr.Reason, mr.Outcome)
		}
	}

	// 4. Record: the verdict file the producer waits on, the state document,
	// the comment. File first -- it is the handoff (§6.2); the other two are
	// records of it.
	path, err := writeVerdict(cfg, v)
	if err != nil {
		return res, failed("writing the verdict: %w", err)
	}
	res.Path = path
	if _, err := state.Update(cfg.StatePath(), cfg.Name, func(s *state.State) error {
		s.LastVerdict = &v
		return nil
	}); err != nil {
		return res, failed("recording the verdict in state: %w", err)
	}
	if err := deps.Driver.Comment(ctx, req.ID, commentFor(v, deps.Reviewer)); err != nil {
		// The handoff is done and recorded; the comment is a courtesy to
		// humans reading the provider. Said, not swallowed.
		fmt.Fprintf(log, "WARNING: verdict recorded but the comment on %s failed: %v\n", req.ID, err)
	}
	fmt.Fprintf(log, "verdict: %s on %s at %s -> %s\n", v.Kind, v.ChangeID, v.SHA, path)
	return res, nil
}

// auditsClear decides whether the audits on a head satisfy an escalate
// class: at least one CLEARED audit pinned to this exact sha, and no BROKEN
// one. The driver already refused audits without attempts at the type
// boundary; this checks them again, because the gate is where it matters.
func auditsClear(audits []scm.Audit, sha string) error {
	cleared := 0
	for _, a := range audits {
		if a.SHA != sha {
			continue
		}
		if err := a.Validate(); err != nil {
			return fmt.Errorf("audit %q is not valid: %w", a.Lens, err)
		}
		switch a.Verdict {
		case scm.AuditBroken:
			return fmt.Errorf("audit %q found the change BROKEN", a.Lens)
		case scm.AuditCleared:
			cleared++
		}
	}
	if cleared == 0 {
		return errors.New("no CLEARED audit is recorded on this head")
	}
	return nil
}

func writeVerdict(cfg *config.Config, v state.Verdict) (string, error) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(cfg.OutboxDir(), v.ChangeID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

func commentFor(v state.Verdict, who scm.Identity) string {
	b := fmt.Sprintf("**Verdict: %s** (at %s", v.Kind, v.SHA)
	if v.MergeCommit != "" {
		b += ", merged as " + v.MergeCommit
	}
	b += ")\n\n" + v.Summary
	if who.Login != "" {
		b += "\n\n— factoryd signal, as " + who.Login
	}
	return b
}
