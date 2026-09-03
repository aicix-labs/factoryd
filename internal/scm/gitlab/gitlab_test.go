package gitlab_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/scm/conformance"
	"github.com/aicix-labs/factoryd/internal/scm/gitlab"
)

// Factory is exported to the conformance control test.
func Factory() conformance.Factory {
	return conformance.Factory{
		Provider:   "gitlab",
		FixtureDir: "testdata",
		// The provider's own wording, taken from the recorded fixture.
		GitUsername:        "oauth2",
		UnmergeableMessage: "Branch cannot be merged",
		New: func(hc *http.Client) (scm.Driver, error) {
			return gitlab.New(gitlab.Config{
				BaseURL: "https://gitlab.example.com/api/v4",
				Project: "acme/widgets", Token: "test-token", HTTPClient: hc,
			})
		},
	}
}

func TestConformance(t *testing.T) {
	for _, r := range conformance.Check(context.Background(), Factory()) {
		if r.Err != nil {
			t.Errorf("%s: %v", r.Scenario, r.Err)
		}
	}
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	cases := map[string]gitlab.Config{
		"no project": {Token: "t"},
		"no token":   {Project: "acme/widgets"},
	}
	for name, cfg := range cases {
		if _, err := gitlab.New(cfg); err == nil {
			t.Errorf("%s: New accepted an invalid config", name)
		}
	}
}
