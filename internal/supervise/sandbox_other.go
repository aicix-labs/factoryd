//go:build !linux

package supervise

import (
	"errors"
	"syscall"
)

var ErrNoNetworkNeedsRoot = errors.New("sandbox.no_network is only implemented on Linux")

func applyNoNetwork(*syscall.SysProcAttr) error { return ErrNoNetworkNeedsRoot }
