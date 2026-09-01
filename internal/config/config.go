// Package config loads one factory's configuration.
//
// Configuration is JSON and is decoded strictly: an unknown key is an error,
// not a shrug. A typo in a key name would otherwise leave the default in place
// and produce a factory that runs happily with the wrong policy.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SchemaVersion is the config schema this build understands.
const SchemaVersion = 1

// Config is one factory.
type Config struct {
	SchemaVersion int    `json:"schema_version"`
	Name          string `json:"name"`
	// Provider selects the driver: "github" or "gitlab".
	Provider string  `json:"provider"`
	GitHub   *GitHub `json:"github,omitempty"`
	GitLab   *GitLab `json:"gitlab,omitempty"`
	// TargetBranch is the branch changes are merged into.
	TargetBranch string      `json:"target_branch"`
	Paths        Paths       `json:"paths"`
	Credentials  Credentials `json:"credentials"`
	Gate         Gate        `json:"gate"`
	// Alerts must not be empty. A factory that detects a stall and has nowhere
	// to report it recorded the stall correctly into a journal nobody read --
	// which is what v1 did for two hours and twenty minutes.
	Alerts []Alert `json:"alerts"`

	// path is where this config was loaded from; used in diagnostics.
	path string
}

// GitHub configures the GitHub driver.
type GitHub struct {
	BaseURL     string `json:"base_url,omitempty"`
	GraphQLURL  string `json:"graphql_url,omitempty"`
	Owner       string `json:"owner"`
	Repo        string `json:"repo"`
	MergeMethod string `json:"merge_method,omitempty"`
}

// GitLab configures the GitLab driver.
type GitLab struct {
	BaseURL            string `json:"base_url,omitempty"`
	Project            string `json:"project"`
	RemoveSourceBranch bool   `json:"remove_source_branch,omitempty"`
}

// Paths are the directories the factory owns.
type Paths struct {
	// Root holds state.json, inbox/ and outbox/.
	Root string `json:"root"`
	// ProducerWorkdir is the clone the producer edits. It must be a clone,
	// not a git worktree: a worktree keeps its refs in the parent repository,
	// outside the producer's sandbox, so the producer cannot write them.
	ProducerWorkdir string `json:"producer_workdir"`
}

// Credentials are the two role credentials. They must resolve to different
// provider identities; that separation is the property that catches defects.
type Credentials struct {
	Producer CredentialRef `json:"producer"`
	Reviewer CredentialRef `json:"reviewer"`
}

// CredentialRef points at a secret. Exactly one of File or Env must be set --
// never the token itself, so a config is safe to commit.
type CredentialRef struct {
	File string `json:"file,omitempty"`
	Env  string `json:"env,omitempty"`
}

// Resolve reads the credential. It fails closed: an unset, unreadable, or
// empty credential is an error, never an empty string that a caller might
// paper over with the other role's token.
func (c CredentialRef) Resolve() (string, error) {
	switch {
	case c.File != "" && c.Env != "":
		return "", fmt.Errorf("credential: both file (%s) and env (%s) are set; exactly one is required", c.File, c.Env)
	case c.File != "":
		b, err := os.ReadFile(c.File)
		if err != nil {
			return "", fmt.Errorf("credential file %s: %w", c.File, err)
		}
		tok := strings.TrimSpace(string(b))
		if tok == "" {
			return "", fmt.Errorf("credential file %s is empty", c.File)
		}
		return tok, nil
	case c.Env != "":
		tok := strings.TrimSpace(os.Getenv(c.Env))
		if tok == "" {
			return "", fmt.Errorf("credential env %s is unset or empty", c.Env)
		}
		return tok, nil
	default:
		return "", fmt.Errorf("credential: neither file nor env is set")
	}
}

// Describe names the credential's source without revealing it.
func (c CredentialRef) Describe() string {
	switch {
	case c.File != "" && c.Env != "":
		return fmt.Sprintf("file %s AND env %s (invalid)", c.File, c.Env)
	case c.File != "":
		return "file " + c.File
	case c.Env != "":
		return "env " + c.Env
	default:
		return "<unset>"
	}
}

