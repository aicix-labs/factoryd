package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/state"
)

// runMigrate is deliberately narrow. A v1 verdict handoff was writable by the
// producer, so migration cannot manufacture registry entries from those bytes.
// It archives the unsafe files and makes the v2 registry ready for a reviewer
// or operator to issue a current verdict again.
func runMigrate(args []string) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "factory config file")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if *cfgPath == "" || len(fs.Args()) != 1 || fs.Args()[0] != "verdict-registry" {
		fmt.Fprintln(os.Stderr, "usage: factoryd migrate --config <f> verdict-registry")
		return exitConfig
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd migrate: %v\n", err)
		return exitConfig
	}
	moved, err := state.MigrateVerdictRegistry(cfg.StatePath(), cfg.Name, cfg.OutboxDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd migrate: verdict registry: %v\n", err)
		return exitConfig
	}
	for _, path := range moved {
		fmt.Printf("quarantined untrusted legacy verdict: %s\n", path)
	}
	fmt.Fprintln(os.Stdout, "verdict registry ready; reissue any still-current verdict with factoryd signal or factoryd verdict")
	return exitOK
}
