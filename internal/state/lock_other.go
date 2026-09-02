//go:build !unix

package state

import "fmt"

// lock has no implementation on this platform.
//
// It returns an error rather than a no-op. A no-op lock would let two
// supervisors read-modify-write the same document and silently lose each
// other's updates -- a lock that cannot fail to be acquired is not a lock.
func lock(path string) (func(), error) {
	return nil, fmt.Errorf("state: file locking is not implemented on this platform; refusing to update %s unsynchronised", path)
}
