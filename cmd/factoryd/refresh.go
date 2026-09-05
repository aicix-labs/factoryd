package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/factory"
	"github.com/aicix-labs/factoryd/internal/gittransport"
	"github.com/aicix-labs/factoryd/internal/refresh"
	"github.com/aicix-labs/factoryd/internal/submit"
	"github.com/aicix-labs/factoryd/internal/supervise"
)

// runRefresh is the operator's verb: bring the producer's workdir to the
// target branch now. It refuses while a change is in flight unless forced,
// because the refresh discards the tree the producer is expected to iterate
// on; the reason is printed, never silently skipped.
func runRefresh(args []string) int {
	fs := flag.NewFlagSet("refresh", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "factory config file")
	force := fs.Bool("force", false, "refresh even while a change is in flight")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "factoryd refresh: --config is required")
		return exitConfig
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd refresh: %v\n", err)
		return exitConfig
	}
	ctx := context.Background()
	deps, err := refreshDeps(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd refresh: %v\n", err)
		return exitConfig
	}
	// Decision and apply under one lock, shared with the producer's turn
	// start: a non-forced refresh that raced a turn could wipe its work.
	r, prev, err := refresh.Guarded(ctx, cfg, deps, *force)
	if errors.Is(err, refresh.ErrRefused) {
		fmt.Fprintf(os.Stderr, "factoryd refresh: %v; --force discards the producer's tree\n", err)
		return exitError
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd refresh: %v\n", err)
		return exitError
	}
	if *force {
		fmt.Fprintf(os.Stderr, "factoryd refresh: forced; the previous cycle was %s\n", prev)
	}
	fmt.Printf("%s at %s\n", cfg.Paths.ProducerWorkdir, r.SHA)
	return exitOK
}

// producerBeforeTurn is the refresh step of the producer's loop (#35):
// refresh.BeforeTurn with the real dependencies.
func producerBeforeTurn(cfg *config.Config) func(ctx context.Context, t supervise.Turn) (string, error) {
	return refresh.BeforeTurn(cfg, func(ctx context.Context) (refresh.Deps, error) { return refreshDeps(ctx, cfg) })
}

// producerQueueReady keeps queued briefs behind a live draft, while using the
// same provider reconciliation as a real producer start to release the next
// item after an operator merge.
func producerQueueReady(cfg *config.Config) func(ctx context.Context) (bool, string, error) {
	return refresh.QueueReady(cfg, func(ctx context.Context) (refresh.Deps, error) { return refreshDeps(ctx, cfg) })
}

// refreshDeps: the transport for the fetch (factoryd's credential, factoryd's
// clone), git in the submit repository for the bundle, and this binary's
// _refresh verb started as the producer for the apply.
func refreshDeps(ctx context.Context, cfg *config.Config) (refresh.Deps, error) {
	producerTok, err := cfg.Credentials.Producer.Resolve()
	if err != nil {
		return refresh.Deps{}, fmt.Errorf("producer credential: %w", err)
	}
	driver, err := factory.NewDriver(cfg, producerTok)
	if err != nil {
		return refresh.Deps{}, err
	}
	transport, err := gittransport.New(cfg, driver, producerTok)
	if err != nil {
		return refresh.Deps{}, err
	}
	if err := transport.Guard(); err != nil {
		return refresh.Deps{}, err
	}
	repo := &submit.RepoGit{Cfg: cfg}
	self, err := os.Executable()
	if err != nil {
		return refresh.Deps{}, err
	}
	return refresh.Deps{
		Fetch:    transport.Fetch,
		Bundle:   repo.Bundle,
		Lookup:   driver.Get,        // one read of the cycle's change, for Reconcile (#43)
		Ancestor: driver.IsAncestor, // and its landing on the configured target, verified (#49 review)
		Apply: func(ctx context.Context, bundle, branch string) (string, error) {
			return applyAsProducer(ctx, cfg, self, bundle, branch)
		},
	}, nil
}

// applyAsProducer runs `factoryd _refresh` as the producer identity in the
// producer's workdir. The same switch the turn runner makes: without the
// privilege to switch, and with a producer user configured, it refuses
// rather than writing the producer's tree as factoryd.
func applyAsProducer(ctx context.Context, cfg *config.Config, self, bundle, branch string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	git, err := gittransport.GitBinary(cfg)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, self, "_refresh", git, cfg.Paths.ProducerWorkdir, bundle, branch)
	cmd.Dir = cfg.Paths.ProducerWorkdir
	env := []string{"PATH=" + os.Getenv("PATH")}
	if home, herr := cfg.ProducerHome(); herr == nil && home != "" {
		env = append(env, "HOME="+home)
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	spec, _ := cfg.RoleSpec("producer")
	// The producer's sandbox, exactly as a turn gets it: git in the
	// producer's tree runs sealed or not at all (#41 review).
	if err := supervise.ApplySandbox(spec, cmd.SysProcAttr); err != nil {
		return "", fmt.Errorf("_refresh as the producer: %w", err)
	}
	if spec.RunAs != nil && spec.RunAs.User != "" {
		u, err := user.Lookup(spec.RunAs.User)
		if err != nil {
			return "", fmt.Errorf("producer run_as user %q: %w", spec.RunAs.User, err)
		}
		uid, _ := strconv.ParseUint(u.Uid, 10, 32)
		gid, _ := strconv.ParseUint(u.Gid, 10, 32)
		if !(os.Geteuid() != 0 && int(uid) == os.Geteuid() && int(gid) == os.Getegid()) {
			cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
		}
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("_refresh as the producer: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// runRefreshHelper is the _refresh verb: ApplyLocal in the identity this
// process was started with, printing HEAD.
func runRefreshHelper(args []string) int {
	if len(args) != 4 {
		fmt.Fprintln(os.Stderr, "factoryd _refresh: internal verb: <git> <workdir> <bundle> <branch>")
		return exitError
	}
	sha, err := refresh.ApplyLocal(context.Background(), args[0], args[1], args[2], args[3])
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd _refresh: %v\n", err)
		return exitError
	}
	fmt.Println(sha)
	return exitOK
}
