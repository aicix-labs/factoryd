package scm

import (
	"context"
	"fmt"
)

// MergeOutcome is what a merge attempt actually did.
//
// v1's merge printed "Branch cannot be merged" and returned exit status 0; the
// caller inferred success from $?. That bug is unrepresentable here: the zero
// value is MergeUnknown, and MergeResult.Validate rejects any result claiming
// Merged without a commit.
type MergeOutcome int

const (
	// MergeUnknown is the zero value. It means the outcome could not be
	// established -- including the case where the provider claimed success but
	// verification did not confirm it. It is never "fine".
	MergeUnknown MergeOutcome = iota
	// Merged means the provider merged the change AND the resulting commit was
	// confirmed to be an ancestor of the target branch.
	Merged
	// RefusedDraft: the change is still a draft.
	RefusedDraft
	// RefusedPipeline: CI is not green.
	RefusedPipeline
	// RefusedConflict: the branch cannot be merged as-is, or the head moved.
	RefusedConflict
	// RefusedScope: scope policy forbids an automatic merge of these paths.
	RefusedScope
	// RefusedMissingAudit: an escalate-class change without a recorded
	// adversarial pass over its current head.
	RefusedMissingAudit
)

// AllMergeOutcomes is every declared outcome. The conformance suite and the
// exhaustiveness test walk it, so a new outcome added without handling shows up
// as a failure rather than as a silent default branch.
var AllMergeOutcomes = []MergeOutcome{
	MergeUnknown, Merged, RefusedDraft, RefusedPipeline,
	RefusedConflict, RefusedScope, RefusedMissingAudit,
}

func (o MergeOutcome) String() string {
	switch o {
	case Merged:
		return "merged"
	case RefusedDraft:
		return "refused-draft"
	case RefusedPipeline:
		return "refused-pipeline"
	case RefusedConflict:
		return "refused-conflict"
	case RefusedScope:
		return "refused-scope"
	case RefusedMissingAudit:
		return "refused-missing-audit"
	case MergeUnknown:
		return "unknown"
	}
	return fmt.Sprintf("MergeOutcome(%d)", int(o))
}

// ProviderMerge is what a provider said happened. It is deliberately a
// different type from MergeResult, and it is what Driver.Merge returns.
//
// A provider cannot attest that a commit landed on a branch -- only the
// repository can -- so the type a driver returns has no way to express
// verification. An unverified merge reported as verified is therefore not a bug
// to test for; it does not typecheck.
type ProviderMerge struct {
	Outcome MergeOutcome
	// MergeCommit is the commit the provider claims it created. Non-empty if
	// and only if Outcome is Merged.
	MergeCommit string
	// Reason is always populated on anything other than Merged.
	Reason string
}

// Validate enforces the shape invariants. Drivers call it before returning, and
// MergeVerified calls it on whatever a driver hands back, so a driver cannot
// ship a result that lies about its own shape.
func (p ProviderMerge) Validate() error {
	return validateShape(p.Outcome, p.MergeCommit, p.Reason)
}

// ProviderMerged is the result a driver returns when the provider merged. The
// commit is required: "merged, but I cannot say into what" is not a merge.
func ProviderMerged(commit string) ProviderMerge {
	return ProviderMerge{Outcome: Merged, MergeCommit: commit}
}

// RefusedByProvider builds a non-merge driver result. It exists so that no
// driver call site has to remember to populate Reason.
func RefusedByProvider(o MergeOutcome, format string, args ...any) ProviderMerge {
	return ProviderMerge{Outcome: o, Reason: fmt.Sprintf(format, args...)}
}

// MergeResult is a merge outcome that has been checked against the repository.
//
// verified is unexported and is set only by MergeVerified. Another package can
// still write MergeResult{Outcome: Merged, MergeCommit: "abc"}, but that value
// fails Validate, so a Merged result that nothing confirmed cannot be
// constructed anywhere outside this file.
type MergeResult struct {
	Outcome MergeOutcome
	// MergeCommit is non-empty if and only if Outcome is Merged, in which case
	// it has been confirmed to be an ancestor of the target branch.
	MergeCommit string
	// ClaimedCommit is the commit the provider said it created. It is retained
	// even when verification failed, because that sha is precisely what a human
	// investigating an Unknown has to go and look at.
	ClaimedCommit string
	// Reason is always populated on anything other than Merged.
	Reason string

	verified bool
}

