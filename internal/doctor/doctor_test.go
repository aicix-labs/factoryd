package doctor_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/doctor"
	"github.com/aicix-labs/factoryd/internal/scm"
)

// fakeDriver resolves to whatever identity its token names, so a test can make
// the two roles the same or different by changing one file.
type fakeDriver struct {
	scm.Driver
	token   string
	listErr error
}

func (f fakeDriver) Provider() string { return "fake" }
func (f fakeDriver) Whoami(context.Context) (scm.Identity, error) {
	if f.token == "" {
		return scm.Identity{}, errors.New("empty token")
	}
	return scm.Identity{ID: "id-" + f.token, Login: f.token}, nil
}
func (f fakeDriver) ListOpen(context.Context) ([]scm.Change, error) { return nil, f.listErr }

func builder(listErr error) doctor.DriverBuilder {
	return func(_ *config.Config, token string) (scm.Driver, error) {
		return fakeDriver{token: token, listErr: listErr}, nil
	}
}

// fakeProber answers the boundary question from a table: which directories
// the "producer" can write. Never touches privilege.
type fakeProber struct {
	name        string
	writable    map[string]bool
	readable    map[string]bool
	traversable map[string]bool
	rootOnly    map[string]bool
	err         error
}

func (f fakeProber) Describe() string {
	if f.name == "" {
		return "fake-producer (uid 4242)"
	}
	return f.name
}
func (f fakeProber) CanWrite(_ context.Context, dir string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.writable[dir], nil
}
func (f fakeProber) Own(string) error { return nil }

// CanExec: everything is executable except paths the test marked root-only.
func (f fakeProber) CanExec(_ context.Context, path string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return !f.rootOnly[path], nil
}
func (f fakeProber) CanTraverse(_ context.Context, path string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.traversable[path], nil
}

func (f fakeProber) CanRead(_ context.Context, path string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.readable[path], nil
}

// healthyDeps wires fakes for a healthy factory: the producer can write its
// workdir and not the submit repo, git pushes as the producer, the local
// config is clean. Tests mutate what they need.
func healthyDeps(cfg *config.Config, listErr error) doctor.Deps {
	return doctor.Deps{
		NewDriver: builder(listErr),
		// One fake serves both principals, keyed on the run_as user: the
		// producer may write only its workdir; the gate may write the declared
		// paths and nothing else, and may read no credential.
		Contain: func() error { return nil },
		NewProber: func(ra *config.RunAs) (doctor.Prober, error) {
			if ra != nil && ra.User == "reads-reviewer-token" {
				return fakeProber{writable: map[string]bool{cfg.Paths.ProducerWorkdir: true, cfg.InboxDir(): true, cfg.OutboxDir(): true},
					readable: map[string]bool{cfg.Paths.ProducerWorkdir: true, cfg.Credentials.Reviewer.File: true}}, nil
			}
			if ra != nil && ra.User == "reads-producer-token" {
				return fakeProber{writable: map[string]bool{cfg.Paths.ProducerWorkdir: true, cfg.InboxDir(): true, cfg.OutboxDir(): true},
					readable: map[string]bool{cfg.Paths.ProducerWorkdir: true, cfg.Credentials.Producer.File: true}}, nil
			}
			if ra != nil && ra.User == "reads-nothing" {
				return fakeProber{writable: map[string]bool{cfg.Paths.ProducerWorkdir: true, cfg.InboxDir(): true, cfg.OutboxDir(): true}, readable: map[string]bool{}}, nil
			}
			// The reviewer principal: reads its own credential, never the
			// operator's -- unless the test says so.
			if ra != nil && ra.User == "factoryd-reviewer" {
				return fakeProber{name: "fake-reviewer (uid 4444)", writable: map[string]bool{cfg.TurnWorkdir("reviewer"): true}, readable: map[string]bool{cfg.Credentials.Reviewer.File: true}}, nil
			}
			if ra != nil && ra.User == "reviewer-reads-operator-token" {
				return fakeProber{name: "fake-reviewer (uid 4444)", writable: map[string]bool{cfg.TurnWorkdir("reviewer"): true}, readable: map[string]bool{cfg.Credentials.Reviewer.File: true, cfg.Credentials.Operator.File: true}}, nil
			}
			if ra != nil && ra.User == "reviewer-reads-nothing" {
				return fakeProber{name: "fake-reviewer (uid 4444)", writable: map[string]bool{cfg.TurnWorkdir("reviewer"): true}, readable: map[string]bool{}}, nil
			}
			if ra != nil && ra.User == "inbox-locked" {
				return fakeProber{writable: map[string]bool{cfg.Paths.ProducerWorkdir: true, cfg.OutboxDir(): true}, readable: map[string]bool{cfg.Paths.ProducerWorkdir: true}}, nil
			}
			if ra != nil && ra.User == "outbox-locked" {
				return fakeProber{writable: map[string]bool{cfg.Paths.ProducerWorkdir: true, cfg.InboxDir(): true}, readable: map[string]bool{cfg.Paths.ProducerWorkdir: true}}, nil
			}
			if ra != nil && ra.User == "gate-traverses-home" {
				w := map[string]bool{}
				for _, p := range cfg.Gate.RequiredWritablePaths {
					if r, err := cfg.ResolveGatePath(p); err == nil {
						w[r] = true
					}
				}
				// readable: false (0711 is not readable); traversable: true.
				return fakeProber{name: "fake-gate (uid 4343)", writable: w, readable: map[string]bool{}, traversable: map[string]bool{cfg.Roles.Producer.Env["HOME"]: true}}, nil
			}
			if ra != nil && ra.User == "factoryd-gate" {
				w := map[string]bool{}
				for _, p := range cfg.Gate.RequiredWritablePaths {
					if r, err := cfg.ResolveGatePath(p); err == nil {
						w[r] = true
					}
				}
				return fakeProber{name: "fake-gate (uid 4343)", writable: w}, nil
			}
			return fakeProber{writable: map[string]bool{cfg.Paths.ProducerWorkdir: true, cfg.InboxDir(): true, cfg.OutboxDir(): true}, readable: map[string]bool{cfg.Paths.ProducerWorkdir: true}}, nil
		},
		GitIdentity: func(_ context.Context, _ *config.Config, _ scm.Driver, secret string) (string, error) {
			return secret, nil // the fake driver's login IS the token
		},
		GitGuard: func(*config.Config, scm.Driver, string) error { return nil },
	}
}

