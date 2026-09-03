// Package gittransport performs submit's git operations under an identity it
// can state, in an environment it owns (SPEC.md §5.4).
//
// The provider driver speaks the API; nothing in it touches git, and the two
// authenticate by different mechanisms. That is how a producer came to push
// under the reviewer's credential while every API check passed: fetch and push
// do not take the token you pass to the API, they consult whatever the ambient
// credential helper returns first.
//
// The invariant every rule here serves: a verification is valid only for an
// operation it shares an environment with, and only while nothing can change
// that environment in between. So the environment is constructed, not
// inherited; the repository is one the producer cannot write; and the checks
// run inside the transport, immediately before the operation they vouch for.
package gittransport

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/scm"
)

// Environment is what every git process is started with. Nothing else.
//
// An earlier design dropped inherited GIT_* variables, which is a denylist
// wearing an environment's clothes: http_proxy, https_proxy, all_proxy,
// NO_PROXY, SSL_CERT_FILE, SSL_CERT_DIR and CURL_CA_BUNDLE all steer git's
// HTTPS transport and none starts with GIT_.
func Environment(cfg *config.Config) []string {
	env := map[string]string{
		"PATH": cfg.Gate.Env["PATH"],
		"HOME": cfg.Gate.Env["HOME"],
		// Only configuration submit sets explicitly applies.
		"GIT_CONFIG_GLOBAL":   "/dev/null",
		"GIT_CONFIG_SYSTEM":   "/dev/null",
		"GIT_CONFIG_NOSYSTEM": "1",
		// A missing credential fails rather than waiting for a human who is
		// not there.
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "/bin/false",
		"SSH_ASKPASS":         "/bin/false",
		// Neutralise the proxy environment in git's own vocabulary as well,
		// so that even a stray value in an inherited variable list would be
		// overridden by explicit empties.
		"NO_PROXY": "",
		"no_proxy": "",
		"LC_ALL":   "C",
	}
	if env["HOME"] == "" {
		env["HOME"] = cfg.Paths.Root
	}
	if cfg.Git.Proxy != "" {
		env["https_proxy"] = cfg.Git.Proxy
		env["HTTPS_PROXY"] = cfg.Git.Proxy
	}
	if cfg.Git.CAFile != "" {
		env["GIT_SSL_CAINFO"] = cfg.Git.CAFile
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

// allowedLocalKeys are the repository-local configuration keys a clone needs
// to be a clone, and nothing else. Every http.*, every url.*, credential.*,
// core.sshCommand, core.worktree, include.path and includeIf.* is absent or
// submit does not run. A key Git adds next year is refused by default rather
// than admitted by omission.
var allowedLocalKeys = map[string]bool{
	"core.repositoryformatversion": true,
	"core.filemode":                true,
	"core.bare":                    true,
	"core.logallrefupdates":        true,
	"core.symlinks":                true,
	"core.ignorecase":              true,
	"core.precomposeunicode":       true,
	"extensions.objectformat":      true,
}

// allowedLocalPrefixes cover remote.<name>.{url,fetch} and
// branch.<name>.{merge,remote}.
func localKeyAllowed(key string) bool {
	key = strings.ToLower(key)
	if allowedLocalKeys[key] {
		return true
	}
	parts := strings.Split(key, ".")
	if len(parts) == 3 {
		switch parts[0] {
		case "remote":
			return parts[2] == "url" || parts[2] == "fetch"
		case "branch":
			return parts[2] == "merge" || parts[2] == "remote"
		}
	}
	return false
}

// AllowlistViolations returns every repository-local key not on the allowlist.
// It reads the file git reads, in git's own parse, so an include or a
// differently-cased key cannot slip past a hand-written parser.
func (t *Transport) AllowlistViolations() ([]string, error) {
	out, err := t.git("config", "--list", "--local", "--name-only")
	if err != nil {
		return nil, fmt.Errorf("reading local git config: %w", err)
	}
	var bad []string
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		key := strings.TrimSpace(line)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if !localKeyAllowed(key) {
			bad = append(bad, key)
		}
	}
	sort.Strings(bad)
	return bad, nil
}

// credentialFile writes the producer credential where git's helper will read
// it: a file the process owns at mode 0600, removed by the caller. Never argv
// -- /proc/<pid>/cmdline is world-readable -- and never a URL.
func credentialFile(dir string, cred Credential) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp(dir, ".factoryd-cred-*")
	if err != nil {
		return "", nil, err
	}
	path = f.Name()
	cleanup = func() { _ = os.Remove(path) }
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, err
	}
	// git-credential protocol: key=value lines, blank line terminates.
	body := fmt.Sprintf("username=%s\npassword=%s\n", cred.Username, cred.Secret)
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

// helperArg is the credential.helper value that reads the file above. The
// helper is a shell one-liner git runs; it only ever emits what the file
// holds, and the file path is the only thing that appears in git's config.
func helperArg(credPath string) string {
	return fmt.Sprintf("!f() { cat %q; }; f", credPath)
}

// Credential is scm.GitCredential: the username half is provider-owned, and
// which username git picked decided which identity pushed in the incident
// behind SPEC.md §1 item 11.
type Credential = scm.GitCredential

func cleanPath(p string) string { return filepath.Clean(p) }
