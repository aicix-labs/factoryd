package supervise_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/proc"
	"github.com/aicix-labs/factoryd/internal/state"
	"github.com/aicix-labs/factoryd/internal/supervise"
)

// fakeRunner is the agent turn under test. Each turn calls act, which decides
// what that turn does to the world: consume its trigger, touch progress, or
// nothing at all.
type fakeRunner struct {
	mu    sync.Mutex
	turns []supervise.Turn
	act   func(n int, t supervise.Turn) supervise.TurnResult
	// run, when set, replaces act and sees the turn's context -- for turns
	// that must block until the supervisor is stopped.
	run func(ctx context.Context, t supervise.Turn) supervise.TurnResult
}

func (f *fakeRunner) Run(ctx context.Context, t supervise.Turn, started func(proc.Ref)) (supervise.TurnResult, error) {
	f.mu.Lock()
	f.turns = append(f.turns, t)
	n := len(f.turns)
	f.mu.Unlock()
	if started != nil {
		if ref, err := proc.Self("turn"); err == nil {
			started(ref)
		}
	}
	if f.run != nil {
		return f.run(ctx, t), nil
	}
	if f.act == nil {
		return supervise.TurnResult{}, nil
	}
	return f.act(n, t), nil
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.turns)
}

type fixture struct {
	afterTurn func(ctx context.Context, t supervise.Turn, res supervise.TurnResult) (string, error)
	cfg       *config.Config
	root      string
	inbox     string
	outbox    string
	slept     []time.Duration
}

