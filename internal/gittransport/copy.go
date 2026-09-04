package gittransport

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

	// The crossing is root-mediated: this runs as factoryd, reading a tree the
	// producer can rewrite at any moment, including while a clean turn's
	// detached child keeps running. So nothing here is judged by path and
	// then used by path. Every entry is opened through a handle on the
	// source root with O_NOFOLLOW on the final component, and what it IS is
	// decided on the descriptor that was actually opened: a regular file is
	// copied from that descriptor; a directory is listed through it; a
	// symlink is read as a link and recreated only if its target passes; and
	// anything else -- a fifo that would block root, a device -- is refused.
	// A file swapped for a link between listing and open fails the no-follow
	// open and refuses the whole copy, which refuses the submit.
	top, err := os.OpenFile(src, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("copytree: %s: %w", src, err)
	}
	defer top.Close()
	return copyDir(top, src, ".", dst)
}

// copyDir copies the directory open as d (at path dirPath, relative rel).
// Children are opened by openat on d with O_NOFOLLOW: the directory in hand
// is the one being read, whatever the path now names.
func copyDir(d *os.File, dirPath, rel, dst string) error {
	entries, err := d.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("copytree: %s: %w", rel, err)
	}
	for _, e := range entries {
		base := e.Name()
		erel := base
		if rel != "." {
			erel = filepath.Join(rel, base)
		}
		// Git control data never crosses; handoff files are not content.
		if base == ".git" {
			continue
		}
		if rel == "." && (base == ".producer-branch" || base == ".producer-commit-msg") {
			continue
		}
		target := filepath.Join(dst, erel)

		f, err := OpenNoFollow(d, base, os.O_RDONLY)
		if err != nil {
			if errors.Is(err, syscall.ELOOP) {
				// A symlink. Judged by where it will point AFTER it is
				// recreated in the destination, not by where it points in
				// the source. A link such as hooks -> .git/hooks is "inside"
				// the producer tree -- the producer's .git is skipped, so it
				// may not even resolve there -- yet recreated verbatim it
				// points at the factory-owned submit_repo/.git/hooks. Once
				// the gate runs producer-authored code it can write through
				// that link, and the hook then runs during git push with the
				// credential helper active.
				// The value checked is the value recreated: one readlink.
				link, lerr := os.Readlink(filepath.Join(dirPath, base))
				if lerr != nil {
					return fmt.Errorf("copytree: %s: %w", erel, lerr)
				}
				if err := checkLinkTarget(erel, link); err != nil {
					return err
				}
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					return err
				}
				if err := os.Symlink(link, target); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("copytree: %s: %w", erel, err)
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return fmt.Errorf("copytree: %s: %w", erel, err)
		}
		switch {
		case fi.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				f.Close()
				return err
			}
			err := copyDir(f, filepath.Join(dirPath, base), erel, dst)
			f.Close()
			if err != nil {
				return err
			}
		case fi.Mode().IsRegular():
			err := copyFrom(f, target, fi.Mode().Perm())
			f.Close()
			if err != nil {
				return fmt.Errorf("copytree: %s: %w", erel, err)
			}
		default:
			f.Close()
			return fmt.Errorf("copytree: %s is a %v, not a file, directory or symlink; refusing to copy it", erel, fi.Mode().Type())
		}
	}
	return nil
}

func copyFrom(in *os.File, dst string, mode fs.FileMode) error {
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

// checkLinkTarget refuses a symlink target that is absolute, that escapes the
// tree via "..", or that passes through a ".git" component -- judged lexically
// relative to the link's own directory, which is exactly how the recreated
// link will resolve in the destination.
func checkLinkTarget(linkRel, target string) error {
	if filepath.IsAbs(target) {
		return fmt.Errorf("copytree: %s is an absolute symlink (%s); refusing", linkRel, target)
	}
	// Where does it land, relative to the tree root?
	landing := filepath.Clean(filepath.Join(filepath.Dir(linkRel), target))
	if landing == ".." || strings.HasPrefix(landing, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("copytree: %s is a symlink escaping the tree (%s); refusing", linkRel, target)
	}
	for _, part := range strings.Split(landing, string(os.PathSeparator)) {
		if part == ".git" {
			return fmt.Errorf("copytree: %s is a symlink into .git (%s); it would point at the submit repository's own control data once recreated; refusing", linkRel, target)
		}
	}
	return nil
}
