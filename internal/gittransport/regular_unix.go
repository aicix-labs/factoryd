//go:build unix

package gittransport

import (
	"fmt"
	"os"
	"syscall"
)

// RequireOwnRegular judges an opened descriptor: it must be a regular file
// with exactly one link. A regular descriptor is not proof the file is the
// producer's -- a hard link to a credential is a regular file too, with the
// credential's inode, on any deployment whose directory permissions and
// hard-link policy allow the link. A link count above one means the same
// inode is reachable from somewhere else, and this crossing does not copy or
// read it.
func RequireOwnRegular(f *os.File, name string) (os.FileInfo, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (%v)", name, fi.Mode().Type())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("%s: no stat available to count links", name)
	}
	if st.Nlink != 1 {
		return nil, fmt.Errorf("%s has %d links; a file reachable from elsewhere is not the producer's to submit", name, st.Nlink)
	}
	return fi, nil
}
