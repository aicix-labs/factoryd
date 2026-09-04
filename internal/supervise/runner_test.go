package supervise_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/proc"
	"github.com/aicix-labs/factoryd/internal/supervise"
	"github.com/aicix-labs/factoryd/internal/watch"
)

// The fake runner in supervise_test.go exercises the loop. These tests exercise
// the runner the loop actually uses in production, which the fake never touches.

func execFixture(t *testing.T, argv []string, timeoutSeconds int) (*fixture, *supervise.ExecRunner, *bytes.Buffer) {
	t.Helper()
	fx := newFixture(t)
	spec := fx.cfg.Roles.Reviewer
	spec.Command = argv
	spec.TimeoutSeconds = timeoutSeconds
	fx.cfg.Roles.Reviewer = spec

	var out bytes.Buffer
	return fx, &supervise.ExecRunner{
		Config: fx.cfg, Role: "reviewer", Stdout: &out, Stderr: &out,
	}, &out
}

func turn(fx *fixture, deadline time.Duration) supervise.Turn {
	t := supervise.Turn{
		ID:   "t1",
		Role: "reviewer",
		Triggers: []watch.Trigger{
			{Label: "wake", Path: filepath.Join(fx.inbox, "wake")},
		},
	}
	if deadline > 0 {
		t.Deadline = time.Now().Add(deadline)
	}
	return t
}

func TestExecRunnerReportsExitCode(t *testing.T) {
	for _, c := range []struct {
		name string
		argv []string
		want int
	}{
		{"success", []string{"sh", "-c", "exit 0"}, 0},
		{"failure", []string{"sh", "-c", "exit 7"}, 7},
	} {
		t.Run(c.name, func(t *testing.T) {
			fx, r, _ := execFixture(t, c.argv, 30)
			res, err := r.Run(context.Background(), turn(fx, 0), nil)
			// A non-zero exit is an ordinary outcome for an agent turn. If it
			// came back as an error, the spin guard would never get to judge it.
			if err != nil {
				t.Fatalf("exit %d surfaced as an error: %v", c.want, err)
			}
			if res.ExitCode != c.want {
				t.Fatalf("exit code = %d, want %d", res.ExitCode, c.want)
			}
			if res.TimedOut {
				t.Fatal("a turn that finished reported TimedOut")
			}
		})
	}
}

// A turn that overruns must be distinguishable from one that failed. Both
// surface as a non-zero exit, and they need different responses.
func TestExecRunnerTimeoutIsDistinctFromFailure(t *testing.T) {
	fx, r, _ := execFixture(t, []string{"sleep", "60"}, 30)

	start := time.Now()
	res, err := r.Run(context.Background(), turn(fx, 150*time.Millisecond), nil)
	if err != nil {
		t.Fatalf("timeout surfaced as an error: %v", err)
	}
	if !res.TimedOut {
		t.Fatal("a turn killed at its deadline did not report TimedOut")
	}
	if res.ExitCode == 0 {
		t.Fatal("a killed turn reported exit 0")
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("the deadline took %v to fire", d)
	}
}

// A turn that times out must not leave its children running. Otherwise the next
// turn races a build the previous one started.
func TestExecRunnerKillsTheProcessGroup(t *testing.T) {
	fx, r, _ := execFixture(t, nil, 30)
	marker := filepath.Join(fx.root, "child-still-alive")

	// The child outlives its parent shell unless the whole group is signalled.
	script := "sh -c 'sleep 3; touch " + marker + "' & sleep 60"
	spec := fx.cfg.Roles.Reviewer
	spec.Command = []string{"sh", "-c", script}
	fx.cfg.Roles.Reviewer = spec

	res, err := r.Run(context.Background(), turn(fx, 200*time.Millisecond), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Fatal("the turn did not time out")
	}

	// Well past when the orphan would have fired.
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a child of the timed-out turn survived and kept working")
	}
}

