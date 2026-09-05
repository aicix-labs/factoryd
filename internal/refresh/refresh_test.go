package refresh_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aicix-labs/factoryd/internal/brief"
	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/proc"
	"github.com/aicix-labs/factoryd/internal/refresh"
	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/state"
	"github.com/aicix-labs/factoryd/internal/submit"
	"github.com/aicix-labs/factoryd/internal/supervise"
	"github.com/aicix-labs/factoryd/internal/watch"
)

func cfgFor(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	return &config.Config{Name: "f", TargetBranch: "main", Paths: config.Paths{Root: root, ProducerWorkdir: filepath.Join(root, "work")}}
}

// Run is fetch, bundle, apply, and the apply is believed only when the
// workdir's HEAD is the bundle's tip.
func TestRunVerifiesTheWorkdirByShaNotByExit(t *testing.T) {
	cfg := cfgFor(t)
	var seq []string
	deps := refresh.Deps{
		Fetch: func(_ context.Context, spec string) error {
			seq = append(seq, "fetch "+spec)
			return nil
		},
		Bundle: func(_ context.Context, path, ref string) (string, error) {
			seq = append(seq, "bundle "+ref)
			if path != refresh.BundlePath(cfg) {
				t.Fatalf("bundle path %s", path)
			}
			return "abc123\n", nil
		},
		Apply: func(_ context.Context, bundle, branch string) (string, error) {
			seq = append(seq, "apply "+branch)
			return "abc123", nil
		},
	}
	r, err := refresh.Run(context.Background(), cfg, deps)
	if err != nil || r.SHA != "abc123" {
		t.Fatalf("r=%+v err=%v", r, err)
	}
	want := "fetch +refs/heads/main:refs/remotes/factoryd/main|bundle refs/remotes/factoryd/main|apply main"
	if got := strings.Join(seq, "|"); got != want {
		t.Fatalf("sequence %s", got)
	}
	if st, err := os.Stat(refresh.Dir(cfg)); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("refresh dir: %v %v; the producer must be able to read the bundle", st, err)
	}

	// The helper exits zero with the workdir somewhere else: refused.
	deps.Apply = func(context.Context, string, string) (string, error) { return "def456", nil }
	if _, err := refresh.Run(context.Background(), cfg, deps); err == nil || !strings.Contains(err.Error(), "not abc123") {
		t.Fatalf("a wrong HEAD after a clean apply was accepted: %v", err)
	}
	deps.Apply = func(context.Context, string, string) (string, error) { return "", errors.New("boom") }
	if _, err := refresh.Run(context.Background(), cfg, deps); err == nil {
		t.Fatal("an apply failure was swallowed")
	}
	deps.Bundle = func(context.Context, string, string) (string, error) { return "", nil }
	if _, err := refresh.Run(context.Background(), cfg, deps); err == nil {
		t.Fatal("a bundle naming no commit was accepted")
	}
}

// Refresh runs only at the start of a cycle. Absence is unknown, and
// unknown authorizes nothing (#41 review).
func TestDecideRunsOnlyAtTheStartOfACycle(t *testing.T) {
	at := func(phase string) *state.State {
		st := state.New("f")
		st.Cycle = &state.Cycle{Phase: phase, ChangeID: "48", Family: "feat"}
		return st
	}
	for phase, want := range map[string]bool{
		state.CycleNew: true, state.CycleFinished: true, state.CycleClean: true,
		state.CycleWorking: false, state.CycleSubmitting: false, state.CycleOpen: false, state.CycleUnknown: false,
	} {
		if run, why := refresh.Decide(at(phase)); run != want {
			t.Fatalf("phase %s: run=%v (%s), want %v", phase, run, why, want)
		}
	}
	nocycle := state.New("f")
	nocycle.Cycle = nil
	if run, why := refresh.Decide(nocycle); run || !strings.Contains(why, "unknown") {
		t.Fatalf("a nil cycle authorized a refresh: %v %q", run, why)
	}
	if run, _ := refresh.Decide(state.New("f")); !run {
		t.Fatal("a fresh document is a new cycle")
	}
}

