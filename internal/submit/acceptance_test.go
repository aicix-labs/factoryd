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
	turns  int // producer turns the wrapper actually ran (counted by the script)
	marker string
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
	l.cfg.Supervisor = config.Supervisor{SpinWarn: 2, SpinAbort: 4, FailAbort: 3, PollIntervalSeconds: 1, BackoffSeconds: 1, ForcePoll: true}
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
		Runner:    &supervise.ExecRunner{Config: a.cfg, Role: "producer", Stdout: io.Discard, Stderr: io.Discard},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sleep:     func(ctx context.Context, d time.Duration) error { return nil }, // backoffs are instant; the count of turns is the evidence
		MaxTurns:  maxTurns,
		AfterTurn: submit.AfterTurn(a.cfg, func(context.Context) (submit.Deps, error) { return a.deps, nil }),
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
	b, _ := json.Marshal(v)
	p := filepath.Join(a.cfg.OutboxDir(), id+".json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
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
// bounded: after PRODUCER_VERDICT_ATTEMPTS partial turns on the verdict,
// further partial turns are no progress, and the factory halts at
// fail_abort with the verdict kept.
func TestEndlessContentChangesAreBoundedPerVerdict(t *testing.T) {
	a := newAgentAcceptance(t, `n=$(wc -l < "$FACTORYD_ROOT/turns"); mkdir -p "$FACTORYD_WORKDIR/src"
printf 'attempt %s\n' "$n" > "$FACTORYD_WORKDIR/src/churn.go"; touch "$FACTORYD_PROGRESS"; exit 0`)
	a.cfg.Roles.Producer.Env["PRODUCER_VERDICT_ATTEMPTS"] = "2"
	if err := a.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	vp := a.verdictFor(t, "48", "producer/fix")
	err := a.runFor(t, 50, 30*time.Second)
	if !errors.Is(err, supervise.ErrHalted) {
		t.Fatalf("Run returned %v, want ErrHalted (turns=%d): endless content churn was unbounded", err, a.producerTurns(t))
	}
	if got := a.producerTurns(t); got != 2+a.cfg.Supervisor.FailAbort {
		t.Fatalf("turns=%d, want bound=2 partial turns + fail_abort=%d", got, a.cfg.Supervisor.FailAbort)
	}
	if _, err := os.Stat(vp); err != nil {
		t.Fatal("the verdict was lost")
	}
	if len(a.tr.calls) != 0 {
		t.Fatalf("submit ran: %v", a.tr.calls)
	}
}
