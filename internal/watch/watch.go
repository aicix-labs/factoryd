// Package watch blocks until a handoff trigger appears on disk.
//
// In v1 the watcher was a separate single-shot script. It exited after firing,
// nothing re-armed it, and a reviewer signal then sat unseen for two hours and
// twenty minutes. Here a Watcher is a value the supervisor owns for its whole
// life; it cannot be independently lost, because there is nothing separate to
// lose.
//
// Two properties are load-bearing:
//
//   - Wait never consumes a trigger. Consuming is the agent turn's job, and the
//     supervisor's spin guard is built on being able to see that a turn did not
//     do it (SPEC.md §4.3).
//   - Wait returns immediately when a trigger is already pending. A watcher
//     that only reported *new* events would miss a trigger written between two
//     calls, which is precisely the two-hour-twenty gap.
package watch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Spec is one trigger to watch for.
type Spec struct {
	// Label names the trigger in state and in logs ("wake", "verdict").
	Label string
	// Dir is the directory to watch.
	Dir string
	// Pattern matches basenames, using filepath.Match. An exact filename is a
	// valid pattern.
	Pattern string
}

func (s Spec) String() string { return s.Label + " (" + filepath.Join(s.Dir, s.Pattern) + ")" }

// Trigger is one file that matched a Spec.
type Trigger struct {
	Label   string    `json:"label"`
	Path    string    `json:"path"`
	ModTime time.Time `json:"mod_time"`
}

// Mode is how a Watcher is observing the filesystem.
type Mode string

const (
	// ModeInotify is event-driven.
	ModeInotify Mode = "inotify"
	// ModePoll is the fallback: correct everywhere, but a trigger is seen
	// within one interval rather than immediately.
	ModePoll Mode = "poll"
)

// Watcher observes a set of trigger specs.
type Watcher struct {
	specs    []Spec
	dirs     []string
	interval time.Duration
	mode     Mode
	// modeReason explains a fallback. It is non-empty exactly when the watcher
	// wanted inotify and did not get it, so a degraded watcher can never be
	// silently mistaken for an event-driven one.
	modeReason string

	ino *inotify // nil in poll mode
}

// Options configures a Watcher.
type Options struct {
	// Interval is the poll period, used in poll mode and as a safety re-check
	// in inotify mode. Defaults to 2s.
	Interval time.Duration
	// ForcePoll skips inotify entirely. Used by tests to exercise the fallback
	// path on a machine where inotify works.
	ForcePoll bool
}

// New builds a Watcher. Every spec's directory must already exist: a watcher
// pointed at a directory that is not there would block forever and report
// nothing, and its silence would be indistinguishable from "no triggers yet".
func New(specs []Spec, opts Options) (*Watcher, error) {
	if len(specs) == 0 {
		return nil, fmt.Errorf("watch: no trigger specs; a watcher for nothing would block forever")
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}

	seen := map[string]bool{}
	var dirs []string
	for _, s := range specs {
		if s.Label == "" || s.Dir == "" || s.Pattern == "" {
			return nil, fmt.Errorf("watch: spec %+v is missing a label, dir, or pattern", s)
		}
		if _, err := filepath.Match(s.Pattern, "probe"); err != nil {
			return nil, fmt.Errorf("watch: spec %s has an invalid pattern %q: %w", s.Label, s.Pattern, err)
		}
		fi, err := os.Stat(s.Dir)
		if err != nil {
			return nil, fmt.Errorf("watch: spec %s: %w", s.Label, err)
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("watch: spec %s: %s is not a directory", s.Label, s.Dir)
		}
		if !seen[s.Dir] {
			seen[s.Dir] = true
			dirs = append(dirs, s.Dir)
		}
	}
	sort.Strings(dirs)

	w := &Watcher{specs: specs, dirs: dirs, interval: interval, mode: ModePoll}
	if opts.ForcePoll {
		w.modeReason = "poll forced by configuration"
		return w, nil
	}

	ino, err := newInotify(dirs)
	if err != nil {
		// Falling back is correct; falling back quietly is not. The reason is
		// retained so the supervisor can say it out loud at boot.
		w.modeReason = "inotify unavailable: " + err.Error()
		return w, nil
	}
	w.ino = ino
	w.mode = ModeInotify
	return w, nil
}

// Mode reports how the watcher is observing.
func (w *Watcher) Mode() Mode { return w.mode }

// ModeReason explains a fallback to poll, and is empty when inotify is in use.
func (w *Watcher) ModeReason() string { return w.modeReason }

// Close releases any inotify resources.
func (w *Watcher) Close() error {
	if w.ino != nil {
		return w.ino.Close()
	}
	return nil
}

// Pending returns every trigger that exists right now, sorted by label then
// path. It never removes anything.
func (w *Watcher) Pending() ([]Trigger, error) {
	var out []Trigger
	for _, s := range w.specs {
		entries, err := os.ReadDir(s.Dir)
		if err != nil {
			if os.IsNotExist(err) {
				// The directory went away underneath us. That is a real fault,
				// not an empty result: reporting "no triggers" here would make
				// a deleted inbox look like a quiet one.
				return nil, fmt.Errorf("watch: %s: %w", s.Label, err)
			}
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ok, err := filepath.Match(s.Pattern, e.Name())
			if err != nil || !ok {
				continue
			}
			info, err := e.Info()
			if err != nil {
				// Raced with a removal; it is not pending.
				continue
			}
			out = append(out, Trigger{
				Label:   s.Label,
				Path:    filepath.Join(s.Dir, e.Name()),
				ModTime: info.ModTime(),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Label != out[j].Label {
			return out[i].Label < out[j].Label
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// Wait returns the pending triggers, blocking until at least one exists or ctx
// is done. It returns immediately if something is already pending.
//
// In inotify mode it still re-checks every interval. inotify can miss events in
// cases that are hard to enumerate (a watch added after a write, an overflowed
// event queue); the periodic re-check bounds how long any such miss can hide,
// so the event path is an optimisation over the poll and never the sole way a
// trigger is noticed.
func (w *Watcher) Wait(ctx context.Context) ([]Trigger, error) {
	for {
		pending, err := w.Pending()
		if err != nil {
			return nil, err
		}
		if len(pending) > 0 {
			return pending, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := w.block(ctx); err != nil {
			return nil, err
		}
	}
}

// block waits for an event or for the interval to elapse.
func (w *Watcher) block(ctx context.Context) error {
	if w.ino == nil {
		t := time.NewTimer(w.interval)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
	return w.ino.wait(ctx, w.interval)
}
