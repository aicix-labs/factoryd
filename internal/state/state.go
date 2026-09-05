// Package state is the factory's one state document.
//
// v1 spread handoff state across eight ad-hoc files with no schema, and it was
// never possible to say what the factory believed at a given moment. Here there
// is exactly one document per factory, versioned, written atomically, and
// refused outright if its schema version is not the one this build understands.
//
// The only files outside this document are the trigger files a sandboxed agent
// must be able to see and write directly (SPEC.md §6.2).
package state

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aicix-labs/factoryd/internal/proc"
)

// SchemaVersion is the state schema this build understands.
//
// v2 adds the verdict registry. A v1 outbox document was producer-writable
// and therefore cannot be retroactively treated as a registered verdict. A
// v1 document loads into an explicit migration-required state instead of
// silently quarantining the old handoff files. v3 adds exact process handles
// for long-running status and health services, so an older binary cannot
// silently erase the record that doctor uses to detect a replaced executable.
// v4 refuses to mistake an empty v3 service map for proof that no pre-registry
// status or health process remains alive. v5 records both the review decision
// for the active open change and open changes deliberately left to the operator
// while the opt-in brief queue starts independent work. v6 records a reviewer
// pipeline wait durably, so a restart resumes a self-clearing CI refusal rather
// than turning it into an operator gate or silently dropping its retry. v7
// makes that retry bounded by durable attempt and deadline records. A v6
// pipeline wait has no trustworthy budget, so the v7 migration preserves it
// only as an exhausted, operator-visible condition; it never silently resumes
// an unbounded automatic loop.
const SchemaVersion = 7

var (
	// ErrProducerLifecycleBusy means a queued handoff or admitted producer
	// turn owns the producer worktree, so root-side cycle transitions must wait.
	ErrProducerLifecycleBusy = errors.New("producer queued handoff is active")
	// ErrProducerWorktreeBusy means a root-side submission has leased the
	// producer worktree while it copies and gates a declared change. A queued
	// handoff must wait rather than start an agent beside that use.
	ErrProducerWorktreeBusy = errors.New("producer worktree is leased for submission")
	// ErrServiceAlreadyRunning means the exact handle for a long-running
	// factoryd service is still live. Replacing it would hide a service doctor
	// must be able to inspect.
	ErrServiceAlreadyRunning = errors.New("factoryd service is already running")
	// ErrServiceRegistryMigrationRequired means a state predates durable
	// service handles, so an operator must complete the restart sweep before
	// doctor or a new long-running service can trust the registry.
	ErrServiceRegistryMigrationRequired = errors.New("service registry migration required")
	// ErrSchemaMigrationRequired prevents an old state writer from being
	// stranded by a new schema before the operator has completed the required
	// all-process restart sweep.
	ErrSchemaMigrationRequired = errors.New("state schema migration required")
)

// Role is one of the two agent roles.
type Role string

const (
	RoleProducer Role = "producer"
	RoleReviewer Role = "reviewer"
)

// Roles is every role, in a stable order.
var Roles = []Role{RoleProducer, RoleReviewer}

// Service is a factoryd command that keeps using a factory after its initial
// invocation. Unlike one-shot verbs, it must leave an exact handle in state
// for doctor to inspect after a binary install.
type Service string

const (
	ServiceStatusServe Service = "status-serve"
	ServiceHealthLoop  Service = "health-loop"
)

// Services is every long-running service in a stable order.
var Services = []Service{ServiceStatusServe, ServiceHealthLoop}

// ValidService reports whether service is a known long-running service.
func ValidService(service Service) bool {
	for _, known := range Services {
		if service == known {
			return true
		}
	}
	return false
}

// ServiceRegistry is the trust state of long-running service handles. A v3
// state has no way to distinguish "no service was ever started" from "an old
// status or health process is still serving from an inode an install deleted".
// It therefore loads blocked until an operator explicitly attests the restart
// sweep of every long-running factoryd process with `factoryd migrate ...
// service-registry`.
type ServiceRegistry struct {
	Status      string    `json:"status"`
	Reason      string    `json:"reason,omitempty"`
	BlockedAt   time.Time `json:"blocked_at,omitempty"`
	AttestedAt  time.Time `json:"attested_at,omitempty"`
	Attestation string    `json:"attestation,omitempty"`
}

const (
	ServiceRegistryReady             = "ready"
	ServiceRegistryMigrationRequired = "migration-required"
)

// Ready reports whether service handles can be trusted to enumerate all
// currently running long-lived factoryd services.
func (r *ServiceRegistry) Ready() bool {
	return r != nil && r.Status == ServiceRegistryReady
}

// MigrationError returns the durable reason service handles cannot yet be
// trusted.
func (r *ServiceRegistry) MigrationError() error {
	if r.Ready() {
		return nil
	}
	if r == nil || r.Reason == "" {
		return ErrServiceRegistryMigrationRequired
	}
	return fmt.Errorf("%w: %s", ErrServiceRegistryMigrationRequired, r.Reason)
}