// fixture writes a complete, healthy factory and returns its config.
func fixture(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"inbox", "outbox", "work", "submit"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Clones: .git is a directory.
	for _, d := range []string{"work", "submit"} {
		if err := os.MkdirAll(filepath.Join(root, d, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeToken(t, filepath.Join(root, "producer.token"), "producer-bot")
	writeToken(t, filepath.Join(root, "reviewer.token"), "factory-reviewer")

	gate, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("no /bin/true available: %v", err)
	}

	cfg := &config.Config{
		SchemaVersion: config.SchemaVersion, Scope: config.EmptyScope(), Health: config.DefaultHealth(),
		Name:         "widgets",
		Provider:     "github",
		GitHub:       &config.GitHub{Owner: "acme", Repo: "widgets"},
		TargetBranch: "main",
		Git:          config.Git{Remote: "https://github.com/acme/widgets.git", Transport: "https"},
		Paths: config.Paths{
			Root:            root,
			ProducerWorkdir: filepath.Join(root, "work"),
			SubmitRepo:      filepath.Join(root, "submit"),
		},
		Credentials: config.Credentials{
			Producer: config.CredentialRef{File: filepath.Join(root, "producer.token")},
			Reviewer: config.CredentialRef{File: filepath.Join(root, "reviewer.token")},
		},
		Gate: config.Gate{Command: []string{gate}, Env: map[string]string{"PATH": "/usr/bin:/bin"},
			RunAs: &config.RunAs{User: "factoryd-gate"}, RequiredWritablePaths: []string{"build/out"}},
		Roles: config.Roles{
			Producer: config.RoleSpec{Command: []string{gate}, Env: map[string]string{"PATH": os.Getenv("PATH"), "HOME": filepath.Join(root, "producer-home")}, RunAs: &config.RunAs{User: "nobody"}},
			Reviewer: config.RoleSpec{Command: []string{gate}, Env: map[string]string{"PATH": os.Getenv("PATH")}},
		},
		Supervisor: config.Supervisor{
			SpinWarn: config.DefaultSpinWarn, SpinAbort: config.DefaultSpinAbort,
			FailAbort:           config.DefaultFailAbort,
			PollIntervalSeconds: config.DefaultPollInterval,
			BackoffSeconds:      config.DefaultBackoffSeconds,
		},
		Alerts: []config.Alert{{Kind: "file", Path: filepath.Join(root, "alerts.log")}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func writeToken(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func failedNames(r doctor.Report) []string {
	var out []string
	for _, c := range r.Failed() {
		out = append(out, c.Name)
	}
	return out
}

// TestHealthyFactoryPasses is the positive control for every case below. If a
// healthy factory does not pass, a failure elsewhere proves nothing.
func TestHealthyFactoryPasses(t *testing.T) {
	cfg := fixture(t)
	r := doctor.RunWith(context.Background(), cfg, healthyDeps(cfg, nil))
	if !r.OK() {
		t.Fatalf("healthy factory failed %v\n%s", failedNames(r), r)
	}
	if len(r.Checks) < 12 {
		t.Fatalf("only %d checks ran; doctor is not asking enough questions", len(r.Checks))
	}
}

// The check the two-party model rests on: one credential file for both roles.
func TestSharedIdentityIsCaught(t *testing.T) {
	cfg := fixture(t)
	writeToken(t, cfg.Credentials.Producer.File, "factory-reviewer")

	r := doctor.RunWith(context.Background(), cfg, healthyDeps(cfg, nil))
	if r.OK() {
		t.Fatal("doctor passed a factory whose producer and reviewer are the same identity")
	}
	if !strings.Contains(r.String(), "distinct identities") {
		t.Fatalf("failure does not name the distinct-identities check:\n%s", r)
	}
}

// A worktree is the specific shape that makes a sandboxed producer unable to
// commit, and it is indistinguishable from a clone by casual inspection.
func TestWorktreeWorkdirIsCaught(t *testing.T) {
	cfg := fixture(t)
	gitPath := filepath.Join(cfg.Paths.SubmitRepo, ".git")
	if err := os.RemoveAll(gitPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitPath, []byte("gitdir: /elsewhere/.git/worktrees/w\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := doctor.RunWith(context.Background(), cfg, healthyDeps(cfg, nil))
	if r.OK() {
		t.Fatal("doctor passed a submit repository that is a worktree, not a clone")
	}
	if !strings.Contains(r.String(), "worktree") {
		t.Fatalf("failure does not name the worktree:\n%s", r)
	}
}

func TestIndividualFailuresAreCaught(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(t *testing.T, cfg *config.Config)
		listErr  error
		contain  error
		wantName string
	}{
		{
			name:     "producer credential missing",
			mutate:   func(t *testing.T, c *config.Config) { os.Remove(c.Credentials.Producer.File) },
			wantName: "credential producer",
		},
		{
			name:     "producer credential empty",
			mutate:   func(t *testing.T, c *config.Config) { writeToken(t, c.Credentials.Producer.File, "") },
			wantName: "credential producer",
		},
		{
			name:     "submit repo is not a git repository",
			mutate:   func(t *testing.T, c *config.Config) { os.RemoveAll(filepath.Join(c.Paths.SubmitRepo, ".git")) },
			wantName: "submit repo",
		},
		{
			name:     "workdir does not exist",
			mutate:   func(t *testing.T, c *config.Config) { os.RemoveAll(c.Paths.ProducerWorkdir) },
			wantName: "producer workdir",
		},
		{
			name:     "inbox missing",
			mutate:   func(t *testing.T, c *config.Config) { os.RemoveAll(c.InboxDir()) },
			wantName: "handoff inbox",
		},
		{
			name: "cache root is a symlink",
			mutate: func(t *testing.T, c *config.Config) {
				real := filepath.Join(t.TempDir(), "real")
				os.MkdirAll(real, 0o755)
				link := filepath.Join(c.Paths.Root, "cache-link")
				if err := os.Symlink(real, link); err != nil {
					t.Fatal(err)
				}
				c.Paths.CacheRoot = link
			},
			wantName: "cache root",
		},
		{
			name: "cache root is world-writable",
			mutate: func(t *testing.T, c *config.Config) {
				d := filepath.Join(c.Paths.Root, "cache-open")
				os.MkdirAll(d, 0o777)
				os.Chmod(d, 0o777)
				c.Paths.CacheRoot = d
			},
			wantName: "cache root",
		},
		{
			name: "cache root missing",
			mutate: func(t *testing.T, c *config.Config) {
				c.Paths.CacheRoot = filepath.Join(c.Paths.Root, "no-such-cache")
			},
			wantName: "cache root",
		},
		{
			// factoryd can write the inbox (it made it); the producer cannot.
			// Found in the acceptance run: every producer turn read as "no
			// progress" while exiting clean.
			name: "producer cannot write the inbox",
			mutate: func(t *testing.T, c *config.Config) {
				c.Roles.Producer.RunAs = &config.RunAs{User: "inbox-locked"}
			},
			wantName: "handoff inbox as producer",
		},
		{
			// The credential the producer must never hold, readable by it: a
			// networkless producer copies it into source and submit pushes it.
			name: "producer can read the reviewer credential",
			mutate: func(t *testing.T, c *config.Config) {
				c.Roles.Producer.RunAs = &config.RunAs{User: "reads-reviewer-token"}
			},
			wantName: "producer cannot read reviewer credential",
		},
		{
			name: "producer can read its own API credential file",
			mutate: func(t *testing.T, c *config.Config) {
				c.Roles.Producer.RunAs = &config.RunAs{User: "reads-producer-token"}
			},
			wantName: "producer cannot read producer credential",
		},
		{
			// "Cannot read" from a probe that cannot read anything is not a
			// boundary; the control must hold or the probes prove nothing.
			name:     "producer read probe has no positive control",
			mutate:   func(t *testing.T, c *config.Config) { c.Roles.Producer.RunAs = &config.RunAs{User: "reads-nothing"} },
			wantName: "producer read probe control",
		},
		{
			name: "producer cannot write the outbox",
			mutate: func(t *testing.T, c *config.Config) {
				c.Roles.Producer.RunAs = &config.RunAs{User: "outbox-locked"}
			},
			wantName: "handoff outbox as producer",
		},
		{
			// The operator principal is a boundary only if the reviewer
			// identity cannot read it (#47 review).
			name: "reviewer can read the operator credential",
			mutate: func(t *testing.T, c *config.Config) {
				writeToken(t, filepath.Join(c.Paths.Root, "operator.token"), "the-operator")
				c.Credentials.Operator = config.CredentialRef{File: filepath.Join(c.Paths.Root, "operator.token")}
				c.Roles.Reviewer.RunAs = &config.RunAs{User: "reviewer-reads-operator-token"}
			},
			wantName: "reviewer cannot read operator credential",
		},
		{
			name: "operator credential configured but the reviewer runs as factoryd itself",
			mutate: func(t *testing.T, c *config.Config) {
				writeToken(t, filepath.Join(c.Paths.Root, "operator.token"), "the-operator")
				c.Credentials.Operator = config.CredentialRef{File: filepath.Join(c.Paths.Root, "operator.token")}
				c.Roles.Reviewer.RunAs = nil
			},
			wantName: "reviewer cannot read operator credential",
		},
		{
			name: "reviewer read probe has no positive control",
			mutate: func(t *testing.T, c *config.Config) {
				writeToken(t, filepath.Join(c.Paths.Root, "operator.token"), "the-operator")
				c.Credentials.Operator = config.CredentialRef{File: filepath.Join(c.Paths.Root, "operator.token")}
				c.Roles.Reviewer.RunAs = &config.RunAs{User: "reviewer-reads-nothing"}
			},
			wantName: "reviewer read probe control",
		},
		{
			name:     "turns cannot be contained",
			mutate:   func(t *testing.T, c *config.Config) {},
			contain:  errors.New("no cgroup v2"),
			wantName: "containment",
		},
		{
			// The 0711 case: the producer's HOME is neither readable nor
			// listable by the gate, and the gate can still traverse it to a
			// known descendant such as .codex/auth.json. A read probe passes
			// this; the traversal probe must fail it.
			name:     "gate can traverse the producer home (0711)",
			mutate:   func(t *testing.T, c *config.Config) { c.Gate.RunAs = &config.RunAs{User: "gate-traverses-home"} },
			wantName: "gate cannot traverse producer home",
		},
		{
			name:     "no alert transport",
			mutate:   func(t *testing.T, c *config.Config) { c.Alerts = nil },
			wantName: "alert transports",
		},
		{
			// The path exists and looks writable to a stat; the probe finds
			// out it is not, because it delivers rather than inspects.
			name: "alert file cannot be appended",
			mutate: func(t *testing.T, c *config.Config) {
				blocker := filepath.Join(t.TempDir(), "not-a-dir")
				if err := os.WriteFile(blocker, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				c.Alerts = []config.Alert{{Kind: "file", Path: filepath.Join(blocker, "alerts.log")}}
			},
			wantName: "alert file",
		},
		{
			name: "alert command refuses the alert",
			mutate: func(t *testing.T, c *config.Config) {
				c.Alerts = []config.Alert{{Kind: "command", Command: []string{"false"}, Env: map[string]string{"PATH": "/usr/bin:/bin"}, TimeoutSeconds: 5}}
			},
			wantName: "alert command",
		},
		{
			name: "one of two transports fails and the report names it",
			mutate: func(t *testing.T, c *config.Config) {
				c.Alerts = append(c.Alerts, config.Alert{Kind: "command", Command: []string{"false"}, Env: map[string]string{"PATH": "/usr/bin:/bin"}, TimeoutSeconds: 5})
			},
			wantName: "alert command",
		},
		{
			name:     "gate command does not exist",
			mutate:   func(t *testing.T, c *config.Config) { c.Gate.Command = []string{"definitely-not-a-real-binary-xyz"} },
			wantName: "gate command",
		},
		{
			name: "a role turn command does not exist",
			mutate: func(t *testing.T, c *config.Config) {
				c.Roles.Reviewer.Command = []string{"definitely-not-a-real-binary-xyz"}
			},
			wantName: "turn reviewer",
		},
		{
			name: "a halt sentinel is present",
			mutate: func(t *testing.T, c *config.Config) {
				if err := os.WriteFile(c.StopPath("producer"),
					[]byte("reason: spin abort\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantName: "halt sentinel producer",
		},
		{
			name:     "repository unreachable",
			mutate:   func(t *testing.T, c *config.Config) {},
			listErr:  errors.New("404 project not found"),
			wantName: "repository",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := fixture(t)
			c.mutate(t, cfg)
			deps := healthyDeps(cfg, c.listErr)
			if c.contain != nil {
				deps.Contain = func() error { return c.contain }
			}
			r := doctor.RunWith(context.Background(), cfg, deps)
			if r.OK() {
				t.Fatalf("doctor passed:\n%s", r)
			}
			got := failedNames(r)
			found := false
			for _, n := range got {
				if n == c.wantName {
					found = true
				}
			}
			if !found {
				t.Fatalf("failures were %v, want %q among them\n%s", got, c.wantName, r)
			}
		})
	}
}

// Distinctness that cannot be established must not read as satisfied. This is
// the difference between "the two roles are different" and "we never found out".
func TestUnresolvableIdentityDoesNotPassDistinctness(t *testing.T) {
	cfg := fixture(t)
	if err := os.Remove(cfg.Credentials.Reviewer.File); err != nil {
		t.Fatal(err)
	}
	r := doctor.RunWith(context.Background(), cfg, healthyDeps(cfg, nil))
	for _, c := range r.Checks {
		if c.Name == "distinct identities" && c.OK {
			t.Fatal("distinctness passed while one identity was never resolved")
		}
	}
}

func TestReportNamesEveryFailure(t *testing.T) {
	cfg := fixture(t)
	cfg.Alerts = nil
	cfg.Gate.Command = []string{"definitely-not-a-real-binary-xyz"}
	r := doctor.RunWith(context.Background(), cfg, healthyDeps(cfg, nil))
	out := r.String()
	for _, want := range []string{"alert transports", "gate command", "FAILED"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not mention %q:\n%s", want, out)
		}
	}
}

// ---------- the boundary, the transport, the gate ----------

// Each row breaks one of the new checks and asserts doctor names it. The
// healthy fixture above is the positive control for all of them.
func TestBoundaryTransportAndGateFailuresAreCaught(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(t *testing.T, cfg *config.Config, d *doctor.Deps)
		wantName string
		wantText string
	}{
		{
			name: "the producer can write the submit repo",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				d.NewProber = func(*config.RunAs) (doctor.Prober, error) {
					return fakeProber{writable: map[string]bool{cfg.Paths.ProducerWorkdir: true, cfg.Paths.SubmitRepo: true}}, nil
				}
			},
			wantName: "boundary", wantText: "CAN write",
		},
		{
			name: "the probe cannot run (no privilege)",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				d.NewProber = func(*config.RunAs) (doctor.Prober, error) {
					return fakeProber{err: errors.New("setuid: operation not permitted")}, nil
				}
			},
			wantName: "boundary", wantText: "undecided",
		},
		{
			// "Cannot write" must not pass on a probe that cannot write anything.
			name: "the control probe fails too",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				d.NewProber = func(*config.RunAs) (doctor.Prober, error) {
					return fakeProber{writable: map[string]bool{}}, nil
				}
			},
			wantName: "boundary", wantText: "proves nothing",
		},
		{
			name: "the producer user does not exist",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				d.NewProber = func(ra *config.RunAs) (doctor.Prober, error) {
					return nil, errors.New("user: unknown user " + ra.User)
				}
			},
			wantName: "producer identity", wantText: "unknown user",
		},
		{
			// The incident: git resolves to someone other than the API does.
			name: "git would push as a different identity",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				d.GitIdentity = func(context.Context, *config.Config, scm.Driver, string) (string, error) {
					return "factory-reviewer", nil
				}
			},
			wantName: "git identity", wantText: "disagree",
		},
		{
			name: "git identity cannot be resolved",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				d.GitIdentity = func(context.Context, *config.Config, scm.Driver, string) (string, error) {
					return "", errors.New("credential fill returned nothing")
				}
			},
			wantName: "git identity", wantText: "undecided",
		},
		{
			name: "the submit repo's local config carries a proxy",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				d.GitGuard = func(*config.Config, scm.Driver, string) error {
					return errors.New("keys outside the allowlist: http.proxy")
				}
			},
			wantName: "git config allowlist", wantText: "http.proxy",
		},
		{
			name: "the gate can write .git",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				d.NewProber = func(ra *config.RunAs) (doctor.Prober, error) {
					if ra.User == "factoryd-gate" {
						return fakeProber{name: "gate", writable: map[string]bool{
							filepath.Join(cfg.Paths.SubmitRepo, ".git"):      true,
							filepath.Join(cfg.Paths.SubmitRepo, "build/out"): true}}, nil
					}
					return fakeProber{writable: map[string]bool{cfg.Paths.ProducerWorkdir: true, cfg.InboxDir(): true, cfg.OutboxDir(): true}, readable: map[string]bool{cfg.Paths.ProducerWorkdir: true}}, nil
				}
			},
			wantName: "gate cannot touch .git", wantText: "plant a hook",
		},
		{
			name: "the gate can read the reviewer credential",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				d.NewProber = func(ra *config.RunAs) (doctor.Prober, error) {
					if ra.User == "factoryd-gate" {
						return fakeProber{name: "gate",
							writable: map[string]bool{filepath.Join(cfg.Paths.SubmitRepo, "build/out"): true},
							readable: map[string]bool{cfg.Credentials.Reviewer.File: true}}, nil
					}
					return fakeProber{writable: map[string]bool{cfg.Paths.ProducerWorkdir: true, cfg.InboxDir(): true, cfg.OutboxDir(): true}, readable: map[string]bool{cfg.Paths.ProducerWorkdir: true}}, nil
				}
			},
			wantName: "gate cannot read reviewer credential", wantText: "two-party",
		},
		{
			// The control for the two above: "cannot" must not be a gate that
			// cannot do anything.
			name: "the gate cannot write a path it declared",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				d.NewProber = func(ra *config.RunAs) (doctor.Prober, error) {
					if ra.User == "factoryd-gate" {
						return fakeProber{name: "gate", writable: map[string]bool{}}, nil
					}
					return fakeProber{writable: map[string]bool{cfg.Paths.ProducerWorkdir: true, cfg.InboxDir(): true, cfg.OutboxDir(): true}, readable: map[string]bool{cfg.Paths.ProducerWorkdir: true}}, nil
				}
			},
			wantName: "gate can write build/out", wantText: "declared it needs",
		},
		{
			// A declared path is a capability grant. "." is the repository root,
			// from which the gate could rename, delete or replace .git.
			name: "a gate path grants the repository root",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				cfg.Gate.RequiredWritablePaths = []string{"."}
			},
			wantName: "gate path .", wantText: "ancestor",
		},
		{
			name: "a gate path grants .git itself",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				cfg.Gate.RequiredWritablePaths = []string{".git"}
			},
			wantName: "gate path .git", wantText: "never own",
		},
		{
			name: "a gate path grants something inside .git",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				cfg.Gate.RequiredWritablePaths = []string{".git/hooks"}
			},
			wantName: "gate path .git/hooks", wantText: "never own",
		},
		{
			// The lexical check cannot see this; the physical one must.
			name: "a gate path is a symlink landing on .git",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				if err := os.Symlink(".git", filepath.Join(cfg.Paths.SubmitRepo, "cache")); err != nil {
					t.Fatal(err)
				}
				cfg.Gate.RequiredWritablePaths = []string{"cache/objects"}
			},
			wantName: "gate can write cache/objects", wantText: "resolves inside",
		},
		{
			// Provisioning changed what the first probe measured.
			name: "the gate can touch .git once the declared paths exist",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				git := filepath.Join(cfg.Paths.SubmitRepo, ".git")
				d.NewProber = func(ra *config.RunAs) (doctor.Prober, error) {
					if ra.User == "factoryd-gate" {
						calls := 0
						return &countingProber{fakeProber: fakeProber{name: "gate",
							writable: map[string]bool{filepath.Join(cfg.Paths.SubmitRepo, "build/out"): true}},
							gitDir: git, calls: &calls}, nil
					}
					return fakeProber{writable: map[string]bool{cfg.Paths.ProducerWorkdir: true, cfg.InboxDir(): true, cfg.OutboxDir(): true}, readable: map[string]bool{cfg.Paths.ProducerWorkdir: true}}, nil
				}
			},
			wantName: "gate cannot touch .git after provisioning", wantText: "once the declared paths exist",
		},
		{
			// An execute bit is not executability by the gate.
			name: "the gate command is root-only",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				exe, _ := config.LookPathIn(cfg.Gate.Env["PATH"], cfg.Gate.Command[0])
				d.NewProber = func(ra *config.RunAs) (doctor.Prober, error) {
					if ra.User == "factoryd-gate" {
						return fakeProber{name: "gate",
							writable: map[string]bool{filepath.Join(cfg.Paths.SubmitRepo, "build/out"): true},
							rootOnly: map[string]bool{exe: true}}, nil
					}
					return fakeProber{writable: map[string]bool{cfg.Paths.ProducerWorkdir: true, cfg.InboxDir(): true, cfg.OutboxDir(): true}, readable: map[string]bool{cfg.Paths.ProducerWorkdir: true}}, nil
				}
			},
			wantName: "gate can run its command", wantText: "doctor could",
		},
		{
			name: "the producer command is root-only",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				exe, _ := config.LookPathIn(cfg.Roles.Producer.Env["PATH"], cfg.Roles.Producer.Command[0])
				d.NewProber = func(ra *config.RunAs) (doctor.Prober, error) {
					if ra.User == "factoryd-gate" {
						return fakeProber{name: "gate", writable: map[string]bool{filepath.Join(cfg.Paths.SubmitRepo, "build/out"): true}}, nil
					}
					return fakeProber{writable: map[string]bool{cfg.Paths.ProducerWorkdir: true}, rootOnly: map[string]bool{exe: true}}, nil
				}
			},
			wantName: "producer can run its command", wantText: "doctor could",
		},
		{
			// roles.producer.workdir overrides the default. Root can reach the
			// override; the producer cannot. A probe against the default would
			// be green about a directory the turn never runs in.
			name: "the producer workdir override is not writable by the producer",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				override := filepath.Join(cfg.Paths.Root, "override")
				if err := os.MkdirAll(override, 0o755); err != nil {
					t.Fatal(err)
				}
				cfg.Roles.Producer.Workdir = override
				d.NewProber = func(ra *config.RunAs) (doctor.Prober, error) {
					if ra.User == "factoryd-gate" {
						return fakeProber{name: "gate", writable: map[string]bool{filepath.Join(cfg.Paths.SubmitRepo, "build/out"): true}}, nil
					}
					// writable: the DEFAULT workdir only, not the override
					return fakeProber{writable: map[string]bool{cfg.Paths.ProducerWorkdir: true, cfg.InboxDir(): true, cfg.OutboxDir(): true}, readable: map[string]bool{cfg.Paths.ProducerWorkdir: true}}, nil
				}
			},
			wantName: "producer can write its workdir", wantText: "every turn would fail",
		},
		{
			// reviewer.run_as is honoured by the runner; doctor must probe the
			// reviewer's command under that identity too.
			name: "the reviewer command is root-only under reviewer.run_as",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				cfg.Roles.Reviewer.RunAs = &config.RunAs{User: "factoryd-reviewer"}
				exe, _ := config.LookPathIn(cfg.Roles.Reviewer.Env["PATH"], cfg.Roles.Reviewer.Command[0])
				d.NewProber = func(ra *config.RunAs) (doctor.Prober, error) {
					switch ra.User {
					case "factoryd-gate":
						return fakeProber{name: "gate", writable: map[string]bool{filepath.Join(cfg.Paths.SubmitRepo, "build/out"): true}}, nil
					case "factoryd-reviewer":
						return fakeProber{name: "reviewer", writable: map[string]bool{cfg.Paths.Root: true}, rootOnly: map[string]bool{exe: true}}, nil
					}
					return fakeProber{writable: map[string]bool{cfg.Paths.ProducerWorkdir: true, cfg.InboxDir(): true, cfg.OutboxDir(): true}, readable: map[string]bool{cfg.Paths.ProducerWorkdir: true}}, nil
				}
			},
			wantName: "reviewer can run its command", wantText: "doctor could",
		},
		{
			name: "a gate path references an unset variable",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				cfg.Gate.RequiredWritablePaths = []string{"${NOPE}/cache"}
			},
			wantName: "gate path ${NOPE}/cache", wantText: "does not set",
		},
		{
			name: "a gate path exists as a file",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				f := filepath.Join(cfg.Paths.SubmitRepo, "notadir")
				if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				cfg.Gate.RequiredWritablePaths = []string{"notadir"}
			},
			wantName: "gate path notadir", wantText: "not a directory",
		},
		{
			name: "a gate path cannot be created",
			mutate: func(t *testing.T, cfg *config.Config, d *doctor.Deps) {
				cfg.Gate.RequiredWritablePaths = []string{"/proc/factoryd-cannot-create/x"}
			},
			wantName: "gate path /proc/factoryd-cannot-create/x", wantText: "cannot be created",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := fixture(t)
			d := healthyDeps(cfg, nil)
			c.mutate(t, cfg, &d)
			r := doctor.RunWith(context.Background(), cfg, d)
			if r.OK() {
				t.Fatalf("doctor passed:\n%s", r)
			}
			var hit *doctor.Check
			for i := range r.Checks {
				if r.Checks[i].Name == c.wantName && !r.Checks[i].OK {
					hit = &r.Checks[i]
				}
			}
			if hit == nil {
				t.Fatalf("no failing check named %q; failures were %v\n%s", c.wantName, failedNames(r), r)
			}
			if hit.Err == nil || !strings.Contains(hit.Err.Error(), c.wantText) {
				t.Fatalf("%s failed but does not say %q: %v", c.wantName, c.wantText, hit.Err)
			}
		})
	}
}

