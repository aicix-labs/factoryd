package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/doctor"
)

func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "factory config file")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "factoryd doctor: --config is required")
		return exitConfig
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd doctor: %v\n", err)
		return exitConfig
	}

	r := doctor.Run(context.Background(), cfg, nil)
	fmt.Print(r.String())
	if !r.OK() {
		return exitConfig
	}
	return exitOK
}
