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
// producer by factoryd under the producer's sandbox) or, in tests, the
// test. It replaces the repository at workdir with a fresh one, fetches
// branch from the bundle, and makes the tree exactly that commit: checked
// out, reset, and cleaned of everything untracked or ignored. It returns
// HEAD afterwards.
//
// No producer-controlled git configuration runs here. The old .git is
// removed before anything is executed -- its config, hooks, filters and
// fsmonitor with it -- and git is run with the system and global config
// files disabled, so the producer's HOME cannot supply them either. A
// smudge filter or a hook in a producer-writable repository is a command
// of the producer's choosing, and reset --hard would run it (#41 review).
// The bundle is factoryd's, so what is fetched carries no config; tracked
// .gitattributes can name a filter, but with no config there is no command
// behind the name. Local branches and reflog go with the old .git: a
// refresh is the start of a cycle, not a continuation.
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
		cmd.Env = GitEnv()
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
		}
		return strings.TrimSpace(out.String()), nil
	}
	if err := os.RemoveAll(filepath.Join(workdir, ".git")); err != nil {
		return "", fmt.Errorf("removing the old repository: %w", err)
	}
	if _, err := run("init", "-q", "--template=", "-b", branch); err != nil {
		return "", err
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

// GitEnv is the environment the helper gives git: PATH and HOME for the
// binary and its temp files, and every configuration file outside the
// fresh repository disabled. It is exported so the test that proves a
// producer-supplied filter does not run can assert the same environment.
func GitEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	}
}
