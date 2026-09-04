// Package signal records the reviewer's verdict and, for "merged", IS the
// merge gate (SPEC.md §2, §6.4): scope policy as data decides what may merge
// automatically, what needs a recorded adversarial pass, and what a human
// must approve -- enforced here, not by convention in a playbook.
package signal

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/scm"
)

// Class is what the policy says about a change.
type Class int

const (
	// Mergeable: no rule fired; CI and the reviewer decide.
	Mergeable Class = iota
	// Escalate: a recorded adversarial audit on the exact head is required.
	Escalate
	// OperatorOnly: a deny path or held content; a human must approve.
	OperatorOnly
)

func (c Class) String() string {
	switch c {
	case Mergeable:
		return "mergeable"
	case Escalate:
		return "escalate"
	case OperatorOnly:
		return "operator-only"
	}
	return fmt.Sprintf("Class(%d)", int(c))
}

// Decision is the policy's answer, with every reason so the reviewer can
// see which rule fired on which path or line.
type Decision struct {
	Class   Class
	Reasons []string
}

// Classify applies the scope policy to a change's diff, in this order: a
// path matching a deny and no allow is operator-only; an ADDED line
// matching a hold pattern is operator-only whatever its path; a path
// matching an escalate pattern requires an audit. The strictest class wins
// and every reason is kept. A rename is judged on both names; a deletion on
// its path (deleting the CI config is as gated as editing it).
func Classify(scope *config.Scope, diffs []scm.FileDiff) Decision {
	deny, allow, hold, escalate := scope.Compiled()
	d := Decision{}
	raise := func(c Class, format string, args ...any) {
		if c > d.Class {
			d.Class = c
		}
		d.Reasons = append(d.Reasons, fmt.Sprintf(format, args...))
	}
	for _, f := range diffs {
		for _, path := range pathsOf(f) {
			if re := firstMatch(deny, path); re != nil {
				if ex := firstMatch(allow, path); ex != nil {
					d.Reasons = append(d.Reasons, fmt.Sprintf("%s matches deny %q but is exempted by allow %q", path, re, ex))
				} else {
					raise(OperatorOnly, "%s matches deny %q", path, re)
				}
			}
			if re := firstMatch(escalate, path); re != nil {
				raise(Escalate, "%s matches escalate %q", path, re)
			}
		}
		for n, line := range addedLines(f.Patch) {
			if re := firstMatch(hold, line); re != nil {
				raise(OperatorOnly, "%s: added line %d matches hold %q", f.Path, n, re)
			}
		}
	}
	sort.Strings(d.Reasons)
	return d
}

func pathsOf(f scm.FileDiff) []string {
	if f.Renamed && f.OldPath != "" && f.OldPath != f.Path {
		return []string{f.Path, f.OldPath}
	}
	return []string{f.Path}
}

func firstMatch(res []*regexp.Regexp, s string) *regexp.Regexp {
	for _, re := range res {
		if re.MatchString(s) {
			return re
		}
	}
	return nil
}

// addedLines returns the lines a unified diff adds, numbered from 1 within
// the patch. Header lines ("+++ b/path") are not additions.
func addedLines(patch string) map[int]string {
	out := map[int]string{}
	n := 0
	for _, l := range strings.Split(patch, "\n") {
		if strings.HasPrefix(l, "+++") {
			continue
		}
		if strings.HasPrefix(l, "+") {
			n++
			out[n] = l[1:]
		}
	}
	return out
}
