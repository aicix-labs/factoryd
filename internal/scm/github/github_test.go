package github_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/scm/conformance"
	"github.com/aicix-labs/factoryd/internal/scm/github"
)

// Factory is exported to the conformance control test so the suite can be run
// against a deliberately broken wrapper of this driver.
func Factory() conformance.Factory {
	return conformance.Factory{
		Provider:   "github",
		FixtureDir: "testdata",
		// The provider's own wording, taken from the recorded fixture.
		UnmergeableMessage: "Pull Request has merge conflicts",
		New: func(hc *http.Client) (scm.Driver, error) {
			return github.New(github.Config{
				BaseURL: "https://api.github.com", Owner: "acme", Repo: "widgets",
				Token: "test-token", HTTPClient: hc,
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
	cases := map[string]github.Config{
		"no owner":         {Repo: "widgets", Token: "t"},
		"no repo":          {Owner: "acme", Token: "t"},
		"no token":         {Owner: "acme", Repo: "widgets"},
		"bad merge method": {Owner: "acme", Repo: "widgets", Token: "t", MergeMethod: "fast-forward"},
	}
	for name, cfg := range cases {
		if _, err := github.New(cfg); err == nil {
			t.Errorf("%s: New accepted an invalid config", name)
		}
	}
}
