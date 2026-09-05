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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SchemaVersion is the config schema this build understands.
//
// Bumped to 2 when the supervisor arrived: a factory now has to say what command
// each role runs, and there is no defensible default for that. An older config
// is refused by version rather than started with a role that does nothing.
//
// Bumped to 3 for submit (SPEC.md §4.4, §5.4): the git transport, the
// factoryd-owned submit repository, the fully declared gate environment, and
// the producer's separate identity are all required and none has a default
// that would be safe to assume.
//
// Bumped to 4 for health (SPEC.md §7): the alert transports a build cannot
// deliver through are refused at load rather than accepted and never used,
// and the health block names the volumes and caches the factory is bounded
// by. A v3 config that listed a webhook it could not send would have passed
// validation and alerted nobody.
//
// Bumped to 5 for the merge gate (SPEC.md §2, §6.4): scope policy is data in
// the config, not convention in a playbook. A v4 config has no scope block,
// and a merge gate with no policy would merge anything; it is refused by
// version rather than run permissive.
const SchemaVersion = 5

// Config is one factory.
type Config struct {
	SchemaVersion int    `json:"schema_version"`
	Name          string `json:"name"`
	// Provider selects the driver: "github" or "gitlab".
	Provider string  `json:"provider"`
	GitHub   *GitHub `json:"github,omitempty"`
	GitLab   *GitLab `json:"gitlab,omitempty"`
	// TargetBranch is the branch changes are merged into.
	TargetBranch string `json:"target_branch"`
	// Git is the transport submit pushes through (§5.4). Declared, not
	// discovered: a transport that inferred its remote from a workdir would
	// verify one thing and push to another.
	Git         Git         `json:"git"`
	Paths       Paths       `json:"paths"`
	Credentials Credentials `json:"credentials"`
	Gate        Gate        `json:"gate"`
	// Roles are the two agent turns. Each is one command that reads its
	// trigger, does one unit of work, and exits. Neither polls or loops -- the
	// supervisor owns all continuity.
	Roles      Roles      `json:"roles"`
	Supervisor Supervisor `json:"supervisor"`
	// Alerts must not be empty. A factory that detects a stall and has nowhere
	// to report it recorded the stall correctly into a journal nobody read --
	// which is what v1 did for two hours and twenty minutes.
	Alerts []Alert `json:"alerts"`
	// Health configures the periodic, model-free tick (§4.5, §7).
	Health Health `json:"health"`
	// Scope is the path policy the merge gate enforces (§2 "scope policy as
	// data", §6.4). Required: a gate with no policy is not a gate.
	Scope Scope `json:"scope"`

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
	// ProducerWorkdir is where the producer edits source. Its .git, if any, is
	// ignored entirely: git never runs in a directory the producer can write.
	ProducerWorkdir string `json:"producer_workdir"`
	// SubmitRepo is the factoryd-owned clone every git command runs in. The
	// producer must not be able to write it; doctor proves that by probe, as
	// the producer, rather than assuming it (§4.4).
	SubmitRepo string `json:"submit_repo"`
	// CacheRoot is the one directory the factory may reclaim space in
	// (§7). Every health.caches[].path must lie inside it, and it may not
	// overlap the factory root, either repository, a credential, or an alert
	// file. Reclamation deletes; a bound that could name anything else is a
	// configuration that can destroy the host. Required when caches are
	// declared.
	CacheRoot string `json:"cache_root,omitempty"`
}

// Git configures the transport (SPEC.md §5.4).
type Git struct {
	// Remote is the URL git will be given explicitly for every fetch and
	// push. It must address the same project the provider block names.
	Remote string `json:"remote"`
	// Transport is https or ssh. Only https is implemented in this build;
	// ssh is refused at load rather than silently falling back.
	Transport string `json:"transport"`
	// SSHKeyFile and KnownHostsFile are required for ssh and ignored otherwise.
	SSHKeyFile     string `json:"ssh_key_file,omitempty"`
	KnownHostsFile string `json:"known_hosts_file,omitempty"`
	// Proxy and CAFile are the ONLY way a proxy or an alternative trust store
	// reaches the transport. Empty means a direct connection and the system
	// store; nothing ambient can widen either.
	Proxy  string `json:"proxy,omitempty"`
	CAFile string `json:"ca_file,omitempty"`
}

