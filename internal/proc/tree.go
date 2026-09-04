//go:build linux

package proc

import (
	"os"
	"strconv"
)

// Node is one process in a tree read from /proc: pid and structure, and
// nothing the process supplied. argv, /proc/<pid>/comm (writable by the
// process, and via prctl(PR_SET_NAME)) and even the executable path (a
// process can exec a copy of itself named anything) are all channels a
// child holding a credential could encode it through, and the status page
// that shows this tree is unauthenticated by design. Labels, if any, come
// from what the caller itself recorded about a pid.
type Node struct {
	PID      int     `json:"pid"`
	Children []*Node `json:"children,omitempty"`
}

// Tree returns the process tree rooted at pid, as /proc shows it now. A
// pid that does not exist yields nil. It reads; it never signals.
func Tree(pid int) (*Node, error) {
	byParent := map[int][]int{}
	seen := map[int]bool{}
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
		seen[p] = true
	}
	if !seen[pid] {
		return nil, nil
	}
	var build func(int, int) *Node
	build = func(p, depth int) *Node {
		n := &Node{PID: p}
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
