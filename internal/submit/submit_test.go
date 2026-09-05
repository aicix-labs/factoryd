package submit_test

import (
	"bytes"
	"context"
	"errors"
	"github.com/aicix-labs/factoryd/internal/signal"
	"github.com/aicix-labs/factoryd/internal/state"
	"github.com/aicix-labs/factoryd/internal/supervise"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/gittransport"
	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/submit"
)

// ---------- fakes ----------

type fakeTransport struct {
	onPush   func()  // runs during the push; the world moves while a push is in flight
	pushErrs []error // consumed one per push: a transient failure, then success
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
	if len(f.pushErrs) > 0 {
		err := f.pushErrs[0]
		f.pushErrs = f.pushErrs[1:]
		if err != nil {
			return err
		}
	}
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
	err         error
	paths       []string
	traversable bool // what the gate identity can do to the producer's home, NOW
	probed      []string
}

func (p *fakeProvisioner) GateCanTraverse(_ context.Context, dir string) (bool, error) {
	p.probed = append(p.probed, dir)
	return p.traversable, nil
}

// provisionOpens models what a privileged chown of a path that reaches the
// home does: after it, the gate can traverse the home.
type opensOnProvision struct{ *fakeProvisioner }

func (p opensOnProvision) Provision(ctx context.Context, path string) error {
	p.fakeProvisioner.traversable = true
	return p.fakeProvisioner.Provision(ctx, path)
}

func (p *fakeProvisioner) Provision(_ context.Context, path string) error {
	p.paths = append(p.paths, path)
	return p.err
}

