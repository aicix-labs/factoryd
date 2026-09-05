package supervise

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aicix-labs/factoryd/internal/brief"
	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/proc"
	"github.com/aicix-labs/factoryd/internal/state"
)

type queueRaceRunner struct{ runs int }

func (r *queueRaceRunner) Run(context.Context, Turn, func(proc.Ref)) (TurnResult, error) {
	r.runs++
	return TurnResult{}, nil
}

func queueTestConfig(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{filepath.Join(root, "inbox"), filepath.Join(root, "outbox")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{
		Name: "queue-race", TargetBranch: "main",
		Paths:      config.Paths{Root: root, ProducerWorkdir: filepath.Join(root, "work"), SubmitRepo: filepath.Join(root, "submit")},
		Roles:      config.Roles{Producer: config.RoleSpec{Command: []string{"true"}}},
		Supervisor: config.Supervisor{PollIntervalSeconds: 1, SpinWarn: 2, SpinAbort: 4, FailAbort: 3, VerdictAttempts: 3, ForcePoll: true},
	}
	if err := brief.Ensure(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The root-side update is deliberately injected immediately after the locked
// check-and-take returns. The follow-up handoff confirmation must restore the
// source and refuse the agent: this is the exact old check/rename interleave,
// but on the far side of the new atomic move.
func TestQueuedBriefRestoresWhenCycleOpensImmediatelyAfterAtomicTake(t *testing.T) {
	cfg := queueTestConfig(t)
	source := filepath.Join(cfg.BriefsDir(), "010-next.md")
	if err := os.WriteFile(source, []byte("next\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Update(cfg.StatePath(), cfg.Name, func(st *state.State) error {
		st.SetCycle(state.CycleNew, time.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	runner := &queueRaceRunner{}
	s, err := New(Options{Config: cfg, Role: "producer", Runner: runner, MaxTurns: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.afterQueuedTake = func() {
		if _, err := state.Update(cfg.StatePath(), cfg.Name, func(st *state.State) error {
			st.SetCycle(state.CycleOpen, time.Now()).ChangeID = "49"
			return nil
		}); err != nil {
			t.Fatalf("opening the cycle: %v", err)
		}
	}
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.runs != 0 {
		t.Fatalf("runner ran %d times after the cycle opened", runner.runs)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source brief was not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.BriefsDoneDir(), "010-next.md")); !os.IsNotExist(err) {
		t.Fatalf("done brief survived a refused handoff: %v", err)
	}
	st, err := state.Load(cfg.StatePath(), cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if st.Cycle == nil || st.Cycle.Phase != state.CycleOpen {
		t.Fatalf("cycle=%+v, want open", st.Cycle)
	}
	if st.Role(state.RoleProducer).QueueReservation != nil {
		t.Fatalf("refused handoff left a reservation: %+v", st.Role(state.RoleProducer).QueueReservation)
	}
}

// QueueStart records this reservation only after CurrentTurn exists. If the
// supervisor dies before brief.Take, a restart reties that pending source to a
// new turn instead of treating CycleWorking as an in-flight draft forever.
func TestPendingQueuedReservationResumesAfterRestart(t *testing.T) {
	cfg := queueTestConfig(t)
	source := filepath.Join(cfg.BriefsDir(), "010-next.md")
	done, err := brief.DonePath(cfg, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("next\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Update(cfg.StatePath(), cfg.Name, func(st *state.State) error {
		st.SetCycle(state.CycleWorking, time.Now())
		rs := st.Role(state.RoleProducer)
		rs.CurrentTurn = &state.Turn{ID: "crashed-queue-turn", StartedAt: time.Now(), Trigger: brief.Label}
		rs.QueueReservation = &state.QueueReservation{Source: source, Done: done, Turn: "crashed-queue-turn", ReservedAt: time.Now()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	runner := &queueRaceRunner{}
	s, err := New(Options{Config: cfg, Role: "producer", Runner: runner, MaxTurns: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.runs != 1 {
		t.Fatalf("recovered pending reservation ran %d times, want 1", runner.runs)
	}
	if _, err := os.Stat(done); err != nil {
		t.Fatalf("recovered brief was not taken to done: %v", err)
	}
	st, err := state.Load(cfg.StatePath(), cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if st.Role(state.RoleProducer).QueueReservation != nil || st.Role(state.RoleProducer).CurrentTurn != nil {
		t.Fatalf("recovery left queue state: %+v", st.Role(state.RoleProducer))
	}
}

// Once the atomic move is recorded, the source is absent by design. Restart
// recovery turns the done/ handoff into a normal retry marker, so the producer
// receives precisely the immutable queued brief instead of waiting on a source
// path that can no longer wake the watcher.
func TestTakenQueuedReservationRecoversThroughRetryMarker(t *testing.T) {
	cfg := queueTestConfig(t)
	source := filepath.Join(cfg.BriefsDir(), "010-next.md")
	done, err := brief.DonePath(cfg, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(done, []byte("next\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Update(cfg.StatePath(), cfg.Name, func(st *state.State) error {
		st.SetCycle(state.CycleWorking, time.Now())
		rs := st.Role(state.RoleProducer)
		rs.CurrentTurn = &state.Turn{ID: "crashed-queue-turn", StartedAt: time.Now(), Trigger: brief.Label}
		rs.QueueReservation = &state.QueueReservation{Source: source, Done: done, Turn: "crashed-queue-turn", ReservedAt: time.Now(), Taken: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	runner := &queueRaceRunner{}
	s, err := New(Options{Config: cfg, Role: "producer", Runner: runner, MaxTurns: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.recoverQueuedReservation(); err != nil {
		t.Fatal(err)
	}
	if got := readRetryLine(cfg.RetryPath("producer"), "brief: "); got != done {
		t.Fatalf("recovery brief=%q, want %q", got, done)
	}
	if st, err := state.Load(cfg.StatePath(), cfg.Name); err != nil || st.Role(state.RoleProducer).QueueReservation != nil {
		t.Fatalf("recovery left taken reservation: state=%+v err=%v", st, err)
	}
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.runs != 1 {
		t.Fatalf("recovered taken reservation ran %d times, want 1", runner.runs)
	}
}
