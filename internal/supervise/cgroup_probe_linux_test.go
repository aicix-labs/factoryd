//go:build linux

package supervise

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The probe must be able to fail in each way a host can fail a turn:
// clone-into-cgroup denied (placement withheld), and a kill that the
// hierarchy accepts but does not perform (a file that takes "1" and kills
// nothing). Root only; run under sudo for the evidence.
func TestContainmentProbeFailsForEachWayAHostCan(t *testing.T) {
	if os.Geteuid() != 0 {
		if err := ProbeContainment(); !errors.Is(err, ErrNoContainment) {
			t.Fatalf("unprivileged probe returned %v, want ErrNoContainment", err)
		}
		t.Skip("the rest needs root")
	}
	if err := ProbeContainment(); err != nil {
		t.Fatalf("real probe failed on a host with cgroup v2 + cgroup.kill: %v", err)
	}
	// Placement withheld: the child runs outside the cgroup.
	probePlace = false
	err := ProbeContainment()
	probePlace = true
	if err == nil || !strings.Contains(err.Error(), "not in the cgroup") {
		t.Fatalf("placement withheld: err=%v; the probe passed without a placed child", err)
	}
	// An ineffective kill: cgroup.max.depth accepts "1" and kills nothing.
	probeKillFile = "cgroup.max.depth"
	err = ProbeContainment()
	probeKillFile = "cgroup.kill"
	if err == nil || !(strings.Contains(err.Error(), "still") || strings.Contains(err.Error(), "not effective")) {
		t.Fatalf("ineffective kill: err=%v; the probe passed without the child dying", err)
	}
	// A child gone before the kill -- killed and reaped deterministically,
	// not raced: the kill finds nothing, and "nothing to kill" is not
	// containment shown.
	probeBeforeKill = func(cmd *exec.Cmd) { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }
	err = ProbeContainment()
	probeBeforeKill = nil
	if err == nil || !strings.Contains(err.Error(), "nothing to kill") {
		t.Fatalf("child gone before the kill: err=%v; an empty cgroup was judged contained", err)
	}
	// And after the failures, no probe cgroup and no child remain.
	if des, _ := os.ReadDir("/sys/fs/cgroup/factoryd/doctor"); len(des) > 0 {
		for _, d := range des {
			if d.IsDir() {
				t.Fatalf("probe cgroup %s left behind", d.Name())
			}
		}
	}
}
