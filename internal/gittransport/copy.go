package gittransport

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// CopyTree copies the producer's source tree into the submit repository's
// work tree (SPEC.md §4.4). It is the crossing point of the two-directory
// boundary, so what it refuses matters more than what it copies:
//
//   - any path named .git, at any depth, file or directory, is skipped. The
//     producer's git control data never reaches the repository git reads.
//   - a symlink whose target resolves outside the source tree is refused, not
//     followed and not copied. Following it would read files the producer
//     cannot write; copying it as a link would plant one in the submit tree.
//   - the two control files the producer writes for submit are skipped, since
//     they are handoff, not content.
//
// Everything under dst that is not .git is removed first, so a file the
// producer deleted is deleted in the copy too. The result is the producer's
// tree, exactly, minus git control data.
func CopyTree(src, dst string) error {
	src, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	dst, err = filepath.Abs(dst)
	if err != nil {
		return err
	}
	if src == dst || strings.HasPrefix(dst, src+string(os.PathSeparator)) || strings.HasPrefix(src, dst+string(os.PathSeparator)) {
		return fmt.Errorf("copytree: %s and %s overlap; the boundary needs two separate directories", src, dst)
	}
	if fi, err := os.Stat(filepath.Join(dst, ".git")); err != nil || !fi.IsDir() {
		return fmt.Errorf("copytree: %s is not a clone (no .git directory); refusing to copy into it", dst)
	}

	// Clear the destination work tree, keeping its .git.
	entries, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}

	return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		base := d.Name()

		// Git control data never crosses.
		if base == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Handoff files are not content.
		if rel == ".producer-branch" || rel == ".producer-commit-msg" {
			return nil
		}

		target := filepath.Join(dst, rel)

		if d.Type()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("copytree: %s: %w", rel, err)
			}
			if resolved != src && !strings.HasPrefix(resolved, src+string(os.PathSeparator)) {
				return fmt.Errorf("copytree: %s is a symlink resolving outside the producer tree (%s); refusing", rel, resolved)
			}
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
