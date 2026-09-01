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

// MergeResult is the typed result of a merge attempt.
type MergeResult struct {
	Outcome MergeOutcome
	// MergeCommit is non-empty if and only if Outcome is Merged.
	MergeCommit string
	// Reason is always populated on anything other than Merged.
	Reason string
	// Verified records that MergeCommit was confirmed to be an ancestor of the
	// target branch, rather than merely reported by the provider API.
	Verified bool
}

// Validate enforces the invariants the type exists to hold. Drivers call it
// before returning, and the conformance suite calls it on every result, so a
// driver cannot ship a result shape that lies.
func (r MergeResult) Validate() error {
	switch r.Outcome {
	case Merged:
		if r.MergeCommit == "" {
			return fmt.Errorf("merge result: Merged with no merge commit")
		}
	case MergeUnknown, RefusedDraft, RefusedPipeline, RefusedConflict, RefusedScope, RefusedMissingAudit:
		if r.MergeCommit != "" {
			return fmt.Errorf("merge result: %v carries a merge commit %q", r.Outcome, r.MergeCommit)
		}
		if r.Reason == "" {
			return fmt.Errorf("merge result: %v with no reason", r.Outcome)
		}
	default:
		return fmt.Errorf("merge result: outcome %v is not a known outcome", r.Outcome)
	}
	return nil
}

// Refused builds a non-merge result. It exists so that no call site has to
// remember to populate Reason.
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
	res, err := d.Merge(ctx, id, expectedHead)
	if err != nil {
		return MergeResult{}, err
	}
	if verr := res.Validate(); verr != nil {
		return MergeResult{}, fmt.Errorf("driver %s returned an invalid merge result: %w", d.Provider(), verr)
	}
	if res.Outcome != Merged {
		return res, nil
	}

	ok, err := d.IsAncestor(ctx, res.MergeCommit, targetBranch)
	if err != nil {
		return Refused(MergeUnknown,
			"provider reported merge commit %s but ancestry against %s could not be checked: %v",
			res.MergeCommit, targetBranch, err), nil
	}
	if !ok {
		return Refused(MergeUnknown,
			"provider reported merge commit %s but it is not an ancestor of %s; the merge did not land",
			res.MergeCommit, targetBranch), nil
	}
	res.Verified = true
	return res, nil
}
