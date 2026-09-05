package submit_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/signal"
	"github.com/aicix-labs/factoryd/internal/state"
	"github.com/aicix-labs/factoryd/internal/submit"
	"github.com/aicix-labs/factoryd/internal/supervise"
)

// ---------- #42 acceptance: the shipped wrapper, a real supervisor ----------

// The producer turn command is the shipped examples/turn-wrapper.sh around
// a shell script that edits, declares intent, and touches progress -- what
// a real agent CLI does. The after-turn step is submit.AfterTurn over the
// lab's fakes. Nothing about the supervisor, the runner or the wrapper is
// faked.
type acceptance struct {
	*lab
	turns      int // producer turns the wrapper actually ran (counted by the script)
	marker     string
	beforeTurn func(context.Context, supervise.Turn) (string, error)
}

func wrapperPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "examples", "turn-wrapper.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func newAcceptance(t *testing.T, script string) *acceptance {
	t.Helper()
	l := newLab(t)
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	a := &acceptance{lab: l, marker: filepath.Join(l.root, "turns")}
	// The script counts its own runs in a file the test reads: the number
	// of producer turns is evidence, not an inference from state.
	full := fmt.Sprintf("echo run >> %s\n%s", a.marker, script)
	l.cfg.Roles = config.Roles{
		Producer: config.RoleSpec{Command: []string{wrapperPath(t), "sh", "-c", full}, Env: map[string]string{"PATH": os.Getenv("PATH")}, RunAs: &config.RunAs{User: u.Username}},
		Reviewer: config.RoleSpec{Command: []string{"true"}, Env: map[string]string{"PATH": os.Getenv("PATH")}},
	}
	l.cfg.Supervisor = config.Supervisor{SpinWarn: 2, SpinAbort: 4, FailAbort: 3, VerdictAttempts: 6, PollIntervalSeconds: 1, BackoffSeconds: 1, ForcePoll: true}
	l.cfg.Alerts = []config.Alert{{Kind: "file", Path: filepath.Join(l.root, "alerts.log")}}
	l.cfg.Credentials = config.Credentials{Producer: config.CredentialRef{Env: "P"}, Reviewer: config.CredentialRef{Env: "R"}}
	if err := l.cfg.Validate(); err != nil {
		t.Fatalf("%v", err)
	}
	return a
}

func (a *acceptance) producerTurns(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(a.marker)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "run")
}

