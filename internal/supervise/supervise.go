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
	"sort"
	"time"

	"github.com/aicix-labs/factoryd/internal/brief"
	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/proc"
	"github.com/aicix-labs/factoryd/internal/state"
	"github.com/aicix-labs/factoryd/internal/watch"
)

// Turn is one unit of work handed to an agent.
type Turn struct {
	ID       string
	Role     string
	Triggers []watch.Trigger
	// Verdicts are the verified facts from the exact outbox bytes admission
	// accepted. They are carried across BeforeTurn into the runner so an
	// agent never learns its verdict facts by reopening a producer-writable
	// handoff file.
	Verdicts []VerifiedVerdict
	// Deadline bounds the turn.
	Deadline time.Time
}

// VerifiedVerdict is the immutable admission snapshot for one verdict
// trigger. Digest identifies the registry entry that admitted it and lets the
// post-turn state update tombstone precisely that issuance.
type VerifiedVerdict struct {
	Path           string
	ChangeID       string
	Kind           string
	SHA            string
	Branch         string
	DeclaredBranch string
	Digest         string
}

// TurnResult is what a turn did.
type TurnResult struct {
	ExitCode int
	// TimedOut records that the turn was killed at its deadline. It is
	// distinct from a non-zero exit: an agent that failed and an agent that
	// never finished need different responses from an operator.
	TimedOut bool
	// Leftover records that the turn's leader exited but other processes in
	// its group were still running -- a background child the agent left
	// behind. They were killed and, under cgroup containment, verified gone
	// before the runner returned, so nothing the producer started is still
	// rewriting the tree when the supervisor judges the turn. A leftover
	// turn that recorded no progress counts as a failure; one that did
	// stands, with the leak counted as hygiene (#33). A child that detached from the group escapes this check; the root-
	// mediated crossing does not rely on it (§4.4: every read is no-follow,
	// judged on the opened descriptor).
	Leftover bool
	// Containment names what held the turn's processes: "cgroup" (every
	// descendant, however it re-parented, killed and verified gone) or
	// "process-group" (unprivileged fallback: a setsid'd child escapes it).
	// It is said so that the weaker one is never mistaken for the stronger.
	Containment string
}

// ExitLeftover is recorded for a turn whose leader exited zero, left
// processes running in its group, and recorded no progress. A leftover
// turn that did record progress stands: the strays are killed either way,
// and the leak is counted as hygiene, not failure (#33).
const ExitLeftover = 1001

// Disposition classifies an after-turn failure for the supervisor (#42).
// It decides what follows: a transient failure re-arms a retry that
// resumes the after-turn step alone, without rerunning the producer; a
// blocked one and an unknown one arm nothing -- blocked because retrying
// cannot change the answer, unknown because retrying might repeat an
// external effect whose outcome was not observed. Unknown is the default
// for any failure that does not say otherwise.
type Disposition string

const (
	DispositionTransient Disposition = "transient"
	DispositionBlocked   Disposition = "blocked"
	DispositionUnknown   Disposition = "unknown"
)

// Disposer is implemented by an after-turn error that knows its
// disposition. An error that does not implement it is unknown.
type Disposer interface {
	Disposition() Disposition
}

// DispositionOf reads an error's disposition, unknown by default.
func DispositionOf(err error) Disposition {
	var d Disposer
	if errors.As(err, &d) {
		switch v := d.Disposition(); v {
		case DispositionTransient, DispositionBlocked:
			return v
		}
	}
	return DispositionUnknown
}

// RestartError says the supervising process itself must exit after it has
// recorded the current turn. It is for a control-plane lease whose durable
// cleanup failed: keeping the holder process alive would make liveness-based
// recovery believe the lease is still in use forever.
type RestartError struct{ Err error }

func (e *RestartError) Error() string { return e.Err.Error() }
func (e *RestartError) Unwrap() error { return e.Err }

// RestartRequired marks a failure that requires the current supervisor
// process to exit. A service manager or operator restart then gives state
// liveness recovery a dead owner it can safely reclaim.
func RestartRequired(err error) error { return &RestartError{Err: err} }

// RequiresRestart reports whether err carries a request to terminate this
// supervisor after normal turn finalization.
func RequiresRestart(err error) bool {
	var r *RestartError
	return errors.As(err, &r)
}

// ExitBeforeTurnFailed is recorded for a turn whose before-turn step failed;
// the agent never ran.
const ExitBeforeTurnFailed = 1002

