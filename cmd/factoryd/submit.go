package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/aicix-labs/factoryd/internal/supervise"
	"os"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/factory"
	"github.com/aicix-labs/factoryd/internal/gittransport"
	"github.com/aicix-labs/factoryd/internal/submit"
)

func runSubmit(args []string) int {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "factory config file")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "factoryd submit: --config is required")
		return exitConfig
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd submit: %v\n", err)
		return exitConfig
	}
	ctx := context.Background()

	deps, err := submitDeps(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd submit: %v\n", err)
		return exitConfig
	}
	r, err := submit.Run(ctx, cfg, deps)
	if err != nil {
		var se *submit.Error
		if errors.As(err, &se) {
			fmt.Fprintf(os.Stderr, "factoryd submit: %v\n", se)
			return se.Exit
		}
		fmt.Fprintf(os.Stderr, "factoryd submit: %v\n", err)
		return exitError
	}
	switch r.Exit {
	case submit.ExitSubmitted:
		fmt.Printf("submitted %s as %s: draft %s\n", r.Branch, deps.Producer.Login, r.Change.ID)
		if r.Change.WebURL != "" {
			fmt.Println(r.Change.WebURL)
		}
	case submit.ExitNothing:
		fmt.Println(r.Reason)
		for _, p := range r.DirtyPaths {
			fmt.Printf("  left uncommitted: %s\n", p)
		}
	case submit.ExitGateRed:
		fmt.Println(r.Reason)
	}
	return r.Exit
}

// submitDeps builds everything submit needs, both credentials fail closed.
// The producer's drives everything; the reviewer's is resolved only to
// prove it is someone else. Shared by the verb and by the producer
// supervisor's after-turn step, so the two cannot drift.
func submitDeps(ctx context.Context, cfg *config.Config) (submit.Deps, error) {
	producerTok, err := cfg.Credentials.Producer.Resolve()
	if err != nil {
		return submit.Deps{}, fmt.Errorf("producer credential: %w", err)
	}
	reviewerTok, err := cfg.Credentials.Reviewer.Resolve()
	if err != nil {
		return submit.Deps{}, fmt.Errorf("reviewer credential: %w", err)
	}
	driver, err := factory.NewDriver(cfg, producerTok)
	if err != nil {
		return submit.Deps{}, err
	}
	producer, err := driver.Whoami(ctx)
	if err != nil {
		return submit.Deps{}, fmt.Errorf("producer identity: %w", err)
	}
	reviewer, err := driver.WhoamiWith(ctx, reviewerTok)
	if err != nil {
		return submit.Deps{}, fmt.Errorf("reviewer identity: %w", err)
	}
	transport, err := gittransport.New(cfg, driver, producerTok)
	if err != nil {
		return submit.Deps{}, err
	}
	return submit.Deps{
		Driver: driver, Transport: transport,
		Git:       &submit.RepoGit{Cfg: cfg},
		Gate:      submit.GateExec{},
		Provision: submit.GateProvisioner{Cfg: cfg},
		Producer:  producer, Reviewer: reviewer,
		Log: os.Stderr,
	}, nil
}

// producerAfterTurn is the SUBMIT step of the §3 loop: after a producer turn
// that declared intent, submit runs outside the sandbox, as factoryd. A
// turn that declared nothing is left alone (exit 4 is not a failure); a
// red gate has already written its question and woken the reviewer (exit
// 5, not a failure either); only a configuration or identity failure (3)
// is the supervisor's to count.
func producerAfterTurn(cfg *config.Config) func(ctx context.Context, t supervise.Turn, res supervise.TurnResult) (string, error) {
	return func(ctx context.Context, t supervise.Turn, res supervise.TurnResult) (string, error) {
		// The same reader submit uses, so a declaration submit would refuse --
		// one file without the other, a link, a fifo -- is a failed after-turn
		// here too, not "no intent" and a consumed trigger over stranded work.
		if _, err := submit.ReadIntent(cfg.TurnWorkdir("producer")); err != nil {
			if errors.Is(err, submit.ErrNoIntent) {
				return "no intent declared; nothing to submit", nil
			}
			return "", fmt.Errorf("submit: the producer's intent is not a valid declaration: %w", err)
		}
		deps, err := submitDeps(ctx, cfg)
		if err != nil {
			return "", fmt.Errorf("submit: %w", err)
		}
		r, err := submit.Run(ctx, cfg, deps)
		if err != nil {
			return "", fmt.Errorf("submit: %w", err)
		}
		switch r.Exit {
		case submit.ExitSubmitted:
			return fmt.Sprintf("submitted %s as draft %s", r.Branch, r.Change.ID), nil
		case submit.ExitGateRed:
			return "gate red: " + r.Reason, nil
		default:
			return r.Reason, nil
		}
	}
}
