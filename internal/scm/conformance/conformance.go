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
	// ChangeTitle and SourceBranch are what the recorder creates on a live
	// provider, so the assertions here and the fixtures stay in step.
	ChangeTitle  = "gate: match command position, not the mention"
	SourceBranch = "producer/fix-thing"
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

	// UnmergeableMessage is this provider's own wording when it refuses an
	// unmergeable change, taken from the recorded fixture.
	//
	// It is per-provider because the two do not agree: GitLab says "Branch
	// cannot be merged" -- the message v1 printed while returning exit status
	// 0 -- and GitHub says "Pull Request has merge conflicts". The suite used
	// to assert GitLab's wording for both, which passed only because the
	// hand-written GitHub fixture had copied it.
	UnmergeableMessage string
}

// Result is one scenario's outcome. Err == nil means the scenario passed.
type Result struct {
	Scenario string
	Err      error
}

type scenario struct {
	name string
	// denyHTTP runs the scenario against a transport that refuses every
	// request, asserting the driver decides locally.
	denyHTTP bool
	run      func(ctx context.Context, d scm.Driver, f Factory) error
}

// Check runs every scenario against the driver f builds and returns one Result
// per scenario, in a stable order, plus results for the suite's own
// self-checks (verb coverage).
func Check(ctx context.Context, f Factory) []Result {
	var out []Result
	observed := map[string]bool{}

	if f.UnmergeableMessage == "" {
		// Without it the unmergeable scenario would accept any reason at all,
		// including an empty one.
		out = append(out, Result{Scenario: "_factory",
			Err: fmt.Errorf("Factory.UnmergeableMessage is empty; the unmergeable scenario would assert nothing about the reason")})
	}

	for _, s := range scenarios() {
		seen, err := runScenario(ctx, f, s)
		for v := range seen {
			observed[v] = true
		}
		out = append(out, Result{Scenario: s.name, Err: err})
	}

	// Coverage is decided on what the scenarios actually called, not on what
	// they claim to call.
	out = append(out, Result{Scenario: "_verb_coverage", Err: checkVerbCoverage(observed)})
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

// checkVerbCoverage compares the verbs the scenarios actually called against
// the methods of scm.Driver. Both sides are derived -- one by reflection, one by
// observation -- so adding a method to the interface without a scenario that
// calls it fails here with no list to remember to update.
func checkVerbCoverage(observed map[string]bool) error {
	var missing []string
	for _, v := range interfaceVerbs() {
		if !observed[v] {
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

func runScenario(ctx context.Context, f Factory, s scenario) (map[string]bool, error) {
	var tr *httpfixture.Transport
	if s.denyHTTP {
		tr = httpfixture.Deny()
	} else {
		b, err := httpfixture.Load(f.FixtureDir, s.name)
		if err != nil {
			return nil, fmt.Errorf("loading fixture: %w", err)
		}
		tr = httpfixture.NewTransport(b)
	}

	inner, err := f.New(tr.Client())
	if err != nil {
		return nil, fmt.Errorf("building driver: %w", err)
	}
	rec := newRecorder(inner)

	if got := rec.Provider(); got != f.Provider {
		return rec.observed(), fmt.Errorf("Provider() = %q, want %q", got, f.Provider)
	}

	if err := s.run(ctx, rec, f); err != nil {
		return rec.observed(), err
	}
	// Fixture accounting is part of the assertion, not bookkeeping: it catches
	// a driver that skipped a call the scenario depends on.
	return rec.observed(), tr.Done()
}

func scenarios() []scenario {
	return []scenario{
		{name: "whoami", run: scWhoami},
		{name: "list_open", run: scListOpen},
		{name: "get", run: scGet},
		{name: "diff", run: scDiff},
		{name: "pipeline_success", run: scPipelineSuccess},
		{name: "pipeline_failed", run: scPipelineFailed},
		{name: "pipeline_none", run: scPipelineNone},
		{name: "comment", run: scComment},
		{name: "set_draft_ready", run: scSetDraftReady},
		{name: "merge_success", run: scMergeSuccess},
		{name: "merge_refused_draft", run: scMergeRefusedDraft},
		{name: "merge_refused_unmergeable", run: scMergeUnmergeable},
		{name: "merge_head_moved", run: scMergeHeadMoved},
		{name: "merge_reported_but_absent", run: scMergeReportedButAbsent},
		{name: "merge_unverified", run: scMergeUnverified},
		{name: "post_audit", run: scPostAudit},
		{name: "audits", run: scAudits},
		{name: "audit_requires_attempts", denyHTTP: true, run: scAuditRequiresAttempts},
	}
}

// ---------- scenarios ----------

func scWhoami(ctx context.Context, d scm.Driver, f Factory) error {
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

func scListOpen(ctx context.Context, d scm.Driver, f Factory) error {
	cs, err := d.ListOpen(ctx)
	if err != nil {
		return err
	}
	// The fixture is paginated one change per page. A driver that ignores
	// pagination returns one change and fails here.
	if len(cs) != 2 {
		return fmt.Errorf("ListOpen returned %d changes, want 2 (the fixture is paginated)", len(cs))
	}

	// Looked up by id, not by position. Providers return open changes
	// newest-first, and the earlier hand-written fixtures happened to list them
	// oldest-first -- so this assertion used to encode an ordering neither API
	// promises. Recording exposed it.
	byID := map[scm.ChangeID]scm.Change{}
	for _, c := range cs {
		byID[c.ID] = c
	}
	first, ok := byID[ChangeID]
	if !ok {
		return fmt.Errorf("ListOpen did not return change %s; got %v", ChangeID, ids(cs))
	}
	second, ok := byID["43"]
	if !ok {
		return fmt.Errorf("ListOpen did not return change 43; got %v", ids(cs))
	}
	if first.State != scm.StateOpen {
		return fmt.Errorf("ListOpen change %s State = %v, want open", ChangeID, first.State)
	}
	if first.Draft {
		return fmt.Errorf("ListOpen change %s Draft = true, want false", ChangeID)
	}
	if !second.Draft {
		return fmt.Errorf("ListOpen change 43 Draft = false, want true (it is a draft in the fixture)")
	}
	return nil
}

func ids(cs []scm.Change) []scm.ChangeID {
	out := make([]scm.ChangeID, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}

func scGet(ctx context.Context, d scm.Driver, f Factory) error {
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
	case c.SourceBranch != SourceBranch:
		return fmt.Errorf("Get.SourceBranch = %q, want %q", c.SourceBranch, SourceBranch)
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

func scDiff(ctx context.Context, d scm.Driver, f Factory) error {
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

func scPipelineSuccess(ctx context.Context, d scm.Driver, f Factory) error {
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

func scPipelineFailed(ctx context.Context, d scm.Driver, f Factory) error {
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

func scPipelineNone(ctx context.Context, d scm.Driver, f Factory) error {
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

func scComment(ctx context.Context, d scm.Driver, f Factory) error {
	// The fixture asserts the body reached the provider; see
	// request_body_contains in comment.json.
	return d.Comment(ctx, ChangeID, "changes-requested: the guard matches the mention, not the command")
}

func scSetDraftReady(ctx context.Context, d scm.Driver, f Factory) error {
	return d.SetDraft(ctx, ChangeID, false)
}

func scMergeSuccess(ctx context.Context, d scm.Driver, f Factory) error {
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
	if !r.Verified() {
		return fmt.Errorf("Verified() = false; a merge reported by the API but not confirmed by ancestry is not a merge")
	}
	if r.ClaimedCommit != MergeSHA {
		return fmt.Errorf("ClaimedCommit = %q, want %q", r.ClaimedCommit, MergeSHA)
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
	if r.Verified() {
		return fmt.Errorf("Outcome %v reported Verified() = true", r.Outcome)
	}
	if reasonSubstr != "" && !strings.Contains(r.Reason, reasonSubstr) {
		return fmt.Errorf("Reason = %q, want it to mention %q", r.Reason, reasonSubstr)
	}
	return nil
}

func scMergeRefusedDraft(ctx context.Context, d scm.Driver, f Factory) error {
	r, err := scm.MergeVerified(ctx, d, ChangeID, HeadSHA, TargetBranch)
	if err != nil {
		return err
	}
	return refusal(r, scm.RefusedDraft, "draft")
}

func scMergeUnmergeable(ctx context.Context, d scm.Driver, f Factory) error {
	r, err := scm.MergeVerified(ctx, d, ChangeID, HeadSHA, TargetBranch)
	if err != nil {
		return err
	}
	// This is the v1 bug, reproduced: the provider refuses, in its own words.
	// v1 printed that message and exited 0.
	return refusal(r, scm.RefusedConflict, f.UnmergeableMessage)
}

func scMergeHeadMoved(ctx context.Context, d scm.Driver, f Factory) error {
	r, err := scm.MergeVerified(ctx, d, ChangeID, HeadSHA, TargetBranch)
	if err != nil {
		return err
	}
	return refusal(r, scm.RefusedConflict, "head moved")
}

func scMergeReportedButAbsent(ctx context.Context, d scm.Driver, f Factory) error {
	r, err := scm.MergeVerified(ctx, d, ChangeID, HeadSHA, TargetBranch)
	if err != nil {
		return err
	}
	// HTTP 200 with a body that does not say "merged". The status code is not
	// the outcome.
	return refusal(r, scm.MergeUnknown, "")
}

func scMergeUnverified(ctx context.Context, d scm.Driver, f Factory) error {
	r, err := scm.MergeVerified(ctx, d, ChangeID, HeadSHA, TargetBranch)
	if err != nil {
		return err
	}
	// The API reported a merge commit; the repository says it is not on the
	// target branch. The API is not the authority on what landed.
	if err := refusal(r, scm.MergeUnknown, "not an ancestor"); err != nil {
		return err
	}
	// The commit the provider claimed must survive into a field. Someone
	// investigating this Unknown has to go and look at that sha, and digging it
	// out of prose is not a structured result.
	if r.ClaimedCommit != MergeSHA {
		return fmt.Errorf("ClaimedCommit = %q, want the commit the provider claimed (%s)", r.ClaimedCommit, MergeSHA)
	}
	return nil
}

func scPostAudit(ctx context.Context, d scm.Driver, f Factory) error {
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

func scAudits(ctx context.Context, d scm.Driver, f Factory) error {
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

func scAuditRequiresAttempts(ctx context.Context, d scm.Driver, f Factory) error {
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
