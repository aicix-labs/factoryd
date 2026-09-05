// Package status answers, in this order: is it working? what is it doing
// right now? what is it waiting on? what needs me? (SPEC.md §8). It is
// read-only: it opens files to read them, asks the provider to list, and
// never starts, stops, or consumes anything. Everything the factory does is
// observable, but only if you know the six places to look; this is those
// six places.
package status

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/health"
	"github.com/aicix-labs/factoryd/internal/proc"
	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/state"
	"github.com/aicix-labs/factoryd/internal/supervise"
)

// Snapshot is everything the page shows for one factory, at one instant.
type Snapshot struct {
	Factory  string    `json:"factory"`
	Provider string    `json:"provider"`
	Target   string    `json:"target_branch"`
	At       time.Time `json:"at"`

	// Working is the one-line answer: every supervisor alive and not
	// halted, health present, fresh and clean.
	Working bool `json:"working"`
	// NeedsMe is what an operator has to act on, most urgent first.
	NeedsMe []string `json:"needs_me"`

	Roles   map[string]RoleView `json:"roles"`
	Health  HealthView          `json:"health"`
	Changes ChangesView         `json:"changes"`
	Verdict *state.Verdict      `json:"last_verdict,omitempty"`
	// VerdictRegistry is explicit during an upgrade: a producer must not
	// look idle when legacy outbox verdicts are blocked pending migration.
	VerdictRegistry *state.VerdictRegistry `json:"verdict_registry,omitempty"`
	// OperatorGates are open changes awaiting a human. They are separate from
	// the active cycle when the operator has allowed the brief queue to keep
	// moving, so an operator-owned wait never looks like producer rework.
	OperatorGates []state.OperatorGate `json:"operator_gates,omitempty"`
	// PipelineWait is a reviewer merge that GitLab explicitly deferred for CI.
	// It is visible separately from NeedsMe because factoryd, not an operator,
	// will re-attempt it.
	PipelineWait *state.PipelineWait `json:"pipeline_wait,omitempty"`
	// Errors are things the page could not read. They are shown, not
	// hidden: a page that could not read the state document must not look
	// like a page for an idle factory.
	Errors []string `json:"errors,omitempty"`
}

// SupervisorView is the supervisor handle as the page shows it: the pid and
// when it started. Not proc.Ref itself, whose descriptive Command field is
// the supervisor's own argv -- and the page is unauthenticated.
type SupervisorView struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

// RoleView is one role's supervisor and turn.
type RoleView struct {
	Supervisor *SupervisorView `json:"supervisor,omitempty"`
	Alive      *bool           `json:"alive,omitempty"` // nil when there is no supervisor to ask about
	Tree       *TreeNode       `json:"process_tree,omitempty"`
	WatchMode  string          `json:"watch_mode,omitempty"`
	Halted     bool            `json:"halted"`
	HaltReason string          `json:"halt_reason,omitempty"`
	// LastHalt is a cleared halt: information, not a need.
	LastHalt *state.Halt `json:"last_halt,omitempty"`
	// Blocked is a submission that will not be retried: a need (#42).
	Blocked *state.Block `json:"blocked,omitempty"`

	Turn    *TurnView     `json:"turn,omitempty"`
	Pending []PendingView `json:"pending,omitempty"`
	Spin    int           `json:"spin_count"`
	Fails   int           `json:"fail_streak"`
	// LeftoverTurns is hygiene: turns that did their work but left
	// processes behind, which were killed (#33).
	LeftoverTurns int `json:"leftover_turns"`
	// Stopped is the halt sentinel on disk, which outlives the process.
	Stopped bool `json:"stop_sentinel"`
}

// TreeNode is the process tree as the page shows it: pid, structure, and a
// label factoryd itself assigned from what IT recorded -- "supervisor" for
// the pid in the state document, "turn" for the pid the supervisor recorded
// for the running turn, "child" for anything else. Never a label the
// process supplied: argv, comm and the exe path are all process-controlled
// channels, and the page is unauthenticated.
type TreeNode struct {
	PID      int         `json:"pid"`
	Label    string      `json:"label"`
	Children []*TreeNode `json:"children,omitempty"`
}

// TurnView is the running turn, or the last one.
type TurnView struct {
	ID      string        `json:"id"`
	Trigger string        `json:"trigger,omitempty"`
	Running bool          `json:"running"`
	Age     time.Duration `json:"age"`
	AgeText string        `json:"age_text"`
	Exit    *int          `json:"exit_code,omitempty"`
}

