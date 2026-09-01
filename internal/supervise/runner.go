package supervise

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/proc"
)

// ExecRunner runs an agent turn as a subprocess.
//
// The turn is one shot. It is handed its trigger through the environment and
// through the filesystem, does one unit of work, and exits. Nothing here loops:
// in v1 each agent was told to "run forever" and reliably did not.
type ExecRunner struct {
	Config *config.Config
	Role   string
	Stdout io.Writer
	Stderr io.Writer
	// ExtraEnv is appended after the factory's own variables.
	ExtraEnv []string
}

var _ Runner = (*ExecRunner)(nil)

// Run executes one turn.
func (r *ExecRunner) Run(ctx context.Context, t Turn, started func(proc.Ref)) (TurnResult, error) {
	spec, ok := r.Config.RoleSpec(r.Role)
	if !ok {
		return TurnResult{ExitCode: -1}, fmt.Errorf("exec: role %q is not producer or reviewer", r.Role)
	}
	if len(spec.Command) == 0 {
		return TurnResult{ExitCode: -1}, fmt.Errorf("exec: roles.%s.command is empty", r.Role)
	}

	turnCtx := ctx
	var cancel context.CancelFunc
	if !t.Deadline.IsZero() {
		turnCtx, cancel = context.WithDeadline(ctx, t.Deadline)
		defer cancel()
	}

	cmd := exec.CommandContext(turnCtx, spec.Command[0], spec.Command[1:]...)
	cmd.Dir = r.Config.TurnWorkdir(r.Role)
	cmd.Stdout = r.Stdout
	cmd.Stderr = r.Stderr
	cmd.Env = append(r.env(t), r.ExtraEnv...)
	// Give the turn its own process group so that killing it at the deadline
	// takes its children too. A turn that spawns a build and times out would
	// otherwise leave the build running and the next turn racing it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second

	if err := cmd.Start(); err != nil {
		return TurnResult{ExitCode: -1}, fmt.Errorf("exec: starting the %s turn: %w", r.Role, err)
	}
	if started != nil {
		// Take the handle from the PID we were just given, not by searching for
		// a matching command line.
		if ref, err := proc.Take(cmd.Process.Pid, r.Role, strings.Join(spec.Command, " ")); err == nil {
			started(ref)
		}
	}

	err := cmd.Wait()
	res := TurnResult{ExitCode: cmd.ProcessState.ExitCode()}
	// A deadline that fired is not the same as an agent that failed, even
	// though both surface as a non-zero exit.
	if turnCtx.Err() != nil && ctx.Err() == nil {
		res.TimedOut = true
	}
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			// A non-zero exit is an ordinary outcome for an agent turn, not a
			// supervisor fault. The spin guard decides what it means.
			return res, nil
		}
		return res, err
	}
	return res, nil
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

// env is what a turn is told about its world. Everything an agent needs to find
// its trigger and report progress is here, so a turn never has to know the
// factory's layout.
func (r *ExecRunner) env(t Turn) []string {
	var trigPaths []string
	for _, tr := range t.Triggers {
		trigPaths = append(trigPaths, tr.Path)
	}
	return append(os.Environ(),
		"FACTORYD_FACTORY="+r.Config.Name,
		"FACTORYD_ROLE="+r.Role,
		"FACTORYD_TURN="+t.ID,
		"FACTORYD_ROOT="+r.Config.Paths.Root,
		"FACTORYD_INBOX="+r.Config.InboxDir(),
		"FACTORYD_OUTBOX="+r.Config.OutboxDir(),
		"FACTORYD_WORKDIR="+r.Config.TurnWorkdir(r.Role),
		"FACTORYD_TARGET_BRANCH="+r.Config.TargetBranch,
		"FACTORYD_PROGRESS="+r.Config.ProgressPath(r.Role),
		"FACTORYD_TRIGGERS="+labels(t.Triggers),
		"FACTORYD_TRIGGER_PATHS="+strings.Join(trigPaths, string(os.PathListSeparator)),
	)
}
