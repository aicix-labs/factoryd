// Package doctor verifies that a factory could actually run.
//
// It is not optional and it is not advisory. v1 lost components across host
// migrations -- a producer daemon, a build-cache volume, a prune job -- and
// each absence was discovered only later, by its consequences. doctor asks the
// questions whose answers those absences would have changed.
package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/factory"
	"github.com/aicix-labs/factoryd/internal/scm"
)

// Check is one question and its answer.
type Check struct {
	Name string
	OK   bool
	// Detail is shown whether the check passed or failed, so a passing report
	// still says what was actually verified.
	Detail string
	Err    error
}

// Report is the full set.
type Report struct {
	Checks []Check
}

// Failed returns the checks that did not pass.
func (r Report) Failed() []Check {
	var out []Check
	for _, c := range r.Checks {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

// OK reports whether every check passed.
func (r Report) OK() bool { return len(r.Failed()) == 0 }

// String renders the report for a terminal.
func (r Report) String() string {
	var sb strings.Builder
	for _, c := range r.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(&sb, "%s  %-28s %s\n", mark, c.Name, c.Detail)
		if c.Err != nil {
			fmt.Fprintf(&sb, "      %v\n", c.Err)
		}
	}
	if r.OK() {
		fmt.Fprintf(&sb, "\n%d checks passed\n", len(r.Checks))
	} else {
		var names []string
		for _, c := range r.Failed() {
			names = append(names, c.Name)
		}
		fmt.Fprintf(&sb, "\n%d of %d checks FAILED: %s\n", len(r.Failed()), len(r.Checks), strings.Join(names, ", "))
	}
	return sb.String()
}

// DriverBuilder builds a driver for a role's token. Injectable so doctor's own
// tests do not need a provider.
type DriverBuilder func(cfg *config.Config, token string) (scm.Driver, error)

// Run performs every check. newDriver may be nil, in which case the real
// provider drivers are used.
func Run(ctx context.Context, cfg *config.Config, newDriver DriverBuilder) Report {
	if newDriver == nil {
		newDriver = factory.NewDriver
	}
	var r Report
	add := func(name string, err error, detail string) {
		r.Checks = append(r.Checks, Check{Name: name, OK: err == nil, Detail: detail, Err: err})
	}

	add("config", nil, fmt.Sprintf("%s: factory %q, provider %s, target branch %s",
		cfg.Path(), cfg.Name, cfg.Provider, cfg.TargetBranch))

	// --- paths ---
	add("paths.root", checkWritableDir(cfg.Paths.Root), cfg.Paths.Root)
	for _, d := range []string{cfg.InboxDir(), cfg.OutboxDir()} {
		add("handoff "+filepath.Base(d), checkWritableDir(d), d)
	}

	kind, err := workdirKind(cfg.Paths.ProducerWorkdir)
	add("producer workdir", err, fmt.Sprintf("%s (%s)", cfg.Paths.ProducerWorkdir, kind))

	// --- gate ---
	add("gate command", checkCommand(cfg.Gate.Command, "gate.command",
		"submit would push code nothing has built"), strings.Join(cfg.Gate.Command, " "))

	// --- role turns ---
	for _, role := range []string{"producer", "reviewer"} {
		spec, _ := cfg.RoleSpec(role)
		add("turn "+role, checkCommand(spec.Command, "roles."+role+".command",
			"the supervisor would have no turn to run"), strings.Join(spec.Command, " "))
		add("workdir "+role, checkWritableDir(cfg.TurnWorkdir(role)), cfg.TurnWorkdir(role))

		// The halt sentinel is a stop, not a warning. A supervisor started
		// against one refuses to run, so doctor has to say it is there.
		if body, err := os.ReadFile(cfg.StopPath(role)); err == nil {
			add("halt sentinel "+role,
				fmt.Errorf("%s is present; %s will not start until it is removed:\n      %s",
					cfg.StopPath(role), role, strings.ReplaceAll(strings.TrimSpace(string(body)), "\n", "\n      ")),
				cfg.StopPath(role))
		} else {
			add("halt sentinel "+role, nil, "absent")
		}
	}

	add("spin guard", nil, fmt.Sprintf("warn at %d turns with no progress, halt at %d; backoff %ds",
		cfg.Supervisor.SpinWarn, cfg.Supervisor.SpinAbort, cfg.Supervisor.BackoffSeconds))

	// --- alerts ---
	alertErr := error(nil)
	if len(cfg.Alerts) == 0 {
		alertErr = fmt.Errorf("no alert transport configured; detection with nowhere to report to is not an alert")
	}
	kinds := make([]string, 0, len(cfg.Alerts))
	for _, a := range cfg.Alerts {
		kinds = append(kinds, a.Kind)
	}
	add("alert transports", alertErr, strings.Join(kinds, ", "))

	// --- credentials and identities ---
	ids := map[string]scm.Identity{}
	for _, role := range []struct {
		name string
		ref  config.CredentialRef
	}{
		{"producer", cfg.Credentials.Producer},
		{"reviewer", cfg.Credentials.Reviewer},
	} {
		tok, err := role.ref.Resolve()
		add("credential "+role.name, err, role.ref.Describe())
		if err != nil {
			continue
		}
		d, err := newDriver(cfg, tok)
		if err != nil {
			add("identity "+role.name, err, "")
			continue
		}
		id, err := d.Whoami(ctx)
		add("identity "+role.name, err, id.String())
		if err == nil {
			ids[role.name] = id
			if role.name == "reviewer" {
				_, lerr := d.ListOpen(ctx)
				n := ""
				if lerr == nil {
					n = "repository reachable"
				}
				add("repository", lerr, n)
			}
		}
	}

	// The check the two-party model rests on.
	p, pok := ids["producer"]
	v, vok := ids["reviewer"]
	switch {
	case !pok || !vok:
		add("distinct identities", fmt.Errorf("one or both identities could not be resolved; distinctness is undecided, which is not the same as satisfied"), "")
	case p.ID == v.ID:
		add("distinct identities", fmt.Errorf("producer and reviewer both authenticate as %s; the producer could merge its own work", p), "")
	default:
		add("distinct identities", nil, fmt.Sprintf("producer %s, reviewer %s", p, v))
	}

	return r
}

func checkWritableDir(dir string) error {
	if dir == "" {
		return fmt.Errorf("path is empty")
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	// Statting says it exists; writing says the factory can use it.
	f, err := os.CreateTemp(dir, ".factoryd-doctor-*")
	if err != nil {
		return fmt.Errorf("%s is not writable: %w", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// workdirKind classifies the producer's working directory.
//
// The distinction is load-bearing. A git worktree keeps its refs in the parent
// repository, which lives outside the producer's sandbox, so a producer in a
// worktree cannot commit at all -- and the failure surfaces as a permission
// error deep in a git plumbing command, not as anything naming the cause.
func workdirKind(dir string) (string, error) {
	if dir == "" {
		return "unset", fmt.Errorf("paths.producer_workdir is empty")
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return "missing", err
	}
	if !fi.IsDir() {
		return "not a directory", fmt.Errorf("%s is not a directory", dir)
	}
	if err := checkWritableDir(dir); err != nil {
		return "not writable", err
	}

	gitPath := filepath.Join(dir, ".git")
	gi, err := os.Stat(gitPath)
	if os.IsNotExist(err) {
		return "not a git repository", fmt.Errorf("%s has no .git; the producer has nothing to commit into", dir)
	}
	if err != nil {
		return "unknown", err
	}
	if gi.IsDir() {
		return "clone", nil
	}
	// A regular .git file means a worktree or a submodule; both keep refs
	// elsewhere.
	parent := ""
	if b, err := os.ReadFile(gitPath); err == nil {
		parent = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
	}
	return "worktree", fmt.Errorf(
		"%s is a git worktree, not a clone: .git is a file pointing at %s. Its refs live in the parent repository, outside the producer's sandbox, so the producer cannot commit",
		dir, parent)
}

// checkCommand verifies a configured argv is runnable. A command that does not
// exist fails at the moment it is needed, which for a turn is the moment a
// trigger arrives and for the gate is the moment a change is ready to push.
func checkCommand(argv []string, field, consequence string) error {
	if len(argv) == 0 {
		return fmt.Errorf("%s is empty; %s", field, consequence)
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		return fmt.Errorf("%s: %q not found: %w", field, argv[0], err)
	}
	return nil
}
