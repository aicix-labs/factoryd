package submit_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/gittransport"
	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/submit"
)

// ---------- fakes ----------

type fakeTransport struct {
	login    string
	guardErr error
	pushed   []string
	fetched  []string
	calls    []string
}

func (f *fakeTransport) Guard() error { f.calls = append(f.calls, "guard"); return f.guardErr }
func (f *fakeTransport) Identity(context.Context) (gittransport.Identity, error) {
	f.calls = append(f.calls, "identity")
	return gittransport.Identity{Login: f.login, Source: "credential-helper"}, nil
}
func (f *fakeTransport) Fetch(_ context.Context, r string) error {
	f.calls = append(f.calls, "fetch")
	f.fetched = append(f.fetched, r)
	return nil
}
func (f *fakeTransport) Push(_ context.Context, r string) error {
	f.calls = append(f.calls, "push")
	f.pushed = append(f.pushed, r)
	return nil
}

type fakeGit struct {
	calls    []string
	nothing  bool // Commit reports nothing to commit
	commitOK string
}

func (g *fakeGit) Checkout(_ context.Context, branch, start string) error {
	g.calls = append(g.calls, "checkout "+branch+" from "+start)
	return nil
}
func (g *fakeGit) Commit(_ context.Context, msg, name, email string) (string, bool, error) {
	g.calls = append(g.calls, "commit as "+name)
	if g.nothing {
		return "", false, nil
	}
	return g.commitOK, true, nil
}
func (g *fakeGit) Status(context.Context) ([]string, error) { return nil, nil }

type fakeGate struct {
	exit  int
	err   error
	ran   bool
	env   []string
	calls *[]string
}

func (g *fakeGate) Run(_ context.Context, _ *config.Config, exe string, env []string, out io.Writer) (int, error) {
	g.ran = true
	g.env = env
	if g.calls != nil {
		*g.calls = append(*g.calls, "gate")
	}
	return g.exit, g.err
}

type fakeDriver struct {
	scm.Driver
	found  *scm.Change
	opened *scm.DraftSpec
	calls  *[]string
}

func (d *fakeDriver) FindOpenBySource(_ context.Context, branch string) (scm.Change, bool, error) {
	if d.calls != nil {
		*d.calls = append(*d.calls, "find")
	}
	if d.found != nil {
		return *d.found, true, nil
	}
	return scm.Change{}, false, nil
}
func (d *fakeDriver) OpenDraft(_ context.Context, spec scm.DraftSpec) (scm.Change, error) {
	if d.calls != nil {
		*d.calls = append(*d.calls, "open")
	}
	d.opened = &spec
	return scm.Change{ID: "42", SourceBranch: spec.SourceBranch, TargetBranch: spec.TargetBranch, Draft: true, State: scm.StateOpen}, nil
}

// ---------- fixture ----------

type lab struct {
	cfg  *config.Config
	root string
	work string
	deps submit.Deps
	tr   *fakeTransport
	git  *fakeGit
	gate *fakeGate
	drv  *fakeDriver
	log  *bytes.Buffer
}

