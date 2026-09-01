package conformance

import (
	"strings"
	"testing"
)

// The verb-coverage check is the suite's guard against a driver method that no
// scenario exercises -- the shape a "not implemented" stub hides in. Both sides
// of its comparison are derived (one by reflection over scm.Driver, one by
// observing what the scenarios actually called), so it fails by construction
// when a method is added. These tests are its positive control: they show it
// reporting a gap, and show it staying quiet when there is none.

func TestVerbCoverageReportsAMissingVerb(t *testing.T) {
	full := map[string]bool{}
	for _, v := range interfaceVerbs() {
		full[v] = true
	}
	if err := checkVerbCoverage(full); err != nil {
		t.Fatalf("full coverage reported a gap: %v", err)
	}

	// Drop one verb; the check must name it. Without this, a coverage check
	// that always returned nil would look identical from the outside.
	for _, dropped := range interfaceVerbs() {
		partial := map[string]bool{}
		for k, v := range full {
			if k != dropped {
				partial[k] = v
			}
		}
		err := checkVerbCoverage(partial)
		if err == nil {
			t.Errorf("coverage passed with %s unexercised", dropped)
			continue
		}
		if !strings.Contains(err.Error(), dropped) {
			t.Errorf("coverage failed but did not name %s: %v", dropped, err)
		}
	}
}

func TestVerbCoverageIsNotVacuous(t *testing.T) {
	verbs := interfaceVerbs()
	if len(verbs) == 0 {
		t.Fatal("reflection found no methods on scm.Driver; the coverage check would pass trivially")
	}
	// An empty observation set must fail. A check whose "nothing was measured"
	// case reads as success is the exact defect this file exists to prevent.
	err := checkVerbCoverage(map[string]bool{})
	if err == nil {
		t.Fatal("coverage passed when no verb had been exercised at all")
	}
	for _, v := range verbs {
		if !strings.Contains(err.Error(), v) {
			t.Errorf("the empty-coverage error does not name %s: %v", v, err)
		}
	}
}

// The recorder is what makes coverage observed rather than declared. If it
// forwarded a call without recording it, the verb would read as unexercised --
// the safe direction -- but the reverse, recording without forwarding, would be
// a lie. This asserts a fresh recorder claims nothing.
func TestRecorderStartsEmpty(t *testing.T) {
	r := newRecorder(nil)
	if got := r.observed(); len(got) != 0 {
		t.Fatalf("a fresh recorder already claims %v", got)
	}
}
