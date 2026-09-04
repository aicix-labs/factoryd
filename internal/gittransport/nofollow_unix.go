//go:build unix

package gittransport

import (
	"os"
	"path/filepath"
	"syscall"
)

// OpenNoFollow opens name, a single path component, relative to the open
// directory dir, refusing to follow it if it is a symlink: openat(2) with
// O_NOFOLLOW on the descriptor, and nothing in between. os.Root is not used
// for this on purpose -- it resolves an in-root symlink itself before the
// final open, so O_NOFOLLOW never sees the link and a control file linked to
// another file in the tree is read through. The error for a symlink is
// ELOOP. O_NONBLOCK keeps a fifo from blocking the caller; the caller judges
// what it opened by fstat on the descriptor.
func OpenNoFollow(dir *os.File, name string, flags int) (*os.File, error) {
	fd, err := syscall.Openat(int(dir.Fd()), name, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

// PhysicalPrefix resolves p through its deepest existing ancestor: the
// symlinks of what exists are followed, and the part that does not exist
// yet is appended lexically. It is what a path will BE once created.
func PhysicalPrefix(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		parent, base := filepath.Split(cur)
		parent = filepath.Clean(parent)
		if parent == cur {
			return filepath.Join(cur, rest)
		}
		rest = filepath.Join(base, rest)
		cur = parent
	}
}