// A state document written before the cycle record loads as unknown, not
// as new: the upgrade case (#41 review).
func TestLegacyStateLoadsAsUnknownCycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{"schema_version":1,"factory":"f","roles":{"producer":{"last_turn":{"id":"producer-1","exit_code":0}},"reviewer":{}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := state.Load(path, "f")
	if err != nil {
		t.Fatal(err)
	}
	if st.Cycle == nil || st.Cycle.Phase != state.CycleUnknown {
		t.Fatalf("legacy cycle = %+v, want unknown", st.Cycle)
	}
	if run, why := refresh.Decide(st); run || !strings.Contains(why, "--force") {
		t.Fatalf("legacy state authorized a refresh: %v %q", run, why)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x", "GIT_CONFIG_NOSYSTEM=1", "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// The real thing, in the test's own identity: the #35 shape. The producer's
// clone is one merge behind with cycle 1's file untracked, a tracked file
// modified, and a stale intent file. After the refresh the tree is exactly
// the target commit and nothing else.
func TestApplyLocalMakesTheTreeExactlyTheTarget(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	upstream := t.TempDir()
	git(t, upstream, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(upstream, "README"), []byte("v1\n"), 0o644)
	git(t, upstream, "add", ".")
	git(t, upstream, "commit", "-qm", "v1")
	work := t.TempDir()
	git(t, work, "clone", "-q", upstream, ".")
	// Upstream moves on: cycle 1's script is merged.
	os.WriteFile(filepath.Join(upstream, "deploy.sh"), []byte("merged\n"), 0o755)
	os.WriteFile(filepath.Join(upstream, "README"), []byte("v2\n"), 0o644)
	git(t, upstream, "add", ".")
	git(t, upstream, "commit", "-qm", "v2")
	want := git(t, upstream, "rev-parse", "HEAD")
	// The producer's clone: old script untracked, README edited, stale intent.
	os.WriteFile(filepath.Join(work, "deploy.sh"), []byte("older, untracked\n"), 0o755)
	os.WriteFile(filepath.Join(work, "README"), []byte("edited\n"), 0o644)
	os.WriteFile(filepath.Join(work, ".producer-branch"), []byte("feat-stale\n"), 0o644)
	os.WriteFile(filepath.Join(work, "ignored.log"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(work, ".gitignore"), []byte("*.log\n"), 0o644)

	// factoryd's side: fetch into its own clone and bundle.
	submitRepo := t.TempDir()
	git(t, submitRepo, "clone", "-q", upstream, ".")
	git(t, submitRepo, "fetch", "-q", "origin", "+refs/heads/main:refs/remotes/factoryd/main")
	bundle := filepath.Join(t.TempDir(), "main.bundle")
	git(t, submitRepo, "bundle", "create", bundle, "refs/remotes/factoryd/main")

	got, err := refresh.ApplyLocal(context.Background(), "git", work, bundle, "main")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("HEAD %s, want %s", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "deploy.sh")); string(b) != "merged\n" {
		t.Fatalf("deploy.sh = %q: the untracked older copy survived", b)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "README")); string(b) != "v2\n" {
		t.Fatalf("README = %q", b)
	}
	for _, f := range []string{".producer-branch", "ignored.log", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(work, f)); !os.IsNotExist(err) {
			t.Fatalf("%s survived the refresh", f)
		}
	}
	if status := git(t, work, "status", "--porcelain"); status != "" {
		t.Fatalf("workdir not clean after refresh:\n%s", status)
	}
	if br := git(t, work, "rev-parse", "--abbrev-ref", "HEAD"); br != "main" {
		t.Fatalf("branch %s", br)
	}

	// No repository at all: one is made.
	empty := t.TempDir()
	if got, err := refresh.ApplyLocal(context.Background(), "git", empty, bundle, "main"); err != nil || got != want {
		t.Fatalf("fresh dir: %s %v", got, err)
	}
	if _, err := refresh.ApplyLocal(context.Background(), "git", empty, "relative.bundle", "main"); err == nil {
		t.Fatal("a relative bundle path was accepted")
	}
}

// No producer-controlled git configuration runs during a refresh (#41
// review). The producer's old .git/config and its HOME/.gitconfig each
// define a smudge filter that would leave a marker; the refreshed tree
// carries a .gitattributes naming that filter. After the refresh the tree
// is at the target and no marker exists.
func TestRefreshRunsNoProducerSuppliedGitConfiguration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	home := t.TempDir()
	marker := filepath.Join(t.TempDir(), "filter-ran")
	filterCmd := "touch " + marker + " && cat"
	upstream := t.TempDir()
	git(t, upstream, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(upstream, ".gitattributes"), []byte("* filter=evil\n"), 0o644)
	os.WriteFile(filepath.Join(upstream, "README"), []byte("v2\n"), 0o644)
	git(t, upstream, "add", ".")
	git(t, upstream, "commit", "-qm", "v2")
	want := git(t, upstream, "rev-parse", "HEAD")

	work := t.TempDir()
	git(t, work, "clone", "-q", upstream, ".")
	git(t, work, "config", "filter.evil.smudge", filterCmd) // in the producer's .git/config
	git(t, work, "config", "filter.evil.clean", "cat")
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[filter \"evil\"]\n\tsmudge = "+filterCmd+"\n\tclean = cat\n"), 0o644)
	// Control: with the producer's configuration honoured, the filter runs.
	os.Remove(filepath.Join(work, "README"))
	ctl := exec.Command("git", "checkout", "-q", "--", "README")
	ctl.Dir = work
	ctl.Env = append(os.Environ(), "HOME="+home)
	if out, err := ctl.CombinedOutput(); err != nil {
		t.Fatalf("control checkout: %v %s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: the filter did not run with the producer's config honoured; the test cannot fail: %v", err)
	}
	os.Remove(marker)

	submitRepo := t.TempDir()
	git(t, submitRepo, "clone", "-q", upstream, ".")
	git(t, submitRepo, "fetch", "-q", "origin", "+refs/heads/main:refs/remotes/factoryd/main")
	bundle := filepath.Join(t.TempDir(), "main.bundle")
	git(t, submitRepo, "bundle", "create", bundle, "refs/remotes/factoryd/main")

	t.Setenv("HOME", home)
	got, err := refresh.ApplyLocal(context.Background(), "git", work, bundle, "main")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("HEAD %s, want %s", got, want)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a producer-supplied smudge filter ran during the refresh")
	}
	if b, _ := os.ReadFile(filepath.Join(work, ".git", "config")); strings.Contains(string(b), "evil") {
		t.Fatal("the producer's old .git/config survived the refresh")
	}
	for _, kv := range refresh.GitEnv() {
		if strings.HasPrefix(kv, "GIT_CONFIG_GLOBAL=") && kv != "GIT_CONFIG_GLOBAL=/dev/null" {
			t.Fatalf("global config not disabled: %s", kv)
		}
	}
}

// ---------- end to end: a real supervisor, a real git workdir ----------

type scriptedRunner struct {
	mu  sync.Mutex
	n   int
	act func(n int, t supervise.Turn) supervise.TurnResult
}

