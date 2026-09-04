package supervise

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aicix-labs/factoryd/internal/proc"
	"github.com/aicix-labs/factoryd/internal/state"
	"github.com/aicix-labs/factoryd/internal/watch"
)

// checkStopSentinel refuses to start while a halt sentinel is present.
//
// The sentinel is removed by an operator and by nobody else. A circuit breaker
// that resets itself on the next start is not a circuit breaker; it is a
// slower version of the loop it was supposed to stop.
func (s *Supervisor) checkStopSentinel() error {
	path := s.cfg.StopPath(s.role)
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: %s says %q -- remove it to resume",
		ErrStopSentinel, path, strings.TrimSpace(string(body)))
}

// claim registers this process as the role's supervisor, refusing if another
// live one already holds it.
//
// Liveness is decided on a process handle with its start token, never on a
// command-line pattern. v1 matched argv, which matched the invoking shell and
// matched child subshells, and produced two false duplicate alarms before it
// killed the operator's session twice.
func (s *Supervisor) claim() error {
	self, err := proc.Self(s.role)
	if err != nil {
		return err
	}
	_, err = state.Update(s.cfg.StatePath(), s.cfg.Name, func(st *state.State) error {
		rs := st.Role(state.Role(s.role))
		if prev := rs.Supervisor; prev != nil && prev.PID != self.PID {
			alive, err := prev.Alive()
			if err != nil {
				return fmt.Errorf("checking the recorded supervisor %s: %w", prev, err)
			}
			if alive {
				return fmt.Errorf("%w: %s", ErrAlreadyRunning, prev)
			}
			s.log.Info("replacing a dead supervisor record", "previous", prev.String())
		}
		// A restart is the reset of the circuit breaker. This runs only after
		// checkStopSentinel passed, so the operator has removed the sentinel:
		// the recorded halt is cleared and kept as last_halt for the record
		// (#30). Left in place, the first halt a factory ever took kept its
		// health red for good -- a red that never goes green.
		if rs.Halted && (rs.SentinelWritten == nil || !*rs.SentinelWritten) {
			// Never written, or -- the field absent -- recorded by a binary
			// that predates it, which also recorded the halt before the
			// write and only logged a failure. Unknown cannot authorize a
			// circuit-breaker reset (#37 review): both cases persist the
			// sentinel now and refuse, so the reset is always the removal
			// of a sentinel this binary knows it wrote.
			return errHaltUnacknowledged
		}
		if rs.Halted {
			rs.LastHalt = &state.Halt{Reason: rs.HaltReason, At: rs.HaltedAt, ClearedAt: s.now()}
			rs.Halted, rs.HaltReason, rs.HaltedAt, rs.SentinelWritten = false, "", time.Time{}, nil
			s.log.Info("halt cleared by restart", "reason", rs.LastHalt.Reason, "halted_at", rs.LastHalt.At)
		}
		rs.Supervisor = &self
		rs.WatchMode = string(s.watcher.Mode())
		return nil
	})
	if errors.Is(err, errHaltUnacknowledged) {
		return s.persistUnacknowledgedHalt()
	}
	return err
}

// errHaltUnacknowledged: state says halted, but the sentinel write failed at
// the time, so no sentinel was ever there for an operator to remove.
var errHaltUnacknowledged = errors.New("halt recorded but its sentinel was never persisted")

// persistUnacknowledgedHalt writes the sentinel the halt could not, and
// refuses to start. Only the removal of a written sentinel is the reset:
// reading a sentinel that was never written as "the operator removed it"
// would let the breaker close itself after precisely the control-plane
// failure that kept it from persisting its stop signal.
func (s *Supervisor) persistUnacknowledgedHalt() error {
	var reason string
	var at time.Time
	legacy := false
	if _, err := state.Update(s.cfg.StatePath(), s.cfg.Name, func(st *state.State) error {
		rs := st.Role(state.Role(s.role))
		reason, at, legacy = rs.HaltReason, rs.HaltedAt, rs.SentinelWritten == nil
		if err := s.writeStopSentinel(reason, at); err != nil {
			return fmt.Errorf("%w; writing it now failed too: %v", errHaltUnacknowledged, err)
		}
		rs.SentinelWritten = state.Bool(true)
		return nil
	}); err != nil {
		return err
	}
	s.log.Error("halt sentinel written on restart; the halt stands", "reason", reason, "halted_at", at, "predates_sentinel_written", legacy)
	if legacy {
		return fmt.Errorf("%w: the halt at %s (%s) was recorded by a factoryd that predates sentinel_written, so whether its sentinel was ever written is unknown and cannot authorize a reset; written now at %s -- remove it and restart to resume (once, on this upgrade)",
			ErrStopSentinel, at.Format(time.RFC3339), reason, s.cfg.StopPath(s.role))
	}
	return fmt.Errorf("%w: the halt at %s (%s) was recorded but its sentinel could not be written then; written now at %s -- remove it to resume",
		ErrStopSentinel, at.Format(time.RFC3339), reason, s.cfg.StopPath(s.role))
}

