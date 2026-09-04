// Package alert delivers alerts through the configured transports
// (SPEC.md §7). The health subsystem detects; this delivers. v1 had the first
// and not the second, and a stalled factory was recorded correctly every
// fifteen minutes into a journal nobody read.
//
// Transports are best-effort and independent: one failing must not suppress
// the others, and every failure is reported by name rather than folded into a
// single error that hides which channel is dead.
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
)

// Alert is one message. It is the same document on every transport.
type Alert struct {
	Factory string    `json:"factory"`
	At      time.Time `json:"at"`
	// Kind is the condition key ("supervisor_dead", "disk_low", ...) or
	// "doctor" for a delivery probe, "recovered" when a condition clears.
	Kind string `json:"kind"`
	// Severity is "alert", "recovered" or "probe".
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
	// Detail is free text; the structured fields carry what matters.
	Detail string `json:"detail,omitempty"`
	// Count is how many consecutive ticks the condition has held.
	Count int `json:"count,omitempty"`
	// Since is when the condition was first seen.
	Since *time.Time `json:"since,omitempty"`
}

// Transport delivers one alert.
type Transport interface {
	// Name identifies the transport in failure reports ("file /var/...").
	Name() string
	Deliver(ctx context.Context, a Alert) error
}

// Delivery is the outcome on one transport.
type Delivery struct {
	Transport string
	Err       error
}

// Fanout delivers to every transport, independently, and reports each.
type Fanout struct {
	Transports []Transport
}

// New builds the fan-out from config. Every kind the config was allowed to
// carry is constructible here; an unknown one is a programming error, not a
// transport that silently does nothing.
func New(cfg *config.Config) (*Fanout, error) {
	if len(cfg.Alerts) == 0 {
		return nil, errors.New("no alert transport configured")
	}
	f := &Fanout{}
	for i, a := range cfg.Alerts {
		switch a.Kind {
		case "file":
			f.Transports = append(f.Transports, &File{Path: a.Path})
		case "command":
			f.Transports = append(f.Transports, &Command{Argv: a.Command, Env: a.Env, Timeout: time.Duration(a.TimeoutSeconds) * time.Second})
		default:
			return nil, fmt.Errorf("alerts[%d]: kind %q has no transport in this build", i, a.Kind)
		}
	}
	return f, nil
}

// Deliver sends a to every transport. It returns one Delivery per transport,
// in config order, and an error only if EVERY transport failed -- that is the
// case where the alert reached nobody. A partial failure is visible in the
// deliveries and must be surfaced by the caller; it is not an error here
// because the alert did land somewhere.
func (f *Fanout) Deliver(ctx context.Context, a Alert) ([]Delivery, error) {
	var out []Delivery
	failed := 0
	for _, t := range f.Transports {
		err := t.Deliver(ctx, a)
		if err != nil {
			failed++
		}
		out = append(out, Delivery{Transport: t.Name(), Err: err})
	}
	if failed == len(f.Transports) {
		names := make([]string, 0, len(out))
		for _, d := range out {
			names = append(names, fmt.Sprintf("%s: %v", d.Transport, d.Err))
		}
		return out, fmt.Errorf("alert reached no transport: %s", strings.Join(names, "; "))
	}
	return out, nil
}

// File appends one JSON line per alert. The write is synced: an alert that is
// in the page cache when the host dies was not delivered.
type File struct {
	Path string
}

func (f *File) Name() string { return "file " + f.Path }

func (f *File) Deliver(_ context.Context, a Alert) error {
	line, err := json.Marshal(a)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return err
	}
	fh, err := os.OpenFile(f.Path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return err
	}
	defer fh.Close()
	if _, err := fh.Write(line); err != nil {
		return err
	}
	return fh.Sync()
}

// Command runs argv with the alert as JSON on stdin. Exit 0 is delivery;
// anything else is not, and stderr is carried in the error so the operator
// sees why.
type Command struct {
	Argv    []string
	Env     map[string]string
	Timeout time.Duration
}

func (c *Command) Name() string { return "command " + strings.Join(c.Argv, " ") }

func (c *Command) Deliver(ctx context.Context, a Alert) error {
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	// One document, newline-terminated: a sink that appends stdin to a file
	// gets one alert per line, the same shape the file transport writes.
	body = append(body, '\n')
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	exe, err := config.LookPathIn(c.Env["PATH"], c.Argv[0])
	if err != nil {
		return fmt.Errorf("%s: %w", c.Argv[0], err)
	}
	cmd := exec.CommandContext(ctx, exe, c.Argv[1:]...)
	cmd.Env = envList(c.Env)
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("timed out after %s", c.Timeout)
		}
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func envList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}
