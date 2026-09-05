package signal_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/signal"
	"github.com/aicix-labs/factoryd/internal/state"
)

type fakeDriver struct {
	scm.Driver
	change     scm.Change
	diffs      []scm.FileDiff
	diffErr    error
	audits     []scm.Audit
	merge      scm.ProviderMerge
	mergeErr   error
	ancestor   bool
	comments   []string
	commentErr error
	readied    bool
	calls      []string
}

func (d *fakeDriver) Provider() string { return "fake" }
func (d *fakeDriver) Get(context.Context, scm.ChangeID) (scm.Change, error) {
	d.calls = append(d.calls, "get")
	return d.change, nil
}
func (d *fakeDriver) Diff(context.Context, scm.ChangeID) ([]scm.FileDiff, error) {
	d.calls = append(d.calls, "diff")
	return d.diffs, d.diffErr
}
func (d *fakeDriver) Audits(context.Context, scm.ChangeID, string) ([]scm.Audit, error) {
	d.calls = append(d.calls, "audits")
	return d.audits, nil
}
func (d *fakeDriver) SetDraft(_ context.Context, _ scm.ChangeID, draft bool) error {
	d.calls = append(d.calls, "setdraft")
	d.readied = !draft
	return nil
}
func (d *fakeDriver) Merge(_ context.Context, _ scm.ChangeID, expected string) (scm.ProviderMerge, error) {
	d.calls = append(d.calls, "merge:"+expected)
	return d.merge, d.mergeErr
}
func (d *fakeDriver) IsAncestor(context.Context, string, string) (bool, error) {
	d.calls = append(d.calls, "ancestor")
	return d.ancestor, nil
}
func (d *fakeDriver) Comment(_ context.Context, _ scm.ChangeID, body string) error {
	d.calls = append(d.calls, "comment")
	d.comments = append(d.comments, body)
	return d.commentErr
}

type lab struct {
	cfg  *config.Config
	drv  *fakeDriver
	deps signal.Deps
	now  time.Time
}

func newLab(t *testing.T) *lab {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"inbox", "outbox", "work", "submit"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	cfg := &config.Config{
		SchemaVersion: config.SchemaVersion, Name: "widgets", Provider: "github", TargetBranch: "main",
		GitHub: &config.GitHub{Owner: "acme", Repo: "widgets"},
		Paths:  config.Paths{Root: root, ProducerWorkdir: filepath.Join(root, "work"), SubmitRepo: filepath.Join(root, "submit")},
		Scope: config.Scope{
			DenyRegexes:     []string{`^\.github/workflows/`, `^deploy/`},
			AllowRegexes:    []string{`^deploy/.*\.md$`},
			HoldDiffRegexes: []string{`-----BEGIN [A-Z ]*PRIVATE KEY-----`},
			EscalateRegexes: []string{`(^|/)auth`},
		},
		Health: config.DefaultHealth(),
	}
	// Compile the scope the way Load would.
	if err := cfg.Validate(); err != nil {
		// Validate reports other missing fields too; only the scope must compile here.
		if strings.Contains(err.Error(), "does not compile") {
			t.Fatal(err)
		}
	}
	l := &lab{cfg: cfg, now: time.Date(2026, 9, 4, 15, 0, 0, 0, time.UTC)}
	l.drv = &fakeDriver{
		change:   scm.Change{ID: "42", State: scm.StateOpen, Draft: false, Author: "producer-bot", AuthorID: "1", HeadSHA: "abc123", SourceBranch: "producer/fix-abc", TargetBranch: "main"},
		diffs:    []scm.FileDiff{{Path: "src/a.go", Added: 1, Patch: "+++ b/src/a.go\n+package a\n"}},
		merge:    scm.ProviderMerged("m3rge"),
		ancestor: true,
	}
	l.deps = signal.Deps{Driver: l.drv, Reviewer: scm.Identity{ID: "2", Login: "factory-reviewer"}, Now: func() time.Time { return l.now }}
	return l
}

func (l *lab) run(t *testing.T, kind, sha, summary string) (signal.Result, error) {
	t.Helper()
	return signal.Run(context.Background(), l.cfg, l.deps, signal.Request{ID: "42", Kind: kind, SHA: sha, Summary: summary})
}

