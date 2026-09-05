package supervise_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// The shipped turn wrapper derives a turn's exit code from the progress
// marker, because an agent CLI exits 0 whatever the model concluded (#27).
// Each way it can be wrong is a case: progress moved → 0; progress did not
// move → 1 even though the agent exited 0; the agent's own non-zero exit
// passes through; not under a supervisor → 3.
func TestTurnWrapperDerivesTheExitCodeFromProgress(t *testing.T) {
	wrapper, err := filepath.Abs(filepath.Join("..", "..", "examples", "turn-wrapper.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid")
	}
	progress := filepath.Join(t.TempDir(), "producer-progress")
	os.WriteFile(progress, nil, 0o644)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(progress, old, old)
	run := func(agent string) int {
		cmd := exec.Command("sh", wrapper, "sh", "-c", agent)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "FACTORYD_PROGRESS=" + progress}
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode()
			}
			t.Fatal(err)
		}
		return 0
	}
	// The same-second case, made deterministic: the baseline is an exact
	// second and the agent's touch is one nanosecond later. Real progress;
	// a whole-second compare would call it none.
	os.Chtimes(progress, time.Unix(1700000000, 0), time.Unix(1700000000, 0))
	if code := run(`touch -d @1700000000.000000001 "$FACTORYD_PROGRESS"; exit 0`); code != 0 {
		t.Fatalf("progress within the same second: wrapper exited %d, want 0 (nanosecond precision)", code)
	}
	os.Chtimes(progress, old, old)
	if code := run("exit 0"); code != 1 {
		t.Fatalf("agent exited 0 without progress: wrapper exited %d, want 1", code)
	}
	if code := run("touch \"$FACTORYD_PROGRESS\"; exit 0"); code != 0 {
		t.Fatalf("agent progressed: wrapper exited %d, want 0", code)
	}
	if code := run("touch \"$FACTORYD_PROGRESS\"; exit 7"); code != 7 {
		t.Fatalf("agent exited 7: wrapper exited %d, want 7 passed through", code)
	}
	cmd := exec.Command("sh", wrapper, "true")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if err := cmd.Run(); err == nil {
		t.Fatal("outside a supervisor (no FACTORYD_PROGRESS) the wrapper must refuse")
	}
}

// The wrapped leader can exit cleanly while an agent helper keeps running.
// Reaping that helper before the wrapper returns is different from the
// runner's post-return containment: the next agent must never get a window to
// contend with its predecessor for shared state such as an OAuth refresh.
func TestTurnWrapperReapsAgentHelpersBeforeReturning(t *testing.T) {
	wrapper, err := filepath.Abs(filepath.Join("..", "..", "examples", "turn-wrapper.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid")
	}
	progress := filepath.Join(t.TempDir(), "progress")
	childPID := filepath.Join(t.TempDir(), "helper-pid")
	if err := os.WriteFile(progress, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", wrapper, "sh", "-c", `sleep 60 & echo "$!" > "$HELPER_PID"; touch "$FACTORYD_PROGRESS"`)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"FACTORYD_PROGRESS=" + progress,
		"HELPER_PID=" + childPID,
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("wrapper exited %v: %s", err, out)
	}
	b, err := os.ReadFile(childPID)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(b[:len(b)-1]))
	if err != nil {
		t.Fatalf("helper pid %q: %v", b, err)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("agent helper pid %d still exists after the wrapper returned: %v", pid, err)
	}
}
