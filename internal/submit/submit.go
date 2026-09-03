// Package submit is the only network step (SPEC.md §4.4): it materialises the
// producer's declared intent into the factoryd-owned repository, gates it, and
// -- only if the gate is green -- pushes and opens a Draft change.
//
// The producer never runs git and never touches the network. It writes plain
// files; submit, running outside the producer's sandbox as factoryd, does the
// rest. Every decision here has an exit code that means one thing, because the
// caller acts on the code: 0 submitted, 3 configuration or identity failure,
// 4 nothing to submit, 5 the gate went red. A red gate is the producer's
// problem; an exit 3 is the host's. Conflating them trains the operator to
// disbelieve red gates.
package submit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/gittransport"
	"github.com/aicix-labs/factoryd/internal/scm"
)

// Exit codes are the CLI contract.
const (
	ExitSubmitted = 0
	ExitConfig    = 3
	ExitNothing   = 4
	ExitGateRed   = 5
)

// Control files the producer writes. Both are excluded from the commit and
// deleted after use.
const (
	BranchFile    = ".producer-branch"
	CommitMsgFile = ".producer-commit-msg"
)

// Result is what submit did.
type Result struct {
	Exit   int
	Reason string
	// Change is set when a draft was opened or updated.
	Change *scm.Change
	// Branch is the branch that was pushed.
	Branch string
	// DirtyPaths is set on ExitNothing: files the producer changed without
	// declaring a submission (issue #12). The next turn's failure is then
	// predictable rather than surprising.
	DirtyPaths []string
}

// Error carries an exit code out of Run.
type Error struct {
	Exit   int
	Reason string
	Err    error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Reason, e.Err)
	}
	return e.Reason
}
func (e *Error) Unwrap() error { return e.Err }

func fail(exit int, format string, args ...any) error {
	return &Error{Exit: exit, Reason: fmt.Sprintf(format, args...)}
}

func wrap(exit int, err error, format string, args ...any) error {
	return &Error{Exit: exit, Reason: fmt.Sprintf(format, args...), Err: err}
}

// Transport is the git side submit needs. gittransport.Transport satisfies
// it; tests substitute a fake.
type Transport interface {
	Guard() error
	Identity(ctx context.Context) (gittransport.Identity, error)
	Fetch(ctx context.Context, refspec string) error
	Push(ctx context.Context, refspec string) error
}

// Git runs local git commands in the submit repository under the transport's
// environment. Kept separate from Transport so that tests can fake the
// repository operations without a real clone.
type Git interface {
	// Checkout creates branch at start (a ref such as FETCH_HEAD or
	// origin/main), replacing any existing branch of that name.
	Checkout(ctx context.Context, branch, start string) error
	// Commit stages everything and commits as the given author. It returns
	// the commit sha, or ok=false when there was nothing to commit.
	Commit(ctx context.Context, msg, authorName, authorEmail string) (sha string, ok bool, err error)
	// Status lists paths that differ from HEAD.
	Status(ctx context.Context) ([]string, error)
}

// GateRunner runs the gate command as the gate identity.
type GateRunner interface {
	Run(ctx context.Context, cfg *config.Config, exe string, env []string, out io.Writer) (exit int, err error)
}

// Provisioner prepares a declared gate path for the gate identity and proves
// it: created, owned by the gate, and writable BY the gate -- verified by a
// write attempt as that identity, immediately before the gate runs.
//
// CopyTree clears the work tree, so a relative declared path is recreated
// each submission. Created by MkdirAll alone it is factoryd's, root-owned,
// and the gate's first write fails -- which the gate reports as a non-zero
// exit, which submit would report as a red branch. A permission error on the
// host, presented as the producer's fault: exactly the misreport §9.6 forbids.
type Provisioner interface {
	Provision(ctx context.Context, path string) error
}

// Deps are everything Run touches in the world.
type Deps struct {
	Driver    scm.Driver
	Transport Transport
	Git       Git
	Gate      GateRunner
	Provision Provisioner
	// Reviewer resolves the reviewer's identity, for the distinctness check.
	Reviewer scm.Identity
	// Producer is the producer's API identity, already resolved.
	Producer scm.Identity
	// Now is injectable for deterministic timestamps.
	Now func() time.Time
	// Log receives progress. Nil discards.
	Log io.Writer
}

