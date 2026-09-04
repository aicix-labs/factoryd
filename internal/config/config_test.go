package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aicix-labs/factoryd/internal/config"
)

const good = `{
  "schema_version": 5,
  "name": "widgets",
  "provider": "github",
  "github": {"owner": "acme", "repo": "widgets"},
  "target_branch": "main",
  "git": {"remote": "https://github.com/acme/widgets.git", "transport": "https"},
  "paths": {
    "root": "/var/lib/factoryd/widgets",
    "producer_workdir": "/var/lib/factoryd/widgets/work",
    "submit_repo": "/var/lib/factoryd/widgets/submit",
    "cache_root": "/var/cache/factoryd/widgets"
  },
  "health": {"caches": [{"path": "/var/cache/factoryd/widgets/go", "max_bytes": 1}]},
  "credentials": {
    "producer": {"file": "/etc/factoryd/producer.token"},
    "reviewer": {"env": "FACTORYD_REVIEWER_TOKEN"}
  },
  "gate": {
    "command": ["go", "test", "./..."],
    "env": {"PATH": "/usr/local/go/bin:/usr/bin:/bin", "HOME": "/var/lib/factoryd/widgets", "GOCACHE": "/var/cache/factoryd/go"},
    "required_writable_paths": ["${GOCACHE}", "build/out"],
    "run_as": {"user": "factoryd-gate"}
  },
  "roles": {
    "producer": {"command": ["claude", "-p", "producer-brief"], "env": {"PATH": "/usr/local/bin:/usr/bin:/bin"}, "run_as": {"user": "factoryd-producer"}},
    "reviewer": {"command": ["claude", "-p", "reviewer-playbook"], "env": {"PATH": "/usr/local/bin:/usr/bin:/bin"}}
  },
  "alerts": [{"kind": "file", "path": "/var/log/factoryd/alerts.log"}],
  "scope": {
    "deny_regexes": ["^\\.github/workflows/", "(^|/)Dockerfile$"],
    "allow_regexes": ["^deploy/.*\\.md$"],
    "hold_diff_regexes": ["-----BEGIN [A-Z ]*PRIVATE KEY-----"],
    "escalate_regexes": ["(^|/)(auth|token|secret)"]
  }
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
		"missing schema version":              {`"schema_version": 5,`, ``, "schema_version"},
		"future schema version":               {`"schema_version": 5`, `"schema_version": 99`, "schema_version"},
		"previous schema version":             {`"schema_version": 5`, `"schema_version": 4`, "schema_version"},
		"no producer turn":                    {`"command": ["claude", "-p", "producer-brief"], `, `"command": [], `, "roles.producer.command"},
		"warn at or above abort":              {`"gate":`, `"supervisor": {"spin_warn": 9, "spin_abort": 4}, "gate":`, "spin_warn"},
		"no submit repo":                      {`"submit_repo": "/var/lib/factoryd/widgets/submit"`, `"submit_repo": ""`, "submit_repo"},
		"submit repo is the workdir":          {`"submit_repo": "/var/lib/factoryd/widgets/submit"`, `"submit_repo": "/var/lib/factoryd/widgets/work"`, "two-directory"},
		"ssh transport refused":               {`"transport": "https"`, `"transport": "ssh"`, "not implemented"},
		"no transport":                        {`"transport": "https"`, `"transport": ""`, "git.transport"},
		"remote names another project":        {`"remote": "https://github.com/acme/widgets.git"`, `"remote": "https://github.com/acme/other.git"`, "different repository"},
		"remote on a hostile host, same path": {`"remote": "https://github.com/acme/widgets.git"`, `"remote": "https://evil.example/acme/widgets.git"`, "not the provider"},
		"remote on a hostile port":            {`"remote": "https://github.com/acme/widgets.git"`, `"remote": "https://github.com:8443/acme/widgets.git"`, "not the provider"},
		"credentials in the remote URL":       {`"remote": "https://github.com/acme/widgets.git"`, `"remote": "https://token@github.com/acme/widgets.git"`, "never through a URL"},
		"no run_as":                           {`, "run_as": {"user": "factoryd-producer"}`, ``, "run_as"},
		"no PATH in gate env":                 {`"PATH": "/usr/local/go/bin:/usr/bin:/bin", `, ``, "PATH"},
		"path references unset var":           {`"${GOCACHE}"`, `"${TMPDIR}"`, "does not set"},
		"path with glob":                      {`"build/out"`, `"build/*"`, "no globbing"},
		"gate path is the repo root":          {`"build/out"`, `"."`, "ancestor"},
		"gate path is .git":                   {`"build/out"`, `".git"`, "never own"},
		"gate path is above the repo":         {`"build/out"`, `"/var/lib/factoryd"`, "ancestor"},
		"no gate run_as":                      {`"run_as": {"user": "factoryd-gate"}`, `"run_as": {"user": ""}`, "gate.run_as"},
		"gate shares the producer's user":     {`"run_as": {"user": "factoryd-gate"}`, `"run_as": {"user": "factoryd-producer"}`, "cannot share"},
		"negative fail abort":                 {`"gate":`, `"supervisor": {"fail_abort": -1}, "gate":`, "fail_abort"},
		"empty name":                          {`"name": "widgets"`, `"name": ""`, "name"},
		"unknown provider":                    {`"provider": "github"`, `"provider": "bitbucket"`, "provider"},
		"no target branch":                    {`"target_branch": "main"`, `"target_branch": ""`, "target_branch"},
		"empty gate":                          {`"command": ["go", "test", "./..."]`, `"command": []`, "gate.command"},
		"no alerts":                           {`[{"kind": "file", "path": "/var/log/factoryd/alerts.log"}]`, `[]`, "alert transport"},
		"unknown alert kind":                  {`"kind": "file"`, `"kind": "carrier-pigeon"`, "carrier-pigeon"},
		"scope absent": {`,
  "scope": {
    "deny_regexes": ["^\\.github/workflows/", "(^|/)Dockerfile$"],
    "allow_regexes": ["^deploy/.*\\.md$"],
    "hold_diff_regexes": ["-----BEGIN [A-Z ]*PRIVATE KEY-----"],
    "escalate_regexes": ["(^|/)(auth|token|secret)"]
  }`, ``, "scope is absent"},
		"scope regex does not compile":        {`"(^|/)Dockerfile$"`, `"(^|/Dockerfile$"`, "does not compile"},
		"scope empty pattern":                 {`"^deploy/.*\\.md$"`, `""`, "is empty"},
		"producer env declares FACTORYD_":     {`"env": {"PATH": "/usr/local/bin:/usr/bin:/bin"}, "run_as": {"user": "factoryd-producer"}`, `"env": {"PATH": "/usr/local/bin:/usr/bin:/bin", "FACTORYD_CONFIG": "/stale.json"}, "run_as": {"user": "factoryd-producer"}`, "reserved"},
		"reviewer env declares FACTORYD_":     {`"reviewer": {"command": ["claude", "-p", "reviewer-playbook"], "env": {"PATH": "/usr/local/bin:/usr/bin:/bin"}}`, `"reviewer": {"command": ["claude", "-p", "reviewer-playbook"], "env": {"PATH": "/usr/local/bin:/usr/bin:/bin", "FACTORYD_ROOT": "/elsewhere"}}`, "reserved"},
		"reviewer credential named FACTORYD_": {`"reviewer": {"env": "FACTORYD_REVIEWER_TOKEN"}`, `"reviewer": {"env": "FACTORYD_CONFIG"}`, "generates"},
		"producer credential named FACTORYD_": {`"producer": {"file": "/etc/factoryd/producer.token"}`, `"producer": {"env": "FACTORYD_ROOT"}`, "generates"},
		"gate env declares FACTORYD_":         {`"GOCACHE": "/var/cache/factoryd/go"}`, `"GOCACHE": "/var/cache/factoryd/go", "FACTORYD_TURN": "x"}`, "reserved"},
		"relative producer home":              {`"env": {"PATH": "/usr/local/bin:/usr/bin:/bin"}, "run_as": {"user": "factoryd-producer"}`, `"env": {"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": "private"}, "run_as": {"user": "factoryd-producer"}`, "relative"},
		"gate path is the producer home":      {`"env": {"PATH": "/usr/local/bin:/usr/bin:/bin"}, "run_as": {"user": "factoryd-producer"}`, `"env": {"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": "/var/cache/factoryd/go"}, "run_as": {"user": "factoryd-producer"}`, "overlaps the producer's home"},
		"gate path under the producer home":   {`"env": {"PATH": "/usr/local/bin:/usr/bin:/bin"}, "run_as": {"user": "factoryd-producer"}`, `"env": {"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": "/var/cache/factoryd"}, "run_as": {"user": "factoryd-producer"}`, "overlaps the producer's home"},
		"producer home under a gate path":     {`"env": {"PATH": "/usr/local/bin:/usr/bin:/bin"}, "run_as": {"user": "factoryd-producer"}`, `"env": {"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": "/var/cache/factoryd/go/home"}, "run_as": {"user": "factoryd-producer"}`, "overlaps the producer's home"},
		"alert missing path":                  {`{"kind": "file", "path": "/var/log/factoryd/alerts.log"}`, `{"kind": "file"}`, "no path"},
		"webhook is not deliverable here":     {`{"kind": "file", "path": "/var/log/factoryd/alerts.log"}`, `{"kind": "webhook"}`, "not implemented"},
		"syslog is not deliverable here":      {`{"kind": "file", "path": "/var/log/factoryd/alerts.log"}`, `{"kind": "syslog"}`, "not implemented"},
		"alert command without PATH":          {`{"kind": "file", "path": "/var/log/factoryd/alerts.log"}`, `{"kind": "command", "command": ["notify"]}`, "PATH"},
		"alert file relative path":            {`"path": "/var/log/factoryd/alerts.log"`, `"path": "alerts.log"`, "absolute"},
		"cache without a bound":               {`"health": {"caches": [{"path": "/var/cache/factoryd/widgets/go", "max_bytes": 1}]},`, `"health": {"caches": [{"path": "/var/cache/factoryd/widgets/go"}]},`, "max_bytes"},
		"cache relative path":                 {`"health": {"caches": [{"path": "/var/cache/factoryd/widgets/go", "max_bytes": 1}]},`, `"health": {"caches": [{"path": "go", "max_bytes": 1}]},`, "absolute"},
		"caches without a cache root": {`"submit_repo": "/var/lib/factoryd/widgets/submit",
    "cache_root": "/var/cache/factoryd/widgets"`, `"submit_repo": "/var/lib/factoryd/widgets/submit"`, "cache_root"},
		"cache root is /":                     {`"cache_root": "/var/cache/factoryd/widgets"`, `"cache_root": "/"`, "delete the host"},
		"cache root is the factory root":      {`"cache_root": "/var/cache/factoryd/widgets"`, `"cache_root": "/var/lib/factoryd/widgets"`, "overlaps paths.root"},
		"cache root above the factory root":   {`"cache_root": "/var/cache/factoryd/widgets"`, `"cache_root": "/var/lib"`, "overlaps paths.root"},
		"cache root inside the submit repo":   {`"cache_root": "/var/cache/factoryd/widgets"`, `"cache_root": "/var/lib/factoryd/widgets/submit/.cache"`, "overlaps paths.submit_repo"},
		"cache root holds a credential":       {`"cache_root": "/var/cache/factoryd/widgets"`, `"cache_root": "/etc/factoryd"`, "credentials.producer"},
		"cache root holds the alert file":     {`"cache_root": "/var/cache/factoryd/widgets"`, `"cache_root": "/var/log/factoryd"`, "alerts[0] file"},
		"cache outside the cache root":        {`{"path": "/var/cache/factoryd/widgets/go", "max_bytes": 1}`, `{"path": "/var/cache/other/go", "max_bytes": 1}`, "not inside paths.cache_root"},
		"cache in a sibling sharing a prefix": {`{"path": "/var/cache/factoryd/widgets/go", "max_bytes": 1}`, `{"path": "/var/cache/factoryd/widgets-old/go", "max_bytes": 1}`, "not inside paths.cache_root"},
		"disk headroom out of range":          {`"health": {"caches": [{"path": "/var/cache/factoryd/widgets/go", "max_bytes": 1}]},`, `"health": {"disk_min_free_percent": 100},`, "disk_min_free_percent"},
		"negative unreviewed":                 {`"health": {"caches": [{"path": "/var/cache/factoryd/widgets/go", "max_bytes": 1}]},`, `"health": {"unreviewed_seconds": -1},`, "unreviewed_seconds"},
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

// The git host follows the provider endpoint: github.com for the public API,
// the base_url's authority for GitHub Enterprise and for every GitLab.
func TestProviderGitHost(t *testing.T) {
	cases := []struct {
		provider, base, want string
	}{
		{"github", "", "github.com"},
		{"github", "https://api.github.com", "github.com"},
		{"github", "https://ghe.example/api/v3", "ghe.example"},
		{"github", "https://ghe.example:8443/api/v3", "ghe.example:8443"},
		{"gitlab", "", "gitlab.com"},
		{"gitlab", "https://gitlab.dev.aicix.com/api/v4", "gitlab.dev.aicix.com"},
	}
	for _, c := range cases {
		cfg := &config.Config{Provider: c.provider}
		if c.provider == "github" {
			cfg.GitHub = &config.GitHub{BaseURL: c.base}
		} else {
			cfg.GitLab = &config.GitLab{BaseURL: c.base}
		}
		got, err := cfg.ProviderGitHost()
		if err != nil || got != c.want {
			t.Errorf("%s base=%q: host=%q err=%v, want %q", c.provider, c.base, got, err, c.want)
		}
	}
	// A GHE remote on the GHE host passes; the same path on github.com does not.
	ghe := strings.Replace(good, `"github": {"owner": "acme", "repo": "widgets"}`, `"github": {"owner": "acme", "repo": "widgets", "base_url": "https://ghe.example/api/v3"}`, 1)
	ghe = strings.Replace(ghe, `"remote": "https://github.com/acme/widgets.git"`, `"remote": "https://ghe.example/acme/widgets.git"`, 1)
	if ghe == good {
		t.Fatal("test setup did not modify the config")
	}
	if _, err := config.Load(write(t, ghe)); err != nil {
		t.Fatalf("a GHE remote on the GHE host was refused: %v", err)
	}
	wrong := strings.Replace(ghe, `"remote": "https://ghe.example/acme/widgets.git"`, `"remote": "https://github.com/acme/widgets.git"`, 1)
	if _, err := config.Load(write(t, wrong)); err == nil {
		t.Fatal("a github.com remote was accepted for a GHE provider")
	}
}

func TestGatePathResolution(t *testing.T) {
	c, err := config.Load(write(t, good))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.ResolveGatePath("${GOCACHE}")
	if err != nil || got != "/var/cache/factoryd/go" {
		t.Fatalf("ResolveGatePath(${GOCACHE}) = %q, %v", got, err)
	}
	// Relative paths resolve against the gate workdir -- the submit repo.
	got, err = c.ResolveGatePath("build/out")
	if err != nil || got != "/var/lib/factoryd/widgets/submit/build/out" {
		t.Fatalf("ResolveGatePath(build/out) = %q, %v", got, err)
	}
	// An unset variable is an error, never an empty expansion.
	if got, err := c.ResolveGatePath("${NOPE}/x"); err == nil {
		t.Fatalf("an unset variable expanded to %q instead of failing", got)
	}
}

// The gate environment is built from configuration alone. Whatever the process
// was started with must not reach it.
func TestGateEnvIsConstructedNotInherited(t *testing.T) {
	c, err := config.Load(write(t, good))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("https_proxy", "http://interceptor.example:3128")
	t.Setenv("GOFLAGS", "-ambient")
	env := c.GateEnv(map[string]string{"FACTORYD_ROLE": "producer"})
	joined := strings.Join(env, "\n")
	for _, leaked := range []string{"https_proxy", "GOFLAGS", "interceptor"} {
		if strings.Contains(joined, leaked) {
			t.Errorf("the ambient %s reached the gate environment:\n%s", leaked, joined)
		}
	}
	for _, want := range []string{"PATH=/usr/local/go/bin:/usr/bin:/bin", "GOCACHE=/var/cache/factoryd/go", "FACTORYD_ROLE=producer"} {
		if !strings.Contains(joined, want) {
			t.Errorf("declared %s is missing from the gate environment:\n%s", want, joined)
		}
	}
	// And it is deterministic, since two commands must agree on it.
	if again := strings.Join(c.GateEnv(map[string]string{"FACTORYD_ROLE": "producer"}), "\n"); again != joined {
		t.Fatal("GateEnv is not deterministic")
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

// A sibling that merely shares a string prefix with a protected path is not
// inside it: /var/lib/factoryd/widgets-cache does not overlap
// /var/lib/factoryd/widgets. Without this control the containment tests
// above would also pass for a prefix comparison.
func TestSharedPrefixSiblingIsNotAnOverlap(t *testing.T) {
	body := strings.Replace(good, `"cache_root": "/var/cache/factoryd/widgets"`, `"cache_root": "/var/lib/factoryd/widgets-cache"`, 1)
	body = strings.Replace(body, `{"path": "/var/cache/factoryd/widgets/go", "max_bytes": 1}`, `{"path": "/var/lib/factoryd/widgets-cache/go", "max_bytes": 1}`, 1)
	if _, err := config.Load(write(t, body)); err != nil {
		t.Fatalf("a sibling with a shared prefix was refused: %v", err)
	}
	if config.PathWithin("/a/bc", "/a/b") || !config.PathWithin("/a/b/c", "/a/b") || !config.PathWithin("/a/b", "/a/b") || !config.PathWithin("/x", "/") {
		t.Fatal("PathWithin")
	}
}

// Generated FACTORYD_* values win over a role's declared env even when a
// config reached the runner without Validate: the order of the two loops in
// TurnEnv is the rule, not an accident. A reviewer told a stale config path
// by its own env is the case that was found.
func TestGeneratedTurnEnvWinsOverRoleEnv(t *testing.T) {
	cfg := &config.Config{Roles: config.Roles{Reviewer: config.RoleSpec{Env: map[string]string{"PATH": "/usr/bin", "FACTORYD_CONFIG": "/stale/factory.json", "FACTORYD_ROOT": "/stale"}}}}
	env := cfg.TurnEnv("reviewer", map[string]string{"FACTORYD_CONFIG": "/etc/factoryd/real.json", "FACTORYD_ROOT": "/var/lib/factoryd/real"}, nil)
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if got["FACTORYD_CONFIG"] != "/etc/factoryd/real.json" || got["FACTORYD_ROOT"] != "/var/lib/factoryd/real" || got["PATH"] != "/usr/bin" {
		t.Fatalf("env=%v; the generated values must win and the rest must survive", env)
	}
}

// The reviewer's credential is injected by the name the config chose, from
// the supervisor's environment. A name that is a generated key must not put
// the token where the config path belongs, whatever the order of injection:
// the generated value wins, and the token appears nowhere in the turn's env.
func TestReviewerCredentialCannotReplaceAGeneratedValue(t *testing.T) {
	const token = "TOKEN-SENTINEL-7f3a9c"
	cfg := &config.Config{
		Roles:       config.Roles{Reviewer: config.RoleSpec{Env: map[string]string{"PATH": "/usr/bin"}}},
		Credentials: config.Credentials{Reviewer: config.CredentialRef{Env: "FACTORYD_CONFIG"}},
	}
	env := cfg.TurnEnv("reviewer", map[string]string{"FACTORYD_CONFIG": "/etc/factoryd/real.json"}, []string{"FACTORYD_CONFIG=" + token, "OTHER=x"})
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "FACTORYD_CONFIG=/etc/factoryd/real.json") {
		t.Fatalf("the config path was replaced:\n%s", joined)
	}
	if strings.Contains(joined, token) {
		t.Fatalf("the token reached the turn's environment under a generated name:\n%s", joined)
	}
	// Control: an ordinary credential name is injected.
	cfg.Credentials.Reviewer.Env = "REVIEWER_TOKEN"
	env = cfg.TurnEnv("reviewer", map[string]string{"FACTORYD_CONFIG": "/etc/factoryd/real.json"}, []string{"REVIEWER_TOKEN=" + token})
	if !strings.Contains(strings.Join(env, "\n"), "REVIEWER_TOKEN="+token) {
		t.Fatalf("an ordinary credential name was not injected:\n%s", strings.Join(env, "\n"))
	}
}

// The operator principal is a boundary only as a third secret: a file the
// role identities cannot read, distinct from both role tokens, never an
// environment variable (#47 review).
func TestOperatorCredentialMustBeAThirdFile(t *testing.T) {
	base := func(t *testing.T) *config.Config {
		t.Helper()
		c, err := config.Load("../../examples/factory.github.json")
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := base(t)
	c.Credentials.Operator = c.Credentials.Reviewer
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "credentials.operator and credentials.reviewer point at the same secret") {
		t.Fatalf("operator == reviewer accepted: %v", err)
	}
	c = base(t)
	c.Credentials.Operator = c.Credentials.Producer
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "credentials.operator and credentials.producer point at the same secret") {
		t.Fatalf("operator == producer accepted: %v", err)
	}
	c = base(t)
	c.Credentials.Operator = config.CredentialRef{Env: "OPERATOR_TOKEN"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "credentials.operator.env is not accepted") {
		t.Fatalf("operator from env accepted: %v", err)
	}
	c = base(t)
	c.Credentials.Operator = config.CredentialRef{File: "/etc/factoryd/widgets/operator.token"}
	if err := c.Validate(); err != nil {
		t.Fatalf("a distinct operator file refused: %v", err)
	}
}

// The operator credential's directory is protected from cache reclamation
// like the two role credentials' are: reclamation deletes, and a token
// under the cache root is a token that disappears (#47 review).
func TestCacheRootMayNotContainTheOperatorCredential(t *testing.T) {
	c, err := config.Load("../../examples/factory.github.json")
	if err != nil {
		t.Fatal(err)
	}
	c.Paths.CacheRoot = "/var/cache/factoryd"
	c.Health.Caches = []config.Cache{{Path: "/var/cache/factoryd/go", MaxBytes: 1}}
	c.Credentials.Operator = config.CredentialRef{File: "/var/cache/factoryd/secrets/operator.token"}
	err = c.Validate()
	if err == nil || !strings.Contains(err.Error(), "credentials.operator file") {
		t.Fatalf("an operator token under the cache root was accepted: %v", err)
	}
	c.Credentials.Operator = config.CredentialRef{File: "/etc/factoryd/widgets/operator.token"}
	if err := c.Validate(); err != nil {
		t.Fatalf("a token outside the cache root refused: %v", err)
	}
}