// markSentinelWritten records that the halt's sentinel is on disk, so the
// next start can tell a removed sentinel from one that never existed.
func (s *Supervisor) markSentinelWritten() {
	if _, err := state.Update(s.cfg.StatePath(), s.cfg.Name, func(st *state.State) error {
		st.Role(state.Role(s.role)).SentinelWritten = state.Bool(true)
		return nil
	}); err != nil {
		s.log.Error("could not record that the stop sentinel was written", "err", err)
	}
}

// progressMTime reads the role's progress marker.
//
// SPEC.md §6.3: this is how the supervisor tells "working, not finished" from
// "achieving nothing". A missing marker is the zero time, not an error -- a
// role that has never touched it has simply never reported progress.
func (s *Supervisor) progressMTime() time.Time {
	fi, err := os.Stat(s.cfg.ProgressPath(s.role))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func toPending(ts []watch.Trigger, now time.Time) []state.Pending {
	out := make([]state.Pending, 0, len(ts))
	for _, t := range ts {
		out = append(out, state.Pending{Label: t.Label, Path: t.Path, FirstSeen: now})
	}
	return out
}

func labels(ts []watch.Trigger) string {
	seen := map[string]bool{}
	var out []string
	for _, t := range ts {
		if !seen[t.Label] {
			seen[t.Label] = true
			out = append(out, t.Label)
		}
	}
	return strings.Join(out, ",")
}

// consumed reports whether every trigger the turn acted on is gone.
func consumed(acted []watch.Trigger, remaining []watch.Trigger) bool {
	still := make(map[string]bool, len(remaining))
	for _, t := range remaining {
		still[t.Path] = true
	}
	for _, t := range acted {
		if still[t.Path] {
			return false
		}
	}
	return true
}

// oneTurn runs exactly one agent turn and applies the spin guard. It reports
// whether the supervisor halted.
func (s *Supervisor) oneTurn(ctx context.Context, triggers []watch.Trigger) (bool, error) {
	s.turnSeq++
	now := s.now()
	turn := Turn{
		ID:       fmt.Sprintf("%s-%s-%d", s.role, now.Format("20060102T150405"), s.turnSeq),
		Role:     s.role,
		Triggers: triggers,
	}
	if spec, ok := s.cfg.RoleSpec(s.role); ok && spec.TimeoutSeconds > 0 {
		turn.Deadline = now.Add(time.Duration(spec.TimeoutSeconds) * time.Second)
	}

	// Record the pending triggers and the turn before running anything, so a
	// supervisor killed mid-turn leaves evidence of what it was doing.
	if _, err := state.Update(s.cfg.StatePath(), s.cfg.Name, func(st *state.State) error {
		rs := st.Role(state.Role(s.role))
		rs.SetPending(toPending(triggers, now))
		rs.CurrentTurn = &state.Turn{ID: turn.ID, StartedAt: now, Trigger: labels(triggers)}
		return nil
	}); err != nil {
		return false, err
	}

	before := s.progressMTime()
	s.log.Info("turn starting", "turn", turn.ID, "triggers", labels(triggers))

	var res TurnResult
	var runErr error
	skipped := false
	// A retry marker that names the after-turn step resumes that step
	// alone: the producer's turn already succeeded and its intent is still
	// declared; rerunning the model is new work, not a retry (#42).
	resumeAfterTurn := false
	if _, onRetry := withoutRetry(triggers); onRetry && s.afterTurn != nil && readRetryStep(s.cfg.RetryPath(s.role)) == RetryStepAfterTurn {
		resumeAfterTurn = true
		skipped = true
		s.log.Info("retry resumes the after-turn step; the agent does not rerun", "turn", turn.ID)
	}
	if s.beforeTurn != nil && !resumeAfterTurn {
		msg, err := s.beforeTurn(ctx, turn)
		if err != nil && ctx.Err() == nil {
			// The agent does not run over a tree the step could not prepare.
			// A failed turn, on the streak, with the triggers left pending.
			s.log.Error("before-turn step failed; the turn does not run and counts as failed", "turn", turn.ID, "err", err)
			res, skipped = TurnResult{ExitCode: ExitBeforeTurnFailed}, true
		} else if msg != "" {
			s.log.Info("before-turn step", "turn", turn.ID, "result", msg)
		}
	}
	if !skipped {
		res, runErr = s.runner.Run(ctx, turn, func(p proc.Ref) {
			if _, err := state.Update(s.cfg.StatePath(), s.cfg.Name, func(st *state.State) error {
				if t := st.Role(state.Role(s.role)).CurrentTurn; t != nil && t.ID == turn.ID {
					t.Process = &p
				}
				return nil
			}); err != nil {
				s.log.Warn("could not record the turn's process", "turn", turn.ID, "err", err)
			}
		})
	}

	ended := s.now()

	if ctx.Err() != nil {
		// The supervisor was told to stop while the turn was running. The turn
		// was killed with the process group and its exit code says nothing
		// about the agent. Record that it happened and touch no counter: a
		// clean shutdown that left a failure on the streak would halt the
		// factory on a later, unrelated failure -- and the operator who
		// stopped it would never connect the two.
		if _, err := state.Update(s.cfg.StatePath(), s.cfg.Name, func(st *state.State) error {
			rs := st.Role(state.Role(s.role))
			t := state.Turn{ID: turn.ID, StartedAt: now, EndedAt: &ended, Trigger: labels(triggers), Interrupted: true}
			code := res.ExitCode
			t.ExitCode = &code
			if rs.CurrentTurn != nil && rs.CurrentTurn.ID == turn.ID {
				t.Process = rs.CurrentTurn.Process
			}
			rs.LastTurn = &t
			rs.CurrentTurn = nil
			return nil
		}); err != nil {
			s.log.Warn("could not record the interrupted turn", "turn", turn.ID, "err", err)
		}
		// If the agent had already consumed its trigger when it was killed,
		// nothing external will start the next turn after a restart -- the
		// same stall as a consumed failure, arriving via shutdown. Persist an
		// interrupted marker so the restart runs exactly one recovery turn.
		// The streak stays at zero: this is not a failure, it is continuity.
		if real, onRetry := withoutRetry(triggers); len(real) > 0 && !onRetry {
			if remaining, perr := s.watcher.Pending(); perr == nil && consumed(real, removeRetry(remaining)) {
				if werr := s.writeInterruptedMarker(turn, res, ended); werr != nil {
					// Cannot record continuity: say so where it will be seen, and
					// exit non-zero so the stop is not mistaken for clean.
					s.log.Error("shutdown after a consumed trigger, and the retry marker could not be written; the next start will idle", "err", werr)
					return false, fmt.Errorf("shutdown: %w", werr)
				}
				s.log.Info("turn interrupted after consuming its trigger; a recovery turn will run on restart",
					"turn", turn.ID, "retry", s.cfg.RetryPath(s.role))
			}
		}
		s.log.Info("turn interrupted by shutdown", "turn", turn.ID, "exit", res.ExitCode)
		return false, ctx.Err()
	}

	if runErr != nil {
		// A runner that could not start the turn at all is a supervisor-level
		// fault, not an agent failure.
		s.log.Error("turn could not run", "turn", turn.ID, "err", runErr)
	}

	// A turn that left processes running: they were killed and verified
	// gone before the runner returned, so the tree is quiescent either way.
	// What the strays mean depends on whether the turn did its work. Real
	// agent CLIs leave helpers behind (shell snapshots, language servers,
	// MCP hosts) after posting their audit and signalling; counting such a
	// turn as failed re-armed a retry that reviewed the same change again,
	// and under supersession every redundant verdict grew the change family
	// by one draft (#33). With progress recorded the turn stands and the
	// leak is a hygiene warning, counted in state; with no progress it is
	// the failure the containment check was written for.
	hygiene := false
	if runErr == nil && res.Leftover {
		if s.progressMTime().After(before) {
			hygiene = true
			s.log.Error("turn left processes running after its leader exited; killed. The turn recorded progress, so it stands and is not retried", "turn", turn.ID)
		} else {
			s.log.Error("turn left processes running after its leader exited and recorded no progress; killed, and counting the turn as failed", "turn", turn.ID)
			res.ExitCode = ExitLeftover
		}
	}
	// After a clean turn, the supervisor's own follow-up (submit, for the
	// producer). Its failure is the turn's failure.
	afterTurnDisposition := DispositionTransient
	if runErr == nil && res.ExitCode == 0 && !res.TimedOut && s.afterTurn != nil {
		msg, err := s.afterTurn(ctx, turn, res)
		if err != nil {
			afterTurnDisposition = DispositionOf(err)
			s.log.Error("after-turn step failed; counting the turn as failed", "turn", turn.ID, "disposition", string(afterTurnDisposition), "err", err)
			res.ExitCode = ExitAfterTurnFailed
		} else if msg != "" {
			s.log.Info("after-turn step", "turn", turn.ID, "result", msg)
		}
	}

	after := s.progressMTime()
	remaining, err := s.watcher.Pending()
	if err != nil {
		return false, err
	}

	// The retry marker is the supervisor's, not the agent's: the agent is never
	// asked to consume it, so it must not count as an unconsumed trigger, and
	// it is removed here once the turn it re-armed has run.
	real, onRetry := withoutRetry(triggers)
	remaining = removeRetry(remaining)
	didConsume := consumed(real, remaining) && (len(real) > 0 || onRetry)
	// Progress, not consumption, is what resets the counter. A large task
	// legitimately spans turns; a guard that cannot tell "still working" from
	// "achieving nothing" halts real work, which it did in the first
	// implementation of this rule.
	progressed := after.After(before)

	// A non-zero exit or a timeout is a failed turn whatever happened to the
	// trigger. The spin guard cannot see a turn that consumed its trigger and
	// then failed -- nothing is pending, so nothing re-arms and nothing counts.
	// That is exactly the shape that idled a factory for 3.5h with finished
	// work stranded and every signal green (issue #12).
	failed := res.ExitCode != 0 || res.TimedOut

	var halted bool
	var haltReason string
	spin, fails := 0, 0

	if _, err := state.Update(s.cfg.StatePath(), s.cfg.Name, func(st *state.State) error {
		rs := st.Role(state.Role(s.role))
		t := state.Turn{
			ID: turn.ID, StartedAt: now, EndedAt: &ended,
			Trigger: labels(triggers),
		}
		code := res.ExitCode
		t.ExitCode = &code
		if rs.CurrentTurn != nil && rs.CurrentTurn.ID == turn.ID {
			t.Process = rs.CurrentTurn.Process
		}
		rs.LastTurn = &t
		rs.CurrentTurn = nil
		rs.SetPending(toPending(remaining, ended))

		if didConsume || progressed {
			rs.SpinCount = 0
		} else {
			rs.SpinCount++
		}
		spin = rs.SpinCount

		// Progress resets this one too; consumption does not.
		if failed && !progressed {
			rs.FailStreak++
		} else {
			rs.FailStreak = 0
		}
		fails = rs.FailStreak

		switch {
		case spin >= s.cfg.Supervisor.SpinAbort:
			halted = true
			haltReason = fmt.Sprintf(
				"%d consecutive turns consumed no trigger and reported no progress (spin_abort=%d); last trigger %s, last exit %d",
				spin, s.cfg.Supervisor.SpinAbort, labels(triggers), res.ExitCode)
		case fails >= s.cfg.Supervisor.FailAbort:
			halted = true
			haltReason = fmt.Sprintf(
				"%d consecutive turns failed without reporting progress (fail_abort=%d); last trigger %s, last exit %d, timed out %v",
				fails, s.cfg.Supervisor.FailAbort, labels(triggers), res.ExitCode, res.TimedOut)
		}
		if halted {
			rs.Halted = true
			rs.HaltReason = haltReason
			rs.HaltedAt = ended
			rs.SentinelWritten = state.Bool(false) // until the write below succeeds
		}
		if hygiene {
			rs.LeftoverTurns++
		}
		return nil
	}); err != nil {
		return false, err
	}

	s.log.Info("turn finished",
		"turn", turn.ID, "exit", res.ExitCode, "timed_out", res.TimedOut,
		"consumed", didConsume, "progressed", progressed, "spin", spin, "fail_streak", fails)

	if failed && didConsume && !halted && res.ExitCode == ExitAfterTurnFailed && afterTurnDisposition != DispositionTransient {
		// A blocked or unknown after-turn failure arms nothing: blocked
		// because no retry can change the answer (the change left the
		// producer's hands; the declaration is invalid), unknown because a
		// retry might repeat an external effect whose outcome was not seen.
		// The step has recorded the block where status and health read it;
		// the factory idles, visibly, until the operator acts (#42, #40).
		s.log.Error("after-turn failure is not retried", "turn", turn.ID, "disposition", string(afterTurnDisposition))
		if onRetry {
			if err := os.Remove(s.cfg.RetryPath(s.role)); err != nil && !os.IsNotExist(err) {
				return true, s.haltNow(ended, fmt.Sprintf("could not remove the retry marker after a %s after-turn failure (%v)", afterTurnDisposition, err))
			}
		}
		return false, nil
	}

	if failed && didConsume && !halted {
		// The quiet case: the trigger is gone, so nothing external will re-arm,
		// and to every other signal this now looks like an idle factory. The
		// supervisor re-arms itself with a durable marker, so the streak can
		// climb to fail_abort and halt with a reason -- rather than a factory
		// that sits idle for hours with work stranded (issue #12). A file, not
		// memory: a retry lost on restart recreates the stall it exists to fix.
		step := RetryStepTurn
		if res.ExitCode == ExitAfterTurnFailed {
			step = RetryStepAfterTurn
		}
		if err := s.writeRetry(turn, res, fails, ended, step); err != nil {
			// Without the marker there is nothing to re-arm on and the factory
			// would sit idle -- the exact stall, reached through an I/O error.
			// This is a control-plane failure, and it halts with its reason
			// rather than continuing as though the marker existed.
			return true, s.haltNow(ended, fmt.Sprintf(
				"could not write the retry marker after a consumed failure (%v); nothing is pending and the factory cannot re-arm itself", err))
		}
		s.log.Warn("turn failed after consuming its trigger; re-arming a retry",
			"turn", turn.ID, "exit", res.ExitCode, "timed_out", res.TimedOut,
			"fail_streak", fails, "abort_at", s.cfg.Supervisor.FailAbort,
			"retry", s.cfg.RetryPath(s.role), "backoff", s.backoff(fails).String())
		return false, s.sleep(ctx, s.backoff(fails))
	}

	// The retry marker is removed once the turn it re-armed has run -- unless
	// that turn is the one that halts. Then it stays, like a real trigger
	// does: it is the record of what the factory was retrying when it gave
	// up, and a halt should not erase its own evidence.
	if onRetry && !halted {
		if err := os.Remove(s.cfg.RetryPath(s.role)); err != nil && !os.IsNotExist(err) {
			// A marker that cannot be removed would re-arm the same retry
			// forever, each run a success, invisible to both guards. Halting
			// is the only honest outcome.
			return true, s.haltNow(ended, fmt.Sprintf(
				"could not remove the retry marker after the retry ran (%v); leaving it would retry forever", err))
		}
	}

	switch {
	case halted:
		// Both destinations, deliberately: the sentinel stops the next start,
		// the log and state say why. A halt recorded only in state is a halt
		// nobody sees until they go looking.
		if err := s.writeStopSentinel(haltReason, ended); err != nil {
			s.log.Error("could not write the stop sentinel", "err", err)
		} else {
			s.markSentinelWritten()
		}
		s.log.Error("supervisor halting", "reason", haltReason,
			"sentinel", s.cfg.StopPath(s.role),
			"triggers_preserved", labels(remaining))
		return true, nil

	case didConsume || progressed:
		return false, nil

	default:
		if spin >= s.cfg.Supervisor.SpinWarn {
			s.log.Warn("turn achieved nothing", "spin", spin,
				"warn_at", s.cfg.Supervisor.SpinWarn, "abort_at", s.cfg.Supervisor.SpinAbort)
		}
		// Back off before re-arming. The trigger is still there, so Wait would
		// return instantly and the loop would spend money in a tight ring.
		return false, s.sleep(ctx, s.backoff(spin))
	}
}

// backoff scales with the spin count and is capped, so a stuck factory slows
// down without ever going quiet for so long that a recovery goes unnoticed.
func (s *Supervisor) backoff(spin int) time.Duration {
	base := time.Duration(s.cfg.Supervisor.BackoffSeconds) * time.Second
	if base <= 0 || spin <= 0 {
		return 0
	}
	d := time.Duration(spin) * base
	if max := 10 * base; d > max {
		d = max
	}
	return d
}

// writeStopSentinel records the halt where the next start will find it. The
// triggers are deliberately left in place: they are the only evidence a signal
// ever arrived.
func (s *Supervisor) writeStopSentinel(reason string, at time.Time) error {
	body := fmt.Sprintf("halted %s\nrole: %s\nfactory: %s\nreason: %s\n\nRemove this file to resume.\n",
		at.Format(time.RFC3339), s.role, s.cfg.Name, reason)
	return os.WriteFile(s.cfg.StopPath(s.role), []byte(body), 0o644)
}

// withoutRetry splits the supervisor's retry marker out of a trigger set.
func withoutRetry(ts []watch.Trigger) (real []watch.Trigger, onRetry bool) {
	for _, t := range ts {
		if t.Label == RetryLabel {
			onRetry = true
			continue
		}
		real = append(real, t)
	}
	return real, onRetry
}

func removeRetry(ts []watch.Trigger) []watch.Trigger {
	out, _ := withoutRetry(ts)
	return out
}

// writeRetry records why the next turn is a retry, where the agent can read it.
//
// The origin -- the real trigger the failing chain started from -- is carried
// forward from the previous marker rather than recomputed, so that after N
// retries the file still says what the factory was originally woken for. A
// marker that only named "retry" would be the record of a record.
func (s *Supervisor) writeRetry(turn Turn, res TurnResult, fails int, at time.Time, step string) error {
	path := s.cfg.RetryPath(s.role)
	real, onRetry := withoutRetry(turn.Triggers)
	origin := labels(real)
	if onRetry && origin == "" {
		origin = readRetryLine(path, "origin: ")
	}
	if origin == "" {
		origin = "unknown"
	}
	body := fmt.Sprintf(
		"retry %d of %d\norigin: %s\nstep: %s\nafter turn: %s\nexit: %d\ntimed out: %v\nat: %s\n\n"+
			"The previous turn consumed its trigger and then failed. Nothing else is pending.\n",
		fails, s.cfg.Supervisor.FailAbort, origin, step, turn.ID, res.ExitCode, res.TimedOut, at.Format(time.RFC3339))
	return os.WriteFile(path, []byte(body), 0o644)
}

// Retry steps: what the retry marker re-arms. RetryStepTurn reruns the
// agent; RetryStepAfterTurn resumes the supervisor's own after-turn step
// over the intent the agent already declared (#42).
const (
	RetryStepTurn      = "turn"
	RetryStepAfterTurn = "after-turn"
)

// readRetryStep recovers the "step:" line from an existing marker. A marker
// without one (written by an earlier build) reruns the turn.
func readRetryStep(path string) string {
	if v := readRetryLine(path, "step: "); v != "" {
		return v
	}
	return RetryStepTurn
}

// readRetryLine recovers one prefixed line from an existing marker.
func readRetryLine(path, prefix string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}

// haltNow records a control-plane halt -- a failure of the supervisor's own
// bookkeeping, not of the agent -- in state and as the sentinel, and returns
// ErrHalted. Both destinations are attempted even if the first fails: a halt
// that only one of them knows about is a halt nobody can act on.
func (s *Supervisor) haltNow(at time.Time, reason string) error {
	if _, err := state.Update(s.cfg.StatePath(), s.cfg.Name, func(st *state.State) error {
		rs := st.Role(state.Role(s.role))
		rs.Halted, rs.HaltReason, rs.HaltedAt = true, reason, at
		rs.SentinelWritten = state.Bool(false) // until the write below succeeds
		return nil
	}); err != nil {
		s.log.Error("could not record the halt in state", "err", err)
	}
	if err := s.writeStopSentinel(reason, at); err != nil {
		s.log.Error("could not write the stop sentinel", "err", err)
	} else {
		s.markSentinelWritten()
	}
	s.log.Error("supervisor halting", "reason", reason, "sentinel", s.cfg.StopPath(s.role))
	return ErrHalted
}

// writeInterruptedMarker records that a shutdown cut a turn off after it had
// consumed its trigger, so the next start knows to run one recovery turn.
func (s *Supervisor) writeInterruptedMarker(turn Turn, res TurnResult, at time.Time) error {
	real, _ := withoutRetry(turn.Triggers)
	body := fmt.Sprintf(
		"retry 0 of %d\norigin: %s\nafter turn: %s\nexit: %d\ninterrupted: true\nat: %s\n\n"+
			"The supervisor was stopped after this turn consumed its trigger. This is a recovery turn, not a failure.\n",
		s.cfg.Supervisor.FailAbort, labels(real), turn.ID, res.ExitCode, at.Format(time.RFC3339))
	return os.WriteFile(s.cfg.RetryPath(s.role), []byte(body), 0o644)
}
