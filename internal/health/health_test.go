package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aicix-labs/factoryd/internal/alert"
	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/health"
	"github.com/aicix-labs/factoryd/internal/proc"
	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/state"
)

type probes struct {
	alive    map[int]bool
	aliveErr error
	free     map[string]float64 // free percent by path; default 50
	dev      map[string]uint64  // device by path; default 1
	statErr  error
}

func (p *probes) Alive(rs state.RoleState) (bool, error) {
	if p.aliveErr != nil {
		return false, p.aliveErr
	}
	return p.alive[rs.Supervisor.PID], nil
}

func (p *probes) Statfs(path string) (health.Volume, error) {
	if p.statErr != nil {
		return health.Volume{}, p.statErr
	}
	free, ok := p.free[path]
	if !ok {
		free = 50
	}
	return health.Volume{Path: path, TotalBytes: 1000, FreeBytes: uint64(free * 10), FreePercent: free}, nil
}

func (p *probes) DeviceID(path string) (uint64, error) {
	if d, ok := p.dev[path]; ok {
		return d, nil
	}
	return 1, nil
}

type lab struct {
	cfg    *config.Config
	root   string
	p      *probes
	deps   health.Deps
	now    time.Time
	alerts string
	open   []scm.Change
}

func newLab(t *testing.T) *lab {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"work", "submit", "inbox", "outbox"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	l := &lab{root: root, now: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC), alerts: filepath.Join(root, "alerts.log")}
	l.cfg = &config.Config{
		SchemaVersion: config.SchemaVersion, Name: "widgets", Provider: "github",
		GitHub: &config.GitHub{Owner: "acme", Repo: "widgets"}, TargetBranch: "main",
		Paths:  config.Paths{Root: root, ProducerWorkdir: filepath.Join(root, "work"), SubmitRepo: filepath.Join(root, "submit")},
		Roles:  config.Roles{Producer: config.RoleSpec{TimeoutSeconds: 600}, Reviewer: config.RoleSpec{TimeoutSeconds: 600}},
		Alerts: []config.Alert{{Kind: "file", Path: l.alerts}},
		Health: config.Health{IntervalSeconds: 60, AlertAfter: 2, RepeatSeconds: 600, StaleTriggerSeconds: 300, TurnGraceSeconds: 60, DiskMinFreePercent: 10},
	}
	l.p = &probes{alive: map[int]bool{}, free: map[string]float64{}, dev: map[string]uint64{}}
	fan, err := alert.New(l.cfg)
	if err != nil {
		t.Fatal(err)
	}
	l.deps = health.Deps{Probes: l.p, Alerts: fan, Now: func() time.Time { return l.now },
		ListOpen: func(context.Context) ([]scm.Change, error) { return l.open, nil }}
	// A state document exists: a supervisor registered and is alive.
	l.withState(t, func(s *state.State) {
		s.Role(state.RoleProducer).Supervisor = &proc.Ref{PID: 100, StartToken: "t"}
		s.Role(state.RoleReviewer).Supervisor = &proc.Ref{PID: 200, StartToken: "t"}
	})
	l.p.alive[100], l.p.alive[200] = true, true
	return l
}

func (l *lab) withState(t *testing.T, fn func(*state.State)) {
	t.Helper()
	if _, err := state.Update(l.cfg.StatePath(), l.cfg.Name, func(s *state.State) error { fn(s); return nil }); err != nil {
		t.Fatal(err)
	}
}

func (l *lab) tick(t *testing.T) health.Report {
	t.Helper()
	rep, err := health.Tick(context.Background(), l.cfg, l.deps)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func keys(rep health.Report) []string {
	var out []string
	for _, f := range rep.Findings {
		out = append(out, f.Key)
	}
	return out
}

func hasKey(rep health.Report, prefix string) bool {
	for _, f := range rep.Findings {
		if strings.HasPrefix(f.Key, prefix) {
			return true
		}
	}
	return false
}

func (l *lab) alertLines(t *testing.T) []alert.Alert {
	t.Helper()
	body, err := os.ReadFile(l.alerts)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []alert.Alert
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if line == "" {
			continue
		}
		var a alert.Alert
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			t.Fatalf("alert line is not JSON: %v: %s", err, line)
		}
		out = append(out, a)
	}
	return out
}

