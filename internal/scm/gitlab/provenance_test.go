package gitlab_test

import (
	"testing"

	"github.com/aicix-labs/factoryd/internal/scm/conformance"
)

// TestFixtureProvenance reports where this driver's fixtures came from.
//
// It cannot fail a hand-written fixture -- some cases genuinely cannot be
// provoked on a live provider -- but every fixture must say which it is and,
// when hand-written, why. A fixture written from documentation describes what
// the API was believed to do; a driver that matches it exactly passes every
// test above it while being wrong about the provider. That is a check that can
// fail against the wrong reality, and the only defence is not letting the
// distinction go unstated.
//
// Load() rejects a fixture with no source, so the failure mode this guards
// against is a new fixture appearing with its provenance unrecorded.
func TestFixtureProvenance(t *testing.T) {
	p, err := conformance.ReadProvenance("testdata")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + p.String())

	if len(p.Recorded) == 0 {
		t.Error("no fixture in this package was recorded from a live provider; the whole suite is testing the driver against documentation")
	}
	for _, name := range p.Unrecorded() {
		if p.HandOrigin[name] == "" {
			t.Errorf("fixture %s is hand-written with no stated reason", name)
		}
	}
}

// TestNoSecretsInFixtures scans what is committed, with no list of expected
// values. The recorder's own guard only runs at the moment of writing and only
// against the values that run registered; this checks the files themselves.
func TestNoSecretsInFixtures(t *testing.T) {
	problems, err := conformance.ScanForSecrets("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("credential-shaped content in a committed fixture: %s", p)
	}
}
