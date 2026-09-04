// Package principal resolves the factory's provider identities in a
// factory-owned context and proves them distinct. Two files that hold the
// same token are two paths to one authority; only the provider's stable
// identity says who a credential is (#47 review).
package principal

import (
	"context"
	"errors"
	"fmt"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/scm"
)

// DriverBuilder builds a driver for a token.
type DriverBuilder func(cfg *config.Config, token string) (scm.Driver, error)

// Three is the resolved trio. Operator is set only when configured.
type Three struct {
	Producer, Reviewer scm.Identity
	Operator           *scm.Identity
}

// Resolve resolves every configured credential to a provider identity, in
// this process's own context: a token that cannot be read or does not
// authenticate is an error, never a role silently left out of the
// comparison. Then it proves the identities pairwise distinct by stable
// ID. Any equality is an error naming the pair.
func Resolve(ctx context.Context, cfg *config.Config, newDriver DriverBuilder) (Three, error) {
	if newDriver == nil {
		return Three{}, errors.New("principal: no driver builder")
	}
	who := func(name string, ref config.CredentialRef) (scm.Identity, error) {
		tok, err := ref.Resolve()
		if err != nil {
			return scm.Identity{}, fmt.Errorf("%s credential: %w", name, err)
		}
		d, err := newDriver(cfg, tok)
		if err != nil {
			return scm.Identity{}, fmt.Errorf("%s identity: %w", name, err)
		}
		id, err := d.Whoami(ctx)
		if err != nil {
			return scm.Identity{}, fmt.Errorf("%s identity: %w", name, err)
		}
		if id.ID == "" {
			return scm.Identity{}, fmt.Errorf("%s identity: the provider returned no stable id", name)
		}
		return id, nil
	}
	var t Three
	var err error
	if t.Producer, err = who("producer", cfg.Credentials.Producer); err != nil {
		return Three{}, err
	}
	if t.Reviewer, err = who("reviewer", cfg.Credentials.Reviewer); err != nil {
		return Three{}, err
	}
	if t.Producer.ID == t.Reviewer.ID {
		return Three{}, fmt.Errorf("producer and reviewer both authenticate as %s; the producer could merge its own work", t.Producer)
	}
	if o := cfg.Credentials.Operator; o.File != "" || o.Env != "" {
		id, err := who("operator", o)
		if err != nil {
			return Three{}, err
		}
		if id.ID == t.Reviewer.ID {
			return Three{}, fmt.Errorf("operator and reviewer both authenticate as %s: two files, one authority; the reviewer could close as the operator", id)
		}
		if id.ID == t.Producer.ID {
			return Three{}, fmt.Errorf("operator and producer both authenticate as %s: two files, one authority", id)
		}
		t.Operator = &id
	}
	return t, nil
}
