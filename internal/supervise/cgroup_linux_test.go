//go:build linux

package supervise_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aicix-labs/factoryd/internal/supervise"
)

// alive reports the pid and command line of any OTHER process whose command
// line contains needle, "" if none. The test's own process, its parent
// chain and anything that merely quotes the needle (a shell that launched
// this test, say) are not the descendant being looked for: only a process
// whose argv[0] is sleep counts.
func alive(needle string) string {
	des, _ := os.ReadDir("/proc")
	for _, d := range des {
		b, err := os.ReadFile(filepath.Join("/proc", d.Name(), "cmdline"))
		if err != nil {
			continue
		}
		argv := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		if len(argv) >= 2 && filepath.Base(argv[0]) == "sleep" && argv[1] == needle {
			return d.Name() + " " + strings.Join(argv, " ")
		}
	}
	return ""
}

// The reviewer's case, for real: a leader that exits clean after starting a
// setsid'd descendant -- a new session, outside the process group, which a
// group-based reaper cannot see. The cgroup holds it: it is killed, verified
// gone, and the turn is reported leftover. Root only, because only root can
// create the cgroup; run under sudo, this is the evidence.
func TestSetsidDescendantIsContainedByTheCgroup(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: a per-turn cgroup is root's to create (run this test under sudo for the evidence)")
	}
	const marker = "30.0713"
	_, r, _ := execFixture(t, []string{"sh", "-c", "setsid sleep " + marker + " >/dev/null 2>&1 </dev/null & sleep 0.2; exit 0"}, 20)
	res, err := r.Run(context.Background(), supervise.Turn{ID: "t-setsid", Role: "reviewer"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Containment != "cgroup" {
		t.Fatalf("containment=%q, want cgroup under root", res.Containment)
	}
	if !res.Leftover {
		t.Fatal("the setsid'd descendant was not reported as leftover")
	}
	deadline := time.Now().Add(2 * time.Second)
	for alive(marker) != "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if who := alive(marker); who != "" {
		t.Fatalf("the setsid'd descendant is still running after the turn (%s); the cgroup did not contain it", who)
	}
	// Positive control: a quiescent turn under the same containment.
	_, r2, _ := execFixture(t, []string{"true"}, 10)
	res, err = r2.Run(context.Background(), supervise.Turn{ID: "t-quiet", Role: "reviewer"}, nil)
	if err != nil || res.Leftover || res.Containment != "cgroup" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// Unprivileged, the fallback is named, never mistaken for containment.
func TestUnprivilegedContainmentIsNamedProcessGroup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	_, r, _ := execFixture(t, []string{"true"}, 10)
	res, err := r.Run(context.Background(), supervise.Turn{ID: "t", Role: "reviewer"}, nil)
	if err != nil || res.Containment != "process-group" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}
