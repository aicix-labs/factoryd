// Package conformance is the one behaviour suite both provider drivers must
// pass.
//
// It exists because of a specific v1 failure: the GitHub and GitLab drivers
// drifted apart, five verbs the producer needed were implemented only on the
// GitHub side, and the producer daemon was silently GitHub-only for weeks.
// Nothing detected it, because each driver was tested against itself.
//
// The suite is written as a plain function returning results rather than
// against *testing.T, so that the suite itself can be tested: see
// control_test.go, which asserts the suite FAILS against deliberately broken
// drivers. A conformance suite nothing has ever been shown to reject is a check
// that cannot fail.
package conformance

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/scm/httpfixture"
)

// Fixture identifiers shared by every provider's recorded bundles. Both
// providers record the same story with the same values, so one assertion can
// hold for both.
const (
	ChangeID     = scm.ChangeID("42")
	TargetBranch = "main"
	HeadSHA      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	MergeSHA     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	StaleSHA     = "cccccccccccccccccccccccccccccccccccccccc"
	ReviewerName = "factory-reviewer"
	ProducerName = "producer-bot"
	// UnmergeableMessage is the literal GitLab message that v1 printed while
	// returning exit status 0.
	UnmergeableMessage = "Branch cannot be merged"
)

// Factory builds the driver under test, bound to a supplied HTTP client.
type Factory struct {
	// Provider is the value Driver.Provider() must return, and the name used
	// in failure messages.
	Provider string
	// FixtureDir holds <scenario>.json bundles.
	FixtureDir string
	// New builds the driver. It must not perform any I/O.
	New func(hc *http.Client) (scm.Driver, error)
}

// Result is one scenario's outcome. Err == nil means the scenario passed.
type Result struct {
	Scenario string
	Err      error
}

// driverVerbs is every method of scm.Driver. Check asserts the scenario set
// exercises all of them: a driver method that no scenario calls could be a stub
// returning "not implemented" and nothing would notice.
var driverVerbs = []string{
	"Provider", "ListOpen", "Get", "Diff", "Pipeline", "Comment", "SetDraft",
	"Merge", "IsAncestor", "PostAudit", "Audits", "Whoami",
}

type scenario struct {
	name string
	// verbs are the Driver methods this scenario exercises.
	verbs []string
	// denyHTTP runs the scenario against a transport that refuses every
	// request, asserting the driver decides locally.
	denyHTTP bool
	run      func(ctx context.Context, d scm.Driver) error
}

// Check runs every scenario against the driver f builds and returns one Result
// per scenario, in a stable order, plus results for the suite's own
// self-checks (verb coverage).
func Check(ctx context.Context, f Factory) []Result {
	var out []Result

	if err := checkVerbCoverage(); err != nil {
		out = append(out, Result{Scenario: "_verb_coverage", Err: err})
	}

	for _, s := range scenarios() {
		out = append(out, Result{Scenario: s.name, Err: runScenario(ctx, f, s)})
	}
	return out
}

// Failed reports the results that did not pass.
func Failed(rs []Result) []Result {
	var out []Result
	for _, r := range rs {
		if r.Err != nil {
			out = append(out, r)
		}
	}
	return out
}