func currentUser(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return u.Username
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	fx := &fixture{
		root:   root,
		inbox:  filepath.Join(root, "inbox"),
		outbox: filepath.Join(root, "outbox"),
	}
	for _, d := range []string{fx.inbox, fx.outbox, filepath.Join(root, "work")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fx.cfg = &config.Config{
		SchemaVersion: config.SchemaVersion, Scope: config.EmptyScope(), Health: config.DefaultHealth(),
		Name:         "widgets",
		Provider:     "github",
		GitHub:       &config.GitHub{Owner: "acme", Repo: "widgets"},
		TargetBranch: "main",
		Git:          config.Git{Remote: "https://github.com/acme/widgets.git", Transport: "https"},
		Paths:        config.Paths{Root: root, ProducerWorkdir: filepath.Join(root, "work"), SubmitRepo: filepath.Join(root, "submit")},
		Credentials: config.Credentials{
			Producer: config.CredentialRef{Env: "P"},
			Reviewer: config.CredentialRef{Env: "R"},
		},
		Gate: config.Gate{Command: []string{"true"}, Env: map[string]string{"PATH": "/usr/bin:/bin"}, RunAs: &config.RunAs{User: "factoryd-gate"}},
		Roles: config.Roles{
			// The test's own user: switching to oneself needs no privilege, so
			// the run_as path is exercised without root. The "cannot switch"
			// case is its own test in runner_test.go.
			Producer: config.RoleSpec{Command: []string{"true"}, Env: map[string]string{"PATH": os.Getenv("PATH")}, RunAs: &config.RunAs{User: currentUser(t)}},
			Reviewer: config.RoleSpec{Command: []string{"true"}, Env: map[string]string{"PATH": os.Getenv("PATH")}},
		},
		Supervisor: config.Supervisor{
			SpinWarn: 2, SpinAbort: 4, FailAbort: 3, PollIntervalSeconds: 1, BackoffSeconds: 1,
			ForcePoll: true,
		},
		Alerts: []config.Alert{{Kind: "file", Path: filepath.Join(root, "alerts.log")}},
	}
	if err := fx.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return fx
}

func (fx *fixture) wake(t *testing.T) string {
	t.Helper()
	p := filepath.Join(fx.inbox, "wake")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func (fx *fixture) progress(t *testing.T) {
	t.Helper()
	if err := fx.progressQuiet(); err != nil {
		t.Fatal(err)
	}
}

// progressQuiet is what a runner goroutine calls. t.Fatal from a goroutine that
// is not the test's own is not allowed, and a supervisor still winding down
// after the test body returns would trip exactly that.
func (fx *fixture) progressQuiet() error { return fx.progressQuietFor("reviewer") }

func (fx *fixture) progressQuietFor(role string) error {
	p := fx.cfg.ProgressPath(role)
	// A fresh mtime, unambiguously later than anything already recorded.
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		return err
	}
	now := time.Now().Add(time.Second)
	return os.Chtimes(p, now, now)
}

func (fx *fixture) newSupervisor(t *testing.T, r supervise.Runner, maxTurns int) *supervise.Supervisor {
	t.Helper()
	s, err := supervise.New(supervise.Options{
		Config: fx.cfg,
		Role:   "reviewer",
		Runner: r,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sleep: func(ctx context.Context, d time.Duration) error {
			fx.slept = append(fx.slept, d)
			return ctx.Err()
		},
		MaxTurns:  maxTurns,
		AfterTurn: fx.afterTurn,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func (fx *fixture) roleState(t *testing.T) *state.RoleState {
	t.Helper()
	st, err := state.Load(fx.cfg.StatePath(), fx.cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	return st.Role(state.RoleReviewer)
}

func ctxWithTimeout(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 20*time.Second)
}

// ---------- the loop ----------

// One trigger, one turn. The turn consumes it, and the supervisor re-arms
// rather than stopping -- v1's producer stopped after every verdict and needed
// a human to re-prompt it.
func TestConsumedTriggerRunsOneTurnAndReArms(t *testing.T) {
	fx := newFixture(t)
	p := fx.wake(t)

	r := &fakeRunner{act: func(n int, _ supervise.Turn) supervise.TurnResult {
		if n == 1 {
			os.Remove(p)
		}
		return supervise.TurnResult{}
	}}
	s := fx.newSupervisor(t, r, 1)

	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.count(); got != 1 {
		t.Fatalf("ran %d turns for one trigger, want 1", got)
	}

	rs := fx.roleState(t)
	if rs.SpinCount != 0 {
		t.Fatalf("spin count = %d after a consumed trigger, want 0", rs.SpinCount)
	}
	if rs.LastTurn == nil || rs.LastTurn.Running() {
		t.Fatalf("last turn = %+v, want a finished turn", rs.LastTurn)
	}
	if len(rs.Pending) != 0 {
		t.Fatalf("pending = %+v after the trigger was consumed", rs.Pending)
	}
	if rs.Supervisor == nil {
		t.Fatal("the supervisor did not record itself")
	}
}

// The distinction §6.3 exists for: a turn that leaves its trigger but touches
// progress is working, not spinning. A guard that could not tell these apart
// would halt a legitimate multi-turn task.
func TestProgressResetsTheSpinCounterWithoutConsumption(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)

	r := &fakeRunner{act: func(_ int, _ supervise.Turn) supervise.TurnResult {
		fx.progress(t)
		return supervise.TurnResult{}
	}}
	s := fx.newSupervisor(t, r, 6)

	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatalf("a task that kept reporting progress was halted: %v", err)
	}
	if got := r.count(); got != 6 {
		t.Fatalf("ran %d turns, want 6", got)
	}
	if rs := fx.roleState(t); rs.SpinCount != 0 || rs.Halted {
		t.Fatalf("spin=%d halted=%v after six turns of real progress", rs.SpinCount, rs.Halted)
	}
	if len(fx.slept) != 0 {
		t.Fatalf("backed off %v while the agent was making progress", fx.slept)
	}
}

// Neither consumed nor progressed: the counter must climb and the supervisor
// must halt at spin_abort rather than relaunching a paid model turn forever.
func TestSpinGuardHalts(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)

	r := &fakeRunner{act: func(_ int, _ supervise.Turn) supervise.TurnResult {
		return supervise.TurnResult{} // exit 0: ran, consumed nothing, progressed nothing
	}}
	s := fx.newSupervisor(t, r, 50)

	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	err := s.Run(ctx)
	if !errors.Is(err, supervise.ErrHalted) {
		t.Fatalf("Run returned %v, want ErrHalted", err)
	}
	// spin_abort is 4, so the fourth fruitless turn halts.
	if got := r.count(); got != 4 {
		t.Fatalf("ran %d turns before halting, want 4 (spin_abort)", got)
	}

	rs := fx.roleState(t)
	if !rs.Halted {
		t.Fatal("state does not record the halt")
	}
	if rs.HaltReason == "" {
		t.Fatal("halted with no reason recorded")
	}
	if !strings.Contains(rs.HaltReason, "spin_abort") {
		t.Fatalf("halt reason does not name the guard that fired: %q", rs.HaltReason)
	}

	// The sentinel must exist and say why.
	body, err := os.ReadFile(fx.cfg.StopPath("reviewer"))
	if err != nil {
		t.Fatalf("no stop sentinel: %v", err)
	}
	if !strings.Contains(string(body), "reason:") {
		t.Fatalf("sentinel does not carry a reason:\n%s", body)
	}

	// The trigger is the only evidence a signal ever arrived. Halting must not
	// destroy it.
	if _, err := os.Stat(filepath.Join(fx.inbox, "wake")); err != nil {
		t.Fatalf("the trigger was removed on abort: %v", err)
	}
	if len(rs.Pending) == 0 {
		t.Fatal("state forgot the pending trigger on abort")
	}
}

// The realistic stall: a turn reports progress once, then achieves nothing.
//
// The counter must climb from the last progress and halt. The progress file is
// durable, so a guard that asked whether it EXISTS rather than whether it MOVED
// would see progress on every subsequent turn, reset the counter every time,
// and never fire again -- a producer that progressed once and then crash-loops
// would be relaunched forever. TestSpinGuardHalts cannot catch that, because
// its turns never touch progress at all.
func TestProgressOnceThenStallStillHalts(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)

	r := &fakeRunner{act: func(n int, _ supervise.Turn) supervise.TurnResult {
		if n == 1 {
			// One real step, then nothing. The file it touched stays on disk.
			_ = fx.progressQuiet()
			return supervise.TurnResult{}
		}
		return supervise.TurnResult{}
	}}
	s := fx.newSupervisor(t, r, 50)

	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	err := s.Run(ctx)
	if !errors.Is(err, supervise.ErrHalted) {
		t.Fatalf("Run returned %v, want ErrHalted: a turn that progressed once and then stalled was relaunched forever", err)
	}
	// Turn 1 progresses (spin 0); turns 2-5 achieve nothing (spin 1..4), and
	// spin_abort is 4.
	if got := r.count(); got != 5 {
		t.Fatalf("ran %d turns, want 5 (one that progressed, then spin_abort=4 that did not)", got)
	}

	rs := fx.roleState(t)
	if !rs.Halted {
		t.Fatal("state does not record the halt")
	}
	// The progress file is still there. Its presence must not have been what
	// the guard was reading.
	if _, err := os.Stat(fx.cfg.ProgressPath("reviewer")); err != nil {
		t.Fatalf("the progress file vanished, so this test proved nothing: %v", err)
	}
}

