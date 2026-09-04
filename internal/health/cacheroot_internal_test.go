package health

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
)

// The property the bound root exists for: after the root is opened and
// verified, and before anything is deleted, the path is retargeted at a
// victim. Deletion must follow the HANDLE, not the path -- so the victim is
// intact and the real root was reclaimed. A reclaimer that removes by
// absolute path passes every before-the-tick test and fails this one.
func TestDeletionFollowsTheHandleNotThePath(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "cache")
	cache := filepath.Join(root, "go")
	now := time.Now()
	mk := func(dir, name string, mtime time.Time) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, 1000), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, mtime, mtime)
	}
	mk(cache, "old/blob", now.Add(-3*time.Hour))
	mk(cache, "new/blob", now)
	victim := filepath.Join(base, "victim")
	mk(victim, "go/old/blob", now.Add(-3*time.Hour))
	mk(victim, "go/new/blob", now)
	// A symlink entry in both, older than everything: the first thing
	// reclamation reaches, removed as a link -- and only in the real root.
	target := filepath.Join(base, "target")
	mk(target, "blob", now.Add(-5*time.Hour))
	for _, d := range []string{cache, filepath.Join(victim, "go")} {
		if err := os.Symlink(target, filepath.Join(d, "aaa-link")); err != nil {
			t.Fatal(err)
		}
	}
	// The link's own mtime is its creation; date the real files after it.
	for _, d := range []string{cache, filepath.Join(victim, "go")} {
		os.Chtimes(filepath.Join(d, "old/blob"), now.Add(time.Hour), now.Add(time.Hour))
		os.Chtimes(filepath.Join(d, "new/blob"), now.Add(2*time.Hour), now.Add(2*time.Hour))
	}

	afterOpenCacheRoot = func() {
		// The root's path now means the victim.
		if err := os.Rename(root, root+".moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, root); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { afterOpenCacheRoot = nil })

	cfg := &config.Config{Paths: config.Paths{CacheRoot: root}, Health: config.Health{Caches: []config.Cache{{Path: cache, MaxBytes: 1000}}}}
	reps, findings := reclaimCaches(cfg, now, os.Stderr)
	_ = context.Background()
	// 2000 bytes of files + a link over a 1000 bound: the link and the old
	// entry go, the newest stays.
	if len(reps) != 1 || reps[0].ReclaimedCount != 2 {
		t.Fatalf("reps=%+v findings=%+v; the verified root should have been reclaimed", reps, findings)
	}
	if _, err := os.Lstat(filepath.Join(victim, "go", "aaa-link")); err != nil {
		t.Fatal("the victim's link entry was removed: link removal followed the path, not the handle")
	}
	if _, err := os.Lstat(filepath.Join(root+".moved", "go", "aaa-link")); err == nil {
		t.Fatal("the real root's link entry survived")
	}
	if _, err := os.Stat(filepath.Join(target, "blob")); err != nil {
		t.Fatal("a link was followed")
	}
	if _, err := os.Stat(filepath.Join(victim, "go/old/blob")); err != nil {
		t.Fatal("the victim was deleted: removal followed the path, not the handle")
	}
	if _, err := os.Stat(filepath.Join(root+".moved", "go/old/blob")); err == nil {
		t.Fatal("the real root's oldest entry survived; nothing was reclaimed at all")
	}
}

// The window between the no-follow open and the os.Root bind: if the root
// is swapped in it, the bound root is not the verified one, and the inode
// comparison is what refuses. Without the seam this check could not be
// shown to fail.
func TestRootSwappedBetweenTheTwoOpensIsRefused(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "cache")
	other := filepath.Join(base, "other")
	for _, d := range []string{root, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	betweenOpens = func() {
		os.Rename(root, root+".moved")
		os.Rename(other, root) // a different directory now sits at the path
	}
	t.Cleanup(func() { betweenOpens = nil })
	if cr, err := OpenCacheRoot(root); err == nil {
		cr.Close()
		t.Fatal("a root swapped between the two opens was bound")
	}
}
