package principal_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/principal"
	"github.com/aicix-labs/factoryd/internal/scm"
)

type idDriver struct {
	scm.Driver
	token string
	fail  bool
}

func (d idDriver) Whoami(context.Context) (scm.Identity, error) {
	if d.fail {
		return scm.Identity{}, os.ErrPermission
	}
	return scm.Identity{ID: "id-" + d.token, Login: d.token}, nil
}

func build(cfg *config.Config, token string) (scm.Driver, error) { return idDriver{token: token}, nil }

func cfgWith(t *testing.T, producer, reviewer, operator string) *config.Config {
	t.Helper()
	root := t.TempDir()
	write := func(name, tok string) config.CredentialRef {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return config.CredentialRef{File: p}
	}
	c := &config.Config{Credentials: config.Credentials{Producer: write("producer.token", producer), Reviewer: write("reviewer.token", reviewer)}}
	if operator != "" {
		c.Credentials.Operator = write("operator.token", operator)
	}
	return c
}

// Distinct paths are not distinct principals: the provider's stable id is.
// Two files holding the reviewer's token resolve to one identity and are
// refused; a token that cannot be read or does not authenticate is an
// error, never a role left out of the comparison (#47 review).
func TestThreeIdentitiesAreProvenDistinctByProviderID(t *testing.T) {
	ctx := context.Background()
	three, err := principal.Resolve(ctx, cfgWith(t, "p", "r", "o"), build)
	if err != nil || three.Operator == nil || three.Operator.ID != "id-o" || three.Reviewer.ID != "id-r" {
		t.Fatalf("three=%+v err=%v", three, err)
	}
	three, err = principal.Resolve(ctx, cfgWith(t, "p", "r", ""), build)
	if err != nil || three.Operator != nil {
		t.Fatalf("no operator configured: %+v %v", three, err)
	}
	if _, err := principal.Resolve(ctx, cfgWith(t, "p", "r", "r"), build); err == nil || !strings.Contains(err.Error(), "operator and reviewer both authenticate as") {
		t.Fatalf("identical reviewer/operator tokens under distinct paths passed: %v", err)
	}
	if _, err := principal.Resolve(ctx, cfgWith(t, "p", "r", "p"), build); err == nil || !strings.Contains(err.Error(), "operator and producer both authenticate as") {
		t.Fatalf("identical producer/operator tokens passed: %v", err)
	}
	if _, err := principal.Resolve(ctx, cfgWith(t, "same", "same", "o"), build); err == nil || !strings.Contains(err.Error(), "producer and reviewer both authenticate") {
		t.Fatalf("identical producer/reviewer tokens passed: %v", err)
	}
	c := cfgWith(t, "p", "r", "o")
	os.Remove(c.Credentials.Reviewer.File)
	if _, err := principal.Resolve(ctx, c, build); err == nil || !strings.Contains(err.Error(), "reviewer credential") {
		t.Fatalf("an unreadable reviewer token was skipped rather than refused: %v", err)
	}
	failing := func(cfg *config.Config, token string) (scm.Driver, error) {
		return idDriver{token: token, fail: token == "o"}, nil
	}
	if _, err := principal.Resolve(ctx, cfgWith(t, "p", "r", "o"), failing); err == nil || !strings.Contains(err.Error(), "operator identity") {
		t.Fatalf("an operator token the provider rejects passed: %v", err)
	}
	empty := func(cfg *config.Config, token string) (scm.Driver, error) { return emptyIDDriver{}, nil }
	if _, err := principal.Resolve(ctx, cfgWith(t, "p", "r", "o"), empty); err == nil || !strings.Contains(err.Error(), "no stable id") {
		t.Fatalf("an identity with no stable id passed: %v", err)
	}
}

type emptyIDDriver struct{ scm.Driver }

func (emptyIDDriver) Whoami(context.Context) (scm.Identity, error) {
	return scm.Identity{Login: "x"}, nil
}