// A progress file left by an earlier run must not read as progress made by this
// one. Same rule, different entry path: here the file predates the first turn.
func TestStaleProgressFileIsNotProgress(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)
	fx.progress(t) // as if a previous run had advanced

	r := &fakeRunner{act: func(int, supervise.Turn) supervise.TurnResult {
		return supervise.TurnResult{}
	}}
	s := fx.newSupervisor(t, r, 50)

	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	if err := s.Run(ctx); !errors.Is(err, supervise.ErrHalted) {
		t.Fatalf("Run returned %v, want ErrHalted: a progress file from an earlier run counted as progress", err)
	}
	if got := r.count(); got != 4 {
		t.Fatalf("ran %d turns, want 4 (spin_abort); the stale file bought extra turns", got)
	}
}

// Two roles, one factory directory. One role's progress must not reset the
// other's spin counter.
//
// The handoff directory is factory-wide and the role suffix on the progress
// marker is the only thing separating the two. Collapse it and a busy producer
// continuously clears the reviewer's count, so a reviewer in a crash loop never
// halts -- the same silent disabling of the breaker, reached from a third
// direction. Both roles running against one factory is the normal
// configuration, not a corner.
func TestOneRolesProgressDoesNotResetAnothers(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)

	// A busy producer, advancing on every reviewer turn. The reviewer itself
	// achieves nothing.
	r := &fakeRunner{act: func(int, supervise.Turn) supervise.TurnResult {
		if err := fx.progressQuietFor("producer"); err != nil {
			t.Error(err)
		}
		return supervise.TurnResult{}
	}}
	s := fx.newSupervisor(t, r, 50)

	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	if err := s.Run(ctx); !errors.Is(err, supervise.ErrHalted) {
		t.Fatalf("Run returned %v, want ErrHalted: the producer's progress kept clearing the reviewer's spin counter", err)
	}
	if got := r.count(); got != 4 {
		t.Fatalf("ran %d turns, want 4 (spin_abort); another role's progress bought extra turns", got)
	}

	// Positive control: the producer really was advancing. Without this, the
	// halt above could just mean nothing ever touched anything.
	if _, err := os.Stat(fx.cfg.ProgressPath("producer")); err != nil {
		t.Fatalf("the producer never advanced, so this test proved nothing: %v", err)
	}
	if _, err := os.Stat(fx.cfg.ProgressPath("reviewer")); err == nil {
		t.Fatal("the reviewer's own progress marker exists; the two roles are not separated in this fixture")
	}
}

// Consecutive failures across real, distinct triggers: each turn consumes one
// and a fresh one is left for the next. The spin guard never engages, because
// consumption resets it every time; the fail streak is the counter that climbs.
//
// This is NOT the issue #12 stall -- that has one trigger and no replacement,
// and is TestStallAfterConsumedFailureIsRetriedThenHalted below.
func TestConsecutiveConsumedFailuresHalt(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)

	names := []string{"wake", "question.md"}
	r := &fakeRunner{act: func(n int, tr supervise.Turn) supervise.TurnResult {
		for _, trig := range tr.Triggers {
			os.Remove(trig.Path) // consume what we were woken for
		}
		next := filepath.Join(fx.inbox, names[n%2]) // leave a different one
		if err := os.WriteFile(next, []byte("x"), 0o644); err != nil {
			t.Error(err)
		}
		return supervise.TurnResult{ExitCode: 1}
	}}
	s := fx.newSupervisor(t, r, 50)

	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	err := s.Run(ctx)
	if !errors.Is(err, supervise.ErrHalted) {
		t.Fatalf("Run returned %v, want ErrHalted: turns that consumed their trigger and failed were never escalated", err)
	}
	if got := r.count(); got != 3 {
		t.Fatalf("ran %d turns, want 3 (fail_abort)", got)
	}

	rs := fx.roleState(t)
	// The distinguishing fact: the spin guard saw nothing.
	if rs.SpinCount != 0 {
		t.Fatalf("spin count = %d; this scenario must be invisible to the spin guard, or the test is not testing the gap", rs.SpinCount)
	}
	if !strings.Contains(rs.HaltReason, "fail_abort") {
		t.Fatalf("halt reason does not name the fail guard: %q", rs.HaltReason)
	}
	if _, err := os.Stat(fx.cfg.StopPath("reviewer")); err != nil {
		t.Fatalf("no stop sentinel: %v", err)
	}
}

// A failure that reported progress is a partial step, not a stall. Same rule as
// the spin guard: progress resets, consumption does not.
// The stall from issue #12, exactly: ONE trigger, consumed by a turn that then
// fails, and nothing else ever arrives. Before this fix the supervisor logged a
// warning and waited forever with fail_streak=1 -- fail_abort was unreachable
// without an unrelated trigger, which is precisely what the 3.5-hour idle
// looked like. Now the supervisor re-arms itself on a durable marker, the
// streak climbs, and it halts with a reason.
func TestStallAfterConsumedFailureIsRetriedThenHalted(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t) // the only trigger there will ever be

	var sawRetry int
	r := &fakeRunner{act: func(n int, tr supervise.Turn) supervise.TurnResult {
		for _, trig := range tr.Triggers {
			if trig.Label == supervise.RetryLabel {
				sawRetry++
				continue
			}
			os.Remove(trig.Path) // consume the real trigger; replace nothing
		}
		return supervise.TurnResult{ExitCode: 1}
	}}
	s := fx.newSupervisor(t, r, 50)

	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	err := s.Run(ctx)
	if !errors.Is(err, supervise.ErrHalted) {
		t.Fatalf("Run returned %v, want ErrHalted: with one trigger and no replacement the factory idled instead of escalating", err)
	}
	// Turn 1 on the real trigger, then fail_abort-1 retries.
	if got := r.count(); got != 3 {
		t.Fatalf("ran %d turns, want 3 (one real, two retries, fail_abort=3)", got)
	}
	if sawRetry != 2 {
		t.Fatalf("the agent was woken for %d retries, want 2: the supervisor did not re-arm itself", sawRetry)
	}
	// And the retries were backed off, not fired in a tight ring.
	if len(fx.slept) != 2 {
		t.Fatalf("backed off %v, want two delays before the halt", fx.slept)
	}

	rs := fx.roleState(t)
	if rs.SpinCount != 0 {
		t.Fatalf("spin count = %d; the spin guard must be blind to this scenario", rs.SpinCount)
	}
	if !strings.Contains(rs.HaltReason, "fail_abort") {
		t.Fatalf("halt reason does not name the fail guard: %q", rs.HaltReason)
	}
	// The marker is durable evidence of what happened; a halt does not erase it.
	body, err := os.ReadFile(fx.cfg.RetryPath("reviewer"))
	if err != nil {
		t.Fatalf("no retry marker after the halt: %v", err)
	}
	if !strings.Contains(string(body), "wake") {
		t.Fatalf("the retry marker does not name the original trigger:\n%s", body)
	}
}

