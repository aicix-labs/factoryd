package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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

	// Both credentials, both fail closed. The producer's drives everything;
	// the reviewer's is resolved only to prove it is someone else.
	producerTok, err := cfg.Credentials.Producer.Resolve()
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd submit: producer credential: %v\n", err)
		return exitConfig
	}
	reviewerTok, err := cfg.Credentials.Reviewer.Resolve()
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd submit: reviewer credential: %v\n", err)
		return exitConfig
	}
	driver, err := factory.NewDriver(cfg, producerTok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd submit: %v\n", err)
		return exitConfig
	}
	producer, err := driver.Whoami(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd submit: producer identity: %v\n", err)
		return exitConfig
	}
	reviewer, err := driver.WhoamiWith(ctx, reviewerTok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd submit: reviewer identity: %v\n", err)
		return exitConfig
	}
	transport, err := gittransport.New(cfg, driver, producerTok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd submit: %v\n", err)
		return exitConfig
	}

	r, err := submit.Run(ctx, cfg, submit.Deps{
		Driver: driver, Transport: transport,
		Git:       &submit.RepoGit{Cfg: cfg},
		Gate:      submit.GateExec{},
		Provision: submit.GateProvisioner{Cfg: cfg},
		Producer:  producer, Reviewer: reviewer,
		Log: os.Stderr,
	})
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
		fmt.Printf("submitted %s as %s: draft %s\n", r.Branch, producer.Login, r.Change.ID)
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
