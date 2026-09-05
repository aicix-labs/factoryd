package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/factory"
	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/signal"
)

// reviewerDriver builds the driver as the reviewer and returns who that is.
// Both verbs here are the reviewer's acts; the producer's credential is
// never consulted, so a misconfiguration cannot make the producer merge.
func reviewerDriver(ctx context.Context, cfg *config.Config) (scm.Driver, scm.Identity, error) {
	tok, err := cfg.Credentials.Reviewer.Resolve()
	if err != nil {
		return nil, scm.Identity{}, fmt.Errorf("reviewer credential: %w", err)
	}
	d, err := factory.NewDriver(cfg, tok)
	if err != nil {
		return nil, scm.Identity{}, err
	}
	who, err := d.Whoami(ctx)
	if err != nil {
		return nil, scm.Identity{}, fmt.Errorf("reviewer identity: %w", err)
	}
	return d, who, nil
}

// runSignal: factoryd signal --config <f> <id> <verdict> <sha|auto> --summary <s>
func runSignal(args []string) int {
	fs := flag.NewFlagSet("signal", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "factory config file")
	summary := fs.String("summary", "", "the verdict, standing alone; the producer may be unable to read the comment")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	rest := fs.Args()
	// Flags may also follow the positionals.
	if len(rest) > 3 {
		if err := fs.Parse(rest[3:]); err != nil {
			return exitError
		}
		rest = rest[:3]
	}
	if *cfgPath == "" || len(rest) != 3 {
		fmt.Fprintln(os.Stderr, "usage: factoryd signal --config <f> <id> <merged|changes-requested|operator-gated> <sha|auto> --summary <s>")
		return exitConfig
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd signal: %v\n", err)
		return exitConfig
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	d, who, err := reviewerDriver(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd signal: %v\n", err)
		return exitConfig
	}
	res, err := signal.Run(ctx, cfg, signal.Deps{Driver: d, Reviewer: who, Log: os.Stderr},
		signal.Request{ID: scm.ChangeID(rest[0]), Kind: rest[1], SHA: rest[2], Summary: *summary})
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd signal: %v\n", err)
		var se *signal.Error
		if errors.As(err, &se) {
			return se.Exit
		}
		return exitConfig
	}
	if res.PipelineWait != nil {
		if res.PipelineBlocked != nil {
			fmt.Printf("waiting for CI on %s at %s exhausted its automatic retry budget; operator attention required: %s\n", res.PipelineWait.ChangeID, res.PipelineWait.SHA, res.PipelineBlocked.Reason)
			return signal.ExitDone
		}
		fmt.Printf("waiting for CI on %s at %s; reviewer retry scheduled: %s\n", res.PipelineWait.ChangeID, res.PipelineWait.SHA, res.PipelineWait.Reason)
		return signal.ExitDone
	}
	fmt.Printf("%s %s at %s", res.Verdict.Kind, res.Verdict.ChangeID, res.Verdict.SHA)
	if res.Verdict.MergeCommit != "" {
		fmt.Printf(" merged as %s (verified)", res.Verdict.MergeCommit)
	}
	fmt.Printf("; wrote %s\n", res.Path)
	return signal.ExitDone
}