// A retry that succeeds clears everything: streak reset, marker removed, and
// the supervisor goes back to a genuine idle.
func TestSuccessfulRetryClearsTheStreak(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)

	r := &fakeRunner{act: func(n int, tr supervise.Turn) supervise.TurnResult {
		for _, trig := range tr.Triggers {
			if trig.Label != supervise.RetryLabel {
				os.Remove(trig.Path)
			}
		}
		if n == 1 {
			return supervise.TurnResult{ExitCode: 1} // fails once
		}
		return supervise.TurnResult{} // the retry succeeds
	}}
	s := fx.newSupervisor(t, r, 2)

	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatalf("a failure followed by a successful retry halted: %v", err)
	}
	if got := r.count(); got != 2 {
		t.Fatalf("ran %d turns, want 2", got)
	}
	rs := fx.roleState(t)
	if rs.FailStreak != 0 || rs.Halted {
		t.Fatalf("fail_streak=%d halted=%v after a successful retry", rs.FailStreak, rs.Halted)
	}
	if _, err := os.Stat(fx.cfg.RetryPath("reviewer")); err == nil {
		t.Fatal("the retry marker survived a successful retry; the next start would retry again for nothing")
	}
	if len(rs.Pending) != 0 {
		t.Fatalf("pending = %+v after everything was handled", rs.Pending)
	}
}

// Stopping the supervisor mid-turn is not an agent failure. The kill produces a
// non-zero exit, and counting it would let an ordinary shutdown poison the
// streak -- so that a later, unrelated failure halts the factory and nobody
// connects the two.
func TestShutdownDuringATurnIsNotAFailure(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)

	started := make(chan struct{})
	r := &fakeRunner{act: func(int, supervise.Turn) supervise.TurnResult { panic("unused") }}
	r.run = func(ctx context.Context, tr supervise.Turn) supervise.TurnResult {
		close(started)
		<-ctx.Done() // the turn is running when the supervisor is stopped
		return supervise.TurnResult{ExitCode: 143}
	}
	s := fx.newSupervisor(t, r, 0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("the turn never started")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on shutdown, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not stop")
	}

	rs := fx.roleState(t)
	if rs.FailStreak != 0 {
		t.Fatalf("fail_streak = %d after a shutdown mid-turn; the streak was poisoned", rs.FailStreak)
	}
	if rs.Halted {
		t.Fatal("a shutdown halted the factory")
	}
	if _, err := os.Stat(fx.cfg.RetryPath("reviewer")); err == nil {
		t.Fatal("a shutdown re-armed a retry, as if the agent had failed")
	}
	if rs.LastTurn == nil || !rs.LastTurn.Interrupted {
		t.Fatalf("the interrupted turn is not recorded as interrupted: %+v", rs.LastTurn)
	}
	// The real trigger is untouched: the turn never got to consume it.
	if _, err := os.Stat(filepath.Join(fx.inbox, "wake")); err != nil {
		t.Fatal("the trigger vanished during a shutdown")
	}
}

// The stall arriving via shutdown: the agent consumes its trigger, then the
// supervisor is stopped mid-turn. Nothing is pending, so a restart would idle
// forever -- the same 3.5 hours, reached by an operator's Ctrl-C. The
// supervisor must persist continuity: exactly one recovery turn on restart, and
// no failure counted, because nothing failed.
func TestShutdownAfterConsumptionRecoversOnRestart(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)

	started := make(chan struct{})
	r := &fakeRunner{}
	r.run = func(ctx context.Context, tr supervise.Turn) supervise.TurnResult {
		for _, trig := range tr.Triggers {
			os.Remove(trig.Path) // consumed...
		}
		close(started)
		<-ctx.Done() // ...then killed by the shutdown
		return supervise.TurnResult{ExitCode: 143}
	}
	first := fx.newSupervisor(t, r, 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- first.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("the turn never started")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v on shutdown, want nil", err)
	}

	rs := fx.roleState(t)
	if rs.FailStreak != 0 {
		t.Fatalf("fail_streak = %d after a shutdown; a shutdown is not a failure", rs.FailStreak)
	}
	body, err := os.ReadFile(fx.cfg.RetryPath("reviewer"))
	if err != nil {
		t.Fatalf("no recovery marker after a shutdown that followed consumption: %v", err)
	}
	if !strings.Contains(string(body), "interrupted: true") || !strings.Contains(string(body), "wake") {
		t.Fatalf("the marker does not say it is a recovery from an interruption of the wake turn:\n%s", body)
	}

	// Restart. Exactly one recovery turn, which succeeds, and then a genuine idle.
	second := &fakeRunner{act: func(int, supervise.Turn) supervise.TurnResult {
		return supervise.TurnResult{}
	}}
	s2 := fx.newSupervisor(t, second, 0)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	_ = s2.Run(ctx2) // returns nil on the deadline once it is idle
	if got := second.count(); got != 1 {
		t.Fatalf("restart ran %d turns, want exactly 1 recovery turn", got)
	}
	if _, err := os.Stat(fx.cfg.RetryPath("reviewer")); err == nil {
		t.Fatal("the recovery marker survived a successful recovery turn")
	}
	if rs := fx.roleState(t); rs.Halted || rs.FailStreak != 0 {
		t.Fatalf("after recovery: halted=%v fail_streak=%d", rs.Halted, rs.FailStreak)
	}
}

