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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/aicix-labs/factoryd/internal/proc"
)

// SchemaVersion is the state schema this build understands.
const SchemaVersion = 1

// Role is one of the two agent roles.
type Role string

const (
	RoleProducer Role = "producer"
	RoleReviewer Role = "reviewer"
)

// Roles is every role, in a stable order.
var Roles = []Role{RoleProducer, RoleReviewer}

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
}

// New returns an empty state document for a factory.
func New(factory string) *State {
	s := &State{
		SchemaVersion: SchemaVersion,
		Factory:       factory,
		Roles:         map[Role]*RoleState{},
	}
	for _, r := range Roles {
		s.Roles[r] = &RoleState{}
	}
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

// Validate checks the document's internal consistency.
func (s *State) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("state: schema_version is %d, this build understands %d", s.SchemaVersion, SchemaVersion)
	}
	if s.Factory == "" {
		return fmt.Errorf("state: factory name is empty")
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
		for i, p := range rs.Pending {
			if p.Label == "" || p.Path == "" {
				return fmt.Errorf("state: role %s pending[%d] has no label or path", r, i)
			}
			if p.FirstSeen.IsZero() {
				return fmt.Errorf("state: role %s pending[%d] (%s) has no first-seen time; a pending trigger with no age cannot be escalated", r, i, p.Label)
			}
		}
	}
	if v := s.LastVerdict; v != nil && !ValidVerdictKind(v.Kind) {
		return fmt.Errorf("state: last verdict kind %q is not one of %s, %s, %s",
			v.Kind, VerdictMerged, VerdictChangesRequested, VerdictOperatorGated)
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
	switch probe.SchemaVersion {
	case SchemaVersion:
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
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, r := range Roles {
		s.Role(r)
	}
	return &s, nil
}

// Save writes the document atomically: a temporary file in the same directory,
// fsynced, then renamed over the target. A reader either sees the previous
// document or the new one, never a half-written one.
func (s *State) Save(path string) error {
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
	unlock, err := lock(path)
	if err != nil {
		return nil, err
	}
	defer unlock()

	s, err := Load(path, factory)
	if err != nil {
		return nil, err
	}
	if err := fn(s); err != nil {
		return nil, err
	}
	if err := s.Save(path); err != nil {
		return nil, err
	}
	return s, nil
}

// LockPath is the advisory lock guarding the state document.
func LockPath(statePath string) string { return statePath + ".lock" }