// Turn is one agent turn. Agent turns are one-shot: the supervisor owns all
// continuity.
type Turn struct {
	ID        string     `json:"id"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	// Trigger names what caused the turn ("inbox/wake", "inbox/brief.md").
	Trigger string `json:"trigger,omitempty"`
	// Interrupted records that the supervisor was stopped while the turn was
	// running. The exit code is whatever the kill produced and says nothing
	// about the agent; an interrupted turn counts toward no guard.
	Interrupted bool `json:"interrupted,omitempty"`
	// Process is the turn's own process, held by handle.
	Process *proc.Ref `json:"process,omitempty"`
}

// Running reports whether the turn has not yet ended.
func (t *Turn) Running() bool { return t != nil && t.EndedAt == nil }

// Age is how long the turn has been running, or how long it ran.
func (t *Turn) Age(now time.Time) time.Duration {
	if t == nil {
		return 0
	}
	if t.EndedAt != nil {
		return t.EndedAt.Sub(t.StartedAt)
	}
	return now.Sub(t.StartedAt)
}

// Pending is a trigger that exists and has not been consumed. It carries the
// time it was first observed, so an unhandled signal has an age rather than
// just a presence -- v1 could tell you a trigger existed but not that it had
// been sitting there for two hours.
type Pending struct {
	Label     string    `json:"label"`
	Path      string    `json:"path"`
	FirstSeen time.Time `json:"first_seen"`
}

// RoleState is everything the factory knows about one role.
type RoleState struct {
	// Supervisor is the supervising process, by handle. Nil means no
	// supervisor has ever registered.
	Supervisor  *proc.Ref `json:"supervisor,omitempty"`
	CurrentTurn *Turn     `json:"current_turn,omitempty"`
	LastTurn    *Turn     `json:"last_turn,omitempty"`

	// SpinCount is consecutive turns that produced no progress. It is reset by
	// evidence of progress, not by trigger consumption: a large task
	// legitimately spans turns, and a guard that cannot tell "still working"
	// from "achieving nothing" halts real work.
	SpinCount int `json:"spin_count"`
	// FailStreak is consecutive turns that exited non-zero or timed out
	// without reporting progress. Unlike SpinCount it does not reset when the
	// trigger is consumed: a turn that consumes its trigger and then fails
	// leaves nothing pending, which reads as idle to every other signal.
	FailStreak int `json:"fail_streak"`
	// Pending are the triggers seen and not yet consumed, oldest first.
	Pending []Pending `json:"pending,omitempty"`
	// QueueReservation is the factoryd-owned record of a queued brief between
	// selecting its source and completing its turn. A filesystem rename cannot
	// be atomic with state, so this is the restart authority for deciding
	// whether to take the source again, restore a pre-record crash window, or
	// resume the already-taken done/ handoff. The producer never writes it.
	QueueReservation *QueueReservation `json:"queue_reservation,omitempty"`
	// SubmissionLease serializes root-side use of the producer worktree with
	// admission of the next queued brief. It is held before CopyTree through
	// the gate outcome, never in a producer-writable directory.
	SubmissionLease *ProducerWorktreeLease `json:"submission_lease,omitempty"`
	// WatchMode records whether the watcher is event-driven or polling, so a
	// degraded watcher is visible rather than merely slower.
	WatchMode string `json:"watch_mode,omitempty"`
	// LastProgressAt is the mtime of the role's progress file at the last
	// observation.
	LastProgressAt time.Time `json:"last_progress_at,omitempty"`

	// Halted records that the circuit breaker tripped. HaltReason must be
	// non-empty whenever Halted is true: a halt with no stated reason is
	// indistinguishable from an idle factory.
	Halted     bool      `json:"halted"`
	HaltReason string    `json:"halt_reason,omitempty"`
	HaltedAt   time.Time `json:"halted_at,omitempty"`
	// SentinelWritten records whether the halt's stop sentinel was
	// persisted. The reset is the removal of a sentinel that was written; a
	// halt whose sentinel write failed has nothing an operator could have
	// removed, so a restart re-persists it and refuses rather than reading
	// the missing file as an acknowledgement (#32 review). Three states,
	// deliberately: false is recorded with the halt and true after the
	// write, so nil means the halt was recorded by a binary that predates
	// the field. Nil is unknown, and unknown cannot authorize a reset: a
	// pre-field binary also recorded the halt before the write and only
	// logged a failure, so the restart persists the sentinel and refuses
	// once, and the removal of that sentinel is the acknowledgement (#37).
	SentinelWritten *bool `json:"sentinel_written,omitempty"`
	// Blocked is a submission the producer's after-turn step could not
	// complete and must not retry: blocked (no retry can change the answer)
	// or unknown (a retry might repeat an external effect). It is durable,
	// operator-visible in status and health, cleared only by a later
	// submission that succeeds -- never by progress, a restart, or a new
	// turn (#42).
	Blocked *Block `json:"blocked,omitempty"`
	// PipelineWait is a reviewer merge attempt GitLab explicitly refused only
	// because CI is pending or red. It is neither a producer task nor an
	// operator gate: the reviewer supervisor re-arms a later review attempt
	// only within the durable attempt/time budget carried by the wait. It is
	// recorded in state rather than inferred from a retry marker so an agent
	// cannot consume the marker and strand a self-clearing condition.
	PipelineWait *PipelineWait `json:"pipeline_wait,omitempty"`
	// TriggerAttempts counts, per pending trigger path, the turns that ran
	// with it and left it pending. Reset when the trigger is consumed. The
	// supervisor's own bound on how long a verdict may be carried (#50
	// review): kept here, root-owned, not in a handoff directory the
	// producer writes.
	TriggerAttempts map[string]int `json:"trigger_attempts,omitempty"`
	// LeftoverTurns counts turns that recorded progress but left processes
	// behind after the leader exited. The strays are killed and verified
	// gone; the turn stands (#33). The count is the hygiene signal an
	// operator reads to know an agent's tooling is leaking.
	LeftoverTurns int `json:"leftover_turns,omitempty"`
	// LastHalt is the most recent halt after it was cleared: the audit
	// trail of a circuit breaker that tripped and was reset. The reset is
	// the operator's restart after removing the sentinel (#30); a halt that
	// nothing cleared kept health and status red after the role recovered.
	LastHalt *Halt `json:"last_halt,omitempty"`
}

// PipelineWait is the provider-confirmed, retryable reason a reviewer merge
// did not happen. The exact change and head bind a later review attempt to the
// decision it is resuming; a changed head starts a fresh budget. Attempts and
// Deadline are durable, preventing restarts or progress touches from extending
// an automatic review loop forever.
type PipelineWait struct {
	ChangeID     string     `json:"change_id"`
	SHA          string     `json:"sha"`
	Reason       string     `json:"reason"`
	At           time.Time  `json:"at"`
	Attempts     int        `json:"attempts"`
	AttemptLimit int        `json:"attempt_limit"`
	Deadline     time.Time  `json:"deadline"`
	ExhaustedAt  *time.Time `json:"exhausted_at,omitempty"`
}

// Exhausted reports whether this wait may no longer re-arm automatically.
func (w PipelineWait) Exhausted(at time.Time) bool {
	return w.ExhaustedAt != nil || w.Attempts >= w.AttemptLimit || !at.Before(w.Deadline)
}

// PipelineWaitExhausted is the reviewer block disposition produced when the
// durable retry budget is spent. It is a named state, not a terminal verdict:
// an operator can resolve CI or a new reviewed head can start a fresh budget.
const PipelineWaitExhausted = "pipeline-wait-exhausted"

// QueueReservation binds one ordered brief to the producer turn that reserved
// the cycle. Source is the queue path, Done is factoryd's immutable handoff
// path, and Taken says the state write after the rename completed. Process
// flags bind the handoff to the producer process, so restart recovery never
// launches a duplicate beside an orphaned agent. A restart can therefore
// distinguish a pending source from a taken brief even though the watched
// source path no longer exists in the latter case.
type QueueReservation struct {
	Source     string    `json:"source"`
	Done       string    `json:"done"`
	Turn       string    `json:"turn"`
	ReservedAt time.Time `json:"reserved_at"`
	Taken      bool      `json:"taken"`
	// ProcessStarted means factoryd crossed the durable launch boundary. The
	// exact process handle follows immediately in CurrentTurn.Process; if that
	// write is lost, recovery blocks rather than assuming no agent exists.
	ProcessStarted  bool `json:"process_started,omitempty"`
	ProcessFinished bool `json:"process_finished,omitempty"`
}

// ProducerWorktreeLease records the root process that is copying and gating a
// producer declaration. The process handle makes a crash recoverable: queue
// admission can clear a lease only after proving its owner is gone, and blocks
// safely if that proof cannot be made.
type ProducerWorktreeLease struct {
	Holder     proc.Ref  `json:"holder"`
	AcquiredAt time.Time `json:"acquired_at"`
}

// PermitProducerCycleMutation refuses a root-side lifecycle mutation while a
// queued handoff's producer process has not finished. Queue reservation makes
// CycleWorking mean more than a phase: it binds a specific handoff to the
// current producer turn. Submit calls this before submitting, opening, or
// cleaning a cycle, so no root-side operation can create a draft in the small
// interval after the handoff check but before the agent is launched.
func (s *State) PermitProducerCycleMutation() error {
	q := s.Role(RoleProducer).QueueReservation
	if q == nil || q.ProcessFinished {
		return nil
	}
	return fmt.Errorf("%w: %q is reserved for turn %s, whose process has not finished", ErrProducerLifecycleBusy, q.Source, q.Turn)
}

// AcquireProducerWorktreeLease is the durable barrier immediately before
// root-side CopyTree. It gives an admitted producer trigger priority if it won
// the state lock first, and otherwise makes producer admission wait until the
// submission has finished using the producer worktree and gate result.
func (s *State) AcquireProducerWorktreeLease(holder proc.Ref, now time.Time) error {
	if holder.PID <= 0 || holder.StartToken == "" {
		return fmt.Errorf("%w: submission lease holder is not an exact process reference", ErrProducerWorktreeBusy)
	}
	if err := s.PermitProducerCycleMutation(); err != nil {
		return err
	}
	if err := s.permitRootProducerWorktreeUse(holder); err != nil {
		return err
	}
	if err := s.permitSubmissionLease(); err != nil {
		return err
	}
	s.Role(RoleProducer).SubmissionLease = &ProducerWorktreeLease{Holder: holder, AcquiredAt: now}
	return nil
}

// permitRootProducerWorktreeUse makes a generic producer CurrentTurn the
// other half of the CopyTree/gate barrier. A root-side submit arriving after a
// legacy brief, answer, verdict, or retry was admitted must wait for that turn
// rather than copy beside it. The producer supervisor is allowed to submit
// from its own AfterTurn after the runner has returned; a separate manual
// submit is not. Dead supervisors and their proven-dead children are cleaned
// here so a crash before a runner process was recorded cannot strand submit.
func (s *State) permitRootProducerWorktreeUse(holder proc.Ref) error {
	rs := s.Role(RoleProducer)
	turn := rs.CurrentTurn
	if turn == nil {
		return nil
	}
	if supervisor := rs.Supervisor; supervisor != nil && supervisor.PID == holder.PID && supervisor.StartToken == holder.StartToken {
		return nil // AfterTurn runs inside the producer supervisor process.
	}
	if turn.Process != nil {
		alive, err := turn.Process.Alive()
		if err != nil {
			return fmt.Errorf("%w: cannot prove current producer turn process %s is gone: %v", ErrProducerLifecycleBusy, turn.Process, err)
		}
		if alive {
			return fmt.Errorf("%w: current producer turn %s process %s still owns the worktree", ErrProducerLifecycleBusy, turn.ID, turn.Process)
		}
	}
	if rs.Supervisor == nil {
		return fmt.Errorf("%w: current producer turn %s has no supervisor identity, so its worktree ownership cannot be released safely", ErrProducerLifecycleBusy, turn.ID)
	}
	alive, err := rs.Supervisor.Alive()
	if err != nil {
		return fmt.Errorf("%w: cannot prove producer supervisor %s is gone: %v", ErrProducerLifecycleBusy, rs.Supervisor, err)
	}
	if alive {
		return fmt.Errorf("%w: current producer turn %s is owned by live supervisor %s", ErrProducerLifecycleBusy, turn.ID, rs.Supervisor)
	}
	rs.CurrentTurn = nil
	return nil
}

// PermitProducerWorktreeUse is called under state.Update before a producer
// turn is admitted or a queued brief is refreshed. A live submit lease is a
// deliberate deferral; a dead owner is reclaimed in this same locked update,
// so a crashed submit cannot strand any producer trigger indefinitely.
func (s *State) PermitProducerWorktreeUse() error {
	return s.permitSubmissionLease()
}

// PermitQueuedProducerHandoff is retained for callers compiled against the
// earlier queue-only name. New admission paths must use
// PermitProducerWorktreeUse: legacy briefs, answers, verdicts, and retries
// use the producer worktree too.
func (s *State) PermitQueuedProducerHandoff() error {
	return s.PermitProducerWorktreeUse()
}

func (s *State) permitSubmissionLease() error {
	rs := s.Role(RoleProducer)
	lease := rs.SubmissionLease
	if lease == nil {
		return nil
	}
	alive, err := lease.Holder.Alive()
	if err != nil {
		return fmt.Errorf("%w: cannot prove submission lease holder %s is gone: %v", ErrProducerWorktreeBusy, lease.Holder, err)
	}
	if alive {
		return fmt.Errorf("%w: holder %s acquired it at %s", ErrProducerWorktreeBusy, lease.Holder, lease.AcquiredAt.UTC().Format(time.RFC3339))
	}
	rs.SubmissionLease = nil
	return nil
}

// ReleaseProducerWorktreeLease clears only the lease held by this exact
// process. A mismatched release is refused rather than allowing one submit to
// erase another submit's barrier.
func (s *State) ReleaseProducerWorktreeLease(holder proc.Ref) error {
	rs := s.Role(RoleProducer)
	lease := rs.SubmissionLease
	if lease == nil {
		return nil
	}
	if lease.Holder.PID != holder.PID || lease.Holder.StartToken != holder.StartToken {
		return fmt.Errorf("%w: refusing to release lease held by %s", ErrProducerWorktreeBusy, lease.Holder)
	}
	rs.SubmissionLease = nil
	return nil
}

// Block is a submission that will not be retried automatically.
type Block struct {
	Disposition string    `json:"disposition"` // blocked or unknown
	Reason      string    `json:"reason"`
	Turn        string    `json:"turn,omitempty"`
	Family      string    `json:"family,omitempty"`
	Digest      string    `json:"digest,omitempty"`
	At          time.Time `json:"at"`
	// Quarantined lists the declaration files moved aside, so the intent
	// is not resubmitted by the next turn and is not lost either.
	Quarantined []string `json:"quarantined,omitempty"`
}

// IssuedVerdict is a registry entry: what was written to the outbox, by
// whom, and the sha256 of the bytes written.
type IssuedVerdict struct {
	Kind           string    `json:"kind"`
	Branch         string    `json:"branch,omitempty"`
	DeclaredBranch string    `json:"declared_branch,omitempty"`
	Digest         string    `json:"digest"`
	RecordedBy     string    `json:"recorded_by,omitempty"`
	IssuedAt       time.Time `json:"issued_at"`
	// ConsumedAt is the factoryd-owned, one-shot receipt for this issuance.
	// Keeping the digest and receipt together means a producer that restores
	// the byte-identical handoff body cannot manufacture a fresh turn.
	ConsumedAt     *time.Time `json:"consumed_at,omitempty"`
	ConsumedByTurn string     `json:"consumed_by_turn,omitempty"`
	// PendingSubmission is written only by factoryd's root-side submit step
	// after it has read a declaration matching this changes-requested
	// verdict. It survives an after-turn retry, but is not a receipt: only a
	// successful submission writes ConsumedAt.
	PendingSubmission *VerdictSubmission `json:"pending_submission,omitempty"`
}

// VerdictSubmission binds a verified changes-requested verdict to the
// root-side submission attempting to satisfy it.
type VerdictSubmission struct {
	Turn   string    `json:"turn"`
	Digest string    `json:"digest"`
	At     time.Time `json:"at"`
}

// DigestOf is the registry digest of a handoff document's bytes.
func DigestOf(b []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(b)) }

// VerdictRegistry is the state of the factory-owned verdict registry. The
// registry is either ready to admit verdicts or explicitly blocked while an
// operator migrates a v1 state document. There is deliberately no implicit
// "trust all extant outbox files" transition: their bytes were writable by
// the producer before the registry existed.
type VerdictRegistry struct {
	Status    string    `json:"status"`
	Reason    string    `json:"reason,omitempty"`
	BlockedAt time.Time `json:"blocked_at,omitempty"`
}

const (
	VerdictRegistryReady             = "ready"
	VerdictRegistryMigrationRequired = "migration-required"
)

// ErrVerdictRegistryMigrationRequired says verdict admission is intentionally
// blocked until pre-registry handoff files have been explicitly quarantined.
var ErrVerdictRegistryMigrationRequired = errors.New("verdict registry migration required")

// Ready reports whether this is a registry-aware state allowed to admit
// verdicts.
func (r *VerdictRegistry) Ready() bool {
	return r != nil && r.Status == VerdictRegistryReady
}

// MigrationError returns the durable reason a verdict registry is blocked.
func (r *VerdictRegistry) MigrationError() error {
	if r.Ready() {
		return nil
	}
	if r == nil || r.Reason == "" {
		return ErrVerdictRegistryMigrationRequired
	}
	return fmt.Errorf("%w: %s", ErrVerdictRegistryMigrationRequired, r.Reason)
}

// Bool is a pointer to b, for the tri-state fields.
func Bool(b bool) *bool { return &b }

// Cycle phases. Refresh runs only at CycleNew and CycleFinished: every
// other phase names work the producer may still be doing, or a state this
// build cannot vouch for.
const (
	// CycleNew: the workdir is at Base and nothing has touched it.
	CycleNew = "new"
	// CycleWorking: a producer turn has started on this cycle. Its edits,
	// declared or not, are the producer's work and are never reset away.
	CycleWorking = "working"
	// CycleSubmitting: submit has validated an intent and is about to push
	// and open a draft. Written BEFORE the push, so a crash between the
	// draft's creation and the record of it leaves this, not nothing.
	CycleSubmitting = "submitting"
	// CycleOpen: a draft is open for ChangeID. Supersession moves ChangeID
	// to the newest member of the family.
	CycleOpen = "open"
	// CycleFinished: the draft named by ChangeID merged.
	CycleFinished = "finished"
	// CycleClean: a turn consumed its trigger and declared nothing, or
	// declared a tree identical to the target. The cycle produced no
	// change; the next brief starts a new one. Without this a no-op turn
	// left "working" behind for good, and the stale workdir returned (#41
	// review). Undeclared edits do not survive it: an edit the producer
	// wanted would have been declared (#12).
	CycleClean = "clean"
	// CycleUnknown: the document predates the record, or the record was
	// unreadable. Unknown authorizes nothing; the operator's forced refresh
	// starts the next cycle.
	CycleUnknown = "unknown"
)

// Cycle is the producer's work cycle.
type Cycle struct {
	Phase string `json:"phase"`
	// Base is the target-branch commit the workdir was refreshed to.
	Base string `json:"base,omitempty"`
	// Family and Digest identify the declared change being submitted: the
	// declared branch name and the content-derived immutable branch.
	Family string `json:"family,omitempty"`
	Digest string `json:"digest,omitempty"`
	// ChangeID is the open draft, newest in the family.
	ChangeID string `json:"change_id,omitempty"`
	// ReviewDecision is the verified reviewer decision for this exact open
	// change. LastVerdict is an activity feed across every change, so it is not
	// authority to advance this cycle (#56 review).
	ReviewDecision *ReviewDecision `json:"review_decision,omitempty"`
	Note           string          `json:"note,omitempty"`
	StartedAt      time.Time       `json:"started_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// ReviewDecision is the lineage-bound portion of a verified reviewer verdict.
// Its change identity lives on Cycle; keeping the branch facts here proves the
// decision was about that cycle's immutable submitted branch rather than a
// similarly named or subsequently superseded change.
type ReviewDecision struct {
	Kind           string    `json:"kind"`
	Branch         string    `json:"branch"`
	DeclaredBranch string    `json:"declared_branch"`
	SHA            string    `json:"sha"`
	Summary        string    `json:"summary,omitempty"`
	At             time.Time `json:"at"`
}

// OperatorGate is an open change the reviewer declared complete from the
// producer's perspective, but which a human still has to merge or otherwise
// resolve. It is retained separately from Cycle when the operator has opted
// into starting a later queued brief beside it.
type OperatorGate struct {
	ChangeID string    `json:"change_id"`
	Branch   string    `json:"branch"`
	Family   string    `json:"family"`
	SHA      string    `json:"sha"`
	Summary  string    `json:"summary"`
	GatedAt  time.Time `json:"gated_at"`
}

// SetCycle moves the cycle to phase, keeping identifiers unless the phase
// starts over.
func (s *State) SetCycle(phase string, now time.Time) *Cycle {
	if s.Cycle == nil || phase == CycleNew {
		s.Cycle = &Cycle{StartedAt: now}
	}
	s.Cycle.Phase = phase
	s.Cycle.UpdatedAt = now
	return s.Cycle
}

// Halt is a cleared halt, kept for the record.
type Halt struct {
	Reason    string    `json:"reason"`
	At        time.Time `json:"at"`
	ClearedAt time.Time `json:"cleared_at"`
}

// SetPending replaces the pending set with what was just observed, keeping the
// original first-seen time for anything already known. Re-stamping FirstSeen on
// every observation would make every trigger look freshly arrived, and an alarm
// that resets its own clock never fires.
func (rs *RoleState) SetPending(observed []Pending) {
	prior := make(map[string]time.Time, len(rs.Pending))
	for _, p := range rs.Pending {
		prior[p.Path] = p.FirstSeen
	}
	out := make([]Pending, 0, len(observed))
	for _, p := range observed {
		if first, ok := prior[p.Path]; ok {
			p.FirstSeen = first
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FirstSeen.Before(out[j].FirstSeen) })
	rs.Pending = out
}

// OldestPending returns the longest-waiting pending trigger.
func (rs *RoleState) OldestPending() (Pending, bool) {
	if len(rs.Pending) == 0 {
		return Pending{}, false
	}
	return rs.Pending[0], true
}

// Verdict is the reviewer's decision on one change.
type Verdict struct {
	ChangeID string `json:"change_id"`
	// Kind is merged, changes-requested, or operator-gated. There are three.
	Kind string `json:"kind"`
	SHA  string `json:"sha,omitempty"`
	// Summary must stand alone: the counterpart may be a sandboxed agent that
	// cannot fetch the comment this summary refers to.
	Summary string    `json:"summary"`
	At      time.Time `json:"at"`
	// MergeCommit is set only for a verified merge.
	MergeCommit string `json:"merge_commit,omitempty"`
	// Branch is the change's pushed source branch, <declared>-<tree>, and
	// DeclaredBranch the family the producer declared (#29). A change is
	// never updated in place: a fix is a new immutable branch that
	// SUPERSEDES the old draft only if declared under the same family. A
	// producer told "changes requested on change 48" holds no credential
	// and no network, and nothing else in its reach maps the id to that
	// family. So the verdict carries it.
	Branch         string `json:"branch,omitempty"`
	DeclaredBranch string `json:"declared_branch,omitempty"`
	// RecordedBy names who recorded the verdict: "reviewer" (signal, the
	// gate) or "operator" (factoryd verdict, a human closing the loop on a
	// change they merged outside factoryd, #43). Empty means reviewer.
	RecordedBy string `json:"recorded_by,omitempty"`
}

// ParseVerdict parses the already-read bytes of an outbox/<id>.json handoff
// document and requires its lineage to be complete and coherent before a turn
// may act on it. Admission deliberately uses this form: digest verification
// and the facts handed to a turn must come from the same immutable byte
// snapshot, not from two opens of a producer-writable path.
func ParseVerdict(b []byte, path string) (Verdict, error) {
	var v Verdict
	if err := json.Unmarshal(b, &v); err != nil {
		return Verdict{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := v.ValidateLineage(path); err != nil {
		return Verdict{}, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

// ReadVerdictFile is the convenience form for callers that only need to
// inspect a file. The supervisor does not use it for admission because that
// would separate verification from use.
func ReadVerdictFile(path string) (Verdict, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Verdict{}, err
	}
	return ParseVerdict(b, path)
}

// ValidateLineage requires everything a producer needs to act on a verdict
// and nothing that contradicts itself: one of the three kinds; a change id
// that is the file's own name; a pushed branch; a declared family that is
// the family of that branch.
func (v Verdict) ValidateLineage(path string) error {
	if !ValidVerdictKind(v.Kind) {
		return fmt.Errorf("verdict kind %q is not one of merged, changes-requested, operator-gated", v.Kind)
	}
	if v.ChangeID == "" {
		return errors.New("verdict names no change")
	}
	if path != "" {
		if want := v.ChangeID + ".json"; filepath.Base(path) != want {
			return fmt.Errorf("verdict for change %s is in a file named %s, not %s", v.ChangeID, filepath.Base(path), want)
		}
	}
	if v.Branch == "" || v.DeclaredBranch == "" {
		return errors.New("verdict carries no branch lineage (written before the family was recorded?); a turn cannot know which family to re-declare")
	}
	if got := FamilyOf(v.Branch); got != v.DeclaredBranch {
		return fmt.Errorf("verdict's declared family %q is not the family of its branch %q (%q)", v.DeclaredBranch, v.Branch, got)
	}
	return nil
}

// FamilyOf recovers the declared family from a pushed branch
// <declared>-<10 hex>; a name without that suffix is its own family.
func FamilyOf(branch string) string {
	i := strings.LastIndex(branch, "-")
	if i <= 0 || len(branch)-i-1 != 10 {
		return branch
	}
	for _, c := range branch[i+1:] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return branch
		}
	}
	return branch[:i]
}

// Verdict kinds. There are three and only three (SPEC.md §2).
const (
	VerdictMerged           = "merged"
	VerdictChangesRequested = "changes-requested"
	VerdictOperatorGated    = "operator-gated"
)

// ValidVerdictKind reports whether kind is one of the three.
func ValidVerdictKind(kind string) bool {
	switch kind {
	case VerdictMerged, VerdictChangesRequested, VerdictOperatorGated:
		return true
	}
	return false
}

// State is the whole document.
type State struct {
	SchemaVersion int       `json:"schema_version"`
	Factory       string    `json:"factory"`
	UpdatedAt     time.Time `json:"updated_at"`

	Roles map[Role]*RoleState `json:"roles"`

	// LastVerdict is the most recent verdict recorded, whatever it was.
	LastVerdict *Verdict `json:"last_verdict,omitempty"`
	// Issued is the registry of verdicts factoryd issued, by change id: the
	// factoryd-owned identity of a verdict (#50 review). The outbox is a
	// handoff directory the producer writes, so a file there is a trigger
	// only if its digest matches an entry here; a forged or replaced file
	// is not a verdict, and a count keyed by this identity survives a
	// producer deleting and recreating the file.
	Issued map[string]IssuedVerdict `json:"issued,omitempty"`
	// VerdictRegistry states whether Issued can be used to admit an outbox
	// document. It is explicit because v1 outbox documents cannot be trusted
	// merely because a later build knows how to hash them.
	VerdictRegistry *VerdictRegistry `json:"verdict_registry"`
	// Cycle is the producer's work cycle: the durable, write-ahead record of
	// where the workdir stands between a refresh and a merge (#35). It is
	// never inferred from absence: a document that predates it loads as
	// CycleUnknown, and only a refresh forced by the operator starts a new
	// one from there.
	Cycle *Cycle `json:"cycle,omitempty"`

	// Health is the cadence record of the health tick: for each standing
	// condition, when it was first seen and last alerted. It lives here, under
	// the same lock as everything else, so that a tick that crashed between
	// deciding to alert and recording that it did cannot alert twice -- or,
	// worse, decide it already had.
	Health map[string]*Condition `json:"health,omitempty"`
	// ServiceRegistry says whether Services is a complete inventory. An absent
	// record is unknown, never evidence that no old service remains alive.
	ServiceRegistry *ServiceRegistry `json:"service_registry"`
	// Services holds exact handles for long-running non-supervisor commands.
	// A service records itself before it begins using the factory and removes
	// only its own handle on exit. This lets doctor distinguish a stopped
	// service from one still executing an inode an install replaced.
	Services map[Service]*proc.Ref `json:"services,omitempty"`
	// OperatorGates are open changes that the producer no longer owes work on.
	// They stay visible while an explicitly opted-in queue advances to another
	// brief, so the active Cycle never overwrites an operator obligation.
	OperatorGates []OperatorGate `json:"operator_gates,omitempty"`

	// schemaMigrationFrom is runtime-only. Load sets it for a state written by
	// an older binary; Save refuses to promote that document until the explicit
	// migration has proved all old state writers are stopped.
	schemaMigrationFrom int
}

// Condition is one standing health condition.
type Condition struct {
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// Ticks is consecutive ticks the condition has held.
	Ticks int `json:"ticks"`
	// LastAlerted is zero until the first alert went out.
	LastAlerted time.Time `json:"last_alerted,omitempty"`
	Summary     string    `json:"summary"`
}

// New returns an empty state document for a factory.
func New(factory string) *State {
	s := &State{
		SchemaVersion: SchemaVersion,
		Factory:       factory,
		Roles:         map[Role]*RoleState{},
		VerdictRegistry: &VerdictRegistry{
			Status: VerdictRegistryReady,
		},
		ServiceRegistry: &ServiceRegistry{
			Status: ServiceRegistryReady,
		},
	}
	for _, r := range Roles {
		s.Roles[r] = &RoleState{}
	}
	s.Cycle = &Cycle{Phase: CycleNew}
	return s
}

// Role returns the role's state, creating it if the document predates it.
func (s *State) Role(r Role) *RoleState {
	if s.Roles == nil {
		s.Roles = map[Role]*RoleState{}
	}
	if s.Roles[r] == nil {
		s.Roles[r] = &RoleState{}
	}
	return s.Roles[r]
}

// SetReviewerPipelineWait records a retryable CI wait from a verified reviewer
// merge attempt. It deliberately belongs only to the reviewer role: a
// producer cannot clear or manufacture an external merge condition.
func (s *State) SetReviewerPipelineWait(wait PipelineWait) error {
	if wait.ChangeID == "" || wait.SHA == "" || wait.Reason == "" || wait.At.IsZero() || wait.Attempts < 0 || wait.AttemptLimit <= 0 || wait.Deadline.IsZero() || !wait.Deadline.After(wait.At) {
		return errors.New("state: reviewer pipeline wait is incomplete")
	}
	r := s.Role(RoleReviewer)
	if previous := r.PipelineWait; previous == nil || previous.ChangeID != wait.ChangeID || previous.SHA != wait.SHA {
		if b := r.Blocked; b != nil && b.Disposition == PipelineWaitExhausted && b.Family == wait.ChangeID {
			r.Blocked = nil // a newly reviewed head gets a fresh bounded wait.
		}
	}
	copy := wait
	r.PipelineWait = &copy
	return nil
}

// ClearReviewerPipelineWait removes a wait only after factoryd has recorded a
// conclusive verdict for that same change. A verdict about another change is
// global activity, not authority to abandon this retry.
func (s *State) ClearReviewerPipelineWait(changeID string) {
	r := s.Role(RoleReviewer)
	if r.PipelineWait != nil && r.PipelineWait.ChangeID == changeID {
		r.PipelineWait = nil
	}
	if b := r.Blocked; b != nil && b.Disposition == PipelineWaitExhausted && b.Family == changeID {
		r.Blocked = nil
	}
}

// ExhaustReviewerPipelineWait stops automatic retries after the wait's stored
// budget has elapsed and records an operator-visible block. The caller must
// hold the state update lock.
func (s *State) ExhaustReviewerPipelineWait(at time.Time) *Block {
	r := s.Role(RoleReviewer)
	w := r.PipelineWait
	if w == nil || !w.Exhausted(at) {
		return nil
	}
	if w.ExhaustedAt == nil {
		when := at
		w.ExhaustedAt = &when
	}
	if b := r.Blocked; b != nil && b.Disposition == PipelineWaitExhausted && b.Family == w.ChangeID && b.Digest == w.SHA {
		return b
	}
	b := &Block{
		Disposition: PipelineWaitExhausted,
		Reason:      fmt.Sprintf("CI/mergeability remained unresolved for change %s at %s after %d of %d reviewer attempts or until %s; investigate the provider and reissue a conclusive reviewer verdict", w.ChangeID, w.SHA, w.Attempts, w.AttemptLimit, w.Deadline.Format(time.RFC3339)),
		Family:      w.ChangeID,
		Digest:      w.SHA,
		At:          at,
	}
	r.Blocked = b
	return b
}

// Service returns the recorded handle for service, if any.
func (s *State) Service(service Service) *proc.Ref {
	if s.Services == nil {
		return nil
	}
	return s.Services[service]
}

// ClaimService records holder as the exact process running service. Call it
// within Update: checking an old holder and replacing it must share the state
// lock, otherwise two service instances could both decide they own the slot.
func (s *State) ClaimService(service Service, holder proc.Ref) error {
	if err := s.ServiceRegistry.MigrationError(); err != nil {
		return err
	}
	if !ValidService(service) {
		return fmt.Errorf("state: unknown service %q", service)
	}
	if holder.PID <= 0 || holder.StartToken == "" {
		return fmt.Errorf("state: %s service has an incomplete process handle", service)
	}
	if existing := s.Service(service); existing != nil {
		if existing.PID == holder.PID && existing.StartToken == holder.StartToken {
			return nil
		}
		alive, err := existing.Alive()
		if err != nil {
			return fmt.Errorf("state: cannot prove recorded %s service %s is gone: %w", service, existing, err)
		}
		if alive {
			return fmt.Errorf("%w: %s is held by %s", ErrServiceAlreadyRunning, service, existing)
		}
	}
	if s.Services == nil {
		s.Services = map[Service]*proc.Ref{}
	}
	copy := holder
	s.Services[service] = &copy
	return nil
}

// ReleaseService removes service only when holder is still the exact process
// that claimed it. A stopped older service must not erase a newer instance's
// durable handle.
func (s *State) ReleaseService(service Service, holder proc.Ref) {
	if existing := s.Service(service); existing != nil && existing.PID == holder.PID && existing.StartToken == holder.StartToken {
		delete(s.Services, service)
	}
}

// RecordCycleReview stores a verified reviewer verdict on the active cycle it
// names. Verdicts for another change deliberately leave the active cycle
// alone: LastVerdict remains a global activity record, never its authority.
func (s *State) RecordCycleReview(v *Verdict) error {
	if v == nil || v.Kind == VerdictMerged {
		return nil
	}
	c := s.Cycle
	if c == nil || c.Phase != CycleOpen || c.ChangeID == "" || c.ChangeID != v.ChangeID {
		return nil
	}
	// A partially written legacy/current cycle cannot prove that this verdict
	// belongs to it. Preserve the global verdict record, but do not manufacture
	// cycle authority from incomplete or mismatched lineage.
	if v.Branch == "" || v.DeclaredBranch == "" || v.SHA == "" ||
		c.Family == "" || c.Digest == "" || c.Family != v.DeclaredBranch || c.Digest != v.Branch {
		return nil
	}
	if v.At.IsZero() {
		return nil
	}
	c.ReviewDecision = &ReviewDecision{
		Kind:           v.Kind,
		Branch:         v.Branch,
		DeclaredBranch: v.DeclaredBranch,
		SHA:            v.SHA,
		Summary:        v.Summary,
		At:             v.At,
	}
	return nil
}

// DeferOperatorGatedCycle records an open cycle as owned by the operator. The
// authority is the decision durably attached to this exact cycle, not the
// global LastVerdict, which another change can overwrite at any time.
func (s *State) DeferOperatorGatedCycle() error {
	c := s.Cycle
	if c == nil || c.Phase != CycleOpen || c.ChangeID == "" || c.ReviewDecision == nil {
		return errors.New("state: no operator-gated review decision authorizes queue continuation")
	}
	v := c.ReviewDecision
	if v.Kind != VerdictOperatorGated {
		return errors.New("state: no operator-gated review decision authorizes queue continuation")
	}
	if v.Branch == "" || v.DeclaredBranch == "" || v.SHA == "" || v.At.IsZero() {
		return errors.New("state: operator-gated review decision has incomplete lineage")
	}
	if c.Family == "" || c.Digest == "" || c.Family != v.DeclaredBranch || c.Digest != v.Branch {
		return errors.New("state: operator-gated review decision lineage does not match the open cycle")
	}
	for _, gate := range s.OperatorGates {
		if gate.ChangeID == c.ChangeID {
			return nil
		}
	}
	s.OperatorGates = append(s.OperatorGates, OperatorGate{
		ChangeID: c.ChangeID,
		Branch:   v.Branch,
		Family:   v.DeclaredBranch,
		SHA:      v.SHA,
		Summary:  v.Summary,
		GatedAt:  v.At,
	})
	return nil
}

// ClearOperatorGate removes a human obligation after factoryd has verified
// the operator merged that specific change.
func (s *State) ClearOperatorGate(changeID string) {
	for i := range s.OperatorGates {
		if s.OperatorGates[i].ChangeID == changeID {
			s.OperatorGates = append(s.OperatorGates[:i], s.OperatorGates[i+1:]...)
			return
		}
	}
}

// schemaMigrationAllowed lets the explicit migration save a state in the
// current schema after it has established that no old process can write it.
func (s *State) schemaMigrationAllowed() { s.schemaMigrationFrom = 0 }

// VerdictsReady reports whether this state can admit a producer outbox
// verdict. A nil registry is never ready; nil is only possible for a corrupt
// current-schema document because v1 is converted to an explicit blocked
// record at load.
func (s *State) VerdictsReady() bool {
	return s != nil && s.VerdictRegistry.Ready()
}

// ServicesReady reports whether service handles can be used as the complete
// inventory of long-running status and health processes.
func (s *State) ServicesReady() bool {
	return s != nil && s.ServiceRegistry.Ready()
}

// Validate checks the document's internal consistency.
func (s *State) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("state: schema_version is %d, this build understands %d", s.SchemaVersion, SchemaVersion)
	}
	if s.Factory == "" {
		return fmt.Errorf("state: factory name is empty")
	}
	if s.VerdictRegistry == nil {
		return errors.New("state: verdict registry is missing; refusing to guess whether outbox verdicts predate it")
	}
	switch s.VerdictRegistry.Status {
	case VerdictRegistryReady:
	case VerdictRegistryMigrationRequired:
		if s.VerdictRegistry.BlockedAt.IsZero() || s.VerdictRegistry.Reason == "" {
			return errors.New("state: verdict registry migration is blocked without a time or reason")
		}
	default:
		return fmt.Errorf("state: verdict registry status %q is not known", s.VerdictRegistry.Status)
	}
	if s.ServiceRegistry == nil {
		return errors.New("state: service registry is missing; refusing to guess whether pre-registry services remain alive")
	}
	switch s.ServiceRegistry.Status {
	case ServiceRegistryReady:
	case ServiceRegistryMigrationRequired:
		if s.ServiceRegistry.BlockedAt.IsZero() || s.ServiceRegistry.Reason == "" {
			return errors.New("state: service registry migration is blocked without a time or reason")
		}
	default:
		return fmt.Errorf("state: service registry status %q is not known", s.ServiceRegistry.Status)
	}
	for _, r := range Roles {
		rs := s.Roles[r]
		if rs == nil {
			continue
		}
		if rs.Halted && rs.HaltReason == "" {
			return fmt.Errorf("state: role %s is halted with no reason recorded; a silent halt is indistinguishable from an idle factory", r)
		}
		if rs.SpinCount < 0 {
			return fmt.Errorf("state: role %s has a negative spin count", r)
		}
		if rs.FailStreak < 0 {
			return fmt.Errorf("state: role %s has a negative fail streak", r)
		}
		if wait := rs.PipelineWait; wait != nil {
			if r != RoleReviewer {
				return fmt.Errorf("state: role %s carries a reviewer pipeline wait", r)
			}
			if wait.ChangeID == "" || wait.SHA == "" || wait.Reason == "" || wait.At.IsZero() || wait.Attempts < 0 || wait.AttemptLimit <= 0 || wait.Deadline.IsZero() || !wait.Deadline.After(wait.At) {
				return errors.New("state: reviewer pipeline wait is incomplete")
			}
			if wait.ExhaustedAt != nil && wait.Attempts < wait.AttemptLimit && wait.ExhaustedAt.Before(wait.Deadline) {
				return errors.New("state: reviewer pipeline wait is marked exhausted before its budget elapsed")
			}
		}
		if lease := rs.SubmissionLease; lease != nil {
			if lease.AcquiredAt.IsZero() || lease.Holder.PID <= 0 || lease.Holder.StartToken == "" {
				return fmt.Errorf("state: role %s has an incomplete producer worktree submission lease", r)
			}
		}
		for i, p := range rs.Pending {
			if p.Label == "" || p.Path == "" {
				return fmt.Errorf("state: role %s pending[%d] has no label or path", r, i)
			}
			if p.FirstSeen.IsZero() {
				return fmt.Errorf("state: role %s pending[%d] (%s) has no first-seen time; a pending trigger with no age cannot be escalated", r, i, p.Label)
			}
		}
	}
	for service, ref := range s.Services {
		if !ValidService(service) {
			return fmt.Errorf("state: unknown long-running service %q", service)
		}
		if ref == nil || ref.PID <= 0 || ref.StartToken == "" {
			return fmt.Errorf("state: service %s has an incomplete process handle", service)
		}
	}
	seenGates := map[string]bool{}
	for i, gate := range s.OperatorGates {
		if gate.ChangeID == "" || gate.Branch == "" || gate.Family == "" || gate.SHA == "" || gate.GatedAt.IsZero() {
			return fmt.Errorf("state: operator gate[%d] is incomplete", i)
		}
		if seenGates[gate.ChangeID] {
			return fmt.Errorf("state: operator gate %q is recorded more than once", gate.ChangeID)
		}
		seenGates[gate.ChangeID] = true
	}
	if c := s.Cycle; c != nil {
		switch c.Phase {
		case CycleNew, CycleWorking, CycleSubmitting, CycleOpen, CycleFinished, CycleClean, CycleUnknown:
		default:
			return fmt.Errorf("state: cycle phase %q is not one this build knows", c.Phase)
		}
		if c.Phase == CycleOpen && c.ChangeID == "" {
			return errors.New("state: cycle is open with no change id")
		}
		if d := c.ReviewDecision; d != nil {
			if c.ChangeID == "" || c.Family == "" || c.Digest == "" {
				return errors.New("state: review decision is not bound to a complete cycle")
			}
			if d.Kind != VerdictChangesRequested && d.Kind != VerdictOperatorGated {
				return fmt.Errorf("state: cycle review decision kind %q cannot authorize an open cycle", d.Kind)
			}
			if d.Branch == "" || d.DeclaredBranch == "" || d.SHA == "" || d.At.IsZero() {
				return errors.New("state: cycle review decision is incomplete")
			}
			if d.DeclaredBranch != c.Family || d.Branch != c.Digest {
				return errors.New("state: cycle review decision lineage does not match the cycle")
			}
		}
	}
	if v := s.LastVerdict; v != nil && !ValidVerdictKind(v.Kind) {
		return fmt.Errorf("state: last verdict kind %q is not one of %s, %s, %s",
			v.Kind, VerdictMerged, VerdictChangesRequested, VerdictOperatorGated)
	}
	for id, v := range s.Issued {
		if !ValidVerdictKind(v.Kind) || v.Digest == "" {
			return fmt.Errorf("state: issued verdict %q is incomplete or has an unknown kind", id)
		}
		if p := v.PendingSubmission; p != nil {
			if v.Kind != VerdictChangesRequested || v.ConsumedAt != nil || p.Turn == "" || p.Digest != v.Digest || p.At.IsZero() {
				return fmt.Errorf("state: issued verdict %q has an invalid pending submission receipt", id)
			}
		}
	}
	return nil
}

