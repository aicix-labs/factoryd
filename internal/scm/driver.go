// Package scm defines the provider-agnostic contract every source-control
// driver must satisfy, and the typed results those operations return.
//
// The governing rule (SPEC.md §5.1) is that the zero value of every outcome
// enum means "unknown", never "fine". A caller that forgets to inspect an
// outcome gets an unusable value, not a false green.
package scm

import (
	"context"
	"fmt"
	"time"
)

// ChangeID is a provider-native identifier for a pull request or merge
// request. It is the number as a string ("42"), not a URL and not a global id.
type ChangeID string

// ChangeState is the lifecycle state of a change.
type ChangeState int

const (
	// StateUnknown is the zero value. It is never a valid observed state; a
	// driver returning it has failed to map the provider's response.
	StateUnknown ChangeState = iota
	StateOpen
	StateMerged
	StateClosed
)

func (s ChangeState) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateMerged:
		return "merged"
	case StateClosed:
		return "closed"
	case StateUnknown:
		return "unknown"
	}
	return fmt.Sprintf("ChangeState(%d)", int(s))
}

// Change is one pull request or merge request.
type Change struct {
	ID           ChangeID
	Title        string
	Author       string
	SourceBranch string
	TargetBranch string
	HeadSHA      string
	Draft        bool
	State        ChangeState
	WebURL       string
	Labels       []string
	UpdatedAt    time.Time
}

// FileDiff is one file's change within a Change.
type FileDiff struct {
	Path    string
	OldPath string // set only on a rename
	Added   int
	Removed int
	New     bool
	Deleted bool
	Renamed bool
	Patch   string
}

// PipelineState is the CI verdict for a commit.
//
// The distinction between PipelineNone and PipelineSuccess is load-bearing:
// a commit with no pipeline at all must never read as green. See SPEC.md §9.4
// ("ask what silence means").
type PipelineState int

const (
	// PipelineUnknown is the zero value and is never success.
	PipelineUnknown PipelineState = iota
	// PipelineNone means the provider reported zero pipelines/checks for the
	// commit. Nothing ran, so nothing passed.
	PipelineNone
	PipelinePending
	PipelineRunning
	PipelineSuccess
	PipelineFailed
	PipelineCanceled
)

func (p PipelineState) String() string {
	switch p {
	case PipelineNone:
		return "none"
	case PipelinePending:
		return "pending"
	case PipelineRunning:
		return "running"
	case PipelineSuccess:
		return "success"
	case PipelineFailed:
		return "failed"
	case PipelineCanceled:
		return "canceled"
	case PipelineUnknown:
		return "unknown"
	}
	return fmt.Sprintf("PipelineState(%d)", int(p))
}

// Green reports whether the pipeline is a positive result. It is deliberately
// a method rather than a comparison spread across call sites, so that
// PipelineNone can never be mistaken for PipelineSuccess by an inverted test.
func (p PipelineState) Green() bool { return p == PipelineSuccess }

// PipelineStatus is the CI status of one commit.
type PipelineStatus struct {
	State  PipelineState
	SHA    string
	WebURL string
	// Count is the number of pipelines or check runs the provider reported.
	// Zero with State == PipelineNone is the "nothing ran" case.
	Count int
}

// AuditVerdict is the outcome of one adversarial pass over a change.
type AuditVerdict int

const (
	// AuditUnknown is the zero value and never clears a change for merge.
	AuditUnknown AuditVerdict = iota
	// AuditCleared means the lens tried to break the change and failed to.
	AuditCleared
	// AuditBroken means the lens broke the change.
	AuditBroken
)

func (v AuditVerdict) String() string {
	switch v {
	case AuditCleared:
		return "CLEARED"
	case AuditBroken:
		return "BROKEN"
	case AuditUnknown:
		return "UNKNOWN"
	}
	return fmt.Sprintf("AuditVerdict(%d)", int(v))
}

// Audit is a recorded adversarial pass, pinned to one commit.
type Audit struct {
	Lens     string       `json:"lens"`
	Verdict  AuditVerdict `json:"-"`
	SHA      string       `json:"sha"`
	Attempts []string     `json:"attempts"`
	Notes    string       `json:"notes,omitempty"`
	PostedBy string       `json:"posted_by,omitempty"`
	PostedAt time.Time    `json:"posted_at,omitempty"`
}