func exitOf(t *testing.T, err error) int {
	t.Helper()
	var se *signal.Error
	if !errors.As(err, &se) {
		t.Fatalf("not a signal.Error: %v", err)
	}
	return se.Exit
}

func (l *lab) verdictFile(t *testing.T) *state.Verdict {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(l.cfg.OutboxDir(), "42.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var v state.Verdict
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

// Positive control: a mergeable change is readied, merged with the expected
// head, verified by ancestry, and recorded in all three places.
func TestMergedVerdictMergesVerifiesAndRecords(t *testing.T) {
	l := newLab(t)
	res, err := l.run(t, "merged", "auto", "clean; tests added")
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict.MergeCommit != "m3rge" || res.Verdict.SHA != "abc123" || res.Decision.Class != signal.Mergeable {
		t.Fatalf("res=%+v", res)
	}
	if got := strings.Join(l.drv.calls, ","); got != "get,diff,merge:abc123,ancestor,comment" {
		t.Fatalf("calls=%s; the gate never marks a draft ready", got)
	}
	v := l.verdictFile(t)
	if v == nil || v.Kind != "merged" || v.MergeCommit != "m3rge" || v.Summary != "clean; tests added" {
		t.Fatalf("verdict file=%+v", v)
	}
	st, err := state.Load(l.cfg.StatePath(), l.cfg.Name)
	if err != nil || st.LastVerdict == nil || st.LastVerdict.MergeCommit != "m3rge" {
		t.Fatalf("state: %v %+v", err, st.LastVerdict)
	}
	if len(l.drv.comments) != 1 || !strings.Contains(l.drv.comments[0], "clean; tests added") || !strings.Contains(l.drv.comments[0], "m3rge") {
		t.Fatalf("comments=%v", l.drv.comments)
	}
}

func TestNonMergeVerdictsRecordWithoutMerging(t *testing.T) {
	for _, kind := range []string{"changes-requested", "operator-gated"} {
		l := newLab(t)
		res, err := l.run(t, kind, "auto", "needs a test for the empty case")
		if err != nil {
			t.Fatal(err)
		}
		if res.Verdict.MergeCommit != "" || strings.Contains(strings.Join(l.drv.calls, ","), "merge") || strings.Contains(strings.Join(l.drv.calls, ","), "setdraft") {
			t.Fatalf("%s: res=%+v calls=%v", kind, res, l.drv.calls)
		}
		if v := l.verdictFile(t); v == nil || v.Kind != kind {
			t.Fatalf("%s: verdict file=%+v", kind, v)
		}
	}
}

func TestRefusals(t *testing.T) {
	cases := map[string]struct {
		mutate  func(l *lab)
		kind    string
		sha     string
		summary string
		exit    int
		want    string
	}{
		"unknown verdict kind": {func(l *lab) {}, "approved", "auto", "s", signal.ExitConfig, "not one of"},
		"empty summary":        {func(l *lab) {}, "merged", "auto", "  ", signal.ExitConfig, "summary is empty"},
		"head moved":           {func(l *lab) {}, "merged", "0ld", "s", signal.ExitRefused, "head moved"},
		"change not open":      {func(l *lab) { l.drv.change.State = scm.StateMerged }, "merged", "auto", "s", signal.ExitRefused, "not open"},
		"empty diff":           {func(l *lab) { l.drv.diffs = nil }, "merged", "auto", "s", signal.ExitRefused, "empty diff"},
		"deny path":            {func(l *lab) { l.drv.diffs = []scm.FileDiff{{Path: ".github/workflows/ci.yml", Patch: "+x\n"}} }, "merged", "auto", "s", signal.ExitRefused, "operator-only"},
		"deny path via rename": {func(l *lab) {
			l.drv.diffs = []scm.FileDiff{{Path: "docs/x.md", OldPath: ".github/workflows/ci.yml", Renamed: true}}
		}, "merged", "auto", "s", signal.ExitRefused, "operator-only"},
		"held content anywhere": {func(l *lab) {
			l.drv.diffs = []scm.FileDiff{{Path: "src/k.go", Patch: "+++ b/src/k.go\n+const k = `-----BEGIN RSA PRIVATE KEY-----`\n"}}
		}, "merged", "auto", "s", signal.ExitRefused, "matches hold"},
		"escalate without audit": {func(l *lab) { l.drv.diffs = []scm.FileDiff{{Path: "internal/auth/login.go", Patch: "+x\n"}} }, "merged", "auto", "s", signal.ExitRefused, "no CLEARED audit"},
		"escalate with broken audit": {func(l *lab) {
			l.drv.diffs = []scm.FileDiff{{Path: "internal/auth/login.go", Patch: "+x\n"}}
			l.drv.audits = []scm.Audit{{Lens: "authz", SHA: "abc123", Verdict: scm.AuditBroken, Attempts: []string{"forged cookie"}, PostedBy: "factory-reviewer", PostedByID: "2"}}
		}, "merged", "auto", "s", signal.ExitRefused, "BROKEN"},
		"escalate with audit on another head": {func(l *lab) {
			l.drv.diffs = []scm.FileDiff{{Path: "internal/auth/login.go", Patch: "+x\n"}}
			l.drv.audits = []scm.Audit{{Lens: "authz", SHA: "0ld", Verdict: scm.AuditCleared, Attempts: []string{"forged cookie"}, PostedBy: "factory-reviewer", PostedByID: "2"}}
		}, "merged", "auto", "s", signal.ExitRefused, "no CLEARED audit by the reviewer"},
		"escalate with audit that tried nothing": {func(l *lab) {
			l.drv.diffs = []scm.FileDiff{{Path: "internal/auth/login.go", Patch: "+x\n"}}
			l.drv.audits = []scm.Audit{{Lens: "authz", SHA: "abc123", Verdict: scm.AuditCleared, PostedBy: "factory-reviewer", PostedByID: "2"}}
		}, "merged", "auto", "s", signal.ExitRefused, "no attempts"},
		"provider refused":  {func(l *lab) { l.drv.merge = scm.RefusedByProvider(scm.RefusedConflict, "conflict") }, "merged", "auto", "s", signal.ExitRefused, "provider refused"},
		"merge unverified":  {func(l *lab) { l.drv.ancestor = false }, "merged", "auto", "s", signal.ExitUnknown, "UNKNOWN"},
		"diff unreadable":   {func(l *lab) { l.drv.diffErr = errors.New("502") }, "merged", "auto", "s", signal.ExitConfig, "reading the diff"},
		"still a draft":     {func(l *lab) { l.drv.change.Draft = true }, "merged", "auto", "s", signal.ExitRefused, "set-draft 42 false"},
		"self-merge":        {func(l *lab) { l.drv.change.AuthorID = "2"; l.drv.change.Author = "factory-reviewer" }, "merged", "auto", "s", signal.ExitRefused, "same identity"},
		"author id unknown": {func(l *lab) { l.drv.change.AuthorID = "" }, "merged", "auto", "s", signal.ExitRefused, "no author id"},
		"audit forged by the producer": {func(l *lab) {
			l.drv.diffs = []scm.FileDiff{{Path: "internal/auth/login.go", Patch: "+x\n"}}
			l.drv.audits = []scm.Audit{{Lens: "authz", SHA: "abc123", Verdict: scm.AuditCleared, Attempts: []string{"tried nothing real"}, PostedBy: "producer-bot", PostedByID: "1"}}
		}, "merged", "auto", "s", signal.ExitRefused, "no CLEARED audit by the reviewer"},
		"audit by a third party": {func(l *lab) {
			l.drv.diffs = []scm.FileDiff{{Path: "internal/auth/login.go", Patch: "+x\n"}}
			l.drv.audits = []scm.Audit{{Lens: "authz", SHA: "abc123", Verdict: scm.AuditCleared, Attempts: []string{"x"}, PostedBy: "someone", PostedByID: "9"}}
		}, "merged", "auto", "s", signal.ExitRefused, "no CLEARED audit by the reviewer"},
		"audit with no authenticated author": {func(l *lab) {
			l.drv.diffs = []scm.FileDiff{{Path: "internal/auth/login.go", Patch: "+x\n"}}
			l.drv.audits = []scm.Audit{{Lens: "authz", SHA: "abc123", Verdict: scm.AuditCleared, Attempts: []string{"x"}, PostedBy: "factory-reviewer"}}
		}, "merged", "auto", "s", signal.ExitRefused, "no CLEARED audit by the reviewer"},
		"incomplete diff with hold rules": {func(l *lab) {
			l.drv.diffs = []scm.FileDiff{{Path: "vendor/blob.bin", Added: 3, Incomplete: true, IncompleteReason: "too_large"}}
		}, "merged", "auto", "s", signal.ExitRefused, "did not deliver its content"},
		"renamed file with omitted content": {func(l *lab) {
			l.drv.diffs = []scm.FileDiff{{Path: "internal/tokens.go", OldPath: "internal/plain.go", Renamed: true, Added: 12, Incomplete: true, IncompleteReason: "renamed with content changes but no patch delivered"}}
		}, "merged", "auto", "s", signal.ExitRefused, "did not deliver its content"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			l := newLab(t)
			c.mutate(l)
			_, err := l.run(t, c.kind, c.sha, c.summary)
			if err == nil {
				t.Fatalf("accepted; calls=%v", l.drv.calls)
			}
			if exitOf(t, err) != c.exit || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("exit=%d err=%v; want exit %d containing %q", exitOf(t, err), err, c.exit, c.want)
			}
			// The provider's own refusal, and an unverified merge, come back
			// FROM the merge call; every other refusal must precede it.
			merges := name == "provider refused" || c.exit == signal.ExitUnknown
			if !merges && strings.Contains(strings.Join(l.drv.calls, ","), "merge:") {
				t.Fatalf("a refused signal still merged: %v", l.drv.calls)
			}
			if l.verdictFile(t) != nil {
				t.Fatal("a refused signal wrote a verdict file")
			}
		})
	}
}

