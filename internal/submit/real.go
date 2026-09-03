package submit

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/gittransport"
)

// RepoGit runs local git operations in the submit repository under the
// transport's constructed environment. No network: fetch and push belong to
// the Transport, which guards them.
type RepoGit struct {
	Cfg     *config.Config
	Timeout time.Duration
}

func (g *RepoGit) run(ctx context.Context, args ...string) (string, error) {
	timeout := g.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.Cfg.Paths.SubmitRepo
	cmd.Env = gittransport.Environment(g.Cfg)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func (g *RepoGit) Checkout(ctx context.Context, branch, start string) error {
	// -B: create or reset. A previous submission of the same branch is
	// superseded by the producer's current tree, not merged with it.
	_, err := g.run(ctx, "checkout", "-q", "-B", branch, start)
	return err
}

func (g *RepoGit) Commit(ctx context.Context, msg, authorName, authorEmail string) (string, bool, error) {
	if _, err := g.run(ctx, "add", "-A", "--", "."); err != nil {
		return "", false, err
	}
	status, err := g.run(ctx, "status", "--porcelain")
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(status) == "" {
		return "", false, nil
	}
	ident := fmt.Sprintf("%s <%s>", authorName, authorEmail)
	if _, err := g.run(ctx,
		"-c", "user.name="+authorName, "-c", "user.email="+authorEmail,
		"commit", "-q", "--author="+ident, "-m", msg); err != nil {
		return "", false, err
	}
	sha, err := g.run(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(sha), true, nil
}

func (g *RepoGit) Status(ctx context.Context) ([]string, error) {
	out, err := g.run(ctx, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if len(line) > 3 {
			paths = append(paths, strings.TrimSpace(line[3:]))
		}
	}
	return paths, nil
}

// GateExec runs the gate command as gate.run_as, in the submit repository,
// with exactly the constructed environment. It is the same setuid mechanism
// doctor's probe uses, with supplementary groups dropped, so what the probe
// verified is what the gate runs as.
type GateExec struct{}

func (GateExec) Run(ctx context.Context, cfg *config.Config, exe string, env []string, out io.Writer) (int, error) {
	timeout := time.Duration(cfg.Gate.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Duration(config.DefaultGateTimeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, cfg.Gate.Command[1:]...)
	cmd.Dir = cfg.GateWorkdir()
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second

	if cfg.Gate.RunAs == nil || cfg.Gate.RunAs.User == "" {
		return -1, fmt.Errorf("gate.run_as is empty; the gate must not run as factoryd")
	}
	u, err := user.Lookup(cfg.Gate.RunAs.User)
	if err != nil {
		return -1, fmt.Errorf("gate.run_as user %q: %w", cfg.Gate.RunAs.User, err)
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}

	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("could not start the gate as %s: %w", cfg.Gate.RunAs.User, err)
	}
	err = cmd.Wait()
	exit := cmd.ProcessState.ExitCode()
	if ctx.Err() != nil {
		return exit, fmt.Errorf("the gate exceeded its %s timeout", timeout)
	}
	var ee *exec.ExitError
	if err != nil && !asExitError(err, &ee) {
		return exit, err
	}
	return exit, nil
}

func asExitError(err error, target **exec.ExitError) bool {
	for err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