// Positive control: a factory with nothing wrong is healthy, writes the
// document, and alerts nobody. Every negative case below rests on this.
func TestHealthyFactoryIsHealthy(t *testing.T) {
	l := newLab(t)
	rep := l.tick(t)
	if !rep.Healthy || len(rep.Findings) != 0 {
		t.Fatalf("findings=%v errors=%v", keys(rep), rep.Errors)
	}
	body, err := os.ReadFile(l.cfg.HealthPath())
	if err != nil {
		t.Fatalf("no health document: %v", err)
	}
	var got health.Report
	if err := json.Unmarshal(body, &got); err != nil || !got.Healthy || got.Factory != "widgets" {
		t.Fatalf("document: %v %s", err, body)
	}
	if len(rep.Volumes) == 0 {
		t.Fatal("no volume was observed; a disk check that looked at nothing cannot fail")
	}
	if l.alertLines(t) != nil {
		t.Fatal("a healthy factory alerted")
	}
}

// A factory nobody has ever supervised is a finding on every tick, not only
// the first: the tick's own cadence record creates state.json, so a finding
// keyed on the file's absence would be erased by the tick that reported it.
func TestNeverSupervisedIsAFindingOnEveryTick(t *testing.T) {
	l := newLab(t)
	os.Remove(l.cfg.StatePath())
	for i := 0; i < 3; i++ {
		rep := l.tick(t)
		if rep.Healthy || !hasKey(rep, "never_supervised") {
			t.Fatalf("tick %d: findings=%v", i+1, keys(rep))
		}
		l.now = l.now.Add(time.Minute)
	}
	if _, err := os.Stat(l.cfg.StatePath()); err != nil {
		t.Fatal("the cadence record was not written; the test did not exercise the self-erasure")
	}
}

func TestDeadSupervisorIsDetected(t *testing.T) {
	l := newLab(t)
	l.p.alive[100] = false
	rep := l.tick(t)
	if !hasKey(rep, "supervisor_dead/producer") || hasKey(rep, "supervisor_dead/reviewer") {
		t.Fatalf("findings=%v", keys(rep))
	}
}

// A supervisor that halted on purpose is not dead; it is halted, and that is
// its own standing condition until an operator clears it.
func TestHaltedIsItsOwnCondition(t *testing.T) {
	l := newLab(t)
	l.p.alive[100] = false
	l.withState(t, func(s *state.State) {
		r := s.Role(state.RoleProducer)
		r.Halted, r.HaltReason, r.HaltedAt = true, "spin abort", l.now
	})
	rep := l.tick(t)
	if !hasKey(rep, "halted/producer") || hasKey(rep, "supervisor_dead/producer") {
		t.Fatalf("findings=%v", keys(rep))
	}
}

// Liveness that cannot be determined is not liveness.
func TestUnknownLivenessIsAFinding(t *testing.T) {
	l := newLab(t)
	l.p.aliveErr = errors.New("/proc unreadable")
	rep := l.tick(t)
	if !hasKey(rep, "supervisor_unknown/") || rep.Healthy {
		t.Fatalf("findings=%v", keys(rep))
	}
}

func TestTurnPastTimeoutPlusGraceIsDetected(t *testing.T) {
	l := newLab(t)
	start := l.now.Add(-(600 + 60 + 1) * time.Second)
	l.withState(t, func(s *state.State) {
		s.Role(state.RoleReviewer).CurrentTurn = &state.Turn{ID: "t1", StartedAt: start}
	})
	if rep := l.tick(t); !hasKey(rep, "turn_overlong/reviewer") {
		t.Fatalf("findings=%v", keys(rep))
	}
	// Control: inside the grace it is a long turn, not a stuck supervisor.
	l.withState(t, func(s *state.State) {
		s.Role(state.RoleReviewer).CurrentTurn.StartedAt = l.now.Add(-(600 + 30) * time.Second)
	})
	if rep := l.tick(t); hasKey(rep, "turn_overlong/") {
		t.Fatalf("a turn inside its grace was reported: %v", keys(rep))
	}
}

func TestStaleTriggerIsDetected(t *testing.T) {
	l := newLab(t)
	l.withState(t, func(s *state.State) {
		s.Role(state.RoleProducer).SetPending([]state.Pending{{Label: "inbox/brief.md", Path: "/x", FirstSeen: l.now.Add(-301 * time.Second)}})
	})
	if rep := l.tick(t); !hasKey(rep, "stale_trigger/producer") {
		t.Fatalf("findings=%v", keys(rep))
	}
	l.withState(t, func(s *state.State) {
		// A different trigger: SetPending keeps the first-seen time of a
		// path it already knows, by design.
		s.Role(state.RoleProducer).SetPending([]state.Pending{{Label: "inbox/wake", Path: "/y", FirstSeen: l.now.Add(-10 * time.Second)}})
	})
	if rep := l.tick(t); hasKey(rep, "stale_trigger/") {
		t.Fatalf("a fresh trigger was reported stale: %v", keys(rep))
	}
}

