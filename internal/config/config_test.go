package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aicix-labs/factoryd/internal/config"
)

const good = `{
  "schema_version": 2,
  "name": "widgets",
  "provider": "github",
  "github": {"owner": "acme", "repo": "widgets"},
  "target_branch": "main",
  "paths": {"root": "/var/lib/factoryd/widgets", "producer_workdir": "/var/lib/factoryd/widgets/work"},
  "credentials": {
    "producer": {"file": "/etc/factoryd/producer.token"},
    "reviewer": {"env": "FACTORYD_REVIEWER_TOKEN"}
  },
  "gate": {"command": ["go", "test", "./..."]},
  "roles": {
    "producer": {"command": ["claude", "-p", "producer-brief"]},
    "reviewer": {"command": ["claude", "-p", "reviewer-playbook"]}
  },
  "alerts": [{"kind": "file", "path": "/var/log/factoryd/alerts.log"}]
}`

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "factory.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadGoodConfig(t *testing.T) {
	c, err := config.Load(write(t, good))
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "widgets" || c.Provider != "github" || c.GitHub.Owner != "acme" {
		t.Fatalf("loaded %+v", c)
	}
	if got := c.StatePath(); got != "/var/lib/factoryd/widgets/state.json" {
		t.Fatalf("StatePath = %q", got)
	}
}

// A misspelled key must be an error. Otherwise the default stays in place and
// the factory runs with a policy the operator did not write.
func TestUnknownKeyIsRefused(t *testing.T) {
	body := strings.Replace(good, `"target_branch": "main"`, `"target_brunch": "main"`, 1)
	_, err := config.Load(write(t, body))
	if err == nil {
		t.Fatal("a misspelled key was accepted")
	}
	if !strings.Contains(err.Error(), "target_brunch") {
		t.Fatalf("error does not name the offending key: %v", err)
	}
}

func TestValidationFailures(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"missing schema version":  {`"schema_version": 2,`, ``, "schema_version"},
		"future schema version":   {`"schema_version": 2`, `"schema_version": 99`, "schema_version"},
		"previous schema version": {`"schema_version": 2`, `"schema_version": 1`, "schema_version"},
		"no producer turn":        {`"producer": {"command": ["claude", "-p", "producer-brief"]}`, `"producer": {"command": []}`, "roles.producer.command"},
		"warn at or above abort":  {`"gate":`, `"supervisor": {"spin_warn": 9, "spin_abort": 4}, "gate":`, "spin_warn"},
		"negative fail abort":     {`"gate":`, `"supervisor": {"fail_abort": -1}, "gate":`, "fail_abort"},
		"empty name":              {`"name": "widgets"`, `"name": ""`, "name"},
		"unknown provider":        {`"provider": "github"`, `"provider": "bitbucket"`, "provider"},
		"no target branch":        {`"target_branch": "main"`, `"target_branch": ""`, "target_branch"},
		"empty gate":              {`"command": ["go", "test", "./..."]`, `"command": []`, "gate.command"},
		"no alerts":               {`[{"kind": "file", "path": "/var/log/factoryd/alerts.log"}]`, `[]`, "alert transport"},
		"unknown alert kind":      {`"kind": "file"`, `"kind": "carrier-pigeon"`, "carrier-pigeon"},
		"alert missing path":      {`{"kind": "file", "path": "/var/log/factoryd/alerts.log"}`, `{"kind": "file"}`, "no path"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(good, c.from, c.to, 1)
			if body == good {
				t.Fatalf("test setup did not modify the config (looking for %q)", c.from)
			}
			_, err := config.Load(write(t, body))
			if err == nil {
				t.Fatal("config was accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error does not mention %q: %v", c.want, err)
			}
		})
	}
}

// The provider block and the provider name must agree. A github block under a
// gitlab provider is a config someone edited halfway.
func TestProviderBlockMustMatchProvider(t *testing.T) {
	body := strings.Replace(good, `"provider": "github"`, `"provider": "gitlab"`, 1)
	_, err := config.Load(write(t, body))
	if err == nil {
		t.Fatal("a gitlab provider with only a github block was accepted")
	}
}