// ExitAfterTurnFailed is the exit code recorded for a turn whose agent
// exited zero but whose after-turn step failed. It is outside any code a
// process can return, so it is never mistaken for the agent's own.
const ExitAfterTurnFailed = 1000

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

	// BeforeTurn runs before the turn's command is started, with the turn
	// and its triggers. It is how the producer's workdir is brought to the
	// target branch when no change is in flight (#35). An error from it is
	// a failed turn: the command does not run, the triggers stay pending,
	// and it counts on the fail streak -- a factory whose workdir cannot be
	// refreshed is stalled, and a turn over a stale tree is the silent case
	// this exists to prevent. The returned string is logged.
	BeforeTurn func(ctx context.Context, t Turn) (string, error)

	// AfterTurn runs after a turn that exited zero and was not interrupted,
	// before progress and consumption are judged. It is how SUBMIT follows
	// the producer's turn in the §3 loop: the turn declares intent in files
	// and exits; the supervisor, outside the sandbox and as itself, does
	// the git and network work. An error from it is a failed turn -- it
	// counts on the fail streak like an agent that exited non-zero, because
	// a factory whose submit keeps failing is stalled exactly the same way.
	// The returned string is logged.
	AfterTurn func(ctx context.Context, t Turn, res TurnResult) (string, error)

	// QueueStart is consulted only for a producer with queued briefs and no
	// other trigger. It performs the final lifecycle check and reserves the
	// cycle for the queued turn while holding the state lock. The command wires
	// it to refresh's merge reconciliation and refresh step, so a human-merged
	// open change releases the next queued brief without launching a second
	// producer turn while the old one is still in flight.
	QueueStart func(ctx context.Context, t Turn) (started bool, reason string, err error)
}

// Supervisor is one role's loop.
type Supervisor struct {
	beforeTurn func(ctx context.Context, t Turn) (string, error)
	afterTurn  func(ctx context.Context, t Turn, res TurnResult) (string, error)
	queueStart func(ctx context.Context, t Turn) (started bool, reason string, err error)
	cfg        *config.Config
	role       string
	runner     Runner
	log        *slog.Logger
	now        func() time.Time
	sleep      func(context.Context, time.Duration) error
	maxTurn    int

	watcher *watch.Watcher
	turnSeq int

	// afterQueuedTake is a deterministic interleaving hook for the package's
	// lifecycle test. It is never set by production construction.
	afterQueuedTake func()
	// afterQueuedConfirm is the matching hook after the final handoff check and
	// before Runner.Run. It proves root lifecycle operations are rejected for
	// the active queued turn, rather than merely observed before process start.
	afterQueuedConfirm func()
}

// RetryLabel names the supervisor-owned retry trigger.
const RetryLabel = "retry"