// An absent-but-creatable gate path is accepted and said to be creatable,
// since submit creates it. This is the other half of the "exists as a file"
// row: the check must distinguish the two, not fail everything absent.
func TestAbsentButCreatableGatePathIsAccepted(t *testing.T) {
	cfg := fixture(t)
	cfg.Gate.RequiredWritablePaths = []string{"build/out"}
	r := doctor.RunWith(context.Background(), cfg, healthyDeps(cfg, nil))
	for _, c := range r.Checks {
		if c.Name == "gate path build/out" && !c.OK {
			t.Fatalf("an absent, creatable gate path was refused: %v", c.Err)
		}
	}
	if !r.OK() {
		t.Fatalf("unrelated failures: %v", failedNames(r))
	}
}

// doctor must resolve the gate command where the gate will look, not where
// doctor's own shell looks. Ambient PATH finds it; the declared PATH does not;
// doctor must fail.
func TestGateCommandResolvedAgainstDeclaredPath(t *testing.T) {
	cfg := fixture(t)
	cfg.Gate.Command = []string{"true"} // a bare name: resolution is the question
	cfg.Gate.Env["PATH"] = t.TempDir()  // declared PATH with nothing on it
	r := doctor.RunWith(context.Background(), cfg, healthyDeps(cfg, nil))
	found := false
	for _, c := range r.Checks {
		if c.Name == "gate command" && !c.OK && strings.Contains(c.Err.Error(), "declared PATH") {
			found = true
		}
	}
	if !found {
		t.Fatalf("doctor passed the gate command against an empty declared PATH (its own PATH found it):\n%s", r)
	}
}

