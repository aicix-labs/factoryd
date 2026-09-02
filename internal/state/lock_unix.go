//go:build unix

package state

import (
	"fmt"
	"os"
	"syscall"
)

// lock takes an exclusive advisory lock on path+".lock".
//
// The state document is shared: the producer's supervisor and the reviewer's
// supervisor both write it. Update is a read-modify-write, so without this the
// later writer silently discards whatever the other one recorded in between --
// including a halt reason, which is the one field nobody can afford to lose.
//
// flock is released by the kernel when the process dies, so a supervisor killed
// mid-update leaves no stale lock to clear by hand.
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("state: opening the lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("state: locking %s: %w", f.Name(), err)
	}
	return func() {
		// Closing the descriptor releases the lock.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
