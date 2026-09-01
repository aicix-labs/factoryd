package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/supervise"
)

func runSupervise(args []string) int {
	fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "factory config file")
	role := fs.String("role", "", "producer or reviewer")
	maxTurns := fs.Int("max-turns", 0, "stop after N turns (0 = run until stopped)")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if *cfgPath == "" || *role == "" {
		fmt.Fprintln(os.Stderr, "factoryd supervise: --config and --role are required")
		return exitConfig
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd supervise: %v\n", err)
		return exitConfig
	}
	if _, ok := cfg.RoleSpec(*role); !ok {
		fmt.Fprintf(os.Stderr, "factoryd supervise: --role %q is not producer or reviewer\n", *role)
		return exitConfig
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	s, err := supervise.New(supervise.Options{
		Config: cfg,
		Role:   *role,
		Runner: &supervise.ExecRunner{
			Config: cfg, Role: *role, Stdout: os.Stdout, Stderr: os.Stderr,
		},
		Log:      log,
		MaxTurns: *maxTurns,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd supervise: %v\n", err)
		return exitConfig
	}
	defer func() { _ = s.Close() }()

	// SIGINT and SIGTERM stop the loop between turns. The running turn is a
	// child in its own process group and is cancelled with the context, so a
	// stop never needs a pattern-matching kill.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := s.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "factoryd supervise: %v\n", err)
		switch {
		case errors.Is(err, supervise.ErrHalted),
			errors.Is(err, supervise.ErrStopSentinel),
			errors.Is(err, supervise.ErrAlreadyRunning):
			// A halt must exit non-zero, or an init system reads it as a clean
			// shutdown and the operator is never told.
			return exitConfig
		}
		return exitError
	}
	return exitOK
}