// The started callback is how the supervisor records a real process handle.
// Without it, status would have to guess at the process tree by matching argv.
func TestExecRunnerReportsItsProcess(t *testing.T) {
	fx, r, _ := execFixture(t, []string{"sh", "-c", "sleep 0.2"}, 30)

	var got proc.Ref
	if _, err := r.Run(context.Background(), turn(fx, 0), func(p proc.Ref) { got = p }); err != nil {
		t.Fatal(err)
	}
	if got.PID == 0 {
		t.Fatal("the runner never reported its process")
	}
	if got.StartToken == "" {
		t.Fatal("the reported process has no start token; a recycled PID would look alive")
	}
	if got.PID == os.Getpid() {
		t.Fatal("the runner reported the supervisor's own process as the turn")
	}
}

// A turn is told where everything is, so an agent never has to know the
// factory's layout -- or worse, guess at it.
func TestExecRunnerEnvironment(t *testing.T) {
	fx, r, out := execFixture(t, []string{"sh", "-c", "env | grep '^FACTORYD_'"}, 30)
	fx.cfg.SetPath(filepath.Join(fx.root, "factory.json"))

	if _, err := r.Run(context.Background(), turn(fx, 0), nil); err != nil {
		t.Fatal(err)
	}
	env := out.String()
	for _, want := range []string{
		// The config the turn was started from: a reviewer turn drives
		// factoryd's own verbs and must not be told this by hand (#22).
		"FACTORYD_CONFIG=" + filepath.Join(fx.root, "factory.json"),
		"FACTORYD_FACTORY=widgets",
		"FACTORYD_ROLE=reviewer",
		"FACTORYD_TURN=t1",
		"FACTORYD_ROOT=" + fx.root,
		"FACTORYD_INBOX=" + fx.inbox,
		"FACTORYD_OUTBOX=" + fx.outbox,
		"FACTORYD_TARGET_BRANCH=main",
		"FACTORYD_PROGRESS=" + fx.cfg.ProgressPath("reviewer"),
		"FACTORYD_TRIGGERS=wake",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("the turn was not told %s\ngot:\n%s", want, env)
		}
	}
}

func TestExecRunnerRunsInTheRoleWorkdir(t *testing.T) {
	fx, r, out := execFixture(t, []string{"pwd"}, 30)
	if _, err := r.Run(context.Background(), turn(fx, 0), nil); err != nil {
		t.Fatal(err)
	}
	// The reviewer runs at the factory root; the producer runs in its clone.
	if got := strings.TrimSpace(out.String()); got != fx.root {
		t.Fatalf("the reviewer turn ran in %q, want %q", got, fx.root)
	}

	fx.cfg.Roles.Producer.Command = []string{"pwd"}
	var pout bytes.Buffer
	pr := &supervise.ExecRunner{Config: fx.cfg, Role: "producer", Stdout: &pout, Stderr: &pout}
	if _, err := pr.Run(context.Background(), turn(fx, 0), nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(pout.String()); got != fx.cfg.Paths.ProducerWorkdir {
		t.Fatalf("the producer turn ran in %q, want the clone %q", got, fx.cfg.Paths.ProducerWorkdir)
	}
}

func TestExecRunnerRejectsAnUnrunnableTurn(t *testing.T) {
	fx, r, _ := execFixture(t, []string{"definitely-not-a-real-binary-xyz"}, 30)
	if _, err := r.Run(context.Background(), turn(fx, 0), nil); err == nil {
		t.Fatal("a command that does not exist ran without error")
	}

	fx2, r2, _ := execFixture(t, nil, 30)
	spec := fx2.cfg.Roles.Reviewer
	spec.Command = nil
	fx2.cfg.Roles.Reviewer = spec
	if _, err := r2.Run(context.Background(), turn(fx2, 0), nil); err == nil {
		t.Fatal("an empty command ran without error")
	}
}

// A producer turn runs as its own OS identity, by the same mechanism doctor's
// write probe uses. When factoryd cannot switch to that identity, the turn must
// NOT start as factoryd instead -- that would be the two-party separation on
// paper with nothing holding it up. It fails to start, and says why.
func TestProducerTurnRefusesToStartAsTheWrongIdentity(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can switch to any user; this test needs an unprivileged process")
	}
	fx, _, _ := execFixture(t, []string{"true"}, 30)
	fx.cfg.Roles.Producer.Command = []string{"true"}
	fx.cfg.Roles.Producer.RunAs = &config.RunAs{User: "root"} // cannot switch to it unprivileged
	r := &supervise.ExecRunner{Config: fx.cfg, Role: "producer", Stdout: io.Discard, Stderr: io.Discard}

	res, err := r.Run(context.Background(), turn(fx, 0), nil)
	if err == nil {
		t.Fatalf("the producer turn started (exit %d) despite factoryd being unable to switch to its identity", res.ExitCode)
	}
	if !strings.Contains(err.Error(), "operation not permitted") && !strings.Contains(err.Error(), "run_as") {
		t.Fatalf("the failure does not say why: %v", err)
	}

	// And a user that does not exist is named, not reduced to a number.
	fx.cfg.Roles.Producer.RunAs = &config.RunAs{User: "no-such-factoryd-user"}
	if _, err := r.Run(context.Background(), turn(fx, 0), nil); err == nil || !strings.Contains(err.Error(), "no-such-factoryd-user") {
		t.Fatalf("a missing run_as user was not named: %v", err)
	}
}

