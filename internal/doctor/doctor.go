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
	"github.com/aicix-labs/factoryd/internal/alert"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/factory"
	"github.com/aicix-labs/factoryd/internal/gittransport"
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

// Deps are the pieces of doctor that touch the outside world, injectable so
// the package can be tested without a provider, privilege, or git remote.
// Nil fields mean the real thing.
type Deps struct {
	NewDriver DriverBuilder
	// NewProber builds the write probe for the producer's OS identity.
	NewProber func(*config.RunAs) (Prober, error)
	// GitIdentity resolves who git would push as, using the producer's
	// driver and secret. Nil uses the real transport.
	GitIdentity func(ctx context.Context, cfg *config.Config, d scm.Driver, secret string) (string, error)
	// GitGuard runs the transport's pre-operation guard on submit_repo.
	GitGuard func(cfg *config.Config, d scm.Driver, secret string) error
}

// Run performs every check with the real dependencies.
func Run(ctx context.Context, cfg *config.Config, newDriver DriverBuilder) Report {
	return RunWith(ctx, cfg, Deps{NewDriver: newDriver})
}

// RunWith performs every check with the given dependencies.
func RunWith(ctx context.Context, cfg *config.Config, deps Deps) Report {
	newDriver := deps.NewDriver
	if newDriver == nil {
		newDriver = factory.NewDriver
	}
	if deps.NewProber == nil {
		deps.NewProber = NewProber
	}
	if deps.GitIdentity == nil {
		deps.GitIdentity = realGitIdentity
	}
	if deps.GitGuard == nil {
		deps.GitGuard = realGitGuard
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

	if err := checkWritableDir(cfg.Paths.ProducerWorkdir); err != nil {
		add("producer workdir", err, cfg.Paths.ProducerWorkdir)
	} else {
		add("producer workdir", nil, cfg.Paths.ProducerWorkdir)
	}
	kind, err := workdirKind(cfg.Paths.SubmitRepo)
	add("submit repo", err, fmt.Sprintf("%s (%s)", cfg.Paths.SubmitRepo, kind))
	// Owned by factoryd's own identity, not merely unwritable by the producer.
	// Found live: with factoryd running as root (which switching to the
	// producer's uid requires) and the clone owned by someone else, git refuses
	// the repository outright -- and the honest reading of that refusal is
	// that the repository is not ours, not that git should be told to trust it.
	add("submit repo owner", checkOwnedByMe(cfg.Paths.SubmitRepo), cfg.Paths.SubmitRepo)

	// --- the boundary, by probe ---
	// "The producer cannot write submit_repo" is established by trying, as
	// the producer, and having it fail -- with the same probe succeeding
	// against the producer's own workdir, or "cannot write" is just a probe
	// that cannot write anything.
	prober, perr := deps.NewProber(cfg.Roles.Producer.RunAs)
	if perr != nil {
		add("producer identity", perr, "")
		add("boundary", fmt.Errorf("undecided: no producer identity to probe as"), "")
	} else {
		add("producer identity", nil, prober.Describe())
		// The control probes the directory the turn will ACTUALLY run in --
		// roles.producer.workdir may override paths.producer_workdir, and a
		// green against the default says nothing about the override.
		producerDir := cfg.TurnWorkdir("producer")
		canSubmit, err1 := prober.CanWrite(ctx, cfg.Paths.SubmitRepo)
		canWork, err2 := prober.CanWrite(ctx, producerDir)
		switch {
		case err1 != nil:
			add("boundary", fmt.Errorf("undecided, which is not the same as satisfied: %v", err1), "")
		case err2 != nil:
			add("boundary", fmt.Errorf("the control probe could not run: %v", err2), "")
		case canSubmit:
			add("boundary", fmt.Errorf("%s CAN write %s; the submit repository is not separated from the producer", prober.Describe(), cfg.Paths.SubmitRepo), "")
		case !canWork:
			add("boundary", fmt.Errorf("%s cannot write its own workdir %s either; the probe proves nothing", prober.Describe(), producerDir), "")
		default:
			add("boundary", nil, fmt.Sprintf("%s: refused at %s, allowed at %s", prober.Describe(), cfg.Paths.SubmitRepo, producerDir))
		}
	}

	// --- the gate's identity, by probe ---
	// The gate runs producer-authored code. It must be able to write its
	// declared paths and must not be able to write .git or read a credential.
	// Each is probed as the gate user; each has its own control.
	gate, gerr := deps.NewProber(cfg.Gate.RunAs)
	if gerr != nil {
		add("gate identity", gerr, "")
	} else {
		add("gate identity", nil, gate.Describe())
		gitDir := filepath.Join(cfg.Paths.SubmitRepo, ".git")
		if can, err := gate.CanWrite(ctx, gitDir); err != nil {
			add("gate cannot touch .git", fmt.Errorf("undecided: %v", err), "")
		} else if can {
			add("gate cannot touch .git", fmt.Errorf("%s CAN write %s; producer-authored code run by the gate could plant a hook that runs during push", gate.Describe(), gitDir), "")
		} else {
			add("gate cannot touch .git", nil, gitDir)
		}
		for _, cred := range []struct{ name, file string }{
			{"reviewer", cfg.Credentials.Reviewer.File}, {"producer", cfg.Credentials.Producer.File},
		} {
			if cred.file == "" {
				continue
			}
			if can, err := gate.CanRead(ctx, cred.file); err != nil {
				add("gate cannot read "+cred.name+" credential", fmt.Errorf("undecided: %v", err), cred.file)
			} else if can {
				add("gate cannot read "+cred.name+" credential", fmt.Errorf("%s CAN read %s; the two-party model is in the gate's hands", gate.Describe(), cred.file), cred.file)
			} else {
				add("gate cannot read "+cred.name+" credential", nil, cred.file)
			}
		}
		provisioned := 0
		for _, p := range cfg.Gate.RequiredWritablePaths {
			resolved, err := cfg.ResolveGatePath(p)
			if err != nil {
				continue // reported under "gate path" below
			}
			// A declared path is a capability grant, so it is judged after
			// PHYSICAL resolution -- a symlink can make "cache" land on .git --
			// and refused if it is .git, inside it, or above it.
			if err := physicalPathMayNotReachGit(cfg, p, resolved); err != nil {
				add("gate can write "+p, err, resolved)
				continue
			}
			// Created here as submit would create it: owned by the gate, so the
			// gate can write it, inside a repository the gate cannot otherwise
			// touch. Ownership is applied only when doctor has the privilege
			// to apply it; otherwise the probe reports the truth of the host.
			if err := os.MkdirAll(resolved, 0o755); err != nil {
				add("gate can write "+p, err, resolved)
				continue
			}
			if err := gate.Own(resolved); err != nil {
				add("gate can write "+p, fmt.Errorf("could not give %s to the gate: %w", resolved, err), resolved)
				continue
			}
			provisioned++
			if can, err := gate.CanWrite(ctx, resolved); err != nil {
				add("gate can write "+p, fmt.Errorf("undecided: %v", err), resolved)
			} else if !can {
				add("gate can write "+p, fmt.Errorf("%s cannot write %s, which the gate declared it needs", gate.Describe(), resolved), resolved)
			} else {
				add("gate can write "+p, nil, resolved)
			}
		}
		// Re-probe .git AFTER provisioning. The earlier probe described the
		// repository before any path was created or given away; this one
		// describes the environment the gate will actually run in.
		if provisioned > 0 {
			if can, err := gate.CanWrite(ctx, gitDir); err != nil {
				add("gate cannot touch .git after provisioning", fmt.Errorf("undecided: %v", err), "")
			} else if can {
				add("gate cannot touch .git after provisioning", fmt.Errorf("%s CAN write %s once the declared paths exist; a declared path granted the gate reach into .git", gate.Describe(), gitDir), "")
			} else {
				add("gate cannot touch .git after provisioning", nil, gitDir)
			}
		}

		// Executability by the identity that will run the command, not by
		// doctor. A root-owned 0700 binary passes an execute-bit check and
		// fails the moment the child switches uid.
		if exe, err := config.LookPathIn(cfg.Gate.Env["PATH"], cfg.Gate.Command[0]); err == nil {
			if can, err := gate.CanExec(ctx, exe); err != nil {
				add("gate can run its command", fmt.Errorf("undecided: %v", err), exe)
			} else if !can {
				add("gate can run its command", fmt.Errorf("%s cannot execute %s; doctor could, which is a different question", gate.Describe(), exe), exe)
			} else {
				add("gate can run its command", nil, exe)
			}
		}
	}
	// --- every role that runs as its own identity ---
	// The runner applies run_as to either role and honours a workdir override,
	// so doctor verifies the actual command and the actual workdir under each
	// role's configured identity. A role without run_as runs as factoryd, and
	// what factoryd can do is what doctor already established.
	for _, role := range []string{"producer", "reviewer"} {
		spec, _ := cfg.RoleSpec(role)
		if spec.RunAs == nil || spec.RunAs.User == "" {
			continue
		}
		var who Prober
		if role == "producer" && perr == nil {
			who = prober
		} else {
			w, err := deps.NewProber(spec.RunAs)
			if err != nil {
				add(role+" identity", err, "")
				continue
			}
			who = w
			add(role+" identity", nil, who.Describe())
		}
		if who == nil {
			continue
		}
		dir := cfg.TurnWorkdir(role)
		if can, err := who.CanWrite(ctx, dir); err != nil {
			add(role+" can write its workdir", fmt.Errorf("undecided: %v", err), dir)
		} else if !can {
			add(role+" can write its workdir", fmt.Errorf("%s cannot write %s, where its turns run; every turn would fail on start", who.Describe(), dir), dir)
		} else {
			add(role+" can write its workdir", nil, dir)
		}
		if exe, err := config.LookPathIn(spec.Env["PATH"], spec.Command[0]); err == nil {
			if can, err := who.CanExec(ctx, exe); err != nil {
				add(role+" can run its command", fmt.Errorf("undecided: %v", err), exe)
			} else if !can {
				add(role+" can run its command", fmt.Errorf("%s cannot execute %s; doctor could, which is a different question", who.Describe(), exe), exe)
			} else {
				add(role+" can run its command", nil, exe)
			}
		}
	}

	// --- gate environment and paths ---
	if _, ok := cfg.Gate.Env["PATH"]; !ok {
		add("gate env", fmt.Errorf("gate.env declares no PATH"), "")
	} else {
		add("gate env", nil, fmt.Sprintf("%d variables declared, nothing inherited", len(cfg.Gate.Env)))
	}
	for _, p := range cfg.Gate.RequiredWritablePaths {
		resolved, err := cfg.ResolveGatePath(p)
		if err != nil {
			add("gate path "+p, err, "")
			continue
		}
		add("gate path "+p, checkWritableOrCreatable(resolved), resolved)
	}

	// --- gate ---
	// Resolved against the DECLARED PATH, not doctor's own. Doctor's shell can
	// find go where the gate's environment cannot, and then a green doctor
	// vouches for a gate that fails on its first run.
	add("gate command", checkCommand(cfg.Gate.Env["PATH"], cfg.Gate.Command, "gate.command",
		"submit would push code nothing has built"), strings.Join(cfg.Gate.Command, " "))

	// --- role turns ---
	for _, role := range []string{"producer", "reviewer"} {
		spec, _ := cfg.RoleSpec(role)
		add("turn "+role, checkCommand(spec.Env["PATH"], spec.Command, "roles."+role+".command",
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
	// Delivered, not inspected: a probe alert goes through every transport
	// now, because "the file path looks writable" and "the command exists"
	// are not the same as an alert arriving. What would have to break for
	// this to go red is exactly what breaks when a real alert is lost.
	if len(cfg.Alerts) == 0 {
		add("alert transports", fmt.Errorf("no alert transport configured; detection with nowhere to report to is not an alert"), "")
	} else if fan, err := alert.New(cfg); err != nil {
		add("alert transports", err, "")
	} else {
		probe := alert.Alert{Factory: cfg.Name, At: time.Now(), Kind: "doctor", Severity: "probe",
			Summary: "factoryd doctor delivery probe; if you can read this, the transport works"}
		ds, _ := fan.Deliver(ctx, probe)
		for _, d := range ds {
			kind, _, _ := strings.Cut(d.Transport, " ")
			add("alert "+kind, d.Err, d.Transport+": probe alert delivered")
		}
	}
	if cr := cfg.Paths.CacheRoot; cr != "" {
		add("cache root", checkCacheRoot(cr), cr)
	}
	add("health thresholds", nil, fmt.Sprintf("alert after %d ticks, repeat every %ds; stale trigger %ds, turn grace %ds, disk headroom %d%%, %d bounded cache(s)",
		cfg.Health.AlertAfter, cfg.Health.RepeatSeconds, cfg.Health.StaleTriggerSeconds, cfg.Health.TurnGraceSeconds, cfg.Health.DiskMinFreePercent, len(cfg.Health.Caches)))

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

	// --- the git binary: the one on the declared PATH, by absolute path ---
	if exe, err := gittransport.GitBinary(cfg); err != nil {
		add("git binary", err, "")
	} else {
		add("git binary", nil, exe)
	}

	// --- the git transport: same identity, owned target ---
	if tok, err := cfg.Credentials.Producer.Resolve(); err == nil {
		if d, err := newDriver(cfg, tok); err == nil {
			add("git config allowlist", deps.GitGuard(cfg, d, tok), cfg.Paths.SubmitRepo)
			login, err := deps.GitIdentity(ctx, cfg, d, tok)
			switch {
			case err != nil:
				add("git identity", fmt.Errorf("undecided: %v", err), "")
			case ids["producer"].Login == "":
				add("git identity", fmt.Errorf("git would push as %q but the producer's API identity could not be resolved to compare", login), "")
			case login != ids["producer"].Login:
				add("git identity", fmt.Errorf("git would push as %q, but the producer's API identity is %q; the two mechanisms disagree", login, ids["producer"].Login), "")
			default:
				add("git identity", nil, fmt.Sprintf("git pushes as %s, the same identity the API sees", login))
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
func checkCommand(declaredPath string, argv []string, field, consequence string) error {
	if len(argv) == 0 {
		return fmt.Errorf("%s is empty; %s", field, consequence)
	}
	if _, err := config.LookPathIn(declaredPath, argv[0]); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}

// checkWritableOrCreatable accepts a writable directory, or an absent path
// whose nearest existing ancestor is writable -- submit creates it. A path
// that exists as a non-directory, or whose ancestor is unwritable, fails.
func checkWritableOrCreatable(p string) error {
	fi, err := os.Stat(p)
	switch {
	case err == nil && !fi.IsDir():
		return fmt.Errorf("%s exists and is not a directory", p)
	case err == nil:
		return checkWritableDir(p)
	case !os.IsNotExist(err):
		return err
	}
	// Absent: walk up to the nearest existing ancestor.
	for dir := filepath.Dir(p); ; dir = filepath.Dir(dir) {
		if fi, err := os.Stat(dir); err == nil {
			if !fi.IsDir() {
				return fmt.Errorf("%s: ancestor %s is not a directory", p, dir)
			}
			if err := checkWritableDir(dir); err != nil {
				return fmt.Errorf("%s is absent and cannot be created: %w", p, err)
			}
			return nil
		}
		if dir == filepath.Dir(dir) {
			return fmt.Errorf("%s: no existing ancestor", p)
		}
	}
}

func realGitIdentity(ctx context.Context, cfg *config.Config, d scm.Driver, secret string) (string, error) {
	tr, err := gittransport.New(cfg, d, secret)
	if err != nil {
		return "", err
	}
	id, err := tr.Identity(ctx)
	if err != nil {
		return "", err
	}
	return id.Login, nil
}

func realGitGuard(cfg *config.Config, d scm.Driver, secret string) error {
	tr, err := gittransport.New(cfg, d, secret)
	if err != nil {
		return err
	}
	return tr.Guard()
}

// checkOwnedByMe requires the path to be owned by the identity doctor runs as,
// which is the identity every git command will run as.
func checkOwnedByMe(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot read the owner of %s on this platform", path)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d but factoryd runs as uid %d; git will refuse a repository it does not own, and it should -- the submit repository must be factoryd's", path, st.Uid, os.Geteuid())
	}
	return nil
}

// physicalPathMayNotReachGit resolves the nearest existing ancestor of resolved
// through any symlinks and repeats the ancestor/descendant check against .git
// on the result. The lexical check in config cannot see a symlink.
func physicalPathMayNotReachGit(cfg *config.Config, declared, resolved string) error {
	// Both sides of the comparison go through the same resolution. Resolving
	// only the declared path put the two in different namespaces: with
	// submit_repo itself a symlink, "cache -> .git" resolved to
	// /real/repo/.git while the target was /link/repo/.git, the guard missed,
	// and Own followed the link and chowned .git before the re-probe could
	// object -- a boundary permanently broken by the check meant to keep it.
	gitDir, err := canonical(filepath.Join(cfg.Paths.SubmitRepo, ".git"))
	if err != nil {
		return fmt.Errorf("resolving the submit repository's .git: %w", err)
	}
	physical, err := canonical(resolved)
	if err != nil {
		return fmt.Errorf("path %q: %w", declared, err)
	}
	switch {
	case physical == gitDir:
		return fmt.Errorf("path %q resolves to the submit repository's .git; the gate may never own it", declared)
	case strings.HasPrefix(physical, gitDir+string(os.PathSeparator)):
		return fmt.Errorf("path %q resolves inside the submit repository's .git; the gate may never own it", declared)
	case strings.HasPrefix(gitDir, physical+string(os.PathSeparator)):
		return fmt.Errorf("path %q resolves to an ancestor of the submit repository's .git (%s); owning it would let the gate rename, delete or replace .git", declared, physical)
	}
	return nil
}

// canonical resolves p through every symlink in its existing prefix and
// re-appends the part that does not exist yet.
func canonical(p string) (string, error) {
	rest := ""
	probe := filepath.Clean(p)
	for {
		if _, err := os.Lstat(probe); err == nil {
			break
		}
		rest = filepath.Join(filepath.Base(probe), rest)
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	real, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Join(real, rest)), nil
}

// checkCacheRoot: the one directory reclamation may delete in must exist,
// be a real directory (not a symlink to somewhere else), be owned by the
// user factoryd runs as, and not be writable by anyone else -- a group- or
// world-writable cache root lets any local user plant a symlink for the
// next tick to follow, and the physical check at reclamation is the last
// line, not the only one.
func checkCacheRoot(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("is a symlink; the cache root must be a real directory")
	}
	if !fi.IsDir() {
		return fmt.Errorf("is not a directory")
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("is writable by group or others (%v); anyone could plant a symlink for reclamation to follow", fi.Mode().Perm())
	}
	if uid, ok := ownerUID(fi); ok && uid != os.Getuid() {
		return fmt.Errorf("is owned by uid %d, not the uid factoryd runs as (%d)", uid, os.Getuid())
	}
	return nil
}

func ownerUID(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
