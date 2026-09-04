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

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/proc"
	"github.com/aicix-labs/factoryd/internal/refresh"
	"github.com/aicix-labs/factoryd/internal/state"
	"github.com/aicix-labs/factoryd/internal/supervise"
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
		state.CycleNew: true, state.CycleFinished: true,
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
		Supervisor: config.Supervisor{SpinWarn: 2, SpinAbort: 4, FailAbort: 3, PollIntervalSeconds: 1, BackoffSeconds: 1, ForcePoll: true},
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
	s, err := supervise.New(supervise.Options{
		Config: e.cfg, Role: "producer", Runner: r,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sleep:      func(ctx context.Context, d time.Duration) error { return ctx.Err() },
		MaxTurns:   maxTurns,
		BeforeTurn: refresh.BeforeTurn(e.cfg, e.deps(t)),
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

// The upgrade case: a state document with no cycle record. Nothing is
// refreshed, the turn runs on the tree as it is, and the record says why.
func TestLegacyStateIsNeverRefreshed(t *testing.T) {
	e := newE2E(t)
	legacy := `{"schema_version":1,"factory":"widgets","roles":{"producer":{"last_turn":{"id":"producer-1","exit_code":0}},"reviewer":{}}}`
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
