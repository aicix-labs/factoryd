package gittransport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/scm"
)

// Identity is who the transport authenticates as at the remote.
type Identity struct {
	Login string
	// Source names how it was resolved: "credential-helper" for https.
	Source string
}

// Transport runs git in the submit repository, under the constructed
// environment, with the producer credential and nothing else.
type Transport struct {
	cfg    *config.Config
	driver scm.Driver
	// cred is the producer's git credential. It is written to a 0600 file
	// for the duration of each operation and removed afterwards.
	cred Credential
	// Git is the binary; tests may point it at a wrapper.
	Git string
	// Timeout bounds one git invocation.
	Timeout time.Duration
}

// New builds a Transport for cfg, using d to resolve identities and to supply
// the provider's username convention. secret is the producer's token.
func New(cfg *config.Config, d scm.Driver, secret string) (*Transport, error) {
	if cfg.Git.Transport != "https" {
		return nil, fmt.Errorf("gittransport: transport %q is not implemented in this build", cfg.Git.Transport)
	}
	if secret == "" {
		return nil, fmt.Errorf("gittransport: refusing to operate with an empty credential")
	}
	if d == nil {
		return nil, fmt.Errorf("gittransport: no driver")
	}
	// The git binary is resolved from the DECLARED PATH, not the parent's.
	// exec.Command resolves a bare "git" against factoryd's own inherited
	// PATH before cmd.Env is ever consulted, so a wrapper first on the
	// service's PATH would run -- and receive the producer's credential --
	// despite the constructed child environment. An absolute path closes it.
	gitExe, err := GitBinary(cfg)
	if err != nil {
		return nil, err
	}
	return &Transport{
		cfg: cfg, driver: d, cred: d.GitCredential(secret),
		Git: gitExe, Timeout: 5 * time.Minute,
	}, nil
}

// GitBinary resolves git against the declared gate PATH -- the PATH every git
// process is given -- and returns an absolute path. doctor verifies this exact
// executable; submit and the transport execute it and nothing else.
func GitBinary(cfg *config.Config) (string, error) {
	exe, err := config.LookPathIn(cfg.Gate.Env["PATH"], "git")
	if err != nil {
		return "", fmt.Errorf("gittransport: %w", err)
	}
	return exe, nil
}

// Repo is the directory every git command runs in.
func (t *Transport) Repo() string { return t.cfg.Paths.SubmitRepo }

// git runs one git command in the submit repository under the constructed
// environment. It never inherits the parent's environment.
func (t *Transport) git(args ...string) (string, error) {
	return t.gitWith(nil, args...)
}

// gitWith runs git with extra -c overrides applied ahead of args. A -c beats
// the repository's own config, which is how credential.helper and
// core.sshCommand are pinned regardless of what the local file says.
func (t *Transport) gitWith(overrides []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), t.Timeout)
	defer cancel()

	argv := []string{}
	for _, o := range overrides {
		argv = append(argv, "-c", o)
	}
	argv = append(argv, args...)

	cmd := exec.CommandContext(ctx, t.Git, argv...)
	cmd.Dir = t.Repo()
	cmd.Env = Environment(t.cfg)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// credentialOverrides clears any inherited helper list and installs one that
// reads the producer credential from path. The empty first value is git's
// documented way to reset the multi-valued key before appending.
func credentialOverrides(path string) []string {
	return []string{
		"credential.helper=",
		"credential.helper=" + helperArg(path),
		// Never use a helper's cached value from anywhere else.
		"credential.useHttpPath=true",
	}
}

// Guard is the check that runs inside the transport, immediately before every
// fetch and push (SPEC.md §5.4): the repository's local config carries only
// allowlisted keys, and the URL git will actually use -- after rewriting -- is
// the configured remote. Run here, not only as a doctor preflight, because a
// preflight answers a question about a moment that has passed.
func (t *Transport) Guard() error {
	bad, err := t.AllowlistViolations()
	if err != nil {
		return err
	}
	if len(bad) > 0 {
		return fmt.Errorf("gittransport: the submit repository's local git config carries keys outside the allowlist: %s; refusing to run git against it",
			strings.Join(bad, ", "))
	}
	fetchURL, pushURL, err := t.EffectiveURLs()
	if err != nil {
		return err
	}
	if fetchURL != t.cfg.Git.Remote {
		return fmt.Errorf("gittransport: git would fetch from %q, not the configured %q; a rewrite is in force", fetchURL, t.cfg.Git.Remote)
	}
	if pushURL != t.cfg.Git.Remote {
		return fmt.Errorf("gittransport: git would push to %q, not the configured %q", pushURL, t.cfg.Git.Remote)
	}
	return nil
}