// The producer's environment is constructed. A reviewer credential referenced
// by credentials.reviewer.env lives in the supervisor's environment, and
// passing that environment through would put the two-party model one getenv
// away from gone. The producer must not see it; the reviewer must receive it
// by name -- the control that proves the mechanism delivers anything at all.
func TestProducerEnvironmentCarriesNoReviewerCredential(t *testing.T) {
	const sentinel = "REVIEWER_TOKEN_SENTINEL_9f3c1a"
	t.Setenv("FACTORYD_TEST_REVIEWER_TOKEN", sentinel)
	t.Setenv("UNRELATED_AMBIENT", "ambient-value")

	fx, _, out := execFixture(t, []string{"sh", "-c", "env"}, 30)
	fx.cfg.Credentials.Reviewer = config.CredentialRef{Env: "FACTORYD_TEST_REVIEWER_TOKEN"}
	fx.cfg.Roles.Producer.Command = []string{"sh", "-c", "env"}
	fx.cfg.Roles.Producer.RunAs = &config.RunAs{User: currentUser(t)}

	pr := &supervise.ExecRunner{Config: fx.cfg, Role: "producer", Stdout: out, Stderr: out}
	if _, err := pr.Run(context.Background(), turn(fx, 0), nil); err != nil {
		t.Fatal(err)
	}
	env := out.String()
	if strings.Contains(env, sentinel) {
		t.Fatalf("the reviewer credential reached the producer's environment:\n%s", env)
	}
	if strings.Contains(env, "ambient-value") {
		t.Fatalf("an ambient variable reached the producer's environment; it is not constructed:\n%s", env)
	}
	if !strings.Contains(env, "FACTORYD_ROLE=producer") {
		t.Fatalf("the constructed environment is missing FACTORYD_ROLE:\n%s", env)
	}

	// Control: the reviewer DOES receive it, by name, and still nothing else.
	out.Reset()
	rr := &supervise.ExecRunner{Config: fx.cfg, Role: "reviewer", Stdout: out, Stderr: out}
	if _, err := rr.Run(context.Background(), turn(fx, 0), nil); err != nil {
		t.Fatal(err)
	}
	env = out.String()
	if !strings.Contains(env, "FACTORYD_TEST_REVIEWER_TOKEN="+sentinel) {
		t.Fatalf("the reviewer did not receive its own credential; the mechanism delivers nothing:\n%s", env)
	}
	if strings.Contains(env, "ambient-value") {
		t.Fatalf("an ambient variable reached the reviewer's environment:\n%s", env)
	}
}

// A command is resolved against the turn's declared PATH, not the process's.
func TestTurnCommandResolvesAgainstDeclaredPath(t *testing.T) {
	fx, r, _ := execFixture(t, []string{"true"}, 30)
	spec := fx.cfg.Roles.Reviewer
	spec.Env = map[string]string{"PATH": t.TempDir()} // declared PATH with nothing on it
	fx.cfg.Roles.Reviewer = spec
	_, err := r.Run(context.Background(), turn(fx, 0), nil)
	if err == nil {
		t.Fatal("the turn ran a command its declared PATH cannot find; it was resolved against the supervisor's PATH")
	}
	if !strings.Contains(err.Error(), "declared PATH") {
		t.Fatalf("the failure does not say the declared PATH is the problem: %v", err)
	}
}

