// Package supervise runs one agent role: block on a trigger, run exactly one
// turn, re-arm.
//
// One implementation, parameterised by role. In v1 the producer had no relaunch
// loop at all -- it stopped after every verdict and needed a human to re-prompt
// it -- while the reviewer's watcher was single-shot and, once it had fired,
// left later signals reaching nobody. Both are the same missing thing: nothing
// owned continuity. Here the supervisor owns all of it, and the agent turns are
// one-shot by construction.
package supervise

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/proc"
	"github.com/aicix-labs/factoryd/internal/watch"
)

// Turn is one unit of work handed to an agent.
type Turn struct {
	ID       string
	Role     string
	Triggers []watch.Trigger
	// Deadline bounds the turn.
	Deadline time.Time
}

// TurnResult is what a turn did.
type TurnResult struct {
	ExitCode int
	// TimedOut records that the turn was killed at its deadline. It is
	// distinct from a non-zero exit: an agent that failed and an agent that
	// never finished need different responses from an operator.
	TimedOut bool
}

// Runner executes one turn. started is called with a handle to the turn's
// process as soon as there is one, so the supervisor can record it in state and
// the status page can show a real process tree.
type Runner interface {
	Run(ctx context.Context, t Turn, started func(proc.Ref)) (TurnResult, error)
}

// ErrHalted is returned when the spin guard trips. It is deliberately an error:
// a supervisor that halts must exit non-zero, or an init system will report it
// as a clean shutdown.
var ErrHalted = errors.New("supervisor halted")

// ErrStopSentinel is returned when a halt sentinel is already present at start.
var ErrStopSentinel = errors.New("stop sentinel present")

// ErrAlreadyRunning is returned when another live supervisor holds this role.
var ErrAlreadyRunning = errors.New("a supervisor for this role is already running")

// Options configures a Supervisor.
type Options struct {
	Config *config.Config
	Role   string
	Runner Runner
	Log    *slog.Logger

	// Now and Sleep are injectable so the spin guard can be tested without
	// waiting out real backoffs.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	// MaxTurns bounds the loop. Zero means unbounded, which is what production
	// wants; tests set it so a loop that never terminates fails as a test
	// rather than hanging one.
	MaxTurns int
}

// Supervisor is one role's loop.
type Supervisor struct {
	cfg     *config.Config
	role    string
	runner  Runner
	log     *slog.Logger
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error
	maxTurn int

	watcher *watch.Watcher
	turnSeq int
}

// TriggersFor returns the trigger specs a role waits on (SPEC.md §6.2).
//
// The handoff directories are named from the reviewer's point of view: inbox is
// what arrives for review, outbox is what the reviewer sends back.
func TriggersFor(cfg *config.Config, role string) ([]watch.Spec, error) {
	switch role {
	case "producer":
		return []watch.Spec{
			{Label: "brief", Dir: cfg.InboxDir(), Pattern: "brief.md"},
			{Label: "answer", Dir: cfg.OutboxDir(), Pattern: "answer.md"},
			{Label: "verdict", Dir: cfg.OutboxDir(), Pattern: "*.json"},
		}, nil
	case "reviewer":
		return []watch.Spec{
			{Label: "wake", Dir: cfg.InboxDir(), Pattern: "wake"},
			{Label: "question", Dir: cfg.InboxDir(), Pattern: "question.md"},
		}, nil
	default:
		return nil, fmt.Errorf("supervise: role %q is not producer or reviewer", role)
	}
}

// New builds a Supervisor and opens its watcher.
func New(opts Options) (*Supervisor, error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("supervise: no config")
	}
	if _, ok := opts.Config.RoleSpec(opts.Role); !ok {
		return nil, fmt.Errorf("supervise: role %q is not producer or reviewer", opts.Role)
	}
	if opts.Runner == nil {
		return nil, fmt.Errorf("supervise: no runner")
	}

	s := &Supervisor{
		cfg:     opts.Config,
		role:    opts.Role,
		runner:  opts.Runner,
		log:     opts.Log,
		now:     opts.Now,
		sleep:   opts.Sleep,
		maxTurn: opts.MaxTurns,
	}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	s.log = s.log.With("role", s.role, "factory", s.cfg.Name)
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.sleep == nil {
		s.sleep = sleepCtx
	}

	specs, err := TriggersFor(s.cfg, s.role)
	if err != nil {
		return nil, err
	}
	w, err := watch.New(specs, watch.Options{
		Interval:  time.Duration(s.cfg.Supervisor.PollIntervalSeconds) * time.Second,
		ForcePoll: s.cfg.Supervisor.ForcePoll,
	})
	if err != nil {
		return nil, err
	}
	s.watcher = w
	return s, nil
}

// Close releases the watcher.
func (s *Supervisor) Close() error { return s.watcher.Close() }

// Watcher exposes the watcher, for status reporting.
func (s *Supervisor) Watcher() *watch.Watcher { return s.watcher }

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run is the loop. It returns nil only on context cancellation -- a clean
// shutdown -- and an error on anything else, including a halt.
func (s *Supervisor) Run(ctx context.Context) error {
	if err := s.checkStopSentinel(); err != nil {
		return err
	}
	if err := s.claim(); err != nil {
		return err
	}

	mode := s.watcher.Mode()
	if reason := s.watcher.ModeReason(); reason != "" {
		// A watcher that fell back is still correct, but slower and worth
		// saying out loud. Silent degradation is how an operator ends up
		// debugging latency that was configured, not accidental.
		s.log.Warn("watcher is not event-driven", "mode", mode, "reason", reason)
	} else {
		s.log.Info("watcher armed", "mode", mode)
	}

	for {
		// Checked at the top of every iteration, not only inside Wait. When a
		// trigger is already pending, Wait returns instantly, so a turn that
		// keeps reporting progress would otherwise loop forever and ignore
		// cancellation entirely -- an idle-looking supervisor that cannot be
		// stopped is exactly what gets killed by command-line pattern.
		if err := ctx.Err(); err != nil {
			s.log.Info("supervisor stopping", "reason", "context done")
			return nil
		}
		if s.maxTurn > 0 && s.turnSeq >= s.maxTurn {
			return nil
		}

		triggers, err := s.watcher.Wait(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				s.log.Info("supervisor stopping", "reason", "context done")
				return nil
			}
			return err
		}

		halted, err := s.oneTurn(ctx, triggers)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
		if halted {
			return ErrHalted
		}
	}
}
