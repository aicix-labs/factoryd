package brief_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aicix-labs/factoryd/internal/brief"
	"github.com/aicix-labs/factoryd/internal/config"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &config.Config{Paths: config.Paths{Root: root}}
}

func TestListOrdersRegularMarkdownBriefs(t *testing.T) {
	cfg := testConfig(t)
	if err := brief.Ensure(cfg); err != nil {
		t.Fatal(err)
	}
	for name := range map[string]bool{"020-second.md": true, "010-first.md": true, "notes.txt": true} {
		if err := os.WriteFile(filepath.Join(cfg.BriefsDir(), name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(cfg.BriefsDir(), "030-directory.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := brief.List(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "010-first.md" || entries[1].Name != "020-second.md" {
		t.Fatalf("entries=%+v, want lexical markdown files only", entries)
	}
}

func TestTakeMovesWithoutOverwritingTheAuditTrail(t *testing.T) {
	cfg := testConfig(t)
	if err := brief.Ensure(cfg); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(cfg.BriefsDir(), "010-first.md")
	if err := os.WriteFile(src, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst, err := brief.Take(cfg, src)
	if err != nil {
		t.Fatal(err)
	}
	if !brief.ValidDonePath(cfg, dst) || brief.ValidDonePath(cfg, filepath.Join(cfg.Paths.Root, "outside.md")) {
		t.Fatalf("completed path validation accepted the wrong boundary: %s", dst)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source still exists after take: %v", err)
	}
	if body, err := os.ReadFile(dst); err != nil || string(body) != "first\n" {
		t.Fatalf("done body=%q err=%v", body, err)
	}
	if err := os.WriteFile(src, []byte("replacement\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := brief.Take(cfg, src); err == nil {
		t.Fatal("take overwrote an existing completed brief")
	}
	if body, _ := os.ReadFile(dst); string(body) != "first\n" {
		t.Fatalf("audit record was overwritten: %q", body)
	}
}

func TestTakeRejectsSymlink(t *testing.T) {
	cfg := testConfig(t)
	if err := brief.Ensure(cfg); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(cfg.Paths.Root, "outside.md")
	if err := os.WriteFile(target, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(cfg.BriefsDir(), "010-link.md")
	if err := os.Symlink(target, src); err != nil {
		t.Fatal(err)
	}
	if _, err := brief.Take(cfg, src); err == nil {
		t.Fatal("symlink brief was accepted")
	}
}