func checkVerbCoverage() error {
	covered := map[string]bool{}
	for _, s := range scenarios() {
		for _, v := range s.verbs {
			covered[v] = true
		}
	}
	var missing []string
	for _, v := range driverVerbs {
		if !covered[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("no scenario exercises %s; an unexercised verb may be a stub on one provider and real on the other",
		strings.Join(missing, ", "))
}

func runScenario(ctx context.Context, f Factory, s scenario) error {
	var tr *httpfixture.Transport
	if s.denyHTTP {
		tr = httpfixture.Deny()
	} else {
		b, err := httpfixture.Load(f.FixtureDir, s.name)
		if err != nil {
			return fmt.Errorf("loading fixture: %w", err)
		}
		tr = httpfixture.NewTransport(b)
	}

	d, err := f.New(tr.Client())
	if err != nil {
		return fmt.Errorf("building driver: %w", err)
	}
	if got := d.Provider(); got != f.Provider {
		return fmt.Errorf("Provider() = %q, want %q", got, f.Provider)
	}

	if err := s.run(ctx, d); err != nil {
		return err
	}
	// Fixture accounting is part of the assertion, not bookkeeping: it catches
	// a driver that skipped a call the scenario depends on.
	return tr.Done()
}

func scenarios() []scenario {
	return []scenario{
		// Provider is asserted by runScenario for every scenario; it is
		// listed once so the coverage check accounts for it.
		{name: "whoami", verbs: []string{"Whoami", "Provider"}, run: scWhoami},
		{name: "list_open", verbs: []string{"ListOpen"}, run: scListOpen},
		{name: "get", verbs: []string{"Get"}, run: scGet},
		{name: "diff", verbs: []string{"Diff"}, run: scDiff},
		{name: "pipeline_success", verbs: []string{"Pipeline"}, run: scPipelineSuccess},
		{name: "pipeline_failed", verbs: []string{"Pipeline"}, run: scPipelineFailed},
		{name: "pipeline_none", verbs: []string{"Pipeline"}, run: scPipelineNone},
		{name: "comment", verbs: []string{"Comment"}, run: scComment},
		{name: "set_draft_ready", verbs: []string{"SetDraft"}, run: scSetDraftReady},
		{name: "merge_success", verbs: []string{"Merge", "IsAncestor"}, run: scMergeSuccess},
		{name: "merge_refused_draft", verbs: []string{"Merge"}, run: scMergeRefusedDraft},
		{name: "merge_refused_unmergeable", verbs: []string{"Merge"}, run: scMergeUnmergeable},
		{name: "merge_head_moved", verbs: []string{"Merge"}, run: scMergeHeadMoved},
		{name: "merge_reported_but_absent", verbs: []string{"Merge"}, run: scMergeReportedButAbsent},
		{name: "merge_unverified", verbs: []string{"Merge", "IsAncestor"}, run: scMergeUnverified},
		{name: "post_audit", verbs: []string{"PostAudit"}, run: scPostAudit},
		{name: "audits", verbs: []string{"Audits"}, run: scAudits},
		{name: "audit_requires_attempts", verbs: []string{"PostAudit"}, denyHTTP: true, run: scAuditRequiresAttempts},
	}
}

// ---------- scenarios ----------

func scWhoami(ctx context.Context, d scm.Driver) error {
	id, err := d.Whoami(ctx)
	if err != nil {
		return err
	}
	if id.ID == "" {
		return fmt.Errorf("Whoami returned no id; identity distinctness is decided on the id")
	}
	if id.Login != ReviewerName {
		return fmt.Errorf("Whoami login = %q, want %q", id.Login, ReviewerName)
	}
	return nil
}

func scListOpen(ctx context.Context, d scm.Driver) error {
	cs, err := d.ListOpen(ctx)
	if err != nil {
		return err
	}
	// The fixture is paginated one change per page. A driver that ignores
	// pagination returns one change and fails here.
	if len(cs) != 2 {
		return fmt.Errorf("ListOpen returned %d changes, want 2 (the fixture is paginated)", len(cs))
	}
	if cs[0].ID != ChangeID || cs[1].ID != "43" {
		return fmt.Errorf("ListOpen ids = %q, %q; want %q, %q", cs[0].ID, cs[1].ID, ChangeID, "43")
	}
	if cs[0].State != scm.StateOpen {
		return fmt.Errorf("ListOpen[0].State = %v, want open", cs[0].State)
	}
	if cs[0].Draft {
		return fmt.Errorf("ListOpen[0].Draft = true, want false")
	}
	if !cs[1].Draft {
		return fmt.Errorf("ListOpen[1].Draft = false, want true (change 43 is a draft in the fixture)")
	}
	return nil
}

func scGet(ctx context.Context, d scm.Driver) error {
	c, err := d.Get(ctx, ChangeID)
	if err != nil {
		return err
	}
	switch {
	case c.ID != ChangeID:
		return fmt.Errorf("Get.ID = %q, want %q", c.ID, ChangeID)
	case c.HeadSHA != HeadSHA:
		return fmt.Errorf("Get.HeadSHA = %q, want %q", c.HeadSHA, HeadSHA)
	case c.TargetBranch != TargetBranch:
		return fmt.Errorf("Get.TargetBranch = %q, want %q", c.TargetBranch, TargetBranch)
	case c.SourceBranch != "producer/fix-thing":
		return fmt.Errorf("Get.SourceBranch = %q, want %q", c.SourceBranch, "producer/fix-thing")
	case c.Author != ProducerName:
		return fmt.Errorf("Get.Author = %q, want %q", c.Author, ProducerName)
	case c.State != scm.StateOpen:
		return fmt.Errorf("Get.State = %v, want open", c.State)
	case c.Draft:
		return fmt.Errorf("Get.Draft = true, want false")
	case c.WebURL == "":
		return fmt.Errorf("Get.WebURL is empty")
	}
	return nil
}

func scDiff(ctx context.Context, d scm.Driver) error {
	fs, err := d.Diff(ctx, ChangeID)
	if err != nil {
		return err
	}
	if len(fs) != 2 {
		return fmt.Errorf("Diff returned %d files, want 2", len(fs))
	}
	a := fs[0]
	if a.Path != "internal/gate/scope.go" {
		return fmt.Errorf("Diff[0].Path = %q, want internal/gate/scope.go", a.Path)
	}
	if a.Added != 3 || a.Removed != 1 {
		return fmt.Errorf("Diff[0] added/removed = %d/%d, want 3/1", a.Added, a.Removed)
	}
	b := fs[1]
	if !b.Renamed {
		return fmt.Errorf("Diff[1].Renamed = false, want true")
	}
	if b.OldPath != "old/name.go" || b.Path != "new/name.go" {
		return fmt.Errorf("Diff[1] rename = %q -> %q, want old/name.go -> new/name.go", b.OldPath, b.Path)
	}
	return nil
}

func scPipelineSuccess(ctx context.Context, d scm.Driver) error {
	p, err := d.Pipeline(ctx, ChangeID)
	if err != nil {
		return err
	}
	if p.State != scm.PipelineSuccess || !p.State.Green() {
		return fmt.Errorf("Pipeline.State = %v, want success", p.State)
	}
	if p.SHA != HeadSHA {
		return fmt.Errorf("Pipeline.SHA = %q, want %q", p.SHA, HeadSHA)
	}
	if p.Count < 1 {
		return fmt.Errorf("Pipeline.Count = %d, want at least 1", p.Count)
	}
	return nil
}

func scPipelineFailed(ctx context.Context, d scm.Driver) error {
	p, err := d.Pipeline(ctx, ChangeID)
	if err != nil {
		return err
	}
	if p.State != scm.PipelineFailed {
		return fmt.Errorf("Pipeline.State = %v, want failed", p.State)
	}
	if p.State.Green() {
		return fmt.Errorf("a failed pipeline reported Green()")
	}
	return nil
}

func scPipelineNone(ctx context.Context, d scm.Driver) error {
	p, err := d.Pipeline(ctx, ChangeID)
	if err != nil {
		return err
	}
	// The whole point: nothing ran, so nothing passed. A driver that maps
	// "zero pipelines" to success fails here.
	if p.State != scm.PipelineNone {
		return fmt.Errorf("Pipeline.State = %v with no pipelines recorded, want none", p.State)
	}
	if p.State.Green() {
		return fmt.Errorf("a commit with no pipeline at all reported Green()")
	}
	if p.Count != 0 {
		return fmt.Errorf("Pipeline.Count = %d, want 0", p.Count)
	}
	return nil
}

func scComment(ctx context.Context, d scm.Driver) error {
	// The fixture asserts the body reached the provider; see
	// request_body_contains in comment.json.
	return d.Comment(ctx, ChangeID, "changes-requested: the guard matches the mention, not the command")
}

func scSetDraftReady(ctx context.Context, d scm.Driver) error {
	return d.SetDraft(ctx, ChangeID, false)
}

func scMergeSuccess(ctx context.Context, d scm.Driver) error {
	r, err := scm.MergeVerified(ctx, d, ChangeID, HeadSHA, TargetBranch)
	if err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return fmt.Errorf("invalid result: %w", err)
	}
	if r.Outcome != scm.Merged {
		return fmt.Errorf("Outcome = %v (%s), want merged", r.Outcome, r.Reason)
	}
	if r.MergeCommit != MergeSHA {
		return fmt.Errorf("MergeCommit = %q, want %q", r.MergeCommit, MergeSHA)
	}
	if !r.Verified {
		return fmt.Errorf("Verified = false; a merge reported by the API but not confirmed by ancestry is not a merge")
	}
	return nil
}

func refusal(r scm.MergeResult, want scm.MergeOutcome, reasonSubstr string) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("invalid result: %w", err)
	}
	if r.Outcome != want {
		return fmt.Errorf("Outcome = %v (reason %q), want %v", r.Outcome, r.Reason, want)
	}
	if r.MergeCommit != "" {
		return fmt.Errorf("Outcome %v carries merge commit %q", r.Outcome, r.MergeCommit)
	}
	if r.Verified {
		return fmt.Errorf("Outcome %v reported Verified = true", r.Outcome)
	}
	if reasonSubstr != "" && !strings.Contains(r.Reason, reasonSubstr) {
		return fmt.Errorf("Reason = %q, want it to mention %q", r.Reason, reasonSubstr)
	}
	return nil
}

