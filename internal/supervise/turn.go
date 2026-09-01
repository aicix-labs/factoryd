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
	if runErr != nil && ctx.Err() == nil {
		// A runner that could not start the turn at all is a supervisor-level
		// fault, not an agent failure.
		s.log.Error("turn could not run", "turn", turn.ID, "err", runErr)
	}

	after := s.progressMTime()
	remaining, err := s.watcher.Pending()
	if err != nil {
		return false, err
	}

	didConsume := consumed(triggers, remaining)
	// Progress, not consumption, is what resets the counter. A large task
	// legitimately spans turns; a guard that cannot tell "still working" from
	// "achieving nothing" halts real work, which it did in the first
	// implementation of this rule.
	progressed := after.After(before)

	var halted bool
	var haltReason string
	spin := 0

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

		if spin >= s.cfg.Supervisor.SpinAbort {
			halted = true
			haltReason = fmt.Sprintf(
				"%d consecutive turns consumed no trigger and reported no progress (spin_abort=%d); last trigger %s, last exit %d",
				spin, s.cfg.Supervisor.SpinAbort, labels(triggers), res.ExitCode)
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
		"consumed", didConsume, "progressed", progressed, "spin", spin)

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