// Gate is the build/vet/test command submit runs before pushing anything.
type Gate struct {
	// Command is argv, run without a shell unless argv[0] is a shell.
	Command []string `json:"command"`
	// TimeoutSeconds bounds the gate. Zero means the default.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// Alert is one alert transport.
type Alert struct {
	// Kind is one of command, webhook, file, syslog.
	Kind    string   `json:"kind"`
	Command []string `json:"command,omitempty"`
	URL     string   `json:"url,omitempty"`
	Path    string   `json:"path,omitempty"`
	Tag     string   `json:"tag,omitempty"`
}

// Path is where the config was loaded from.
func (c *Config) Path() string { return c.path }

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c.path = abs
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Validate checks everything decidable without touching the network or the
// filesystem. Anything requiring I/O belongs in doctor, so that Load stays
// usable in tests.
func (c *Config) Validate() error {
	var problems []string
	add := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }

	switch c.SchemaVersion {
	case SchemaVersion:
	case 0:
		add("schema_version is missing; refusing to guess which schema this is")
	default:
		add("schema_version is %d, this build understands %d", c.SchemaVersion, SchemaVersion)
	}

	if c.Name == "" {
		add("name is empty")
	}
	if c.TargetBranch == "" {
		add("target_branch is empty")
	}

	switch c.Provider {
	case "github":
		if c.GitLab != nil {
			add("provider is github but a gitlab block is present")
		}
		switch {
		case c.GitHub == nil:
			add("provider is github but there is no github block")
		default:
			if c.GitHub.Owner == "" {
				add("github.owner is empty")
			}
			if c.GitHub.Repo == "" {
				add("github.repo is empty")
			}
			switch c.GitHub.MergeMethod {
			case "", "merge", "squash", "rebase":
			default:
				add("github.merge_method %q is not one of merge, squash, rebase", c.GitHub.MergeMethod)
			}
		}
	case "gitlab":
		if c.GitHub != nil {
			add("provider is gitlab but a github block is present")
		}
		switch {
		case c.GitLab == nil:
			add("provider is gitlab but there is no gitlab block")
		case c.GitLab.Project == "":
			add("gitlab.project is empty")
		}
	case "":
		add("provider is empty; expected github or gitlab")
	default:
		add("provider %q is not github or gitlab", c.Provider)
	}

	if c.Paths.Root == "" {
		add("paths.root is empty")
	}
	if c.Paths.ProducerWorkdir == "" {
		add("paths.producer_workdir is empty")
	}

	for role, ref := range map[string]CredentialRef{
		"producer": c.Credentials.Producer,
		"reviewer": c.Credentials.Reviewer,
	} {
		if ref.File == "" && ref.Env == "" {
			add("credentials.%s is unset; the two roles must have distinct credentials", role)
		}
		if ref.File != "" && ref.Env != "" {
			add("credentials.%s sets both file and env; exactly one is required", role)
		}
	}
	if p, r := c.Credentials.Producer, c.Credentials.Reviewer; p == r && (p.File != "" || p.Env != "") {
		add("credentials.producer and credentials.reviewer point at the same secret (%s); the two-party split is the property that catches defects", p.Describe())
	}

	if len(c.Gate.Command) == 0 {
		add("gate.command is empty; submit would push unbuilt code")
	}
	if c.Gate.TimeoutSeconds < 0 {
		add("gate.timeout_seconds is negative")
	}

	if len(c.Alerts) == 0 {
		add("no alert transport is configured; a factory that cannot deliver an alert detects stalls into a journal nobody reads")
	}
	for i, a := range c.Alerts {
		switch a.Kind {
		case "command":
			if len(a.Command) == 0 {
				add("alerts[%d]: kind command with no command", i)
			}
		case "webhook":
			if a.URL == "" {
				add("alerts[%d]: kind webhook with no url", i)
			}
		case "file":
			if a.Path == "" {
				add("alerts[%d]: kind file with no path", i)
			}
		case "syslog":
		case "":
			add("alerts[%d]: kind is empty", i)
		default:
			add("alerts[%d]: kind %q is not one of command, webhook, file, syslog", i, a.Kind)
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid config:\n  - %s", strings.Join(problems, "\n  - "))
}

// StatePath is where this factory's state document lives.
func (c *Config) StatePath() string { return filepath.Join(c.Paths.Root, "state.json") }

// InboxDir and OutboxDir are the filesystem handoff channels (SPEC.md §6.2).
func (c *Config) InboxDir() string  { return filepath.Join(c.Paths.Root, "inbox") }
func (c *Config) OutboxDir() string { return filepath.Join(c.Paths.Root, "outbox") }
