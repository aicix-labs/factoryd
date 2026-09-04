//go:build linux

package proc

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Node is one process in a tree read from /proc.
type Node struct {
	PID      int     `json:"pid"`
	Command  string  `json:"command"`
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
		cmd[p] = commandOf(p)
	}
	if _, ok := cmd[pid]; !ok {
		return nil, nil
	}
	var build func(int, int) *Node
	build = func(p, depth int) *Node {
		n := &Node{PID: p, Command: cmd[p]}
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

func commandOf(pid int) string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil || len(b) == 0 {
		if c, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm")); err == nil {
			return strings.TrimSpace(string(c))
		}
		return ""
	}
	return strings.TrimRight(strings.ReplaceAll(string(b), "\x00", " "), " ")
}