// countingProber answers "cannot write .git" the first time and "can" after,
// modelling provisioning that handed the gate reach into .git.
type countingProber struct {
	fakeProber
	gitDir string
	calls  *int
}

func (c *countingProber) CanWrite(ctx context.Context, dir string) (bool, error) {
	if dir == c.gitDir {
		*c.calls++
		return *c.calls > 1, nil
	}
	return c.fakeProber.CanWrite(ctx, dir)
}

// The positive control for the capability rows: an ordinary cache directory
// inside the repository, and one outside it, are both accepted.
func TestOrdinaryGatePathsAreAccepted(t *testing.T) {
	cfg := fixture(t)
	outside := filepath.Join(t.TempDir(), "gocache")
	cfg.Gate.Env["GOCACHE"] = outside
	cfg.Gate.RequiredWritablePaths = []string{"build/out", "${GOCACHE}"}
	d := healthyDeps(cfg, nil)
	d.NewProber = func(ra *config.RunAs) (doctor.Prober, error) {
		if ra.User == "factoryd-gate" {
			return fakeProber{name: "gate", writable: map[string]bool{
				filepath.Join(cfg.Paths.SubmitRepo, "build/out"): true, outside: true}}, nil
		}
		return fakeProber{writable: map[string]bool{cfg.Paths.ProducerWorkdir: true, cfg.InboxDir(): true, cfg.OutboxDir(): true}, readable: map[string]bool{cfg.Paths.ProducerWorkdir: true}}, nil
	}
	r := doctor.RunWith(context.Background(), cfg, d)
	if !r.OK() {
		t.Fatalf("ordinary gate paths were refused: %v\n%s", failedNames(r), r)
	}
}

