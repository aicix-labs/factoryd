//go:build linux

package proc_test

import (
	"os"
	"os/exec"
	"testing"

	"github.com/aicix-labs/factoryd/internal/proc"
)

// The tree is read from /proc as it is now: a child this test spawns is in
// it, and a child that has exited is not.
func TestTreeShowsLiveChildren(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("no sleep: %v", err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	n, err := proc.Tree(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if n == nil || n.PID != os.Getpid() {
		t.Fatalf("root=%+v", n)
	}
	found := false
	for _, c := range n.Children {
		if c.PID == cmd.Process.Pid {
			found = true
			if c.Exe == "" {
				t.Fatal("child has no command line")
			}
		}
	}
	if !found {
		t.Fatalf("spawned child %d not in tree %+v", cmd.Process.Pid, n.Children)
	}
	cmd.Process.Kill()
	cmd.Wait()
	n, _ = proc.Tree(os.Getpid())
	for _, c := range n.Children {
		if c.PID == cmd.Process.Pid {
			t.Fatal("an exited child is still shown")
		}
	}
	if n, _ := proc.Tree(1 << 30); n != nil {
		t.Fatal("a pid that does not exist has a tree")
	}
}