// An escalate-class change with a CLEARED audit on the exact head merges;
// the audits are consulted, which the mergeable control above does not do.
func TestEscalateWithClearedAuditMerges(t *testing.T) {
	l := newLab(t)
	l.drv.diffs = []scm.FileDiff{{Path: "internal/auth/login.go", Patch: "+x\n"}, {Path: "deploy/README.md", Patch: "+doc\n"}}
	l.drv.audits = []scm.Audit{{Lens: "authz-bypass", SHA: "abc123", Verdict: scm.AuditCleared, Attempts: []string{"forged session cookie", "replayed token"}, PostedBy: "factory-reviewer", PostedByID: "2"}}
	res, err := l.run(t, "merged", "abc123", "cleared after adversarial pass")
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision.Class != signal.Escalate || len(res.Audits) != 1 || res.Verdict.MergeCommit != "m3rge" {
		t.Fatalf("res=%+v", res)
	}
	if !strings.Contains(strings.Join(l.drv.calls, ","), "audits") {
		t.Fatalf("audits not consulted: %v", l.drv.calls)
	}
	// deploy/README.md matched deny but was exempted; that is said.
	if !strings.Contains(strings.Join(res.Decision.Reasons, "\n"), "exempted by allow") {
		t.Fatalf("reasons=%v", res.Decision.Reasons)
	}
}