// lockInbox makes the handoff directory unwritable for the rest of the test,
// so the supervisor's own marker I/O fails while everything else proceeds.
func lockInbox(t *testing.T, fx *fixture) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions; this test needs an unprivileged user")
	}
	if err := os.Chmod(fx.inbox, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(fx.inbox, 0o755) })
}

// A retry marker that cannot be written leaves nothing to re-arm on. Logging
// and carrying on would reproduce the stall through an I/O error; the only
// honest outcome is a halt that says so.
func TestRetryMarkerWriteFailureHalts(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)

	r := &fakeRunner{act: func(_ int, tr supervise.Turn) supervise.TurnResult {
		for _, trig := range tr.Triggers {
			os.Remove(trig.Path)
		}
		lockInbox(t, fx) // consumed, then the marker cannot be written
		return supervise.TurnResult{ExitCode: 1}
	}}
	s := fx.newSupervisor(t, r, 50)
	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	err := s.Run(ctx)
	if !errors.Is(err, supervise.ErrHalted) {
		t.Fatalf("Run returned %v, want ErrHalted: a marker write failure was swallowed and the factory idled", err)
	}
	if got := r.count(); got != 1 {
		t.Fatalf("ran %d turns, want 1", got)
	}
	rs := fx.roleState(t)
	if !rs.Halted || !strings.Contains(rs.HaltReason, "retry marker") {
		t.Fatalf("halt not recorded with its reason: halted=%v reason=%q", rs.Halted, rs.HaltReason)
	}
}

// A retry marker that cannot be removed would re-arm the same retry forever,
// every run a success, invisible to both guards. Same answer: halt, and say why.
func TestRetryMarkerRemovalFailureHalts(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)

	r := &fakeRunner{act: func(n int, tr supervise.Turn) supervise.TurnResult {
		for _, trig := range tr.Triggers {
			if trig.Label != supervise.RetryLabel {
				os.Remove(trig.Path)
			}
		}
		if n == 1 {
			return supervise.TurnResult{ExitCode: 1} // marker gets written
		}
		lockInbox(t, fx) // the retry succeeds, but its marker cannot be removed
		return supervise.TurnResult{}
	}}
	s := fx.newSupervisor(t, r, 50)
	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	err := s.Run(ctx)
	if !errors.Is(err, supervise.ErrHalted) {
		t.Fatalf("Run returned %v, want ErrHalted: a stale marker was left to retry forever", err)
	}
	if got := r.count(); got != 2 {
		t.Fatalf("ran %d turns, want 2 (the failure and the retry that could not be cleaned up)", got)
	}
	if rs := fx.roleState(t); !strings.Contains(rs.HaltReason, "remove the retry marker") {
		t.Fatalf("halt reason does not name the removal failure: %q", rs.HaltReason)
	}
}

func TestProgressResetsTheFailStreak(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)

	r := &fakeRunner{act: func(int, supervise.Turn) supervise.TurnResult {
		_ = fx.progressQuiet()
		return supervise.TurnResult{ExitCode: 1}
	}}
	s := fx.newSupervisor(t, r, 8)

	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatalf("turns that failed but kept reporting progress were halted: %v", err)
	}
	if rs := fx.roleState(t); rs.FailStreak != 0 || rs.Halted {
		t.Fatalf("fail_streak=%d halted=%v after eight failing-but-progressing turns", rs.FailStreak, rs.Halted)
	}
}

// One failure is not a halt -- agents fail -- but it must not be silent either.
func TestSingleConsumedFailureIsRecordedNotHalted(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)

	r := &fakeRunner{act: func(_ int, tr supervise.Turn) supervise.TurnResult {
		for _, trig := range tr.Triggers {
			os.Remove(trig.Path)
		}
		return supervise.TurnResult{ExitCode: 1}
	}}
	s := fx.newSupervisor(t, r, 1)

	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatalf("a single failed turn halted the factory: %v", err)
	}
	rs := fx.roleState(t)
	if rs.Halted {
		t.Fatal("one failure halted the supervisor")
	}
	if rs.FailStreak != 1 {
		t.Fatalf("fail_streak = %d after one consumed failure, want 1: the failure was not recorded", rs.FailStreak)
	}
	if rs.LastTurn == nil || rs.LastTurn.ExitCode == nil || *rs.LastTurn.ExitCode != 1 {
		t.Fatalf("last turn does not record the non-zero exit: %+v", rs.LastTurn)
	}
	// Not silent: the supervisor has re-armed itself.
	if _, err := os.Stat(fx.cfg.RetryPath("reviewer")); err != nil {
		t.Fatalf("no retry marker after a consumed failure: %v", err)
	}
}

// A timeout is a failure for this purpose, whatever the exit code says.
func TestTimedOutTurnsCountAsFailures(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)

	names := []string{"wake", "question.md"}
	r := &fakeRunner{act: func(n int, tr supervise.Turn) supervise.TurnResult {
		for _, trig := range tr.Triggers {
			os.Remove(trig.Path)
		}
		_ = os.WriteFile(filepath.Join(fx.inbox, names[n%2]), []byte("x"), 0o644)
		return supervise.TurnResult{ExitCode: 0, TimedOut: true}
	}}
	s := fx.newSupervisor(t, r, 50)

	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	if err := s.Run(ctx); !errors.Is(err, supervise.ErrHalted) {
		t.Fatalf("Run returned %v, want ErrHalted: timed-out turns were not counted as failures", err)
	}
}

