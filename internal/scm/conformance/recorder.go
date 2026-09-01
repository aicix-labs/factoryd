package conformance

import (
	"context"
	"reflect"
	"sort"
	"sync"

	"github.com/aicix-labs/factoryd/internal/scm"
)

// recorder wraps a Driver and records which verbs a scenario actually calls.
//
// The suite's verb-coverage check used to compare scenarios against a
// hand-maintained list of method names. That check could only fail if someone
// edited the list, and the person adding a method had no reason to -- a check
// that cannot fail. Coverage is now observed here and compared against the
// interface itself via reflection, so neither side of the comparison is
// maintained by hand.
//
// recorder must implement scm.Driver, so a method added to the interface stops
// this file from compiling until it is forwarded. That is the intended
// pressure: the failure is at build time, and forwarding without recording
// would show up as an uncovered verb, which is the safe direction.
type recorder struct {
	inner scm.Driver

	mu   sync.Mutex
	seen map[string]bool
}

var _ scm.Driver = (*recorder)(nil)

func newRecorder(d scm.Driver) *recorder {
	return &recorder{inner: d, seen: map[string]bool{}}
}

func (r *recorder) note(verb string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen[verb] = true
}

// observed returns the verbs called so far.
func (r *recorder) observed() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]bool, len(r.seen))
	for k, v := range r.seen {
		out[k] = v
	}
	return out
}

func (r *recorder) Provider() string {
	r.note("Provider")
	return r.inner.Provider()
}

func (r *recorder) ListOpen(ctx context.Context) ([]scm.Change, error) {
	r.note("ListOpen")
	return r.inner.ListOpen(ctx)
}

func (r *recorder) Get(ctx context.Context, id scm.ChangeID) (scm.Change, error) {
	r.note("Get")
	return r.inner.Get(ctx, id)
}

func (r *recorder) Diff(ctx context.Context, id scm.ChangeID) ([]scm.FileDiff, error) {
	r.note("Diff")
	return r.inner.Diff(ctx, id)
}

func (r *recorder) Pipeline(ctx context.Context, id scm.ChangeID) (scm.PipelineStatus, error) {
	r.note("Pipeline")
	return r.inner.Pipeline(ctx, id)
}

func (r *recorder) Comment(ctx context.Context, id scm.ChangeID, body string) error {
	r.note("Comment")
	return r.inner.Comment(ctx, id, body)
}

func (r *recorder) SetDraft(ctx context.Context, id scm.ChangeID, draft bool) error {
	r.note("SetDraft")
	return r.inner.SetDraft(ctx, id, draft)
}

func (r *recorder) Merge(ctx context.Context, id scm.ChangeID, expectedHead string) (scm.ProviderMerge, error) {
	r.note("Merge")
	return r.inner.Merge(ctx, id, expectedHead)
}

func (r *recorder) IsAncestor(ctx context.Context, sha, ref string) (bool, error) {
	r.note("IsAncestor")
	return r.inner.IsAncestor(ctx, sha, ref)
}

func (r *recorder) PostAudit(ctx context.Context, id scm.ChangeID, sha string, a scm.Audit) error {
	r.note("PostAudit")
	return r.inner.PostAudit(ctx, id, sha, a)
}

func (r *recorder) Audits(ctx context.Context, id scm.ChangeID, sha string) ([]scm.Audit, error) {
	r.note("Audits")
	return r.inner.Audits(ctx, id, sha)
}

func (r *recorder) Whoami(ctx context.Context) (scm.Identity, error) {
	r.note("Whoami")
	return r.inner.Whoami(ctx)
}

// interfaceVerbs is every method of scm.Driver, read from the interface itself.
// Adding a method to Driver adds it here with no edit anywhere.
func interfaceVerbs() []string {
	t := reflect.TypeOf((*scm.Driver)(nil)).Elem()
	verbs := make([]string, 0, t.NumMethod())
	for i := range t.NumMethod() {
		verbs = append(verbs, t.Method(i).Name)
	}
	sort.Strings(verbs)
	return verbs
}