// Load reads the document at path. A missing file yields a fresh document for
// factory, because a factory that has never run has no state -- but a file that
// exists and cannot be understood is an error, never a fresh start. Silently
// replacing an unreadable state document would discard the record of whatever
// went wrong.
func Load(path, factory string) (*State, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return New(factory), nil
	}
	if err != nil {
		return nil, err
	}

	// Read the version before decoding the rest: a future schema may not be
	// decodable at all, and "cannot decode" must not be reported as "corrupt".
	var probe struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("%s: not valid JSON: %w", path, err)
	}
	legacyVerdictRegistry := false
	legacyServiceRegistry := false
	legacySchema := 0
	switch probe.SchemaVersion {
	case SchemaVersion:
	case 6, 5, 4, 3, 2:
		// Neither older schema can inventory every current long-running
		// process or preserve newer durable supervisor records.
		// Their empty Services maps prove nothing: a process started by an old
		// binary has no handle to put there. Preserve that uncertainty as a
		// durable blocked record rather than promoting it to an empty trusted
		// registry.
		legacyServiceRegistry = true
		legacySchema = probe.SchemaVersion
	case 1:
		legacyVerdictRegistry = true
		legacyServiceRegistry = true
		legacySchema = probe.SchemaVersion
	case 0:
		return nil, fmt.Errorf("%s: no schema_version; refusing to guess which schema this is", path)
	default:
		return nil, fmt.Errorf("%s: schema_version is %d, this build understands %d; refusing to operate on it",
			path, probe.SchemaVersion, SchemaVersion)
	}

	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.Factory == "" {
		s.Factory = factory
	}
	if legacySchema != 0 {
		quarantineLegacyPipelineWait(&s, time.Now().UTC())
	}
	if legacyServiceRegistry {
		s.SchemaVersion = SchemaVersion
		s.ServiceRegistry = &ServiceRegistry{
			Status:    ServiceRegistryMigrationRequired,
			Reason:    "state predates the complete long-running process and durable queue/reviewer retry registries; stop or restart every pre-upgrade factoryd process, including producer/reviewer supervisors and `factoryd status --serve`/`factoryd health --loop`, then have the operator run `factoryd migrate --config <file> service-registry`",
			BlockedAt: time.Now().UTC(),
		}
	}
	if legacyVerdictRegistry {
		// These files were placed in an outbox with no factory-owned identity
		// record. Do not hash and accept them now: the producer could have
		// planted them before this binary was installed. The next Update
		// persists this block as the current schema, and only the explicit migration
		// command may make the registry ready.
		s.SchemaVersion = SchemaVersion
		s.VerdictRegistry = &VerdictRegistry{
			Status:    VerdictRegistryMigrationRequired,
			Reason:    "state predates the verdict registry; existing outbox verdicts are untrusted until explicitly quarantined and reissued",
			BlockedAt: time.Now().UTC(),
		}
	}
	// A document written before the cycle record existed says nothing about
	// the producer's workdir. Unknown, explicitly: never "no draft".
	if s.Cycle == nil {
		s.Cycle = &Cycle{Phase: CycleUnknown, Note: "state predates the cycle record; `factoryd refresh --force` starts a new cycle"}
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, r := range Roles {
		s.Role(r)
	}
	s.schemaMigrationFrom = legacySchema
	return &s, nil
}