func scMergeRefusedDraft(ctx context.Context, d scm.Driver) error {
	r, err := scm.MergeVerified(ctx, d, ChangeID, HeadSHA, TargetBranch)
	if err != nil {
		return err
	}
	return refusal(r, scm.RefusedDraft, "draft")
}

func scMergeUnmergeable(ctx context.Context, d scm.Driver) error {
	r, err := scm.MergeVerified(ctx, d, ChangeID, HeadSHA, TargetBranch)
	if err != nil {
		return err
	}
	// This is the v1 bug, reproduced: the provider says the branch cannot be
	// merged. v1 printed it and exited 0.
	return refusal(r, scm.RefusedConflict, UnmergeableMessage)
}

func scMergeHeadMoved(ctx context.Context, d scm.Driver) error {
	r, err := scm.MergeVerified(ctx, d, ChangeID, HeadSHA, TargetBranch)
	if err != nil {
		return err
	}
	return refusal(r, scm.RefusedConflict, "head moved")
}

func scMergeReportedButAbsent(ctx context.Context, d scm.Driver) error {
	r, err := scm.MergeVerified(ctx, d, ChangeID, HeadSHA, TargetBranch)
	if err != nil {
		return err
	}
	// HTTP 200 with a body that does not say "merged". The status code is not
	// the outcome.
	return refusal(r, scm.MergeUnknown, "")
}

