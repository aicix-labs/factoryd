package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/supervise"
)

// The refresh helper runs under the producer's sandbox exactly as a turn
// does (#41 review): without the privilege to seal it, it refuses rather
// than running git in the producer's tree connected.
func TestRefreshHelperRefusesToRunConnectedWhenTheProducerIsSandboxed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; the refusal path is for the unprivileged case")
	}
	root := t.TempDir()
	cfg := &config.Config{
		Name: "f", TargetBranch: "main",
		Paths: config.Paths{Root: root, ProducerWorkdir: filepath.Join(root, "work")},
		Roles: config.Roles{Producer: config.RoleSpec{Command: []string{"true"}, Sandbox: &config.Sandbox{NoNetwork: true}}},
		Gate:  config.Gate{Env: map[string]string{"PATH": os.Getenv("PATH")}},
	}
	os.MkdirAll(cfg.Paths.ProducerWorkdir, 0o755)
	_, err := applyAsProducer(context.Background(), cfg, "/bin/true", filepath.Join(root, "main.bundle"), "main")
	if !errors.Is(err, supervise.ErrNoNetworkNeedsRoot) {
		t.Fatalf("err=%v; the helper started without the sandbox the producer was promised", err)
	}
}
