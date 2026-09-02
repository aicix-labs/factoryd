package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aicix-labs/factoryd/internal/scm/httpfixture"
)

// Provenance summarises where a provider's fixtures came from.
type Provenance struct {
	Recorded   []string
	HandOrigin map[string]string // scenario -> stated reason
}

// Unrecorded returns the hand-written scenarios, sorted.
func (p Provenance) Unrecorded() []string {
	out := make([]string, 0, len(p.HandOrigin))
	for name := range p.HandOrigin {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// String renders the split for a test log.
func (p Provenance) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d recorded from a live provider, %d hand-written\n",
		len(p.Recorded), len(p.HandOrigin))
	for _, name := range p.Unrecorded() {
		fmt.Fprintf(&sb, "  hand-written  %-28s %s\n", name, p.HandOrigin[name])
	}
	return sb.String()
}

// ReadProvenance loads every fixture in dir and reports its origin.
//
// The suite cannot fail a hand-written fixture -- some cases genuinely cannot
// be provoked on a live provider -- but it can refuse to let the distinction go
// unstated. A fixture written from documentation describes what the API was
// believed to do, and a driver that matches it exactly passes every test above
// it while being wrong about the provider.
func ReadProvenance(dir string) (Provenance, error) {
	p := Provenance{HandOrigin: map[string]string{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return p, err
	}
	seen := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		b, err := httpfixture.Load(dir, name)
		if err != nil {
			return p, err
		}
		seen++
		if b.Source.Recorded {
			p.Recorded = append(p.Recorded, name)
			continue
		}
		p.HandOrigin[name] = b.Source.Note
	}
	if seen == 0 {
		// An empty directory would otherwise report perfect provenance.
		return p, fmt.Errorf("no fixtures found in %s", filepath.Clean(dir))
	}
	sort.Strings(p.Recorded)
	return p, nil
}

// ScanForSecrets reads every fixture in dir and reports anything that looks
// like a credential.
//
// The recorder already refuses to write a leak, but only against the values
// that run registered. This scans what is actually committed, with no list at
// all, so a fixture added by hand -- or recorded before the guard existed -- is
// still checked. A backstop that only runs at the moment of writing is not a
// backstop.
func ScanForSecrets(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var problems []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range httpfixture.Leaks(string(raw), nil) {
			problems = append(problems, fmt.Sprintf("%s: %s", e.Name(), f))
		}
	}
	sort.Strings(problems)
	return problems, nil
}
