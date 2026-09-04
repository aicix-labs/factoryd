package config_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/aicix-labs/factoryd/internal/config"
)

// Every shipped example must load, and its gate must be a gate: one that
// can go red. `true` cannot fail, so it cannot fail the way a real gate
// does -- by walking the 0700 .git it may not read (canary issue #21) --
// and an operator who copies it ships a check that is not a check. The
// gate must also prune .git itself, because that is the one directory the
// gate identity cannot enter by design.
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
	}
}
