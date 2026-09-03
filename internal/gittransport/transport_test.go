package gittransport_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/gittransport"
	"github.com/aicix-labs/factoryd/internal/scm"
)

// fakeDriver resolves any secret to a login derived from it, and uses a fixed
// username convention. It never makes a request.
type fakeDriver struct {
	scm.Driver
	username string
	whoami   func(secret string) (scm.Identity, error)
}

func (f fakeDriver) Provider() string { return "fake" }
func (f fakeDriver) GitCredential(secret string) scm.GitCredential {
	return scm.GitCredential{Username: f.username, Secret: secret}
}
func (f fakeDriver) WhoamiWith(_ context.Context, secret string) (scm.Identity, error) {
	return f.whoami(secret)
}

type lab struct {
	t      *testing.T
	root   string
	bare   string // the "remote"
	submit string // the factoryd-owned clone
	work   string // the producer's tree
	cfg    *config.Config
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// newLab builds a bare "remote", a factoryd clone of it, and a producer tree.
// The remote URL is a file:// URL rewritten to look like the https one via a
// planted rule ONLY in the tests that plant one; by default the configured
// remote is the bare repo itself so fetch and push work with no network.
func newLab(t *testing.T) *lab {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	l := &lab{t: t, root: root,
		bare: filepath.Join(root, "remote.git"), submit: filepath.Join(root, "submit"), work: filepath.Join(root, "work")}
	git(t, root, "init", "-q", "--bare", "-b", "main", l.bare)
	seed := filepath.Join(root, "seed")
	git(t, root, "init", "-q", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-q", "-m", "seed")
	git(t, seed, "push", "-q", l.bare, "main")
	git(t, root, "clone", "-q", l.bare, l.submit)
	if err := os.MkdirAll(l.work, 0o755); err != nil {
		t.Fatal(err)
	}

	l.cfg = &config.Config{
		SchemaVersion: config.SchemaVersion, Health: config.DefaultHealth(), Name: "lab", Provider: "github",
		GitHub: &config.GitHub{Owner: "acme", Repo: "widgets"}, TargetBranch: "main",
		Git:   config.Git{Remote: "file://" + l.bare, Transport: "https"},
		Paths: config.Paths{Root: root, ProducerWorkdir: l.work, SubmitRepo: l.submit},
		Gate:  config.Gate{Command: []string{"true"}, Env: map[string]string{"PATH": os.Getenv("PATH"), "HOME": root}, RunAs: &config.RunAs{User: "factoryd-gate"}},
	}
	return l
}

func (l *lab) transport(secret string) *gittransport.Transport {
	l.t.Helper()
	d := fakeDriver{username: "x-access-token", whoami: func(s string) (scm.Identity, error) {
		return scm.Identity{ID: "id-" + s, Login: "user-of-" + s}, nil
	}}
	tr, err := gittransport.New(l.cfg, d, secret)
	if err != nil {
		l.t.Fatal(err)
	}
	return tr
}

func (l *lab) plantLocal(key, value string) {
	l.t.Helper()
	git(l.t, l.submit, "config", "--local", key, value)
}

// ---------- the guard ----------

func TestGuardPassesACleanClone(t *testing.T) {
	l := newLab(t)
	if err := l.transport("tok").Guard(); err != nil {
		t.Fatalf("a fresh clone failed the guard: %v", err)
	}
}

// A rewrite changes where git goes without changing what was configured. The
// guard asks git for the effective URL rather than trusting the config.
func TestGuardRefusesAnInsteadOfRewrite(t *testing.T) {
	l := newLab(t)
	l.plantLocal("url.https://evil.example/.insteadOf", "file://")
	err := l.transport("tok").Guard()
	if err == nil {
		t.Fatal("a url.*.insteadOf rewrite passed the guard")
	}
	// Refused twice over: the key is not allowlisted, AND the URL moved. The
	// allowlist fires first; assert the refusal names the key.
	if !strings.Contains(err.Error(), "insteadof") {
		t.Fatalf("refusal does not name the rewrite: %v", err)
	}
}

// Same for a push-only rewrite, which leaves the fetch URL intact.
func TestGuardRefusesAPushInsteadOfRewrite(t *testing.T) {
	l := newLab(t)
	l.plantLocal("url.https://evil.example/.pushInsteadOf", "file://")
	if err := l.transport("tok").Guard(); err == nil {
		t.Fatal("a pushInsteadOf rewrite passed the guard")
	}
}

// The control that shows the URL check alone was never sufficient: a proxy or
// TLS setting leaves the effective URL exactly as configured, and must still
// be refused.
func TestGuardRefusesProxyAndTLSSettingsWithURLUnchanged(t *testing.T) {
	for _, c := range []struct{ key, value string }{
		{"http.proxy", "http://interceptor.example:3128"},
		{"http.sslVerify", "false"},
		{"http.sslCAInfo", "/tmp/not-the-real-ca.pem"},
		{"http." + "file://" + "/.proxy", "http://interceptor.example:3128"},
	} {
		t.Run(c.key, func(t *testing.T) {
			l := newLab(t)
			l.plantLocal(c.key, c.value)
			tr := l.transport("tok")

			// The URL check passes -- that is the point.
			f, p, err := tr.EffectiveURLs()
			if err != nil {
				t.Fatal(err)
			}
			if f != l.cfg.Git.Remote || p != l.cfg.Git.Remote {
				t.Fatalf("the effective URL moved (%q / %q); this control needs it unchanged", f, p)
			}
			// And the guard must still refuse.
			if err := tr.Guard(); err == nil {
				t.Fatalf("%s=%s passed the guard with the URL unchanged; the allowlist is not in force", c.key, c.value)
			}
		})
	}
}

// Anything outside the allowlist -- including a key Git might add next year.
func TestGuardIsDefaultDeny(t *testing.T) {
	for _, key := range []string{"credential.helper", "core.sshCommand", "core.worktree", "include.path", "some.futureKey"} {
		t.Run(key, func(t *testing.T) {
			l := newLab(t)
			l.plantLocal(key, "x")
			if err := l.transport("tok").Guard(); err == nil {
				t.Fatalf("%s passed the guard; the allowlist is not default-deny", key)
			}
		})
	}
}

// The guard runs inside the operation, not only as a preflight: a rewrite
// planted after a green guard must still stop the push.
func TestPushReGuardsImmediatelyBefore(t *testing.T) {
	l := newLab(t)
	tr := l.transport("tok")
	if err := tr.Guard(); err != nil {
		t.Fatal(err)
	}
	l.plantLocal("url.https://evil.example/.insteadOf", "file://") // after the preflight
	git(t, l.submit, "checkout", "-q", "-b", "producer/x")
	if err := os.WriteFile(filepath.Join(l.submit, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, l.submit, "add", ".")
	git(t, l.submit, "commit", "-q", "-m", "x")

	err := tr.Push(context.Background(), "producer/x")
	if err == nil {
		t.Fatal("a rewrite planted after the preflight did not stop the push")
	}
	// And the bare remote must not have received the branch.
	out := git(t, l.bare, "branch", "--list", "producer/x")
	if strings.TrimSpace(out) != "" {
		t.Fatal("the push went through despite the guard")
	}
}

// ---------- the environment ----------

// Ambient proxy variables must not reach git; a declared proxy must.
func TestEnvironmentIsConstructed(t *testing.T) {
	l := newLab(t)
	t.Setenv("https_proxy", "http://ambient.example:3128")
	t.Setenv("SSL_CERT_FILE", "/ambient/ca.pem")
	t.Setenv("GIT_SSH_COMMAND", "evil-ssh")
	env := strings.Join(gittransport.Environment(l.cfg), "\n")
	for _, leaked := range []string{"ambient.example", "/ambient/ca.pem", "evil-ssh"} {
		if strings.Contains(env, leaked) {
			t.Errorf("ambient %q reached the git environment", leaked)
		}
	}
	for _, want := range []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0"} {
		if !strings.Contains(env, want) {
			t.Errorf("git environment is missing %s", want)
		}
	}
	// The positive control: a proxy that IS declared takes effect.
	l.cfg.Git.Proxy = "http://declared.example:3128"
	if !strings.Contains(strings.Join(gittransport.Environment(l.cfg), "\n"), "https_proxy=http://declared.example:3128") {
		t.Fatal("a declared git.proxy did not reach the environment; \"unaffected by ambient\" could just mean \"ignores proxies\"")
	}
}

// ---------- identity ----------

// Identity asks git what credential it would use, then asks the provider whose
// it is. The secret must be the producer's, and it must never appear in argv.
func TestIdentityResolvesThroughCredentialFill(t *testing.T) {
	l := newLab(t)
	tr := l.transport("producer-secret")
	id, err := tr.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.Login != "user-of-producer-secret" || id.Source != "credential-helper" {
		t.Fatalf("Identity = %+v", id)
	}
	// No credential file left behind.
	entries, _ := os.ReadDir(l.submit)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".factoryd-cred-") {
			t.Fatalf("credential file %s survived the operation", e.Name())
		}
	}
}

