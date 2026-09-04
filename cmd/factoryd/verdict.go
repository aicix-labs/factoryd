package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/factory"
	"github.com/aicix-labs/factoryd/internal/principal"
	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/signal"
)

// runVerdict is the operator's verb for a change merged outside factoryd
// (#43): `factoryd verdict --config <f> <id> merged [summary]`. It records
// the merge as a real verdict -- verified at the provider first -- so the
// human's action re-enters the protocol where it left: the outbox wakes
// the producer, the cycle finishes. Nothing polls. The read runs as the
// operator principal when one is configured (proven a third identity, as
// for close), else as the reviewer: a read, not a write, so the reviewer's
// token is enough to look, and the verdict says who recorded it.
func runVerdict(args []string) int {
	fs := flag.NewFlagSet("verdict", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "factory config file")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	rest := fs.Args()
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "factoryd verdict: --config is required")
		return exitConfig
	}
	if len(rest) < 2 || rest[1] != "merged" {
		fmt.Fprintln(os.Stderr, "factoryd verdict: usage: verdict --config <f> <id> merged [summary]  (only merged is recorded by hand; the reviewer records the rest)")
		return exitError
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd verdict: %v\n", err)
		return exitConfig
	}
	ctx := context.Background()
	d, err := verdictDriver(ctx, cfg, factory.NewDriver)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd verdict: %v\n", err)
		return exitConfig
	}
	v, path, err := signal.RecordMerged(ctx, cfg, d, scm.ChangeID(rest[0]), strings.Join(rest[2:], " "), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd verdict: %v\n", err)
		return exitError
	}
	fmt.Printf("recorded merged for %s at %s (by %s); wrote %s; the producer is woken\n", v.ChangeID, v.MergeCommit, v.RecordedBy, path)
	return exitOK
}

// verdictDriver: the operator principal when configured (all three
// identities proven distinct, the driver bound to the validated token),
// else the reviewer's credential for a read.
func verdictDriver(ctx context.Context, cfg *config.Config, newDriver principal.DriverBuilder) (scm.Driver, error) {
	if cfg.Credentials.Operator.File != "" {
		three, err := principal.Resolve(ctx, cfg, newDriver)
		if err != nil {
			return nil, err
		}
		return three.OperatorDriver, nil
	}
	tok, err := cfg.Credentials.Reviewer.Resolve()
	if err != nil {
		return nil, fmt.Errorf("reviewer credential: %w", err)
	}
	return newDriver(cfg, tok)
}