// When a turn both leaves its trigger and fails, both counters climb. Whichever
// threshold is reached first halts, and the reason names that guard -- an
// operator reading the sentinel should not have to guess which rule fired.
func TestBothGuardsClimbAndTheLowerThresholdWins(t *testing.T) {
	fx := newFixture(t) // fail_abort=3 < spin_abort=4
	fx.wake(t)
	r := &fakeRunner{act: func(int, supervise.Turn) supervise.TurnResult {
		return supervise.TurnResult{ExitCode: 1}
	}}
	s := fx.newSupervisor(t, r, 50)

	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	if err := s.Run(ctx); !errors.Is(err, supervise.ErrHalted) {
		t.Fatalf("Run returned %v, want ErrHalted", err)
	}
	if got := r.count(); got != 3 {
		t.Fatalf("ran %d turns, want 3 (the lower of the two thresholds)", got)
	}
	rs := fx.roleState(t)
	if rs.SpinCount != 3 || rs.FailStreak != 3 {
		t.Fatalf("spin=%d fail_streak=%d; both should have climbed together", rs.SpinCount, rs.FailStreak)
	}
	if !strings.Contains(rs.HaltReason, "fail_abort") {
		t.Fatalf("halt reason names the wrong guard: %q", rs.HaltReason)
	}
}

func TestSpinGuardBacksOffBeforeHalting(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)
	r := &fakeRunner{act: func(int, supervise.Turn) supervise.TurnResult {
		return supervise.TurnResult{}
	}}
	s := fx.newSupervisor(t, r, 50)

	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	_ = s.Run(ctx)

	// Three fruitless turns precede the halting fourth, each followed by a
	// backoff. Without one, the loop would re-arm instantly on a trigger that
	// is still there and burn model turns in a tight ring.
	if len(fx.slept) != 3 {
		t.Fatalf("backed off %v, want three delays before the halt", fx.slept)
	}
	for i := 1; i < len(fx.slept); i++ {
		if fx.slept[i] <= fx.slept[i-1] {
			t.Fatalf("backoff did not escalate: %v", fx.slept)
		}
	}
}

// A halted factory stays halted until an operator clears it.
func TestStopSentinelRefusesToStart(t *testing.T) {
	fx := newFixture(t)
	if err := os.WriteFile(fx.cfg.StopPath("reviewer"), []byte("reason: earlier halt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fx.wake(t)

	r := &fakeRunner{}
	s := fx.newSupervisor(t, r, 5)

	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	err := s.Run(ctx)
	if !errors.Is(err, supervise.ErrStopSentinel) {
		t.Fatalf("Run returned %v, want ErrStopSentinel", err)
	}
	if r.count() != 0 {
		t.Fatalf("ran %d turns despite the stop sentinel", r.count())
	}
}

// Two supervisors for one role must not both run. The check is on a process
// handle, not on a command-line pattern.
func TestSecondSupervisorIsRefused(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)

	// The first supervisor must stay alive for the duration, so its turns
	// report progress rather than tripping its own spin guard.
	alive := &fakeRunner{act: func(int, supervise.Turn) supervise.TurnResult {
		_ = fx.progressQuiet()
		return supervise.TurnResult{}
	}}
	first := fx.newSupervisor(t, alive, 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- first.Run(ctx) }()
	// Stop the first supervisor before the fixture directory goes away,
	// whichever way this test exits.
	t.Cleanup(func() { cancel(); <-done })

	// Wait for the first to claim the role.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if fx.roleState(t).Supervisor != nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the first supervisor never recorded itself")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// This process IS the recorded supervisor, so a same-PID claim is allowed
	// (a restart in place). Simulate a genuinely different live process by
	// pointing the record at our parent, which is alive and is not us.
	if _, err := state.Update(fx.cfg.StatePath(), fx.cfg.Name, func(st *state.State) error {
		ref, err := proc.Take(os.Getppid(), "reviewer", "other supervisor")
		if err != nil {
			return err
		}
		st.Role(state.RoleReviewer).Supervisor = &ref
		return nil
	}); err != nil {
		t.Skipf("cannot take a handle on the parent process: %v", err)
	}

	second := fx.newSupervisor(t, &fakeRunner{}, 5)
	if err := second.Run(context.Background()); !errors.Is(err, supervise.ErrAlreadyRunning) {
		t.Fatalf("second supervisor returned %v, want ErrAlreadyRunning", err)
	}
}

// A dead supervisor record must not block a restart forever. This is the other
// half of the liveness check: it has to be able to say "not running" too.
func TestDeadSupervisorRecordIsReplaced(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)
	if _, err := state.Update(fx.cfg.StatePath(), fx.cfg.Name, func(st *state.State) error {
		st.Role(state.RoleReviewer).Supervisor = &proc.Ref{PID: 999999, StartToken: "1", Role: "reviewer"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	r := &fakeRunner{act: func(int, supervise.Turn) supervise.TurnResult {
		os.Remove(filepath.Join(fx.inbox, "wake"))
		return supervise.TurnResult{}
	}}
	s := fx.newSupervisor(t, r, 1)
	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatalf("a dead supervisor record blocked a restart: %v", err)
	}
}

// A pending trigger's age must survive across turns. Re-stamping it every
// observation would make an old signal look new, and an alarm that resets its
// own clock never fires.
func TestPendingAgeSurvivesTurns(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)

	r := &fakeRunner{act: func(int, supervise.Turn) supervise.TurnResult {
		fx.progress(t)
		return supervise.TurnResult{}
	}}
	s := fx.newSupervisor(t, r, 1)
	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	first, ok := fx.roleState(t).OldestPending()
	if !ok {
		t.Fatal("no pending trigger recorded")
	}

	time.Sleep(20 * time.Millisecond)
	s2 := fx.newSupervisor(t, r, 1)
	if err := s2.Run(ctx); err != nil {
		t.Fatal(err)
	}
	second, ok := fx.roleState(t).OldestPending()
	if !ok {
		t.Fatal("the pending trigger vanished")
	}
	if !second.FirstSeen.Equal(first.FirstSeen) {
		t.Fatalf("first-seen moved from %v to %v; the trigger's age was reset",
			first.FirstSeen, second.FirstSeen)
	}
}

// A supervisor whose trigger is always pending and whose turns always report
// progress never blocks in Wait. It must still stop when cancelled, or it
// cannot be shut down at all while it is busy.
func TestCancelStopsABusySupervisor(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)

	r := &fakeRunner{act: func(int, supervise.Turn) supervise.TurnResult {
		_ = fx.progressQuiet()
		return supervise.TurnResult{}
	}}
	s := fx.newSupervisor(t, r, 0) // unbounded, as in production

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Let it get into the loop, then stop it.
	deadline := time.Now().Add(5 * time.Second)
	for r.count() == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the supervisor never ran a turn")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on cancellation, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a busy supervisor did not stop within 10s of cancellation")
	}
}

