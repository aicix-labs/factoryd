package main

import (
	"bytes"
	"context"
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
	"github.com/aicix-labs/factoryd/internal/state"
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
	st, err := state.Load(cfg.StatePath(), cfg.Name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd refresh: %v\n", err)
		return exitConfig
	}
	if busy, why := refresh.InFlight(st); busy && !*force {
		fmt.Fprintf(os.Stderr, "factoryd refresh: refused: %s; --force discards the producer's tree\n", why)
		return exitError
	}
	deps, err := refreshDeps(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd refresh: %v\n", err)
		return exitConfig
	}
	r, err := refresh.Run(ctx, cfg, deps)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd refresh: %v\n", err)
		return exitError
	}
	fmt.Printf("%s at %s\n", cfg.Paths.ProducerWorkdir, r.SHA)
	return exitOK
}

// producerBeforeTurn is the refresh step of the producer's loop: before a
// turn, when no change of the producer's is in flight, the workdir is
// brought to the target branch. A change in flight is logged and left
// alone; a refresh that fails is a failed turn (#35).
func producerBeforeTurn(cfg *config.Config) func(ctx context.Context, t supervise.Turn) (string, error) {
	return func(ctx context.Context, t supervise.Turn) (string, error) {
		st, err := state.Load(cfg.StatePath(), cfg.Name)
		if err != nil {
			return "", fmt.Errorf("refresh: %w", err)
		}
		if busy, why := refresh.InFlight(st); busy {
			return "workdir not refreshed: " + why, nil
		}
		deps, err := refreshDeps(ctx, cfg)
		if err != nil {
			return "", fmt.Errorf("refresh: %w", err)
		}
		r, err := refresh.Run(ctx, cfg, deps)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("workdir refreshed to %s at %s", cfg.TargetBranch, r.SHA), nil
	}
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
		Fetch:  transport.Fetch,
		Bundle: repo.Bundle,
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
	if spec, ok := cfg.RoleSpec("producer"); ok && spec.RunAs != nil && spec.RunAs.User != "" {
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