func scMergeUnverified(ctx context.Context, d scm.Driver) error {
	r, err := scm.MergeVerified(ctx, d, ChangeID, HeadSHA, TargetBranch)
	if err != nil {
		return err
	}
	// The API reported a merge commit; the repository says it is not on the
	// target branch. The API is not the authority on what landed.
	return refusal(r, scm.MergeUnknown, "not an ancestor")
}

func scPostAudit(ctx context.Context, d scm.Driver) error {
	return d.PostAudit(ctx, ChangeID, HeadSHA, scm.Audit{
		Lens:    "scope-escape",
		Verdict: scm.AuditCleared,
		Attempts: []string{
			"reverted the guard and confirmed the new test goes red",
			"fed the guard a path that only mentions the pattern in a comment",
		},
		Notes: "no escape found",
	})
}

func scAudits(ctx context.Context, d scm.Driver) error {
	as, err := d.Audits(ctx, ChangeID, HeadSHA)
	if err != nil {
		return err
	}
	// The fixture holds three comments: a plain one, an audit pinned to a stale
	// sha, and an audit pinned to the head. Only the last one counts.
	if len(as) != 1 {
		return fmt.Errorf("Audits returned %d audits for the head sha, want 1 (the fixture also holds a plain comment and an audit of a stale sha)", len(as))
	}
	a := as[0]
	if a.Lens != "scope-escape" {
		return fmt.Errorf("Audits[0].Lens = %q, want scope-escape", a.Lens)
	}
	if a.Verdict != scm.AuditCleared {
		return fmt.Errorf("Audits[0].Verdict = %v, want CLEARED", a.Verdict)
	}
	if len(a.Attempts) != 2 {
		return fmt.Errorf("Audits[0] recorded %d attempts, want 2", len(a.Attempts))
	}
	if a.SHA != HeadSHA {
		return fmt.Errorf("Audits[0].SHA = %q, want %q", a.SHA, HeadSHA)
	}
	if a.PostedBy == "" {
		return fmt.Errorf("Audits[0].PostedBy is empty; an unattributed audit is not evidence")
	}
	return nil
}

func scAuditRequiresAttempts(ctx context.Context, d scm.Driver) error {
	// No attempts recorded. This must be rejected before any request is made:
	// the transport denies everything, so a driver that posts it fails here.
	err := d.PostAudit(ctx, ChangeID, HeadSHA, scm.Audit{
		Lens:    "scope-escape",
		Verdict: scm.AuditCleared,
	})
	if err == nil {
		return fmt.Errorf("PostAudit accepted an audit with no attempts; an audit that lists nothing tried is not a pass")
	}
	if !strings.Contains(err.Error(), "attempts") {
		return fmt.Errorf("PostAudit rejected the audit but the error does not mention attempts: %v", err)
	}
	return nil
}