// The two-party split is the property that catches defects; one secret for both
// roles quietly removes it.
func TestSharedCredentialIsRefused(t *testing.T) {
	body := strings.Replace(good,
		`"reviewer": {"env": "FACTORYD_REVIEWER_TOKEN"}`,
		`"reviewer": {"file": "/etc/factoryd/producer.token"}`, 1)
	_, err := config.Load(write(t, body))
	if err == nil {
		t.Fatal("one credential shared by both roles was accepted")
	}
	if !strings.Contains(err.Error(), "same secret") {
		t.Fatalf("error does not explain the problem: %v", err)
	}
}

func TestCredentialResolveFailsClosed(t *testing.T) {
	dir := t.TempDir()

	t.Run("unset", func(t *testing.T) {
		if _, err := (config.CredentialRef{}).Resolve(); err == nil {
			t.Fatal("an unset credential resolved")
		}
	})
	t.Run("missing file", func(t *testing.T) {
		ref := config.CredentialRef{File: filepath.Join(dir, "nope")}
		if _, err := ref.Resolve(); err == nil {
			t.Fatal("a missing credential file resolved")
		}
	})
	t.Run("empty file", func(t *testing.T) {
		p := filepath.Join(dir, "empty")
		if err := os.WriteFile(p, []byte("   \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := (config.CredentialRef{File: p}).Resolve(); err == nil {
			t.Fatal("an empty credential file resolved to an empty token")
		}
	})
	t.Run("unset env", func(t *testing.T) {
		if _, err := (config.CredentialRef{Env: "FACTORYD_TEST_UNSET"}).Resolve(); err == nil {
			t.Fatal("an unset env credential resolved")
		}
	})
	t.Run("both file and env", func(t *testing.T) {
		ref := config.CredentialRef{File: filepath.Join(dir, "x"), Env: "Y"}
		if _, err := ref.Resolve(); err == nil {
			t.Fatal("a credential naming two sources resolved")
		}
	})
	t.Run("good file", func(t *testing.T) {
		p := filepath.Join(dir, "tok")
		if err := os.WriteFile(p, []byte("  secret \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := (config.CredentialRef{File: p}).Resolve()
		if err != nil {
			t.Fatal(err)
		}
		if got != "secret" {
			t.Fatalf("resolved %q, want %q", got, "secret")
		}
	})
	t.Run("good env", func(t *testing.T) {
		t.Setenv("FACTORYD_TEST_SET", "secret")
		got, err := (config.CredentialRef{Env: "FACTORYD_TEST_SET"}).Resolve()
		if err != nil {
			t.Fatal(err)
		}
		if got != "secret" {
			t.Fatalf("resolved %q", got)
		}
	})
}

// The handoff directory and the factory root are shared between the two roles.
// The role suffix is the only thing separating their per-role files, so it is
// worth asserting directly: a shared progress marker lets one role reset the
// other's spin counter, and a shared halt sentinel lets halting one role stop
// the other.
func TestPerRolePathsAreDistinct(t *testing.T) {
	c, err := config.Load(write(t, good))
	if err != nil {
		t.Fatal(err)
	}
	if a, b := c.ProgressPath("producer"), c.ProgressPath("reviewer"); a == b {
		t.Errorf("both roles share the progress marker %s", a)
	}
	if a, b := c.StopPath("producer"), c.StopPath("reviewer"); a == b {
		t.Errorf("both roles share the halt sentinel %s", a)
	}
	// And they must still live where the spec puts them.
	if got := c.ProgressPath("producer"); filepath.Base(got) != "producer-progress" {
		t.Errorf("producer progress marker is %s, want inbox/producer-progress", got)
	}
}

// Describe must never reveal the secret itself.
func TestDescribeDoesNotLeak(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(p, []byte("super-secret-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := (config.CredentialRef{File: p}).Describe(); strings.Contains(got, "super-secret-token") {
		t.Fatalf("Describe leaked the token: %q", got)
	}
}
