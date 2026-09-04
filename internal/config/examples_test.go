package config_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aicix-labs/factoryd/internal/config"
)

// Every shipped example must load, and its gate must be a gate: one that
// can go red, that prunes the .git it may not read, and that does not let
// Go's VCS stamping walk it either. And no example sets a FACTORYD_* value
// by hand; the supervisor generates those.
func TestShippedExampleGatesAreRealAndPruneDotGit(t *testing.T) {
	examples, err := filepath.Glob(filepath.Join("..", "..", "examples", "*", "factory.json"))
	if err != nil {
		t.Fatal(err)
	}
	top, _ := filepath.Glob(filepath.Join("..", "..", "examples", "*.json"))
	examples = append(examples, top...)
	if len(examples) < 3 {
		t.Fatalf("found %d examples, want at least 3: %v", len(examples), examples)
	}
	for _, p := range examples {
		cfg, err := config.Load(p)
		if err != nil {
			t.Errorf("%s does not load: %v", p, err)
			continue
		}
		cmd := strings.Join(cfg.Gate.Command, " ")
		if len(cfg.Gate.Command) == 0 || cfg.Gate.Command[0] == "true" || cmd == "true" {
			t.Errorf("%s: gate %q cannot fail; a gate that cannot go red gates nothing", p, cmd)
		}
		// Anything that walks the tree must prune .git: a find without the
		// prune, or gofmt pointed at the tree root (which walks everything,
		// dot-directories included -- the Go build tools do not, gofmt does).
		walksUnpruned := (strings.Contains(cmd, "find .") && !strings.Contains(cmd, "-path ./.git -prune")) ||
			strings.Contains(cmd, "gofmt -l .") || strings.Contains(cmd, "gofmt -l ./") || strings.Contains(cmd, "gofmt -w .")
		if walksUnpruned {
			t.Errorf("%s: gate walks the tree without pruning .git, which the gate identity may not read: %q", p, cmd)
		}
		// Go's default VCS stamping runs git status for a main package, and
		// that dies on the 0700 .git the gate may not read (#21, second
		// round). The gate's environment must switch it off.
		if !strings.Contains(cfg.Gate.Env["GOFLAGS"], "-buildvcs=false") {
			t.Errorf("%s: gate GOFLAGS %q lacks -buildvcs=false; a main package would fail VCS stamping on an unreadable .git", p, cfg.Gate.Env["GOFLAGS"])
		}
	}
}

// The condition itself, not a string check: a module with a main package,
// its .git unreadable, built with and without -buildvcs=false. Unprivileged
// this uses a 0000 .git the test cannot enter either; the same run under
// the gate identity against a root-owned 0700 .git is the live evidence.
func TestBuildVCSFalseIsWhatSurvivesAnUnreadableDotGit(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go on PATH")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can read a 0000 directory; run the gate-identity check on the harness instead")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/m\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	if err := os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	if err := os.Chmod(filepath.Join(dir, ".git"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(dir, ".git"), 0o755) })
	run := func(goflags string) (string, error) {
		cmd := exec.Command(goBin, "build", "./...")
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GOCACHE=" + filepath.Join(dir, "cache"), "GOFLAGS=" + goflags, "GOPATH=" + filepath.Join(dir, "gopath"), "GOTOOLCHAIN=local"}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("-count=1"); err == nil || !strings.Contains(out, "VCS") {
		t.Fatalf("without -buildvcs=false the build must fail on VCS status; err=%v out=%s", err, out)
	}
	if out, err := run("-count=1 -buildvcs=false"); err != nil {
		t.Fatalf("with -buildvcs=false the build must pass: %v\n%s", err, out)
	}
}