func TestLowDiskIsDetectedOncePerVolume(t *testing.T) {
	l := newLab(t)
	// root and work share a volume that is nearly full; submit is on another
	// with headroom.
	l.p.free[l.root] = 4
	l.p.free[l.cfg.Paths.ProducerWorkdir] = 4
	l.p.dev[l.cfg.Paths.SubmitRepo] = 2
	l.p.free[l.cfg.Paths.SubmitRepo] = 40
	rep := l.tick(t)
	n := 0
	for _, f := range rep.Findings {
		if strings.HasPrefix(f.Key, "disk_low/") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d disk_low findings, want exactly 1 (one per volume, not per path): %v", n, keys(rep))
	}
	if len(rep.Volumes) != 2 {
		t.Fatalf("%d volumes observed, want 2", len(rep.Volumes))
	}
}

// A volume the tick cannot stat is not a volume with headroom.
func TestUnstatableVolumeIsATickError(t *testing.T) {
	l := newLab(t)
	l.p.statErr = errors.New("EIO")
	rep := l.tick(t)
	if rep.Healthy || !hasKey(rep, "tick_error") || len(rep.Errors) == 0 {
		t.Fatalf("healthy=%v findings=%v errors=%v", rep.Healthy, keys(rep), rep.Errors)
	}
}

func fill(t *testing.T, dir, name string, size int, mtime time.Time) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestCacheIsBoundedOldestFirstAndReported(t *testing.T) {
	l := newLab(t)
	cache := filepath.Join(l.root, "cache")
	fill(t, cache, "old/blob", 400, l.now.Add(-3*time.Hour))
	fill(t, cache, "mid/blob", 400, l.now.Add(-2*time.Hour))
	fill(t, cache, "new/blob", 400, l.now.Add(-1*time.Hour))
	l.cfg.Health.Caches = []config.Cache{{Path: cache, MaxBytes: 900}}
	rep := l.tick(t)
	if len(rep.Caches) != 1 {
		t.Fatalf("caches=%+v", rep.Caches)
	}
	c := rep.Caches[0]
	if c.ReclaimedCount != 1 || c.ReclaimedBytes != 400 || c.Bytes != 800 {
		t.Fatalf("reclaimed %d entries / %d bytes, left %d; want the oldest entry only", c.ReclaimedCount, c.ReclaimedBytes, c.Bytes)
	}
	if _, err := os.Stat(filepath.Join(cache, "old")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the oldest entry survived")
	}
	for _, keep := range []string{"mid", "new"} {
		if _, err := os.Stat(filepath.Join(cache, keep)); err != nil {
			t.Fatalf("%s was reclaimed although the bound was already met", keep)
		}
	}
	if hasKey(rep, "cache_over_bound/") || !rep.Healthy {
		t.Fatalf("a cache brought under its bound is not a finding: %v", keys(rep))
	}
}

func TestCacheStillOverBoundIsAFinding(t *testing.T) {
	l := newLab(t)
	cache := filepath.Join(l.root, "cache")
	fill(t, cache, "huge/blob", 2000, l.now)
	l.cfg.Health.Caches = []config.Cache{{Path: cache, MaxBytes: 900}}
	rep := l.tick(t)
	// A single entry over the bound is the newest entry: it is never
	// removed (it would be deleted and rebuilt every tick), and the cache
	// being over its bound is said instead.
	if rep.Caches[0].ReclaimedCount != 0 || !hasKey(rep, "cache_over_bound/") {
		t.Fatalf("reclaimed=%d findings=%v", rep.Caches[0].ReclaimedCount, keys(rep))
	}
	if _, err := os.Stat(filepath.Join(cache, "huge")); err != nil {
		t.Fatal("the only entry was removed")
	}
	os.RemoveAll(cache)
	fill(t, cache, "a/blob", 1000, l.now.Add(-time.Hour))
	fill(t, cache, "b/blob", 1000, l.now)
	rep = l.tick(t)
	if rep.Caches[0].ReclaimedCount != 1 || rep.Caches[0].Bytes != 1000 || !hasKey(rep, "cache_over_bound/") {
		t.Fatalf("reclaimed=%d left=%d findings=%v; the older goes, the newest stays and is over the bound", rep.Caches[0].ReclaimedCount, rep.Caches[0].Bytes, keys(rep))
	}
}

