// Package doctor verifies that a factory could actually run.
//
// It is not optional and it is not advisory. v1 lost components across host
// migrations -- a producer daemon, a build-cache volume, a prune job -- and
// each absence was discovered only later, by its consequences. doctor asks the
// questions whose answers those absences would have changed.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"github.com/aicix-labs/factoryd/internal/alert"
	"github.com/aicix-labs/factoryd/internal/health"
	"github.com/aicix-labs/factoryd/internal/supervise"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/factory"
	"github.com/aicix-labs/factoryd/internal/gittransport"
	"github.com/aicix-labs/factoryd/internal/principal"
	"github.com/aicix-labs/factoryd/internal/proc"
	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/state"
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
	// Contain proves a turn can be contained on this host: a per-turn cgroup
	// created, killed and removed. Nil uses the real probe.
	Contain func() error
	// Reach probes whether a turn of spec can reach addr, sandboxed or not.
	// Nil uses the real runner with this binary's own _netprobe verb.
	Reach func(ctx context.Context, spec config.RoleSpec, addr string, sandboxed bool) (bool, error)
	// ProcessExecutable resolves the executable path of a recorded live
	// factoryd process. Nil reads /proc through proc.Ref.Executable.
	ProcessExecutable func(proc.Ref) (string, error)
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
	if deps.Reach == nil {
		deps.Reach = realReach
	}
	if deps.Contain == nil {
		deps.Contain = realContain
	}
	if deps.ProcessExecutable == nil {
		deps.ProcessExecutable = func(ref proc.Ref) (string, error) { return ref.Executable() }
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
		// The inbox is the producer's to write: it consumes its brief there
		// and records progress there (§6.2, §6.3). Probed as the producer,
		// because factoryd being able to write it says nothing -- found in
		// the acceptance run, where a root-owned inbox made every producer
		// turn read as "no progress" while the turn itself exited clean.
		// Both handoff directories are the producer's to write: it consumes
		// its brief and records progress in the inbox, and consumes the
		// verdicts and answers that arrive in the outbox. Probed as the
		// producer, because factoryd being able to write them says nothing.
		// Found in the acceptance run twice: a root-owned inbox made every
		// producer turn read as "no progress" while exiting clean, and a
		// root-owned outbox left every verdict unconsumed.
		for _, d := range []string{cfg.InboxDir(), cfg.OutboxDir()} {
			name := "handoff " + filepath.Base(d) + " as producer"
			if can, err := prober.CanWrite(ctx, d); err != nil {
				add(name, fmt.Errorf("undecided: %v", err), d)
			} else if !can {
				add(name, fmt.Errorf("%s cannot write %s; it could not consume what arrives there, and a trigger that is never consumed re-runs the turn forever", prober.Describe(), d), d)
			} else {
				add(name, nil, prober.Describe()+" can write "+d)
			}
		}
		// The factory root holds state.json and its lock: the supervisor's
		// own record, where the verdict registry and the attempt counts
		// live. The producer receives FACTORYD_ROOT and writes inbox/ and
		// outbox/ beneath it; it must not be able to write the root itself
		// (create, delete or replace state.json) nor state.json (#50
		// review). Probed as the producer; the inbox write above is the
		// control that this prober can write where it should.
		if can, err := prober.CanWrite(ctx, cfg.Paths.Root); err != nil {
			add("producer cannot write the factory root", fmt.Errorf("undecided: %v", err), cfg.Paths.Root)
		} else if can {
			add("producer cannot write the factory root", fmt.Errorf("%s CAN write %s; it could replace state.json and reset every bound the supervisor keeps there", prober.Describe(), cfg.Paths.Root), cfg.Paths.Root)
		} else {
			add("producer cannot write the factory root", nil, prober.Describe()+" cannot write "+cfg.Paths.Root)
		}
		if _, err := os.Lstat(cfg.StatePath()); err == nil {
			if can, err := prober.CanWriteFile(ctx, cfg.StatePath()); err != nil {
				add("producer cannot write state.json", fmt.Errorf("undecided: %v", err), cfg.StatePath())
			} else if can {
				add("producer cannot write state.json", fmt.Errorf("%s CAN write %s; the supervisor's record is the producer's to edit", prober.Describe(), cfg.StatePath()), cfg.StatePath())
			} else {
				add("producer cannot write state.json", nil, prober.Describe()+" cannot write "+cfg.StatePath())
			}
		}
		// The producer receives no credential (§4.4). Probed as the producer:
		// a reviewer token it can read is a token a networkless producer can
		// copy into ordinary source and have submit push for it. The control
		// is a file in its own workdir, which it must be able to read --
		// otherwise "cannot read" is a broken probe.
		for _, cr := range []struct{ name, file string }{{"producer credential", cfg.Credentials.Producer.File}, {"reviewer credential", cfg.Credentials.Reviewer.File}, {"operator credential", cfg.Credentials.Operator.File}} {
			if cr.file == "" {
				continue
			}
			can, err := prober.CanRead(ctx, cr.file)
			switch {
			case err != nil:
				add("producer cannot read "+cr.name, fmt.Errorf("undecided: %v", err), cr.file)
			case can:
				add("producer cannot read "+cr.name, fmt.Errorf("%s CAN read %s; a producer that can read a credential can push it out through its own submit", prober.Describe(), cr.file), cr.file)
			default:
				add("producer cannot read "+cr.name, nil, prober.Describe()+" cannot read "+cr.file)
			}
		}
		// The control is printed whether it passes or fails: a control that
		// is silent when it passes cannot be audited (canary review note).
		if canOwn, err := prober.CanRead(ctx, producerDir); err != nil || !canOwn {
			add("producer read probe control", fmt.Errorf("%s cannot read its own workdir %s (%v); the credential probes above prove nothing", prober.Describe(), producerDir, err), producerDir)
		} else {
			add("producer read probe control", nil, prober.Describe()+" can read "+producerDir+", so the two refusals above are refusals")
		}
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
	// The operator principal is a boundary only if the reviewer identity
	// cannot read it: a reviewer that can read the operator's token can
	// close a draft, the act the automated protocol is kept out of (#47
	// review). Probed AS the reviewer, with the control that the reviewer
	// can read its own credential -- otherwise "cannot read" is a broken
	// probe. Only when an operator credential and a reviewer identity are
	// configured; a reviewer running as factoryd itself is not a boundary
	// either, and is reported as such.
	if of := cfg.Credentials.Operator.File; of != "" {
		switch {
		case cfg.Roles.Reviewer.RunAs == nil || cfg.Roles.Reviewer.RunAs.User == "":
			add("reviewer cannot read operator credential", fmt.Errorf("roles.reviewer.run_as is unset: the reviewer runs as factoryd itself and can read %s; the operator principal is no boundary", of), of)
		default:
			rev, rerr := deps.NewProber(cfg.Roles.Reviewer.RunAs)
			if rerr != nil {
				add("reviewer cannot read operator credential", fmt.Errorf("undecided: %v", rerr), of)
				break
			}
			if can, err := rev.CanRead(ctx, of); err != nil {
				add("reviewer cannot read operator credential", fmt.Errorf("undecided: %v", err), of)
			} else if can {
				add("reviewer cannot read operator credential", fmt.Errorf("%s CAN read %s; the automated reviewer could close drafts as the operator", rev.Describe(), of), of)
			} else {
				add("reviewer cannot read operator credential", nil, rev.Describe()+" cannot read "+of)
			}
			if rf := cfg.Credentials.Reviewer.File; rf != "" {
				if can, err := rev.CanRead(ctx, rf); err != nil || !can {
					add("reviewer read probe control", fmt.Errorf("%s cannot read its own credential %s (%v); the refusal above proves nothing", rev.Describe(), rf, err), rf)
				} else {
					add("reviewer read probe control", nil, rev.Describe()+" can read "+rf+", so the refusal above is a refusal")
				}
			}
		}
	}

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
			{"reviewer", cfg.Credentials.Reviewer.File}, {"producer", cfg.Credentials.Producer.File}, {"operator", cfg.Credentials.Operator.File},
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
		// The producer's HOME holds whatever model credential its agent CLI
		// keeps (codex: ~/.codex/auth.json). The gate runs producer-authored
		// code; it must not be able to read that either. "The home happens
		// to be 0700" is not evidence -- the canary found codex creating
		// .codex as 0775 beside a 0600 auth.json -- so this is probed as the
		// gate, like the two provider credentials are.
		// Probed for TRAVERSAL, not reading: a 0711 home is neither readable
		// nor listable and passes a read probe, while producer-authored gate
		// code that knows the path $HOME/.codex/auth.json reads it all the
		// same. Search permission is the exposure.
		if home, herr := cfg.ProducerHome(); herr != nil {
			// Never probed against the wrong directory: a relative home is a
			// failure, not a skipped check.
			add("producer home", herr, cfg.Roles.Producer.Env["HOME"])
		} else if home != "" {
			if can, err := gate.CanTraverse(ctx, home); err != nil {
				add("gate cannot traverse producer home", fmt.Errorf("undecided: %v", err), home)
			} else if can {
				add("gate cannot traverse producer home", fmt.Errorf("%s CAN traverse %s; producer-authored build code that knows a path under it -- its own model credential -- can read it", gate.Describe(), home), home)
			} else {
				add("gate cannot traverse producer home", nil, home)
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
		// The shipped wrapper is an interpreter-level command: resolving its
		// path alone says nothing about the utilities it resolves later from
		// the role's deliberately constructed PATH. Probe each utility as the
		// identity that will run it, not as doctor (#51).
		for _, tools := range turnEntrypointTools(spec.Command) {
			exe, err := resolveTool(spec.Env["PATH"], tools)
			if err != nil {
				// The PATH-only check below records the missing utility. There is
				// no pathname to permission-probe here.
				continue
			}
			tool := strings.Join(tools, " or ")
			if can, err := who.CanExec(ctx, exe); err != nil {
				add(role+" can run turn-entrypoint tool "+tool, fmt.Errorf("undecided: %v", err), exe)
			} else if !can {
				add(role+" can run turn-entrypoint tool "+tool, fmt.Errorf("%s cannot execute %s; doctor could, which is a different question", who.Describe(), exe), exe)
			} else {
				add(role+" can run turn-entrypoint tool "+tool, nil, exe)
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
		if spec.Sandbox != nil && spec.Sandbox.NoNetwork {
			// Proved from inside, against a listener doctor itself opens:
			// sandboxed, the turn must NOT reach it; unsandboxed, the same
			// probe MUST -- otherwise "cannot reach" is a broken probe, not a
			// sandbox. A factoryd without the privilege to create the
			// namespace fails here, because the turn would otherwise start
			// connected and nothing would say so.
			add("sandbox "+role, checkNoNetwork(ctx, deps.Reach, spec), "no_network: the turn cannot reach a listener on this host")
		}
		add("turn "+role, checkCommand(spec.Env["PATH"], spec.Command, "roles."+role+".command",
			"the supervisor would have no turn to run"), strings.Join(spec.Command, " "))
		if tools := turnEntrypointTools(spec.Command); len(tools) > 0 {
			add("turn "+role+" entrypoint tools", checkTools(spec.Env["PATH"], tools, "roles."+role+".command entrypoint"),
				"resolved from roles."+role+".env.PATH: "+toolNames(tools))
		}
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

	add("spin guard", nil, fmt.Sprintf("warn at %d turns with no progress, halt at %d; backoff %ds; CI wait retries at most %d turns or %ds before operator escalation",
		cfg.Supervisor.SpinWarn, cfg.Supervisor.SpinAbort, cfg.Supervisor.BackoffSeconds,
		cfg.Supervisor.PipelineAttempts, cfg.Supervisor.PipelineTimeoutSeconds))
	if cfg.Queue.ContinueWhileGated {
		add("brief queue policy", nil, "continues after an exact operator-gated verdict; two open changes may conflict when merged")
	} else {
		add("brief queue policy", nil, "waits behind every open draft (default conservative policy)")
	}

	// An install replaces the path's inode but not a process that already has
	// it mapped. systemd then truthfully says the old process is active, while
	// every command it serves comes from code no longer on disk. state holds an
	// exact process handle, so inspect that process rather than searching an
	// argv or trusting a service manager (#53). This includes the long-running
	// status and health services: they too keep the old inode mapped after an
	// install, even though they do not supervise an agent role.
	if st, err := state.Load(cfg.StatePath(), cfg.Name); err != nil {
		add("factoryd process binaries", fmt.Errorf("cannot inspect recorded factoryd processes: %w", err), cfg.StatePath())
	} else {
		if err := st.ServiceRegistry.MigrationError(); err != nil {
			add("service registry", fmt.Errorf("%v; stop or restart every pre-registry factoryd process, including producer/reviewer supervisors and `factoryd status --serve`/`factoryd health --loop`, then run `factoryd migrate --config %s service-registry` as the operator", err, cfg.Path()), "long-running service inventory is intentionally unknown")
		} else {
			add("service registry", nil, "complete exact-handle inventory")
		}
		checkRecordedBinary := func(name string, ref *proc.Ref, absent, restart string) {
			if ref == nil {
				add(name, nil, absent)
				return
			}
			path, err := deps.ProcessExecutable(*ref)
			switch {
			case errors.Is(err, proc.ErrNotRunning):
				// status and health report a dead process. Doctor's question here
				// is narrower: whether a live one is executing a replaced binary.
				// Do not call no running process a stale executable.
				add(name, nil, ref.String()+" is not live; no executable to compare")
			case err != nil:
				add(name, fmt.Errorf("cannot inspect %s: %w", ref, err), "")
			case strings.HasSuffix(path, " (deleted)"):
				add(name, fmt.Errorf("%s is executing %s; its on-disk binary was replaced. Restart %s before trusting its output", ref, path, restart), path)
			default:
				add(name, nil, path)
			}
		}
		for _, role := range state.Roles {
			name := "supervisor " + string(role) + " binary"
			checkRecordedBinary(name, st.Role(role).Supervisor, "no supervisor recorded", "the "+string(role)+" supervisor/service")
		}
		for _, service := range state.Services {
			name := "service " + string(service) + " binary"
			checkRecordedBinary(name, st.Service(service), "no service recorded", "the "+string(service)+" service")
		}
	}

	// --- containment ---
	// A turn's processes are held in a per-turn cgroup, killed as a whole
	// and verified gone before anything follows the turn. A process group
	// cannot do that -- setsid(2) leaves it -- so a host on which the
	// cgroup cannot be made is a host on which a clean turn is not a
	// quiescent producer, and doctor says so rather than letting the
	// weaker containment pass for the stronger.
	add("containment", deps.Contain(), "per-turn cgroup: created, killed as a whole, verified empty, removed")

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
	// The operator is a third PROVIDER principal, or it is no boundary: two
	// files holding one token are two paths to one authority, and the
	// unreadability probe above cannot tell (#47 review). Resolved here, in
	// doctor's own context, by stable id; and again immediately before
	// every close.
	if o := cfg.Credentials.Operator; o.File != "" || o.Env != "" {
		three, err := principal.Resolve(ctx, cfg, principal.DriverBuilder(newDriver))
		switch {
		case err != nil:
			add("operator identity", err, o.Describe())
		case three.Operator == nil:
			add("operator identity", fmt.Errorf("undecided: the operator credential resolved to no identity"), o.Describe())
		default:
			add("operator identity", nil, fmt.Sprintf("operator %s, distinct from producer %s and reviewer %s by provider id", *three.Operator, three.Producer, three.Reviewer))
		}
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

// turnEntrypointTools is the external-command contract for the shipped turn
// scripts. Keep each list in step with its script's startup check. Both
// entrypoints are normally absolute while these commands deliberately resolve
// from roles.<role>.env.PATH; checking only argv[0] made doctor green for a
// turn that immediately refused to run (#51).
//
// Each element is a preference-ordered set of alternatives. The producer
// entrypoint selects sha256sum when it is present and falls back to cksum, so
// doctor makes the same selection rather than refusing a cksum-only host or
// accepting a non-executable preferred hasher.
var wrapperRuntimeTools = requiredTools("setsid", "sh", "stat", "grep", "mktemp", "cat", "rm", "sleep")

var producerAgentRuntimeTools = append(
	append([][]string{}, wrapperRuntimeTools...),
	append(requiredTools("dirname", "cut", "cp", "find", "sort", "xargs", "sed", "date", "mv", "touch"), []string{"sha256sum", "cksum"})...,
)

func requiredTools(names ...string) [][]string {
	tools := make([][]string, 0, len(names))
	for _, name := range names {
		tools = append(tools, []string{name})
	}
	return tools
}

func turnEntrypointTools(argv []string) [][]string {
	if len(argv) == 0 {
		return nil
	}
	switch filepath.Base(argv[0]) {
	case "turn-wrapper.sh":
		return wrapperRuntimeTools
	case "producer-turn-agent.sh":
		return producerAgentRuntimeTools
	default:
		return nil
	}
}

func toolNames(requirements [][]string) string {
	parts := make([]string, 0, len(requirements))
	for _, tools := range requirements {
		parts = append(parts, strings.Join(tools, " or "))
	}
	return strings.Join(parts, ", ")
}

func resolveTool(declaredPath string, alternatives []string) (string, error) {
	var errs []string
	for _, tool := range alternatives {
		exe, err := config.LookPathIn(declaredPath, tool)
		if err == nil {
			return exe, nil
		}
		errs = append(errs, err.Error())
	}
	return "", fmt.Errorf("none of %s resolves on the declared PATH: %s", strings.Join(alternatives, " or "), strings.Join(errs, "; "))
}

func checkTools(declaredPath string, requirements [][]string, field string) error {
	for _, alternatives := range requirements {
		if _, err := resolveTool(declaredPath, alternatives); err != nil {
			return fmt.Errorf("%s requires %s: %w", field, strings.Join(alternatives, " or "), err)
		}
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

// checkCacheRoot opens and verifies the cache root exactly as reclamation
// will: no-follow, owned by this uid, writable by nobody else, bound to the
// inode that was verified. It establishes the environment at doctor time
// only; reclamation repeats it at every tick, because that is the moment a
// deletion happens.
func checkCacheRoot(dir string) error {
	cr, err := health.OpenCacheRoot(dir)
	if err != nil {
		return err
	}
	return cr.Close()
}

func checkNoNetwork(ctx context.Context, reach func(context.Context, config.RoleSpec, string, bool) (bool, error), spec config.RoleSpec) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("could not open a probe listener: %w", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	addr := ln.Addr().String()
	open, err := reach(ctx, spec, addr, false)
	if err != nil {
		return fmt.Errorf("control probe did not run: %w", err)
	}
	if !open {
		return fmt.Errorf("the unsandboxed control probe could not reach %s; the probe is broken, so nothing here is proved", addr)
	}
	closed, err := reach(ctx, spec, addr, true)
	if err != nil {
		return fmt.Errorf("sandbox cannot be applied: %w", err)
	}
	if closed {
		return fmt.Errorf("the sandboxed turn reached %s; no_network is not holding", addr)
	}
	return nil
}

func realReach(ctx context.Context, spec config.RoleSpec, addr string, sandboxed bool) (bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	return supervise.ProbeReach(ctx, spec, exe, addr, sandboxed)
}

func realContain() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("doctor is not root; a per-turn cgroup cannot be created, and a turn would be held by a process group only, which a setsid'd child leaves")
	}
	return supervise.ProbeContainment()
}
