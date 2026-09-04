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
	"syscall"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/gittransport"
	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/state"
	"github.com/aicix-labs/factoryd/internal/supervise"
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
	// Supersedes lists the earlier drafts in the family that the new draft
	// replaces. They are left open: closing one would be a write to a change
	// that may have left the producer's hands since it was last read, and no
	// provider offers a close conditional on that. An operator retires them.
	Supersedes []scm.ChangeID
	// DirtyPaths is set on ExitNothing: files the producer changed without
	// declaring a submission (issue #12). The next turn's failure is then
	// predictable rather than surprising.
	DirtyPaths []string
}

// Error carries an exit code and a disposition out of Run. The disposition
// is what the supervisor acts on (#42): blocked cannot be changed by a
// retry, transient is safe to replay, unknown must not be replayed
// automatically. It is set per site, deliberately, and defaults to
// unknown: a refusal that did not say it is safe to repeat is not.
type Error struct {
	Exit   int
	Reason string
	Err    error
	Kind   supervise.Disposition
}

// Disposition implements supervise.Disposer.
func (e *Error) Disposition() supervise.Disposition {
	if e.Kind == "" {
		return supervise.DispositionUnknown
	}
	return e.Kind
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

// blocked: no retry can change this answer. Invalid declarations, policy
// and boundary refusals, and a change that left the producer's hands.
func blocked(err error) error {
	if e, ok := err.(*Error); ok {
		e.Kind = supervise.DispositionBlocked
	}
	return err
}

// transient: safe to replay, and argued so at each site: a fetch, a
// non-force push of a content-derived branch, local git over the same
// tree, the gate over the same tree, a provider read, a state record
// before any external effect.
func transient(err error) error {
	if e, ok := err.(*Error); ok {
		e.Kind = supervise.DispositionTransient
	}
	return err
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
	// Tree returns the sha of HEAD's tree: the content, independent of who
	// committed it or when. It names the immutable branch.
	Tree(ctx context.Context) (string, error)
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
	// GateCanTraverse reports whether the GATE identity can pass through
	// dir, probed now. The producer owns its home and can widen it after
	// doctor looked; the gate runs producer-authored code later. Only a
	// probe at this crossing, after the producer has quiesced, binds the
	// state doctor saw to the state the gate gets.
	GateCanTraverse(ctx context.Context, dir string) (bool, error)
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
	// Wake signals the reviewer. Nil touches inbox/wake. Injectable so a
	// test can interleave the reviewer's merge with the wake itself.
	Wake func() error
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
		return Result{}, blocked(fail(ExitConfig, "producer and reviewer both resolve to %s; the producer would review its own work", deps.Producer))
	}
	gitID, err := deps.Transport.Identity(ctx)
	if err != nil {
		return Result{}, wrap(ExitConfig, err, "git identity is undecided")
	}
	if gitID.Login != deps.Producer.Login {
		return Result{}, blocked(fail(ExitConfig, "git would push as %q but the producer's API identity is %q; the two mechanisms disagree", gitID.Login, deps.Producer.Login))
	}
	fmt.Fprintf(log, "identity: %s (git and API agree)\n", deps.Producer)

	// 2. Intent. The producer declares it in plain files; submit never guesses.
	// The directory the producer actually ran in. roles.producer.workdir may
	// override paths.producer_workdir; the supervisor and doctor honour it,
	// and reading the default here would report "nothing to submit" while
	// the declared work sat elsewhere.
	work := cfg.TurnWorkdir("producer")
	intent, err := ReadIntent(work)
	if err != nil && !errors.Is(err, ErrNoIntent) {
		return Result{}, blocked(wrap(ExitConfig, err, "reading the producer's intent"))
	}
	msg, branch := intent.Message, intent.Branch
	if errors.Is(err, ErrNoIntent) {
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
		if err := RecordNoWork(cfg, deps.Now()); err != nil {
			return Result{}, transient(wrap(ExitConfig, err, "recording the clean cycle"))
		}
		return r, nil
	}
	if branch == cfg.TargetBranch {
		return Result{}, blocked(fail(ExitConfig, "%s names the target branch %q; the producer never writes to the target", BranchFile, branch))
	}
	if strings.ContainsAny(branch, " \t\n~^:?*[\\") || strings.HasPrefix(branch, "-") {
		return Result{}, blocked(fail(ExitConfig, "%s names an invalid branch %q", BranchFile, branch))
	}
	fmt.Fprintf(log, "intent: branch %s, message %q\n", branch, firstLine(msg))

	// 3. Materialise into the repository the producer cannot write. The
	// branch that is pushed is derived from the CONTENT (declared name plus
	// commit sha), which is what closes the check-to-push race: a push never
	// modifies a branch any existing change references, because a different
	// tree is a different branch, and the same tree is the same sha and a
	// no-op. No lease is needed from a provider that offers none. The
	// declared name is the family; each submission is an immutable member.
	if err := deps.Transport.Guard(); err != nil {
		return Result{}, blocked(wrap(ExitConfig, err, "the submit repository failed its guard before any git ran"))
	}
	if err := deps.Transport.Fetch(ctx, "+refs/heads/"+cfg.TargetBranch+":refs/remotes/factoryd/"+cfg.TargetBranch); err != nil {
		return Result{}, transient(wrap(ExitConfig, err, "fetching %s", cfg.TargetBranch))
	}
	declared := branch
	if err := deps.Git.Checkout(ctx, declared, "refs/remotes/factoryd/"+cfg.TargetBranch); err != nil {
		return Result{}, transient(wrap(ExitConfig, err, "creating %s from %s", declared, cfg.TargetBranch))
	}
	if err := gittransport.CopyTree(work, cfg.Paths.SubmitRepo); err != nil {
		return Result{}, transient(wrap(ExitConfig, err, "copying the producer's tree into the submit repository"))
	}
	sha, committed, err := deps.Git.Commit(ctx, msg, deps.Producer.Login, authorEmail(deps.Producer, cfg))
	if err != nil {
		return Result{}, transient(wrap(ExitConfig, err, "committing"))
	}
	if !committed {
		if err := RecordNoWork(cfg, deps.Now()); err != nil {
			return Result{}, transient(wrap(ExitConfig, err, "recording the clean cycle"))
		}
		return Result{Exit: ExitNothing, Reason: "the producer's tree is identical to " + cfg.TargetBranch + "; nothing to submit"}, nil
	}
	tree, err := deps.Git.Tree(ctx)
	if err != nil {
		return Result{}, transient(wrap(ExitConfig, err, "reading the tree of %s", sha))
	}
	branch = ImmutableBranch(declared, tree)
	if err := deps.Git.Checkout(ctx, branch, "HEAD"); err != nil {
		return Result{}, transient(wrap(ExitConfig, err, "naming the immutable branch %s", branch))
	}
	fmt.Fprintf(log, "materialised: %s at %s (tree %s)\n", branch, sha, tree)

	// 4. Prior drafts in this family are located now and validated as ours.
	// One on THIS branch is the same tree already submitted: there is
	// nothing new to gate or push, and the answer is "already submitted".
	family, err := openInFamily(ctx, deps.Driver, declared)
	if err != nil {
		return Result{}, transient(wrap(ExitConfig, err, "looking for open changes on %s", declared))
	}
	for _, c := range family {
		// Every member must be ours. A ready or foreign change in the family
		// stops the submission before the gate spends anything.
		if err := changeIsOurs(c, c.SourceBranch, cfg.TargetBranch, deps.Producer); err != nil {
			return Result{}, blocked(wrap(ExitConfig, err, "an open change exists on %s and is not one this producer may update", declared))
		}
		if c.SourceBranch == branch {
			existing := c
			// The cycle names the draft this tree already is, so its
			// merge finishes the cycle even if the record was lost.
			if err := recordOpen(cfg, deps.Now(), declared, branch, string(existing.ID)); err != nil {
				return Result{}, wrap(ExitConfig, err, "recording the open draft in state")
			}
			return Result{Exit: ExitNothing, Change: &existing, Branch: branch,
				Reason: fmt.Sprintf("this tree is already submitted as %s on %s; nothing new to submit", existing.ID, branch)}, nil
		}
	}

	// 5. The gate's paths, provisioned FOR THE GATE and proven writable BY the
	// gate immediately before it runs. CopyTree just cleared the work tree,
	// so a relative path no longer exists; created as factoryd's it would be
	// unwritable by the gate, and the gate's failure would be reported as a
	// red branch. A missing or unprovisionable path exits 3, not 5.
	// 5a. The producer's home, re-proved at the crossing. doctor is an
	// operator command that saw a moment; the producer owns its home and
	// can reopen it during its turn; the gate runs producer-authored code
	// after. So the gate identity is asked NOW, with the producer quiesced,
	// whether it can traverse that home -- and a yes is a boundary failure
	// (exit 3), not a red branch: nothing the producer wrote is judged.
	// The home comes through one helper that refuses a relative value: to
	// the turn a relative HOME is under the producer workdir; to this
	// process it would be under its own working directory, and a probe
	// green about the wrong directory is worse than none.
	home, herr := cfg.ProducerHome()
	if herr != nil {
		return Result{}, wrap(ExitConfig, herr, "producer home")
	}
	if home != "" {
		can, err := deps.Provision.GateCanTraverse(ctx, home)
		if err != nil {
			return Result{}, blocked(wrap(ExitConfig, err, "probing whether the gate can traverse the producer's home %s", home))
		}
		if can {
			return Result{}, blocked(fail(ExitConfig, "the gate identity can traverse the producer's home %s; the producer reopened it after doctor, and producer-authored gate code would reach its model credential -- not running the gate", home))
		}
		fmt.Fprintf(log, "boundary: the gate cannot traverse %s\n", home)
	}

	// 5b. No gate path may overlap the producer's home, judged PHYSICALLY:
	// a declared path is chowned to the gate by this privileged process,
	// and one that reaches into the home -- a symlink planted under a
	// declared path, say -- would grant the gate exactly the traversal
	// just refused, after the probe and before the gate. Judged before any
	// ownership changes; Validate's lexical check is the first lock.
	if home != "" {
		physHome := gittransport.PhysicalPrefix(home)
		for _, p := range cfg.Gate.RequiredWritablePaths {
			resolved, err := cfg.ResolveGatePath(p)
			if err != nil {
				return Result{}, wrap(ExitConfig, err, "gate path %s", p)
			}
			if phys := gittransport.PhysicalPrefix(resolved); config.PathsOverlap(phys, physHome) {
				return Result{}, blocked(fail(ExitConfig, "gate path %q resolves to %s, which overlaps the producer's home %s; provisioning it would hand the gate the producer's model credential -- not provisioning, not running the gate", p, phys, physHome))
			}
		}
	}
	for _, p := range cfg.Gate.RequiredWritablePaths {
		resolved, err := cfg.ResolveGatePath(p)
		if err != nil {
			return Result{}, wrap(ExitConfig, err, "gate path %s", p)
		}
		if err := deps.Provision.Provision(ctx, resolved); err != nil {
			return Result{}, wrap(ExitConfig, err, "gate path %s could not be provisioned for the gate", p)
		}
	}
	// 5c. Defense in depth: provisioning changed ownership under the gate;
	// the boundary is asked again after it, so a path that reached the home
	// by a route neither lock saw still refuses the gate.
	if home != "" {
		can, err := deps.Provision.GateCanTraverse(ctx, home)
		if err != nil {
			return Result{}, wrap(ExitConfig, err, "re-probing the producer's home %s after provisioning", home)
		}
		if can {
			return Result{}, blocked(fail(ExitConfig, "the gate identity can traverse the producer's home %s AFTER provisioning the gate paths; a declared path reached the home -- not running the gate", home))
		}
	}
	exe, err := config.LookPathIn(cfg.Gate.Env["PATH"], cfg.Gate.Command[0])
	if err != nil {
		return Result{}, transient(wrap(ExitConfig, err, "gate command"))
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
		return Result{}, transient(wrap(ExitConfig, err, "the gate could not run"))
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

	// 7. Immediately before the push -- after the gate, which may have run
	// for a long time -- every open change in the family is re-read. One that
	// has left the producer's hands since the earlier look (a reviewer marked
	// it ready during the gate) stops the submission here, before any push:
	// the producer's newer work waits for the reviewer's decision on the
	// change they took. This is the check the immutable branch makes
	// sufficient: even a flip in the instant after it cannot be pushed INTO,
	// because the push below targets a branch no change references.
	family, err = openInFamily(ctx, deps.Driver, declared)
	if err != nil {
		return Result{}, transient(wrap(ExitConfig, err, "re-reading open changes on %s before the push", declared))
	}
	for _, c := range family {
		if err := changeIsOurs(c, c.SourceBranch, cfg.TargetBranch, deps.Producer); err != nil {
			return Result{}, blocked(wrap(ExitConfig, err, "change %s left the producer's hands while the gate ran; not pushing", c.ID))
		}
	}

	// Write-ahead: the cycle says "submitting" BEFORE the push, so a crash
	// between the draft's creation and the record of it leaves a phase that
	// forbids a refresh, never an absence that permits one (#35 review).
	if _, err := state.Update(cfg.StatePath(), cfg.Name, func(st *state.State) error {
		c := st.SetCycle(state.CycleSubmitting, deps.Now())
		c.Family, c.Digest = declared, branch
		return nil
	}); err != nil {
		return Result{}, transient(wrap(ExitConfig, err, "recording the submission before the push"))
	}

	// Non-force: a branch that somehow already exists with different content
	// is rejected by git itself. The push cannot modify anything.
	if err := deps.Transport.Push(ctx, "refs/heads/"+branch+":refs/heads/"+branch); err != nil {
		return Result{}, transient(wrap(ExitConfig, err, "pushing %s", branch))
	}
	fmt.Fprintf(log, "pushed: %s\n", branch)

	// 8. Open the Draft. The branch is new by construction (a same-tree
	// draft returned "already submitted" before the gate), so this is
	// always a create, never an update of a change something else may hold.
	var supersedes []scm.ChangeID
	for _, c := range family {
		supersedes = append(supersedes, c.ID)
	}
	change, err := deps.Driver.OpenDraft(ctx, scm.DraftSpec{
		SourceBranch: branch, TargetBranch: cfg.TargetBranch,
		Title: firstLine(msg), Body: draftBody(msg, sha, deps.Now(), supersedes),
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

	// Earlier drafts in the family are superseded, and REPORTED as such:
	// named in the new draft's body, here, and in the result. They are not
	// closed. Submit never writes to an existing change: any read of one is
	// stale by the time the write lands, and neither provider offers a
	// close conditional on the state that was read. A draft a reviewer
	// took in that window would be closed under them. Neither does the
	// automated reviewer, for the same reason; the OPERATOR retires what is
	// superseded, as the operator principal, having read both (#36).
	for _, id := range supersedes {
		fmt.Fprintf(log, "draft: supersedes %s (left open; an operator retires it with factoryd scm close)\n", id)
	}

	// 9. Record the open draft, THEN signal the reviewer, then retire the
	// control files. The order matters: a reviewer woken before the record
	// could merge and signal while the cycle still said submitting, and the
	// record written afterwards would bury that verdict under "open" with
	// no later signal to finish it (#41 review). recordOpen also converges
	// the other way round: a merged verdict already on file for this
	// change finishes the cycle instead of opening it.
	if err := recordOpen(cfg, deps.Now(), declared, branch, string(change.ID)); err != nil {
		return Result{}, wrap(ExitConfig, err, "recording the open draft in state")
	}
	wake := deps.Wake
	if wake == nil {
		wake = func() error { return touch(filepath.Join(cfg.InboxDir(), "wake")) }
	}
	if err := wake(); err != nil {
		return Result{}, wrap(ExitConfig, err, "signalling the reviewer")
	}
	for _, f := range []string{BranchFile, CommitMsgFile} {
		_ = os.Remove(filepath.Join(work, f))
	}
	return Result{Exit: ExitSubmitted, Reason: "submitted", Change: &change, Branch: branch, Supersedes: supersedes}, nil
}

// AfterTurn is the SUBMIT step of the §3 loop as the producer supervisor
// runs it: after a producer turn that declared intent, submit runs outside
// the sandbox, as factoryd. A turn that declared nothing is a clean cycle
// (exit 4 is not a failure) -- recorded here, without building submit's
// dependencies, because the ordinary no-op turn must not leave the cycle
// "working" for good (#41 review); a red gate has already written its
// question and woken the reviewer (exit 5, not a failure either); only a
// configuration or identity failure (3) is the supervisor's to count.
func AfterTurn(cfg *config.Config, mkDeps func(ctx context.Context) (Deps, error)) func(ctx context.Context, t supervise.Turn, res supervise.TurnResult) (string, error) {
	return func(ctx context.Context, t supervise.Turn, res supervise.TurnResult) (string, error) {
		// A standing block is consulted first (#45 review). While one stands
		// -- unknown, with the intent kept live for reconciliation, or
		// blocked -- the automatic step does nothing: no read of the intent
		// as a submission, no deps, no gate, no push. Only the operator's
		// explicit `factoryd submit`, after fixing the cause, uses that
		// intent again, and its success is what clears the block. Without
		// this, a later brief whose agent declared nothing new would submit
		// the stale intent -- the automatic replay unknown exists to prevent.
		st, err := state.Load(cfg.StatePath(), cfg.Name)
		if err != nil {
			return "", fmt.Errorf("submit: %w", err)
		}
		if b := st.Role(state.RoleProducer).Blocked; b != nil {
			return fmt.Sprintf("a %s submission stands since turn %s (%s); nothing submitted until the operator reconciles it with factoryd submit", b.Disposition, b.Turn, firstLine(b.Reason)), nil
		}
		// The same reader submit uses, so a declaration submit would refuse --
		// one file without the other, a link, a fifo -- is a blocked
		// submission here too: recorded, quarantined, never "no intent" and
		// a consumed trigger over stranded work, and never a plain error
		// the supervisor suppresses in silence (#45 review).
		if _, err := ReadIntent(cfg.TurnWorkdir("producer")); err != nil {
			if errors.Is(err, ErrNoIntent) {
				if err := RecordNoWork(cfg, time.Now()); err != nil {
					return "", fmt.Errorf("submit: recording the clean cycle: %w", err)
				}
				return "no intent declared; nothing to submit", nil
			}
			return "", recordBlock(cfg, t, blocked(wrap(ExitConfig, err, "submit: the producer's intent is not a valid declaration")))
		}
		deps, err := mkDeps(ctx)
		if err != nil {
			// Not classified: a credential that did not resolve, an identity
			// the provider would not confirm. Unknown, and recorded as such.
			return "", recordBlock(cfg, t, fmt.Errorf("submit: %w", err))
		}
		r, err := Run(ctx, cfg, deps)
		if err != nil {
			return "", recordBlock(cfg, t, fmt.Errorf("submit: %w", err))
		}
		switch r.Exit {
		case ExitSubmitted:
			return fmt.Sprintf("submitted %s as draft %s", r.Branch, r.Change.ID), nil
		case ExitGateRed:
			return "gate red: " + r.Reason, nil
		default:
			return r.Reason, nil
		}
	}
}

// recordBlock is the after-turn step's failure path (#42). A transient
// failure is returned as it is: the supervisor re-arms a retry of this
// step. A blocked or unknown one is recorded in state -- durable,
// operator-visible, cleared only by a later successful submission -- and,
// for blocked, the declaration files are moved aside so the next turn
// cannot resubmit the same refused intent; the producer's source work is
// not touched. The error is returned with its disposition intact.
func recordBlock(cfg *config.Config, t supervise.Turn, err error) error {
	d := supervise.DispositionOf(err)
	if d == supervise.DispositionTransient {
		return err
	}
	b := &state.Block{Disposition: string(d), Reason: err.Error(), Turn: t.ID, At: time.Now()}
	var se *Error
	if errors.As(err, &se) {
		if intent, ierr := ReadIntent(cfg.TurnWorkdir("producer")); ierr == nil {
			b.Family = intent.Branch
		}
	}
	if d == supervise.DispositionBlocked {
		b.Quarantined = quarantineIntent(cfg.TurnWorkdir("producer"), t.ID)
	}
	if _, uerr := state.Update(cfg.StatePath(), cfg.Name, func(st *state.State) error {
		if c := st.Cycle; c != nil {
			b.Digest = c.Digest
			if b.Family == "" {
				b.Family = c.Family
			}
		}
		st.Role(state.RoleProducer).Blocked = b
		return nil
	}); uerr != nil {
		return fmt.Errorf("%w; and the block could not be recorded: %v", err, uerr)
	}
	return err
}

// quarantineIntent moves the declaration files aside, named by the turn
// that was refused. Source work is not touched.
func quarantineIntent(work, turnID string) []string {
	var moved []string
	for _, f := range []string{BranchFile, CommitMsgFile} {
		src := filepath.Join(work, f)
		if _, err := os.Lstat(src); err != nil {
			continue
		}
		dst := src + ".blocked-" + turnID
		if err := os.Rename(src, dst); err == nil {
			moved = append(moved, filepath.Base(dst))
		}
	}
	return moved
}

// recordOpen names the open draft in the cycle. Supersession moves the id
// to the newest member of the family; the merge of THAT id finishes the
// cycle (#35). If the merged verdict for this very change is already on
// file -- the reviewer was faster than the record -- the cycle is finished
// here, so either ordering converges.
func recordOpen(cfg *config.Config, now time.Time, declared, branch, changeID string) error {
	_, err := state.Update(cfg.StatePath(), cfg.Name, func(st *state.State) error {
		phase := state.CycleOpen
		if v := st.LastVerdict; v != nil && v.Kind == state.VerdictMerged && v.ChangeID == changeID {
			phase = state.CycleFinished
		}
		c := st.SetCycle(phase, now)
		c.Family, c.Digest, c.ChangeID = declared, branch, changeID
		// A submission that succeeded is the only thing that clears a block.
		st.Role(state.RoleProducer).Blocked = nil
		return nil
	})
	return err
}

// RecordNoWork marks the cycle clean: the turn declared nothing, or a tree
// identical to the target. Shared by submit and the supervisor's after-turn
// step, which short-circuits the no-intent case without building submit's
// dependencies. A cycle that is already open, finished or unknown is left
// as it is: "nothing new this turn" says nothing about a draft in flight.
func RecordNoWork(cfg *config.Config, now time.Time) error {
	_, err := state.Update(cfg.StatePath(), cfg.Name, func(st *state.State) error {
		if c := st.Cycle; c != nil && (c.Phase == state.CycleNew || c.Phase == state.CycleWorking) {
			st.SetCycle(state.CycleClean, now)
		}
		return nil
	})
	return err
}

// readControl reads a control file. Absent or blank means "not declared".
// ErrNoIntent is returned by ReadIntent when neither control file exists:
// the turn declared nothing, which is an ordinary outcome.
var ErrNoIntent = errors.New("no intent declared: neither control file exists")

// Intent is what the producer declared.
type Intent struct {
	Branch  string
	Message string
}

// ReadIntent is THE reader of the producer's intent, shared by submit and by
// the supervisor's after-turn step so the two cannot disagree. Both control
// files are read no-follow on the workdir's descriptor and must be regular,
// single-link files. Neither present is ErrNoIntent. Anything else that is
// not a complete, valid declaration is an error: one file without the
// other, an empty file, a symlink (dangling or not), a fifo, a hard link. A
// supervisor that read "no intent" for any of those would consume the
// trigger and strand the work; direct submit refuses them, and so must it.
func ReadIntent(work string) (Intent, error) {
	msg, hasMsg, merr := readControl(work, CommitMsgFile)
	branch, hasBranch, berr := readControl(work, BranchFile)
	if merr != nil {
		return Intent{}, fmt.Errorf("%s: %w", CommitMsgFile, merr)
	}
	if berr != nil {
		return Intent{}, fmt.Errorf("%s: %w", BranchFile, berr)
	}
	switch {
	case !hasMsg && !hasBranch:
		return Intent{}, ErrNoIntent
	case hasMsg && !hasBranch:
		return Intent{}, fmt.Errorf("%s is declared but %s names no branch; a commit message without a branch is intent without a destination, and submit refuses to guess one", CommitMsgFile, BranchFile)
	case hasBranch && !hasMsg:
		return Intent{}, fmt.Errorf("%s names %q but %s is absent or empty; a branch without a message is not a declaration", BranchFile, branch, CommitMsgFile)
	}
	return Intent{Branch: branch, Message: msg}, nil
}

// readControl reads a control file the producer wrote, as root, without
// following anything the producer controls: the file is opened through a
// handle on the workdir with O_NOFOLLOW on the final component, and must be
// a regular file on the descriptor that was actually opened. There is no
// separate stat: a check by path and a read by path are two looks at a
// thing the producer can change between them. A control file that is a
// symlink -- to a credential, say -- is refused, and its target is never
// read, so nothing of it can reach a commit message, a draft body, or a log.
func readControl(work, name string) (string, bool, error) {
	dir, err := os.OpenFile(work, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return "", false, err
	}
	defer dir.Close()
	f, err := gittransport.OpenNoFollow(dir, name, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return "", false, fmt.Errorf("%s is a symlink; a control file must be a regular file the producer wrote, not a pointer to something else", name)
		}
		return "", false, err
	}
	defer f.Close()
	fi, err := gittransport.RequireOwnRegular(f, name)
	if err != nil {
		return "", false, fmt.Errorf("%w; refusing to read it", err)
	}
	if fi.Size() > 64*1024 {
		return "", false, fmt.Errorf("%s is %d bytes; a control file is a branch name or a commit message", name, fi.Size())
	}
	raw, err := io.ReadAll(f)
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

func draftBody(msg, sha string, at time.Time, supersedes []scm.ChangeID) string {
	b := fmt.Sprintf("%s\n\n---\nopened by factoryd submit at %s\ncommit %s\n", msg, at.Format(time.RFC3339), sha)
	for _, id := range supersedes {
		b += fmt.Sprintf("supersedes %s (left open; an operator retires it with `factoryd scm close`, never the automated reviewer)\n", id)
	}
	return b
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
	case producer.Login == "":
		return fmt.Errorf("the producer has no login; ownership of change %s cannot be established", c.ID)
	case c.Author == "":
		// Both drivers map an omitted author to "". An incomplete response
		// must not satisfy an ownership check: an unknown owner is not ours.
		return fmt.Errorf("change %s reports no author; ownership is unknown, which is not the same as ours", c.ID)
	case c.Author != producer.Login:
		return fmt.Errorf("change %s was opened by %q, not by this producer %q", c.ID, c.Author, producer.Login)
	}
	return nil
}

// ImmutableBranch names the branch a submission is pushed to: the declared
// family name plus the TREE it carries. Different content is a different
// branch; identical content is the same branch however often, and by
// whomever, it is committed -- which a commit sha, carrying timestamps,
// would not give.
func ImmutableBranch(declared, tree string) string {
	n := 10
	if len(tree) < n {
		n = len(tree)
	}
	return declared + "-" + tree[:n]
}

// DeclaredFamily is the inverse of ImmutableBranch: the declared name
// recovered from a pushed branch. One implementation, in state, so the
// verdict's own lineage check and submit cannot disagree.
func DeclaredFamily(branch string) string { return state.FamilyOf(branch) }

// openInFamily lists the open changes whose source branch belongs to the
// declared family: the family name itself, or the family name followed by the
// immutable suffix. Author is not filtered here; ownership is judged per
// change by changeIsOurs, so that a foreign change in the family refuses
// rather than being silently ignored.
func openInFamily(ctx context.Context, d scm.Driver, declared string) ([]scm.Change, error) {
	all, err := d.ListOpen(ctx)
	if err != nil {
		return nil, err
	}
	var out []scm.Change
	for _, c := range all {
		if c.SourceBranch == declared || strings.HasPrefix(c.SourceBranch, declared+"-") {
			out = append(out, c)
		}
	}
	return out, nil
}
