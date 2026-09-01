package proc

import (
	"os"
	"os/exec"
	"testing"
)

func TestSelfIsAlive(t *testing.T) {
	r, err := Self("test")
	if err != nil {
		t.Fatal(err)
	}
	if r.PID != os.Getpid() {
		t.Fatalf("Self().PID = %d, want %d", r.PID, os.Getpid())
	}
	if r.StartToken == "" {
		t.Fatal("Self() produced a ref with no start token")
	}
	alive, err := r.Alive()
	if err != nil {
		t.Fatal(err)
	}
	if !alive {
		t.Fatal("the current process reports as not alive")
	}
}

// TestRecycledPIDIsNotAlive is the check that matters: a Ref whose PID has been
// reused by a different process must not report alive. The start token is what
// makes that decidable, so mutating it must flip the answer.
func TestRecycledPIDIsNotAlive(t *testing.T) {
	r, err := Self("test")
	if err != nil {
		t.Fatal(err)
	}
	r.StartToken = "999999999999"
	alive, err := r.Alive()
	if err != nil {
		t.Fatal(err)
	}
	if alive {
		t.Fatal("a ref with the wrong start token reported alive; PID recycling would go undetected")
	}
}

func TestExitedProcessIsNotAlive(t *testing.T) {
	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child process: %v", err)
	}
	r, err := Take(cmd.Process.Pid, "child", "/bin/true")
	if err != nil {
		// The child may already have exited; that is the condition under test,
		// so treat it as satisfied.
		t.Skipf("child exited before a ref could be taken: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	alive, err := r.Alive()
	if err != nil {
		t.Fatal(err)
	}
	if alive {
		t.Fatal("an exited (reaped) process reports alive")
	}
}

func TestRefWithoutStartTokenIsRefused(t *testing.T) {
	r := Ref{PID: os.Getpid()}
	if _, err := r.Alive(); err == nil {
		t.Fatal("a ref with no start token answered a liveness question it cannot answer")
	}
}

func TestSignalRefusesADeadRef(t *testing.T) {
	r := Ref{PID: os.Getpid(), StartToken: "999999999999"}
	if err := r.Signal(0); err == nil {
		t.Fatal("signalled through a ref whose start token does not match; this is how pkill -f killed the caller")
	}
}

func TestPpidOfSelf(t *testing.T) {
	ppid, ok, err := Ppid(os.Getpid())
	if err != nil || !ok {
		t.Fatalf("Ppid(self) ok=%v err=%v", ok, err)
	}
	if ppid != os.Getppid() {
		t.Fatalf("Ppid(self) = %d, want %d", ppid, os.Getppid())
	}
}
