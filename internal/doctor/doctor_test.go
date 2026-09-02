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

// fixture writes a complete, healthy factory and returns its config.
func fixture(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"inbox", "outbox", "work"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A clone: .git is a directory.
	if err := os.MkdirAll(filepath.Join(root, "work", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeToken(t, filepath.Join(root, "producer.token"), "producer-bot")
	writeToken(t, filepath.Join(root, "reviewer.token"), "factory-reviewer")

	gate, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("no /bin/true available: %v", err)
	}

	cfg := &config.Config{
		SchemaVersion: config.SchemaVersion,
		Name:          "widgets",
		Provider:      "github",
		GitHub:        &config.GitHub{Owner: "acme", Repo: "widgets"},
		TargetBranch:  "main",
		Paths: config.Paths{
			Root:            root,
			ProducerWorkdir: filepath.Join(root, "work"),
		},
		Credentials: config.Credentials{
			Producer: config.CredentialRef{File: filepath.Join(root, "producer.token")},
			Reviewer: config.CredentialRef{File: filepath.Join(root, "reviewer.token")},
		},
		Gate: config.Gate{Command: []string{gate}},
		Roles: config.Roles{
			Producer: config.RoleSpec{Command: []string{gate}},
			Reviewer: config.RoleSpec{Command: []string{gate}},
		},
		Supervisor: config.Supervisor{
			SpinWarn: config.DefaultSpinWarn, SpinAbort: config.DefaultSpinAbort,
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
	r := doctor.Run(context.Background(), fixture(t), builder(nil))
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

	r := doctor.Run(context.Background(), cfg, builder(nil))
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
	gitPath := filepath.Join(cfg.Paths.ProducerWorkdir, ".git")
	if err := os.RemoveAll(gitPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitPath, []byte("gitdir: /elsewhere/.git/worktrees/w\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := doctor.Run(context.Background(), cfg, builder(nil))
	if r.OK() {
		t.Fatal("doctor passed a producer workdir that is a worktree, not a clone")
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
			name:     "workdir is not a git repository",
			mutate:   func(t *testing.T, c *config.Config) { os.RemoveAll(filepath.Join(c.Paths.ProducerWorkdir, ".git")) },
			wantName: "producer workdir",
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
			name:     "no alert transport",
			mutate:   func(t *testing.T, c *config.Config) { c.Alerts = nil },
			wantName: "alert transports",
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
			r := doctor.Run(context.Background(), cfg, builder(c.listErr))
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
	r := doctor.Run(context.Background(), cfg, builder(nil))
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
	r := doctor.Run(context.Background(), cfg, builder(nil))
	out := r.String()
	for _, want := range []string{"alert transports", "gate command", "FAILED"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not mention %q:\n%s", want, out)
		}
	}
}
