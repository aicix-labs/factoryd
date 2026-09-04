package refresh

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ApplyLocal is the helper's work, done in the current identity: the
// process running it IS the producer (the _refresh verb, started as the
// producer by factoryd) or, in tests, the test. It fetches branch from the
// bundle into the repository at workdir -- initialising one if there is
// none -- and makes the tree exactly that commit: checked out, reset, and
// cleaned of everything untracked or ignored. It returns HEAD afterwards.
//
// The clean is total on purpose. The stale case in #35 was cycle 1's
// files left untracked; a stale .producer-branch would be worse -- a
// resubmission. Anything the producer wants to keep across cycles belongs
// under the cache root, not in the tree.
func ApplyLocal(ctx context.Context, git, workdir, bundle, branch string) (string, error) {
	if !filepath.IsAbs(bundle) {
		return "", fmt.Errorf("bundle path %q is not absolute", bundle)
	}
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, git, args...)
		cmd.Dir = workdir
		// The producer's own environment is the point: this runs as the
		// producer, in its repository. Only the variables git needs.
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "GIT_TERMINAL_PROMPT=0"}
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
		}
		return strings.TrimSpace(out.String()), nil
	}
	if _, err := os.Stat(filepath.Join(workdir, ".git")); err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}
		if _, err := run("init", "-q"); err != nil {
			return "", err
		}
	}
	ref := "refs/remotes/factoryd/" + branch
	if _, err := run("fetch", "-q", "--no-tags", bundle, "+"+ref+":"+ref); err != nil {
		return "", err
	}
	// HEAD is pointed at the branch first and the tree reset to the ref
	// second: checkout refuses when an untracked file is in the way, and an
	// untracked file in the way is the case (#35: cycle 1's script).
	if _, err := run("symbolic-ref", "HEAD", "refs/heads/"+branch); err != nil {
		return "", err
	}
	if _, err := run("reset", "-q", "--hard", ref); err != nil {
		return "", err
	}
	if _, err := run("clean", "-qfdx"); err != nil {
		return "", err
	}
	return run("rev-parse", "HEAD")
}
