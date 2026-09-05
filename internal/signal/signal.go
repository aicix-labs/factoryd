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
	"github.com/aicix-labs/factoryd/internal/submit"
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
	// Two-party trust, decided here, at the irreversible operation, on the
	// provider's stable ids -- not on logins, and not on a doctor result
	// that may be stale or a credential that may have been swapped since.
	// An author the provider did not name is not known to be distinct.
	switch {
	case deps.Reviewer.ID == "":
		return Result{}, failed("the reviewer identity has no provider id; distinctness cannot be established")
	case change.AuthorID == "":
		return Result{}, refuse("change %s has no author id from the provider; it cannot be shown to be someone else's work", req.ID)
	case change.AuthorID == deps.Reviewer.ID:
		return Result{}, refuse("change %s was authored by %s (id %s), the same identity signalling as reviewer; a reviewer never merges its own work", req.ID, change.Author, change.AuthorID)
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
	v := state.Verdict{ChangeID: string(req.ID), Kind: req.Kind, SHA: sha, Summary: req.Summary, At: now(),
		Branch: change.SourceBranch, DeclaredBranch: submit.DeclaredFamily(change.SourceBranch)}
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
		// Content policy is evaluated on delivered content only. A file the
		// provider did not deliver -- collapsed, too large, binary -- cannot
		// be evaluated, and is refused rather than read as "nothing added".
		for _, f := range diffs {
			if f.Incomplete && len(cfg.Scope.HoldDiffRegexes) > 0 {
				return res, refuse("scope policy cannot be evaluated on %s: the provider did not deliver its content (%s); review it by hand and signal operator-gated, or merge outside the gate", f.Path, f.IncompleteReason)
			}
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
			if err := auditsClear(audits, sha, deps.Reviewer, change, log); err != nil {
				return res, refuse("scope policy requires an adversarial audit of %s at %s: %v\n  %s", req.ID, sha, err, strings.Join(res.Decision.Reasons, "\n  "))
			}
			fmt.Fprintf(log, "audit: %d cleared on %s\n", len(audits), sha)
		}

		// 3. Merge with the expected head, then verify. The gate does NOT
		// mark a draft ready: no provider offers a ready mutation conditional
		// on the head, so a producer pushing between the check and the
		// mutation would have its unreviewed head marked ready by the gate
		// (the merge, being head-conditional, would then refuse -- but the
		// ready state would stand). Marking ready is the reviewer's own,
		// explicit act, done after review and before signalling.
		if change.Draft {
			return res, refuse("change %s is still a draft; the gate does not mark a draft ready. After review: factoryd scm --config <f> set-draft %s false, then signal", req.ID, req.ID)
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
		// The cycle finishes on the merge of ITS change, by id. A verdict on
		// any other change -- an older member of the family, an unrelated
		// draft -- says nothing about the producer's workdir (#35 review).
		// A cycle still "submitting" whose immutable branch this verdict
		// names is the same draft seen before submit recorded it: finish
		// it too, so either ordering converges (#41 review).
		if c := s.Cycle; c != nil && v.Kind == state.VerdictMerged &&
			((c.Phase == state.CycleOpen && c.ChangeID == v.ChangeID) ||
				(c.Phase == state.CycleSubmitting && v.Branch != "" && c.Digest == v.Branch)) {
			s.SetCycle(state.CycleFinished, now())
			s.Cycle.ChangeID = v.ChangeID
		}
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
// class: at least one CLEARED audit pinned to this exact sha, posted by the
// authenticated reviewer, and no BROKEN one by the reviewer. Authorship is
// the provider's word (the driver sets it from the comment's authenticated
// author). Audits by anyone else -- the change's author above all, whose
// audit of its own head is the forgery the requirement exists to prevent --
// are not counted and are named: they can neither clear a head nor veto
// it, because a producer's comment must not decide a merge either way. The
// driver already refused audits without attempts at the type boundary;
// this checks them again, because the gate is where it matters.
func auditsClear(audits []scm.Audit, sha string, reviewer scm.Identity, change scm.Change, log io.Writer) error {
	cleared := 0
	for _, a := range audits {
		if a.SHA != sha {
			continue
		}
		switch {
		case a.PostedByID == "":
			fmt.Fprintf(log, "audit: ignoring %q: no provider-authenticated author\n", a.Lens)
			continue
		case a.PostedByID == change.AuthorID:
			fmt.Fprintf(log, "audit: IGNORING %q posted by the change's own author %s; a producer does not audit its own head\n", a.Lens, a.PostedBy)
			continue
		case a.PostedByID != reviewer.ID:
			fmt.Fprintf(log, "audit: ignoring %q posted by %s (id %s), not the reviewer signalling now\n", a.Lens, a.PostedBy, a.PostedByID)
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
		return errors.New("no CLEARED audit by the reviewer is recorded on this head")
	}
	return nil
}

// writeVerdict writes the handoff document and REGISTERS it in state, by
// change id and digest, before the file lands: the outbox is the
// producer's to write, so the registry is the verdict's identity (#50
// review). An entry recorded and a write that then fails leaves a
// registered verdict with no file, which is a verdict nobody can act on
// and is reported; a file with no entry is not a verdict at all.
// Issue writes and registers a verdict document: the one way a verdict
// reaches the outbox, exported so tests issue registered verdicts the way
// the reviewer does rather than planting files the supervisor rightly
// refuses.
func Issue(cfg *config.Config, v state.Verdict) (string, error) { return writeVerdict(cfg, v) }

func writeVerdict(cfg *config.Config, v state.Verdict) (string, error) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	body = append(body, '\n')
	digest := state.DigestOf(body)
	if _, err := state.Update(cfg.StatePath(), cfg.Name, func(st *state.State) error {
		if err := st.VerdictRegistry.MigrationError(); err != nil {
			return err
		}
		if st.Issued == nil {
			st.Issued = map[string]state.IssuedVerdict{}
		}
		st.Issued[v.ChangeID] = state.IssuedVerdict{Kind: v.Kind, Branch: v.Branch, DeclaredBranch: v.DeclaredBranch, Digest: digest, RecordedBy: v.RecordedBy, IssuedAt: v.At}
		return nil
	}); err != nil {
		return "", fmt.Errorf("registering the verdict: %w", err)
	}
	path := filepath.Join(cfg.OutboxDir(), v.ChangeID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
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

// RecordMerged is the operator's verb for a change merged outside factoryd
// (#43): the reviewer signalled operator-gated, a human merged, and
// nothing told the factory. Nothing polls; the operator says so once, and
// the merge re-enters the protocol as a real verdict: verified at the
// provider first -- the change is merged and its head is on the target --
// then written to the outbox, which wakes the producer, recorded in state,
// and the cycle finished if the change is the cycle's. A change that is
// not merged is refused: this records what happened, it does not make it
// happen.
func RecordMerged(ctx context.Context, cfg *config.Config, d scm.Driver, id scm.ChangeID, summary string, now func() time.Time) (state.Verdict, string, error) {
	if now == nil {
		now = time.Now
	}
	if strings.TrimSpace(summary) == "" {
		summary = "merged by the operator outside factoryd"
	}
	change, err := d.Get(ctx, id)
	if err != nil {
		return state.Verdict{}, "", fmt.Errorf("reading change %s: %w", id, err)
	}
	if change.State != scm.StateMerged {
		return state.Verdict{}, "", fmt.Errorf("change %s is %s, not merged; factoryd verdict records a merge that happened, it does not perform one", id, change.State)
	}
	if change.HeadSHA == "" {
		return state.Verdict{}, "", fmt.Errorf("change %s reports no head sha; the merge cannot be verified", id)
	}
	// The configured target, not the change's: a draft opened for main can
	// be retargeted and merged into release, and that is not this
	// factory's cycle completing (#49 review).
	if change.TargetBranch != cfg.TargetBranch {
		return state.Verdict{}, "", fmt.Errorf("change %s targets %q, not this factory's target %q; a merge elsewhere is not this cycle's merge", id, change.TargetBranch, cfg.TargetBranch)
	}
	on, err := d.IsAncestor(ctx, change.HeadSHA, cfg.TargetBranch)
	if err != nil {
		return state.Verdict{}, "", fmt.Errorf("verifying %s on %s: %w", change.HeadSHA, cfg.TargetBranch, err)
	}
	if !on {
		return state.Verdict{}, "", fmt.Errorf("change %s says merged but its head %s is not on %s; not recording a merge that is not there", id, change.HeadSHA, cfg.TargetBranch)
	}
	v := state.Verdict{
		ChangeID: string(id), Kind: state.VerdictMerged, SHA: change.HeadSHA, Summary: summary, At: now(),
		MergeCommit: change.HeadSHA, Branch: change.SourceBranch, DeclaredBranch: state.FamilyOf(change.SourceBranch),
		RecordedBy: "operator",
	}
	path, err := writeVerdict(cfg, v)
	if err != nil {
		return state.Verdict{}, "", fmt.Errorf("writing the verdict: %w", err)
	}
	if _, err := state.Update(cfg.StatePath(), cfg.Name, func(s *state.State) error {
		s.LastVerdict = &v
		if c := s.Cycle; c != nil && ((c.Phase == state.CycleOpen && c.ChangeID == v.ChangeID) ||
			(c.Phase == state.CycleSubmitting && v.Branch != "" && c.Digest == v.Branch)) {
			s.SetCycle(state.CycleFinished, now())
			s.Cycle.ChangeID = v.ChangeID
		}
		return nil
	}); err != nil {
		return state.Verdict{}, "", fmt.Errorf("recording the verdict in state: %w", err)
	}
	return v, path, nil
}
