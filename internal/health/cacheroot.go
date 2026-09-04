//go:build unix

package health

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aicix-labs/factoryd/internal/config"
)

// CacheRoot is the one directory reclamation may delete in, held open. Every
// operation on it is relative to the handle and refuses to follow a symlink
// out of it, so the answer to "where does this delete" is decided by the
// directory that was opened and verified -- not by the path, which anything
// with write access to an ancestor can retarget between a check and the
// deletion. A rename of the root's parent and a symlink at the old name
// changes what the PATH means; it does not change what the HANDLE is.
type CacheRoot struct {
	dir  string
	root *os.Root
	dev  uint64
	ino  uint64
}

// betweenOpens is a test seam, nil in production.
var betweenOpens func()

// OpenCacheRoot opens dir without following a final symlink, verifies the
// opened directory is owned by this process's uid and writable by nobody
// else, then binds an os.Root to it and checks that the root refers to the
// same inode. The two opens are separate syscalls; the inode comparison is
// what makes a swap between them a refusal rather than a window.
func OpenCacheRoot(dir string) (*CacheRoot, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("cache root %q is not absolute", dir)
	}
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if err == syscall.ELOOP || err == syscall.ENOTDIR {
			return nil, fmt.Errorf("cache root %s is a symlink or not a directory; it must be a real directory", dir)
		}
		return nil, fmt.Errorf("open cache root %s: %w", dir, err)
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("fstat cache root %s: %w", dir, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return nil, fmt.Errorf("cache root %s is not a directory", dir)
	}
	if int(st.Uid) != os.Getuid() {
		return nil, fmt.Errorf("cache root %s is owned by uid %d, not the uid factoryd runs as (%d)", dir, st.Uid, os.Getuid())
	}
	if st.Mode&0o022 != 0 {
		return nil, fmt.Errorf("cache root %s is writable by group or others (%04o); anyone could plant a symlink for reclamation", dir, st.Mode&0o777)
	}
	if betweenOpens != nil {
		betweenOpens() // test seam: the world moves between the two opens
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("bind cache root %s: %w", dir, err)
	}
	fi, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, fmt.Errorf("stat bound cache root %s: %w", dir, err)
	}
	rst, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || uint64(rst.Dev) != uint64(st.Dev) || rst.Ino != st.Ino {
		root.Close()
		return nil, fmt.Errorf("cache root %s changed between verification and binding; refusing", dir)
	}
	return &CacheRoot{dir: dir, root: root, dev: uint64(st.Dev), ino: st.Ino}, nil
}

// Close releases the handle.
func (c *CacheRoot) Close() error { return c.root.Close() }

// Rel returns the path of an absolute cache inside the root, relative to
// the handle. Containment was checked at load; here it is only a shape.
func (c *CacheRoot) Rel(abs string) (string, error) {
	if !config.PathWithin(abs, c.dir) {
		return "", fmt.Errorf("%s is not inside the cache root %s", abs, c.dir)
	}
	rel, err := filepath.Rel(c.dir, abs)
	if err != nil {
		return "", err
	}
	return rel, nil
}