// If some other helper answered, that is the incident, and it must be named.
func TestIdentityRefusesAForeignCredential(t *testing.T) {
	l := newLab(t)
	// A local helper that returns the REVIEWER's token, ahead of ours.
	l.plantLocal("credential.helper", "!f() { echo username=reviewer; echo password=reviewer-secret; }; f")
	tr := l.transport("producer-secret")
	// Guard would refuse the key; Identity is asked directly to show that
	// even then, our -c override wins and the foreign helper never answers.
	id, err := tr.Identity(context.Background())
	if err != nil {
		t.Fatalf("Identity failed: %v", err)
	}
	if id.Login != "user-of-producer-secret" {
		t.Fatalf("git resolved %q; the local credential.helper was consulted despite the override", id.Login)
	}
}

func TestIdentityIsUndecidedWhenTheProviderCannotSay(t *testing.T) {
	l := newLab(t)
	d := fakeDriver{username: "x-access-token", whoami: func(string) (scm.Identity, error) {
		return scm.Identity{}, errors.New("401")
	}}
	tr, err := gittransport.New(l.cfg, d, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Identity(context.Background()); err == nil {
		t.Fatal("an unresolvable identity was reported as an identity")
	}
}

// ---------- the copy ----------

func TestCopyTreeExcludesGitControlDataAtAnyDepth(t *testing.T) {
	l := newLab(t)
	for _, p := range []string{"src/a.go", "nested/.git/config", "nested/keep.txt", ".git/config", ".producer-branch"} {
		if err := os.MkdirAll(filepath.Join(l.work, filepath.Dir(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(l.work, p), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := gittransport.CopyTree(l.work, l.submit); err != nil {
		t.Fatal(err)
	}
	for _, mustExist := range []string{"src/a.go", "nested/keep.txt"} {
		if _, err := os.Stat(filepath.Join(l.submit, mustExist)); err != nil {
			t.Errorf("%s was not copied", mustExist)
		}
	}
	for _, mustNot := range []string{"nested/.git", ".producer-branch"} {
		if _, err := os.Stat(filepath.Join(l.submit, mustNot)); err == nil {
			t.Errorf("%s crossed the boundary", mustNot)
		}
	}
	// The submit repo's OWN .git is intact and is still factoryd's.
	out := git(t, l.submit, "rev-parse", "--is-inside-work-tree")
	if strings.TrimSpace(out) != "true" {
		t.Fatal("the submit repository lost its .git")
	}
	if b, _ := os.ReadFile(filepath.Join(l.submit, ".git", "config")); strings.Contains(string(b), "x") && len(b) == 1 {
		t.Fatal("the producer's .git/config overwrote the submit repository's")
	}
}

// A link that is "inside" the producer tree but resolves into .git once
// recreated in the submit repository: the producer plants hooks -> .git/hooks
// (its own .git is skipped, so in the source this may not even resolve). In the
// destination it points at the factory-owned hooks directory; producer code run
// by the gate writes a pre-push through it, and that hook runs during git push
// with the credential helper active. Judged by the landing, not the source.
func TestCopyTreeRefusesASymlinkIntoDotGit(t *testing.T) {
	cases := map[string]string{
		"hooks":           ".git/hooks",
		"nested/pre-push": "../.git/hooks/pre-push",
		"deep/a/b/c":      "../../../.git/config",
		"dotdot-then-git": "sub/../.git/config",
	}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			l := newLab(t)
			if err := os.MkdirAll(filepath.Join(l.work, filepath.Dir(name)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(l.work, name)); err != nil {
				t.Fatal(err)
			}
			if err := gittransport.CopyTree(l.work, l.submit); err == nil {
				t.Fatalf("%s -> %s was copied; it would point into the submit repository's .git", name, target)
			}
			if _, err := os.Lstat(filepath.Join(l.submit, name)); err == nil {
				t.Fatalf("%s was planted in the submit tree", name)
			}
		})
	}
}

func TestCopyTreeRefusesASymlinkEscape(t *testing.T) {
	l := newLab(t)
	if err := os.Symlink("/etc/passwd", filepath.Join(l.work, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := gittransport.CopyTree(l.work, l.submit); err == nil {
		t.Fatal("a symlink resolving outside the producer tree was copied")
	}
	if _, err := os.Lstat(filepath.Join(l.submit, "escape")); err == nil {
		t.Fatal("the escaping symlink was planted in the submit tree")
	}
	// Control: a symlink within the tree is preserved.
	os.Remove(filepath.Join(l.work, "escape"))
	if err := os.WriteFile(filepath.Join(l.work, "real"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(l.work, "inside")); err != nil {
		t.Fatal(err)
	}
	if err := gittransport.CopyTree(l.work, l.submit); err != nil {
		t.Fatalf("an in-tree symlink was refused: %v", err)
	}
}

func TestCopyTreeRefusesOverlapOrANonClone(t *testing.T) {
	l := newLab(t)
	if err := gittransport.CopyTree(l.work, l.work); err == nil {
		t.Fatal("copying a tree onto itself was allowed")
	}
	if err := gittransport.CopyTree(l.work, l.root); err == nil {
		t.Fatal("copying into a directory that is not a clone was allowed")
	}
}

// ---------- end to end, no network ----------

func TestPushLandsOnTheRemote(t *testing.T) {
	l := newLab(t)
	if err := os.WriteFile(filepath.Join(l.work, "change.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := gittransport.CopyTree(l.work, l.submit); err != nil {
		t.Fatal(err)
	}
	git(t, l.submit, "checkout", "-q", "-b", "producer/fix")
	git(t, l.submit, "add", "-A")
	git(t, l.submit, "commit", "-q", "-m", "fix")
	if err := l.transport("tok").Push(context.Background(), "producer/fix"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(git(t, l.bare, "branch", "--list", "producer/fix"), "producer/fix") {
		t.Fatal("the branch did not land on the remote")
	}
}

// exec.Command resolves a bare "git" against the PARENT's PATH before the
// child's Env is consulted. A wrapper first on factoryd's inherited PATH would
// run and receive the producer's credential despite the constructed
// environment. The transport must resolve git from the declared PATH and run
// it by absolute path; the wrapper must never execute.
func TestGitIsResolvedFromTheDeclaredPathNotTheAmbientOne(t *testing.T) {
	l := newLab(t)
	trap := t.TempDir()
	marker := filepath.Join(trap, "wrapper-ran")
	wrapper := "#!/bin/sh\necho ran > " + marker + "\nexec /usr/bin/git \"$@\"\n"
	if err := os.WriteFile(filepath.Join(trap, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	// The wrapper is FIRST on the process's PATH...
	t.Setenv("PATH", trap+string(os.PathListSeparator)+os.Getenv("PATH"))
	// ...and absent from the declared one.
	l.cfg.Gate.Env["PATH"] = "/usr/bin:/bin"

	tr := l.transport("tok")
	if strings.HasPrefix(tr.Git, trap) {
		t.Fatalf("the transport resolved git to the ambient wrapper %s", tr.Git)
	}
	if !filepath.IsAbs(tr.Git) {
		t.Fatalf("git is not an absolute path: %q", tr.Git)
	}
	if err := tr.Guard(); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Identity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the ambient git wrapper ran during a transport operation")
	}

	// Control: with the trap ON the declared PATH, it is what runs -- proving
	// the test can detect the wrapper executing at all.
	l.cfg.Gate.Env["PATH"] = trap + string(os.PathListSeparator) + "/usr/bin:/bin"
	tr2 := l.transport("tok")
	if !strings.HasPrefix(tr2.Git, trap) {
		t.Fatalf("with the wrapper on the declared PATH the transport still chose %s", tr2.Git)
	}
	_ = tr2.Guard()
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the wrapper on the declared PATH did not run; the marker mechanism does not detect execution")
	}
}
