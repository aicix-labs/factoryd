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
	onPush   func() // runs during the push; the world moves while a push is in flight
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
	if f.onPush != nil {
		f.onPush()
	}
	return nil
}

type fakeGit struct {
	tree     string // what Tree returns
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
func (g *fakeGit) Tree(context.Context) (string, error) {
	g.calls = append(g.calls, "tree")
	return g.tree, nil
}

func (g *fakeGit) Status(context.Context) ([]string, error) { return nil, nil }

type fakeGate struct {
	exit   int
	err    error
	ran    bool
	env    []string
	calls  *[]string
	during func() // runs while the gate "runs"
}

func (g *fakeGate) Run(_ context.Context, _ *config.Config, exe string, env []string, out io.Writer) (int, error) {
	g.ran = true
	g.env = env
	if g.calls != nil {
		*g.calls = append(*g.calls, "gate")
	}
	if g.during != nil {
		g.during()
	}
	return g.exit, g.err
}

type fakeProvisioner struct {
	err   error
	paths []string
}

func (p *fakeProvisioner) Provision(_ context.Context, path string) error {
	p.paths = append(p.paths, path)
	return p.err
}

type fakeDriver struct {
	scm.Driver
	family    []scm.Change // what ListOpen returns; tests mutate it, even mid-gate
	after     *scm.Change  // what Get returns after the push; nil means the family entry
	afterList func()       // runs after every ListOpen: state that changes the instant after it was read
	opened    *scm.DraftSpec
	closed    []scm.ChangeID
	calls     *[]string
}

func (d *fakeDriver) ListOpen(context.Context) ([]scm.Change, error) {
	if d.calls != nil {
		*d.calls = append(*d.calls, "list")
	}
	out := append([]scm.Change(nil), d.family...)
	if d.afterList != nil {
		d.afterList() // the world moves the instant after a read returns
	}
	return out, nil
}

func (d *fakeDriver) Close(_ context.Context, id scm.ChangeID, _ string) error {
	d.closed = append(d.closed, id)
	return nil
}

func (d *fakeDriver) Get(_ context.Context, id scm.ChangeID) (scm.Change, error) {
	if d.calls != nil {
		*d.calls = append(*d.calls, "get")
	}
	if d.after != nil {
		return *d.after, nil
	}
	for _, c := range d.family {
		if c.ID == id {
			return c, nil
		}
	}
	return scm.Change{ID: id, Draft: true, State: scm.StateOpen, Author: "producer-bot"}, nil
}

func (d *fakeDriver) FindOpenBySource(_ context.Context, branch string) (scm.Change, bool, error) {
	for _, c := range d.family {
		if c.SourceBranch == branch {
			return c, true, nil
		}
	}
	return scm.Change{}, false, nil
}
func (d *fakeDriver) OpenDraft(_ context.Context, spec scm.DraftSpec) (scm.Change, error) {
	if d.calls != nil {
		*d.calls = append(*d.calls, "open")
	}
	d.opened = &spec
	return scm.Change{ID: "42", SourceBranch: spec.SourceBranch, TargetBranch: spec.TargetBranch, Draft: true, State: scm.StateOpen, Author: "producer-bot"}, nil
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
	prov *fakeProvisioner
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
	l.git = &fakeGit{commitOK: "c0mm1t", tree: "abc123"} // distinct: a branch named after the commit is wrong
	l.gate = &fakeGate{exit: 0, calls: calls}
	l.drv = &fakeDriver{calls: calls}
	t.Cleanup(func() {
		if len(l.drv.closed) != 0 {
			t.Errorf("submit closed %v; submit never writes to an existing change (a read is stale by the time a write lands, and no provider offers a conditional close)", l.drv.closed)
		}
	})
	l.prov = &fakeProvisioner{}
	l.deps = submit.Deps{
		Driver: l.drv, Transport: l.tr, Git: l.git, Gate: l.gate, Provision: l.prov,
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
	want := submit.ImmutableBranch("producer/fix", "abc123")
	if len(l.tr.pushed) != 1 || l.tr.pushed[0] != "refs/heads/"+want+":refs/heads/"+want {
		t.Fatalf("pushed %v, want a non-force push of the content-derived branch %s", l.tr.pushed, want)
	}
	if r.Branch != want {
		t.Fatalf("result branch = %s, want %s", r.Branch, want)
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
	if all != "list,gate,list,open" {
		t.Fatalf("driver/gate order = %s, want list,gate,list,open (the family is re-read after the gate, immediately before the push)", all)
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
	// Unresolvable: references a variable gate.env does not set.
	l.cfg.Gate.RequiredWritablePaths = []string{"${NOPE}/cache"}
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

func TestSameTreeIsAlreadySubmitted(t *testing.T) {
	l := newLab(t)
	l.edit(t, "f")
	l.declare(t, "producer/fix", "msg")
	same := submit.ImmutableBranch("producer/fix", "abc123")
	l.drv.family = []scm.Change{{ID: "7", SourceBranch: same, TargetBranch: "main", Draft: true, State: scm.StateOpen, Author: "producer-bot"}}
	r, err := submit.Run(context.Background(), l.cfg, l.deps)
	if err != nil {
		t.Fatal(err)
	}
	if r.Exit != submit.ExitNothing || r.Change == nil || r.Change.ID != "7" {
		t.Fatalf("result=%+v; the same tree is already submitted as 7 and that is the answer", r)
	}
	if l.gate.ran || len(l.tr.pushed) != 0 || l.drv.opened != nil || len(l.drv.closed) != 0 {
		t.Fatalf("gate=%v pushed=%v opened=%v closed=%v for a tree already submitted", l.gate.ran, l.tr.pushed, l.drv.opened != nil, l.drv.closed)
	}
}

// ---------- the five findings from the second review ----------

// A declared gate path is provisioned FOR the gate and proven writable BY the
// gate right before the gate runs. CopyTree cleared the work tree; created as
// factoryd's, build/out is root-owned and the gate's first write fails -- which
// would be reported as a red branch. Unprovisionable is exit 3, gate never ran.
func TestUnprovisionableGatePathIsExit3NotARedGate(t *testing.T) {
	l := newLab(t)
	l.edit(t, "f")
	l.declare(t, "producer/fix", "msg")
	l.prov.err = errors.New("factoryd-gate (uid 995) cannot write build/out after provisioning")
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	if exitOf(t, err) != submit.ExitConfig {
		t.Fatalf("exit %d, want 3: a host permission problem was reported as the producer's fault: %v", exitOf(t, err), err)
	}
	if l.gate.ran {
		t.Fatal("the gate ran against a path it cannot write")
	}
	if len(l.prov.paths) != 1 || !strings.HasSuffix(l.prov.paths[0], "build/out") {
		t.Fatalf("provisioned %v, want the declared path", l.prov.paths)
	}
}

// An existing change is validated BEFORE the push. A reviewer who marked it
// ready has taken it out of the producer's hands; pushing would put new code
// into it.
func TestExistingChangeNotOursRefusesBeforePush(t *testing.T) {
	base := scm.Change{ID: "7", SourceBranch: "producer/fix", TargetBranch: "main", Draft: true, State: scm.StateOpen, Author: "producer-bot"}
	cases := map[string]func(c *scm.Change){
		"marked ready":   func(c *scm.Change) { c.Draft = false },
		"wrong target":   func(c *scm.Change) { c.TargetBranch = "release" },
		"closed":         func(c *scm.Change) { c.State = scm.StateClosed },
		"another author": func(c *scm.Change) { c.Author = "someone-else" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			l := newLab(t)
			l.edit(t, "f")
			l.declare(t, "producer/fix", "msg")
			c := base
			c.SourceBranch = submit.ImmutableBranch("producer/fix", "abc123")
			mutate(&c)
			l.drv.family = []scm.Change{c}
			_, err := submit.Run(context.Background(), l.cfg, l.deps)
			if exitOf(t, err) != submit.ExitConfig {
				t.Fatalf("exit %d, want 3: %v", exitOf(t, err), err)
			}
			if len(l.tr.pushed) != 0 {
				t.Fatalf("pushed into a change that is %s", name)
			}
			if l.gate.ran {
				t.Fatal("the gate ran for a change that would never be pushed")
			}
		})
	}
}

// The provider's response to OpenDraft is validated in full.
func TestOpenDraftResponseIsValidatedInFull(t *testing.T) {
	l := newLab(t)
	l.edit(t, "f")
	l.declare(t, "producer/fix", "msg")
	wrong := &fakeDriver{calls: l.gate.calls}
	l.deps.Driver = wrongTargetDriver{fakeDriver: wrong}
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	if exitOf(t, err) != submit.ExitConfig || !strings.Contains(err.Error(), "not the change that was requested") {
		t.Fatalf("exit %d: %v", exitOf(t, err), err)
	}
}

type wrongTargetDriver struct{ *fakeDriver }

func (d wrongTargetDriver) OpenDraft(_ context.Context, spec scm.DraftSpec) (scm.Change, error) {
	return scm.Change{ID: "42", SourceBranch: spec.SourceBranch, TargetBranch: "release", Draft: true, State: scm.StateOpen, Author: "producer-bot"}, nil
}

// roles.producer.workdir overrides the default; submit must read and copy
// from where the producer actually ran, or it reports "nothing" while the
// declared work sits elsewhere.
func TestSubmitHonoursTheProducerWorkdirOverride(t *testing.T) {
	l := newLab(t)
	override := filepath.Join(l.root, "elsewhere")
	if err := os.MkdirAll(override, 0o755); err != nil {
		t.Fatal(err)
	}
	l.cfg.Roles.Producer.Workdir = override
	// The work is in the override, not the default.
	if err := os.WriteFile(filepath.Join(override, "real.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(override, submit.BranchFile), []byte("producer/fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(override, submit.CommitMsgFile), []byte("msg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := submit.Run(context.Background(), l.cfg, l.deps)
	if err != nil {
		t.Fatal(err)
	}
	if r.Exit != submit.ExitSubmitted {
		t.Fatalf("exit %d, want 0: the override's declared work was not seen", r.Exit)
	}
	if !exists(filepath.Join(l.cfg.Paths.SubmitRepo, "real.go")) {
		t.Fatal("the override's tree was not the one copied")
	}
}

// A red gate whose wake cannot be written leaves the reviewer unsignalled --
// the stall again, one step later. It is an infrastructure failure, exit 3.
func TestRedGateWithUnsignallableReviewerIsExit3(t *testing.T) {
	l := newLab(t)
	l.edit(t, "f")
	l.declare(t, "producer/fix", "msg")
	l.gate.exit = 1
	// The question can be written; the wake cannot: a directory sits where
	// the file must go.
	if err := os.MkdirAll(filepath.Join(l.cfg.InboxDir(), "wake"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	if exitOf(t, err) != submit.ExitConfig || !strings.Contains(err.Error(), "could not be signalled") {
		t.Fatalf("exit %d: %v", exitOf(t, err), err)
	}
	if !exists(filepath.Join(l.cfg.InboxDir(), "question.md")) {
		t.Fatal("the question was not written before the wake failed")
	}
}

// The gate can run for a long time. A draft that a reviewer marks ready
// while it runs has left the producer's hands; the push must not happen at
// all, not merely be noticed afterwards.
func TestDraftFlippedDuringGateIsNotPushed(t *testing.T) {
	l := newLab(t)
	l.edit(t, "src/a.go")
	l.declare(t, "producer/fix", "fix")
	old := submit.ImmutableBranch("producer/fix", "0ld0ld0ld0")
	l.drv.family = []scm.Change{{ID: "7", SourceBranch: old, TargetBranch: "main", Draft: true, State: scm.StateOpen, Author: "producer-bot"}}
	l.gate.during = func() { l.drv.family[0].Draft = false }
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	if exitOf(t, err) != submit.ExitConfig {
		t.Fatalf("err=%v, want exit %d", err, submit.ExitConfig)
	}
	if !l.gate.ran {
		t.Fatal("the gate did not run; the flip happened nowhere")
	}
	if len(l.tr.pushed) != 0 {
		t.Fatalf("pushed %v after the draft was marked ready during the gate", l.tr.pushed)
	}
	if l.drv.opened != nil || len(l.drv.closed) != 0 {
		t.Fatalf("opened=%v closed=%v; the reviewer's change must be left exactly as it is", l.drv.opened != nil, l.drv.closed)
	}
}

// Both drivers map an omitted author to "". An unknown owner is not ours.
func TestChangeWithNoAuthorIsNotOurs(t *testing.T) {
	l := newLab(t)
	l.edit(t, "src/a.go")
	l.declare(t, "producer/fix", "fix")
	same := submit.ImmutableBranch("producer/fix", "abc123")
	l.drv.family = []scm.Change{{ID: "7", SourceBranch: same, TargetBranch: "main", Draft: true, State: scm.StateOpen, Author: ""}}
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	if exitOf(t, err) != submit.ExitConfig || !strings.Contains(err.Error(), "no author") {
		t.Fatalf("err=%v, want exit %d refusing the authorless change", err, submit.ExitConfig)
	}
	if l.gate.ran || len(l.tr.pushed) != 0 {
		t.Fatalf("gate ran=%v pushed=%v; nothing may proceed on an unknown owner", l.gate.ran, l.tr.pushed)
	}
}

// A producer whose own login is unknown cannot establish ownership of anything.
func TestProducerWithoutLoginOwnsNothing(t *testing.T) {
	l := newLab(t)
	l.edit(t, "src/a.go")
	l.declare(t, "producer/fix", "fix")
	l.deps.Producer.Login = ""
	l.tr.login = "" // both mechanisms agree on an empty login; ownership is what is tested
	same := submit.ImmutableBranch("producer/fix", "abc123")
	l.drv.family = []scm.Change{{ID: "7", SourceBranch: same, TargetBranch: "main", Draft: true, State: scm.StateOpen, Author: "producer-bot"}}
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	if exitOf(t, err) != submit.ExitConfig || !strings.Contains(err.Error(), "no login") {
		t.Fatalf("err=%v, want exit %d", err, submit.ExitConfig)
	}
	if len(l.tr.pushed) != 0 {
		t.Fatalf("pushed %v", l.tr.pushed)
	}
}

// New content is a new branch and a new draft; the earlier draft in the
// family is closed with a pointer to it. It is never pushed into.
func TestNewContentSupersedesEarlierDraft(t *testing.T) {
	l := newLab(t)
	l.edit(t, "src/a.go")
	l.declare(t, "producer/fix", "fix")
	old := submit.ImmutableBranch("producer/fix", "0ld0ld0ld0")
	l.drv.family = []scm.Change{{ID: "7", SourceBranch: old, TargetBranch: "main", Draft: true, State: scm.StateOpen, Author: "producer-bot"}}
	r, err := submit.Run(context.Background(), l.cfg, l.deps)
	if err != nil {
		t.Fatal(err)
	}
	want := submit.ImmutableBranch("producer/fix", "abc123")
	if l.drv.opened == nil || l.drv.opened.SourceBranch != want {
		t.Fatalf("opened=%+v, want a new draft on %s", l.drv.opened, want)
	}
	if len(l.tr.pushed) != 1 || strings.Contains(l.tr.pushed[0], old) {
		t.Fatalf("pushed %v; the old branch must never be pushed to", l.tr.pushed)
	}
	if len(l.drv.closed) != 0 {
		t.Fatalf("closed %v; the superseded draft is reported, never closed", l.drv.closed)
	}
	if len(r.Supersedes) != 1 || r.Supersedes[0] != "7" {
		t.Fatalf("result.Supersedes=%v, want [7]", r.Supersedes)
	}
	if !strings.Contains(l.drv.opened.Body, "supersedes 7") {
		t.Fatalf("new draft body does not name the superseded draft:\n%s", l.drv.opened.Body)
	}
	if r.Change == nil || r.Change.ID == "7" {
		t.Fatalf("result=%+v", r)
	}
}

// A ready change anywhere in the family is the reviewer's: the submission
// stops before the gate spends anything, and the change is untouched.
func TestReadySiblingStopsBeforeGate(t *testing.T) {
	l := newLab(t)
	l.edit(t, "src/a.go")
	l.declare(t, "producer/fix", "fix")
	old := submit.ImmutableBranch("producer/fix", "0ld0ld0ld0")
	l.drv.family = []scm.Change{{ID: "7", SourceBranch: old, TargetBranch: "main", Draft: false, State: scm.StateOpen, Author: "producer-bot"}}
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	if exitOf(t, err) != submit.ExitConfig {
		t.Fatalf("err=%v, want exit %d", err, submit.ExitConfig)
	}
	if l.gate.ran || len(l.tr.pushed) != 0 || len(l.drv.closed) != 0 {
		t.Fatalf("gate=%v pushed=%v closed=%v", l.gate.ran, l.tr.pushed, l.drv.closed)
	}
}

func TestImmutableBranchIsContentDerived(t *testing.T) {
	a := submit.ImmutableBranch("producer/fix", "0123456789abcdef")
	b := submit.ImmutableBranch("producer/fix", "fedcba9876543210")
	if a == b || a != "producer/fix-0123456789" {
		t.Fatalf("a=%s b=%s", a, b)
	}
	if submit.ImmutableBranch("x", "ab") != "x-ab" {
		t.Fatal("short sha")
	}
}

// The reviewer's case: the older draft is marked ready the instant after
// submit's LAST read of it (the pre-push family read), and again during the
// push for good measure. Close must never be invoked: the read was stale
// when the write would have landed, and there is no conditional close.
// The new content still goes up as its own draft, naming the older one.
func TestOlderDraftFlippedAfterLastReadIsNeverClosed(t *testing.T) {
	l := newLab(t)
	l.edit(t, "src/a.go")
	l.declare(t, "producer/fix", "fix")
	old := submit.ImmutableBranch("producer/fix", "0ld0ld0ld0")
	l.drv.family = []scm.Change{{ID: "7", SourceBranch: old, TargetBranch: "main", Draft: true, State: scm.StateOpen, Author: "producer-bot"}}
	lists := 0
	l.drv.afterList = func() {
		lists++
		if lists == 2 { // the pre-push read is the second and last
			l.drv.family[0].Draft = false
		}
	}
	l.tr.onPush = func() { l.drv.family[0].Draft = false }
	r, err := submit.Run(context.Background(), l.cfg, l.deps)
	if err != nil {
		t.Fatal(err)
	}
	if lists != 2 {
		t.Fatalf("ListOpen called %d times; the flip did not happen after the last read", lists)
	}
	if len(l.tr.pushed) != 1 || l.drv.opened == nil {
		t.Fatalf("pushed=%v opened=%v", l.tr.pushed, l.drv.opened != nil)
	}
	if len(l.drv.closed) != 0 {
		t.Fatalf("Close invoked on %v after the draft was marked ready; that is the reviewer's change", l.drv.closed)
	}
	if len(r.Supersedes) != 1 || r.Supersedes[0] != "7" {
		t.Fatalf("Supersedes=%v", r.Supersedes)
	}
}

// The branch names the tree, not the commit: the same content committed
// again -- new timestamp, new commit sha -- must map to the same draft.
func TestSameTreeRecommittedIsAlreadySubmitted(t *testing.T) {
	l := newLab(t)
	l.edit(t, "src/a.go")
	l.declare(t, "producer/fix", "fix")
	l.git.commitOK = "different-commit-sha"
	same := submit.ImmutableBranch("producer/fix", "abc123")
	l.drv.family = []scm.Change{{ID: "7", SourceBranch: same, TargetBranch: "main", Draft: true, State: scm.StateOpen, Author: "producer-bot"}}
	r, err := submit.Run(context.Background(), l.cfg, l.deps)
	if err != nil {
		t.Fatal(err)
	}
	if r.Exit != submit.ExitNothing || r.Change == nil || r.Change.ID != "7" || len(l.tr.pushed) != 0 {
		t.Fatalf("result=%+v pushed=%v; a recommit of the same tree is not new content", r, l.tr.pushed)
	}
}