// Validate enforces SPEC.md §6.4: an audit that records no attempts is not a
// pass. A CLEARED verdict with an empty Attempts list is the exact shape of a
// rubber stamp, so it is rejected at the type boundary rather than trusted.
func (a Audit) Validate() error {
	if a.Lens == "" {
		return fmt.Errorf("audit: lens is empty")
	}
	if a.SHA == "" {
		return fmt.Errorf("audit: sha is empty; an audit not pinned to a commit proves nothing")
	}
	switch a.Verdict {
	case AuditCleared:
		if len(a.Attempts) == 0 {
			return fmt.Errorf("audit %q: CLEARED with no attempts recorded; an audit that lists nothing tried is not a pass", a.Lens)
		}
	case AuditBroken:
		if len(a.Attempts) == 0 {
			return fmt.Errorf("audit %q: BROKEN with no attempts recorded", a.Lens)
		}
	case AuditUnknown:
		return fmt.Errorf("audit %q: verdict is UNKNOWN", a.Lens)
	default:
		return fmt.Errorf("audit %q: verdict %v is not a known verdict", a.Lens, a.Verdict)
	}
	for i, at := range a.Attempts {
		if at == "" {
			return fmt.Errorf("audit %q: attempt %d is empty", a.Lens, i)
		}
	}
	return nil
}

// Identity is who a credential resolves to on the provider.
type Identity struct {
	// ID is the provider's stable numeric or opaque id. Distinctness of the
	// producer and reviewer credentials is decided on this field, never on
	// the display name, which is not unique.
	ID    string
	Login string
	Name  string
}

func (i Identity) String() string {
	if i.Login == "" && i.ID == "" {
		return "<unresolved identity>"
	}
	return fmt.Sprintf("%s (id %s)", i.Login, i.ID)
}

// Driver is the whole provider surface the factory uses. Both the GitHub and
// GitLab implementations must pass one shared conformance suite
// (internal/scm/conformance): in v1 the two drivers diverged, and verbs that
// existed only on one side silently made half the factory single-provider.
type Driver interface {
	// Provider names the implementation ("github", "gitlab"). Used in
	// diagnostics and by the conformance suite for fixture lookup.
	Provider() string

	ListOpen(ctx context.Context) ([]Change, error)
	Get(ctx context.Context, id ChangeID) (Change, error)
	Diff(ctx context.Context, id ChangeID) ([]FileDiff, error)
	Pipeline(ctx context.Context, id ChangeID) (PipelineStatus, error)
	Comment(ctx context.Context, id ChangeID, body string) error
	SetDraft(ctx context.Context, id ChangeID, draft bool) error

	// Merge attempts the merge, pinned to expectedHead. It reports what the
	// provider said; it never returns Merged on a response that did not merge.
	//
	// It returns ProviderMerge, not MergeResult, because a provider cannot
	// attest that a commit landed on a branch. Only MergeVerified, having
	// checked ancestry, can produce a Merged MergeResult.
	Merge(ctx context.Context, id ChangeID, expectedHead string) (ProviderMerge, error)

	// IsAncestor reports whether sha is reachable from ref. This is how a
	// reported merge is verified against the repository rather than against
	// the provider's own word (SPEC.md §5.1).
	IsAncestor(ctx context.Context, sha, ref string) (bool, error)

	PostAudit(ctx context.Context, id ChangeID, sha string, a Audit) error
	Audits(ctx context.Context, id ChangeID, sha string) ([]Audit, error)

	Whoami(ctx context.Context) (Identity, error)

	// WhoamiWith resolves the identity of an arbitrary secret, so the git
	// transport can ask "whose token did git just resolve?" of the API rather
	// than of configuration (SPEC.md §5.4). No I/O beyond the one request.
	WhoamiWith(ctx context.Context, secret string) (Identity, error)

	// FindOpenBySource returns the open change whose source branch is
	// branch, if any. Submit is idempotent on it: a second run for the same
	// branch updates the existing draft rather than opening a duplicate.
	FindOpenBySource(ctx context.Context, branch string) (Change, bool, error)

	// OpenDraft opens a Draft change from source into target. The change is
	// created as a draft -- never ready -- because marking ready is the
	// reviewer's act, not the producer's (SPEC.md §2).
	OpenDraft(ctx context.Context, d DraftSpec) (Change, error)

	// GitCredential is what git is handed over HTTPS for this provider. The
	// username half is provider-owned: GitHub and GitLab do not agree on it,
	// so it cannot be derived from the token alone. Pure; no I/O.
	GitCredential(secret string) GitCredential
}

// DraftSpec is what OpenDraft needs.
type DraftSpec struct {
	SourceBranch string
	TargetBranch string
	Title        string
	Body         string
}

// GitCredential is a username/secret pair for git's credential protocol.
type GitCredential struct {
	Username string
	Secret   string
}