func TestUnreviewedChangeIsDetectedOnlyWhenEnabled(t *testing.T) {
	l := newLab(t)
	l.open = []scm.Change{
		{ID: "7", SourceBranch: "b", UpdatedAt: l.now.Add(-2 * time.Hour)},
		{ID: "8", SourceBranch: "c", UpdatedAt: l.now.Add(-10 * time.Minute)},
		{ID: "9", SourceBranch: "d"}, // no timestamp
	}
	if rep := l.tick(t); hasKey(rep, "unreviewed/") {
		t.Fatalf("disabled check reported: %v", keys(rep))
	}
	l.cfg.Health.UnreviewedSeconds = 3600
	rep := l.tick(t)
	if !hasKey(rep, "unreviewed/7") || hasKey(rep, "unreviewed/8") || !hasKey(rep, "unreviewed/9") {
		t.Fatalf("findings=%v; want 7 (old) and 9 (unknown age), not 8", keys(rep))
	}
	for _, f := range rep.Findings {
		if f.Key == "unreviewed/9" && !strings.Contains(f.Summary, "unknown") {
			t.Fatalf("a change with no timestamp was reported with an invented age: %s", f.Summary)
		}
	}
	l.deps.ListOpen = func(context.Context) ([]scm.Change, error) { return nil, errors.New("401") }
	if rep := l.tick(t); rep.Healthy || !hasKey(rep, "tick_error") {
		t.Fatalf("a provider the tick cannot reach read as healthy: %v", keys(rep))
	}
}

// Cadence: nothing on the first tick, an alert once the condition has held
// alert_after ticks, silence until repeat_seconds, then again; one recovery
// when it clears, and nothing after.
func TestCadenceAlertsAfterNTicksRepeatsAndRecoversOnce(t *testing.T) {
	l := newLab(t)
	l.p.alive[100] = false
	step := func(d time.Duration) health.Report { l.now = l.now.Add(d); return l.tick(t) }

	if rep := l.tick(t); len(rep.Alerted) != 0 {
		t.Fatalf("alerted on the first tick: %v", rep.Alerted)
	}
	if rep := step(time.Minute); len(rep.Alerted) != 1 || rep.Alerted[0] != "supervisor_dead/producer" {
		t.Fatalf("second tick alerted %v, want the condition", rep.Alerted)
	}
	if rep := step(time.Minute); len(rep.Alerted) != 0 {
		t.Fatalf("repeated inside repeat_seconds: %v", rep.Alerted)
	}
	if rep := step(10 * time.Minute); len(rep.Alerted) != 1 {
		t.Fatalf("no repeat after repeat_seconds: %v", rep.Alerted)
	}
	l.p.alive[100] = true
	if rep := step(time.Minute); len(rep.Recovered) != 1 || len(rep.Alerted) != 0 {
		t.Fatalf("recovered=%v alerted=%v", rep.Recovered, rep.Alerted)
	}
	if rep := step(time.Minute); len(rep.Recovered) != 0 {
		t.Fatalf("recovered twice: %v", rep.Recovered)
	}
	lines := l.alertLines(t)
	if len(lines) != 3 {
		t.Fatalf("%d alert lines, want alert, repeat, recovered", len(lines))
	}
	if lines[0].Severity != "alert" || lines[0].Count != 2 || lines[0].Since == nil ||
		lines[1].Severity != "alert" || lines[2].Severity != "recovered" {
		t.Fatalf("lines: %+v", lines)
	}
	// The cadence record lives in the state document, under its lock.
	st, err := state.Load(l.cfg.StatePath(), l.cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Health) != 0 {
		t.Fatalf("a recovered condition is still recorded: %+v", st.Health)
	}
}

// A condition that never reached alert_after was never announced; it clears
// silently. Announcing a recovery nobody was told about is noise.
func TestTransientConditionNeverAnnouncedNeverRecovers(t *testing.T) {
	l := newLab(t)
	l.p.alive[100] = false
	l.tick(t)
	l.p.alive[100] = true
	l.now = l.now.Add(time.Minute)
	rep := l.tick(t)
	if len(rep.Recovered) != 0 || l.alertLines(t) != nil {
		t.Fatalf("recovered=%v lines=%v", rep.Recovered, l.alertLines(t))
	}
}

// A failed delivery is recorded in the document and does not stop the tick.
func TestFailedDeliveryIsRecorded(t *testing.T) {
	l := newLab(t)
	blocker := filepath.Join(t.TempDir(), "f")
	os.WriteFile(blocker, nil, 0o644)
	l.cfg.Alerts = []config.Alert{{Kind: "file", Path: filepath.Join(blocker, "x.log")}}
	fan, _ := alert.New(l.cfg)
	l.deps.Alerts = fan
	l.p.alive[100] = false
	l.tick(t)
	l.now = l.now.Add(time.Minute)
	rep := l.tick(t)
	if len(rep.Alerted) != 1 || len(rep.Deliveries) != 1 || rep.Deliveries[0].Err == "" {
		t.Fatalf("alerted=%v deliveries=%+v", rep.Alerted, rep.Deliveries)
	}
}
