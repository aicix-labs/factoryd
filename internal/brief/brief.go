// Package brief owns the operator-to-producer brief queue.
//
// Queue entries are ordinary files so an operator can inspect and edit the
// backlog without a separate service. Taking one is nevertheless factoryd's
// operation: the entry is moved into done/ before the producer starts, leaving
// an auditable record instead of relying on a producer-writable deletion.
package brief

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aicix-labs/factoryd/internal/config"
)

// Label is the producer supervisor's trigger label for queued briefs.
const Label = "brief-queue"

// Entry is one queued brief, ordered by its base name.
type Entry struct {
	Name string
	Path string
}

// Ensure creates the queue layout. An empty queue is a normal idle state, so
// the supervisor creates it rather than requiring an operator to bootstrap an
// otherwise invisible directory.
func Ensure(cfg *config.Config) error {
	if err := os.MkdirAll(cfg.BriefsDoneDir(), 0o755); err != nil {
		return fmt.Errorf("creating brief queue: %w", err)
	}
	return nil
}

// List returns regular *.md queue entries in lexical order. A missing queue is
// an empty queue: existing brief.md-only factories keep working without an
// upgrade-time directory creation.
func List(cfg *config.Config) ([]Entry, error) {
	dir := cfg.BriefsDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading brief queue %s: %w", dir, err)
	}
	var out []Entry
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("examining queued brief %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("queued brief %s is not a regular file", entry.Name())
		}
		out = append(out, Entry{Name: entry.Name(), Path: filepath.Join(dir, entry.Name())})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DonePath is the audit location for a queued brief. It validates that src is
// directly inside inbox/briefs, never a path supplied from elsewhere.
func DonePath(cfg *config.Config, src string) (string, error) {
	clean := filepath.Clean(src)
	if filepath.Dir(clean) != filepath.Clean(cfg.BriefsDir()) {
		return "", fmt.Errorf("queued brief %q is not directly in %s", src, cfg.BriefsDir())
	}
	name := filepath.Base(clean)
	if name == "." || name == string(filepath.Separator) || !strings.HasSuffix(name, ".md") {
		return "", fmt.Errorf("queued brief %q does not have a valid .md name", src)
	}
	return filepath.Join(cfg.BriefsDoneDir(), name), nil
}

// ValidDonePath reports whether p is one direct completed-brief entry. Retry
// markers live in the handoff area, so their optional brief path is accepted
// only when it remains inside this one purpose-built directory.
func ValidDonePath(cfg *config.Config, p string) bool {
	clean := filepath.Clean(p)
	name := filepath.Base(clean)
	return filepath.Dir(clean) == filepath.Clean(cfg.BriefsDoneDir()) && name != "." && strings.HasSuffix(name, ".md")
}

// Take moves src to its durable done location. Existing entries are never
// overwritten: reusing a queue filename is an operator-visible conflict, not
// permission to erase the earlier audit record.
func Take(cfg *config.Config, src string) (string, error) {
	dst, err := DonePath(cfg, src)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(src)
	if err != nil {
		return "", fmt.Errorf("examining queued brief %s: %w", src, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("queued brief %s is not a regular file", src)
	}
	if err := os.MkdirAll(cfg.BriefsDoneDir(), 0o755); err != nil {
		return "", fmt.Errorf("creating completed brief directory: %w", err)
	}
	if _, err := os.Lstat(dst); err == nil {
		return "", fmt.Errorf("completed brief %s already exists; refusing to overwrite its audit record", dst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("examining completed brief %s: %w", dst, err)
	}
	if err := os.Rename(src, dst); err != nil {
		return "", fmt.Errorf("moving queued brief %s to %s: %w", src, dst, err)
	}
	return dst, nil
}

// Restore returns a brief to its queue position when preparation fails before
// the producer process starts. It refuses to overwrite a newly-arrived file;
// in that unusual race the completed copy remains as the durable evidence.
func Restore(cfg *config.Config, src, dst string) error {
	want, err := DonePath(cfg, src)
	if err != nil {
		return err
	}
	if filepath.Clean(dst) != filepath.Clean(want) {
		return fmt.Errorf("completed brief %q is not the expected location for %q", dst, src)
	}
	if _, err := os.Lstat(src); err == nil {
		return fmt.Errorf("cannot restore queued brief %s: a replacement already exists", src)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("examining queued brief restore path %s: %w", src, err)
	}
	if err := os.Rename(dst, src); err != nil {
		return fmt.Errorf("restoring queued brief %s: %w", src, err)
	}
	return nil
}
