package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aicix-labs/factoryd/internal/alert"
	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/factory"
	"github.com/aicix-labs/factoryd/internal/health"
	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/state"
)

// tickOnce runs one tick and classifies it: 0 healthy, 1 findings, 3 could
// not look -- an ObservationError, or a tick that could not run at all. The
// report is printed in every case it exists, so "could not look" still
// shows what was seen.
func tickOnce(ctx context.Context, cfg *config.Config, deps health.Deps, asJSON bool, stdout, stderr io.Writer) int {
	rep, err := health.Tick(ctx, cfg, deps)
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		fmt.Fprintf(stdout, "%s %s", rep.At.Format(time.RFC3339), rep.Summary())
	}
	var obs *health.ObservationError
	switch {
	case errors.As(err, &obs):
		fmt.Fprintf(stderr, "factoryd health: %v\n", err)
		return exitConfig
	case err != nil:
		fmt.Fprintf(stderr, "factoryd health: %v\n", err)
		return exitConfig
	case !rep.Healthy:
		return exitUnhealthy
	}
	return exitOK
}

// exitUnhealthy is the health verb's own code: the tick ran and found
// something. It is distinct from exitConfig (the tick could not run) so that
// "unhealthy" and "could not look" are never the same number.
const exitUnhealthy = 1

func runHealth(args []string) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "factory config file")
	loop := fs.Bool("loop", false, "run every health.interval_seconds until stopped")
	asJSON := fs.Bool("json", false, "print the health document instead of a summary")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "factoryd health: --config is required")
		return exitConfig
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd health: %v\n", err)
		return exitConfig
	}
	// A transport that cannot be built is a startup error, before any tick.
	fan, err := alert.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd health: %v\n", err)
		return exitConfig
	}
	deps := health.Deps{Probes: health.HostProbes{}, Alerts: fan, Log: os.Stderr}
	if cfg.Health.UnreviewedSeconds > 0 {
		// The reviewer credential, read-only: the tick never acts on a change.
		tok, err := cfg.Credentials.Reviewer.Resolve()
		if err != nil {
			fmt.Fprintf(os.Stderr, "factoryd health: reviewer credential: %v\n", err)
			return exitConfig
		}
		drv, err := factory.NewDriver(cfg, tok)
		if err != nil {
			fmt.Fprintf(os.Stderr, "factoryd health: %v\n", err)
			return exitConfig
		}
		deps.ListOpen = func(ctx context.Context) ([]scm.Change, error) { return drv.ListOpen(ctx) }
	}

	if !*loop {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return tickOnce(ctx, cfg, deps, *asJSON, os.Stdout, os.Stderr)
	}

	release, err := claimLongRunningService([]*config.Config{cfg}, state.ServiceHealthLoop)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd health: %v\n", err)
		return exitConfig
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	for {
		// Every loop tick reports its own result; this service exits only when
		// it is stopped, so a transient finding does not drop its registration.
		tickOnce(ctx, cfg, deps, *asJSON, os.Stdout, os.Stderr)
		select {
		case <-ctx.Done():
			if err := release(); err != nil {
				fmt.Fprintf(os.Stderr, "factoryd health: %v\n", err)
				return exitError
			}
			return exitOK
		case <-time.After(time.Duration(cfg.Health.IntervalSeconds) * time.Second):
		}
	}
}