func newLab(t *testing.T) *lab {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	submitRepo := filepath.Join(root, "submit")
	for _, d := range []string{work, filepath.Join(submitRepo, ".git"), filepath.Join(root, "inbox"), filepath.Join(root, "outbox")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{
		SchemaVersion: config.SchemaVersion, Name: "lab", Provider: "github",
		GitHub: &config.GitHub{Owner: "acme", Repo: "widgets"}, TargetBranch: "main",
		Git:   config.Git{Remote: "https://github.com/acme/widgets.git", Transport: "https"},
		Paths: config.Paths{Root: root, ProducerWorkdir: work, SubmitRepo: submitRepo},
		Gate: config.Gate{Command: []string{"true"}, Env: map[string]string{"PATH": os.Getenv("PATH")},
			RunAs: &config.RunAs{User: "factoryd-gate"}, RequiredWritablePaths: []string{"build/out"}},
	}
	l := &lab{cfg: cfg, root: root, work: work, log: &bytes.Buffer{}}
	calls := &[]string{}
	l.tr = &fakeTransport{login: "producer-bot"}
	l.git = &fakeGit{commitOK: "abc123"}
	l.gate = &fakeGate{exit: 0, calls: calls}
	l.drv = &fakeDriver{calls: calls}
	l.deps = submit.Deps{
		Driver: l.drv, Transport: l.tr, Git: l.git, Gate: l.gate,
		Producer: scm.Identity{ID: "1", Login: "producer-bot"},
		Reviewer: scm.Identity{ID: "2", Login: "factory-reviewer"},
		Log:      l.log,
	}
	return l
}

func (l *lab) declare(t *testing.T, branch, msg string) {
	t.Helper()
	if branch != "" {
		if err := os.WriteFile(filepath.Join(l.work, submit.BranchFile), []byte(branch+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if msg != "" {
		if err := os.WriteFile(filepath.Join(l.work, submit.CommitMsgFile), []byte(msg+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (l *lab) edit(t *testing.T, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(l.work, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.work, name), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exitOf(t *testing.T, err error) int {
	t.Helper()
	var se *submit.Error
	if !errors.As(err, &se) {
		t.Fatalf("error is not a submit.Error: %v", err)
	}
	return se.Exit
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// ---------- the happy path, and its ordering ----------

func TestSubmitOpensADraftAndSignals(t *testing.T) {
	l := newLab(t)
	l.edit(t, "src/a.go")
	l.declare(t, "producer/fix", "gate: match command position\n\nlonger body")

	r, err := submit.Run(context.Background(), l.cfg, l.deps)
	if err != nil {
		t.Fatalf("%v\n%s", err, l.log)
	}
	if r.Exit != submit.ExitSubmitted || r.Change == nil || r.Change.ID != "42" {
		t.Fatalf("result = %+v", r)
	}
	if len(l.tr.pushed) != 1 || !strings.Contains(l.tr.pushed[0], "producer/fix") {
		t.Fatalf("pushed %v", l.tr.pushed)
	}
	if l.drv.opened == nil || l.drv.opened.Title != "gate: match command position" || l.drv.opened.TargetBranch != "main" {
		t.Fatalf("draft spec = %+v", l.drv.opened)
	}
	if !exists(filepath.Join(l.cfg.InboxDir(), "wake")) {
		t.Fatal("the reviewer was not woken")
	}
	// Control files retired; they are handoff, not content.
	for _, f := range []string{submit.BranchFile, submit.CommitMsgFile} {
		if exists(filepath.Join(l.work, f)) {
			t.Fatalf("%s survived a successful submit", f)
		}
	}
	// The tree crossed the boundary.
	if !exists(filepath.Join(l.cfg.Paths.SubmitRepo, "src/a.go")) {
		t.Fatal("the producer's change was not copied into the submit repository")
	}
	// The gate ran with a constructed environment, not the process's.
	joined := strings.Join(l.gate.env, "\n")
	if !strings.Contains(joined, "FACTORYD_ROLE=gate") || strings.Contains(joined, "HOME="+os.Getenv("HOME")+"\n") && os.Getenv("HOME") != "" && l.cfg.Gate.Env["HOME"] == "" {
		t.Fatalf("gate env: %s", joined)
	}
}

// The order is the contract: identity before anything is read, the guard
// before any git, the gate before the push, the draft after the push.
func TestSubmitOrdering(t *testing.T) {
	l := newLab(t)
	l.edit(t, "f")
	l.declare(t, "producer/fix", "msg")
	if _, err := submit.Run(context.Background(), l.cfg, l.deps); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(l.tr.calls, ",")
	want := "identity,guard,fetch,push"
	if got != want {
		t.Fatalf("transport calls = %s, want %s", got, want)
	}
	all := strings.Join(*l.gate.calls, ",")
	if all != "gate,find,open" {
		t.Fatalf("gate/driver order = %s, want gate,find,open", all)
	}
}

// ---------- exit 3: configuration and identity ----------

func TestSubmitRefusesTheSameIdentity(t *testing.T) {
	l := newLab(t)
	l.edit(t, "f")
	l.declare(t, "producer/fix", "msg")
	l.deps.Reviewer = l.deps.Producer
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	if exitOf(t, err) != submit.ExitConfig {
		t.Fatalf("exit %d, want 3: %v", exitOf(t, err), err)
	}
	// Refused before anything was touched.
	if len(l.tr.calls) != 0 || len(l.git.calls) != 0 || l.gate.ran {
		t.Fatalf("work happened after the identity refusal: transport=%v git=%v gate=%v", l.tr.calls, l.git.calls, l.gate.ran)
	}
}

// The production incident: git resolves to someone other than the API does.
func TestSubmitRefusesWhenGitAndAPIDisagree(t *testing.T) {
	l := newLab(t)
	l.edit(t, "f")
	l.declare(t, "producer/fix", "msg")
	l.tr.login = "factory-reviewer"
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	if exitOf(t, err) != submit.ExitConfig || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("exit %d: %v", exitOf(t, err), err)
	}
	if len(l.tr.pushed) != 0 {
		t.Fatal("pushed under a disagreeing identity")
	}
}

func TestSubmitRefusesIntentWithoutADestination(t *testing.T) {
	cases := map[string]struct{ branch, want string }{
		"no branch":      {"", "refuses to guess"},
		"target branch":  {"main", "never writes to the target"},
		"invalid branch": {"bad branch~name", "invalid"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			l := newLab(t)
			l.edit(t, "f")
			l.declare(t, c.branch, "msg")
			_, err := submit.Run(context.Background(), l.cfg, l.deps)
			if exitOf(t, err) != submit.ExitConfig || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("exit %d: %v", exitOf(t, err), err)
			}
			if len(l.tr.pushed) != 0 {
				t.Fatal("pushed anyway")
			}
		})
	}
}

// A guard failure is the host's problem and happens before any git.
func TestSubmitGuardFailsBeforeAnyGit(t *testing.T) {
	l := newLab(t)
	l.edit(t, "f")
	l.declare(t, "producer/fix", "msg")
	l.tr.guardErr = errors.New("rewrite in force")
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	if exitOf(t, err) != submit.ExitConfig {
		t.Fatalf("exit %d: %v", exitOf(t, err), err)
	}
	if len(l.git.calls) != 0 || len(l.tr.fetched) != 0 {
		t.Fatalf("git ran after a failed guard: %v %v", l.git.calls, l.tr.fetched)
	}
}

// An environmental failure is exit 3, never exit 5 (§9.6): a gate that could
// not run is not a red branch.
func TestGateThatCannotRunIsExit3NotExit5(t *testing.T) {
	l := newLab(t)
	l.edit(t, "f")
	l.declare(t, "producer/fix", "msg")
	l.gate.err = errors.New("could not start the gate as factoryd-gate: operation not permitted")
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	if exitOf(t, err) != submit.ExitConfig {
		t.Fatalf("exit %d, want 3: a gate that could not run was reported as a red branch: %v", exitOf(t, err), err)
	}
	if len(l.tr.pushed) != 0 {
		t.Fatal("pushed after the gate failed to run")
	}
}

func TestGatePathThatCannotBeCreatedIsExit3(t *testing.T) {
	l := newLab(t)
	l.edit(t, "f")
	l.declare(t, "producer/fix", "msg")
	l.cfg.Gate.RequiredWritablePaths = []string{"/proc/factoryd-cannot-create/x"}
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	if exitOf(t, err) != submit.ExitConfig {
		t.Fatalf("exit %d, want 3: %v", exitOf(t, err), err)
	}
	if l.gate.ran {
		t.Fatal("the gate ran against a path that could not be created")
	}
}

// ---------- exit 4: nothing to submit ----------

// Issue #12: a producer that edited files but declared nothing leaves a dirty
// tree the next turn trips over. Exit 4 must name what is being left behind.
func TestNothingDeclaredNamesTheDirtyPaths(t *testing.T) {
	l := newLab(t)
	l.edit(t, "deploy/provision.sh")
	l.edit(t, "deploy/tests/x_test.sh")
	// no control files
	r, err := submit.Run(context.Background(), l.cfg, l.deps)
	if err != nil {
		t.Fatal(err)
	}
	if r.Exit != submit.ExitNothing {
		t.Fatalf("exit %d, want 4", r.Exit)
	}
	if len(r.DirtyPaths) != 2 || r.DirtyPaths[0] != "deploy/provision.sh" {
		t.Fatalf("dirty paths = %v; the stranded work was not named", r.DirtyPaths)
	}
	if !strings.Contains(l.log.String(), "deploy/provision.sh") {
		t.Fatalf("the log does not name the dirty path:\n%s", l.log)
	}
	if len(l.tr.calls) != 1 { // identity only
		t.Fatalf("transport was used for a non-submission: %v", l.tr.calls)
	}
}

func TestIdenticalTreeIsExit4(t *testing.T) {
	l := newLab(t)
	l.declare(t, "producer/fix", "msg")
	l.git.nothing = true
	r, err := submit.Run(context.Background(), l.cfg, l.deps)
	if err != nil {
		t.Fatal(err)
	}
	if r.Exit != submit.ExitNothing || l.gate.ran || len(l.tr.pushed) != 0 {
		t.Fatalf("result=%+v gate=%v pushed=%v", r, l.gate.ran, l.tr.pushed)
	}
}

// ---------- exit 5: the gate is red ----------

func TestRedGateDoesNotPushAndAsksAQuestion(t *testing.T) {
	l := newLab(t)
	l.edit(t, "f")
	l.declare(t, "producer/fix", "msg")
	l.gate.exit = 1
	r, err := submit.Run(context.Background(), l.cfg, l.deps)
	if err != nil {
		t.Fatal(err)
	}
	if r.Exit != submit.ExitGateRed {
		t.Fatalf("exit %d, want 5", r.Exit)
	}
	if len(l.tr.pushed) != 0 {
		t.Fatal("a red branch was pushed")
	}
	if l.drv.opened != nil {
		t.Fatal("a draft was opened for a red branch")
	}
	q, err := os.ReadFile(filepath.Join(l.cfg.InboxDir(), "question.md"))
	if err != nil {
		t.Fatalf("no question was written for the reviewer: %v", err)
	}
	// The question must stand alone and carry a proposed fix (§6.2).
	for _, want := range []string{"producer/fix", "exit: 1", "Proposed:"} {
		if !strings.Contains(string(q), want) {
			t.Errorf("question does not carry %q:\n%s", want, q)
		}
	}
	if !exists(filepath.Join(l.cfg.InboxDir(), "wake")) {
		t.Fatal("the reviewer was not woken about the question")
	}
	// Intent is preserved: the producer can fix and re-declare.
	if !exists(filepath.Join(l.work, submit.BranchFile)) {
		t.Fatal("the control files were removed after a red gate; the producer's intent is lost")
	}
}

// ---------- idempotence ----------

func TestSecondSubmissionUpdatesTheExistingDraft(t *testing.T) {
	l := newLab(t)
	l.edit(t, "f")
	l.declare(t, "producer/fix", "msg")
	l.drv.found = &scm.Change{ID: "7", SourceBranch: "producer/fix", Draft: true, State: scm.StateOpen}
	r, err := submit.Run(context.Background(), l.cfg, l.deps)
	if err != nil {
		t.Fatal(err)
	}
	if r.Change == nil || r.Change.ID != "7" {
		t.Fatalf("result=%+v; a second draft was opened instead of updating the first", r)
	}
	if l.drv.opened != nil {
		t.Fatal("OpenDraft was called although a draft already existed")
	}
}