// Run performs a submission. Every early return carries an *Error with the
// exit code the CLI must use.
func Run(ctx context.Context, cfg *config.Config, deps Deps) (Result, error) {
	log := deps.Log
	if log == nil {
		log = io.Discard
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.Driver == nil || deps.Transport == nil || deps.Git == nil || deps.Gate == nil || deps.Provision == nil {
		return Result{}, fail(ExitConfig, "submit: missing a dependency")
	}

	// 1. Identity. Fail closed: the producer must be someone, and not the
	// reviewer. This runs before anything is read or written, because every
	// later step acts as the producer and must not act as anyone else.
	if deps.Producer.ID == "" {
		return Result{}, fail(ExitConfig, "the producer credential did not resolve to an identity")
	}
	if deps.Reviewer.ID != "" && deps.Producer.ID == deps.Reviewer.ID {
		return Result{}, fail(ExitConfig, "producer and reviewer both resolve to %s; the producer would review its own work", deps.Producer)
	}
	gitID, err := deps.Transport.Identity(ctx)
	if err != nil {
		return Result{}, wrap(ExitConfig, err, "git identity is undecided")
	}
	if gitID.Login != deps.Producer.Login {
		return Result{}, fail(ExitConfig, "git would push as %q but the producer's API identity is %q; the two mechanisms disagree", gitID.Login, deps.Producer.Login)
	}
	fmt.Fprintf(log, "identity: %s (git and API agree)\n", deps.Producer)

	// 2. Intent. The producer declares it in plain files; submit never guesses.
	// The directory the producer actually ran in. roles.producer.workdir may
	// override paths.producer_workdir; the supervisor and doctor honour it,
	// and reading the default here would report "nothing to submit" while
	// the declared work sat elsewhere.
	work := cfg.TurnWorkdir("producer")
	msg, hasWork, err := readControl(filepath.Join(work, CommitMsgFile))
	if err != nil {
		return Result{}, wrap(ExitConfig, err, "reading %s", CommitMsgFile)
	}
	if !hasWork {
		// Nothing declared. Say what is being left behind, so a dirty tree
		// does not become the next turn's surprise (issue #12).
		dirty, _ := dirtyPaths(work)
		r := Result{Exit: ExitNothing, Reason: "no commit message declared; nothing to submit", DirtyPaths: dirty}
		if len(dirty) > 0 {
			r.Reason = fmt.Sprintf("no commit message declared; leaving %d changed path(s) uncommitted in the producer workdir", len(dirty))
			fmt.Fprintf(log, "%s:\n", r.Reason)
			for _, p := range dirty {
				fmt.Fprintf(log, "  %s\n", p)
			}
		}
		return r, nil
	}
	branch, hasBranch, err := readControl(filepath.Join(work, BranchFile))
	if err != nil {
		return Result{}, wrap(ExitConfig, err, "reading %s", BranchFile)
	}
	if !hasBranch {
		return Result{}, fail(ExitConfig, "%s names no branch; a commit message without a branch is intent without a destination, and submit refuses to guess one", BranchFile)
	}
	if branch == cfg.TargetBranch {
		return Result{}, fail(ExitConfig, "%s names the target branch %q; the producer never writes to the target", BranchFile, branch)
	}
	if strings.ContainsAny(branch, " \t\n~^:?*[\\") || strings.HasPrefix(branch, "-") {
		return Result{}, fail(ExitConfig, "%s names an invalid branch %q", BranchFile, branch)
	}
	fmt.Fprintf(log, "intent: branch %s, message %q\n", branch, firstLine(msg))

	// 3. An existing change on this branch is validated BEFORE anything is
	// pushed to it. The lookup keys on the source branch; that alone says
	// nothing about whether the change is still ours to push to. If a
	// reviewer has marked it ready, a push would put new code into a change
	// that has left the producer's hands.
	existing, found, err := deps.Driver.FindOpenBySource(ctx, branch)
	if err != nil {
		return Result{}, wrap(ExitConfig, err, "looking for an open change on %s", branch)
	}
	if found {
		if err := changeIsOurs(existing, branch, cfg.TargetBranch, deps.Producer); err != nil {
			return Result{}, wrap(ExitConfig, err, "an open change already exists on %s and is not one this producer may update", branch)
		}
	}

	// 4. Materialise into the repository the producer cannot write.
	if err := deps.Transport.Guard(); err != nil {
		return Result{}, wrap(ExitConfig, err, "the submit repository failed its guard before any git ran")
	}
	if err := deps.Transport.Fetch(ctx, "+refs/heads/"+cfg.TargetBranch+":refs/remotes/factoryd/"+cfg.TargetBranch); err != nil {
		return Result{}, wrap(ExitConfig, err, "fetching %s", cfg.TargetBranch)
	}
	if err := deps.Git.Checkout(ctx, branch, "refs/remotes/factoryd/"+cfg.TargetBranch); err != nil {
		return Result{}, wrap(ExitConfig, err, "creating %s from %s", branch, cfg.TargetBranch)
	}
	if err := gittransport.CopyTree(work, cfg.Paths.SubmitRepo); err != nil {
		return Result{}, wrap(ExitConfig, err, "copying the producer's tree into the submit repository")
	}
	sha, committed, err := deps.Git.Commit(ctx, msg, deps.Producer.Login, authorEmail(deps.Producer, cfg))
	if err != nil {
		return Result{}, wrap(ExitConfig, err, "committing")
	}
	if !committed {
		return Result{Exit: ExitNothing, Reason: "the producer's tree is identical to " + cfg.TargetBranch + "; nothing to submit"}, nil
	}
	fmt.Fprintf(log, "materialised: %s at %s\n", branch, sha)

	// 5. The gate's paths, provisioned FOR THE GATE and proven writable BY the
	// gate immediately before it runs. CopyTree just cleared the work tree,
	// so a relative path no longer exists; created as factoryd's it would be
	// unwritable by the gate, and the gate's failure would be reported as a
	// red branch. A missing or unprovisionable path exits 3, not 5.
	for _, p := range cfg.Gate.RequiredWritablePaths {
		resolved, err := cfg.ResolveGatePath(p)
		if err != nil {
			return Result{}, wrap(ExitConfig, err, "gate path %s", p)
		}
		if err := deps.Provision.Provision(ctx, resolved); err != nil {
			return Result{}, wrap(ExitConfig, err, "gate path %s could not be provisioned for the gate", p)
		}
	}
	exe, err := config.LookPathIn(cfg.Gate.Env["PATH"], cfg.Gate.Command[0])
	if err != nil {
		return Result{}, wrap(ExitConfig, err, "gate command")
	}

	// 6. The gate, as the gate's own identity, in the constructed environment.
	env := cfg.GateEnv(map[string]string{
		"FACTORYD_FACTORY": cfg.Name, "FACTORYD_ROLE": "gate",
		"FACTORYD_BRANCH": branch, "FACTORYD_TARGET_BRANCH": cfg.TargetBranch,
		"FACTORYD_WORKDIR": cfg.Paths.SubmitRepo,
	})
	fmt.Fprintf(log, "gate: %s\n", strings.Join(cfg.Gate.Command, " "))
	gateExit, err := deps.Gate.Run(ctx, cfg, exe, env, log)
	if err != nil {
		// Could not run at all: the host, not the branch.
		return Result{}, wrap(ExitConfig, err, "the gate could not run")
	}
	if gateExit != 0 {
		// Red. Do not push. Tell the reviewer why, in a form the producer can
		// read without network access (§6.2), and signal.
		q := fmt.Sprintf("# gate red\n\nbranch: %s\ncommit: %s\nexit: %d\n\nThe gate (%s) exited %d. The change was not pushed.\n\nProposed: fix the failure and re-declare the same branch; submit will update the same draft.\n",
			branch, sha, gateExit, strings.Join(cfg.Gate.Command, " "), gateExit)
		if err := os.WriteFile(filepath.Join(cfg.InboxDir(), "question.md"), []byte(q), 0o644); err != nil {
			return Result{}, wrap(ExitConfig, err, "gate red, and the question could not be written")
		}
		if err := touch(filepath.Join(cfg.InboxDir(), "wake")); err != nil {
			// The reviewer would never learn of the question. A red gate
			// nobody is told about is the stall again, one step later.
			return Result{}, wrap(ExitConfig, err, "gate red, and the reviewer could not be signalled")
		}
		return Result{Exit: ExitGateRed, Reason: fmt.Sprintf("gate exited %d; not pushing a red branch", gateExit), Branch: branch}, nil
	}
	fmt.Fprintf(log, "gate: green\n")

	// 7. Push, through the transport's own guard, as the producer.
	if err := deps.Transport.Push(ctx, "+refs/heads/"+branch+":refs/heads/"+branch); err != nil {
		return Result{}, wrap(ExitConfig, err, "pushing %s", branch)
	}
	fmt.Fprintf(log, "pushed: %s\n", branch)

	// 8. Create-or-update the Draft, idempotent on the branch.
	var change scm.Change
	if found {
		// The window between the pre-push check and the push is real: a
		// reviewer can mark the change ready in it. Re-read it now. If it
		// changed underneath the push, the push has already landed on a
		// change that left the producer's hands -- unrecoverable here, so it
		// is reported as an infrastructure failure, loudly, not as success.
		now, err := deps.Driver.Get(ctx, existing.ID)
		if err != nil {
			return Result{}, wrap(ExitConfig, err, "re-reading %s after the push", existing.ID)
		}
		if err := changeIsOurs(now, branch, cfg.TargetBranch, deps.Producer); err != nil {
			return Result{}, wrap(ExitConfig, err, "change %s changed state between the check and the push; the push landed on it and a human must look", existing.ID)
		}
		change = now
		fmt.Fprintf(log, "draft: updated %s\n", change.ID)
	} else {
		change, err = deps.Driver.OpenDraft(ctx, scm.DraftSpec{
			SourceBranch: branch, TargetBranch: cfg.TargetBranch,
			Title: firstLine(msg), Body: draftBody(msg, sha, deps.Now()),
		})
		if err != nil {
			return Result{}, wrap(ExitConfig, err, "opening the draft for %s", branch)
		}
		// The provider's response is validated in full, not just its draft
		// flag: the change it opened must be the one that was asked for.
		if err := changeIsOurs(change, branch, cfg.TargetBranch, deps.Producer); err != nil {
			return Result{}, wrap(ExitConfig, err, "the provider opened %s but it is not the change that was requested", change.ID)
		}
		fmt.Fprintf(log, "draft: opened %s\n", change.ID)
	}

	// 9. Signal the reviewer and retire the control files.
	if err := touch(filepath.Join(cfg.InboxDir(), "wake")); err != nil {
		return Result{}, wrap(ExitConfig, err, "signalling the reviewer")
	}
	for _, f := range []string{BranchFile, CommitMsgFile} {
		_ = os.Remove(filepath.Join(work, f))
	}
	return Result{Exit: ExitSubmitted, Reason: "submitted", Change: &change, Branch: branch}, nil
}

// readControl reads a control file. Absent or blank means "not declared".
func readControl(path string) (string, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	s := strings.TrimSpace(string(raw))
	return s, s != "", nil
}

// dirtyPaths lists files in the producer workdir other than control files and
// git control data. The producer's tree is the whole truth of what changed;
// there is no git to ask.
func dirtyPaths(work string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(work, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(work, p)
		if rel == "." {
			return nil
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || rel == BranchFile || rel == CommitMsgFile {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	sort.Strings(out)
	return out, err
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func draftBody(msg, sha string, at time.Time) string {
	return fmt.Sprintf("%s\n\n---\nopened by factoryd submit at %s\ncommit %s\n", msg, at.Format(time.RFC3339), sha)
}

func authorEmail(id scm.Identity, cfg *config.Config) string {
	host, err := cfg.ProviderGitHost()
	if err != nil || host == "" {
		host = "factoryd.invalid"
	}
	return fmt.Sprintf("%s@users.noreply.%s", id.Login, host)
}

func touch(path string) error {
	return os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
}

// changeIsOurs is what makes an existing or newly opened change safe to push
// to: open, still a draft, on this branch, into the configured target, and
// authored by this producer. Any one of these failing means the change is not
// the producer's to update.
func changeIsOurs(c scm.Change, branch, target string, producer scm.Identity) error {
	switch {
	case c.State != scm.StateOpen:
		return fmt.Errorf("change %s is %v, not open", c.ID, c.State)
	case !c.Draft:
		return fmt.Errorf("change %s is no longer a draft; a reviewer has marked it ready and it has left the producer's hands", c.ID)
	case c.SourceBranch != branch:
		return fmt.Errorf("change %s is from %q, not %q", c.ID, c.SourceBranch, branch)
	case c.TargetBranch != target:
		return fmt.Errorf("change %s targets %q, not the configured %q", c.ID, c.TargetBranch, target)
	case c.Author != "" && producer.Login != "" && c.Author != producer.Login:
		return fmt.Errorf("change %s was opened by %q, not by this producer %q", c.ID, c.Author, producer.Login)
	}
	return nil
}