// The handoff is the file; the comment is a courtesy. A comment that fails
// does not un-record the verdict, and it is said.
func TestCommentFailureIsSaidNotFatal(t *testing.T) {
	l := newLab(t)
	l.drv.commentErr = errors.New("403")
	var log strings.Builder
	l.deps.Log = &log
	if _, err := l.run(t, "changes-requested", "auto", "s"); err != nil {
		t.Fatal(err)
	}
	if l.verdictFile(t) == nil || !strings.Contains(log.String(), "WARNING") {
		t.Fatalf("file=%v log=%q", l.verdictFile(t) != nil, log.String())
	}
}

// A file the provider did not deliver is only a problem for content
// policy; with no hold rules configured, path policy still decides.
func TestIncompleteDiffWithoutHoldRulesStillMerges(t *testing.T) {
	l := newLab(t)
	l.cfg.Scope.HoldDiffRegexes = nil
	l.cfg.Validate()
	l.drv.diffs = []scm.FileDiff{{Path: "vendor/blob.bin", Added: 3, Incomplete: true, IncompleteReason: "too_large"}}
	if _, err := l.run(t, "merged", "auto", "s"); err != nil {
		t.Fatalf("err=%v; with no content policy an undelivered patch cannot fail it", err)
	}
}

// The identity check guards every verdict, not only merges: a reviewer that
// is the author must not record any verdict on its own work.
func TestSelfSignalRefusedForEveryVerdict(t *testing.T) {
	for _, kind := range []string{"merged", "changes-requested", "operator-gated"} {
		l := newLab(t)
		l.drv.change.AuthorID = "2"
		_, err := l.run(t, kind, "auto", "s")
		if err == nil || exitOf(t, err) != signal.ExitRefused {
			t.Fatalf("%s: err=%v", kind, err)
		}
	}
}

