//go:build linux

package supervise

import (
	"errors"
	"os"
	"syscall"
)

// ErrNoNetworkNeedsRoot is returned when a turn asks for a network
// namespace and factoryd cannot create one.
var ErrNoNetworkNeedsRoot = errors.New("a new network namespace needs root; refusing to start the turn connected")

// applyNoNetwork asks the kernel for a fresh network namespace at clone.
// The namespace has a downed loopback and nothing else. It is created by
// the parent's privilege, so it is applied whether or not a Credential
// later drops that privilege in the child.
func applyNoNetwork(attr *syscall.SysProcAttr) error {
	if os.Geteuid() != 0 {
		return ErrNoNetworkNeedsRoot
	}
	attr.Cloneflags |= syscall.CLONE_NEWNET
	return nil
}
