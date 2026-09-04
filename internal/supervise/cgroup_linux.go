//go:build linux

package supervise

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
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
func (c *turnCgroup) killAll() (int, error) { return c.killAllVia("cgroup.kill") }

func (c *turnCgroup) killAllVia(killFile string) (int, error) {
	left, err := c.procs()
	if err != nil {
		return 0, err
	}
	if len(left) == 0 {
		return 0, nil
	}
	if err := os.WriteFile(filepath.Join(c.path, killFile), []byte("1"), 0); err != nil {
		return len(left), fmt.Errorf("%s: %w", killFile, err)
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

// Seams for the containment probe's tests: placement can be withheld, and
// the kill can be pointed at a file that accepts the write but kills
// nothing. Production values are the real ones.
var (
	probePlace    = true
	probeKillFile = "cgroup.kill"
	probeChild    = []string{"/bin/sleep", "60"}
	// probeBeforeKill runs after placement is verified and before the kill;
	// a test uses it to take the child away first.
	probeBeforeKill func(*exec.Cmd)
)

// ProbeContainment is doctor's evidence that a turn can be contained on
// this host -- shown, not asserted. It creates a cgroup, starts a real
// child into it with the exact SysProcAttr a turn gets, requires the
// child's pid to appear in cgroup.procs, invokes the kill, requires the
// child to be dead and the cgroup empty, and removes it. A host where the
// cgroup can be made but clone-into-cgroup or cgroup.kill is denied fails
// here rather than at the first turn. An empty cgroup proves nothing, so
// the probe never judges one.
func ProbeContainment() error {
	c, err := newTurnCgroup("doctor", "probe", fmt.Sprintf("%d", os.Getpid()))
	if err != nil {
		return err
	}
	defer c.close()

	cmd := exec.Command(probeChild[0], probeChild[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if probePlace {
		c.attr(cmd.SysProcAttr)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("containment probe: could not start a child into the cgroup: %w", err)
	}
	// Whatever happens below, the child does not outlive the probe.
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	pid := fmt.Sprint(cmd.Process.Pid)
	procs, err := c.procs()
	if err != nil {
		return fmt.Errorf("containment probe: %w", err)
	}
	placed := false
	for _, p := range procs {
		if p == pid {
			placed = true
		}
	}
	if !placed {
		return fmt.Errorf("containment probe: the child (pid %s) is not in the cgroup (procs: %v); clone-into-cgroup is not working here", pid, procs)
	}

	// killAllVia is the same kill a turn gets, and it returns only when the
	// kernel reports the cgroup empty (or errors when it will not empty --
	// the ineffective-kill case). A process gone from cgroup.procs has
	// exited, so no separate liveness wait is needed; the deferred Wait
	// reaps it. What the kill can still fail to show is that it killed
	// anything: a child gone before the kill leaves nothing to kill, and
	// that is not evidence of containment.
	if probeBeforeKill != nil {
		probeBeforeKill(cmd)
	}
	n, err := c.killAllVia(probeKillFile)
	if err != nil {
		return fmt.Errorf("containment probe: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("containment probe: the kill found nothing to kill (the child had already gone); nothing was shown")
	}
	return nil
}