// A producer's forged audit decides nothing: it cannot clear the head, and
// it cannot veto the reviewer's genuine audit either. Found live: the first
// cut refused on the forged audit even with a valid reviewer audit beside
// it, which let the producer block its own merge -- pointless, but a
// producer's comment must not decide a merge in either direction.
func TestProducerAuditIsIgnoredNotAVeto(t *testing.T) {
	l := newLab(t)
	l.drv.diffs = []scm.FileDiff{{Path: "internal/auth/login.go", Patch: "+x\n"}}
	l.drv.audits = []scm.Audit{
		{Lens: "authz", SHA: "abc123", Verdict: scm.AuditBroken, Attempts: []string{"claims broken"}, PostedBy: "producer-bot", PostedByID: "1"},
		{Lens: "authz", SHA: "abc123", Verdict: scm.AuditCleared, Attempts: []string{"claims cleared"}, PostedBy: "producer-bot", PostedByID: "1"},
		{Lens: "authz", SHA: "abc123", Verdict: scm.AuditCleared, Attempts: []string{"forged session cookie"}, PostedBy: "factory-reviewer", PostedByID: "2"},
	}
	var log strings.Builder
	l.deps.Log = &log
	if _, err := l.run(t, "merged", "auto", "s"); err != nil {
		t.Fatalf("err=%v; the reviewer's audit must decide", err)
	}
	if !strings.Contains(log.String(), "IGNORING") || !strings.Contains(log.String(), "own author") {
		t.Fatalf("the producer's audits were not named as ignored:\n%s", log.String())
	}
	// An unattributed audit is ignored for its own reason, said as such:
	// "no authenticated author" is a driver defect to fix, not a stranger.
	l = newLab(t)
	l.drv.diffs = []scm.FileDiff{{Path: "internal/auth/login.go", Patch: "+x\n"}}
	l.drv.audits = []scm.Audit{{Lens: "authz", SHA: "abc123", Verdict: scm.AuditCleared, Attempts: []string{"x"}, PostedBy: "factory-reviewer"}}
	log.Reset()
	l.deps.Log = &log
	if _, err := l.run(t, "merged", "auto", "s"); err == nil || !strings.Contains(log.String(), "no provider-authenticated author") {
		t.Fatalf("err=%v log=%q", err, log.String())
	}
	// And the reviewer's own BROKEN still refuses.
	l = newLab(t)
	l.drv.diffs = []scm.FileDiff{{Path: "internal/auth/login.go", Patch: "+x\n"}}
	l.drv.audits = []scm.Audit{{Lens: "authz", SHA: "abc123", Verdict: scm.AuditBroken, Attempts: []string{"x"}, PostedBy: "factory-reviewer", PostedByID: "2"}}
	if _, err := l.run(t, "merged", "auto", "s"); err == nil || !strings.Contains(err.Error(), "BROKEN") {
		t.Fatalf("err=%v", err)
	}
}

func TestClassifyHoldOnlyOnAddedLines(t *testing.T) {
	cfg := newLab(t).cfg
	// A removed key and the +++ header are not additions.
	d := signal.Classify(&cfg.Scope, []scm.FileDiff{{Path: "src/k.go", Patch: "+++ b/-----BEGIN PRIVATE KEY-----\n------BEGIN RSA PRIVATE KEY-----\n+safe\n"}})
	if d.Class != signal.Mergeable {
		t.Fatalf("decision=%+v", d)
	}
	d = signal.Classify(&cfg.Scope, []scm.FileDiff{{Path: "src/k.go", Patch: "+-----BEGIN RSA PRIVATE KEY-----\n"}})
	if d.Class != signal.OperatorOnly {
		t.Fatalf("decision=%+v", d)
	}
	// Strictest wins, all reasons kept.
	d = signal.Classify(&cfg.Scope, []scm.FileDiff{{Path: "internal/auth/x.go"}, {Path: "deploy/k8s.yaml"}})
	if d.Class != signal.OperatorOnly || len(d.Reasons) != 2 {
		t.Fatalf("decision=%+v", d)
	}
}

