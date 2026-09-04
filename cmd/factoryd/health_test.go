package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aicix-labs/factoryd/internal/alert"
	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/health"
	"github.com/aicix-labs/factoryd/internal/proc"
	"github.com/aicix-labs/factoryd/internal/state"
)

func healthCfg(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"work", "submit", "inbox", "outbox", "cache"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &config.Config{
		SchemaVersion: config.SchemaVersion, Name: "widgets", Provider: "github",
		GitHub: &config.GitHub{Owner: "acme", Repo: "widgets"}, TargetBranch: "main",
		Paths:  config.Paths{Root: root, ProducerWorkdir: filepath.Join(root, "work"), SubmitRepo: filepath.Join(root, "submit"), CacheRoot: filepath.Join(root, "cache")},
		Roles:  config.Roles{Producer: config.RoleSpec{TimeoutSeconds: 600}, Reviewer: config.RoleSpec{TimeoutSeconds: 600}},
		Alerts: []config.Alert{{Kind: "file", Path: filepath.Join(root, "alerts.log")}},
		Health: config.DefaultHealth(),
	}
}

// The three exit codes are the contract (§7): 0 healthy, 1 findings, 3
// could not look. Each is produced here from a real tick, so that a
// classification bug in the CLI -- not the package -- is caught.
func TestHealthExitCodes(t *testing.T) {
	cfg := healthCfg(t)
	fan, err := alert.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	deps := health.Deps{Probes: health.HostProbes{}, Alerts: fan}
	run := func() int {
		var out, errb bytes.Buffer
		return tickOnce(context.Background(), cfg, deps, false, &out, &errb)
	}

	// 1: findings -- nothing has ever supervised this factory.
	if code := run(); code != exitUnhealthy {
		t.Fatalf("never supervised: exit %d, want %d", code, exitUnhealthy)
	}

	// 0: a live supervisor (this test process) registered for both roles.
	self, err := proc.Self("test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Update(cfg.StatePath(), cfg.Name, func(s *state.State) error {
		for _, r := range state.Roles {
			s.Role(r).Supervisor = &self
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code := run(); code != exitOK {
		t.Fatalf("healthy: exit %d, want 0", code)
	}

	// 3: could not look -- the state document is corrupt.
	if err := os.WriteFile(cfg.StatePath(), []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run(); code != exitConfig {
		t.Fatalf("corrupt state: exit %d, want %d (could not look is not a finding)", code, exitConfig)
	}
}