type fakeDriver struct {
	scm.Driver
	family    []scm.Change // what ListOpen returns; tests mutate it, even mid-gate
	openWrong bool         // OpenDraft answers with a foreign change
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
	if d.openWrong {
		// The provider answered with a change that is not the one asked for.
		return scm.Change{ID: "99", SourceBranch: "someone/else", TargetBranch: spec.TargetBranch, Draft: false, State: scm.StateOpen, Author: "stranger"}, nil
	}
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
		SchemaVersion: config.SchemaVersion, Scope: config.EmptyScope(), Health: config.DefaultHealth(), Name: "lab", Provider: "github",
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
	// The cycle names the open draft (#35): supersession moves the id, the
	// merge of that id finishes the cycle, and a refresh under an open
	// draft is refused.
	if st, err := state.Load(l.cfg.StatePath(), l.cfg.Name); err != nil || st.Cycle == nil || st.Cycle.Phase != state.CycleOpen || st.Cycle.ChangeID != "42" || st.Cycle.Family != "producer/fix" || st.Cycle.Digest != want {
		t.Fatalf("cycle not recorded as open: %+v %v", st.Cycle, err)
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

// A control file is read by root. Linked to a credential, os.ReadFile would
// hand its content to the commit message and the draft body before any push.
// The read is no-follow on a handle: the link is refused, nothing is
// committed, pushed or opened, and the secret reaches no log and no error.
func TestSymlinkedControlFileIsRefusedAndLeaksNothing(t *testing.T) {
	l := newLab(t)
	l.edit(t, "src/a.go")
	secretFile := filepath.Join(t.TempDir(), "reviewer.token")
	const secret = "FAKE-CREDENTIAL-7f3a9c2b"
	if err := os.WriteFile(secretFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secretFile, filepath.Join(l.work, submit.CommitMsgFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.work, submit.BranchFile), []byte("producer/fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	if err == nil {
		t.Fatalf("a symlinked control file was accepted; calls=%v", l.git.calls)
	}
	// Outside the workdir the root handle refuses the escape; inside it the
	// no-follow open refuses the link. Either way it is never read.
	if exitOf(t, err) != submit.ExitConfig || !(strings.Contains(err.Error(), "symlink") || strings.Contains(err.Error(), "escapes")) {
		t.Fatalf("err=%v", err)
	}
	for _, out := range []string{err.Error(), l.log.String()} {
		if strings.Contains(out, secret) {
			t.Fatalf("the secret reached an output:\n%s", out)
		}
	}
	if len(l.git.calls) != 0 || len(l.tr.pushed) != 0 || l.drv.opened != nil || l.gate.ran {
		t.Fatalf("git=%v pushed=%v opened=%v gate=%v; nothing may follow a refused control file", l.git.calls, l.tr.pushed, l.drv.opened != nil, l.gate.ran)
	}
}

// The in-tree variant: the control file links to a file INSIDE the workdir.
// The root handle would follow that; the no-follow open does not.
func TestControlFileLinkedInsideTheTreeIsRefused(t *testing.T) {
	l := newLab(t)
	l.edit(t, "src/a.go")
	if err := os.WriteFile(filepath.Join(l.work, "notes.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("notes.txt", filepath.Join(l.work, submit.CommitMsgFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.work, submit.BranchFile), []byte("producer/fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	if err == nil || !strings.Contains(err.Error(), "symlink") || len(l.git.calls) != 0 {
		t.Fatalf("err=%v git=%v", err, l.git.calls)
	}
}

// The reviewer's case: a turn exits clean but a DETACHED child keeps
// running and swaps a source file for a symlink to a credential before
// submit copies the tree. The copy is no-follow on a handle: the swap is
// refused, nothing is committed or pushed, and the secret crosses nowhere.
func TestDetachedChildSwappingASourceFileIsRefused(t *testing.T) {
	l := newLab(t)
	l.edit(t, "src/a.go")
	l.declare(t, "producer/fix", "fix")
	secretFile := filepath.Join(t.TempDir(), "reviewer.token")
	const secret = "FAKE-CREDENTIAL-detached-9a1c"
	if err := os.WriteFile(secretFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The "turn": exits at once; a setsid'd child (outside the turn's process
	// group, so the runner's reaper cannot see it) does the swap after it.
	swapped := filepath.Join(l.work, "swapped")
	turn := exec.Command("sh", "-c", "setsid sh -c 'ln -sf "+secretFile+" "+filepath.Join(l.work, "src/a.go")+"; touch "+swapped+"' >/dev/null 2>&1 & exit 0")
	if err := turn.Run(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(swapped); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fi, err := os.Lstat(filepath.Join(l.work, "src/a.go")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the detached child did not swap the file; the test proved nothing")
	}
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	if err == nil {
		t.Fatalf("the swapped tree was accepted; git=%v pushed=%v", l.git.calls, l.tr.pushed)
	}
	if !strings.Contains(err.Error(), "copytree") {
		t.Fatalf("err=%v", err)
	}
	for _, c := range l.git.calls {
		if strings.HasPrefix(c, "commit") {
			t.Fatalf("a commit was made from a swapped tree: %v", l.git.calls)
		}
	}
	if len(l.tr.pushed) != 0 || l.drv.opened != nil {
		t.Fatalf("pushed=%v opened=%v", l.tr.pushed, l.drv.opened != nil)
	}
	if b, err := os.ReadFile(filepath.Join(l.cfg.Paths.SubmitRepo, "src/a.go")); err == nil && strings.Contains(string(b), secret) {
		t.Fatal("the secret was copied into the submit repository")
	}
	if strings.Contains(l.log.String(), secret) {
		t.Fatal("the secret reached the log")
	}
}

// A control file that is not a regular file -- a fifo, which would block a
// root reader without O_NONBLOCK and reads as empty with it -- is refused
// on the opened descriptor, not read as "no work declared".
func TestFifoControlFileIsRefused(t *testing.T) {
	l := newLab(t)
	l.edit(t, "src/a.go")
	if err := syscall.Mkfifo(filepath.Join(l.work, submit.CommitMsgFile), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(l.work, submit.BranchFile), []byte("producer/fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") || len(l.git.calls) != 0 {
		t.Fatalf("err=%v git=%v", err, l.git.calls)
	}
}

// O_NOFOLLOW stops symlinks, not hard links: a hard link to a credential is
// a regular file with the credential's inode. Two links to one inode means
// the file is reachable from elsewhere, and it does not cross -- as a
// control file or as source.
func TestHardLinkedCredentialIsRefusedOnBothCrossings(t *testing.T) {
	const secret = "FAKE-CREDENTIAL-hardlink-3c9e"
	// Same filesystem as the workdir, so the link can exist at all.
	for _, where := range []string{"control", "source"} {
		t.Run(where, func(t *testing.T) {
			l := newLab(t)
			l.edit(t, "src/a.go")
			cred := filepath.Join(l.root, "reviewer.token")
			if err := os.WriteFile(cred, []byte(secret+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var target string
			if where == "control" {
				target = filepath.Join(l.work, submit.CommitMsgFile)
				os.WriteFile(filepath.Join(l.work, submit.BranchFile), []byte("producer/fix\n"), 0o644)
			} else {
				l.declare(t, "producer/fix", "fix")
				target = filepath.Join(l.work, "src", "leak.go")
			}
			if err := os.Link(cred, target); err != nil {
				t.Skipf("hard link not permitted here: %v", err)
			}
			_, err := submit.Run(context.Background(), l.cfg, l.deps)
			if err == nil || !strings.Contains(err.Error(), "links") {
				t.Fatalf("err=%v; a multi-link file crossed", err)
			}
			for _, c := range l.git.calls {
				if strings.HasPrefix(c, "commit") {
					t.Fatalf("committed: %v", l.git.calls)
				}
			}
			if len(l.tr.pushed) != 0 || l.drv.opened != nil {
				t.Fatalf("pushed=%v opened=%v", l.tr.pushed, l.drv.opened != nil)
			}
			for _, out := range []string{err.Error(), l.log.String()} {
				if strings.Contains(out, secret) {
					t.Fatalf("the secret reached an output:\n%s", out)
				}
			}
			if b, err := os.ReadFile(filepath.Join(l.cfg.Paths.SubmitRepo, "src", "leak.go")); err == nil && strings.Contains(string(b), secret) {
				t.Fatal("the secret was copied into the submit repository")
			}
		})
	}
}

// ReadIntent is the one reader both submit and the supervisor use. Neither
// file is "no intent"; anything short of a complete, valid declaration is
// an error -- a supervisor that read it as "no intent" would consume the
// trigger and strand the work.
func TestReadIntentDistinguishesNoneFromInvalid(t *testing.T) {
	cases := map[string]struct {
		setup  func(work string)
		noInt  bool
		errHas string
	}{
		"neither": {func(string) {}, true, ""},
		"both": {func(w string) {
			os.WriteFile(filepath.Join(w, submit.BranchFile), []byte("p/x\n"), 0o644)
			os.WriteFile(filepath.Join(w, submit.CommitMsgFile), []byte("m\n"), 0o644)
		}, false, ""},
		"only the message": {func(w string) { os.WriteFile(filepath.Join(w, submit.CommitMsgFile), []byte("m\n"), 0o644) }, false, "names no branch"},
		"only the branch":  {func(w string) { os.WriteFile(filepath.Join(w, submit.BranchFile), []byte("p/x\n"), 0o644) }, false, "absent or empty"},
		"empty message file": {func(w string) {
			os.WriteFile(filepath.Join(w, submit.BranchFile), []byte("p/x\n"), 0o644)
			os.WriteFile(filepath.Join(w, submit.CommitMsgFile), nil, 0o644)
		}, false, "absent or empty"},
		"dangling branch symlink": {func(w string) {
			os.Symlink("/nonexistent/target", filepath.Join(w, submit.BranchFile))
			os.WriteFile(filepath.Join(w, submit.CommitMsgFile), []byte("m\n"), 0o644)
		}, false, "symlink"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			c.setup(work)
			in, err := submit.ReadIntent(work)
			switch {
			case c.noInt:
				if !errors.Is(err, submit.ErrNoIntent) {
					t.Fatalf("err=%v, want ErrNoIntent", err)
				}
			case c.errHas != "":
				if err == nil || errors.Is(err, submit.ErrNoIntent) || !strings.Contains(err.Error(), c.errHas) {
					t.Fatalf("err=%v, want an error containing %q and not ErrNoIntent", err, c.errHas)
				}
			default:
				if err != nil || in.Branch != "p/x" || in.Message != "m" {
					t.Fatalf("in=%+v err=%v", in, err)
				}
			}
		})
	}
}

// The reviewer's case: doctor was green (the home was closed when it
// looked), the producer reopened its own home during its turn, and the
// gate would run producer-authored code with that access. The boundary is
// re-proved at the crossing, after the producer has quiesced: the gate is
// refused, nothing is provisioned, pushed or opened.
func TestGateIsRefusedWhenTheProducerReopenedItsHome(t *testing.T) {
	home := "producer-home"
	// Control: with the home closed to the gate, the gate runs and the
	// probe was made against the declared home.
	l := newLab(t)
	l.edit(t, "src/a.go")
	l.declare(t, "producer/fix", "fix")
	l.cfg.Roles.Producer.Env = map[string]string{"PATH": "/usr/bin", "HOME": filepath.Join(l.root, home)}
	l.prov.traversable = false
	if _, err := submit.Run(context.Background(), l.cfg, l.deps); err != nil {
		t.Fatalf("closed home: %v", err)
	}
	if !l.gate.ran || len(l.prov.probed) == 0 || l.prov.probed[0] != filepath.Join(l.root, home) {
		t.Fatalf("control: gate ran=%v probed=%v", l.gate.ran, l.prov.probed)
	}

	// The producer reopened its home after doctor looked.
	l = newLab(t)
	l.edit(t, "src/a.go")
	l.declare(t, "producer/fix", "fix")
	l.cfg.Roles.Producer.Env = map[string]string{"PATH": "/usr/bin", "HOME": filepath.Join(l.root, home)}
	l.prov.traversable = true
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	if err == nil || exitOf(t, err) != submit.ExitConfig || !strings.Contains(err.Error(), "traverse") {
		t.Fatalf("err=%v, want exit %d refusing the crossing", err, submit.ExitConfig)
	}
	if l.gate.ran || len(l.prov.paths) != 0 || len(l.tr.pushed) != 0 || l.drv.opened != nil {
		t.Fatalf("gate ran=%v provisioned=%v pushed=%v opened=%v; nothing may follow a reopened home", l.gate.ran, l.prov.paths, l.tr.pushed, l.drv.opened != nil)
	}
}

// A declared gate path that overlaps the producer's home would be chowned
// to the gate by this privileged process, granting exactly the traversal
// the boundary refuses. Refused before any ownership change: the
// provisioner is never called. The lab config bypasses Validate, so this
// is the second lock being tested, and the symlink case is the one the
// lexical first lock cannot see.
func TestGatePathReachingTheProducerHomeIsRefusedBeforeAnyChown(t *testing.T) {
	for name, mk := range map[string]func(l *lab, home string) string{
		"equal to the home": func(l *lab, home string) string { return home },
		"under the home":    func(l *lab, home string) string { return filepath.Join(home, ".codex") },
		"a symlink into the home": func(l *lab, home string) string {
			link := filepath.Join(l.root, "build-out")
			if err := os.Symlink(home, link); err != nil {
				t.Fatal(err)
			}
			return link
		},
		"a not-yet-existing path under a symlink into the home": func(l *lab, home string) string {
			link := filepath.Join(l.root, "cache-link")
			if err := os.Symlink(home, link); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(link, "go", "build")
		},
	} {
		t.Run(name, func(t *testing.T) {
			l := newLab(t)
			l.edit(t, "src/a.go")
			l.declare(t, "producer/fix", "fix")
			home := filepath.Join(l.root, "producer-home")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			l.cfg.Roles.Producer.Env = map[string]string{"PATH": "/usr/bin", "HOME": home}
			l.cfg.Gate.RequiredWritablePaths = []string{mk(l, home)}
			_, err := submit.Run(context.Background(), l.cfg, l.deps)
			if err == nil || exitOf(t, err) != submit.ExitConfig || !strings.Contains(err.Error(), "overlaps the producer's home") {
				t.Fatalf("err=%v", err)
			}
			if len(l.prov.paths) != 0 {
				t.Fatalf("the provisioner was called (%v); ownership would have changed before the refusal", l.prov.paths)
			}
			if l.gate.ran || len(l.tr.pushed) != 0 {
				t.Fatalf("gate ran=%v pushed=%v", l.gate.ran, l.tr.pushed)
			}
		})
	}
}

// Defense in depth: a provisioning that reached the home by a route neither
// lock saw is caught by the re-probe after provisioning; the gate does not
// run.
func TestGateIsRefusedIfProvisioningOpenedTheHome(t *testing.T) {
	l := newLab(t)
	l.edit(t, "src/a.go")
	l.declare(t, "producer/fix", "fix")
	home := filepath.Join(l.root, "producer-home")
	os.MkdirAll(home, 0o700)
	l.cfg.Roles.Producer.Env = map[string]string{"PATH": "/usr/bin", "HOME": home}
	l.cfg.Gate.RequiredWritablePaths = []string{"build/out"} // innocent on its face
	l.deps.Provision = opensOnProvision{l.prov}
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	if err == nil || exitOf(t, err) != submit.ExitConfig || !strings.Contains(err.Error(), "AFTER provisioning") {
		t.Fatalf("err=%v", err)
	}
	if l.gate.ran || len(l.tr.pushed) != 0 {
		t.Fatalf("gate ran=%v pushed=%v after the home was opened by provisioning", l.gate.ran, l.tr.pushed)
	}
}

// The differing-CWD regression: a relative producer HOME, with submit
// running from a directory that is not the producer workdir. The probe
// must not be made against the wrong directory and come back green;
// submit refuses the home as relative, before the gate.
func TestRelativeProducerHomeIsRefusedFromADifferentCWD(t *testing.T) {
	l := newLab(t)
	l.edit(t, "src/a.go")
	l.declare(t, "producer/fix", "fix")
	l.cfg.Roles.Producer.Env = map[string]string{"PATH": "/usr/bin", "HOME": "private"}
	// Plant a closed "private" under the producer workdir (what the turn
	// means) AND an open one under submit's own cwd (what a naive probe
	// would see).
	if err := os.MkdirAll(filepath.Join(l.work, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(elsewhere, "private"), 0o755); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if err := os.Chdir(elsewhere); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })
	l.prov.traversable = false // a naive probe would report green
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	if err == nil || exitOf(t, err) != submit.ExitConfig || !strings.Contains(err.Error(), "relative") {
		t.Fatalf("err=%v; a relative home must be refused, not probed in the wrong directory", err)
	}
	if len(l.prov.probed) != 0 || l.gate.ran || len(l.tr.pushed) != 0 {
		t.Fatalf("probed=%v gate=%v pushed=%v", l.prov.probed, l.gate.ran, l.tr.pushed)
	}
}

func TestDeclaredFamilyInvertsImmutableBranch(t *testing.T) {
	for declared, tree := range map[string]string{"producer/fix": "0123456789abcdef", "a-b-c": "fedcba9876543210", "x": "abcdef0123456789"} {
		if got := submit.DeclaredFamily(submit.ImmutableBranch(declared, tree)); got != declared {
			t.Fatalf("DeclaredFamily(ImmutableBranch(%q, %q)) = %q", declared, tree, got)
		}
	}
	// Not a pushed name: returned unchanged, never mangled.
	for _, plain := range []string{"producer/fix", "release-2026", "fix-0123456789ab", "fix-ZZZZZZZZZZ"} {
		if got := submit.DeclaredFamily(plain); got != plain {
			t.Fatalf("DeclaredFamily(%q) = %q, want unchanged", plain, got)
		}
	}
}

// The cycle says "submitting" before the push, so a crash between the
// draft's creation and the record of it leaves a phase that forbids a
// refresh, never an absence that permits one (#41 review). Proved by a
// push that fails: the record is already there.
func TestCycleIsRecordedAsSubmittingBeforeThePush(t *testing.T) {
	l := newLab(t)
	l.edit(t, "src/a.go")
	l.declare(t, "producer/fix", "gate: match command position\n\nlonger body")
	var seen *state.Cycle
	l.tr.onPush = func() {
		st, err := state.Load(l.cfg.StatePath(), l.cfg.Name)
		if err != nil {
			t.Error(err)
			return
		}
		seen = st.Cycle
		panic("crash during the push")
	}
	func() {
		defer func() { recover() }()
		submit.Run(context.Background(), l.cfg, l.deps)
	}()
	want := submit.ImmutableBranch("producer/fix", "abc123")
	if seen == nil || seen.Phase != state.CycleSubmitting || seen.Family != "producer/fix" || seen.Digest != want {
		t.Fatalf("cycle at push time = %+v, want submitting %s/%s", seen, "producer/fix", want)
	}
	st, _ := state.Load(l.cfg.StatePath(), l.cfg.Name)
	if st.Cycle == nil || st.Cycle.Phase != state.CycleSubmitting {
		t.Fatalf("after the crash the cycle is %+v, want submitting", st.Cycle)
	}
}

// A queued handoff reserves the cycle until the producer command exits. An
// external/root-side submit in the gap after handoff confirmation but before
// process start must not turn that reservation into submitting and create work
// beside the queued agent.
func TestSubmitRefusesAnUnfinishedQueuedHandoff(t *testing.T) {
	l := newLab(t)
	l.edit(t, "src/a.go")
	l.declare(t, "producer/fix", "gate: queued lifecycle\n\nbody")
	if _, err := state.Update(l.cfg.StatePath(), l.cfg.Name, func(st *state.State) error {
		st.SetCycle(state.CycleWorking, time.Now())
		st.Role(state.RoleProducer).QueueReservation = &state.QueueReservation{
			Source:          filepath.Join(l.cfg.BriefsDir(), "010-next.md"),
			Done:            filepath.Join(l.cfg.BriefsDoneDir(), "010-next.md"),
			Turn:            "producer-queued",
			ReservedAt:      time.Now(),
			Taken:           true,
			ProcessStarted:  false,
			ProcessFinished: false,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	var got *submit.Error
	if !errors.As(err, &got) || got.Kind != supervise.DispositionBlocked || !errors.Is(err, state.ErrProducerLifecycleBusy) {
		t.Fatalf("submit error=%v, want blocked active queued-handoff refusal", err)
	}
	if len(l.tr.pushed) != 0 {
		t.Fatalf("submit pushed beside an unfinished queued handoff: %v", l.tr.pushed)
	}
	st, err := state.Load(l.cfg.StatePath(), l.cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if st.Cycle == nil || st.Cycle.Phase != state.CycleWorking {
		t.Fatalf("submit changed active queued cycle: %+v", st.Cycle)
	}
}

// The initial handoff fence is deliberately before even a red gate: running
// one against the queued producer's partial worktree would create a false
// reviewer workflow. In particular, it must not leave question.md or wake
// behind for a reviewer to mistake for the queued turn's result.
func TestSubmitDoesNotRunOrReportARedGateBesideAnUnfinishedQueuedHandoff(t *testing.T) {
	l := newLab(t)
	l.edit(t, "src/a.go")
	l.declare(t, "producer/fix", "gate: queued lifecycle\n\nbody")
	l.gate.exit = 1
	if _, err := state.Update(l.cfg.StatePath(), l.cfg.Name, func(st *state.State) error {
		st.SetCycle(state.CycleWorking, time.Now())
		st.Role(state.RoleProducer).QueueReservation = &state.QueueReservation{
			Source:          filepath.Join(l.cfg.BriefsDir(), "010-next.md"),
			Done:            filepath.Join(l.cfg.BriefsDoneDir(), "010-next.md"),
			Turn:            "producer-queued",
			ReservedAt:      time.Now(),
			Taken:           true,
			ProcessStarted:  false,
			ProcessFinished: false,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	_, err := submit.Run(context.Background(), l.cfg, l.deps)
	var got *submit.Error
	if !errors.As(err, &got) || got.Kind != supervise.DispositionBlocked || !errors.Is(err, state.ErrProducerLifecycleBusy) {
		t.Fatalf("submit error=%v, want blocked active queued-handoff refusal", err)
	}
	if l.gate.ran {
		t.Fatal("submit ran a red gate beside an active queued handoff")
	}
	for _, name := range []string{"question.md", "wake"} {
		if exists(filepath.Join(l.cfg.InboxDir(), name)) {
			t.Fatalf("submit wrote inbox/%s beside an active queued handoff", name)
		}
	}
}

// reviewerDriver is the fast reviewer's view of the provider for the
// interleaving test: the draft submit just opened, mergeable at once.
type reviewerDriver struct {
	scm.Driver
	change scm.Change
}

func (d *reviewerDriver) Provider() string { return "fake" }
func (d *reviewerDriver) Get(context.Context, scm.ChangeID) (scm.Change, error) {
	return d.change, nil
}
func (d *reviewerDriver) Diff(context.Context, scm.ChangeID) ([]scm.FileDiff, error) {
	return []scm.FileDiff{{Path: "src/a.go", Added: 1, Patch: "+++ b/src/a.go\n+package a\n"}}, nil
}
func (d *reviewerDriver) Audits(context.Context, scm.ChangeID, string) ([]scm.Audit, error) {
	return nil, nil
}
func (d *reviewerDriver) SetDraft(context.Context, scm.ChangeID, bool) error { return nil }
func (d *reviewerDriver) Merge(context.Context, scm.ChangeID, string) (scm.ProviderMerge, error) {
	return scm.ProviderMerged("m3rge"), nil
}
func (d *reviewerDriver) IsAncestor(context.Context, string, string) (bool, error) { return true, nil }
func (d *reviewerDriver) Comment(context.Context, scm.ChangeID, string) error      { return nil }

// The reviewer is woken only after the open draft is recorded, and a
// reviewer fast enough to merge and signal inside the wake itself leaves
// the cycle finished, not open (#41 review). The merge runs through the
// real signal.Run, deterministically, from the Wake hook.
func TestReviewerWokenAfterTheRecordAndAMergeInsideTheWakeConverges(t *testing.T) {
	l := newLab(t)
	l.edit(t, "src/a.go")
	l.declare(t, "producer/fix", "gate: match command position\n\nlonger body")
	want := submit.ImmutableBranch("producer/fix", "abc123")
	var atWake *state.Cycle
	l.deps.Wake = func() error {
		st, err := state.Load(l.cfg.StatePath(), l.cfg.Name)
		if err != nil {
			return err
		}
		atWake = st.Cycle
		// The fast reviewer: review, merge, signal -- before submit returns.
		rd := &reviewerDriver{change: scm.Change{ID: "42", State: scm.StateOpen, Draft: false, Author: "producer-bot", AuthorID: "1",
			HeadSHA: "abc123", SourceBranch: want, TargetBranch: "main"}}
		_, err = signal.Run(context.Background(), l.cfg, signal.Deps{Driver: rd, Reviewer: scm.Identity{ID: "2", Login: "factory-reviewer"}},
			signal.Request{ID: "42", Kind: "merged", SHA: "auto", Summary: "clean"})
		return err
	}
	r, err := submit.Run(context.Background(), l.cfg, l.deps)
	if err != nil || r.Exit != submit.ExitSubmitted {
		t.Fatalf("r=%+v err=%v\n%s", r, err, l.log)
	}
	if atWake == nil || atWake.Phase != state.CycleOpen || atWake.ChangeID != "42" {
		t.Fatalf("at the wake the cycle was %+v, want open 42: the reviewer was woken before the record", atWake)
	}
	st, _ := state.Load(l.cfg.StatePath(), l.cfg.Name)
	if st.Cycle == nil || st.Cycle.Phase != state.CycleFinished || st.Cycle.ChangeID != "42" {
		t.Fatalf("after a merge inside the wake the cycle is %+v, want finished 42", st.Cycle)
	}
}

// The other ordering, forced: the merged verdict for the draft is on file
// before submit records the open draft. The record converges to finished.
func TestAMergedVerdictAlreadyOnFileFinishesTheCycleAtRecordTime(t *testing.T) {
	l := newLab(t)
	l.edit(t, "src/a.go")
	l.declare(t, "producer/fix", "gate: match command position\n\nlonger body")
	if _, err := state.Update(l.cfg.StatePath(), l.cfg.Name, func(st *state.State) error {
		st.LastVerdict = &state.Verdict{ChangeID: "42", Kind: state.VerdictMerged, MergeCommit: "m3rge"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if r, err := submit.Run(context.Background(), l.cfg, l.deps); err != nil || r.Exit != submit.ExitSubmitted {
		t.Fatalf("r=%+v err=%v", r, err)
	}
	st, _ := state.Load(l.cfg.StatePath(), l.cfg.Name)
	if st.Cycle == nil || st.Cycle.Phase != state.CycleFinished {
		t.Fatalf("cycle %+v, want finished: the record buried the verdict", st.Cycle)
	}
}

// A turn that declared nothing, or a tree identical to the target, ends
// the cycle clean; a tree already submitted names its draft (#41 review).
func TestNoWorkTurnsEndTheCycleCleanAndAnAlreadySubmittedTreeNamesItsDraft(t *testing.T) {
	l := newLab(t)
	if _, err := state.Update(l.cfg.StatePath(), l.cfg.Name, func(st *state.State) error {
		st.SetCycle(state.CycleWorking, time.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// No intent, through the supervisor's after-turn step (never builds deps).
	after := submit.AfterTurn(l.cfg, func(context.Context) (submit.Deps, error) {
		t.Fatal("deps built for a no-intent turn")
		return submit.Deps{}, nil
	})
	if msg, err := after(context.Background(), supervise.Turn{}, supervise.TurnResult{}); err != nil || !strings.Contains(msg, "no intent") {
		t.Fatalf("%q %v", msg, err)
	}
	st, _ := state.Load(l.cfg.StatePath(), l.cfg.Name)
	if st.Cycle.Phase != state.CycleClean {
		t.Fatalf("after a no-intent turn the cycle is %s, want clean", st.Cycle.Phase)
	}

	// Identical tree: declared, but nothing to commit.
	l2 := newLab(t)
	state.Update(l2.cfg.StatePath(), l2.cfg.Name, func(st *state.State) error { st.SetCycle(state.CycleWorking, time.Now()); return nil })
	l2.declare(t, "producer/fix", "msg")
	l2.git.nothing = true
	if r, err := submit.Run(context.Background(), l2.cfg, l2.deps); err != nil || r.Exit != submit.ExitNothing {
		t.Fatalf("r=%+v err=%v", r, err)
	}
	st, _ = state.Load(l2.cfg.StatePath(), l2.cfg.Name)
	if st.Cycle.Phase != state.CycleClean {
		t.Fatalf("after an identical-tree turn the cycle is %s, want clean", st.Cycle.Phase)
	}

	// Already submitted: the same tree is an open draft.
	l3 := newLab(t)
	state.Update(l3.cfg.StatePath(), l3.cfg.Name, func(st *state.State) error { st.SetCycle(state.CycleWorking, time.Now()); return nil })
	l3.edit(t, "src/a.go")
	l3.declare(t, "producer/fix", "msg")
	want := submit.ImmutableBranch("producer/fix", "abc123")
	l3.drv.family = []scm.Change{{ID: "41", State: scm.StateOpen, Draft: true, Author: "producer-bot", AuthorID: "1", SourceBranch: want, TargetBranch: "main"}}
	r, err := submit.Run(context.Background(), l3.cfg, l3.deps)
	if err != nil || r.Exit != submit.ExitNothing || r.Change == nil || r.Change.ID != "41" {
		t.Fatalf("r=%+v err=%v\n%s", r, err, l3.log)
	}
	st, _ = state.Load(l3.cfg.StatePath(), l3.cfg.Name)
	if st.Cycle.Phase != state.CycleOpen || st.Cycle.ChangeID != "41" {
		t.Fatalf("after an already-submitted turn the cycle is %+v, want open 41", st.Cycle)
	}

	// A no-op turn while a draft is open says nothing about the draft.
	l4 := newLab(t)
	state.Update(l4.cfg.StatePath(), l4.cfg.Name, func(st *state.State) error {
		c := st.SetCycle(state.CycleOpen, time.Now())
		c.ChangeID = "7"
		return nil
	})
	after4 := submit.AfterTurn(l4.cfg, func(context.Context) (submit.Deps, error) { return submit.Deps{}, nil })
	after4(context.Background(), supervise.Turn{}, supervise.TurnResult{})
	st, _ = state.Load(l4.cfg.StatePath(), l4.cfg.Name)
	if st.Cycle.Phase != state.CycleOpen {
		t.Fatalf("a no-intent turn changed an open cycle to %s", st.Cycle.Phase)
	}
}
