package supervise

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/proc"
	"github.com/aicix-labs/factoryd/internal/state"
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

	// Resolved against the turn's OWN PATH, then executed by absolute path,
	// so what doctor could find and what the turn can find are the same
	// question with the same answer.
	exe, lookErr := config.LookPathIn(spec.Env["PATH"], spec.Command[0])
	if lookErr != nil {
		return TurnResult{ExitCode: -1}, fmt.Errorf("exec: %s turn: %w", r.Role, lookErr)
	}
	cmd := exec.CommandContext(turnCtx, exe, spec.Command[1:]...)
	cmd.Dir = r.Config.TurnWorkdir(r.Role)
	cmd.Stdout = r.Stdout
	cmd.Stderr = r.Stderr
	turnEnv, envErr := r.env(t)
	if envErr != nil {
		return TurnResult{ExitCode: -1}, fmt.Errorf("exec: %s turn: %w", r.Role, envErr)
	}
	cmd.Env = append(turnEnv, r.ExtraEnv...)
	// Give the turn its own process group so that killing it at the deadline
	// takes its children too. A turn that spawns a build and times out would
	// otherwise leave the build running and the next turn racing it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Containment. Privileged factoryd puts the turn in its own cgroup at
	// clone; a process group cannot hold a child that calls setsid, a
	// cgroup can, and cgroup.kill with a verified-empty read is the kill-all
	// the crossing that follows the turn is entitled to. Privileged but
	// unable to create one: refuse, never fall back silently.
	var cg *turnCgroup
	containment := "process-group"
	if os.Geteuid() == 0 {
		var err error
		cg, err = newTurnCgroup(r.Config.Name, r.Role, t.ID)
		if err != nil {
			return TurnResult{ExitCode: -1}, fmt.Errorf("exec: %s turn: %w", r.Role, err)
		}
		defer cg.close()
		cg.attr(cmd.SysProcAttr)
		containment = "cgroup"
	}
	// The sandbox is applied by the supervisor at clone, before the identity
	// switch: a new network namespace is root's to create, and the turn
	// inherits an empty one it cannot leave. Without the privilege, the
	// turn must not start connected instead.
	if err := ApplySandbox(spec, cmd.SysProcAttr); err != nil {
		return TurnResult{ExitCode: -1}, fmt.Errorf("exec: %s turn: %w", r.Role, err)
	}
	// The producer runs as its own OS identity -- the same mechanism doctor's
	// write probe uses, so what the probe verified is what the turn runs as.
	// Without the privilege to switch, the turn must not start as factoryd
	// instead: that would be the separation on paper with nothing holding it.
	if spec.RunAs != nil && spec.RunAs.User != "" {
		cred, err := credentialFor(spec.RunAs.User)
		if err != nil {
			return TurnResult{ExitCode: -1}, fmt.Errorf("exec: %s turn: %w", r.Role, err)
		}
		// A switch to the identity the process already has is not a switch.
		// Setting a Credential would still call setgroups, which an
		// unprivileged process cannot -- so an unprivileged factoryd running
		// a turn as itself would refuse to start for no reason. When there
		// IS a switch, the credential applies in full, groups dropped.
		if !isSelf(cred) {
			cmd.SysProcAttr.Credential = cred
		}
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	// A leader that has exited while children hold its stdio is a leftover
	// turn; a short delay bounds how long that can look like a running one.
	cmd.WaitDelay = 3 * time.Second

	if err := cmd.Start(); err != nil {
		if spec.RunAs != nil && spec.RunAs.User != "" {
			// Name the switch, whatever errno the kernel or a sandbox returned
			// for it: the operator needs to know the turn did not start as
			// factoryd instead.
			return TurnResult{ExitCode: -1}, fmt.Errorf("exec: could not start the %s turn as run_as user %q: %w", r.Role, spec.RunAs.User, err)
		}
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
	res := TurnResult{ExitCode: cmd.ProcessState.ExitCode(), Containment: containment}
	// The leader is gone; is the group? A clean exit with a child still
	// running is not a quiescent producer. Children holding the leader's
	// stdio past the wait delay are that same child seen from the pipes:
	// not a runner fault, and the reaper below is what reports it.
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	if survivors := reapGroup(cmd.Process.Pid); survivors {
		res.Leftover = true
	}
	if cg != nil {
		// Whatever the group said, the cgroup is the authority: anything
		// left in it, in any session, is killed and verified gone before
		// this returns. A cgroup that will not empty is a runner error --
		// nothing may follow a turn whose processes are still alive.
		n, kerr := cg.killAll()
		if kerr != nil {
			return res, fmt.Errorf("exec: %s turn: containment: %w", r.Role, kerr)
		}
		if n > 0 {
			res.Leftover = true
		}
	}
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

// reapGroup reports whether any process remained in the group led by pid
// after the leader exited, and kills the group if so: TERM, a grace, KILL.
func reapGroup(pid int) bool {
	if syscall.Kill(-pid, 0) != nil {
		return false // nobody left
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(-pid, 0) != nil {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	return true
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
// VerdictEnv is one entry of FACTORYD_VERDICTS: the verdict document that
// woke the turn, with the path it was read from. A turn may carry several
// verdict triggers at once; a scalar could name only one family, and a
// producer that re-declared the wrong one would get an unrelated draft
// (#29, second round). So every verdict is mapped exactly, and the scalar
// keys are filled only when there is exactly one.
type VerdictEnv struct {
	Path           string `json:"path"`
	ChangeID       string `json:"change_id"`
	Kind           string `json:"kind"`
	SHA            string `json:"sha"`
	Branch         string `json:"branch"`
	DeclaredBranch string `json:"declared_branch"`
}

func (r *ExecRunner) env(t Turn) ([]string, error) {
	var trigPaths []string
	for _, tr := range t.Triggers {
		trigPaths = append(trigPaths, tr.Path)
	}
	factoryd := map[string]string{
		"FACTORYD_FACTORY":       r.Config.Name,
		"FACTORYD_ROLE":          r.Role,
		"FACTORYD_TURN":          t.ID,
		"FACTORYD_ROOT":          r.Config.Paths.Root,
		"FACTORYD_INBOX":         r.Config.InboxDir(),
		"FACTORYD_OUTBOX":        r.Config.OutboxDir(),
		"FACTORYD_WORKDIR":       r.Config.TurnWorkdir(r.Role),
		"FACTORYD_TARGET_BRANCH": r.Config.TargetBranch,
		"FACTORYD_PROGRESS":      r.Config.ProgressPath(r.Role),
		"FACTORYD_TRIGGERS":      labels(t.Triggers),
		"FACTORYD_TRIGGER_PATHS": strings.Join(trigPaths, string(os.PathListSeparator)),
		// The config this turn was started from, so a turn that drives
		// factoryd's own verbs (the reviewer: scm, audit, signal) does not
		// have to be told by hand (canary issue #22).
		"FACTORYD_CONFIG": r.Config.Path(),
		// Verdicts (#29): FACTORYD_VERDICTS is an exact JSON mapping of
		// EVERY verdict trigger; the scalar keys name the one verdict when
		// the turn carries exactly one, and are empty otherwise -- never
		// the first of several. All keys are always present, so the
		// generated set is exact.
		"FACTORYD_VERDICTS":      "[]",
		"FACTORYD_VERDICT":       "",
		"FACTORYD_CHANGE_ID":     "",
		"FACTORYD_CHANGE_BRANCH": "",
	}
	var verdicts []VerdictEnv
	for _, tr := range t.Triggers {
		if tr.Label != "verdict" {
			continue
		}
		v, err := state.ReadVerdictFile(tr.Path)
		if err != nil {
			// A verdict the runner cannot read is not a turn to start with
			// empty values: the agent would act on the trigger without the
			// facts it carries. It is a runner error, said as such.
			return nil, fmt.Errorf("verdict trigger %s: %w", tr.Path, err)
		}
		verdicts = append(verdicts, VerdictEnv{Path: tr.Path, ChangeID: v.ChangeID, Kind: v.Kind, SHA: v.SHA, Branch: v.Branch, DeclaredBranch: v.DeclaredBranch})
	}
	if len(verdicts) > 0 {
		b, err := json.Marshal(verdicts)
		if err != nil {
			return nil, err
		}
		factoryd["FACTORYD_VERDICTS"] = string(b)
	}
	if len(verdicts) == 1 {
		factoryd["FACTORYD_VERDICT"] = verdicts[0].Kind
		factoryd["FACTORYD_CHANGE_ID"] = verdicts[0].ChangeID
		factoryd["FACTORYD_CHANGE_BRANCH"] = verdicts[0].DeclaredBranch
	}
	// Constructed, never inherited: os.Environ() is consulted for exactly the
	// reviewer's credential name and nothing else (Config.TurnEnv).
	return r.Config.TurnEnv(r.Role, factoryd, os.Environ()), nil
}

// credentialFor resolves an OS user to the credential the turn is started
// with. Resolved at launch, not at load, so a user deleted since doctor ran
// fails here by name.
// ApplySandbox applies a role's declared sandbox to a process about to be
// started as that role: the turn command, and anything else factoryd runs
// as the producer in the producer's tree -- the refresh helper (#41
// review). One function, so a second path cannot start connected while
// the first is sealed. Without the privilege it refuses, never falls back.
func ApplySandbox(spec config.RoleSpec, attr *syscall.SysProcAttr) error {
	if spec.Sandbox != nil && spec.Sandbox.NoNetwork {
		if err := applyNoNetwork(attr); err != nil {
			return fmt.Errorf("sandbox.no_network: %w", err)
		}
	}
	return nil
}

// CredentialFor is exported for its test; it is not part of the runner's API.
func CredentialFor(name string) (*syscall.Credential, error) { return credentialFor(name) }

func credentialFor(name string) (*syscall.Credential, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("run_as user %q: %w", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, err
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, err
	}
	// Groups is set to an explicitly empty list, not left nil with NoSetGroups.
	// NoSetGroups keeps the PARENT's supplementary groups -- factoryd's, which
	// is root's -- so a "producer" turn would still be in group root, and a
	// 775 root:root .git is writable by it. Found by doctor's live probe: the
	// setuid child could write what sudo -u could not.
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}, nil
}

// isSelf reports whether cred names the identity this process already runs
// as. Root is never "self" for this purpose: root switching to root still
// wants its supplementary groups dropped, and can.
func isSelf(cred *syscall.Credential) bool {
	return os.Geteuid() != 0 && int(cred.Uid) == os.Geteuid() && int(cred.Gid) == os.Getegid()
}
