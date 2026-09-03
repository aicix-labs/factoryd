package supervise

import (
	"context"
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
		rs.Supervisor = &self
		rs.WatchMode = string(s.watcher.Mode())
		return nil
	})
	return err
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

	res, runErr := s.runner.Run(ctx, turn, func(p proc.Ref) {
		if _, err := state.Update(s.cfg.StatePath(), s.cfg.Name, func(st *state.State) error {
			if t := st.Role(state.Role(s.role)).CurrentTurn; t != nil && t.ID == turn.ID {
				t.Process = &p
			}
			return nil
		}); err != nil {
			s.log.Warn("could not record the turn's process", "turn", turn.ID, "err", err)
		}
	})

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
		}
		return nil
	}); err != nil {
		return false, err
	}

	s.log.Info("turn finished",
		"turn", turn.ID, "exit", res.ExitCode, "timed_out", res.TimedOut,
		"consumed", didConsume, "progressed", progressed, "spin", spin, "fail_streak", fails)

	if failed && didConsume && !halted {
		// The quiet case: the trigger is gone, so nothing external will re-arm,
		// and to every other signal this now looks like an idle factory. The
		// supervisor re-arms itself with a durable marker, so the streak can
		// climb to fail_abort and halt with a reason -- rather than a factory
		// that sits idle for hours with work stranded (issue #12). A file, not
		// memory: a retry lost on restart recreates the stall it exists to fix.
		if err := s.writeRetry(turn, res, fails, ended); err != nil {
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
func (s *Supervisor) writeRetry(turn Turn, res TurnResult, fails int, at time.Time) error {
	path := s.cfg.RetryPath(s.role)
	real, onRetry := withoutRetry(turn.Triggers)
	origin := labels(real)
	if onRetry && origin == "" {
		origin = readRetryOrigin(path)
	}
	if origin == "" {
		origin = "unknown"
	}
	body := fmt.Sprintf(
		"retry %d of %d\norigin: %s\nafter turn: %s\nexit: %d\ntimed out: %v\nat: %s\n\n"+
			"The previous turn consumed its trigger and then failed. Nothing else is pending.\n",
		fails, s.cfg.Supervisor.FailAbort, origin, turn.ID, res.ExitCode, res.TimedOut, at.Format(time.RFC3339))
	return os.WriteFile(path, []byte(body), 0o644)
}

// readRetryOrigin recovers the "origin:" line from an existing marker.
func readRetryOrigin(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "origin: ") {
			return strings.TrimPrefix(line, "origin: ")
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
		return nil
	}); err != nil {
		s.log.Error("could not record the halt in state", "err", err)
	}
	if err := s.writeStopSentinel(reason, at); err != nil {
		s.log.Error("could not write the stop sentinel", "err", err)
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