func (r *scriptedRunner) Run(_ context.Context, t supervise.Turn, _ func(proc.Ref)) (supervise.TurnResult, error) {
	r.mu.Lock()
	r.n++
	n := r.n
	r.mu.Unlock()
	return r.act(n, t), nil
}

func (r *scriptedRunner) count() int { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

type e2e struct {
	lookup     func(context.Context, scm.ChangeID) (scm.Change, error)
	ancestor   func(context.Context, string, string) (bool, error)
	cfg        *config.Config
	upstream   string
	submitRepo string
	work       string
	refreshes  int
	target     string
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	e := &e2e{upstream: t.TempDir(), submitRepo: filepath.Join(root, "submit"), work: filepath.Join(root, "work")}
	git(t, e.upstream, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(e.upstream, "README"), []byte("v1\n"), 0o644)
	git(t, e.upstream, "add", ".")
	git(t, e.upstream, "commit", "-qm", "v1")
	os.MkdirAll(e.submitRepo, 0o755)
	git(t, e.submitRepo, "clone", "-q", e.upstream, ".")
	os.MkdirAll(e.work, 0o755)
	git(t, e.work, "clone", "-q", e.upstream, ".")
	e.target = git(t, e.upstream, "rev-parse", "HEAD")
	for _, d := range []string{"inbox", "outbox"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	e.cfg = &config.Config{
		SchemaVersion: config.SchemaVersion, Scope: config.EmptyScope(), Health: config.DefaultHealth(),
		Name: "widgets", Provider: "github", GitHub: &config.GitHub{Owner: "acme", Repo: "widgets"},
		TargetBranch: "main",
		Git:          config.Git{Remote: "https://github.com/acme/widgets.git", Transport: "https"},
		Paths:        config.Paths{Root: root, ProducerWorkdir: e.work, SubmitRepo: e.submitRepo},
		Credentials:  config.Credentials{Producer: config.CredentialRef{Env: "P"}, Reviewer: config.CredentialRef{Env: "R"}},
		Gate:         config.Gate{Command: []string{"true"}, Env: map[string]string{"PATH": "/usr/bin:/bin"}, RunAs: &config.RunAs{User: "factoryd-gate"}},
		Roles: config.Roles{
			Producer: config.RoleSpec{Command: []string{"true"}, Env: map[string]string{"PATH": os.Getenv("PATH")}, RunAs: &config.RunAs{User: u.Username}},
			Reviewer: config.RoleSpec{Command: []string{"true"}, Env: map[string]string{"PATH": os.Getenv("PATH")}},
		},
		Supervisor: config.Supervisor{SpinWarn: 2, SpinAbort: 4, FailAbort: 3, VerdictAttempts: 6, PollIntervalSeconds: 1, BackoffSeconds: 1, ForcePoll: true},
		Alerts:     []config.Alert{{Kind: "file", Path: filepath.Join(root, "alerts.log")}},
	}
	if err := e.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return e
}

// deps: a real local fetch into the submit repo, a real bundle, and the
// real helper in the test's own identity.
func (e *e2e) deps(t *testing.T) func(context.Context) (refresh.Deps, error) {
	return func(context.Context) (refresh.Deps, error) {
		return refresh.Deps{
			Fetch: func(_ context.Context, spec string) error {
				git(t, e.submitRepo, "fetch", "-q", e.upstream, spec)
				return nil
			},
			Bundle: func(_ context.Context, path, ref string) (string, error) {
				git(t, e.submitRepo, "bundle", "create", path, ref)
				return git(t, e.submitRepo, "rev-parse", ref), nil
			},
			Apply: func(ctx context.Context, bundle, branch string) (string, error) {
				e.refreshes++
				return refresh.ApplyLocal(ctx, "git", e.work, bundle, branch)
			},
			Lookup:   e.lookup,
			Ancestor: e.ancestor,
		}, nil
	}
}

func (e *e2e) wake(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.cfg.Paths.Root, "inbox", "brief.md"), []byte("do the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *e2e) progress(t *testing.T) {
	t.Helper()
	p := e.cfg.ProgressPath("producer")
	os.WriteFile(p, []byte("x"), 0o644)
	now := time.Now().Add(time.Second)
	os.Chtimes(p, now, now)
}

func (e *e2e) run(t *testing.T, r supervise.Runner, maxTurns int) error {
	t.Helper()
	return e.runWith(t, r, maxTurns, nil)
}

func (e *e2e) runWith(t *testing.T, r supervise.Runner, maxTurns int, after func(context.Context, supervise.Turn, supervise.TurnResult) (string, error)) error {
	t.Helper()
	s, err := supervise.New(supervise.Options{
		Config: e.cfg, Role: "producer", Runner: r,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sleep:      func(ctx context.Context, d time.Duration) error { return ctx.Err() },
		MaxTurns:   maxTurns,
		BeforeTurn: refresh.BeforeTurn(e.cfg, e.deps(t)),
		AfterTurn:  after,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = s.Run(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

func (e *e2e) cycle(t *testing.T) *state.Cycle {
	t.Helper()
	st, err := state.Load(e.cfg.StatePath(), e.cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	return st.Cycle
}

func (e *e2e) setCycle(t *testing.T, fn func(st *state.State)) {
	t.Helper()
	if _, err := state.Update(e.cfg.StatePath(), e.cfg.Name, func(st *state.State) error { fn(st); return nil }); err != nil {
		t.Fatal(err)
	}
}

func (e *e2e) edit(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.work, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *e2e) has(name string) bool {
	_, err := os.Stat(filepath.Join(e.work, name))
	return err == nil
}

// A first turn edits, reports progress, and fails before declaring a
// draft. Its retry keeps the edits: the cycle is working, not new.
func TestPartialProducerWorkSurvivesTheRetry(t *testing.T) {
	e := newE2E(t)
	e.wake(t)
	r := &scriptedRunner{act: func(n int, _ supervise.Turn) supervise.TurnResult {
		switch n {
		case 1:
			e.edit(t, "partial.go", "half done")
			e.progress(t)
			return supervise.TurnResult{ExitCode: 1} // fails without declaring
		default:
			if !e.has("partial.go") {
				t.Errorf("turn %d: the retry lost the first turn's edit", n)
			}
			e.progress(t)
			return supervise.TurnResult{}
		}
	}}
	if err := e.run(t, r, 2); err != nil {
		t.Fatal(err)
	}
	if r.count() != 2 {
		t.Fatalf("ran %d turns, want 2", r.count())
	}
	if e.refreshes != 1 {
		t.Fatalf("refreshed %d times, want exactly 1: once at the start of the cycle, never on the retry", e.refreshes)
	}
	if c := e.cycle(t); c == nil || c.Phase != state.CycleWorking || c.Base != e.target {
		t.Fatalf("cycle %+v, want working at %s", c, e.target)
	}
}

// A registry-aware state document that predates the cycle record. Nothing is
// refreshed, the turn runs on the tree as it is, and the record says why. A
// schema-v1 document is a separate verdict-registry migration block and must
// not reach a producer turn at all.
func TestStateWithoutCycleIsNeverRefreshed(t *testing.T) {
	e := newE2E(t)
	legacy := `{"schema_version":2,"factory":"widgets","verdict_registry":{"status":"ready"},"roles":{"producer":{"last_turn":{"id":"producer-1","exit_code":0}},"reviewer":{}}}`
	if err := os.WriteFile(e.cfg.StatePath(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	e.edit(t, "in-flight.go", "a draft the old binary opened")
	e.wake(t)
	r := &scriptedRunner{act: func(int, supervise.Turn) supervise.TurnResult { e.progress(t); return supervise.TurnResult{} }}
	if err := e.run(t, r, 1); err != nil {
		t.Fatal(err)
	}
	if e.refreshes != 0 || !e.has("in-flight.go") {
		t.Fatalf("legacy state was refreshed (%d) or the tree lost work (%v)", e.refreshes, e.has("in-flight.go"))
	}
	if c := e.cycle(t); c == nil || c.Phase != state.CycleUnknown {
		t.Fatalf("cycle %+v, want unknown to persist", c)
	}
}

// A crash between the draft's creation and the record of it: the cycle
// says submitting (written ahead of the push). No refresh.
func TestCrashBetweenDraftAndRecordIsNotRefreshed(t *testing.T) {
	e := newE2E(t)
	e.setCycle(t, func(st *state.State) {
		c := st.SetCycle(state.CycleSubmitting, time.Now())
		c.Family, c.Digest = "feat/x", "feat/x-abc123"
	})
	e.edit(t, "submitted.go", "pushed, draft open, record lost")
	e.wake(t)
	r := &scriptedRunner{act: func(int, supervise.Turn) supervise.TurnResult { e.progress(t); return supervise.TurnResult{} }}
	if err := e.run(t, r, 1); err != nil {
		t.Fatal(err)
	}
	if e.refreshes != 0 || !e.has("submitted.go") {
		t.Fatalf("a submitting cycle was refreshed (%d) or lost its tree (%v)", e.refreshes, e.has("submitted.go"))
	}
}

// A later verdict on an unrelated change does not finish the cycle: the
// open draft's tree stays. The merge of the cycle's own change does.
func TestUnrelatedVerdictDoesNotFinishTheCycle(t *testing.T) {
	e := newE2E(t)
	e.setCycle(t, func(st *state.State) {
		c := st.SetCycle(state.CycleOpen, time.Now())
		c.Family, c.Digest, c.ChangeID = "feat/x", "feat/x-abc123", "48"
		st.LastVerdict = &state.Verdict{ChangeID: "47", Kind: state.VerdictMerged}
	})
	e.edit(t, "open.go", "draft 48 is open")
	e.wake(t)
	r := &scriptedRunner{act: func(int, supervise.Turn) supervise.TurnResult { e.progress(t); return supervise.TurnResult{} }}
	if err := e.run(t, r, 1); err != nil {
		t.Fatal(err)
	}
	if e.refreshes != 0 || !e.has("open.go") {
		t.Fatalf("an open cycle was refreshed (%d) on an unrelated merge, or lost its tree (%v)", e.refreshes, e.has("open.go"))
	}

	// Now upstream moves and the cycle's own draft merges: the next turn
	// starts a new cycle at the new target.
	os.WriteFile(filepath.Join(e.upstream, "README"), []byte("v2\n"), 0o644)
	git(t, e.upstream, "add", ".")
	git(t, e.upstream, "commit", "-qm", "v2 (48 merged)")
	want := git(t, e.upstream, "rev-parse", "HEAD")
	e.setCycle(t, func(st *state.State) { st.SetCycle(state.CycleFinished, time.Now()) })
	e.wake(t)
	if err := e.run(t, r, 1); err != nil {
		t.Fatal(err)
	}
	if e.refreshes != 1 || e.has("open.go") {
		t.Fatalf("a finished cycle was not refreshed (%d) or its tree survived (%v)", e.refreshes, e.has("open.go"))
	}
	if b, _ := os.ReadFile(filepath.Join(e.work, "README")); string(b) != "v2\n" {
		t.Fatalf("README = %q after the refresh", b)
	}
	if c := e.cycle(t); c == nil || c.Phase != state.CycleWorking || c.Base != want || c.ChangeID != "" {
		t.Fatalf("new cycle %+v, want working at %s with no change", c, want)
	}
}

// The ordinary no-op turn: a brief the producer consumes without
// declaring anything. The cycle ends clean, the target advances, and the
// next brief refreshes (#41 review). The after-turn step is the shipped
// submit.AfterTurn.
func TestNoOpBriefThenTargetAdvancesThenTheNextBriefRefreshes(t *testing.T) {
	e := newE2E(t)
	e.wake(t)
	r := &scriptedRunner{act: func(n int, _ supervise.Turn) supervise.TurnResult {
		e.progress(t)
		return supervise.TurnResult{} // consumes the brief, declares nothing
	}}
	after := submit.AfterTurn(e.cfg, func(context.Context) (submit.Deps, error) {
		t.Error("submit deps built for a no-intent turn")
		return submit.Deps{}, nil
	})
	if err := e.runWith(t, r, 1, after); err != nil {
		t.Fatal(err)
	}
	if c := e.cycle(t); c == nil || c.Phase != state.CycleClean {
		t.Fatalf("after a no-op brief the cycle is %+v, want clean", c)
	}
	if e.refreshes != 1 {
		t.Fatalf("refreshes=%d after the first brief, want 1", e.refreshes)
	}

	os.WriteFile(filepath.Join(e.upstream, "README"), []byte("v2\n"), 0o644)
	git(t, e.upstream, "add", ".")
	git(t, e.upstream, "commit", "-qm", "v2")
	want := git(t, e.upstream, "rev-parse", "HEAD")
	e.wake(t)
	if err := e.runWith(t, r, 1, after); err != nil {
		t.Fatal(err)
	}
	if e.refreshes != 2 {
		t.Fatalf("refreshes=%d after the second brief, want 2: a clean cycle did not start a new one", e.refreshes)
	}
	if b, _ := os.ReadFile(filepath.Join(e.work, "README")); string(b) != "v2\n" {
		t.Fatalf("README = %q: the workdir is stale behind the target", b)
	}
	if c := e.cycle(t); c == nil || c.Base != want {
		t.Fatalf("cycle %+v, want base %s", c, want)
	}
}

// ---------- the operator's refresh against a starting turn ----------

// deps whose Fetch hands control to the test while the caller holds the
// state lock: the point at which the other party is made to start.
func (e *e2e) depsWithHook(t *testing.T, onFetch func()) refresh.Deps {
	d, _ := e.deps(t)(context.Background())
	inner := d.Fetch
	d.Fetch = func(ctx context.Context, spec string) error {
		if onFetch != nil {
			onFetch()
		}
		return inner(ctx, spec)
	}
	return d
}

// A producer turn starts after the operator's non-forced refresh has
// decided but before it has applied. The decision was taken under the
// lock the turn start needs, so the turn cannot run -- and edit -- until
// the operator has applied and recorded. Sensitivity: the turn edits a
// file and fails before declaring; had the turn slipped in between the
// operator's decision and apply, the apply would have wiped that edit. The
// hook waits for the edit up to a bound: under the lock it never comes
// during the hook (the turn is blocked), and the edit is made afterwards
// and survives; without the lock it comes during the hook and is wiped.
func TestOperatorRefreshHoldsTheTurnStartUntilItHasApplied(t *testing.T) {
	e := newE2E(t)
	e.setCycle(t, func(st *state.State) { st.SetCycle(state.CycleClean, time.Now()) })
	e.wake(t)
	turnDone := make(chan error, 1)
	edited := make(chan struct{}, 1)
	r := &scriptedRunner{act: func(int, supervise.Turn) supervise.TurnResult {
		e.edit(t, "in-progress.go", "the turn's work")
		e.progress(t)
		edited <- struct{}{}
		return supervise.TurnResult{ExitCode: 1} // fails before declaring: cycle stays working
	}}
	hookRan := false
	editedDuringHook := false
	ops := e.depsWithHook(t, func() {
		hookRan = true
		// The operator has decided and holds the lock. The producer
		// supervisor starts a turn now.
		go func() { turnDone <- e.run(t, r, 1) }()
		select {
		case <-edited:
			editedDuringHook = true // the turn ran between decision and apply
		case <-time.After(2 * time.Second):
		}
	})
	if _, _, err := refresh.Guarded(context.Background(), e.cfg, ops, false); err != nil {
		t.Fatalf("operator refresh: %v", err)
	}
	if !hookRan {
		t.Fatal("the hook did not run; the test proved nothing")
	}
	if err := <-turnDone; err != nil {
		t.Fatal(err)
	}
	if editedDuringHook {
		t.Fatal("the turn ran and edited between the operator's decision and its apply")
	}
	if !e.has("in-progress.go") {
		t.Fatal("the operator's apply wiped the turn's edit")
	}
	if c := e.cycle(t); c == nil || c.Phase != state.CycleWorking {
		t.Fatalf("cycle %+v, want working after the failed turn", c)
	}
}

// The other order: the producer's turn start holds the lock (deciding,
// refreshing, marking working) when the operator's non-forced refresh
// arrives. The operator waits, re-reads the cycle under the lock, sees
// working, and refuses. The turn's edit survives.
func TestOperatorRefreshRefusesWhenATurnStartedFirst(t *testing.T) {
	e := newE2E(t)
	e.setCycle(t, func(st *state.State) { st.SetCycle(state.CycleClean, time.Now()) })
	e.wake(t)
	opDone := make(chan error, 1)
	opStarted := false
	mkDeps := func(context.Context) (refresh.Deps, error) {
		return e.depsWithHook(t, func() {
			// The turn start has decided and holds the lock. The operator
			// runs an ordinary refresh now.
			opStarted = true
			go func() {
				_, _, err := refresh.Guarded(context.Background(), e.cfg, e.depsWithHook(t, nil), false)
				opDone <- err
			}()
			select {
			case err := <-opDone:
				t.Errorf("the operator's refresh completed (%v) while the turn start held the lock", err)
				opDone <- err
			case <-time.After(300 * time.Millisecond):
			}
		}), nil
	}
	r := &scriptedRunner{act: func(int, supervise.Turn) supervise.TurnResult {
		e.edit(t, "in-progress.go", "the turn's work")
		e.progress(t)
		return supervise.TurnResult{ExitCode: 1} // fails before declaring: cycle stays working
	}}
	s, err := supervise.New(supervise.Options{
		Config: e.cfg, Role: "producer", Runner: r,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sleep:      func(ctx context.Context, d time.Duration) error { return ctx.Err() },
		MaxTurns:   1,
		BeforeTurn: refresh.BeforeTurn(e.cfg, mkDeps),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Run(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	s.Close()
	if !opStarted {
		t.Fatal("the hook did not run; the test proved nothing")
	}
	err = <-opDone
	if !errors.Is(err, refresh.ErrRefused) || !strings.Contains(err.Error(), "working") {
		t.Fatalf("operator refresh returned %v, want a refusal naming the working cycle", err)
	}
	if !e.has("in-progress.go") {
		t.Fatal("the operator's refresh wiped the turn's work")
	}
	if e.refreshes != 1 {
		t.Fatalf("refreshes=%d, want 1 (the turn start's)", e.refreshes)
	}
}

// ---------- #43: a change merged outside factoryd ----------

type lookupFake struct {
	state    scm.ChangeState
	target   string // the change's target at the provider; "" means main
	err      error
	onTarget bool // IsAncestor(head, main)
	ancErr   error
	calls    int
	ancCalls int
}

func (f *lookupFake) get(context.Context, scm.ChangeID) (scm.Change, error) {
	f.calls++
	if f.err != nil {
		return scm.Change{}, f.err
	}
	target := f.target
	if target == "" {
		target = "main"
	}
	return scm.Change{ID: "55", State: f.state, TargetBranch: target, HeadSHA: "h55"}, nil
}

func (f *lookupFake) ancestor(_ context.Context, sha, ref string) (bool, error) {
	f.ancCalls++
	if f.ancErr != nil {
		return false, f.ancErr
	}
	return f.onTarget && ref == "main" && sha == "h55", nil
}

// Reconcile asks the provider about an open cycle's own change and nothing
// else. Merged finishes the cycle only when the change's target is the
// configured target AND its head is on it at the provider (#49 review):
// a draft retargeted and merged into release, or "merged" with a head
// not on main, leaves the cycle open with a note. Closed without a merge
// finishes it; a failed read leaves it open; other phases are not looked
// up.
func TestReconcileFinishesAnOpenCycleOnlyForAMergeIntoTheConfiguredTarget(t *testing.T) {
	cfg := &config.Config{TargetBranch: "main"}
	open := func() *state.State {
		st := state.New("f")
		c := st.SetCycle(state.CycleOpen, time.Now())
		c.ChangeID = "55"
		st.LastVerdict = &state.Verdict{ChangeID: "55", Kind: state.VerdictOperatorGated}
		return st
	}
	cases := []struct {
		name  string
		f     *lookupFake
		want  string
		found bool
		note  string
	}{
		{"merged into main, head on main", &lookupFake{state: scm.StateMerged, onTarget: true}, state.CycleFinished, true, "merged into main"},
		{"retargeted: merged into release", &lookupFake{state: scm.StateMerged, target: "release", onTarget: true}, state.CycleOpen, false, `merged into "release", not the configured target "main"`},
		{"merged says main but head not on main", &lookupFake{state: scm.StateMerged, onTarget: false}, state.CycleOpen, false, "is not on main"},
		{"merged but ancestry unverifiable", &lookupFake{state: scm.StateMerged, ancErr: errors.New("503")}, state.CycleOpen, false, "could not verify"},
		{"closed without merge", &lookupFake{state: scm.StateClosed}, state.CycleFinished, true, "closed without a merge"},
		{"still open", &lookupFake{state: scm.StateOpen}, state.CycleOpen, false, ""},
		{"read failed", &lookupFake{err: errors.New("503")}, state.CycleOpen, false, "left open"},
	}
	for _, c := range cases {
		st := open()
		changed, note := refresh.Reconcile(context.Background(), cfg, st, c.f.get, c.f.ancestor, time.Now())
		if changed != c.found || st.Cycle.Phase != c.want || c.f.calls != 1 || !strings.Contains(note, c.note) {
			t.Fatalf("%s: changed=%v phase=%s calls=%d note=%q", c.name, changed, st.Cycle.Phase, c.f.calls, note)
		}
	}
	// No ancestor check available: merged is not believed.
	st := open()
	f := &lookupFake{state: scm.StateMerged, onTarget: true}
	if changed, note := refresh.Reconcile(context.Background(), cfg, st, f.get, nil, time.Now()); changed || !strings.Contains(note, "cannot be verified") {
		t.Fatalf("merged believed without an ancestry check: %v %q", changed, note)
	}
	if changed, _ := refresh.Reconcile(context.Background(), cfg, open(), nil, nil, time.Now()); changed {
		t.Fatal("no lookup, yet the cycle changed")
	}
	for _, phase := range []string{state.CycleWorking, state.CycleSubmitting, state.CycleFinished, state.CycleClean, state.CycleUnknown} {
		st := state.New("f")
		st.SetCycle(phase, time.Now()).ChangeID = "55"
		f := &lookupFake{state: scm.StateMerged, onTarget: true}
		if changed, _ := refresh.Reconcile(context.Background(), cfg, st, f.get, f.ancestor, time.Now()); changed || f.calls != 0 {
			t.Fatalf("phase %s: looked up (%d) or changed (%v)", phase, f.calls, changed)
		}
	}
}

// A queued brief is a reason to reconcile an operator-merged draft, but not
// a reason to launch a failed producer turn while it remains open. Once the
// provider proves the merge landed, the queue refreshes and atomically marks
// its next work item working before the selected file is taken.
func TestQueueStartReconcilesAndReservesAnOperatorMergedCycle(t *testing.T) {
	cfg := cfgFor(t)
	if _, err := state.Update(cfg.StatePath(), cfg.Name, func(st *state.State) error {
		c := st.SetCycle(state.CycleOpen, time.Now())
		c.ChangeID, c.Family, c.Digest = "55", "feat/x", "feat/x-abc"
		st.LastVerdict = &state.Verdict{ChangeID: "55", Kind: state.VerdictOperatorGated}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f := &lookupFake{state: scm.StateMerged, onTarget: true}
	started, note, err := refresh.QueueStart(cfg, func(context.Context) (refresh.Deps, error) {
		return refresh.Deps{
			Fetch:    func(context.Context, string) error { return nil },
			Bundle:   func(context.Context, string, string) (string, error) { return "abc123", nil },
			Apply:    func(context.Context, string, string) (string, error) { return "abc123", nil },
			Lookup:   f.get,
			Ancestor: f.ancestor,
		}, nil
	})(context.Background(), supervise.Turn{ID: "queue-1", Role: "producer", Triggers: []watch.Trigger{{
		Label: brief.Label, Path: filepath.Join(cfg.BriefsDir(), "010-next.md"),
	}}})
	if err != nil || !started || !strings.Contains(note, "cycle finished") || !strings.Contains(note, "workdir refreshed") {
		t.Fatalf("started=%v note=%q err=%v", started, note, err)
	}
	st, err := state.Load(cfg.StatePath(), cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	q := st.Role(state.RoleProducer).QueueReservation
	if st.Cycle == nil || st.Cycle.Phase != state.CycleWorking || st.Cycle.Base != "abc123" || q == nil || q.Source != filepath.Join(cfg.BriefsDir(), "010-next.md") || q.Taken || f.calls != 1 || f.ancCalls != 1 {
		t.Fatalf("cycle=%+v lookup=%d ancestor=%d", st.Cycle, f.calls, f.ancCalls)
	}
}

// A submit that won the producer-worktree barrier must finish copying and
// gating before a queued brief can refresh and launch an agent beside it.
func TestQueueStartDefersWhileSubmissionLeaseIsLive(t *testing.T) {
	cfg := cfgFor(t)
	holder, err := proc.Self("submit-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Update(cfg.StatePath(), cfg.Name, func(st *state.State) error {
		st.SetCycle(state.CycleNew, time.Now())
		return st.AcquireProducerWorktreeLease(holder, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	depsBuilt := 0
	started, note, err := refresh.QueueStart(cfg, func(context.Context) (refresh.Deps, error) {
		depsBuilt++
		return refresh.Deps{}, nil
	})(context.Background(), supervise.Turn{ID: "queue-lease", Role: "producer", Triggers: []watch.Trigger{{
		Label: brief.Label, Path: filepath.Join(cfg.BriefsDir(), "010-next.md"),
	}}})
	if err != nil || started || !strings.Contains(note, state.ErrProducerWorktreeBusy.Error()) {
		t.Fatalf("started=%v note=%q err=%v, want queue deferral for the live submission lease", started, note, err)
	}
	if depsBuilt != 0 {
		t.Fatalf("queue built refresh dependencies %d times while a submit held the worktree", depsBuilt)
	}
	st, err := state.Load(cfg.StatePath(), cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	rs := st.Role(state.RoleProducer)
	if rs.SubmissionLease == nil || rs.QueueReservation != nil {
		t.Fatalf("queue changed active submission lease state: %+v", rs)
	}
}

// BeforeTurn is also reached by legacy inbox briefs, answers, verdicts, and
// retries. It must reject the same live submit lease before it builds refresh
// dependencies or touches the producer worktree.
func TestBeforeTurnDefersWhileSubmissionLeaseIsLive(t *testing.T) {
	cfg := cfgFor(t)
	holder, err := proc.Self("submit-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Update(cfg.StatePath(), cfg.Name, func(st *state.State) error {
		st.SetCycle(state.CycleNew, time.Now())
		return st.AcquireProducerWorktreeLease(holder, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	depsBuilt := 0
	_, err = refresh.BeforeTurn(cfg, func(context.Context) (refresh.Deps, error) {
		depsBuilt++
		return refresh.Deps{}, nil
	})(context.Background(), supervise.Turn{ID: "legacy-brief", Role: "producer"})
	if !errors.Is(err, state.ErrProducerWorktreeBusy) {
		t.Fatalf("BeforeTurn error=%v, want live submission lease refusal", err)
	}
	if depsBuilt != 0 {
		t.Fatalf("BeforeTurn built refresh dependencies %d times beside a submission lease", depsBuilt)
	}
	st, err := state.Load(cfg.StatePath(), cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if st.Cycle == nil || st.Cycle.Phase != state.CycleNew {
		t.Fatalf("BeforeTurn changed cycle despite the live submission lease: %+v", st.Cycle)
	}
}

// The durable submission barrier carries an exact process reference rather
// than a timeout. Once that submit process has died, the next admission
// reclaims the stale lease inside its locked decision and does not strand the
// first queued brief.
func TestQueueStartReclaimsADeadSubmissionLease(t *testing.T) {
	cfg := cfgFor(t)
	if _, err := state.Update(cfg.StatePath(), cfg.Name, func(st *state.State) error {
		st.SetCycle(state.CycleNew, time.Now())
		st.Role(state.RoleProducer).SubmissionLease = &state.ProducerWorktreeLease{
			Holder:     proc.Ref{PID: os.Getpid(), StartToken: "not-this-process"},
			AcquiredAt: time.Now(),
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	started, note, err := refresh.QueueStart(cfg, func(context.Context) (refresh.Deps, error) {
		return refresh.Deps{
			Fetch:  func(context.Context, string) error { return nil },
			Bundle: func(context.Context, string, string) (string, error) { return "abc123", nil },
			Apply:  func(context.Context, string, string) (string, error) { return "abc123", nil },
		}, nil
	})(context.Background(), supervise.Turn{ID: "queue-after-crash", Role: "producer", Triggers: []watch.Trigger{{
		Label: brief.Label, Path: filepath.Join(cfg.BriefsDir(), "010-next.md"),
	}}})
	if err != nil || !started {
		t.Fatalf("started=%v note=%q err=%v, want dead submission lease reclaimed", started, note, err)
	}
	st, err := state.Load(cfg.StatePath(), cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	rs := st.Role(state.RoleProducer)
	if rs.SubmissionLease != nil || rs.QueueReservation == nil {
		t.Fatalf("dead submission lease did not become a queue reservation: %+v", rs)
	}
}

// End to end: the reviewer signalled operator-gated, a human merged !55,
// last_verdict still says operator-gated. The next brief's before-turn
// step reconciles under the lock, finishes the cycle, and refreshes to
// the moved target. Nothing polled: one read, at the decision.
func TestOperatorMergedChangeIsReconciledAtTheNextBrief(t *testing.T) {
	e := newE2E(t)
	e.setCycle(t, func(st *state.State) {
		c := st.SetCycle(state.CycleOpen, time.Now())
		c.ChangeID, c.Family, c.Digest = "55", "feat/x", "feat/x-abc"
		st.LastVerdict = &state.Verdict{ChangeID: "55", Kind: state.VerdictOperatorGated}
	})
	e.edit(t, "landed.go", "the change a human merged")
	os.WriteFile(filepath.Join(e.upstream, "landed.go"), []byte("the change a human merged"), 0o644)
	git(t, e.upstream, "add", ".")
	git(t, e.upstream, "commit", "-qm", "merge !55")
	want := git(t, e.upstream, "rev-parse", "HEAD")
	lk := &lookupFake{state: scm.StateMerged, onTarget: true}
	e.lookup, e.ancestor = lk.get, lk.ancestor
	e.wake(t)
	r := &scriptedRunner{act: func(int, supervise.Turn) supervise.TurnResult { e.progress(t); return supervise.TurnResult{} }}
	if err := e.run(t, r, 1); err != nil {
		t.Fatal(err)
	}
	if lk.calls != 1 {
		t.Fatalf("provider read %d times, want exactly 1", lk.calls)
	}
	if e.refreshes != 1 {
		t.Fatalf("refreshes=%d, want 1: the reconciled cycle did not start a new one", e.refreshes)
	}
	if c := e.cycle(t); c == nil || c.Phase != state.CycleWorking || c.Base != want {
		t.Fatalf("cycle %+v, want working at %s", c, want)
	}
	// And last_verdict, untouched by the refresh, still says operator-gated:
	// the cycle, not the verdict, governs the refresh.
	st, _ := state.Load(e.cfg.StatePath(), e.cfg.Name)
	if st.LastVerdict.Kind != state.VerdictOperatorGated {
		t.Fatalf("last_verdict = %s", st.LastVerdict.Kind)
	}
}

// End to end, the retargeted case (#49 review): !55 was opened for main,
// retargeted, and merged into release. The next brief's before-turn step
// must NOT finish the cycle or refresh -- the producer's tree, which
// never landed on main, is kept.
func TestRetargetedMergeDoesNotFinishTheCycleOrResetTheTree(t *testing.T) {
	e := newE2E(t)
	e.setCycle(t, func(st *state.State) {
		c := st.SetCycle(state.CycleOpen, time.Now())
		c.ChangeID, c.Family, c.Digest = "55", "feat/x", "feat/x-abc"
	})
	e.edit(t, "in-flight.go", "merged into release, not main")
	lk := &lookupFake{state: scm.StateMerged, target: "release", onTarget: true}
	e.lookup, e.ancestor = lk.get, lk.ancestor
	e.wake(t)
	r := &scriptedRunner{act: func(int, supervise.Turn) supervise.TurnResult { e.progress(t); return supervise.TurnResult{} }}
	if err := e.run(t, r, 1); err != nil {
		t.Fatal(err)
	}
	if e.refreshes != 0 || !e.has("in-flight.go") {
		t.Fatalf("a retargeted merge reset the tree: refreshes=%d kept=%v", e.refreshes, e.has("in-flight.go"))
	}
	if c := e.cycle(t); c == nil || c.Phase != state.CycleOpen {
		t.Fatalf("cycle %+v, want still open", c)
	}
}
