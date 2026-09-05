// Package health is the periodic, model-free tick (SPEC.md §4.5, §7). It
// detects and reports; it must never merge, review, or re-arm anything --
// those need a live agent. Its one write to the factory is reclaiming a cache
// it was told to bound, and it reports what it reclaimed.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aicix-labs/factoryd/internal/alert"
	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/state"
)

// Finding is one condition observed on one tick. Key is stable across ticks
// so that cadence can be kept per condition ("supervisor_dead/producer").
type Finding struct {
	Key     string `json:"key"`
	Summary string `json:"summary"`
	Detail  string `json:"detail,omitempty"`
}

// Volume is one filesystem the factory writes to.
type Volume struct {
	Path        string  `json:"path"`
	Mount       string  `json:"mount,omitempty"`
	TotalBytes  uint64  `json:"total_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	FreePercent float64 `json:"free_percent"`
}

// CacheReport is one bounded cache after this tick.
type CacheReport struct {
	Path           string `json:"path"`
	MaxBytes       int64  `json:"max_bytes"`
	Bytes          int64  `json:"bytes"`
	ReclaimedBytes int64  `json:"reclaimed_bytes"`
	ReclaimedCount int    `json:"reclaimed_count"`
	Err            string `json:"error,omitempty"`
}

// Report is the health document (§4.5), written to <root>/health.json.
type Report struct {
	Factory  string        `json:"factory"`
	At       time.Time     `json:"at"`
	Healthy  bool          `json:"healthy"`
	Findings []Finding     `json:"findings"`
	Volumes  []Volume      `json:"volumes"`
	Caches   []CacheReport `json:"caches,omitempty"`
	// Alerted lists the finding keys alerted on this tick, and Recovered the
	// ones that cleared; each Delivery records where an alert went and
	// whether it arrived.
	Alerted    []string         `json:"alerted,omitempty"`
	Recovered  []string         `json:"recovered,omitempty"`
	Deliveries []DeliveryRecord `json:"deliveries,omitempty"`
	// StateAlertAt is when the fail-safe "cadence record unavailable" alert
	// last went out, kept here because the state document is exactly what
	// is unavailable. It is the fallback cadence.
	StateAlertAt *time.Time `json:"state_alert_at,omitempty"`
	// Errors are failures of the tick itself: a state document it could not
	// read, a volume it could not stat. They make the tick unhealthy: a tick
	// that cannot look is not a tick that saw nothing.
	Errors []string `json:"errors,omitempty"`
}

// DeliveryRecord is one alert on one transport.
type DeliveryRecord struct {
	Key       string `json:"key"`
	Transport string `json:"transport"`
	Err       string `json:"error,omitempty"`
}

// ObservationError is returned by Tick, alongside the report it still wrote,
// when something could not be looked at: a volume that would not stat, a
// provider that would not answer, a state document that would not load.
// It is a distinct type so the CLI can say "could not look" (exit 3) rather
// than "unhealthy" (exit 1): the two are different failures with different
// remedies, and folding them was a way for a blind tick to read as a
// finding.
type ObservationError struct {
	Errors []string
}

func (e *ObservationError) Error() string {
	return "the tick could not observe: " + strings.Join(e.Errors, "; ")
}

// Probes are the host facts the tick reads. They are an interface so that
// tests can present a dying supervisor or a full disk without one.
type Probes interface {
	// Alive reports whether the supervisor handle still refers to the process
	// it was taken from.
	Alive(ref state.RoleState) (bool, error)
	// Statfs reports a path's volume.
	Statfs(path string) (Volume, error)
	// DeviceID identifies the volume a path is on, for de-duplication.
	DeviceID(path string) (uint64, error)
}

// Deps is what a tick needs beyond config.
type Deps struct {
	Probes Probes
	// Alerts is nil only in tests that check detection alone; a real tick
	// always has one, because config refuses to load without a transport.
	Alerts *alert.Fanout
	// ListOpen is consulted only when health.unreviewed_seconds > 0.
	ListOpen func(ctx context.Context) ([]scm.Change, error)
	Now      func() time.Time
	Log      io.Writer
}

// Tick runs one observation: read state, check every condition, reclaim
// bounded caches, keep cadence, alert, write the health document. It returns
// the report; err is non-nil only when the tick could not run at all.
func Tick(ctx context.Context, cfg *config.Config, deps Deps) (Report, error) {
	now := time.Now
	if deps.Now != nil {
		now = deps.Now
	}
	log := deps.Log
	if log == nil {
		log = io.Discard
	}
	at := now()
	rep := Report{Factory: cfg.Name, At: at}

	// 1. Observe. Findings are collected; nothing is decided yet.
	// "Never supervised" is judged from the document's content, not the
	// file's existence: the tick's own cadence record creates the file, so a
	// finding keyed on absence would be erased by the tick that reported it.
	st, err := state.Load(cfg.StatePath(), cfg.Name)
	stateErr := err
	if err != nil {
		rep.Errors = append(rep.Errors, "state: "+err.Error())
		st = state.New(cfg.Name)
	}
	if !everSupervised(st) {
		rep.Findings = append(rep.Findings, Finding{Key: "never_supervised", Summary: "no supervisor has ever registered for either role; nothing is watching this factory"})
	}
	rep.Findings = append(rep.Findings, checkRoles(cfg, st, deps.Probes, at)...)

	vols, findings, errs := checkVolumes(cfg, deps.Probes)
	rep.Volumes = vols
	rep.Findings = append(rep.Findings, findings...)
	rep.Errors = append(rep.Errors, errs...)

	caches, findings := reclaimCaches(cfg, at, log)
	rep.Caches = caches
	rep.Findings = append(rep.Findings, findings...)

	if cfg.Health.UnreviewedSeconds > 0 {
		if deps.ListOpen == nil {
			rep.Errors = append(rep.Errors, "unreviewed check configured but no provider access was given")
		} else if fs, err := checkUnreviewed(ctx, cfg, deps.ListOpen, at); err != nil {
			rep.Errors = append(rep.Errors, "list open changes: "+err.Error())
		} else {
			rep.Findings = append(rep.Findings, fs...)
		}
	}
	for _, e := range rep.Errors {
		rep.Findings = append(rep.Findings, Finding{Key: "tick_error", Summary: "the tick could not observe something", Detail: e})
	}
	sort.Slice(rep.Findings, func(i, j int) bool { return rep.Findings[i].Key < rep.Findings[j].Key })
	rep.Healthy = len(rep.Findings) == 0

	// 2. Cadence, under the state lock. What to alert is decided and recorded
	// in the same critical section; the alerts go out after it. A crash
	// between the two loses at most one alert, which the repeat cadence
	// resends; the reverse order could send the same alert on every tick.
	var toAlert, recovered []Finding
	var stateAlert *alert.Alert
	if stateErr == nil {
		updated, err := state.Update(cfg.StatePath(), cfg.Name, func(s *state.State) error {
			toAlert, recovered = cadence(cfg, s, rep.Findings, at)
			return nil
		})
		if err == nil {
			st = updated // the cadence record the alerts quote is the one just written
		} else {
			stateErr = err
			rep.Errors = append(rep.Errors, "state update: "+err.Error())
			rep.Findings = append(rep.Findings, Finding{Key: "tick_error", Summary: "the tick could not observe something", Detail: "state update: " + err.Error()})
			rep.Healthy = false
		}
	}
	if stateErr != nil {
		// The cadence record is unavailable: a corrupt or unwritable state
		// document. Without a fail-safe this tick would record the failure
		// into health.json and stdout and alert nobody -- the journal-only
		// failure the subsystem exists to prevent. So an alert goes out
		// that depends on nothing but the transports, carrying every
		// finding, bounded by the previous health document's record of
		// when it last did so. If that too is unreadable, it goes out now.
		toAlert, recovered = nil, nil
		if prev, ok := readPrevious(cfg.HealthPath()); !ok || prev.StateAlertAt == nil ||
			at.Sub(*prev.StateAlertAt) >= time.Duration(cfg.Health.RepeatSeconds)*time.Second {
			detail := stateErr.Error()
			for _, f := range rep.Findings {
				if f.Key != "tick_error" {
					detail += "\n" + f.Key + ": " + f.Summary
				}
			}
			stateAlert = &alert.Alert{Factory: cfg.Name, At: at, Kind: "state_unavailable", Severity: "alert",
				Summary: "the health tick cannot keep its cadence record; alerting without one", Detail: detail}
			rep.StateAlertAt = &at
		} else {
			rep.StateAlertAt = prev.StateAlertAt
		}
	}

	// 3. Deliver. Every transport, independently.
	if deps.Alerts != nil {
		if stateAlert != nil {
			rep.Alerted = append(rep.Alerted, stateAlert.Kind)
			rep.Deliveries = append(rep.Deliveries, deliver(ctx, deps.Alerts, stateAlert.Kind, *stateAlert)...)
		}
		for _, f := range toAlert {
			cond := condFor(st, f.Key)
			a := alert.Alert{Factory: cfg.Name, At: at, Kind: f.Key, Severity: "alert", Summary: f.Summary, Detail: f.Detail}
			if cond != nil {
				a.Count = cond.Ticks
				since := cond.FirstSeen
				a.Since = &since
			}
			rep.Alerted = append(rep.Alerted, f.Key)
			rep.Deliveries = append(rep.Deliveries, deliver(ctx, deps.Alerts, f.Key, a)...)
		}
		for _, f := range recovered {
			a := alert.Alert{Factory: cfg.Name, At: at, Kind: f.Key, Severity: "recovered", Summary: "recovered: " + f.Summary}
			rep.Recovered = append(rep.Recovered, f.Key)
			rep.Deliveries = append(rep.Deliveries, deliver(ctx, deps.Alerts, f.Key, a)...)
		}
	}
	for _, d := range rep.Deliveries {
		if d.Err != "" {
			fmt.Fprintf(log, "alert %s via %s FAILED: %s\n", d.Key, d.Transport, d.Err)
		}
	}

	// 4. Write the document, atomically; then say whether the tick could look.
	if err := writeReport(cfg.HealthPath(), rep); err != nil {
		return rep, fmt.Errorf("writing %s: %w", cfg.HealthPath(), err)
	}
	if len(rep.Errors) > 0 {
		return rep, &ObservationError{Errors: rep.Errors}
	}
	return rep, nil
}

func readPrevious(path string) (Report, bool) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Report{}, false
	}
	var r Report
	if err := json.Unmarshal(body, &r); err != nil {
		return Report{}, false
	}
	return r, true
}

func everSupervised(st *state.State) bool {
	for _, r := range state.Roles {
		if st.Role(r).Supervisor != nil {
			return true
		}
	}
	return false
}

func condFor(st *state.State, key string) *state.Condition {
	if st == nil || st.Health == nil {
		return nil
	}
	return st.Health[key]
}

func deliver(ctx context.Context, f *alert.Fanout, key string, a alert.Alert) []DeliveryRecord {
	ds, _ := f.Deliver(ctx, a)
	var out []DeliveryRecord
	for _, d := range ds {
		r := DeliveryRecord{Key: key, Transport: d.Transport}
		if d.Err != nil {
			r.Err = d.Err.Error()
		}
		out = append(out, r)
	}
	return out
}

// cadence updates the standing-condition record and returns what to alert
// now: a condition that has held for alert_after ticks and has not been
// alerted, or one whose last alert is older than repeat_seconds. A
// condition that was recorded and is absent this tick has recovered; it is
// reported once and forgotten.
func cadence(cfg *config.Config, s *state.State, findings []Finding, at time.Time) (toAlert, recovered []Finding) {
	if s.Health == nil {
		s.Health = map[string]*state.Condition{}
	}
	seen := map[string]bool{}
	for _, f := range findings {
		seen[f.Key] = true
		c := s.Health[f.Key]
		if c == nil {
			c = &state.Condition{FirstSeen: at}
			s.Health[f.Key] = c
		}
		c.LastSeen = at
		c.Ticks++
		c.Summary = f.Summary
		if c.Ticks < cfg.Health.AlertAfter {
			continue
		}
		repeat := time.Duration(cfg.Health.RepeatSeconds) * time.Second
		if c.LastAlerted.IsZero() || at.Sub(c.LastAlerted) >= repeat {
			c.LastAlerted = at
			toAlert = append(toAlert, f)
		}
	}
	for key, c := range s.Health {
		if seen[key] {
			continue
		}
		// Only a condition that was actually alerted recovers audibly; one
		// that never reached alert_after was never announced.
		if !c.LastAlerted.IsZero() {
			recovered = append(recovered, Finding{Key: key, Summary: c.Summary})
		}
		delete(s.Health, key)
	}
	sort.Slice(recovered, func(i, j int) bool { return recovered[i].Key < recovered[j].Key })
	return toAlert, recovered
}

// checkRoles observes each role: a registered supervisor that is not alive
// and did not halt; a turn running past its timeout plus grace (the
// supervisor should have killed it, so the supervisor is stuck); a trigger
// left unconsumed past the stale threshold; a halted role, which is a
// condition until an operator clears it -- a halt with no alert is the
// stall v1 recorded into a journal.
func checkRoles(cfg *config.Config, st *state.State, p Probes, at time.Time) []Finding {
	var out []Finding
	if err := st.VerdictRegistry.MigrationError(); err != nil {
		out = append(out, Finding{
			Key:     "verdict_registry_migration",
			Summary: "producer verdict registry migration is required; legacy outbox verdicts are intentionally blocked",
			Detail:  err.Error() + "; run factoryd migrate --config " + cfg.Path() + " verdict-registry, then reissue any still-current verdict",
		})
	}
	for _, r := range state.Roles {
		rs := st.Role(r)
		role := string(r)
		if rs.Halted {
			remedy := "sentinel already cleared; restart to resume"
			if _, err := os.Lstat(cfg.StopPath(role)); err == nil {
				remedy = "remove " + cfg.StopPath(role) + " and restart"
			}
			out = append(out, Finding{Key: "halted/" + role, Summary: fmt.Sprintf("%s supervisor halted: %s", role, rs.HaltReason),
				Detail: fmt.Sprintf("halted at %s; %s", rs.HaltedAt.Format(time.RFC3339), remedy)})
			continue
		}
		if b := rs.Blocked; b != nil {
			out = append(out, Finding{Key: "blocked/" + role, Summary: fmt.Sprintf("%s submission %s: %s", role, b.Disposition, firstLine(b.Reason)),
				Detail: fmt.Sprintf("turn %s at %s; family %s; not retried automatically -- fix the cause and submit again (factoryd submit), which clears this", b.Turn, b.At.Format(time.RFC3339), b.Family)})
		}
		if rs.Supervisor != nil {
			alive, err := p.Alive(*rs)
			switch {
			case err != nil:
				out = append(out, Finding{Key: "supervisor_unknown/" + role, Summary: fmt.Sprintf("%s supervisor liveness could not be determined", role), Detail: err.Error()})
			case !alive:
				out = append(out, Finding{Key: "supervisor_dead/" + role, Summary: fmt.Sprintf("%s supervisor %s is not running and did not halt", role, rs.Supervisor)})
			}
		}
		if t := rs.CurrentTurn; t.Running() {
			limit := time.Duration(cfg.Roles.Spec(role).TimeoutSeconds+cfg.Health.TurnGraceSeconds) * time.Second
			if age := t.Age(at); age > limit {
				out = append(out, Finding{Key: "turn_overlong/" + role, Summary: fmt.Sprintf("%s turn %s has run %s, past its %s timeout plus grace; the supervisor should have ended it", role, t.ID, age.Round(time.Second), limit)})
			}
		}
		if p, ok := rs.OldestPending(); ok {
			stale := time.Duration(cfg.Health.StaleTriggerSeconds) * time.Second
			if age := at.Sub(p.FirstSeen); age > stale {
				out = append(out, Finding{Key: "stale_trigger/" + role, Summary: fmt.Sprintf("%s trigger %s has waited %s unconsumed", role, p.Label, age.Round(time.Second))})
			}
		}
	}
	return out
}

// checkVolumes stats every volume the factory writes to, once per volume,
// and reports any below the headroom. A path that cannot be stat'ed is a
// tick error: a volume the tick cannot see is not a volume with headroom.
func checkVolumes(cfg *config.Config, p Probes) ([]Volume, []Finding, []string) {
	paths := writePaths(cfg)
	seen := map[uint64]bool{}
	var vols []Volume
	var findings []Finding
	var errs []string
	for _, path := range paths {
		dev, err := p.DeviceID(path)
		if err != nil {
			errs = append(errs, fmt.Sprintf("volume of %s: %v", path, err))
			continue
		}
		if seen[dev] {
			continue
		}
		seen[dev] = true
		v, err := p.Statfs(path)
		if err != nil {
			errs = append(errs, fmt.Sprintf("statfs %s: %v", path, err))
			continue
		}
		vols = append(vols, v)
		if v.FreePercent < float64(cfg.Health.DiskMinFreePercent) {
			findings = append(findings, Finding{Key: "disk_low/" + v.Path,
				Summary: fmt.Sprintf("volume of %s has %.1f%% free (%s of %s), below the %d%% headroom", v.Path, v.FreePercent, human(v.FreeBytes), human(v.TotalBytes), cfg.Health.DiskMinFreePercent)})
		}
	}
	return vols, findings, errs
}

// writePaths is every directory the factory writes: root, both repositories,
// the gate's declared paths, the bounded caches, and the alert files. A
// path that does not exist yet is checked through its nearest existing
// ancestor, because that is the volume it will land on.
func writePaths(cfg *config.Config) []string {
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		out = append(out, nearestExisting(p))
	}
	add(cfg.Paths.Root)
	add(cfg.Paths.ProducerWorkdir)
	add(cfg.Paths.SubmitRepo)
	for _, raw := range cfg.Gate.RequiredWritablePaths {
		if p, err := cfg.ResolveGatePath(raw); err == nil {
			add(p)
		}
	}
	add(cfg.Paths.CacheRoot)
	for _, c := range cfg.Health.Caches {
		add(c.Path)
	}
	for _, a := range cfg.Alerts {
		if a.Kind == "file" {
			add(filepath.Dir(a.Path))
		}
	}
	return out
}

func nearestExisting(p string) string {
	for {
		if _, err := os.Lstat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

// afterOpenCacheRoot is a test seam, nil in production.
var afterOpenCacheRoot func()

// reclaimCaches bounds each declared cache: while it exceeds max_bytes, its
// oldest top-level entry (by newest file mtime) is removed. What was
// removed is reported, and a cache still over its bound after reclamation
// -- one enormous entry -- is a finding.
//
// Every operation goes through the opened, verified cache root (CacheRoot):
// relative, refusing to follow a symlink out. No absolute path is used
// after the open, so nothing an untrusted writer does to the path -- a
// symlink at the cache, at the root, or at the root's renamed parent --
// changes what is deleted. A root that cannot be opened and verified is a
// finding under which nothing is reclaimed.
func reclaimCaches(cfg *config.Config, at time.Time, log io.Writer) ([]CacheReport, []Finding) {
	var reps []CacheReport
	var findings []Finding
	if len(cfg.Health.Caches) == 0 {
		return nil, nil
	}
	cr, rootErr := OpenCacheRoot(cfg.Paths.CacheRoot)
	if rootErr == nil {
		defer cr.Close()
	}
	if afterOpenCacheRoot != nil {
		// Test seam: the world moves after the root is bound and before
		// anything is deleted. What is deleted must follow the handle.
		afterOpenCacheRoot()
	}
	for _, c := range cfg.Health.Caches {
		r := CacheReport{Path: c.Path, MaxBytes: c.MaxBytes}
		if rootErr != nil {
			r.Err = rootErr.Error()
			reps = append(reps, r)
			findings = append(findings, Finding{Key: "cache_unsafe/" + c.Path, Summary: "the cache root cannot be opened and verified; nothing reclaimed", Detail: rootErr.Error()})
			continue
		}
		rel, err := cr.Rel(c.Path)
		if err != nil {
			r.Err = err.Error()
			reps = append(reps, r)
			findings = append(findings, Finding{Key: "cache_unsafe/" + c.Path, Summary: "cache is not inside the cache root; nothing reclaimed", Detail: err.Error()})
			continue
		}
		entries, err := cr.entries(rel)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				reps = append(reps, r)
				continue
			}
			r.Err = err.Error()
			reps = append(reps, r)
			findings = append(findings, Finding{Key: "cache_unsafe/" + c.Path, Summary: "cache " + c.Path + " could not be read inside the root; nothing reclaimed", Detail: err.Error()})
			continue
		}
		var total int64
		for _, e := range entries {
			total += e.bytes
		}
		r.Bytes = total
		sort.Slice(entries, func(i, j int) bool { return entries[i].mtime.Before(entries[j].mtime) })
		// The newest entry is never removed: it is the one most likely in
		// use, and a single entry larger than the bound would otherwise be
		// deleted and rebuilt on every tick. Left over the bound, it is a
		// finding instead.
		for i, e := range entries {
			if r.Bytes <= c.MaxBytes || i == len(entries)-1 {
				break
			}
			var err error
			if e.link {
				// A symlink entry is removed as a link, never followed.
				err = cr.root.Remove(e.rel)
			} else {
				err = cr.root.RemoveAll(e.rel)
			}
			if err != nil {
				r.Err = fmt.Sprintf("removing %s: %v", e.rel, err)
				findings = append(findings, Finding{Key: "cache_unsafe/" + c.Path, Summary: "reclamation of " + c.Path + " stopped", Detail: r.Err})
				break
			}
			r.Bytes -= e.bytes
			r.ReclaimedBytes += e.bytes
			r.ReclaimedCount++
			fmt.Fprintf(log, "cache %s: reclaimed %s (%s, last modified %s)\n", c.Path, e.rel, human(uint64(e.bytes)), e.mtime.Format(time.RFC3339))
		}
		if r.Bytes > c.MaxBytes {
			findings = append(findings, Finding{Key: "cache_over_bound/" + c.Path,
				Summary: fmt.Sprintf("cache %s is %s after reclamation, over its %s bound", c.Path, human(uint64(r.Bytes)), human(uint64(c.MaxBytes))), Detail: r.Err})
		}
		reps = append(reps, r)
	}
	return reps, findings
}

type cacheEntry struct {
	rel   string // relative to the cache root
	bytes int64
	mtime time.Time
	link  bool // the entry itself is a symlink
}

// entries lists the top-level entries of a cache, each sized by a walk that
// never follows a symlink and never leaves the root.
func (c *CacheRoot) entries(rel string) ([]cacheEntry, error) {
	d, err := c.root.Open(rel)
	if err != nil {
		return nil, err
	}
	des, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		return nil, err
	}
	var out []cacheEntry
	for _, de := range des {
		p := filepath.Join(rel, de.Name())
		e := cacheEntry{rel: p}
		info, err := c.root.Lstat(p)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			e.link, e.bytes, e.mtime = true, info.Size(), info.ModTime()
			out = append(out, e)
			continue
		}
		if err := c.size(p, &e); err != nil {
			return nil, err
		}
		if e.mtime.IsZero() { // an empty entry: fall back to the directory itself
			e.mtime = info.ModTime()
		}
		out = append(out, e)
	}
	return out, nil
}

// size accumulates file sizes and the newest FILE mtime under rel. Files,
// not directories: a directory's mtime is when an entry was added to it,
// which every walk of a build cache bumps; the newest file says when the
// entry was last used. Symlinks are counted as themselves.
func (c *CacheRoot) size(rel string, e *cacheEntry) error {
	info, err := c.root.Lstat(rel)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		e.bytes += info.Size()
		if info.ModTime().After(e.mtime) {
			e.mtime = info.ModTime()
		}
		return nil
	}
	d, err := c.root.Open(rel)
	if err != nil {
		return err
	}
	des, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		return err
	}
	for _, de := range des {
		if err := c.size(filepath.Join(rel, de.Name()), e); err != nil {
			return err
		}
	}
	return nil
}

// checkUnreviewed reports open changes that have not been touched within the
// threshold. It reads; it never acts.
func checkUnreviewed(ctx context.Context, cfg *config.Config, list func(context.Context) ([]scm.Change, error), at time.Time) ([]Finding, error) {
	changes, err := list(ctx)
	if err != nil {
		return nil, err
	}
	limit := time.Duration(cfg.Health.UnreviewedSeconds) * time.Second
	var out []Finding
	for _, c := range changes {
		last := c.UpdatedAt
		if last.IsZero() {
			out = append(out, Finding{Key: "unreviewed/" + string(c.ID), Summary: fmt.Sprintf("change %s is open with no timestamp the provider would give; its age is unknown, which is not the same as recent", c.ID)})
			continue
		}
		if age := at.Sub(last); age > limit {
			out = append(out, Finding{Key: "unreviewed/" + string(c.ID), Summary: fmt.Sprintf("change %s (%s) has been open untouched for %s", c.ID, c.SourceBranch, age.Round(time.Minute))})
		}
	}
	return out, nil
}

func writeReport(path string, rep Report) error {
	body, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func human(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// Summary renders the report for a terminal.
func (r Report) Summary() string {
	var sb strings.Builder
	if r.Healthy {
		sb.WriteString("healthy\n")
	} else {
		fmt.Fprintf(&sb, "UNHEALTHY: %d finding(s)\n", len(r.Findings))
	}
	for _, f := range r.Findings {
		fmt.Fprintf(&sb, "  %-32s %s\n", f.Key, f.Summary)
	}
	for _, v := range r.Volumes {
		fmt.Fprintf(&sb, "  volume %-25s %.1f%% free (%s of %s)\n", v.Path, v.FreePercent, human(v.FreeBytes), human(v.TotalBytes))
	}
	for _, c := range r.Caches {
		fmt.Fprintf(&sb, "  cache  %-25s %s of %s", c.Path, human(uint64(c.Bytes)), human(uint64(c.MaxBytes)))
		if c.ReclaimedCount > 0 {
			fmt.Fprintf(&sb, "; reclaimed %d entries, %s", c.ReclaimedCount, human(uint64(c.ReclaimedBytes)))
		}
		sb.WriteString("\n")
	}
	for _, k := range r.Alerted {
		fmt.Fprintf(&sb, "  alerted  %s\n", k)
	}
	for _, k := range r.Recovered {
		fmt.Fprintf(&sb, "  recovered %s\n", k)
	}
	return sb.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
