package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/factory"
	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/status"
)

type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint([]string(*m)) }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func runStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	var cfgPaths multiFlag
	fs.Var(&cfgPaths, "config", "factory config file (repeatable)")
	serve := fs.String("serve", "", "listen address, e.g. :8080 or 127.0.0.1:8080; empty prints once")
	asJSON := fs.Bool("json", false, "print JSON instead of text (one-shot mode)")
	provider := fs.Bool("provider", true, "ask the provider for open changes (read-only, reviewer credential)")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if len(cfgPaths) == 0 {
		fmt.Fprintln(os.Stderr, "factoryd status: at least one --config is required")
		return exitConfig
	}
	var cs []*status.Collector
	for _, p := range cfgPaths {
		cfg, err := config.Load(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "factoryd status: %v\n", err)
			return exitConfig
		}
		deps := status.Deps{}
		if *provider {
			tok, err := cfg.Credentials.Reviewer.Resolve()
			if err != nil {
				fmt.Fprintf(os.Stderr, "factoryd status: %s: reviewer credential: %v (use --provider=false to run without)\n", cfg.Name, err)
				return exitConfig
			}
			drv, err := factory.NewDriver(cfg, tok)
			if err != nil {
				fmt.Fprintf(os.Stderr, "factoryd status: %v\n", err)
				return exitConfig
			}
			deps.ListOpen = func(ctx context.Context) ([]scm.Change, error) { return drv.ListOpen(ctx) }
		}
		cs = append(cs, status.New(cfg, deps))
	}
	srv, err := status.NewServer(cs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd status: %v\n", err)
		return exitConfig
	}

	if *serve == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		snaps := srv.Snapshots(ctx)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(snaps)
		} else {
			for _, s := range snaps {
				fmt.Print(status.Text(s))
			}
		}
		for _, s := range snaps {
			if !s.Working {
				return exitUnhealthy
			}
		}
		return exitOK
	}

	ln, err := net.Listen("tcp", *serve)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd status: listen %s: %v\n", *serve, err)
		return exitConfig
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdownCtx)
	}()
	fmt.Fprintf(os.Stderr, "factoryd status: serving %d factor%s on http://%s/ (JSON at /status.json)\n",
		len(cs), map[bool]string{true: "y", false: "ies"}[len(cs) == 1], ln.Addr())
	if err := hs.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "factoryd status: %v\n", err)
		return exitError
	}
	return exitOK
}