// PendingView is a trigger with its age.
type PendingView struct {
	Label   string        `json:"label"`
	Age     time.Duration `json:"age"`
	AgeText string        `json:"age_text"`
}

// HealthView is the health document as the page sees it. Absent and
// unreadable are different answers: "the tick never ran" and "there is a
// record I cannot read" call for different actions, and the second is
// reported as an error the page shows.
type HealthView struct {
	Present    bool                     `json:"present"`
	Err        string                   `json:"error,omitempty"`
	Stale      bool                     `json:"stale"` // older than two health intervals
	AgeText    string                   `json:"age_text,omitempty"`
	Healthy    bool                     `json:"healthy"`
	Findings   []health.Finding         `json:"findings,omitempty"`
	Volumes    []health.Volume          `json:"volumes,omitempty"`
	Caches     []health.CacheReport     `json:"caches,omitempty"`
	BriefQueue *health.BriefQueueReport `json:"brief_queue,omitempty"`
}

// ChangesView is the provider's open changes, or why they are unknown.
type ChangesView struct {
	Open    []scm.Change `json:"open,omitempty"`
	AsOf    time.Time    `json:"as_of,omitempty"`
	Err     string       `json:"error,omitempty"`
	Skipped bool         `json:"skipped"` // no provider access configured for status
}

// Deps is what a collector reads through.
type Deps struct {
	// ListOpen is nil when status runs without provider access.
	ListOpen func(ctx context.Context) ([]scm.Change, error)
	Alive    func(ref proc.Ref) (bool, error)
	Tree     func(pid int) (*proc.Node, error)
	Now      func() time.Time
	// ChangesTTL bounds how often the provider is asked; a page reload is
	// not a reason for an API call.
	ChangesTTL time.Duration
}

// Collector reads one factory. It caches only the provider answer.
type Collector struct {
	cfg  *config.Config
	deps Deps

	mu      sync.Mutex
	changes ChangesView
	// attempted is when the provider was last ASKED, whatever it answered.
	// The throttle keys on it, not on the last good list's time: after a
	// failure that time is already older than the TTL, and a throttle keyed
	// on it would ask again on every reload -- precisely while the
	// provider is down.
	attempted time.Time
}

