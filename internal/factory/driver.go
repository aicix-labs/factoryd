// Package factory wires configuration to a provider driver.
package factory

import (
	"fmt"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/scm/github"
	"github.com/aicix-labs/factoryd/internal/scm/gitlab"
)

// NewDriver builds the driver named by cfg.Provider, authenticated with token.
//
// The token is always an explicit argument. There is no ambient credential and
// no fallback: the producer must never end up operating as the reviewer because
// its own credential was missing.
func NewDriver(cfg *config.Config, token string) (scm.Driver, error) {
	if token == "" {
		return nil, fmt.Errorf("factory: refusing to build a driver with an empty token")
	}
	switch cfg.Provider {
	case "github":
		if cfg.GitHub == nil {
			return nil, fmt.Errorf("factory: provider is github but there is no github block")
		}
		return github.New(github.Config{
			BaseURL:     cfg.GitHub.BaseURL,
			GraphQLURL:  cfg.GitHub.GraphQLURL,
			Owner:       cfg.GitHub.Owner,
			Repo:        cfg.GitHub.Repo,
			MergeMethod: cfg.GitHub.MergeMethod,
			Token:       token,
		})
	case "gitlab":
		if cfg.GitLab == nil {
			return nil, fmt.Errorf("factory: provider is gitlab but there is no gitlab block")
		}
		return gitlab.New(gitlab.Config{
			BaseURL:            cfg.GitLab.BaseURL,
			Project:            cfg.GitLab.Project,
			RemoveSourceBranch: cfg.GitLab.RemoveSourceBranch,
			Token:              token,
		})
	default:
		return nil, fmt.Errorf("factory: provider %q is not github or gitlab", cfg.Provider)
	}
}