func (a *acceptance) brief(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(a.root, "inbox", "brief.md"), []byte("fix it\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (a *acceptance) run(t *testing.T, maxTurns int) error {
	t.Helper()
	return a.runFor(t, maxTurns, 30*time.Second)
}

// runFor bounds an idle supervisor: a blocked factory idles by design, so
// the deadline, not a turn count, ends the run.
func (a *acceptance) runFor(t *testing.T, maxTurns int, d time.Duration) error {
	t.Helper()
	s, err := supervise.New(supervise.Options{
		Config: a.cfg, Role: "producer",
		Runner:     &supervise.ExecRunner{Config: a.cfg, Role: "producer", Stdout: io.Discard, Stderr: io.Discard},
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sleep:      func(ctx context.Context, d time.Duration) error { return nil }, // backoffs are instant; the count of turns is the evidence
		MaxTurns:   maxTurns,
		BeforeTurn: a.beforeTurn,
		AfterTurn:  submit.AfterTurn(a.cfg, func(context.Context) (submit.Deps, error) { return a.deps, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	err = s.Run(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

func (a *acceptance) producer(t *testing.T) *state.RoleState {
	t.Helper()
	st, err := state.Load(a.cfg.StatePath(), a.cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	return st.Role(state.RoleProducer)
}

func (a *acceptance) hasRetry() bool {
	_, err := os.Stat(a.cfg.RetryPath("producer"))
	return err == nil
}

const declareScript = `mkdir -p "$FACTORYD_WORKDIR/src" && printf 'changed\n' > "$FACTORYD_WORKDIR/src/a.go"
printf 'producer/fix\n' > "$FACTORYD_WORKDIR/.producer-branch"
printf 'gate: fix\n\nbody\n' > "$FACTORYD_WORKDIR/.producer-commit-msg"
touch "$FACTORYD_PROGRESS"
IFS=:; for p in $FACTORYD_TRIGGER_PATHS; do rm -f "$p"; done
`

// The ready-change incident (#40), with the stale intent left in the tree
// exactly as the real producer left it: a reviewer marked the family's
// open change ready; the producer re-declared the family. Submit refuses
// (blocked). Then: exactly one producer turn, no further gate or submit,
// no retry marker, the intent quarantined and the source work kept, the
// block durable and visible, and neither a progress touch nor a restart
// changes any of it.
func TestBlockedSubmissionIsNotRetriedAndTheProducerDoesNotRerun(t *testing.T) {
	a := newAcceptance(t, declareScript)
	a.drv.family = []scm.Change{{ID: "53", SourceBranch: "producer/fix-older", TargetBranch: "main", Draft: false, State: scm.StateOpen, Author: "producer-bot", AuthorID: "1"}}
	a.brief(t)
	if err := a.runFor(t, 50, 4*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := a.producerTurns(t); got != 1 {
		t.Fatalf("the wrapper ran %d producer turns, want exactly 1 after a terminal refusal", got)
	}
	gates := strings.Count(strings.Join(*a.gate.calls, ","), "gate")
	if gates != 0 {
		t.Fatalf("the gate ran %d times; a family that left the producer's hands is refused before the gate", gates)
	}
	if a.hasRetry() {
		t.Fatal("a retry was armed for a refusal that cannot change")
	}
	rs := a.producer(t)
	if rs.Blocked == nil || rs.Blocked.Disposition != "blocked" || !strings.Contains(rs.Blocked.Reason, "not one this producer may update") {
		t.Fatalf("blocked = %+v", rs.Blocked)
	}
	if rs.Blocked.Family != "producer/fix" {
		t.Fatalf("block family %q", rs.Blocked.Family)
	}
	for _, f := range []string{submit.BranchFile, submit.CommitMsgFile} {
		if _, err := os.Stat(filepath.Join(a.work, f)); !os.IsNotExist(err) {
			t.Fatalf("%s still declared after a terminal refusal; the next turn would resubmit it", f)
		}
	}
	if len(rs.Blocked.Quarantined) != 2 {
		t.Fatalf("quarantined %v", rs.Blocked.Quarantined)
	}
	if b, _ := os.ReadFile(filepath.Join(a.work, "src", "a.go")); string(b) != "changed\n" {
		t.Fatal("the producer's source work was discarded with the declaration")
	}

	// A progress touch after the refusal changes nothing.
	p := a.cfg.ProgressPath("producer")
	now := time.Now().Add(time.Second)
	os.WriteFile(p, []byte("x"), 0o644)
	os.Chtimes(p, now, now)
	a.brief(t) // even a new brief: the block stands until a submission succeeds
	if err := a.runFor(t, 1, 4*time.Second); err != nil {
		t.Fatal(err)
	}
	if a.producer(t).Blocked == nil {
		t.Fatal("progress and a restart cleared the block")
	}
	if got := a.producerTurns(t); got != 2 {
		t.Fatalf("turns=%d after the new brief, want 2", got)
	}

	// Only a submission that succeeds clears it: the reviewer's change is
	// gone from the family (merged), and the same intent submits.
	a.drv.family = nil
	os.WriteFile(filepath.Join(a.work, submit.BranchFile), []byte("producer/fix\n"), 0o644)
	os.WriteFile(filepath.Join(a.work, submit.CommitMsgFile), []byte("gate: fix\n\nbody\n"), 0o644)
	if r, err := submit.Run(context.Background(), a.cfg, a.deps); err != nil || r.Exit != submit.ExitSubmitted {
		t.Fatalf("r=%+v err=%v\n%s", r, err, a.log)
	}
	if a.producer(t).Blocked != nil {
		t.Fatal("a successful submission did not clear the block")
	}
}

// A transient submit failure (the push failed once) is retried as the
// after-turn step alone: the producer does not rerun, the intent it
// declared is submitted on the retry, and the draft opens.
func TestTransientSubmitFailureResumesSubmitWithoutASecondProducerTurn(t *testing.T) {
	a := newAcceptance(t, declareScript)
	a.tr.pushErrs = []error{errors.New("remote: 502 Bad Gateway")}
	a.brief(t)
	if err := a.run(t, 2); err != nil {
		t.Fatal(err)
	}
	if got := a.producerTurns(t); got != 1 {
		t.Fatalf("the wrapper ran %d producer turns, want 1: a transient submit failure resumes submit, it does not rerun the model", got)
	}
	if pushes := strings.Count(strings.Join(a.tr.calls, ","), "push"); pushes != 2 {
		t.Fatalf("pushes=%d, want 2 (the failed one and the resumed one)", pushes)
	}
	if a.drv.opened == nil {
		t.Fatal("the draft was never opened after the resumed submit")
	}
	rs := a.producer(t)
	if rs.Blocked != nil || a.hasRetry() {
		t.Fatalf("after the resumed submit succeeded: blocked=%+v retry=%v", rs.Blocked, a.hasRetry())
	}
	st := mustLoad(t, a.cfg)
	if st.Cycle == nil || st.Cycle.Phase != state.CycleOpen || st.Cycle.ChangeID != "42" {
		t.Fatalf("cycle %+v", st.Cycle)
	}
}

// An ambiguous external outcome -- the provider answered the open with a
// change that is not the one asked for -- is unknown: no retry, blocked
// pending reconciliation, the intent kept for the operator.
func TestUnknownSubmitOutcomeStaysBlockedPendingReconciliation(t *testing.T) {
	a := newAcceptance(t, declareScript)
	a.drv.openWrong = true
	a.brief(t)
	if err := a.runFor(t, 50, 4*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := a.producerTurns(t); got != 1 {
		t.Fatalf("turns=%d, want 1", got)
	}
	rs := a.producer(t)
	if rs.Blocked == nil || rs.Blocked.Disposition != "unknown" {
		t.Fatalf("blocked = %+v, want unknown", rs.Blocked)
	}
	if a.hasRetry() {
		t.Fatal("an unknown outcome was armed for an automatic retry")
	}
	if _, err := os.Stat(filepath.Join(a.work, submit.BranchFile)); err != nil {
		t.Fatal("the intent was quarantined on an unknown outcome; reconciliation needs it")
	}
}

func mustLoad(t *testing.T, cfg *config.Config) *state.State {
	t.Helper()
	st, err := state.Load(cfg.StatePath(), cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// A malformed declaration -- one control file without the other -- is a
// blocked submission like any other: recorded, quarantined, not retried,
// never a plain error the supervisor suppresses in silence (#45 review).
func TestMalformedDeclarationIsBlockedAndQuarantined(t *testing.T) {
	const partial = `mkdir -p "$FACTORYD_WORKDIR/src" && printf 'changed\n' > "$FACTORYD_WORKDIR/src/a.go"
printf 'producer/fix\n' > "$FACTORYD_WORKDIR/.producer-branch"
touch "$FACTORYD_PROGRESS"
IFS=:; for p in $FACTORYD_TRIGGER_PATHS; do rm -f "$p"; done
`
	a := newAcceptance(t, partial)
	a.brief(t)
	if err := a.runFor(t, 50, 4*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := a.producerTurns(t); got != 1 {
		t.Fatalf("turns=%d, want 1", got)
	}
	rs := a.producer(t)
	if rs.Blocked == nil || rs.Blocked.Disposition != "blocked" || !strings.Contains(rs.Blocked.Reason, "not a valid declaration") {
		t.Fatalf("blocked = %+v; a malformed declaration left no durable record", rs.Blocked)
	}
	if a.hasRetry() {
		t.Fatal("a retry was armed for a malformed declaration")
	}
	if _, err := os.Stat(filepath.Join(a.work, submit.BranchFile)); !os.IsNotExist(err) {
		t.Fatal("the malformed declaration was left live")
	}
	if len(rs.Blocked.Quarantined) != 1 || !strings.HasPrefix(rs.Blocked.Quarantined[0], ".producer-branch.blocked-") {
		t.Fatalf("quarantined %v", rs.Blocked.Quarantined)
	}
	if len(a.tr.calls) != 0 || len(*a.gate.calls) != 0 {
		t.Fatalf("submit side effects for a malformed declaration: transport %v gate %v", a.tr.calls, *a.gate.calls)
	}
}

// After an unknown outcome the intent is kept live for reconciliation --
// and a later brief whose agent declares nothing new must not submit it
// again automatically. The after-turn step consults the block first and
// does nothing; only the operator's explicit submit uses the intent, and
// its success clears the block (#45 review).
func TestSecondTriggerAfterAnUnknownOutcomeHasNoSubmitSideEffects(t *testing.T) {
	a := newAcceptance(t, `touch "$FACTORYD_PROGRESS"
IFS=:; for p in $FACTORYD_TRIGGER_PATHS; do rm -f "$p"; done
`)
	// The unknown outcome, recorded directly: the intent is live, as it
	// was left after the provider's ambiguous answer.
	os.MkdirAll(filepath.Join(a.work, "src"), 0o755)
	os.WriteFile(filepath.Join(a.work, "src", "a.go"), []byte("changed\n"), 0o644)
	os.WriteFile(filepath.Join(a.work, submit.BranchFile), []byte("producer/fix\n"), 0o644)
	os.WriteFile(filepath.Join(a.work, submit.CommitMsgFile), []byte("gate: fix\n\nbody\n"), 0o644)
	if _, err := state.Update(a.cfg.StatePath(), a.cfg.Name, func(st *state.State) error {
		st.Role(state.RoleProducer).Blocked = &state.Block{Disposition: "unknown", Reason: "the provider opened 99 but it is not the change that was requested", Turn: "producer-0", At: time.Now()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A new brief; the agent declares nothing new.
	a.brief(t)
	if err := a.runFor(t, 1, 4*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := a.producerTurns(t); got != 1 {
		t.Fatalf("turns=%d, want 1", got)
	}
	if len(a.tr.calls) != 0 || len(*a.gate.calls) != 0 || a.drv.opened != nil {
		t.Fatalf("the stale intent was replayed automatically: transport %v gate %v opened %v", a.tr.calls, *a.gate.calls, a.drv.opened != nil)
	}
	rs := a.producer(t)
	if rs.Blocked == nil || rs.Blocked.Disposition != "unknown" {
		t.Fatalf("the block did not stand: %+v", rs.Blocked)
	}
	if _, err := os.Stat(filepath.Join(a.work, submit.BranchFile)); err != nil {
		t.Fatal("the intent kept for reconciliation is gone")
	}
	if a.hasRetry() {
		t.Fatal("a retry was armed")
	}

	// The operator reconciles: an explicit submit uses the intent and, on
	// success, clears the block.
	if r, err := submit.Run(context.Background(), a.cfg, a.deps); err != nil || r.Exit != submit.ExitSubmitted {
		t.Fatalf("r=%+v err=%v\n%s", r, err, a.log)
	}
	if a.producer(t).Blocked != nil {
		t.Fatal("the operator's successful submission did not clear the block")
	}
}

// ---------- #50: the agent wrapper on a real supervisor ----------

func agentWrapperPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "examples", "producer-turn-agent.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// newAgentAcceptance runs the producer through examples/producer-turn-
// agent.sh around a scripted "model" that reads its prompt on stdin.
func newAgentAcceptance(t *testing.T, model string) *acceptance {
	t.Helper()
	a := newAcceptance(t, "true")
	full := fmt.Sprintf("cat > %s.prompt; echo run >> %s\n%s", a.marker, a.marker, model)
	a.cfg.Roles.Producer.Command = []string{agentWrapperPath(t), "sh", "-c", full}
	if err := a.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return a
}

func (a *acceptance) verdictFor(t *testing.T, id, family string) string {
	t.Helper()
	v := state.Verdict{ChangeID: id, Kind: state.VerdictChangesRequested, SHA: "abc", Summary: "fix it", At: time.Now(),
		Branch: family + "-0123456789", DeclaredBranch: family}
	// Issued the way the reviewer issues it: written AND registered. A
	// file planted in the outbox is not a verdict (#50 review).
	p, err := signal.Issue(a.cfg, v)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (a *acceptance) lastPrompt(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(a.marker + ".prompt")
	return string(b)
}

// A changes-requested verdict, and the model exits non-zero. The verdict
// trigger survives the failed turn, so the retry the supervisor arms is
// told the verdict and its family again -- not a bare retry with nothing
// to retry (#50 review).
func TestAgentFailureKeepsTheVerdictForTheRetry(t *testing.T) {
	a := newAgentAcceptance(t, `touch "$FACTORYD_PROGRESS"; exit 7`)
	vp := a.verdictFor(t, "48", "producer/fix")
	if err := a.runFor(t, 2, 6*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := a.producerTurns(t); got < 2 {
		t.Fatalf("turns=%d, want the failed turn and its retry", got)
	}
	if _, err := os.Stat(vp); err != nil {
		t.Fatal("the verdict trigger was consumed by a failed turn; the retry had no verdict")
	}
	if p := a.lastPrompt(t); !strings.Contains(p, "THIS TURN ACTS ON ONE changes-requested verdict: change 48") || !strings.Contains(p, "verbatim:\n    producer/fix\n") {
		t.Fatalf("the retry turn was not told the verdict:\n%s", p)
	}
	if len(a.tr.calls) != 0 {
		t.Fatalf("submit ran for a turn that declared nothing: %v", a.tr.calls)
	}
}

// The model exits clean, touches progress, and declares nothing. The
// verdict is not acted on, its trigger is kept, and nothing is submitted;
// the supervisor's spin guard, not silence, is what follows.
func TestAgentCleanNoIntentKeepsTheVerdict(t *testing.T) {
	a := newAgentAcceptance(t, `touch "$FACTORYD_PROGRESS"; exit 0`)
	vp := a.verdictFor(t, "48", "producer/fix")
	if err := a.runFor(t, 1, 4*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(vp); err != nil {
		t.Fatal("the verdict trigger was consumed by a turn that declared nothing")
	}
	if len(a.tr.calls) != 0 || a.drv.opened != nil {
		t.Fatalf("submit side effects for a no-intent turn: %v", a.tr.calls)
	}
	// Not a clean turn: the verdict was not acted on, so the wrapper
	// reports a failure with no progress (#50 review, round 6).
	if rs := a.producer(t); rs.LastTurn == nil || rs.LastTurn.ExitCode == nil || *rs.LastTurn.ExitCode == 0 {
		t.Fatalf("last turn %+v; a no-intent turn on a verdict must not read as clean", rs.LastTurn)
	}
}

// The unresolved-verdict loop (#50 review, round 6): a model that touches
// progress and declares nothing, on a changes-requested verdict, must NOT
// read as progress -- that would reset both guards and re-run the kept
// trigger forever. The wrapper rolls the marker back and fails the turn,
// so the supervisor backs off and halts at fail_abort, with the verdict
// still in the outbox and nothing submitted. Same for the wrong family.
func TestUnresolvedVerdictReachesFailAbortWithTheVerdictKept(t *testing.T) {
	for _, c := range []struct{ name, model string }{
		{"no intent, progress touched", `touch "$FACTORYD_PROGRESS"; exit 0`},
		{"wrong family, progress touched", `printf 'producer/other
' > "$FACTORYD_WORKDIR/.producer-branch"
printf 'msg
' > "$FACTORYD_WORKDIR/.producer-commit-msg"
touch "$FACTORYD_PROGRESS"; exit 0`},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newAgentAcceptance(t, c.model)
			vp := a.verdictFor(t, "48", "producer/fix")
			err := a.runFor(t, 50, 20*time.Second)
			if !errors.Is(err, supervise.ErrHalted) {
				t.Fatalf("Run returned %v, want ErrHalted: the unresolved verdict never reached fail_abort (turns=%d)", err, a.producerTurns(t))
			}
			rs := a.producer(t)
			if !rs.Halted || !strings.Contains(rs.HaltReason, "fail_abort") {
				t.Fatalf("halt: %+v", rs)
			}
			if got := a.producerTurns(t); got != a.cfg.Supervisor.FailAbort {
				t.Fatalf("turns=%d, want exactly fail_abort=%d", got, a.cfg.Supervisor.FailAbort)
			}
			if _, err := os.Stat(vp); err != nil {
				t.Fatal("the verdict was lost on the way to the halt")
			}
			if len(a.tr.calls) != 0 || a.drv.opened != nil {
				t.Fatalf("submit ran: %v", a.tr.calls)
			}
		})
	}
}

// The model declares completely: the verdict trigger is consumed, submit
// runs, and the successor is opened under the same family.
func TestAgentCompleteDeclarationConsumesTheVerdictAndSubmits(t *testing.T) {
	a := newAgentAcceptance(t, `mkdir -p "$FACTORYD_WORKDIR/src" && printf 'fixed\n' > "$FACTORYD_WORKDIR/src/a.go"
printf 'producer/fix\n' > "$FACTORYD_WORKDIR/.producer-branch"
printf 'fix: the requested change\n\nbody\n' > "$FACTORYD_WORKDIR/.producer-commit-msg"
touch "$FACTORYD_PROGRESS"`)
	vp := a.verdictFor(t, "48", "producer/fix")
	if err := a.runFor(t, 1, 4*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(vp); !os.IsNotExist(err) {
		t.Fatal("the verdict trigger was not consumed after a complete declaration")
	}
	if a.drv.opened == nil || a.drv.opened.SourceBranch != submit.ImmutableBranch("producer/fix", "abc123") {
		t.Fatalf("the successor was not opened under the family: %+v", a.drv.opened)
	}
}

// A root-side submission is the acknowledgement. If its first push fails,
// the retry must retain that factoryd-owned pending acknowledgement and only
// consume the verdict after the resumed submit actually succeeds.
func TestChangesRequestedVerdictIsConsumedOnlyAfterResumedRootSubmit(t *testing.T) {
	a := newAgentAcceptance(t, `mkdir -p "$FACTORYD_WORKDIR/src" && printf 'fixed\n' > "$FACTORYD_WORKDIR/src/a.go"
printf 'producer/fix\n' > "$FACTORYD_WORKDIR/.producer-branch"
printf 'fix: the requested change\n\nbody\n' > "$FACTORYD_WORKDIR/.producer-commit-msg"
touch "$FACTORYD_PROGRESS"`)
	a.tr.pushErrs = []error{errors.New("remote: 502 Bad Gateway")}
	a.verdictFor(t, "48", "producer/fix")
	if err := a.runFor(t, 2, 6*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := a.producerTurns(t); got != 1 {
		t.Fatalf("producer turns=%d, want 1: retry must resume only root-side submit", got)
	}
	st := mustLoad(t, a.cfg)
	iv := st.Issued["48"]
	if iv.ConsumedAt == nil || iv.ConsumedByTurn == "" || iv.PendingSubmission != nil {
		t.Fatalf("verdict receipt after resumed submit = %+v, want consumed with no pending receipt", iv)
	}
	if a.drv.opened == nil {
		t.Fatal("the resumed root-side submit did not open the successor")
	}
}

// The model declares completely -- under the WRONG family. The verdict's
// trigger is kept, the declaration is moved aside, the turn fails, and
// the after-turn step never runs: no submit, no unrelated draft (#29,
// #50 review). The retry is told the verdict and its family again.
func TestAgentWrongFamilyKeepsTheVerdictAndSubmitsNothing(t *testing.T) {
	a := newAgentAcceptance(t, `mkdir -p "$FACTORYD_WORKDIR/src" && printf 'fixed\n' > "$FACTORYD_WORKDIR/src/a.go"
printf 'producer/other\n' > "$FACTORYD_WORKDIR/.producer-branch"
printf 'fix: something else\n\nbody\n' > "$FACTORYD_WORKDIR/.producer-commit-msg"
touch "$FACTORYD_PROGRESS"`)
	vp := a.verdictFor(t, "48", "producer/fix")
	if err := a.runFor(t, 2, 6*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(vp); err != nil {
		t.Fatal("the verdict trigger was consumed by a declaration under another family")
	}
	if len(a.tr.calls) != 0 || a.drv.opened != nil {
		t.Fatalf("submit ran for a wrong-family declaration: %v opened=%v", a.tr.calls, a.drv.opened != nil)
	}
	if _, err := os.Stat(filepath.Join(a.work, submit.BranchFile)); !os.IsNotExist(err) {
		t.Fatal("the wrong-family declaration was left for submit")
	}
	if got := a.producerTurns(t); got < 2 {
		t.Fatalf("turns=%d, want the failed turn and its retry", got)
	}
	if p := a.lastPrompt(t); !strings.Contains(p, "verbatim:\n    producer/fix\n") {
		t.Fatalf("the retry was not told the family:\n%s", p)
	}
	if b, _ := os.ReadFile(filepath.Join(a.work, "src", "a.go")); string(b) != "fixed\n" {
		t.Fatal("the producer's source work was touched")
	}
}

// Touch-then-hang (#50 review, round 7): the model touches progress and
// never returns; the supervisor kills the turn at its deadline, so the
// wrapper's rollback never runs. A timed-out turn counts on the fail
// streak whatever the marker says: the factory halts at fail_abort with
// the verdict kept and nothing submitted.
func TestTouchThenTimeoutReachesFailAbortWithTheVerdictKept(t *testing.T) {
	a := newAgentAcceptance(t, `touch "$FACTORYD_PROGRESS"; sleep 30`)
	a.cfg.Roles.Producer.TimeoutSeconds = 1
	vp := a.verdictFor(t, "48", "producer/fix")
	err := a.runFor(t, 50, 30*time.Second)
	if !errors.Is(err, supervise.ErrHalted) {
		t.Fatalf("Run returned %v, want ErrHalted after fail_abort timed-out turns (turns=%d)", err, a.producerTurns(t))
	}
	rs := a.producer(t)
	if !strings.Contains(rs.HaltReason, "fail_abort") || !strings.Contains(rs.HaltReason, "timed out true") {
		t.Fatalf("halt reason %q", rs.HaltReason)
	}
	if got := a.producerTurns(t); got != a.cfg.Supervisor.FailAbort {
		t.Fatalf("turns=%d, want exactly fail_abort=%d", got, a.cfg.Supervisor.FailAbort)
	}
	if _, err := os.Stat(vp); err != nil {
		t.Fatal("the verdict was lost")
	}
	if len(a.tr.calls) != 0 || a.drv.opened != nil {
		t.Fatalf("submit ran: %v", a.tr.calls)
	}
}

// A legitimate multi-turn fix (#50 review, round 7): the model edits the
// tree a little each turn, truthfully touches progress, and declares only
// when the fix is complete -- after MORE than fail_abort turns. Observable
// work is progress; the verdict waits; no halt; the final turn submits.
func TestMultiTurnPartialFixIsProgressAndEventuallySubmits(t *testing.T) {
	a := newAgentAcceptance(t, `n=$(wc -l < "$FACTORYD_ROOT/turns")
mkdir -p "$FACTORYD_WORKDIR/src"
if [ "$n" -le 5 ]; then
  printf 'part %s\n' "$n" > "$FACTORYD_WORKDIR/src/part$n.go"
  touch "$FACTORYD_PROGRESS"
  exit 0
fi
printf 'producer/fix\n' > "$FACTORYD_WORKDIR/.producer-branch"
printf 'fix: complete\n\nbody\n' > "$FACTORYD_WORKDIR/.producer-commit-msg"
touch "$FACTORYD_PROGRESS"`)
	if a.cfg.Supervisor.FailAbort >= 5 || a.cfg.Supervisor.SpinAbort >= 5 {
		t.Fatalf("the control needs more partial turns than fail_abort=%d and spin_abort=%d", a.cfg.Supervisor.FailAbort, a.cfg.Supervisor.SpinAbort)
	}
	vp := a.verdictFor(t, "48", "producer/fix")
	if err := a.runFor(t, 6, 30*time.Second); err != nil {
		t.Fatalf("Run: %v (turns=%d); a multi-turn fix was halted", err, a.producerTurns(t))
	}
	if got := a.producerTurns(t); got != 6 {
		t.Fatalf("turns=%d, want 6: five partial turns and the declaring one", got)
	}
	rs := a.producer(t)
	if rs.Halted {
		t.Fatalf("halted: %s", rs.HaltReason)
	}
	if _, err := os.Stat(vp); !os.IsNotExist(err) {
		t.Fatal("the verdict was not consumed by the declaring turn")
	}
	if a.drv.opened == nil || a.drv.opened.SourceBranch != submit.ImmutableBranch("producer/fix", "abc123") {
		t.Fatalf("the fix was not submitted under the family: %+v", a.drv.opened)
	}
	for n := 1; n <= 5; n++ {
		if _, err := os.Stat(filepath.Join(a.work, "src", fmt.Sprintf("part%d.go", n))); err != nil {
			t.Fatalf("partial work part%d.go is gone", n)
		}
	}
}

// A timestamp is not evidence of work (#50 review, round 8). A model that
// only touches the same worktree artifact each turn -- a heartbeat, a
// refreshed build file -- changes no content, type or mode: the wrapper
// rolls progress back, and the factory halts at fail_abort with the
// verdict kept and nothing submitted.
func TestTouchOnlyHeartbeatReachesFailAbortWithTheVerdictKept(t *testing.T) {
	a := newAgentAcceptance(t, `mkdir -p "$FACTORYD_WORKDIR/build"; [ -e "$FACTORYD_WORKDIR/build/heartbeat" ] || printf 'x\n' > "$FACTORYD_WORKDIR/build/heartbeat"
touch "$FACTORYD_WORKDIR/build/heartbeat"; touch "$FACTORYD_PROGRESS"; exit 0`)
	// The first turn creates the file (real content change, one partial
	// turn); every later turn only touches it. fail_abort more turns halt.
	vp := a.verdictFor(t, "48", "producer/fix")
	err := a.runFor(t, 50, 30*time.Second)
	if !errors.Is(err, supervise.ErrHalted) {
		t.Fatalf("Run returned %v, want ErrHalted (turns=%d): a touched timestamp counted as work", err, a.producerTurns(t))
	}
	if got := a.producerTurns(t); got != 1+a.cfg.Supervisor.FailAbort {
		t.Fatalf("turns=%d, want 1 real change + fail_abort=%d touch-only turns", got, a.cfg.Supervisor.FailAbort)
	}
	if _, err := os.Stat(vp); err != nil {
		t.Fatal("the verdict was lost")
	}
	if len(a.tr.calls) != 0 || a.drv.opened != nil {
		t.Fatalf("submit ran: %v", a.tr.calls)
	}
}

// Even content changes can be meaningless (#50 review, round 8). A model
// that rewrites a file with new content every turn and never declares is
// bounded by the SUPERVISOR (round 9): supervisor.verdict_attempts, in
// factoryd's own state, not in anything the producer writes. Past it a
// turn that leaves the verdict pending is credited no progress, so the
// spin guard halts with the verdict kept.
func TestEndlessContentChangesAreBoundedPerVerdict(t *testing.T) {
	a := newAgentAcceptance(t, `n=$(wc -l < "$FACTORYD_ROOT/turns"); mkdir -p "$FACTORYD_WORKDIR/src"
printf 'attempt %s\n' "$n" > "$FACTORYD_WORKDIR/src/churn.go"; touch "$FACTORYD_PROGRESS"; exit 0`)
	a.cfg.Supervisor.VerdictAttempts = 2
	if err := a.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	vp := a.verdictFor(t, "48", "producer/fix")
	err := a.runFor(t, 50, 30*time.Second)
	if !errors.Is(err, supervise.ErrHalted) {
		t.Fatalf("Run returned %v, want ErrHalted (turns=%d): endless content churn was unbounded", err, a.producerTurns(t))
	}
	if got := a.producerTurns(t); got != 2+a.cfg.Supervisor.SpinAbort {
		t.Fatalf("turns=%d, want verdict_attempts=2 credited turns + spin_abort=%d uncredited", got, a.cfg.Supervisor.SpinAbort)
	}
	if rs := a.producer(t); !strings.Contains(rs.HaltReason, "spin_abort") {
		t.Fatalf("halt reason %q", rs.HaltReason)
	}
	if _, err := os.Stat(vp); err != nil {
		t.Fatal("the verdict was lost")
	}
	if len(a.tr.calls) != 0 {
		t.Fatalf("submit ran: %v", a.tr.calls)
	}
}

// Exhaustion, then a wrong-family declaration, then more partial work
// (#50 review, round 9): the bound is the supervisor's and survives the
// wrapper's own outcomes. Bound 2: two credited partial turns; a
// wrong-family declaration (refused, failed, verdict kept); then partial
// turns that are credited nothing -- the factory halts with the verdict
// kept and nothing submitted. Nothing the producer did reset the count.
func TestExhaustionThenWrongFamilyThenMorePartialWorkStillHalts(t *testing.T) {
	a := newAgentAcceptance(t, `n=$(wc -l < "$FACTORYD_ROOT/turns"); mkdir -p "$FACTORYD_WORKDIR/src"
printf 'attempt %s\n' "$n" > "$FACTORYD_WORKDIR/src/churn.go"
if [ "$n" -eq 3 ]; then
  printf 'producer/other\n' > "$FACTORYD_WORKDIR/.producer-branch"; printf 'msg\n' > "$FACTORYD_WORKDIR/.producer-commit-msg"
fi
touch "$FACTORYD_PROGRESS"; exit 0`)
	a.cfg.Supervisor.VerdictAttempts = 2
	if err := a.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	vp := a.verdictFor(t, "48", "producer/fix")
	err := a.runFor(t, 50, 30*time.Second)
	if !errors.Is(err, supervise.ErrHalted) {
		t.Fatalf("Run returned %v, want ErrHalted (turns=%d): a wrong-family declaration reset the bound", err, a.producerTurns(t))
	}
	// Turns 1-2 credited; turn 3 failed (wrong family: rolled back, no
	// progress, so it is the first uncredited turn on the spin guard, and
	// the count keeps climbing); turns 4.. credited nothing; halt when the
	// spin guard reaches spin_abort.
	if got := a.producerTurns(t); got != 2+a.cfg.Supervisor.SpinAbort {
		t.Fatalf("turns=%d, want 2 credited + spin_abort=%d uncredited (the wrong-family turn among them)", got, a.cfg.Supervisor.SpinAbort)
	}
	if _, err := os.Stat(vp); err != nil {
		t.Fatal("the verdict was lost")
	}
	if len(a.tr.calls) != 0 || a.drv.opened != nil {
		t.Fatalf("submit ran: %v", a.tr.calls)
	}
	if rs := a.producer(t); rs.TriggerAttempts["48"] < 3 {
		t.Fatalf("trigger_attempts=%v; the count did not survive the wrong-family turn", rs.TriggerAttempts)
	}
}

// Two changes-requested verdicts (#50 review, round 10): the supervisor
// selects the active one -- the oldest -- and passes the other to no turn
// until the first is consumed, so a multi-turn fix for A spends none of
// B's allowance. A is fixed over three partial turns and declared on the
// fourth; B then gets its own turns with a fresh count, and is declared.
func TestQueuedVerdictKeepsItsAllowanceWhileTheActiveOneIsFixed(t *testing.T) {
	a := newAgentAcceptance(t, `n=$(wc -l < "$FACTORYD_ROOT/turns"); mkdir -p "$FACTORYD_WORKDIR/src"
fam=$(printf '%s' "$FACTORYD_VERDICTS_TSV" | cut -f5 | head -1)
case "$fam" in
  producer/a) if [ "$n" -le 3 ]; then printf 'a step %s\n' "$n" > "$FACTORYD_WORKDIR/src/a.go"; touch "$FACTORYD_PROGRESS"; exit 0; fi
              printf 'producer/a\n' > "$FACTORYD_WORKDIR/.producer-branch"; printf 'fix a\n\nbody\n' > "$FACTORYD_WORKDIR/.producer-commit-msg"; touch "$FACTORYD_PROGRESS";;
  producer/b) if [ "$n" -le 5 ]; then printf 'b step %s\n' "$n" > "$FACTORYD_WORKDIR/src/b.go"; touch "$FACTORYD_PROGRESS"; exit 0; fi
              printf 'producer/b\n' > "$FACTORYD_WORKDIR/.producer-branch"; printf 'fix b\n\nbody\n' > "$FACTORYD_WORKDIR/.producer-commit-msg"; touch "$FACTORYD_PROGRESS";;
  *) echo "unexpected family $fam" >&2; exit 9;;
esac`)
	a.cfg.Supervisor.VerdictAttempts = 3 // A takes exactly 3 partial turns; B must not have been charged for them
	if err := a.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	// A issued first (older), then B.
	va := a.verdictFor(t, "48", "producer/a")
	time.Sleep(10 * time.Millisecond)
	vb := a.verdictFor(t, "49", "producer/b")
	// A's four turns first. B must have been passed to no turn and charged
	// nothing: the supervisor, not the wrapper, holds it back.
	if err := a.runFor(t, 4, 30*time.Second); err != nil {
		t.Fatalf("Run: %v (turns=%d)", err, a.producerTurns(t))
	}
	if _, err := os.Stat(va); !os.IsNotExist(err) {
		t.Fatal("A not consumed after its four turns")
	}
	if rs := a.producer(t); rs.TriggerAttempts["49"] != 0 {
		t.Fatalf("attempts=%v after A's fix; B was charged for A's turns", rs.TriggerAttempts)
	}
	if _, err := os.Stat(vb); err != nil {
		t.Fatal("B was consumed during A's fix")
	}
	if err := a.runFor(t, 2, 30*time.Second); err != nil {
		t.Fatalf("Run: %v (turns=%d); the queued verdict's allowance was spent by the active one", err, a.producerTurns(t))
	}
	if got := a.producerTurns(t); got != 6 {
		t.Fatalf("turns=%d, want 6: three partial on A, A declared, one partial on B, B declared", got)
	}
	for _, p := range []string{va, vb} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s not consumed", filepath.Base(p))
		}
	}
	if rs := a.producer(t); rs.Halted || len(rs.TriggerAttempts) != 0 {
		t.Fatalf("halted=%v attempts=%v", rs.Halted, rs.TriggerAttempts)
	}
	if opened := a.drv.opened; opened == nil || opened.SourceBranch != submit.ImmutableBranch("producer/b", "abc123") {
		t.Fatalf("last opened %+v; want B's successor", opened)
	}
}

// The outbox is the producer's to write; a file there is a verdict only if
// the registry names it and the bytes match (#50 review, round 10). A
// forged 49.json is quarantined and is no trigger: no turn runs for it.
// And a registered verdict the producer deletes and recreates byte-for-
// byte is the same verdict: its count continues, keyed by change id.
func TestForgedVerdictIsNoTriggerAndAReplacedOneKeepsItsCount(t *testing.T) {
	// Real partial work each turn, so turns are credited and the count
	// climbs without the fail streak intervening.
	a := newAgentAcceptance(t, `n=$(wc -l < "$FACTORYD_ROOT/turns"); mkdir -p "$FACTORYD_WORKDIR/src"
printf 'step %s\n' "$n" > "$FACTORYD_WORKDIR/src/work.go"; touch "$FACTORYD_PROGRESS"; exit 0`)
	forged := filepath.Join(a.cfg.OutboxDir(), "49.json")
	os.WriteFile(forged, []byte(`{"change_id":"49","kind":"changes-requested","summary":"forged","branch":"producer/x-0123456789","declared_branch":"producer/x"}`+"\n"), 0o644)
	if err := a.runFor(t, 1, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := a.producerTurns(t); got != 0 {
		t.Fatalf("turns=%d; a forged verdict ran a turn", got)
	}
	if _, err := os.Stat(forged); !os.IsNotExist(err) {
		t.Fatal("the forged file was not quarantined")
	}
	if _, err := os.Stat(forged + ".unregistered"); err != nil {
		t.Fatal("the forged file was not moved aside as .unregistered")
	}

	// A real verdict, carried two turns (count 2); the producer deletes
	// and recreates it byte for byte; the count continues from 2.
	vp := a.verdictFor(t, "48", "producer/fix")
	body, _ := os.ReadFile(vp)
	if err := a.runFor(t, 2, 4*time.Second); err != nil {
		t.Fatal(err)
	}
	if rs := a.producer(t); rs.TriggerAttempts["48"] != 2 {
		t.Fatalf("attempts=%v, want 48:2", rs.TriggerAttempts)
	}
	os.Remove(vp)
	os.WriteFile(vp, body, 0o644)
	if err := a.runFor(t, 1, 4*time.Second); err != nil {
		t.Fatal(err)
	}
	if rs := a.producer(t); rs.TriggerAttempts["48"] != 3 {
		t.Fatalf("attempts=%v after delete+recreate, want 48:3: the replacement was given a fresh budget", rs.TriggerAttempts)
	}
	// And a recreated file with DIFFERENT bytes is not the verdict.
	os.Remove(vp)
	os.WriteFile(vp, append(body, ' '), 0o644)
	before := a.producerTurns(t)
	if err := a.runFor(t, 1, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if a.producerTurns(t) != before {
		t.Fatal("a tampered verdict file ran a turn")
	}
	if _, err := os.Stat(vp + ".unregistered"); err != nil {
		t.Fatal("the tampered file was not quarantined")
	}
}

// A byte-identical body can stay pending across turns, but once a real turn
// consumes it the receipt belongs to factoryd, not the producer's outbox. A
// saved body restored afterwards is a replay, never a fresh trigger.
func TestConsumedVerdictCannotBeReplayed(t *testing.T) {
	a := newAgentAcceptance(t, `mkdir -p "$FACTORYD_WORKDIR/src" && printf 'fixed\n' > "$FACTORYD_WORKDIR/src/a.go"
printf 'producer/fix\n' > "$FACTORYD_WORKDIR/.producer-branch"
printf 'fix: the requested change\n\nbody\n' > "$FACTORYD_WORKDIR/.producer-commit-msg"
touch "$FACTORYD_PROGRESS"`)
	vp := a.verdictFor(t, "48", "producer/fix")
	body, err := os.ReadFile(vp)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.runFor(t, 1, 4*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(vp); !os.IsNotExist(err) {
		t.Fatal("the first turn did not consume its verdict")
	}
	st := mustLoad(t, a.cfg)
	iv := st.Issued["48"]
	if iv.ConsumedAt == nil || iv.ConsumedByTurn == "" {
		t.Fatalf("issued verdict has no consumption receipt: %+v", iv)
	}
	if err := os.WriteFile(vp, body, 0o644); err != nil {
		t.Fatal(err)
	}
	before := a.producerTurns(t)
	if err := a.runFor(t, 1, 4*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := a.producerTurns(t); got != before {
		t.Fatalf("replayed verdict ran a producer turn: got %d, want %d", got, before)
	}
	if _, err := os.Stat(vp + ".replayed"); err != nil {
		t.Fatalf("the replay was not quarantined: %v", err)
	}
}

// The producer owns the outbox directory, so deleting a verdict path is not
// an acknowledgement. A no-intent turn that removes it must leave the
// issuance unconsumed and make the unresolved work visible to an operator.
func TestProducerDeletionCannotConsumeAChangesRequestedVerdict(t *testing.T) {
	a := newAgentAcceptance(t, `rm -f "$FACTORYD_OUTBOX/48.json"
touch "$FACTORYD_PROGRESS"
exit 0`)
	vp := a.verdictFor(t, "48", "producer/fix")
	if err := a.runFor(t, 1, 4*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(vp); !os.IsNotExist(err) {
		t.Fatal("test producer did not delete the verdict")
	}
	st := mustLoad(t, a.cfg)
	iv := st.Issued["48"]
	if iv.ConsumedAt != nil {
		t.Fatalf("producer deletion wrote a verdict receipt: %+v", iv)
	}
	if iv.PendingSubmission != nil {
		t.Fatalf("a no-intent deletion looked like a root-side submission: %+v", iv.PendingSubmission)
	}
	rs := st.Role(state.RoleProducer)
	if b := rs.Blocked; b == nil || !strings.Contains(b.Reason, "disappeared from the producer-writable outbox") {
		t.Fatalf("deleted unresolved verdict was not visibly blocked: %+v", b)
	}
	if rs.CurrentTurn != nil {
		t.Fatalf("deleted-verdict turn remains running after its process exited: %+v", rs.CurrentTurn)
	}
	if rs.LastTurn == nil || rs.LastTurn.EndedAt == nil || rs.LastTurn.ExitCode == nil || rs.LastTurn.ID != rs.Blocked.Turn {
		t.Fatalf("deleted-verdict turn was not finalized: last=%+v block=%+v", rs.LastTurn, rs.Blocked)
	}
	if b := mustLoad(t, a.cfg).Role(state.RoleProducer).Blocked; b == nil || b.Turn != rs.Blocked.Turn {
		t.Fatalf("deleted-verdict block was not durable: %+v", b)
	}
}

// A matching declaration is still not an acknowledgement if root-side submit
// concludes there is nothing to submit. The wrapper may delete the handoff,
// but the verdict remains unresolved and visible rather than stranded behind
// an in-flight receipt.
func TestUnsuccessfulRootSubmitCannotConsumeAChangesRequestedVerdict(t *testing.T) {
	a := newAgentAcceptance(t, `mkdir -p "$FACTORYD_WORKDIR/src" && printf 'fixed\n' > "$FACTORYD_WORKDIR/src/a.go"
printf 'producer/fix\n' > "$FACTORYD_WORKDIR/.producer-branch"
printf 'fix: gate red\n\nbody\n' > "$FACTORYD_WORKDIR/.producer-commit-msg"
touch "$FACTORYD_PROGRESS"`)
	a.gate.exit = 1
	a.verdictFor(t, "48", "producer/fix")
	if err := a.runFor(t, 1, 4*time.Second); err != nil {
		t.Fatal(err)
	}
	st := mustLoad(t, a.cfg)
	iv := st.Issued["48"]
	if iv.ConsumedAt != nil || iv.PendingSubmission != nil {
		t.Fatalf("unsuccessful root submit recorded a verdict receipt: %+v", iv)
	}
	if b := st.Role(state.RoleProducer).Blocked; b == nil || !strings.Contains(b.Reason, "disappeared from the producer-writable outbox") {
		t.Fatalf("unsuccessful root submit left no visible unresolved verdict: %+v", b)
	}
}

// Admission verifies and decodes its handoff bytes before BeforeTurn runs.
// Replacing the producer-writable path while the refresh hook is running must
// not alter the family passed to the model.
func TestVerifiedVerdictSnapshotSurvivesBeforeTurnReplacement(t *testing.T) {
	a := newAgentAcceptance(t, `exit 0`)
	vp := a.verdictFor(t, "48", "producer/fix")
	a.beforeTurn = func(context.Context, supervise.Turn) (string, error) {
		replacement := state.Verdict{ChangeID: "48", Kind: state.VerdictChangesRequested, SHA: "other", Summary: "divert", At: time.Now(),
			Branch: "producer/evil-0123456789", DeclaredBranch: "producer/evil"}
		body, err := json.Marshal(replacement)
		if err != nil {
			return "", err
		}
		return "", os.WriteFile(vp, append(body, '\n'), 0o644)
	}
	if err := a.runFor(t, 1, 4*time.Second); err != nil {
		t.Fatal(err)
	}
	prompt := a.lastPrompt(t)
	if !strings.Contains(prompt, "verbatim:\n    producer/fix\n") {
		t.Fatalf("turn did not receive the verified verdict family:\n%s", prompt)
	}
	if strings.Contains(prompt, "producer/evil") {
		t.Fatalf("turn received the replacement's family instead of the verified snapshot:\n%s", prompt)
	}
}

// A state written before the registry existed cannot identify the outbox bytes
// it already contains. The producer must leave the evidence in place, record
// an operator-visible migration block, and run no turn until an explicit
// migration archives the files for reissue.
func TestLegacyVerdictRegistryBlocksWithoutSilentQuarantine(t *testing.T) {
	a := newAgentAcceptance(t, `exit 0`)
	legacyState := fmt.Sprintf(`{"schema_version":1,"factory":%q}`, a.cfg.Name)
	if err := os.WriteFile(a.cfg.StatePath(), []byte(legacyState), 0o644); err != nil {
		t.Fatal(err)
	}
	vp := filepath.Join(a.cfg.OutboxDir(), "48.json")
	body := []byte(`{"change_id":"48","kind":"changes-requested","summary":"legacy","branch":"producer/fix-0123456789","declared_branch":"producer/fix"}` + "\n")
	if err := os.WriteFile(vp, body, 0o644); err != nil {
		t.Fatal(err)
	}
	err := a.runFor(t, 1, 4*time.Second)
	if !errors.Is(err, state.ErrVerdictRegistryMigrationRequired) {
		t.Fatalf("Run error = %v, want migration-required", err)
	}
	if got := a.producerTurns(t); got != 0 {
		t.Fatalf("legacy verdict ran %d producer turns", got)
	}
	got, err := os.ReadFile(vp)
	if err != nil || string(got) != string(body) {
		t.Fatalf("legacy handoff was altered before explicit migration: %q, %v", got, err)
	}
	st := mustLoad(t, a.cfg)
	if st.VerdictRegistry == nil || st.VerdictRegistry.Status != state.VerdictRegistryMigrationRequired {
		t.Fatalf("migration block was not persisted: %+v", st.VerdictRegistry)
	}
	if _, err := signal.Issue(a.cfg, state.Verdict{ChangeID: "48", Kind: state.VerdictChangesRequested, Summary: "current", At: time.Now(), Branch: "producer/fix-0123456789", DeclaredBranch: "producer/fix"}); !errors.Is(err, state.ErrVerdictRegistryMigrationRequired) {
		t.Fatalf("signal Issue bypassed registry migration: %v", err)
	}
	moved, err := state.MigrateVerdictRegistry(a.cfg.StatePath(), a.cfg.Name, a.cfg.OutboxDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 1 || moved[0] != vp+".legacy-untrusted" {
		t.Fatalf("moved=%v, want the explicit legacy quarantine", moved)
	}
	st = mustLoad(t, a.cfg)
	if !st.VerdictsReady() {
		t.Fatalf("registry was not made ready by explicit migration: %+v", st.VerdictRegistry)
	}
	if _, err := os.Stat(vp); !os.IsNotExist(err) {
		t.Fatal("legacy verdict remained live after explicit migration")
	}
	if _, err := signal.Issue(a.cfg, state.Verdict{ChangeID: "48", Kind: state.VerdictChangesRequested, Summary: "current", At: time.Now(), Branch: "producer/fix-0123456789", DeclaredBranch: "producer/fix"}); err != nil {
		t.Fatalf("a current verdict could not be reissued after migration: %v", err)
	}
}