// EffectiveURLs asks git, under the transport's own environment, what it would
// actually do with the configured remote.
//
// The fetch URL is asked for directly: git applies url.<base>.insteadOf to an
// explicit URL too, which is exactly why it has to be asked rather than
// assumed. The push side cannot be asked the same way -- git will not resolve
// --push for a remote that exists only as a -c override -- so instead the
// effective configuration is scanned for ANY rewrite rule, fetch or push. With
// the global and system files at /dev/null and the local file allowlisted, a
// rule found here can only have come from somewhere factoryd did not own, and
// its mere presence is the refusal.
func (t *Transport) EffectiveURLs() (fetch, push string, err error) {
	f, err := t.git("ls-remote", "--get-url", t.cfg.Git.Remote)
	if err != nil {
		return "", "", err
	}
	fetch = strings.TrimSpace(f)

	rules, err := t.rewriteRules()
	if err != nil {
		return "", "", err
	}
	if len(rules) > 0 {
		return fetch, "", fmt.Errorf("gittransport: rewrite rules are in force in the effective git config: %s", strings.Join(rules, "; "))
	}
	// No rule anywhere git would read: the push URL is the configured one.
	return fetch, t.cfg.Git.Remote, nil
}

// rewriteRules returns every url.*.insteadOf / pushInsteadOf rule git can see.
func (t *Transport) rewriteRules() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), t.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.Git, "config", "--get-regexp", `^url\..*\.(insteadof|pushinsteadof)$`)
	cmd.Dir = t.Repo()
	cmd.Env = Environment(t.cfg)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return nil, nil // exit 1: no matching key, which is the good outcome
	}
	if err != nil {
		return nil, fmt.Errorf("git config --get-regexp: %w", err)
	}
	var rules []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line != "" {
			rules = append(rules, line)
		}
	}
	return rules, nil
}

// Identity resolves who git would authenticate as, by asking the mechanism
// that will be used: git credential fill, under the same isolation and helper
// Fetch and Push use, and then the provider API for whose token that is.
func (t *Transport) Identity(ctx context.Context) (Identity, error) {
	path, cleanup, err := credentialFile(t.Repo(), t.cred)
	if err != nil {
		return Identity{}, err
	}
	defer cleanup()

	// git credential fill reads a description on stdin.
	desc := fmt.Sprintf("url=%s\n\n", t.cfg.Git.Remote)
	got, err := t.credentialFill(path, desc)
	if err != nil {
		return Identity{}, err
	}
	if got.Username == "" || got.Secret == "" {
		return Identity{}, fmt.Errorf("gittransport: git credential fill returned no credential for %s; identity is undecided, which is not the same as satisfied", t.cfg.Git.Remote)
	}
	if got.Secret != t.cred.Secret {
		// Something other than our helper answered. The whole point of the
		// isolation is that this cannot happen; if it does, say so.
		return Identity{}, fmt.Errorf("gittransport: git resolved a credential other than the producer's for %s; an ambient helper is in force", t.cfg.Git.Remote)
	}
	// Whose token is it? Ask the provider, with that exact secret.
	who, err := t.whoamiWith(ctx, got.Secret)
	if err != nil {
		return Identity{}, fmt.Errorf("gittransport: resolving the git credential's identity: %w", err)
	}
	return Identity{Login: who.Login, Source: "credential-helper"}, nil
}

func (t *Transport) credentialFill(credPath, desc string) (Credential, error) {
	ctx, cancel := context.WithTimeout(context.Background(), t.Timeout)
	defer cancel()
	argv := []string{}
	for _, o := range credentialOverrides(credPath) {
		argv = append(argv, "-c", o)
	}
	argv = append(argv, "credential", "fill")
	cmd := exec.CommandContext(ctx, t.Git, argv...)
	cmd.Dir = t.Repo()
	cmd.Env = Environment(t.cfg)
	cmd.Stdin = strings.NewReader(desc)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return Credential{}, fmt.Errorf("git credential fill: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	var c Credential
	for _, line := range strings.Split(out.String(), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "username":
			c.Username = v
		case "password":
			c.Secret = v
		}
	}
	return c, nil
}

// whoamiWith resolves the identity of an arbitrary secret through the same
// provider driver, so the answer comes from the API and not from
// configuration.
func (t *Transport) whoamiWith(ctx context.Context, secret string) (scm.Identity, error) {
	return t.driver.WhoamiWith(ctx, secret)
}

// Fetch fetches refspec from the configured remote, named explicitly.
func (t *Transport) Fetch(ctx context.Context, refspec string) error {
	return t.networkOp(ctx, "fetch", refspec)
}

// Push pushes refspec to the configured remote, named explicitly.
func (t *Transport) Push(ctx context.Context, refspec string) error {
	return t.networkOp(ctx, "push", refspec)
}

func (t *Transport) networkOp(ctx context.Context, verb, refspec string) error {
	if err := t.Guard(); err != nil {
		return err
	}
	path, cleanup, err := credentialFile(t.Repo(), t.cred)
	if err != nil {
		return err
	}
	defer cleanup()
	_, err = t.gitWith(credentialOverrides(path), verb, t.cfg.Git.Remote, refspec)
	if err != nil {
		return fmt.Errorf("gittransport: %s: %w", verb, err)
	}
	return nil
}

// LocalConfigPath is the file the allowlist governs.
func (t *Transport) LocalConfigPath() string {
	return cleanPath(t.Repo() + string(os.PathSeparator) + ".git" + string(os.PathSeparator) + "config")
}