// Credentials are the two role credentials. They must resolve to different
// provider identities; that separation is the property that catches defects.
type Credentials struct {
	Producer CredentialRef `json:"producer"`
	Reviewer CredentialRef `json:"reviewer"`
	// Operator is a third principal, optional: the human's own token, used
	// only by `scm close` to retire a superseded draft. It is the boundary
	// that keeps closing out of the automated reviewer protocol -- a flag
	// is a string any turn can pass, a credential the reviewer identity
	// cannot read is not (#47 review). Doctor proves the reviewer and the
	// producer cannot read it. Unset means close is unavailable.
	Operator CredentialRef `json:"operator,omitempty"`
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

// Spec returns the role's spec; an unknown role is the zero spec.
func (r Roles) Spec(role string) RoleSpec {
	switch role {
	case "producer":
		return r.Producer
	case "reviewer":
		return r.Reviewer
	}
	return RoleSpec{}
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
	// Sandbox is what the supervisor itself takes away from the turn. It is
	// enforced by the supervisor, as root, at the moment the turn is
	// started -- not trusted from whatever sandbox the agent's own tooling
	// may or may not apply. §4.4: a safety property inferred from how one
	// sandbox happened to behave is an assumption, not a boundary.
	Sandbox *Sandbox `json:"sandbox,omitempty"`
	// Env is the turn's WHOLE environment beyond the FACTORYD_* variables.
	// Nothing is inherited. Passing the supervisor's environment through
	// would hand the producer every variable the supervisor holds -- including
	// a reviewer credential referenced by credentials.reviewer.env -- and the
	// two-party model would be one getenv away from gone. PATH is required.
	Env map[string]string `json:"env"`
	// RunAs is the OS identity the turn runs under. Required for the
	// producer: it is the principal the submit repository must be unwritable
	// by, and doctor's write probe runs as it. factoryd needs the privilege to
	// switch to it (root or CAP_SETUID); doctor fails if it does not have it,
	// because a separation that cannot be enforced must not be recorded as
	// enforced.
	RunAs *RunAs `json:"run_as,omitempty"`
}

// RunAs names an OS user by name, resolved at doctor time so a deleted user
// fails loudly rather than as a numeric id nobody recognises.
type RunAs struct {
	User string `json:"user"`
}

// Supervisor tunes the spin guard and the watcher.
type Supervisor struct {
	// SpinWarn is the number of consecutive turns with no progress after which
	// the supervisor warns and backs off.
	SpinWarn int `json:"spin_warn,omitempty"`
	// SpinAbort is where it halts: writes the stop sentinel, records why, and
	// exits. Never relaunch indefinitely.
	SpinAbort int `json:"spin_abort,omitempty"`
	// FailAbort is the number of consecutive turns that exited non-zero (or
	// timed out) without reporting progress after which the supervisor halts.
	// It is counted independently of trigger consumption: a turn that consumed
	// its trigger and then failed leaves nothing to re-arm on, so the spin
	// guard never sees it. Observed in production as a factory idle for 3.5h
	// with completed work stranded and every signal green (issue #12).
	FailAbort int `json:"fail_abort,omitempty"`
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
	DefaultFailAbort      = 5
	DefaultPollInterval   = 2
	DefaultBackoffSeconds = 15
	DefaultTurnTimeout    = 3600
	DefaultGateTimeout    = 900

	DefaultHealthInterval  = 60
	DefaultAlertAfter      = 3
	DefaultRepeatSeconds   = 1800
	DefaultStaleTrigger    = 900
	DefaultTurnGrace       = 120
	DefaultUnreviewed      = 0
	DefaultDiskMinFree     = 10
	DefaultAlertCmdTimeout = 30
)

// Sandbox is the set of things a turn is denied.
type Sandbox struct {
	// NoNetwork starts the turn in a new, empty network namespace: no
	// interfaces but a downed loopback, so nothing can be reached -- not
	// the provider, not a proxy, not another process's socket. Requires
	// factoryd to run as root; a factoryd that cannot create the namespace
	// refuses to start the turn rather than starting it connected.
	NoNetwork bool `json:"no_network,omitempty"`
}

// Gate is the build/vet/test command submit runs before pushing anything.
type Gate struct {
	// Command is argv, run without a shell unless argv[0] is a shell.
	Command []string `json:"command"`
	// Env is the gate's WHOLE environment beyond the FACTORYD_* variables.
	// Nothing is inherited: doctor from a terminal and submit as a service
	// would otherwise resolve the same ${VAR} to different paths. PATH is
	// required, because a gate with no PATH runs nothing and inheriting one
	// reintroduces the whole problem.
	Env map[string]string `json:"env"`
	// RequiredWritablePaths are directories the gate writes to, declared
	// because they cannot be discovered from an opaque argv. Relative paths
	// resolve against the gate workdir; ${VAR} expands from Env, and an unset
	// variable is an error, not an empty expansion. Submit creates any that
	// are absent.
	RequiredWritablePaths []string `json:"required_writable_paths"`
	// RunAs is the OS identity the gate runs under. Required, and distinct
	// from the producer's: the gate executes producer-authored build and test
	// code inside the factoryd-owned repository, and factoryd itself holds
	// root to switch identities. Run as factoryd, that code reads every
	// credential on the host; run as the producer, it cannot write the build
	// outputs the gate needs. So the gate is a third principal that may write
	// the declared paths, may not write .git, and may not read a credential.
	RunAs *RunAs `json:"run_as"`
	// TimeoutSeconds bounds the gate. Zero means the default.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// Alert is one alert transport.
type Alert struct {
	// Kind is one of file, command. (webhook and syslog are named by the
	// spec and not implemented in this build; a config that lists one is
	// refused at load, because a transport that is accepted and never
	// delivers is the failure the alert subsystem exists to prevent.)
	Kind string `json:"kind"`
	// Command is argv for kind command. It receives the alert as JSON on
	// stdin and must exit 0; anything else is a delivery failure.
	Command []string `json:"command,omitempty"`
	// Env is the command's WHOLE environment. PATH is required.
	Env map[string]string `json:"env,omitempty"`
	// TimeoutSeconds bounds the command. Zero means the default.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// Path is the file kind's destination: one JSON line appended per alert.
	Path string `json:"path,omitempty"`
}

// AlertKinds this build can deliver through.
var AlertKinds = []string{"file", "command"}

// Health configures the health tick (SPEC.md §7). Every threshold has a
// default, because the tick must run on a config that predates the knob; none
// may be zero after Load, because a zero threshold is a check that cannot
// fail or one that always does.
type Health struct {
	// IntervalSeconds is the tick period when health runs as a loop.
	IntervalSeconds int `json:"interval_seconds,omitempty"`
	// AlertAfter is the number of consecutive ticks a condition must hold
	// before the first alert. One transient tick is not an incident.
	AlertAfter int `json:"alert_after,omitempty"`
	// RepeatSeconds is how often a standing condition is alerted again. It
	// nags; it does not flood.
	RepeatSeconds int `json:"repeat_seconds,omitempty"`
	// StaleTriggerSeconds is how long a trigger may sit unconsumed.
	StaleTriggerSeconds int `json:"stale_trigger_seconds,omitempty"`
	// TurnGraceSeconds is how far past its timeout a turn may still be
	// running before the supervisor that should have killed it is presumed
	// stuck.
	TurnGraceSeconds int `json:"turn_grace_seconds,omitempty"`
	// UnreviewedSeconds is how long a change may stay open, untouched, before
	// it is a condition. Zero disables the check (it needs the network).
	UnreviewedSeconds int `json:"unreviewed_seconds,omitempty"`
	// DiskMinFreePercent is the headroom every volume the factory writes to
	// must keep. The predecessor filled a 232 GB volume to 96% with no signal.
	DiskMinFreePercent int `json:"disk_min_free_percent,omitempty"`
	// Caches are the directories the factory bounds. Each is reclaimed,
	// oldest entry first, when it exceeds its bound, and what was reclaimed
	// is reported. Growth without a bound is a defect.
	Caches []Cache `json:"caches,omitempty"`
}

// DefaultHealth is the health block with every default applied and nothing
// declared: no caches, no unreviewed check.
func DefaultHealth() Health { return Health{}.withDefaults() }

func (h Health) withDefaults() Health {
	if h.IntervalSeconds == 0 {
		h.IntervalSeconds = DefaultHealthInterval
	}
	if h.AlertAfter == 0 {
		h.AlertAfter = DefaultAlertAfter
	}
	if h.RepeatSeconds == 0 {
		h.RepeatSeconds = DefaultRepeatSeconds
	}
	if h.StaleTriggerSeconds == 0 {
		h.StaleTriggerSeconds = DefaultStaleTrigger
	}
	if h.TurnGraceSeconds == 0 {
		h.TurnGraceSeconds = DefaultTurnGrace
	}
	if h.DiskMinFreePercent == 0 {
		h.DiskMinFreePercent = DefaultDiskMinFree
	}
	return h
}

// Scope is the merge gate's path policy, preserved from v1 as data. Go RE2
// syntax; every pattern is compiled at load, and one that does not compile
// refuses the whole config -- a policy with a silently dropped rule is a
// policy with a hole where the operator believes there is a rule.
type Scope struct {
	// DenyRegexes: a changed path matching any of these makes the change
	// operator-gated. CI config, container builds, deploy manifests.
	DenyRegexes []string `json:"deny_regexes"`
	// AllowRegexes: a narrow exemption under a broad deny (docs under a
	// denied deploy tree). A path must match a deny AND no allow to be denied.
	AllowRegexes []string `json:"allow_regexes,omitempty"`
	// HoldDiffRegexes: an ADDED diff line matching any of these makes the
	// change operator-gated, whatever path it is in. Secret-shaped content.
	HoldDiffRegexes []string `json:"hold_diff_regexes,omitempty"`
	// EscalateRegexes: a changed path matching any of these requires a
	// recorded adversarial audit, with attempts, on the exact head, before
	// the gate will merge. Auth, crypto, permissions.
	EscalateRegexes []string `json:"escalate_regexes"`

	deny, allow, hold, escalate []*regexp.Regexp
}

// EmptyScope is a declared policy with no rules: the merge gate merges
// anything that passes CI. It is explicit so that a config cannot arrive at
// it by omission.
func EmptyScope() Scope { return Scope{DenyRegexes: []string{}, EscalateRegexes: []string{}} }

// Compiled returns the compiled patterns, in config order.
func (s *Scope) Compiled() (deny, allow, hold, escalate []*regexp.Regexp) {
	return s.deny, s.allow, s.hold, s.escalate
}

func (s *Scope) compile() []string {
	var problems []string
	do := func(name string, pats []string) []*regexp.Regexp {
		var out []*regexp.Regexp
		for i, p := range pats {
			if p == "" {
				problems = append(problems, fmt.Sprintf("scope.%s[%d] is empty", name, i))
				continue
			}
			re, err := regexp.Compile(p)
			if err != nil {
				problems = append(problems, fmt.Sprintf("scope.%s[%d] %q does not compile: %v", name, i, p, err))
				continue
			}
			out = append(out, re)
		}
		return out
	}
	s.deny = do("deny_regexes", s.DenyRegexes)
	s.allow = do("allow_regexes", s.AllowRegexes)
	s.hold = do("hold_diff_regexes", s.HoldDiffRegexes)
	s.escalate = do("escalate_regexes", s.EscalateRegexes)
	return problems
}

// Cache is one bounded directory.
type Cache struct {
	Path     string `json:"path"`
	MaxBytes int64  `json:"max_bytes"`
}

// Path is where the config was loaded from.
func (c *Config) Path() string { return c.path }

// SetPath records where a config built without Load lives -- embedders and
// tests. Turns are told this path (FACTORYD_CONFIG), so it must be real.
func (c *Config) SetPath(p string) { c.path = p }

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
	if c.Supervisor.FailAbort == 0 {
		c.Supervisor.FailAbort = DefaultFailAbort
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
	c.Health = c.Health.withDefaults()
	for i := range c.Alerts {
		if c.Alerts[i].Kind == "command" && c.Alerts[i].TimeoutSeconds == 0 {
			c.Alerts[i].TimeoutSeconds = DefaultAlertCmdTimeout
		}
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
	if c.Paths.SubmitRepo == "" {
		add("paths.submit_repo is empty; git must run in a repository the producer cannot write")
	}
	if c.Paths.SubmitRepo != "" && c.Paths.SubmitRepo == c.Paths.ProducerWorkdir {
		add("paths.submit_repo is the producer workdir; the two-directory boundary requires them to differ")
	}

	// --- cache root: the only place reclamation may delete ---
	if len(c.Health.Caches) > 0 && c.Paths.CacheRoot == "" {
		add("health.caches declared but paths.cache_root is empty; reclamation deletes, and only inside a dedicated cache root")
	}
	// A configured path with a control character in it is a path that
	// cannot be carried exactly in any line- or tab-delimited handoff (#50
	// review), and no deployment means one; one with the platform's path
	// list separator in it cannot be carried in FACTORYD_TRIGGER_PATHS,
	// which a wrapper splits on it. Both refused at load.
	for _, p := range []struct{ name, path string }{
		{"paths.root", c.Paths.Root}, {"paths.producer_workdir", c.Paths.ProducerWorkdir},
		{"paths.submit_repo", c.Paths.SubmitRepo}, {"paths.cache_root", c.Paths.CacheRoot},
	} {
		if strings.ContainsAny(p.path, "\t\n\r") {
			add("%s %q contains a control character", p.name, p.path)
		}
		if strings.ContainsRune(p.path, os.PathListSeparator) {
			add("%s %q contains %q, the path list separator; FACTORYD_TRIGGER_PATHS could not carry a path under it", p.name, p.path, os.PathListSeparator)
		}
	}
	if cr := c.Paths.CacheRoot; cr != "" {
		switch {
		case !filepath.IsAbs(cr):
			add("paths.cache_root %q is not absolute", cr)
		case filepath.Clean(cr) == string(filepath.Separator):
			add("paths.cache_root is /; reclamation would delete the host")
		default:
			protected := []struct{ name, path string }{
				{"paths.root", c.Paths.Root},
				{"paths.producer_workdir", c.Paths.ProducerWorkdir},
				{"paths.submit_repo", c.Paths.SubmitRepo},
				{"credentials.producer file", parentOf(c.Credentials.Producer.File)},
				{"credentials.reviewer file", parentOf(c.Credentials.Reviewer.File)},
				{"credentials.operator file", parentOf(c.Credentials.Operator.File)},
			}
			for i, a := range c.Alerts {
				if a.Kind == "file" && a.Path != "" {
					protected = append(protected, struct{ name, path string }{fmt.Sprintf("alerts[%d] file", i), parentOf(a.Path)})
				}
			}
			for _, pr := range protected {
				if pr.path == "" {
					continue
				}
				if PathsOverlap(cr, pr.path) {
					add("paths.cache_root %q overlaps %s %q; reclamation may not reach it", cr, pr.name, pr.path)
				}
			}
			for i, cc := range c.Health.Caches {
				if cc.Path != "" && filepath.IsAbs(cc.Path) && !PathWithin(cc.Path, cr) {
					add("health.caches[%d] path %q is not inside paths.cache_root %q", i, cc.Path, cr)
				}
			}
		}
	}

	switch c.Git.Transport {
	case "https":
	case "ssh":
		// Refused rather than degraded. A config asking for ssh and getting
		// https would push over a transport it did not ask for.
		add("git.transport ssh is not implemented in this build; use https")
	case "":
		add("git.transport is empty; expected https")
	default:
		add("git.transport %q is not https or ssh", c.Git.Transport)
	}
	if c.Git.Remote == "" {
		add("git.remote is empty; the transport target is declared, not discovered")
	} else if err := c.remoteMatchesProject(); err != nil {
		add("%v", err)
	}

	if c.Roles.Producer.RunAs == nil || c.Roles.Producer.RunAs.User == "" {
		add("roles.producer.run_as.user is empty; the producer must run as a principal the submit repository is unwritable by")
	}

	switch {
	case c.Gate.RunAs == nil || c.Gate.RunAs.User == "":
		add("gate.run_as.user is empty; the gate runs producer-authored code and must have its own unprivileged identity")
	case c.Roles.Producer.RunAs != nil && c.Gate.RunAs.User == c.Roles.Producer.RunAs.User:
		add("gate.run_as.user is the producer's user; the gate must write inside submit_repo and the producer must not, so they cannot share an identity")
	}
	if _, ok := c.Gate.Env["PATH"]; !ok {
		add("gate.env has no PATH; a gate with no PATH runs nothing, and inheriting one is what this field exists to prevent")
	}
	for _, p := range c.Gate.RequiredWritablePaths {
		if p == "" {
			add("gate.required_writable_paths contains an empty entry")
			continue
		}
		resolved, err := c.ResolveGatePath(p)
		if err != nil {
			add("gate.required_writable_paths: %v", err)
			continue
		}
		// A gate path is provisioned -- created and chowned to the gate --
		// by a privileged submit. One that overlaps the producer's home
		// would hand the gate exactly the traversal the boundary denies it,
		// after doctor and submit both said no.
		if home, herr := c.ProducerHome(); herr == nil && home != "" && PathsOverlap(resolved, home) {
			add("gate.required_writable_paths: %q resolves to %s, which overlaps the producer's home %s; provisioning it would grant the gate the producer's model credential", p, resolved, home)
		}
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
	if o := c.Credentials.Operator; o.File != "" || o.Env != "" {
		if o.File != "" && o.Env != "" {
			add("credentials.operator sets both file and env; exactly one is required")
		}
		if o.Env != "" {
			add("credentials.operator.env is not accepted: an environment variable is readable by whatever inherits it; the operator credential must be a file the role identities cannot read")
		}
		for role, ref := range map[string]CredentialRef{"producer": c.Credentials.Producer, "reviewer": c.Credentials.Reviewer} {
			if ref == o {
				add("credentials.operator and credentials.%s point at the same secret (%s); the operator is a third principal or it is no boundary", role, o.Describe())
			}
		}
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
		if _, ok := spec.Env["PATH"]; !ok {
			add("roles.%s.env has no PATH; a turn with no PATH runs nothing, and inheriting one would inherit everything else too", role)
		}
		// A credential must never be declared into a turn's environment by
		// value; the reviewer's is delivered by name from the supervisor
		// (TurnEnv), and the producer's is not delivered at all.
		for _, ref := range []CredentialRef{c.Credentials.Producer, c.Credentials.Reviewer} {
			if ref.Env != "" {
				if _, declared := spec.Env[ref.Env]; declared {
					add("roles.%s.env declares %s, which is a credential variable; credentials are never written into a config", role, ref.Env)
				}
			}
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
	if sv.FailAbort <= 0 {
		add("supervisor.fail_abort must be positive")
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
			if a.Env["PATH"] == "" {
				add("alerts[%d]: kind command with no PATH in env; the command's environment is declared, not inherited", i)
			}
			if a.TimeoutSeconds < 0 {
				add("alerts[%d]: timeout_seconds is negative", i)
			}
		case "file":
			if a.Path == "" {
				add("alerts[%d]: kind file with no path", i)
			} else if !filepath.IsAbs(a.Path) {
				add("alerts[%d]: path %q is not absolute", i, a.Path)
			}
		case "webhook", "syslog":
			add("alerts[%d]: kind %q is not implemented in this build; a transport that is accepted and never delivers alerts nobody", i, a.Kind)
		case "":
			add("alerts[%d]: kind is empty", i)
		default:
			add("alerts[%d]: kind %q is not one of file, command", i, a.Kind)
		}
	}

	if _, err := c.ProducerHome(); err != nil {
		add("%v", err)
	}

	// FACTORYD_* is reserved: the supervisor generates those for every turn
	// (the config path, the workdir, the triggers, ...). A role that
	// declared one would either be overridden silently or override the
	// truth -- a reviewer turn told a stale config path is the case that was
	// found (canary issue #22, second round).
	for _, r := range []struct {
		name string
		env  map[string]string
	}{{"roles.producer.env", c.Roles.Producer.Env}, {"roles.reviewer.env", c.Roles.Reviewer.Env}, {"gate.env", c.Gate.Env}} {
		for k := range r.env {
			if strings.HasPrefix(k, "FACTORYD_") {
				add("%s declares %s; FACTORYD_* is generated by the supervisor for every turn and is reserved", r.name, k)
			}
		}
	}
	// A credential's environment NAME is an env key too: the reviewer's is
	// injected into its turn by that name. A name that is one of the keys
	// the supervisor generates would put a token where a path belongs. The
	// convention FACTORYD_REVIEWER_TOKEN is a FACTORYD_ name that collides
	// with nothing, so this is judged against the generated keys exactly,
	// from the one list the runner builds from.
	for _, cr := range []struct{ name, env string }{{"credentials.producer.env", c.Credentials.Producer.Env}, {"credentials.reviewer.env", c.Credentials.Reviewer.Env}} {
		if IsGeneratedTurnKey(cr.env) {
			add("%s names %s, which the supervisor generates for every turn; a token would be injected where that value belongs", cr.name, cr.env)
		}
	}

	problems = append(problems, c.Scope.compile()...)
	if c.Scope.DenyRegexes == nil && c.Scope.EscalateRegexes == nil {
		add("scope is absent; the merge gate needs a policy, and an empty one must be declared (\"deny_regexes\": [], \"escalate_regexes\": []) rather than omitted")
	}

	h := c.Health
	for _, k := range []struct {
		n string
		v int
	}{{"interval_seconds", h.IntervalSeconds}, {"alert_after", h.AlertAfter}, {"repeat_seconds", h.RepeatSeconds},
		{"stale_trigger_seconds", h.StaleTriggerSeconds}, {"turn_grace_seconds", h.TurnGraceSeconds}} {
		if k.v <= 0 {
			add("health.%s must be positive", k.n)
		}
	}
	if h.UnreviewedSeconds < 0 {
		add("health.unreviewed_seconds is negative")
	}
	if h.DiskMinFreePercent <= 0 || h.DiskMinFreePercent >= 100 {
		add("health.disk_min_free_percent must be between 1 and 99")
	}
	for i, cc := range h.Caches {
		if cc.Path == "" || !filepath.IsAbs(cc.Path) {
			add("health.caches[%d]: path must be absolute", i)
		}
		if cc.MaxBytes <= 0 {
			add("health.caches[%d]: max_bytes must be positive; a cache with no bound is a defect, not a tuning issue", i)
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid config:\n  - %s", strings.Join(problems, "\n  - "))
}

// StatePath is where this factory's state document lives.
func (c *Config) StatePath() string { return filepath.Join(c.Paths.Root, "state.json") }

// HealthPath is the structured health document the tick writes (§4.5).
func (c *Config) HealthPath() string { return filepath.Join(c.Paths.Root, "health.json") }

// InboxDir and OutboxDir are the filesystem handoff channels (SPEC.md §6.2).
func (c *Config) InboxDir() string  { return filepath.Join(c.Paths.Root, "inbox") }
func (c *Config) OutboxDir() string { return filepath.Join(c.Paths.Root, "outbox") }

// ProgressPath is where a role touches to say "I advanced". The producer's is
// named by SPEC.md §6.3; the reviewer gets the symmetric one.
func (c *Config) ProgressPath(role string) string {
	return filepath.Join(c.InboxDir(), role+"-progress")
}

// RetryPath is the supervisor-owned trigger a role re-arms on after a turn
// consumed its trigger and then failed. Supervisor-owned: the supervisor writes
// it and removes it, and an agent never needs to know it exists. A file rather
// than an in-memory flag so the retry survives a supervisor restart -- an
// in-memory retry lost on restart recreates the exact stall it exists to fix.
func (c *Config) RetryPath(role string) string {
	return filepath.Join(c.InboxDir(), role+"-retry")
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

// remoteMatchesProject checks that git.remote addresses the repository the
// provider block names. A remote pointing at a different project is the right
// account pushing to the wrong place, and no identity check would catch it.
func (c *Config) remoteMatchesProject() error {
	u, err := url.Parse(c.Git.Remote)
	if err != nil {
		return fmt.Errorf("git.remote %q does not parse: %w", c.Git.Remote, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("git.remote %q must be an https URL for the https transport", c.Git.Remote)
	}
	if u.User != nil {
		return fmt.Errorf("git.remote carries credentials in the URL; the credential is supplied separately and never through a URL")
	}
	path := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")

	// The authority is pinned as well as the path. A remote at
	// https://evil.example/acme/widgets.git names the right project on the
	// wrong host, and every downstream check -- the guard, the identity
	// oracle -- would then faithfully verify and push the producer's
	// credential to it. The expected host is derived from the provider block,
	// never from the remote itself.
	wantHost, err := c.ProviderGitHost()
	if err != nil {
		return err
	}
	if !strings.EqualFold(u.Host, wantHost) {
		return fmt.Errorf("git.remote is on %q but the provider endpoint is %q; the transport would push the producer's credential to a host that is not the provider", u.Host, wantHost)
	}

	var want string
	switch c.Provider {
	case "github":
		if c.GitHub != nil {
			want = c.GitHub.Owner + "/" + c.GitHub.Repo
		}
	case "gitlab":
		if c.GitLab != nil {
			want = strings.Trim(c.GitLab.Project, "/")
		}
	}
	if want != "" && !strings.EqualFold(path, want) {
		return fmt.Errorf("git.remote addresses %q but the provider block names %q; the transport would push to a different repository than the one being reviewed", path, want)
	}
	return nil
}

// ProviderGitHost is the host (with port, if any) git must talk to, derived
// from the provider's API endpoint. github.com's API lives on api.github.com
// while git lives on github.com; a GitHub Enterprise or GitLab instance serves
// both from one host, so the API base_url's authority is the git authority.
func (c *Config) ProviderGitHost() (string, error) {
	switch c.Provider {
	case "github":
		base := ""
		if c.GitHub != nil {
			base = c.GitHub.BaseURL
		}
		if base == "" || strings.EqualFold(base, "https://api.github.com") || strings.EqualFold(base, "https://api.github.com/") {
			return "github.com", nil
		}
		u, err := url.Parse(base)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("github.base_url %q does not parse to a host", base)
		}
		return u.Host, nil
	case "gitlab":
		base := "https://gitlab.com/api/v4"
		if c.GitLab != nil && c.GitLab.BaseURL != "" {
			base = c.GitLab.BaseURL
		}
		u, err := url.Parse(base)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("gitlab.base_url %q does not parse to a host", base)
		}
		return u.Host, nil
	default:
		return "", fmt.Errorf("provider %q has no git host", c.Provider)
	}
}

// GateEnv is the gate's complete environment: FACTORYD_* first, then gate.env.
// Nothing else. Built from configuration alone, so doctor and submit compute
// the identical environment from the identical file.
func (c *Config) GateEnv(factoryd map[string]string) []string {
	keys := make([]string, 0, len(c.Gate.Env)+len(factoryd))
	seen := map[string]bool{}
	env := map[string]string{}
	for k, v := range factoryd {
		env[k] = v
	}
	for k, v := range c.Gate.Env {
		env[k] = v // gate.env wins
	}
	for k := range env {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

var gateVarRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ResolveGatePath expands ${VAR} from gate.env and resolves a relative path
// against the gate workdir. An unset variable is an error: expanding
// ${GOCACHE}/x to /x would check a path the gate never touches and pass.
func (c *Config) ResolveGatePath(p string) (string, error) {
	var missing []string
	expanded := gateVarRef.ReplaceAllStringFunc(p, func(m string) string {
		name := gateVarRef.FindStringSubmatch(m)[1]
		v, ok := c.Gate.Env[name]
		if !ok {
			missing = append(missing, name)
			return m
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("path %q references ${%s}, which gate.env does not set", p, strings.Join(missing, "}, ${"))
	}
	if strings.ContainsAny(expanded, "*?[") || strings.HasPrefix(expanded, "~") {
		return "", fmt.Errorf("path %q: no globbing and no ~; write the path out", p)
	}
	if !filepath.IsAbs(expanded) {
		expanded = filepath.Join(c.GateWorkdir(), expanded)
	}
	expanded = filepath.Clean(expanded)
	if err := c.gatePathMayNotReachGit(p, expanded); err != nil {
		return "", err
	}
	return expanded, nil
}

// gatePathMayNotReachGit refuses a declared path that is .git, is inside .git,
// or is an ancestor of .git -- including the submit repository itself and
// everything above it.
//
// A declared path is a capability grant: submit creates it owned by the gate
// user. "." would hand the gate the repository root, from which it can rename,
// delete or replace .git; "/" or any ancestor would too. The .git probe would
// then be green about a state the provisioning had just destroyed. This is the
// lexical half; doctor repeats it after physical resolution, since a symlink
// can make a harmless-looking path land somewhere else.
func (c *Config) gatePathMayNotReachGit(declared, resolved string) error {
	gitDir := filepath.Join(c.Paths.SubmitRepo, ".git")
	switch {
	case resolved == gitDir:
		return fmt.Errorf("path %q is the submit repository's .git; the gate may never own it", declared)
	case strings.HasPrefix(resolved, gitDir+string(os.PathSeparator)):
		return fmt.Errorf("path %q is inside the submit repository's .git; the gate may never own it", declared)
	case gitDir == resolved || strings.HasPrefix(gitDir, resolved+string(os.PathSeparator)):
		return fmt.Errorf("path %q is an ancestor of the submit repository's .git; owning it would let the gate rename, delete or replace .git", declared)
	}
	return nil
}

// GateWorkdir is where the gate runs: the submit repository, since that is
// where the materialised change lives.
func (c *Config) GateWorkdir() string { return c.Paths.SubmitRepo }

// GeneratedTurnKeys are the environment variables the supervisor sets for
// every turn. The runner builds its map from this list and a test holds the
// two together; Validate refuses a credential named after any of them.
var GeneratedTurnKeys = []string{
	"FACTORYD_FACTORY", "FACTORYD_ROLE", "FACTORYD_TURN", "FACTORYD_ROOT",
	"FACTORYD_INBOX", "FACTORYD_OUTBOX", "FACTORYD_WORKDIR", "FACTORYD_TARGET_BRANCH",
	"FACTORYD_PROGRESS", "FACTORYD_TRIGGERS", "FACTORYD_TRIGGER_PATHS", "FACTORYD_CONFIG",
	"FACTORYD_VERDICTS", "FACTORYD_VERDICTS_TSV", "FACTORYD_VERDICT", "FACTORYD_CHANGE_ID", "FACTORYD_CHANGE_BRANCH",
}

// IsGeneratedTurnKey reports whether name is one the supervisor generates.
func IsGeneratedTurnKey(name string) bool {
	for _, k := range GeneratedTurnKeys {
		if k == name {
			return true
		}
	}
	return false
}

// TurnEnv is the complete environment for a role's turn: FACTORYD_* first,
// then roles.<role>.env, then -- for the reviewer only -- the reviewer's
// credential variable, if credentials.reviewer.env names one, copied by name
// from the supervisor's environment. The producer receives no credential
// variable of either role: it never runs git and never calls the API.
//
// supervisorEnv is consulted for exactly one name and nothing else; it is not
// merged, filtered, or defaulted from.
func (c *Config) TurnEnv(role string, factoryd map[string]string, supervisorEnv []string) []string {
	spec, _ := c.RoleSpec(role)
	env := map[string]string{}
	for k, v := range spec.Env {
		env[k] = v
	}
	// The reviewer's credential, by the name the config chose, taken from
	// the supervisor's own environment. Injected BEFORE the generated
	// values: a credential named FACTORYD_CONFIG would otherwise replace the
	// config path with the token, and a reviewer turn would carry a secret
	// where a path belongs and leak it through the first error that printed
	// it. Validate refuses such a name; this is the second lock.
	if role == "reviewer" && c.Credentials.Reviewer.Env != "" {
		name := c.Credentials.Reviewer.Env
		for _, kv := range supervisorEnv {
			if k, v, ok := strings.Cut(kv, "="); ok && k == name {
				env[name] = v
				break
			}
		}
	}
	// Generated values are applied LAST, so they win over everything: role
	// env and credential alike. FACTORYD_* is the supervisor's namespace.
	// Validate refuses a role env or credential name that claims one; this
	// ordering is the second lock, for a config that reached here without
	// Validate, and it makes the order of the loops the rule rather than an
	// accident.
	for k, v := range factoryd {
		env[k] = v
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// ProducerHome is the producer's declared HOME, the directory whose
// contents the gate must not reach. It is "" when none is declared, and
// an ERROR when the declared value is relative: to the turn a relative
// HOME means "under the producer workdir" (the runner starts it there), to
// doctor or submit it would mean "under wherever this process runs" -- and
// a probe green about the wrong directory is worse than no probe. Every
// caller takes the home through this, so no path can see the raw value.
func (c *Config) ProducerHome() (string, error) {
	home := c.Roles.Producer.Env["HOME"]
	if home == "" {
		return "", nil
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("roles.producer.env.HOME %q is relative; it would name a directory under the producer workdir to the turn and a directory under the probing process's own working directory to doctor and submit, so the boundary could not be enforced", home)
	}
	return filepath.Clean(home), nil
}

// PathWithin reports whether p is inside (or is) dir, lexically. Both are
// cleaned; neither is resolved -- the physical check belongs to the moment
// of use, where a symlink can still have changed since load.
func PathWithin(p, dir string) bool {
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	if p == dir {
		return true
	}
	if dir == string(filepath.Separator) {
		return true
	}
	return strings.HasPrefix(p, dir+string(filepath.Separator))
}

// PathsOverlap reports whether one path is inside the other, either way.
func PathsOverlap(a, b string) bool { return PathWithin(a, b) || PathWithin(b, a) }

func parentOf(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Dir(p)
}

// LookPathIn resolves cmd against an explicit PATH, the way the process that
// will run it would. Resolving against the caller's own PATH answers a
// different question: whether doctor could run it, not whether the gate or the
// turn can.
func LookPathIn(path, cmd string) (string, error) {
	if strings.Contains(cmd, "/") {
		if fi, err := os.Stat(cmd); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return cmd, nil
		}
		return "", fmt.Errorf("%q is not an executable file", cmd)
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, cmd)
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return cand, nil
		}
	}
	return "", fmt.Errorf("%q not found on the declared PATH %q", cmd, path)
}
