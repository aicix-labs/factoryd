//go:build unix

package gittransport

import (
	"os"
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
