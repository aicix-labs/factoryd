//go:build linux

package supervise

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// cgroupRoot is where per-turn cgroups live under the unified hierarchy.
const cgroupRoot = "/sys/fs/cgroup/factoryd"

// ErrNoContainment is returned when factoryd is privileged but cannot
// contain a turn: a root that cannot create a cgroup must not start the
// turn on process groups alone, which a setsid'd child leaves at will.
var ErrNoContainment = errors.New("cannot create a per-turn cgroup (cgroup v2 with cgroup.kill is required); refusing to start the turn on a process group alone")

// turnCgroup is one turn's cgroup. Everything the turn starts -- however
// it re-parents, whatever session it makes for itself -- stays in it,
// because a process cannot leave its cgroup without write access to the
// hierarchy, which the turn does not have. That is the containment a
// process group cannot give: setsid(2) leaves the group; nothing leaves
// the cgroup.
type turnCgroup struct {
	path string
	fd   int
}

// newTurnCgroup creates the cgroup and opens it for CLONE_INTO_CGROUP.
func newTurnCgroup(factory, role, turnID string) (*turnCgroup, error) {
	if os.Geteuid() != 0 {
		return nil, ErrNoContainment
	}
	dir := filepath.Join(cgroupRoot, factory, role+"-"+turnID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoContainment, err)
	}
	// cgroup.kill is the whole point; a hierarchy without it (kernel < 5.14)
	// cannot promise a verified kill-all.
	if _, err := os.Stat(filepath.Join(dir, "cgroup.kill")); err != nil {
		_ = syscall.Rmdir(dir)
		return nil, fmt.Errorf("%w: no cgroup.kill in %s", ErrNoContainment, dir)
	}
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		_ = syscall.Rmdir(dir)
		return nil, fmt.Errorf("%w: %v", ErrNoContainment, err)
	}
	return &turnCgroup{path: dir, fd: fd}, nil
}

func (c *turnCgroup) attr(a *syscall.SysProcAttr) {
	a.UseCgroupFD = true
	a.CgroupFD = c.fd
}

// procs lists the pids still in the cgroup.
func (c *turnCgroup) procs() ([]string, error) {
	b, err := os.ReadFile(filepath.Join(c.path, "cgroup.procs"))
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(b)), nil
}

// killAll kills every process in the cgroup and waits until the kernel
// reports it empty. It returns how many were there to kill, and an error if
// the cgroup did not empty -- which is not a case to proceed past.
func (c *turnCgroup) killAll() (int, error) {
	left, err := c.procs()
	if err != nil {
		return 0, err
	}
	if len(left) == 0 {
		return 0, nil
	}
	if err := os.WriteFile(filepath.Join(c.path, "cgroup.kill"), []byte("1"), 0); err != nil {
		return len(left), fmt.Errorf("cgroup.kill: %w", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		now, err := c.procs()
		if err != nil {
			return len(left), err
		}
		if len(now) == 0 {
			return len(left), nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return len(left), fmt.Errorf("cgroup %s still holds processes after cgroup.kill", c.path)
}

// close releases the descriptor and removes the (now empty) cgroup.
func (c *turnCgroup) close() {
	_ = syscall.Close(c.fd)
	_ = syscall.Rmdir(c.path)
}

// ProbeContainment creates, kills and removes a throwaway cgroup, as doctor's
// evidence that a turn can be contained on this host.
func ProbeContainment() error {
	c, err := newTurnCgroup("doctor", "probe", fmt.Sprintf("%d", os.Getpid()))
	if err != nil {
		return err
	}
	defer c.close()
	if _, err := c.killAll(); err != nil {
		return err
	}
	return nil
}