// Verified reports that MergeCommit was confirmed to be an ancestor of the
// target branch, rather than merely reported by the provider API.
func (r MergeResult) Verified() bool { return r.verified }

// Validate enforces the invariants the type exists to hold, including the one
// its documentation claims: Merged means verified.
func (r MergeResult) Validate() error {
	if err := validateShape(r.Outcome, r.MergeCommit, r.Reason); err != nil {
		return err
	}
	if r.Outcome == Merged && !r.verified {
		return fmt.Errorf("merge result: Merged without verification; only MergeVerified can produce a Merged result")
	}
	if r.Outcome != Merged && r.verified {
		return fmt.Errorf("merge result: %v is marked verified", r.Outcome)
	}
	return nil
}

// validateShape holds the rules common to both result types.
func validateShape(o MergeOutcome, commit, reason string) error {
	switch o {
	case Merged:
		if commit == "" {
			return fmt.Errorf("merge result: Merged with no merge commit")
		}
	case MergeUnknown, RefusedDraft, RefusedPipeline, RefusedConflict, RefusedScope, RefusedMissingAudit:
		if commit != "" {
			return fmt.Errorf("merge result: %v carries a merge commit %q", o, commit)
		}
		if reason == "" {
			return fmt.Errorf("merge result: %v with no reason", o)
		}
	default:
		return fmt.Errorf("merge result: outcome %v is not a known outcome", o)
	}
	return nil
}

// Refused builds a non-merge result. It exists so that no call site has to
// remember to populate Reason. Gates outside this package use it to refuse a
// merge on scope or on a missing audit.
func Refused(o MergeOutcome, format string, args ...any) MergeResult {
	return MergeResult{Outcome: o, Reason: fmt.Sprintf(format, args...)}
}

// MergeVerified merges and then confirms the result against the repository.
//
// SPEC.md §5.1: "the API is not the authority on what landed". A provider that
// reports a merge commit which is not an ancestor of the target branch yields
// MergeUnknown -- not Merged, and not a refusal, because what happened is
// genuinely not known and a human has to look.
func MergeVerified(ctx context.Context, d Driver, id ChangeID, expectedHead, targetBranch string) (MergeResult, error) {
	if targetBranch == "" {
		return MergeResult{}, fmt.Errorf("merge %s: target branch is empty; there is nothing to verify against", id)
	}
	claim, err := d.Merge(ctx, id, expectedHead)
	if err != nil {
		return MergeResult{}, err
	}
	if verr := claim.Validate(); verr != nil {
		return MergeResult{}, fmt.Errorf("driver %s returned an invalid merge result: %w", d.Provider(), verr)
	}
	if claim.Outcome != Merged {
		return MergeResult{Outcome: claim.Outcome, Reason: claim.Reason}, nil
	}

	unverified := func(format string, args ...any) MergeResult {
		r := Refused(MergeUnknown, format, args...)
		// The claimed commit survives into a field, not just into prose: it is
		// what has to be checked by hand.
		r.ClaimedCommit = claim.MergeCommit
		return r
	}

	ok, err := d.IsAncestor(ctx, claim.MergeCommit, targetBranch)
	if err != nil {
		return unverified(
			"provider reported merge commit %s but ancestry against %s could not be checked: %v",
			claim.MergeCommit, targetBranch, err), nil
	}
	if !ok {
		return unverified(
			"provider reported merge commit %s but it is not an ancestor of %s; the merge did not land",
			claim.MergeCommit, targetBranch), nil
	}
	return MergeResult{
		Outcome:       Merged,
		MergeCommit:   claim.MergeCommit,
		ClaimedCommit: claim.MergeCommit,
		verified:      true,
	}, nil
}
