//go:build !linux

package supervise

import (
	"errors"
	"syscall"
)

var ErrNoContainment = errors.New("per-turn containment is only implemented on Linux")

type turnCgroup struct{}

func newTurnCgroup(string, string, string) (*turnCgroup, error) { return nil, ErrNoContainment }
func (*turnCgroup) attr(*syscall.SysProcAttr)                   {}
func (*turnCgroup) killAll() (int, error)                       { return 0, nil }
func (*turnCgroup) close()                                      {}
func ProbeContainment() error                                   { return ErrNoContainment }
