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
	// Errors are things the page could not read. They are shown, not
	// hidden: a page that could not read the state document must not look
	// like a page for an idle factory.
	Errors []string `json:"errors,omitempty"`
}

// RoleView is one role's supervisor and turn.
type RoleView struct {
	Supervisor *proc.Ref  `json:"supervisor,omitempty"`
	Alive      *bool      `json:"alive,omitempty"` // nil when there is no supervisor to ask about
	Tree       *proc.Node `json:"process_tree,omitempty"`
	WatchMode  string     `json:"watch_mode,omitempty"`
	Halted     bool       `json:"halted"`
	HaltReason string     `json:"halt_reason,omitempty"`

	Turn    *TurnView     `json:"turn,omitempty"`
	Pending []PendingView `json:"pending,omitempty"`
	Spin    int           `json:"spin_count"`
	Fails   int           `json:"fail_streak"`
	// Stopped is the halt sentinel on disk, which outlives the process.
	Stopped bool `json:"stop_sentinel"`
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

// HealthView is the health document as the page sees it.
type HealthView struct {
	Present  bool                 `json:"present"`
	Stale    bool                 `json:"stale"` // older than two health intervals
	AgeText  string               `json:"age_text,omitempty"`
	Healthy  bool                 `json:"healthy"`
	Findings []health.Finding     `json:"findings,omitempty"`
	Volumes  []health.Volume      `json:"volumes,omitempty"`
	Caches   []health.CacheReport `json:"caches,omitempty"`
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

	for _, r := range state.Roles {
		rs := st.Role(r)
		role := string(r)
		v := RoleView{Supervisor: rs.Supervisor, WatchMode: rs.WatchMode, Halted: rs.Halted, HaltReason: rs.HaltReason, Spin: rs.SpinCount, Fails: rs.FailStreak}
		if rs.Supervisor != nil {
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
						v.Tree = t
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
	s.Changes = c.readChanges(ctx, now)
	s.NeedsMe = needsMe(c.cfg, s, st)
	s.Working = working(s)
	return s
}

func turnView(t *state.Turn, now time.Time) *TurnView {
	age := t.Age(now)
	return &TurnView{ID: t.ID, Trigger: t.Trigger, Running: t.Running(), Age: age, AgeText: ageText(age), Exit: t.ExitCode}
}

func (c *Collector) readHealth(now time.Time) HealthView {
	body, err := os.ReadFile(c.cfg.HealthPath())
	if err != nil {
		return HealthView{}
	}
	var rep health.Report
	if err := json.Unmarshal(body, &rep); err != nil {
		return HealthView{}
	}
	age := now.Sub(rep.At)
	return HealthView{Present: true, Stale: age > 2*time.Duration(c.cfg.Health.IntervalSeconds)*time.Second, AgeText: ageText(age),
		Healthy: rep.Healthy, Findings: rep.Findings, Volumes: rep.Volumes, Caches: rep.Caches}
}

func (c *Collector) readChanges(ctx context.Context, now time.Time) ChangesView {
	if c.deps.ListOpen == nil {
		return ChangesView{Skipped: true}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.changes.AsOf.IsZero() && now.Sub(c.changes.AsOf) < c.deps.ChangesTTL {
		return c.changes
	}
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
	for _, e := range s.Errors {
		out = append(out, "status could not read: "+e)
	}
	for _, r := range state.Roles {
		v := s.Roles[string(r)]
		switch {
		case v.Halted:
			out = append(out, fmt.Sprintf("%s supervisor halted: %s -- clear the stop sentinel and restart", r, v.HaltReason))
		case v.Stopped:
			out = append(out, fmt.Sprintf("%s has a stop sentinel on disk; it will not start until it is removed", r))
		case v.Supervisor == nil:
			out = append(out, fmt.Sprintf("%s has never been supervised", r))
		case v.Alive != nil && !*v.Alive:
			out = append(out, fmt.Sprintf("%s supervisor %s is dead and did not halt", r, v.Supervisor))
		}
	}
	if s.Verdict != nil && s.Verdict.Kind == state.VerdictOperatorGated {
		out = append(out, fmt.Sprintf("change %s is operator-gated: %s", s.Verdict.ChangeID, s.Verdict.Summary))
	}
	if _, err := os.Lstat(filepath.Join(cfg.InboxDir(), "question.md")); err == nil {
		out = append(out, "a question is waiting in the inbox")
	}
	switch {
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

func working(s Snapshot) bool {
	if len(s.Errors) > 0 || !s.Health.Present || s.Health.Stale || !s.Health.Healthy {
		return false
	}
	for _, r := range state.Roles {
		v := s.Roles[string(r)]
		if v.Halted || v.Stopped || v.Supervisor == nil || v.Alive == nil || !*v.Alive {
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