func TestTriggersForBothRoles(t *testing.T) {
	fx := newFixture(t)
	for _, role := range []string{"producer", "reviewer"} {
		specs, err := supervise.TriggersFor(fx.cfg, role)
		if err != nil {
			t.Fatal(err)
		}
		if len(specs) == 0 {
			t.Fatalf("role %s waits on nothing", role)
		}
	}
	if _, err := supervise.TriggersFor(fx.cfg, "operator"); err == nil {
		t.Fatal("TriggersFor accepted a role that does not exist")
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	fx := newFixture(t)
	cases := map[string]supervise.Options{
		"no config": {Role: "reviewer", Runner: &fakeRunner{}},
		"no runner": {Config: fx.cfg, Role: "reviewer"},
		"bad role":  {Config: fx.cfg, Role: "operator", Runner: &fakeRunner{}},
	}
	for name, o := range cases {
		if s, err := supervise.New(o); err == nil {
			s.Close()
			t.Errorf("%s: New accepted it", name)
		}
	}
}

// The after-turn step is SUBMIT's seat in the §3 loop: it runs once after a
// clean turn, with that turn, and not after a turn that failed or timed
// out -- there is nothing to submit from a turn that did not finish.
func TestAfterTurnRunsOnceAfterACleanTurnOnly(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)
	var seen []string
	fx.afterTurn = func(_ context.Context, tn supervise.Turn, res supervise.TurnResult) (string, error) {
		seen = append(seen, tn.ID)
		return "submitted", nil
	}
	r := &fakeRunner{act: func(n int, _ supervise.Turn) supervise.TurnResult {
		fx.progress(t)
		switch n {
		case 1:
			return supervise.TurnResult{} // clean
		case 2:
			return supervise.TurnResult{ExitCode: 1} // failed
		default:
			return supervise.TurnResult{TimedOut: true, ExitCode: -1}
		}
	}}
	s := fx.newSupervisor(t, r, 3)
	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	_ = s.Run(ctx)
	if r.count() != 3 {
		t.Fatalf("ran %d turns, want 3", r.count())
	}
	if len(seen) != 1 {
		t.Fatalf("after-turn ran %d times %v; want once, after the clean turn only", len(seen), seen)
	}
}

// An after-turn failure is the turn's failure: it counts on the fail streak
// exactly as a non-zero exit does, because a factory whose submit keeps
// failing is stalled the same way -- and would otherwise read as a stream
// of clean turns.
func TestAfterTurnFailureCountsOnTheFailStreak(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)
	fx.afterTurn = func(context.Context, supervise.Turn, supervise.TurnResult) (string, error) {
		return "", errors.New("submit: identity failure")
	}
	r := &fakeRunner{act: func(int, supervise.Turn) supervise.TurnResult {
		return supervise.TurnResult{} // the agent itself is fine every time
	}}
	s := fx.newSupervisor(t, r, 50)
	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	err := s.Run(ctx)
	if !errors.Is(err, supervise.ErrHalted) {
		t.Fatalf("Run returned %v, want ErrHalted after fail_abort after-turn failures", err)
	}
	if got := r.count(); got != 3 {
		t.Fatalf("ran %d turns before halting, want 3 (fail_abort)", got)
	}
	rs := fx.roleState(t)
	if !rs.Halted || rs.LastTurn == nil || rs.LastTurn.ExitCode == nil || *rs.LastTurn.ExitCode != supervise.ExitAfterTurnFailed {
		t.Fatalf("state=%+v; the after-turn failure must be recorded as the turn's exit", rs)
	}
}

