// Package proc identifies processes by handle rather than by command-line
// pattern.
//
// v1 supervised with pkill -f "supervisor.sh". That pattern matched the
// invoking shell -- twice killing the operator's own session -- and it matched
// child subshells, which share argv with their parent and so produced two false
// "duplicate supervisor" alarms. Neither is a tuning problem: matching argv
// cannot distinguish a process from anything that merely mentions it.
//
// A PID alone is not an identity either: PIDs are recycled. Every reference
// therefore carries a start token, read from the kernel, that a recycled PID
// cannot reproduce.
package proc

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Ref is a durable reference to a process: a PID plus the kernel's start token
// for that PID at the time the reference was taken.
type Ref struct {
	PID        int       `json:"pid"`
	StartToken string    `json:"start_token"`
	StartedAt  time.Time `json:"started_at"`
	// Role and Command are descriptive only. Nothing is ever identified by
	// them.
	Role    string `json:"role,omitempty"`
	Command string `json:"command,omitempty"`
}

func (r Ref) String() string {
	if r.PID == 0 {
		return "<no process>"
	}
	return fmt.Sprintf("pid %d (start token %s)", r.PID, r.StartToken)
}

// startToken returns an opaque, per-process-instance token for pid. On Linux it
// is field 22 of /proc/<pid>/stat, the process start time in clock ticks since
// boot, which a recycled PID cannot reproduce.
//
// It returns ok=false when the process does not exist.
func startToken(pid int) (token string, ok bool, err error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	// comm (field 2) is parenthesised and may contain spaces, so fields are
	// counted from the last ')'.
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return "", false, fmt.Errorf("proc: /proc/%d/stat is not in the expected format", pid)
	}
	fields := strings.Fields(s[i+1:])
	// After ')' the fields are state(3), ppid(4), ... starttime(22), so
	// starttime is index 19 of this slice.
	const startTimeIdx = 19
	if len(fields) <= startTimeIdx {
		return "", false, fmt.Errorf("proc: /proc/%d/stat has %d fields after comm, want more than %d", pid, len(fields), startTimeIdx)
	}
	return fields[startTimeIdx], true, nil
}

// Ppid returns the parent pid recorded for pid, so status can show the real
// process tree rather than inferring one from argv.
func Ppid(pid int) (int, bool, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return 0, false, fmt.Errorf("proc: /proc/%d/stat is not in the expected format", pid)
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 2 {
		return 0, false, fmt.Errorf("proc: /proc/%d/stat has too few fields", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false, fmt.Errorf("proc: parsing ppid of %d: %w", pid, err)
	}
	return ppid, true, nil
}

// Take builds a Ref for pid, reading its start token now.
func Take(pid int, role, command string) (Ref, error) {
	tok, ok, err := startToken(pid)
	if err != nil {
		return Ref{}, err
	}
	if !ok {
		return Ref{}, fmt.Errorf("proc: no process with pid %d", pid)
	}
	return Ref{PID: pid, StartToken: tok, StartedAt: time.Now().UTC(), Role: role, Command: command}, nil
}

// Self is a Ref to the current process.
func Self(role string) (Ref, error) {
	cmd := ""
	if len(os.Args) > 0 {
		cmd = strings.Join(os.Args, " ")
	}
	return Take(os.Getpid(), role, cmd)
}

// Alive reports whether the referenced process -- that exact instance -- is
// still running. A recycled PID reports false, because its start token differs.
//
// A Ref with no start token is refused rather than downgraded to a PID check:
// a check that silently weakens is worse than one that fails.
func (r Ref) Alive() (bool, error) {
	if r.PID <= 0 {
		return false, nil
	}
	if r.StartToken == "" {
		return false, fmt.Errorf("proc: ref to pid %d has no start token; liveness cannot be established", r.PID)
	}
	tok, ok, err := startToken(r.PID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if tok != r.StartToken {
		// The PID exists but belongs to a different process now.
		return false, nil
	}
	return true, nil
}

// Signal sends sig to the referenced process, but only if it is still the same
// process instance. This is the safe replacement for pkill -f.
func (r Ref) Signal(sig syscall.Signal) error {
	alive, err := r.Alive()
	if err != nil {
		return err
	}
	if !alive {
		return fmt.Errorf("proc: %s is not running; refusing to signal", r)
	}
	p, err := os.FindProcess(r.PID)
	if err != nil {
		return err
	}
	return p.Signal(sig)
}