// The verdict carries the branch the producer must declare to update the
// change (#29): the pushed <declared>-<tree> and the declared family
// recovered from it. Without it a credential-less, networkless producer
// cannot map a change id back to a declarable branch and opens a second
// change beside the one under review.
func TestVerdictCarriesTheBranchAndItsFamily(t *testing.T) {
	l := newLab(t)
	l.drv.change.SourceBranch = "producer/fix-0123456789"
	_, err := l.run(t, "changes-requested", "auto", "add a test for the empty case")
	if err != nil {
		t.Fatal(err)
	}
	v := l.verdictFile(t)
	if v == nil || v.Branch != "producer/fix-0123456789" || v.DeclaredBranch != "producer/fix" {
		t.Fatalf("verdict file=%+v; want branch and declared family", v)
	}
	st, err := state.Load(l.cfg.StatePath(), l.cfg.Name)
	if err != nil || st.LastVerdict == nil || st.LastVerdict.DeclaredBranch != "producer/fix" {
		t.Fatalf("state=%+v err=%v", st.LastVerdict, err)
	}
}

// The cycle finishes on the merge of ITS change, by id: an older member of
// the family or an unrelated draft merging says nothing about the
// producer's workdir (#35, #41 review).
func TestMergeFinishesOnlyTheCycleOfThatChange(t *testing.T) {
	l := newLab(t)
	if _, err := state.Update(l.cfg.StatePath(), l.cfg.Name, func(st *state.State) error {
		c := st.SetCycle(state.CycleOpen, time.Now())
		c.Family, c.Digest, c.ChangeID = "feat/x", "feat/x-abc", "99" // not the change being signalled
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.run(t, "merged", "auto", "clean"); err != nil {
		t.Fatal(err)
	}
	st, _ := state.Load(l.cfg.StatePath(), l.cfg.Name)
	if st.Cycle.Phase != state.CycleOpen || st.Cycle.ChangeID != "99" {
		t.Fatalf("a merge of change 42 finished the cycle of change 99: %+v", st.Cycle)
	}
	// Now the cycle IS change 42.
	l2 := newLab(t)
	if _, err := state.Update(l2.cfg.StatePath(), l2.cfg.Name, func(st *state.State) error {
		c := st.SetCycle(state.CycleOpen, time.Now())
		c.Family, c.Digest, c.ChangeID = "feat/x", "feat/x-abc", "42"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := l2.run(t, "merged", "auto", "clean"); err != nil {
		t.Fatal(err)
	}
	st, _ = state.Load(l2.cfg.StatePath(), l2.cfg.Name)
	if st.Cycle.Phase != state.CycleFinished {
		t.Fatalf("the merge of the cycle's own change did not finish it: %+v", st.Cycle)
	}
	// And changes-requested on it does not.
	l3 := newLab(t)
	if _, err := state.Update(l3.cfg.StatePath(), l3.cfg.Name, func(st *state.State) error {
		c := st.SetCycle(state.CycleOpen, time.Now())
		c.ChangeID, c.Family, c.Digest = "42", "producer/fix-abc", "producer/fix-abc"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := l3.run(t, "changes-requested", "auto", "needs work"); err != nil {
		t.Fatal(err)
	}
	st, _ = state.Load(l3.cfg.StatePath(), l3.cfg.Name)
	if st.Cycle.Phase != state.CycleOpen || st.Cycle.ReviewDecision == nil || st.Cycle.ReviewDecision.Kind != state.VerdictChangesRequested || st.Cycle.ReviewDecision.SHA != "abc123" {
		t.Fatalf("changes-requested finished the cycle: %+v", st.Cycle)
	}
}

// A merged verdict that names the immutable branch of a cycle still
// "submitting" finishes it: the same draft, seen before submit recorded
// its id (#41 review).
func TestMergeOfASubmittingCyclesBranchFinishesIt(t *testing.T) {
	l := newLab(t)
	if _, err := state.Update(l.cfg.StatePath(), l.cfg.Name, func(st *state.State) error {
		c := st.SetCycle(state.CycleSubmitting, time.Now())
		c.Family, c.Digest = "producer/fix", "producer/fix-abc"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.run(t, "merged", "auto", "clean"); err != nil {
		t.Fatal(err)
	}
	st, _ := state.Load(l.cfg.StatePath(), l.cfg.Name)
	if st.Cycle.Phase != state.CycleFinished || st.Cycle.ChangeID != "42" {
		t.Fatalf("cycle %+v, want finished 42", st.Cycle)
	}
	// A different branch: not this cycle's draft.
	l2 := newLab(t)
	state.Update(l2.cfg.StatePath(), l2.cfg.Name, func(st *state.State) error {
		c := st.SetCycle(state.CycleSubmitting, time.Now())
		c.Family, c.Digest = "producer/other", "producer/other-def"
		return nil
	})
	if _, err := l2.run(t, "merged", "auto", "clean"); err != nil {
		t.Fatal(err)
	}
	st, _ = state.Load(l2.cfg.StatePath(), l2.cfg.Name)
	if st.Cycle.Phase != state.CycleSubmitting {
		t.Fatalf("a merge of another branch finished a submitting cycle: %+v", st.Cycle)
	}
}

// The operator's verdict for a merge made outside factoryd (#43): verified
// at the provider (merged, head on the target), written to the outbox --
// which wakes the producer -- recorded as the operator's, and the cycle
// finished if it is the cycle's change. Not merged, or merged but not on
// the target, is refused: this records a merge, it does not perform one.
func TestOperatorRecordsAMergeMadeOutsideFactoryd(t *testing.T) {
	l := newLab(t)
	l.drv.change.State = scm.StateMerged
	if _, err := state.Update(l.cfg.StatePath(), l.cfg.Name, func(st *state.State) error {
		c := st.SetCycle(state.CycleOpen, time.Now())
		c.ChangeID, c.Family = "42", "producer/fix"
		st.LastVerdict = &state.Verdict{ChangeID: "42", Kind: state.VerdictOperatorGated}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	v, path, err := signal.RecordMerged(context.Background(), l.cfg, l.drv, "42", "", func() time.Time { return l.now })
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != state.VerdictMerged || v.RecordedBy != "operator" || v.MergeCommit != "abc123" || v.DeclaredBranch != state.FamilyOf("producer/fix-abc") {
		t.Fatalf("verdict %+v", v)
	}
	if _, err := os.Stat(path); err != nil || filepath.Dir(path) != l.cfg.OutboxDir() {
		t.Fatalf("verdict not written to the outbox: %s %v", path, err)
	}
	got, err := state.ReadVerdictFile(path)
	if err != nil || got.RecordedBy != "operator" {
		t.Fatalf("outbox verdict %+v %v", got, err)
	}
	st, _ := state.Load(l.cfg.StatePath(), l.cfg.Name)
	if st.LastVerdict.Kind != state.VerdictMerged || st.Cycle.Phase != state.CycleFinished {
		t.Fatalf("state: verdict %s, cycle %s", st.LastVerdict.Kind, st.Cycle.Phase)
	}

	// Not merged: refused, nothing written.
	l2 := newLab(t)
	if _, _, err := signal.RecordMerged(context.Background(), l2.cfg, l2.drv, "42", "", nil); err == nil || !strings.Contains(err.Error(), "not merged") {
		t.Fatalf("an open change was recorded as merged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l2.cfg.OutboxDir(), "42.json")); !os.IsNotExist(err) {
		t.Fatal("a verdict was written for a refused record")
	}
	// Retargeted (#49 review): the change now targets release and is merged
	// there. Not this factory's merge; refused, nothing written.
	l4 := newLab(t)
	l4.drv.change.State = scm.StateMerged
	l4.drv.change.TargetBranch = "release"
	if _, _, err := signal.RecordMerged(context.Background(), l4.cfg, l4.drv, "42", "", nil); err == nil || !strings.Contains(err.Error(), `targets "release", not this factory's target "main"`) {
		t.Fatalf("a retargeted merge was recorded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l4.cfg.OutboxDir(), "42.json")); !os.IsNotExist(err) {
		t.Fatal("a verdict was written for a retargeted merge")
	}
	// Merged per the provider but the head is not on the target: refused.
	l3 := newLab(t)
	l3.drv.change.State = scm.StateMerged
	l3.drv.ancestor = false
	if _, _, err := signal.RecordMerged(context.Background(), l3.cfg, l3.drv, "42", "", nil); err == nil || !strings.Contains(err.Error(), "not on main") {
		t.Fatalf("a merge whose head is not on the target was recorded: %v", err)
	}
}
