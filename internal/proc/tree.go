//go:build linux

package proc

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Node is one process in a tree read from /proc. It carries the executable
// name (/proc/<pid>/comm), never the command line: arguments routinely hold
// tokens and sensitive input, and the status page that shows this tree is
// unauthenticated by design.
type Node struct {
	PID      int     `json:"pid"`
	Exe      string  `json:"exe"`
	Children []*Node `json:"children,omitempty"`
}

// Tree returns the process tree rooted at pid, as /proc shows it now. A
// pid that does not exist yields nil. It reads; it never signals.
func Tree(pid int) (*Node, error) {
	byParent := map[int][]int{}
	cmd := map[int]string{}
	des, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	for _, de := range des {
		p, err := strconv.Atoi(de.Name())
		if err != nil {
			continue
		}
		ppid, ok, err := Ppid(p)
		if err != nil || !ok {
			continue
		}
		byParent[ppid] = append(byParent[ppid], p)
		cmd[p] = exeOf(p)
	}
	if _, ok := cmd[pid]; !ok {
		return nil, nil
	}
	var build func(int, int) *Node
	build = func(p, depth int) *Node {
		n := &Node{PID: p, Exe: cmd[p]}
		if depth > 32 {
			return n
		}
		for _, c := range byParent[p] {
			n.Children = append(n.Children, build(c, depth+1))
		}
		return n
	}
	return build(pid, 0), nil
}

// exeOf is the kernel's short executable name. It is what a process is,
// not what it was told; the latter is never read here.
func exeOf(pid int) string {
	c, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(c))
}
