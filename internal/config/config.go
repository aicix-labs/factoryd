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
//
// Bumped to 2 when the supervisor arrived: a factory now has to say what command
// each role runs, and there is no defensible default for that. An older config
// is refused by version rather than started with a role that does nothing.
const SchemaVersion = 2

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
	// Roles are the two agent turns. Each is one command that reads its
	// trigger, does one unit of work, and exits. Neither polls or loops -- the
	// supervisor owns all continuity.
	Roles      Roles      `json:"roles"`
	Supervisor Supervisor `json:"supervisor"`
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

// Roles configures both agent turns.
type Roles struct {
	Producer RoleSpec `json:"producer"`
	Reviewer RoleSpec `json:"reviewer"`
}

// RoleSpec is one agent turn.
type RoleSpec struct {
	// Command is argv for a single turn. It must exit; a command told to "run
	// forever" is the v1 arrangement that reliably did not.
	Command []string `json:"command"`
	// TimeoutSeconds bounds one turn. Zero means the default.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// Workdir overrides where the turn runs. Defaults to the producer workdir
	// for the producer and to paths.root for the reviewer.
	Workdir string `json:"workdir,omitempty"`
}

// Supervisor tunes the spin guard and the watcher.
type Supervisor struct {
	// SpinWarn is the number of consecutive turns with no progress after which
	// the supervisor warns and backs off.
	SpinWarn int `json:"spin_warn,omitempty"`
	// SpinAbort is where it halts: writes the stop sentinel, records why, and
	// exits. Never relaunch indefinitely.
	SpinAbort int `json:"spin_abort,omitempty"`
	// PollIntervalSeconds is the watcher poll period, and the periodic
	// re-check interval even when inotify is in use.
	PollIntervalSeconds int `json:"poll_interval_seconds,omitempty"`
	// BackoffSeconds is the base delay applied after a turn that achieved
	// nothing. It scales with the spin count.
	BackoffSeconds int `json:"backoff_seconds,omitempty"`
	// ForcePoll disables inotify. Set it only to reproduce the fallback.
	ForcePoll bool `json:"force_poll,omitempty"`
}

// Defaults, applied by Load. They are named rather than inlined so doctor and
// the docs can quote the same numbers.
const (
	DefaultSpinWarn       = 3
	DefaultSpinAbort      = 8
	DefaultPollInterval   = 2
	DefaultBackoffSeconds = 15
	DefaultTurnTimeout    = 3600
	DefaultGateTimeout    = 900
)

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
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// applyDefaults fills in the optional knobs. It never defaults anything whose
// wrong value would be silently harmful -- role commands and credentials have no
// defaults at all.
func (c *Config) applyDefaults() {
	if c.Supervisor.SpinWarn == 0 {
		c.Supervisor.SpinWarn = DefaultSpinWarn
	}
	if c.Supervisor.SpinAbort == 0 {
		c.Supervisor.SpinAbort = DefaultSpinAbort
	}
	if c.Supervisor.PollIntervalSeconds == 0 {
		c.Supervisor.PollIntervalSeconds = DefaultPollInterval
	}
	if c.Supervisor.BackoffSeconds == 0 {
		c.Supervisor.BackoffSeconds = DefaultBackoffSeconds
	}
	if c.Gate.TimeoutSeconds == 0 {
		c.Gate.TimeoutSeconds = DefaultGateTimeout
	}
	if c.Roles.Producer.TimeoutSeconds == 0 {
		c.Roles.Producer.TimeoutSeconds = DefaultTurnTimeout
	}
	if c.Roles.Reviewer.TimeoutSeconds == 0 {
		c.Roles.Reviewer.TimeoutSeconds = DefaultTurnTimeout
	}
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

	for role, spec := range map[string]RoleSpec{
		"producer": c.Roles.Producer,
		"reviewer": c.Roles.Reviewer,
	} {
		if len(spec.Command) == 0 {
			add("roles.%s.command is empty; the supervisor would have no turn to run", role)
		}
		if spec.TimeoutSeconds < 0 {
			add("roles.%s.timeout_seconds is negative", role)
		}
	}

	sv := c.Supervisor
	switch {
	case sv.SpinWarn <= 0:
		add("supervisor.spin_warn must be positive")
	case sv.SpinAbort <= 0:
		add("supervisor.spin_abort must be positive")
	case sv.SpinWarn >= sv.SpinAbort:
		add("supervisor.spin_warn (%d) must be below spin_abort (%d), or the warning never precedes the halt",
			sv.SpinWarn, sv.SpinAbort)
	}
	if sv.PollIntervalSeconds <= 0 {
		add("supervisor.poll_interval_seconds must be positive")
	}
	if sv.BackoffSeconds < 0 {
		add("supervisor.backoff_seconds is negative")
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

// ProgressPath is where a role touches to say "I advanced". The producer's is
// named by SPEC.md §6.3; the reviewer gets the symmetric one.
func (c *Config) ProgressPath(role string) string {
	return filepath.Join(c.InboxDir(), role+"-progress")
}

// StopPath is the halt sentinel for a role. Its presence stops the supervisor
// from starting, and only an operator removes it: a circuit breaker that resets
// itself is not a circuit breaker.
func (c *Config) StopPath(role string) string {
	return filepath.Join(c.Paths.Root, role+".stop")
}

// RoleSpec returns the turn configuration for a role, and whether the role is
// one this build knows.
func (c *Config) RoleSpec(role string) (RoleSpec, bool) {
	switch role {
	case "producer":
		return c.Roles.Producer, true
	case "reviewer":
		return c.Roles.Reviewer, true
	default:
		return RoleSpec{}, false
	}
}

// TurnWorkdir is where a role'"'"'s turn runs.
func (c *Config) TurnWorkdir(role string) string {
	if spec, ok := c.RoleSpec(role); ok && spec.Workdir != "" {
		return spec.Workdir
	}
	if role == "producer" {
		return c.Paths.ProducerWorkdir
	}
	return c.Paths.Root
}