// New returns a collector for cfg.
func New(cfg *config.Config, deps Deps) *Collector {
	if deps.Alive == nil {
		deps.Alive = func(r proc.Ref) (bool, error) { return r.Alive() }
	}
	if deps.Tree == nil {
		deps.Tree = proc.Tree
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.ChangesTTL == 0 {
		deps.ChangesTTL = time.Duration(cfg.Health.IntervalSeconds) * time.Second
	}
	return &Collector{cfg: cfg, deps: deps}
}

// Collect reads everything now.
func (c *Collector) Collect(ctx context.Context) Snapshot {
	now := c.deps.Now()
	s := Snapshot{Factory: c.cfg.Name, Provider: c.cfg.Provider, Target: c.cfg.TargetBranch, At: now, Roles: map[string]RoleView{}}

	st, err := state.Load(c.cfg.StatePath(), c.cfg.Name)
	if err != nil {
		s.Errors = append(s.Errors, "state: "+err.Error())
		st = state.New(c.cfg.Name)
	}
	s.Verdict = st.LastVerdict
	s.VerdictRegistry = st.VerdictRegistry
	s.OperatorGates = append(s.OperatorGates, st.OperatorGates...)
	if wait := st.Role(state.RoleReviewer).PipelineWait; wait != nil {
		copy := *wait
		s.PipelineWait = &copy
	}

	for _, r := range state.Roles {
		rs := st.Role(r)
		role := string(r)
		v := RoleView{WatchMode: rs.WatchMode, Halted: rs.Halted, HaltReason: rs.HaltReason, LastHalt: rs.LastHalt, Blocked: rs.Blocked, Spin: rs.SpinCount, Fails: rs.FailStreak, LeftoverTurns: rs.LeftoverTurns}
		if rs.Supervisor != nil {
			v.Supervisor = &SupervisorView{PID: rs.Supervisor.PID, StartedAt: rs.Supervisor.StartedAt}
			alive, err := c.deps.Alive(*rs.Supervisor)
			if err != nil {
				s.Errors = append(s.Errors, role+" liveness: "+err.Error())
			} else {
				v.Alive = &alive
				if alive {
					// A tree that cannot be read is said, not skipped: the
					// page would otherwise show a bare supervisor for a
					// factory whose turns it simply cannot see.
					if t, err := c.deps.Tree(rs.Supervisor.PID); err != nil {
						s.Errors = append(s.Errors, role+" process tree: "+err.Error())
					} else {
						turnPID := 0
						if rs.CurrentTurn != nil && rs.CurrentTurn.Process != nil {
							turnPID = rs.CurrentTurn.Process.PID
						}
						v.Tree = label(t, rs.Supervisor.PID, turnPID)
					}
				}
			}
		}
		if t := rs.CurrentTurn; t != nil {
			v.Turn = turnView(t, now)
		} else if t := rs.LastTurn; t != nil {
			v.Turn = turnView(t, now)
		}
		for _, p := range rs.Pending {
			age := now.Sub(p.FirstSeen)
			v.Pending = append(v.Pending, PendingView{Label: p.Label, Age: age, AgeText: ageText(age)})
		}
		if _, err := os.Lstat(c.cfg.StopPath(role)); err == nil {
			v.Stopped = true
		}
		s.Roles[role] = v
	}

	s.Health = c.readHealth(now)
	if s.Health.Err != "" {
		s.Errors = append(s.Errors, "health document: "+s.Health.Err)
	}
	s.Changes = c.readChanges(ctx, now)
	s.NeedsMe = needsMe(c.cfg, s, st)
	s.Working = working(s)
	return s
}

func label(n *proc.Node, supervisor, turn int) *TreeNode {
	if n == nil {
		return nil
	}
	out := &TreeNode{PID: n.PID, Label: "child"}
	switch n.PID {
	case supervisor:
		out.Label = "supervisor"
	case turn:
		out.Label = "turn"
	}
	for _, c := range n.Children {
		out.Children = append(out.Children, label(c, supervisor, turn))
	}
	return out
}

func turnView(t *state.Turn, now time.Time) *TurnView {
	age := t.Age(now)
	return &TurnView{ID: t.ID, Trigger: t.Trigger, Running: t.Running(), Age: age, AgeText: ageText(age), Exit: t.ExitCode}
}

func (c *Collector) readHealth(now time.Time) HealthView {
	body, err := os.ReadFile(c.cfg.HealthPath())
	if errors.Is(err, os.ErrNotExist) {
		return HealthView{}
	}
	if err != nil {
		return HealthView{Present: true, Err: err.Error()}
	}
	var rep health.Report
	if err := json.Unmarshal(body, &rep); err != nil {
		return HealthView{Present: true, Err: "not valid JSON: " + err.Error()}
	}
	age := now.Sub(rep.At)
	return HealthView{Present: true, Stale: age > 2*time.Duration(c.cfg.Health.IntervalSeconds)*time.Second, AgeText: ageText(age),
		Healthy: rep.Healthy, Findings: rep.Findings, Volumes: rep.Volumes, Caches: rep.Caches, BriefQueue: rep.BriefQueue}
}

func (c *Collector) readChanges(ctx context.Context, now time.Time) ChangesView {
	if c.deps.ListOpen == nil {
		return ChangesView{Skipped: true}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.attempted.IsZero() && now.Sub(c.attempted) < c.deps.ChangesTTL {
		return c.changes
	}
	c.attempted = now
	open, err := c.deps.ListOpen(ctx)
	v := ChangesView{AsOf: now}
	if err != nil {
		v.Err = err.Error()
		// A failed refresh keeps the last good list visible, marked by its
		// own time, alongside the error. Blanking it would make "the
		// provider is down" look like "nothing is open".
		v.Open, v.AsOf = c.changes.Open, c.changes.AsOf
		if v.AsOf.IsZero() {
			v.AsOf = now
		}
	} else {
		sort.Slice(open, func(i, j int) bool { return open[i].UpdatedAt.After(open[j].UpdatedAt) })
		v.Open = open
	}
	c.changes = v
	return v
}

// needsMe lists what an operator must act on. Each line is something no
// agent turn will resolve by itself.
func needsMe(cfg *config.Config, s Snapshot, st *state.State) []string {
	var out []string
	if err := st.VerdictRegistry.MigrationError(); err != nil {
		out = append(out, fmt.Sprintf("%v -- run `factoryd migrate --config %s verdict-registry`; then have the reviewer or operator reissue any still-current verdict", err, cfg.Path()))
	}
	for _, e := range s.Errors {
		out = append(out, "status could not read: "+e)
	}
	for _, r := range state.Roles {
		v := s.Roles[string(r)]
		if b := v.Blocked; b != nil {
			if b.Disposition == state.PipelineWaitExhausted {
				out = append(out, fmt.Sprintf("%s CI wait exhausted and is not retried: %s -- investigate the provider, then have the reviewer issue a conclusive verdict for that head", r, b.Reason))
			} else {
				out = append(out, fmt.Sprintf("%s submission %s and not retried: %s -- fix the cause and run factoryd submit; a successful submission clears this", r, b.Disposition, b.Reason))
			}
		}
		switch {
		case v.Halted:
			// The remedy is phrased from the sentinel's CURRENT existence, not
			// from the recorded halt (#26): an operator told to clear a
			// sentinel that is already gone finds nothing and distrusts the
			// page. The fact (the halt, its reason) is the record's; the
			// instruction is the disk's.
			if v.Stopped {
				out = append(out, fmt.Sprintf("%s supervisor halted: %s -- remove %s and restart", r, v.HaltReason, cfg.StopPath(string(r))))
			} else {
				out = append(out, fmt.Sprintf("%s supervisor halted: %s -- sentinel already cleared; restart to resume", r, v.HaltReason))
			}
		case v.Stopped:
			out = append(out, fmt.Sprintf("%s has a stop sentinel on disk; it will not start until it is removed", r))
		case v.Supervisor == nil:
			out = append(out, fmt.Sprintf("%s has never been supervised", r))
		case v.Alive != nil && !*v.Alive:
			out = append(out, fmt.Sprintf("%s supervisor pid %d is dead and did not halt", r, v.Supervisor.PID))
		}
	}
	for _, gate := range s.OperatorGates {
		summary := gate.Summary
		if summary == "" {
			summary = "reviewer declared the producer finished"
		}
		out = append(out, fmt.Sprintf("operator-gated change %s awaits you: %s -- the brief queue is continuing by operator policy; merge or resolve it independently", gate.ChangeID, summary))
	}
	activeOperatorGate := false
	if c := st.Cycle; c != nil && c.Phase == state.CycleOpen && c.ChangeID != "" && c.ReviewDecision != nil &&
		c.ReviewDecision.Kind == state.VerdictOperatorGated && !hasOperatorGate(s.OperatorGates, c.ChangeID) {
		out = append(out, fmt.Sprintf("change %s is operator-gated: %s", c.ChangeID, c.ReviewDecision.Summary))
		activeOperatorGate = true
	}
	if !activeOperatorGate && s.Verdict != nil && s.Verdict.Kind == state.VerdictOperatorGated && !hasOperatorGate(s.OperatorGates, s.Verdict.ChangeID) {
		out = append(out, fmt.Sprintf("change %s is operator-gated: %s", s.Verdict.ChangeID, s.Verdict.Summary))
	}
	if _, err := os.Lstat(filepath.Join(cfg.InboxDir(), "question.md")); err == nil {
		out = append(out, "a question is waiting in the inbox")
	}
	switch {
	case s.Health.Err != "":
		// already listed under "status could not read"
	case !s.Health.Present:
		out = append(out, "no health document; the health tick has never run here")
	case s.Health.Stale:
		out = append(out, "the health document is "+s.Health.AgeText+" old; the health tick has stopped")
	case !s.Health.Healthy:
		for _, f := range s.Health.Findings {
			out = append(out, "health: "+f.Summary)
		}
	}
	if s.Changes.Err != "" {
		out = append(out, "open changes are unknown: "+s.Changes.Err)
	}
	return out
}

func hasOperatorGate(gates []state.OperatorGate, changeID string) bool {
	for _, gate := range gates {
		if gate.ChangeID == changeID {
			return true
		}
	}
	return false
}

func working(s Snapshot) bool {
	if len(s.Errors) > 0 || !s.Health.Present || s.Health.Err != "" || s.Health.Stale || !s.Health.Healthy {
		return false
	}
	if s.VerdictRegistry == nil || !s.VerdictRegistry.Ready() {
		return false
	}
	for _, r := range state.Roles {
		v := s.Roles[string(r)]
		if v.Halted || v.Stopped || v.Blocked != nil || v.Supervisor == nil || v.Alive == nil || !*v.Alive {
			return false
		}
	}
	return true
}

func ageText(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// ErrNoFactories is returned by a server built with nothing to show.
var ErrNoFactories = errors.New("status: no factories")

// TriggerLabels is exported for the page's "waiting on" legend.
func TriggerLabels(cfg *config.Config, role string) []string {
	specs, err := supervise.TriggersFor(cfg, role)
	if err != nil {
		return nil
	}
	var out []string
	for _, sp := range specs {
		out = append(out, sp.Label)
	}
	return out
}
