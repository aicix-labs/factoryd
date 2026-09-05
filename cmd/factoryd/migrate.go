package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/state"
)

// runMigrate makes a legacy trust boundary explicit. A v1 verdict handoff was
// writable by the producer, so migration cannot manufacture registry entries
// from those bytes. Likewise, a pre-service-registry state cannot prove that
// old long-running factoryd processes were stopped before a state upgrade.
func runMigrate(args []string) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "factory config file")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if *cfgPath == "" || len(fs.Args()) != 1 {
		fmt.Fprintln(os.Stderr, "usage: factoryd migrate --config <f> <verdict-registry|service-registry>")
		return exitConfig
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd migrate: %v\n", err)
		return exitConfig
	}
	switch fs.Args()[0] {
	case "verdict-registry":
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
	case "service-registry":
		if err := state.MigrateServiceRegistry(cfg.StatePath(), cfg.Name); err != nil {
			fmt.Fprintf(os.Stderr, "factoryd migrate: service registry: %v\n", err)
			return exitConfig
		}
		fmt.Fprintln(os.Stdout, "service registry and state schema ready; the operator attested that every pre-upgrade factoryd process, including supervisors and status/health services, was stopped or restarted")
		return exitOK
	default:
		fmt.Fprintln(os.Stderr, "usage: factoryd migrate --config <f> <verdict-registry|service-registry>")
		return exitConfig
	}
}