// TriggersFor returns the trigger specs a role waits on (SPEC.md §6.2).
//
// The handoff directories are named from the reviewer's point of view: inbox is
// what arrives for review, outbox is what the reviewer sends back.
func TriggersFor(cfg *config.Config, role string) ([]watch.Spec, error) {
	// The supervisor's own trigger (see Config.RetryPath). Listed last so that
	// when a real trigger is also present, it is what the turn is told it was
	// woken for first.
	retry := watch.Spec{Label: RetryLabel, Dir: cfg.InboxDir(), Pattern: role + "-retry"}
	switch role {
	case "producer":
		return []watch.Spec{
			{Label: "brief", Dir: cfg.InboxDir(), Pattern: "brief.md"},
			{Label: brief.Label, Dir: cfg.BriefsDir(), Pattern: "*.md"},
			{Label: "answer", Dir: cfg.OutboxDir(), Pattern: "answer.md"},
			{Label: "verdict", Dir: cfg.OutboxDir(), Pattern: "*.json"},
			retry,
		}, nil
	case "reviewer":
		return []watch.Spec{
			{Label: "wake", Dir: cfg.InboxDir(), Pattern: "wake"},
			{Label: "question", Dir: cfg.InboxDir(), Pattern: "question.md"},
			retry,
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

	if opts.Role == "producer" {
		if err := brief.Ensure(opts.Config); err != nil {
			return nil, err
		}
	}

	s := &Supervisor{
		beforeTurn: opts.BeforeTurn,
		afterTurn:  opts.AfterTurn,
		queueStart: opts.QueueStart,
		cfg:        opts.Config,
		role:       opts.Role,
		runner:     opts.Runner,
		log:        opts.Log,
		now:        opts.Now,
		sleep:      opts.Sleep,
		maxTurn:    opts.MaxTurns,
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
	if err := s.checkVerdictRegistry(); err != nil {
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
		if err := s.recoverQueuedReservation(); err != nil {
			return err
		}
		if err := s.recoverReviewerPipelineWait(); err != nil {
			return err
		}

		triggers, err := s.watcher.Wait(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				s.log.Info("supervisor stopping", "reason", "context done")
				return nil
			}
			return err
		}

		triggers = s.selectQueuedBrief(triggers)
		if len(triggers) == 0 {
			continue
		}

		admitted, err := s.admitVerdicts(triggers)
		if err != nil {
			return err
		}
		if len(admitted.Triggers) == 0 {
			continue // everything pending was quarantined; the watcher settles
		}
		halted, err := s.oneTurn(ctx, admitted)
		if err != nil {
			var deferred *turnDeferredError
			if errors.As(err, &deferred) {
				if deferred.reason != "" {
					s.log.Info("producer turn deferred", "reason", deferred.reason)
				}
				if err := s.sleep(ctx, time.Duration(s.cfg.Supervisor.PollIntervalSeconds)*time.Second); err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						return nil
					}
					return err
				}
				continue
			}
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

// recoverReviewerPipelineWait recreates the supervisor-owned retry marker for
// a durable CI wait after a crash. The reviewer command may have consumed its
// ordinary wake before the supervisor wrote the marker, so state -- not a
// producer/reviewer-writable handoff file -- is the restart authority.
func (s *Supervisor) recoverReviewerPipelineWait() error {
	if s.role != string(state.RoleReviewer) {
		return nil
	}
	st, err := state.Load(s.cfg.StatePath(), s.cfg.Name)
	if err != nil {
		return err
	}
	if st.Role(state.RoleReviewer).PipelineWait == nil {
		return nil
	}
	if _, err := os.Lstat(s.cfg.RetryPath(s.role)); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("examining reviewer pipeline retry marker: %w", err)
	}
	return s.writePipelineRetry(*st.Role(state.RoleReviewer).PipelineWait, s.now())
}

// recoverQueuedReservation closes the two crash windows around an ordered
// handoff. Before the rename, the source remains watched and QueueStart reties
// the reservation to the replacement turn. After the rename, the source is
// intentionally gone, so this writes a normal supervisor retry that names the
// durable done/ path. The marker is written before state forgets the
// reservation; a crash in that tiny sequence is idempotent on restart.
func (s *Supervisor) recoverQueuedReservation() error {
	if s.role != "producer" {
		return nil
	}
	st, err := state.Load(s.cfg.StatePath(), s.cfg.Name)
	if err != nil {
		return err
	}
	q := st.Role(state.RoleProducer).QueueReservation
	if q == nil {
		return nil
	}
	q = &state.QueueReservation{
		Source: q.Source, Done: q.Done, Turn: q.Turn, ReservedAt: q.ReservedAt, Taken: q.Taken,
		ProcessStarted: q.ProcessStarted, ProcessFinished: q.ProcessFinished,
	}
	if expected, err := brief.DonePath(s.cfg, q.Source); err != nil || expected != q.Done {
		return s.blockBrokenQueuedReservation(q, "reservation paths are outside the configured brief queue")
	}

	_, sourceErr := os.Stat(q.Source)
	_, doneErr := os.Stat(q.Done)
	if !q.Taken {
		switch {
		case sourceErr == nil:
			return nil // QueueStart will retie this pending source to the new turn.
		case os.IsNotExist(sourceErr) && doneErr == nil:
			// The rename completed but the state write did not. Runner is only
			// called after that write returns, so restoring this source cannot
			// race an agent that saw the done handoff.
			_, err := state.Update(s.cfg.StatePath(), s.cfg.Name, func(st *state.State) error {
				rs := st.Role(state.RoleProducer)
				if current := rs.QueueReservation; current != nil && current.Source == q.Source && current.Done == q.Done && !current.Taken {
					return brief.Restore(s.cfg, q.Source, q.Done)
				}
				return nil
			})
			return err
		default:
			return s.blockBrokenQueuedReservation(q, "pending source is missing and no recoverable done handoff exists")
		}
	}

	if doneErr != nil {
		if sourceErr == nil {
			// A lifecycle change restored the source, but the process died
			// after rename and before the state write clearing Taken. The
			// source is authoritative again; leave the new lifecycle phase
			// alone and let the normal queue selector see it.
			_, err := state.Update(s.cfg.StatePath(), s.cfg.Name, func(st *state.State) error {
				rs := st.Role(state.RoleProducer)
				if current := rs.QueueReservation; current != nil && current.Source == q.Source && current.Done == q.Done && current.Taken {
					rs.QueueReservation = nil
				}
				return nil
			})
			return err
		}
		return s.blockBrokenQueuedReservation(q, "taken done handoff is missing")
	}
	if err := queuedProcessExited(st, q); err != nil {
		return err
	}
	markerBrief := readRetryLine(s.cfg.RetryPath(s.role), "brief: ")
	if markerBrief != "" && markerBrief != q.Done {
		return s.blockBrokenQueuedReservation(q, "a different supervisor retry marker is already pending")
	}
	if markerBrief != q.Done {
		if err := s.writeQueuedRecoveryMarker(q); err != nil {
			return err
		}
	}
	_, err = state.Update(s.cfg.StatePath(), s.cfg.Name, func(st *state.State) error {
		rs := st.Role(state.RoleProducer)
		if current := rs.QueueReservation; current != nil && current.Source == q.Source && current.Done == q.Done && current.Taken {
			rs.QueueReservation = nil
		}
		return nil
	})
	return err
}

// queuedProcessExited is deliberately fail-closed. A supervisor can die while
// its agent survives (SIGKILL skips runner cleanup), so a retry of the taken
// done/ handoff is authorized only after the exact recorded process instance
// is dead. A missing or unreadable handle is not evidence of safety.
func queuedProcessExited(st *state.State, q *state.QueueReservation) error {
	rs := st.Role(state.RoleProducer)
	if q.ProcessFinished {
		return nil
	}
	if !q.ProcessStarted && (rs.CurrentTurn == nil || rs.CurrentTurn.Process == nil) {
		// The supervisor died before Runner reported a process. Runner invokes
		// that callback before the agent is allowed to make progress, so the
		// durable handoff can safely be rearmed.
		return nil
	}
	if rs.CurrentTurn == nil || rs.CurrentTurn.ID != q.Turn || rs.CurrentTurn.Process == nil {
		return fmt.Errorf("queued brief %q was taken by turn %s, but its process cannot be proven dead; refusing recovery", q.Source, q.Turn)
	}
	alive, err := rs.CurrentTurn.Process.Alive()
	if err != nil {
		return fmt.Errorf("queued brief %q was taken by turn %s, but its process liveness could not be checked: %w", q.Source, q.Turn, err)
	}
	if alive {
		return fmt.Errorf("queued brief %q was taken by turn %s and process %s is still alive; refusing duplicate recovery", q.Source, q.Turn, rs.CurrentTurn.Process)
	}
	return nil
}

func (s *Supervisor) writeQueuedRecoveryMarker(q *state.QueueReservation) error {
	body := fmt.Sprintf(
		"retry 0 of %d\norigin: %s\nstep: %s\nbrief: %s\nafter turn: %s\nrecovered queued handoff: true\nat: %s\n\n"+
			"factoryd restarted after taking this queued brief; run the same immutable done handoff once.\n",
		s.cfg.Supervisor.FailAbort, brief.Label, RetryStepTurn, q.Done, q.Turn, s.now().Format(time.RFC3339))
	return os.WriteFile(s.cfg.RetryPath(s.role), []byte(body), 0o644)
}

func (s *Supervisor) blockBrokenQueuedReservation(q *state.QueueReservation, why string) error {
	_, err := state.Update(s.cfg.StatePath(), s.cfg.Name, func(st *state.State) error {
		rs := st.Role(state.RoleProducer)
		current := rs.QueueReservation
		if current == nil || current.Source != q.Source || current.Done != q.Done {
			return nil
		}
		rs.Blocked = &state.Block{
			Disposition: "blocked",
			Reason:      "queued brief reservation cannot be recovered: " + why,
			Turn:        current.Turn,
			At:          s.now(),
		}
		if rs.CurrentTurn != nil && rs.CurrentTurn.ID == current.Turn {
			rs.CurrentTurn = nil
		}
		rs.QueueReservation = nil
		if st.Cycle != nil && st.Cycle.Phase == state.CycleWorking {
			st.SetCycle(state.CycleUnknown, s.now()).Note = "queued brief reservation cannot be recovered"
		}
		return nil
	})
	return err
}

// selectQueuedBrief serializes the operator backlog. A queued brief never
// shares a producer turn with another trigger: a verdict/answer/legacy brief
// describes existing work and must settle before a fresh work order begins.
// When the queue is the only wake-up, exactly its lexical first entry starts
// a cycle; every later entry remains pending. Lifecycle admission happens only
// after oneTurn durably records the selected turn; otherwise a crash between a
// successful reservation and CurrentTurn would strand the queue in working.
func (s *Supervisor) selectQueuedBrief(triggers []watch.Trigger) []watch.Trigger {
	if s.role != "producer" {
		return triggers
	}
	var queued, other []watch.Trigger
	for _, t := range triggers {
		if t.Label == brief.Label {
			queued = append(queued, t)
		} else {
			other = append(other, t)
		}
	}
	if len(queued) == 0 {
		return triggers
	}
	if len(other) > 0 {
		return other
	}
	sort.Slice(queued, func(i, j int) bool { return queued[i].Path < queued[j].Path })
	return []watch.Trigger{queued[0]}
}

func (s *Supervisor) startQueuedCycle(ctx context.Context, turn Turn) (bool, string, error) {
	if s.queueStart != nil {
		return s.queueStart(ctx, turn)
	}

	started := false
	var reason string
	_, err := state.Update(s.cfg.StatePath(), s.cfg.Name, func(st *state.State) error {
		rs := st.Role(state.Role(s.role))
		source, done, ok := queuedBriefPaths(s.cfg, turn.Triggers)
		if !ok {
			return fmt.Errorf("queued cycle start without exactly one queued brief")
		}
		if q := rs.QueueReservation; q != nil {
			if q.Source == source && q.Done == done && !q.Taken && st.Cycle != nil && st.Cycle.Phase == state.CycleWorking {
				q.Turn = turn.ID // a restart resumes the durable reservation
				started = true
				return nil
			}
			reason = "another queued brief reservation is still active"
			return nil
		}
		if err := st.PermitProducerWorktreeUse(); err != nil {
			reason = err.Error()
			return nil
		}
		if st.Cycle == nil {
			reason = "no cycle record; run factoryd refresh --force before taking queued work"
			return nil
		}
		switch st.Cycle.Phase {
		case state.CycleNew, state.CycleFinished, state.CycleClean:
			// A caller without the command's refresh hook still gets the key
			// invariant: the eligible phase is changed under the same lock as
			// the decision before the queued file can be taken.
			st.SetCycle(state.CycleWorking, s.now())
			rs.QueueReservation = &state.QueueReservation{Source: source, Done: done, Turn: turn.ID, ReservedAt: s.now()}
			started = true
		default:
			reason = "cycle is " + st.Cycle.Phase + "; a queued brief waits until the in-flight work settles"
		}
		return nil
	})
	if err != nil {
		return false, "", err
	}
	return started, reason, nil
}

// queuedBriefPaths returns the one factory-selected queue source and its done
// path. Queue turns are intentionally singular; accepting two would make a
// single durable reservation unable to say which handoff may be resumed.
func queuedBriefPaths(cfg *config.Config, triggers []watch.Trigger) (source, done string, ok bool) {
	for _, trigger := range triggers {
		if trigger.Label != brief.Label {
			continue
		}
		if source != "" {
			return "", "", false
		}
		p, err := brief.DonePath(cfg, trigger.Path)
		if err != nil {
			return "", "", false
		}
		source, done = trigger.Path, p
	}
	return source, done, source != ""
}

// checkVerdictRegistry stops a producer before it observes or quarantines a
// legacy outbox. A v1 file may be a real unresolved verdict, but its bytes
// were producer-writable before a registry existed, so neither admission nor
// automatic quarantine is safe. claim has already persisted the explicit
// migration-required state at this point.
func (s *Supervisor) checkVerdictRegistry() error {
	if s.role != "producer" {
		return nil
	}
	st, err := state.Load(s.cfg.StatePath(), s.cfg.Name)
	if err != nil {
		return err
	}
	if err := st.VerdictRegistry.MigrationError(); err != nil {
		return fmt.Errorf("%w; run `factoryd migrate --config %s verdict-registry` before supervising the producer", err, s.cfg.Path())
	}
	return nil
}
