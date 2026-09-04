package refresh_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/refresh"
	"github.com/aicix-labs/factoryd/internal/state"
)

func cfgFor(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	return &config.Config{Name: "f", TargetBranch: "main", Paths: config.Paths{Root: root, ProducerWorkdir: filepath.Join(root, "work")}}
}

// Run is fetch, bundle, apply, and the apply is believed only when the
// workdir's HEAD is the bundle's tip.
func TestRunVerifiesTheWorkdirByShaNotByExit(t *testing.T) {
	cfg := cfgFor(t)
	var seq []string
	deps := refresh.Deps{
		Fetch: func(_ context.Context, spec string) error {
			seq = append(seq, "fetch "+spec)
			return nil
		},
		Bundle: func(_ context.Context, path, ref string) (string, error) {
			seq = append(seq, "bundle "+ref)
			if path != refresh.BundlePath(cfg) {
				t.Fatalf("bundle path %s", path)
			}
			return "abc123\n", nil
		},
		Apply: func(_ context.Context, bundle, branch string) (string, error) {
			seq = append(seq, "apply "+branch)
			return "abc123", nil
		},
	}
	r, err := refresh.Run(context.Background(), cfg, deps)
	if err != nil || r.SHA != "abc123" {
		t.Fatalf("r=%+v err=%v", r, err)
	}
	want := "fetch +refs/heads/main:refs/remotes/factoryd/main|bundle refs/remotes/factoryd/main|apply main"
	if got := strings.Join(seq, "|"); got != want {
		t.Fatalf("sequence %s", got)
	}
	if st, err := os.Stat(refresh.Dir(cfg)); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("refresh dir: %v %v; the producer must be able to read the bundle", st, err)
	}

	// The helper exits zero with the workdir somewhere else: refused.
	deps.Apply = func(context.Context, string, string) (string, error) { return "def456", nil }
	if _, err := refresh.Run(context.Background(), cfg, deps); err == nil || !strings.Contains(err.Error(), "not abc123") {
		t.Fatalf("a wrong HEAD after a clean apply was accepted: %v", err)
	}
	deps.Apply = func(context.Context, string, string) (string, error) { return "", errors.New("boom") }
	if _, err := refresh.Run(context.Background(), cfg, deps); err == nil {
		t.Fatal("an apply failure was swallowed")
	}
	deps.Bundle = func(context.Context, string, string) (string, error) { return "", nil }
	if _, err := refresh.Run(context.Background(), cfg, deps); err == nil {
		t.Fatal("a bundle naming no commit was accepted")
	}
}

// A change with no merged verdict is in flight; one that merged is not.
func TestInFlight(t *testing.T) {
	st := state.New("f")
	if busy, _ := refresh.InFlight(st); busy {
		t.Fatal("nothing submitted reads as in flight")
	}
	st.LastSubmit = &state.Submit{ChangeID: "48", Branch: "feat-abc", At: time.Now()}
	if busy, why := refresh.InFlight(st); !busy || !strings.Contains(why, "48") {
		t.Fatalf("an unverdicted submission is in flight: %v %q", busy, why)
	}
	st.LastVerdict = &state.Verdict{ChangeID: "48", Kind: state.VerdictChangesRequested}
	if busy, _ := refresh.InFlight(st); !busy {
		t.Fatal("changes requested is still in flight: the producer iterates on this tree")
	}
	st.LastVerdict = &state.Verdict{ChangeID: "47", Kind: state.VerdictMerged}
	if busy, _ := refresh.InFlight(st); !busy {
		t.Fatal("a merge of an OLDER change does not finish the current one")
	}
	st.LastVerdict = &state.Verdict{ChangeID: "48", Kind: state.VerdictMerged}
	if busy, _ := refresh.InFlight(st); busy {
		t.Fatal("a merged submission is not in flight")
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x", "GIT_CONFIG_NOSYSTEM=1", "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// The real thing, in the test's own identity: the #35 shape. The producer's
// clone is one merge behind with cycle 1's file untracked, a tracked file
// modified, and a stale intent file. After the refresh the tree is exactly
// the target commit and nothing else.
func TestApplyLocalMakesTheTreeExactlyTheTarget(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	upstream := t.TempDir()
	git(t, upstream, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(upstream, "README"), []byte("v1\n"), 0o644)
	git(t, upstream, "add", ".")
	git(t, upstream, "commit", "-qm", "v1")
	work := t.TempDir()
	git(t, work, "clone", "-q", upstream, ".")
	// Upstream moves on: cycle 1's script is merged.
	os.WriteFile(filepath.Join(upstream, "deploy.sh"), []byte("merged\n"), 0o755)
	os.WriteFile(filepath.Join(upstream, "README"), []byte("v2\n"), 0o644)
	git(t, upstream, "add", ".")
	git(t, upstream, "commit", "-qm", "v2")
	want := git(t, upstream, "rev-parse", "HEAD")
	// The producer's clone: old script untracked, README edited, stale intent.
	os.WriteFile(filepath.Join(work, "deploy.sh"), []byte("older, untracked\n"), 0o755)
	os.WriteFile(filepath.Join(work, "README"), []byte("edited\n"), 0o644)
	os.WriteFile(filepath.Join(work, ".producer-branch"), []byte("feat-stale\n"), 0o644)
	os.WriteFile(filepath.Join(work, "ignored.log"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(work, ".gitignore"), []byte("*.log\n"), 0o644)

	// factoryd's side: fetch into its own clone and bundle.
	submitRepo := t.TempDir()
	git(t, submitRepo, "clone", "-q", upstream, ".")
	git(t, submitRepo, "fetch", "-q", "origin", "+refs/heads/main:refs/remotes/factoryd/main")
	bundle := filepath.Join(t.TempDir(), "main.bundle")
	git(t, submitRepo, "bundle", "create", bundle, "refs/remotes/factoryd/main")

	got, err := refresh.ApplyLocal(context.Background(), "git", work, bundle, "main")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("HEAD %s, want %s", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "deploy.sh")); string(b) != "merged\n" {
		t.Fatalf("deploy.sh = %q: the untracked older copy survived", b)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "README")); string(b) != "v2\n" {
		t.Fatalf("README = %q", b)
	}
	for _, f := range []string{".producer-branch", "ignored.log", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(work, f)); !os.IsNotExist(err) {
			t.Fatalf("%s survived the refresh", f)
		}
	}
	if status := git(t, work, "status", "--porcelain"); status != "" {
		t.Fatalf("workdir not clean after refresh:\n%s", status)
	}
	if br := git(t, work, "rev-parse", "--abbrev-ref", "HEAD"); br != "main" {
		t.Fatalf("branch %s", br)
	}

	// No repository at all: one is made.
	empty := t.TempDir()
	if got, err := refresh.ApplyLocal(context.Background(), "git", empty, bundle, "main"); err != nil || got != want {
		t.Fatalf("fresh dir: %s %v", got, err)
	}
	if _, err := refresh.ApplyLocal(context.Background(), "git", empty, "relative.bundle", "main"); err == nil {
		t.Fatal("a relative bundle path was accepted")
	}
}