// A turn that left processes running is not clean: nothing follows it and
// it counts on the fail streak.
func TestLeftoverTurnIsNotCleanAndNothingFollowsIt(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)
	ran := 0
	fx.afterTurn = func(context.Context, supervise.Turn, supervise.TurnResult) (string, error) { ran++; return "", nil }
	r := &fakeRunner{act: func(int, supervise.Turn) supervise.TurnResult {
		return supervise.TurnResult{ExitCode: 0, Leftover: true}
	}}
	s := fx.newSupervisor(t, r, 50)
	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	err := s.Run(ctx)
	if !errors.Is(err, supervise.ErrHalted) {
		t.Fatalf("Run returned %v, want ErrHalted after fail_abort leftover turns", err)
	}
	if ran != 0 {
		t.Fatalf("after-turn ran %d times after turns that left processes running", ran)
	}
	if got := r.count(); got != 3 {
		t.Fatalf("ran %d turns before halting, want 3", got)
	}
	rs := fx.roleState(t)
	if rs.LastTurn == nil || rs.LastTurn.ExitCode == nil || *rs.LastTurn.ExitCode != supervise.ExitLeftover {
		t.Fatalf("state=%+v", rs.LastTurn)
	}
}

// A halt is a circuit breaker; the operator's restart after removing the
// sentinel is the reset (#30). Left uncleared, the first halt a factory
// ever took kept its health red for good. The recorded halt is cleared on
// that restart and kept as last_halt for the record; a restart with the
// sentinel still present is refused and clears nothing.
func TestRestartAfterTheSentinelIsRemovedClearsTheHalt(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)
	r := &fakeRunner{act: func(int, supervise.Turn) supervise.TurnResult {
		return supervise.TurnResult{ExitCode: 1} // fails, consuming nothing
	}}
	s := fx.newSupervisor(t, r, 50)
	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	if err := s.Run(ctx); !errors.Is(err, supervise.ErrHalted) {
		t.Fatalf("Run returned %v, want ErrHalted", err)
	}
	if rs := fx.roleState(t); !rs.Halted || rs.HaltReason == "" {
		t.Fatalf("not halted: %+v", rs)
	}
	s.Close()

	// A restart with the sentinel still there is refused, and the halt stays.
	s2 := fx.newSupervisor(t, &fakeRunner{act: func(int, supervise.Turn) supervise.TurnResult { return supervise.TurnResult{} }}, 1)
	if err := s2.Run(ctx); err == nil {
		t.Fatal("a restart with the sentinel present was not refused")
	}
	if rs := fx.roleState(t); !rs.Halted || rs.LastHalt != nil {
		t.Fatalf("a refused restart changed the halt record: %+v", rs)
	}
	s2.Close()

	// The operator removes the sentinel and restarts: the breaker resets.
	if err := os.Remove(fx.cfg.StopPath("reviewer")); err != nil {
		t.Fatal(err)
	}
	fx.wake(t)
	s3 := fx.newSupervisor(t, &fakeRunner{act: func(int, supervise.Turn) supervise.TurnResult { fx.progress(t); return supervise.TurnResult{} }}, 1)
	if err := s3.Run(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run after the reset: %v", err)
	}
	rs := fx.roleState(t)
	if rs.Halted || rs.HaltReason != "" || !rs.HaltedAt.IsZero() {
		t.Fatalf("the halt was not cleared by the restart: %+v", rs)
	}
	if rs.LastHalt == nil || rs.LastHalt.Reason == "" || rs.LastHalt.ClearedAt.IsZero() {
		t.Fatalf("the cleared halt was not kept for the record: %+v", rs.LastHalt)
	}
}

// The reset is the removal of a sentinel that was written. A halt whose
// sentinel write failed has nothing an operator could have removed: the
// restart must not read the missing file as an acknowledgement (#32 review).
func TestAHaltWhoseSentinelWasNeverWrittenIsNotResetByARestart(t *testing.T) {
	fx := newFixture(t)
	fx.wake(t)
	stop := fx.cfg.StopPath("reviewer")
	r := &fakeRunner{act: func(int, supervise.Turn) supervise.TurnResult {
		// The sentinel path is occupied by a directory: the write fails.
		_ = os.Mkdir(stop, 0o755)
		return supervise.TurnResult{ExitCode: 1}
	}}
	s := fx.newSupervisor(t, r, 50)
	ctx, cancel := ctxWithTimeout(t)
	defer cancel()
	if err := s.Run(ctx); !errors.Is(err, supervise.ErrHalted) {
		t.Fatalf("Run returned %v, want ErrHalted", err)
	}
	if rs := fx.roleState(t); !rs.Halted || rs.SentinelWritten {
		t.Fatalf("halt state: %+v", rs)
	}
	s.Close()
	if err := os.Remove(stop); err != nil { // now absent, exactly as after a failed write
		t.Fatal(err)
	}

	clean := func() *fakeRunner {
		return &fakeRunner{act: func(int, supervise.Turn) supervise.TurnResult { fx.progress(t); return supervise.TurnResult{} }}
	}
	s2 := fx.newSupervisor(t, clean(), 1)
	if err := s2.Run(ctx); !errors.Is(err, supervise.ErrStopSentinel) {
		t.Fatalf("a restart after a failed sentinel write returned %v, want ErrStopSentinel: the missing file was read as the operator's reset", err)
	}
	s2.Close()
	rs := fx.roleState(t)
	if !rs.Halted || rs.LastHalt != nil || !rs.SentinelWritten {
		t.Fatalf("the refused restart did not keep the halt and persist the sentinel: %+v", rs)
	}
	if body, err := os.ReadFile(stop); err != nil || !strings.Contains(string(body), rs.HaltReason) {
		t.Fatalf("sentinel after the refused restart: %q %v", body, err)
	}

	// Removing the sentinel that was written is the reset.
	if err := os.Remove(stop); err != nil {
		t.Fatal(err)
	}
	fx.wake(t)
	s3 := fx.newSupervisor(t, clean(), 1)
	if err := s3.Run(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run after the reset: %v", err)
	}
	if rs := fx.roleState(t); rs.Halted || rs.SentinelWritten || rs.LastHalt == nil {
		t.Fatalf("the reset did not clear the halt: %+v", rs)
	}
}