// The credential a run_as turn is started with must drop the parent's
// supplementary groups. NoSetGroups keeps them -- and factoryd's are root's --
// so a producer turn would still be in group root and a 775 root:root .git is
// writable by it. This pins the shape; the live doctor probe is what found it.
func TestRunAsCredentialDropsSupplementaryGroups(t *testing.T) {
	cred, err := supervise.CredentialFor(currentUser(t))
	if err != nil {
		t.Fatal(err)
	}
	if cred.NoSetGroups {
		t.Fatal("NoSetGroups is set; the turn would inherit factoryd's supplementary groups")
	}
	if cred.Groups == nil {
		t.Fatal("Groups is nil; with NoSetGroups false a nil list still means \"do not call setgroups\" on some paths -- it must be explicitly empty")
	}
	if len(cred.Groups) != 0 {
		t.Fatalf("Groups = %v, want none", cred.Groups)
	}
}

// A turn that asks for no_network must not start connected when factoryd
// cannot create the namespace. Unprivileged, that is every test run.
func TestNoNetworkRefusesToStartConnectedWithoutRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; the refusal path is for the unprivileged case")
	}
	f, r, _ := execFixture(t, []string{"true"}, 5)
	f.cfg.Roles.Reviewer.Sandbox = &config.Sandbox{NoNetwork: true}
	_, err := r.Run(context.Background(), supervise.Turn{ID: "t", Role: "reviewer"}, nil)
	if err == nil || !errors.Is(err, supervise.ErrNoNetworkNeedsRoot) {
		t.Fatalf("err=%v; the turn must not start without the sandbox it was promised", err)
	}
}

// A leader that exits clean while a child it started is still running is
// not a clean turn: the child is killed and the result says so. Two ways a
// child can linger, each detected by its own path: holding the leader's
// stdio (the wait delay expires) and running silently with stdio closed
// (only the process-group reaper sees it).
func TestLeftoverChildIsKilledAndReported(t *testing.T) {
	for name, script := range map[string]string{
		"child holds stdio":  "sleep 30 & exit 0",
		"child closes stdio": "sleep 30 >/dev/null 2>&1 </dev/null & exit 0",
	} {
		t.Run(name, func(t *testing.T) {
			_, r, _ := execFixture(t, []string{"sh", "-c", script}, 10)
			start := time.Now()
			res, err := r.Run(context.Background(), supervise.Turn{ID: "t", Role: "reviewer"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.ExitCode != 0 || !res.Leftover {
				t.Fatalf("res=%+v after %s; the leader exited 0 and left a child, which must be reported", res, time.Since(start))
			}
		})
	}
	_, r2, _ := execFixture(t, []string{"true"}, 10)
	res, err := r2.Run(context.Background(), supervise.Turn{ID: "t2", Role: "reviewer"}, nil)
	if err != nil || res.Leftover {
		t.Fatalf("res=%+v err=%v; a quiescent turn is not leftover", res, err)
	}
}

// The runner's generated keys and config.GeneratedTurnKeys are one list: a
// key added to the runner and not to the list is a key a credential could
// be named after; one on the list and not set by the runner is a promise
// not kept.
func TestGeneratedTurnKeysMatchTheRunner(t *testing.T) {
	fx, r, out := execFixture(t, []string{"sh", "-c", "env | grep '^FACTORYD_' | cut -d= -f1"}, 30)
	fx.cfg.SetPath(filepath.Join(fx.root, "factory.json"))
	if _, err := r.Run(context.Background(), turn(fx, 0), nil); err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, k := range strings.Fields(out.String()) {
		set[k] = true
	}
	for _, k := range config.GeneratedTurnKeys {
		if !set[k] {
			t.Errorf("config.GeneratedTurnKeys lists %s but the runner did not set it", k)
		}
		delete(set, k)
	}
	for k := range set {
		t.Errorf("the runner set %s, which config.GeneratedTurnKeys does not list; a credential could be named after it", k)
	}
}