// submit_repo itself a symlink, and the clone carries cache -> .git. Resolving
// only the declared path compares /real/repo/.git against /link/repo/.git and
// misses; Own then follows the link and chowns .git. The guard must fail BEFORE
// provisioning, with .git's ownership untouched.
func TestSymlinkedSubmitRepoCannotSmuggleAGrantIntoDotGit(t *testing.T) {
	cfg := fixture(t)
	real := cfg.Paths.SubmitRepo
	link := filepath.Join(filepath.Dir(real), "submit-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	cfg.Paths.SubmitRepo = link // a valid path under every other check
	if err := os.Symlink(".git", filepath.Join(real, "cache")); err != nil {
		t.Fatal(err)
	}
	cfg.Gate.RequiredWritablePaths = []string{"cache"}

	owned := false
	d := healthyDeps(cfg, nil)
	d.NewProber = func(ra *config.RunAs) (doctor.Prober, error) {
		if ra.User == "factoryd-gate" {
			return &owningProber{fakeProber: fakeProber{name: "gate"}, owned: &owned}, nil
		}
		return fakeProber{writable: map[string]bool{cfg.Paths.ProducerWorkdir: true, cfg.InboxDir(): true, cfg.OutboxDir(): true}, readable: map[string]bool{cfg.Paths.ProducerWorkdir: true}}, nil
	}
	r := doctor.RunWith(context.Background(), cfg, d)

	var hit *doctor.Check
	for i := range r.Checks {
		if r.Checks[i].Name == "gate can write cache" {
			hit = &r.Checks[i]
		}
	}
	if hit == nil || hit.OK {
		t.Fatalf("the grant into .git through a symlinked submit_repo was not refused:\n%s", r)
	}
	if !strings.Contains(hit.Err.Error(), "resolves") {
		t.Fatalf("refused for the wrong reason: %v", hit.Err)
	}
	// Refused BEFORE provisioning: nothing was given away.
	if owned {
		t.Fatal("Own ran despite the refusal; .git would have been chowned to the gate")
	}
}

// owningProber records whether Own was ever called.
type owningProber struct {
	fakeProber
	owned *bool
}

func (o *owningProber) Own(string) error { *o.owned = true; return nil }

// The positive control for the per-role probes: a reviewer under its own
// run_as with a normal command and a writable workdir passes every check.
func TestReviewerUnderItsOwnIdentityPasses(t *testing.T) {
	cfg := fixture(t)
	cfg.Roles.Reviewer.RunAs = &config.RunAs{User: "factoryd-reviewer"}
	d := healthyDeps(cfg, nil)
	d.NewProber = func(ra *config.RunAs) (doctor.Prober, error) {
		switch ra.User {
		case "factoryd-gate":
			return fakeProber{name: "gate", writable: map[string]bool{filepath.Join(cfg.Paths.SubmitRepo, "build/out"): true}}, nil
		case "factoryd-reviewer":
			return fakeProber{name: "reviewer", writable: map[string]bool{cfg.Paths.Root: true}}, nil
		}
		return fakeProber{writable: map[string]bool{cfg.Paths.ProducerWorkdir: true, cfg.InboxDir(): true, cfg.OutboxDir(): true}, readable: map[string]bool{cfg.Paths.ProducerWorkdir: true}}, nil
	}
	r := doctor.RunWith(context.Background(), cfg, d)
	if !r.OK() {
		t.Fatalf("a correctly configured reviewer.run_as was refused: %v\n%s", failedNames(r), r)
	}
	seen := false
	for _, c := range r.Checks {
		if c.Name == "reviewer can run its command" && c.OK {
			seen = true
		}
	}
	if !seen {
		t.Fatal("the reviewer's command was never probed under its identity")
	}
}

// The delivery probe is a delivery: a healthy run leaves a probe line in the
// alert file. Without this, "alert file ok" would also be printed by a probe
// that only stat'ed the path.
func TestAlertProbeActuallyDelivers(t *testing.T) {
	cfg := fixture(t)
	r := doctor.RunWith(context.Background(), cfg, healthyDeps(cfg, nil))
	if !r.OK() {
		t.Fatalf("healthy fixture failed:\n%s", r)
	}
	body, err := os.ReadFile(cfg.Alerts[0].Path)
	if err != nil {
		t.Fatalf("no alert file after doctor: %v", err)
	}
	if !strings.Contains(string(body), `"kind":"doctor"`) || !strings.Contains(string(body), `"severity":"probe"`) {
		t.Fatalf("alert file does not hold the probe:\n%s", body)
	}
}

// A relative producer HOME names a directory under the producer workdir to
// the turn and a directory under doctor's own working directory to a probe.
// It is never probed: doctor fails the check outright, and the gate prober
// is not asked about a path it would resolve in the wrong place.
func TestRelativeProducerHomeIsAFailureNotAProbe(t *testing.T) {
	cfg := fixture(t)
	cfg.Roles.Producer.Env["HOME"] = "private"
	var asked []string
	deps := healthyDeps(cfg, nil)
	inner := deps.NewProber
	deps.NewProber = func(ra *config.RunAs) (doctor.Prober, error) {
		p, err := inner(ra)
		if err != nil {
			return nil, err
		}
		return recordingProber{p, &asked}, nil
	}
	r := doctor.RunWith(context.Background(), cfg, deps)
	failed := failedNames(r)
	found := false
	for _, n := range failed {
		if n == "producer home" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a relative home did not fail doctor; failures=%v\n%s", failed, r)
	}
	for _, a := range asked {
		if a == "private" || strings.HasSuffix(a, "/private") {
			t.Fatalf("the gate prober was asked to traverse the relative home (%q), resolved in the wrong directory", a)
		}
	}
}

type recordingProber struct {
	doctor.Prober
	asked *[]string
}

func (p recordingProber) CanTraverse(ctx context.Context, path string) (bool, error) {
	*p.asked = append(*p.asked, path)
	return p.Prober.CanTraverse(ctx, path)
}

// Distinct file paths are not distinct principals. An operator file that
// holds the reviewer's token is unreadable by the reviewer identity and
// still one authority: the provider's stable id says so, and doctor fails
// "operator identity" (#47 review).
func TestOperatorFileHoldingTheReviewerTokenIsOneAuthority(t *testing.T) {
	cfg := fixture(t)
	reviewerTok, err := os.ReadFile(cfg.Credentials.Reviewer.File)
	if err != nil {
		t.Fatal(err)
	}
	op := filepath.Join(cfg.Paths.Root, "operator.token")
	if err := os.WriteFile(op, reviewerTok, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Credentials.Operator = config.CredentialRef{File: op}
	cfg.Roles.Reviewer.RunAs = &config.RunAs{User: "factoryd-reviewer"} // cannot read the operator file
	r := doctor.RunWith(context.Background(), cfg, healthyDeps(cfg, nil))
	if r.OK() {
		t.Fatalf("doctor passed with the reviewer's token behind the operator path:\n%s", r)
	}
	names := failedNames(r)
	found := false
	for _, n := range names {
		if n == "operator identity" {
			found = true
		}
	}
	if !found || !strings.Contains(r.String(), "operator and reviewer both authenticate as") {
		t.Fatalf("failures %v; want operator identity naming the shared authority:\n%s", names, r)
	}
	// And the unreadability probe passed, as it must: the path boundary held
	// and was not the check that caught this.
	if strings.Contains(r.String(), "FAIL  reviewer cannot read operator credential") {
		t.Fatalf("the path probe failed; this case is about the id check:\n%s", r)
	}
}

// With an operator credential the reviewer cannot read, doctor passes and
// prints the reviewer's refusal and its control (positive control for the
// three negative cases above).
func TestOperatorCredentialUnreadableByTheReviewerPasses(t *testing.T) {
	cfg := fixture(t)
	writeToken(t, filepath.Join(cfg.Paths.Root, "operator.token"), "the-operator")
	cfg.Credentials.Operator = config.CredentialRef{File: filepath.Join(cfg.Paths.Root, "operator.token")}
	cfg.Roles.Reviewer.RunAs = &config.RunAs{User: "factoryd-reviewer"}
	r := doctor.RunWith(context.Background(), cfg, healthyDeps(cfg, nil))
	if !r.OK() {
		t.Fatalf("doctor failed:\n%s", r)
	}
	text := r.String()
	for _, want := range []string{"reviewer cannot read operator credential", "reviewer read probe control", "producer cannot read operator credential", "gate cannot read operator credential", "operator identity"} {
		if !strings.Contains(text, want) {
			t.Fatalf("doctor output lacks %q:\n%s", want, text)
		}
	}
}