// quarantineLegacyPipelineWait makes a pre-v7 pipeline wait safe to carry
// across the explicit schema migration. Earlier state has no durable retry
// budget, so assigning a new one while loading would make an old wait look
// newly authorised. Preserve its change/head facts and make the required
// operator action visible instead. A pre-existing block is not overwritten:
// it is already a separate condition the operator must resolve.
func quarantineLegacyPipelineWait(s *State, at time.Time) {
	rs := s.Role(RoleReviewer)
	w := rs.PipelineWait
	if w == nil {
		return
	}
	w.Attempts = 1
	w.AttemptLimit = 1
	w.Deadline = w.At.Add(time.Second)
	w.ExhaustedAt = &at
	w.Reason = "legacy unbounded CI wait was stopped during the v7 migration; inspect CI and have the reviewer issue a conclusive verdict for this head (previous provider reason: " + w.Reason + ")"
	if rs.Blocked != nil {
		return
	}
	rs.Blocked = &Block{
		Disposition: PipelineWaitExhausted,
		Reason:      "state predates the durable CI retry budget; automatic retries were stopped until the reviewer issues a conclusive verdict",
		Family:      w.ChangeID,
		Digest:      w.SHA,
		At:          at,
	}
}

// Save writes the document atomically: a temporary file in the same directory,
// fsynced, then renamed over the target. A reader either sees the previous
// document or the new one, never a half-written one.
func (s *State) Save(path string) error {
	if s.schemaMigrationFrom != 0 {
		return fmt.Errorf("%w: state schema v%d cannot be promoted to v%d until `factoryd migrate --config <file> service-registry` completes the all-process restart sweep", ErrSchemaMigrationRequired, s.schemaMigrationFrom, SchemaVersion)
	}
	s.SchemaVersion = SchemaVersion
	s.UpdatedAt = time.Now().UTC()
	if err := s.Validate(); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(buf); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// Fsync the directory so the rename itself survives a crash.
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// Update loads, applies fn, and saves, holding an exclusive lock for the whole
// sequence. It is the only supported way to modify the document, so that every
// write goes through validation and no two writers interleave.
//
// The lock matters because the document is shared between both roles'"'"'
// supervisors. Load-then-Save without it loses whichever update landed first.
func Update(path, factory string, fn func(*State) error) (*State, error) {
	return update(path, factory, false, fn)
}

// update runs a state mutation under the document lock. A state from an older
// schema is refused before fn runs: some callers perform a guarded refresh or
// another external action inside fn, and discovering the schema barrier only
// at Save would leave that action applied without its write-ahead record.
// Only the explicit migration path is allowed to cross this boundary.
func update(path, factory string, allowSchemaMigration bool, fn func(*State) error) (*State, error) {
	unlock, err := lock(path)
	if err != nil {
		return nil, err
	}
	defer unlock()

	s, err := Load(path, factory)
	if err != nil {
		return nil, err
	}
	if s.schemaMigrationFrom != 0 && !allowSchemaMigration {
		return nil, fmt.Errorf("%w: state schema v%d cannot be changed until `factoryd migrate --config <file> service-registry` completes the all-process restart sweep", ErrSchemaMigrationRequired, s.schemaMigrationFrom)
	}
	if err := fn(s); err != nil {
		return nil, err
	}
	if err := s.Save(path); err != nil {
		return nil, err
	}
	return s, nil
}

func updateForSchemaMigration(path, factory string, fn func(*State) error) (*State, error) {
	return update(path, factory, true, fn)
}

// MigrateVerdictRegistry explicitly retires every pre-registry verdict file
// before allowing a v1 state to become registry-aware. The files are retained
// as .legacy-untrusted evidence; they are never registered from their old
// bytes. A reviewer or operator must issue a current verdict again through
// factoryd after this returns.
func MigrateVerdictRegistry(path, factory, outbox string) ([]string, error) {
	var moved []string
	_, err := updateForSchemaMigration(path, factory, func(s *State) error {
		if s.schemaMigrationFrom != 0 {
			if err := requireAllFactorydProcessesStopped(s); err != nil {
				return err
			}
			s.schemaMigrationAllowed()
		}
		if s.VerdictRegistry.Ready() {
			return nil
		}
		if err := s.VerdictRegistry.MigrationError(); err != nil {
			if !errors.Is(err, ErrVerdictRegistryMigrationRequired) {
				return err
			}
		}
		entries, err := os.ReadDir(outbox)
		if err != nil {
			return fmt.Errorf("reading legacy outbox %s: %w", outbox, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			src := filepath.Join(outbox, entry.Name())
			if _, err := os.Lstat(src); os.IsNotExist(err) {
				continue // the producer removed it while the explicit migration ran
			} else if err != nil {
				return fmt.Errorf("examining legacy verdict %s: %w", src, err)
			}
			dst, err := legacyVerdictPath(src)
			if err != nil {
				return err
			}
			if err := os.Rename(src, dst); err != nil {
				return fmt.Errorf("quarantining legacy verdict %s: %w", src, err)
			}
			moved = append(moved, dst)
		}
		if s.Issued == nil {
			s.Issued = map[string]IssuedVerdict{}
		}
		s.VerdictRegistry = &VerdictRegistry{Status: VerdictRegistryReady}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return moved, nil
}

// MigrateServiceRegistry records the operator's explicit attestation that
// every pre-registry long-running factoryd process was stopped or restarted.
// Old status and health services have no durable handles, so that part cannot
// be inferred from an empty map. Producer and reviewer supervisors do have
// handles, and must be proven dead before this writes the schema that an old
// supervisor cannot load. After the attestation only services started by this
// build may claim a fresh exact handle.
func MigrateServiceRegistry(path, factory string) error {
	_, err := updateForSchemaMigration(path, factory, func(s *State) error {
		if s.ServiceRegistry.Ready() && s.schemaMigrationFrom == 0 {
			return nil
		}
		if err := s.ServiceRegistry.MigrationError(); err != nil {
			if !errors.Is(err, ErrServiceRegistryMigrationRequired) {
				return err
			}
		}
		requireStopped := func(label string, ref *proc.Ref) error {
			if ref == nil {
				return nil
			}
			alive, err := ref.Alive()
			if err != nil {
				return fmt.Errorf("cannot prove recorded %s %s is gone: %w", label, ref, err)
			}
			if alive {
				return fmt.Errorf("recorded %s %s is still live; stop it before attesting the all-process restart sweep", label, ref)
			}
			return nil
		}
		if err := requireAllFactorydProcessesStoppedWith(s, requireStopped); err != nil {
			return err
		}
		// Any remaining handles name dead processes. The post-attestation
		// inventory begins empty and is populated only by fresh claims.
		s.Services = nil
		s.ServiceRegistry = &ServiceRegistry{
			Status:      ServiceRegistryReady,
			AttestedAt:  time.Now().UTC(),
			Attestation: "operator attested that every pre-registry factoryd process, including supervisors and status/health services, was stopped or restarted",
		}
		s.schemaMigrationAllowed()
		return nil
	})
	return err
}

// requireAllFactorydProcessesStopped is the schema-upgrade barrier. A
// previous binary can write state too, so a newer schema is safe only after
// every exact process handle is dead; otherwise its next update would stall
// against an unreadable document.
func requireAllFactorydProcessesStopped(s *State) error {
	return requireAllFactorydProcessesStoppedWith(s, func(label string, ref *proc.Ref) error {
		if ref == nil {
			return nil
		}
		alive, err := ref.Alive()
		if err != nil {
			return fmt.Errorf("cannot prove recorded %s %s is gone: %w", label, ref, err)
		}
		if alive {
			return fmt.Errorf("recorded %s %s is still live; stop it before the all-process restart sweep", label, ref)
		}
		return nil
	})
}

func requireAllFactorydProcessesStoppedWith(s *State, requireStopped func(string, *proc.Ref) error) error {
	for _, role := range Roles {
		if err := requireStopped(string(role)+" supervisor", s.Role(role).Supervisor); err != nil {
			return err
		}
	}
	for _, service := range Services {
		if err := requireStopped(string(service)+" service", s.Service(service)); err != nil {
			return err
		}
	}
	return nil
}

func legacyVerdictPath(src string) (string, error) {
	base := src + ".legacy-untrusted"
	for n := 0; ; n++ {
		candidate := base
		if n > 0 {
			candidate = fmt.Sprintf("%s.%d", base, n)
		}
		_, err := os.Lstat(candidate)
		if os.IsNotExist(err) {
			return candidate, nil
		}
		if err != nil {
			return "", fmt.Errorf("examining legacy verdict quarantine path %s: %w", candidate, err)
		}
	}
}

// LockPath is the advisory lock guarding the state document.
func LockPath(statePath string) string { return statePath + ".lock" }
