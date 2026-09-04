package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/scm"
)

// closeDriver is the provider as the close verb sees it.
type closeDriver struct {
	scm.Driver
	name     string
	state    scm.ChangeState
	ready    bool     // the change is no longer a draft
	closes   []string // reasons passed to Close
	closeErr error
	// after is the state the re-read shows once Close returned: closed by
	// default; open when the provider lied; merged when another party
	// merged in the window the close cannot exclude.
	after scm.ChangeState
}

func (d *closeDriver) Get(context.Context, scm.ChangeID) (scm.Change, error) {
	return scm.Change{ID: "48", State: d.state, Draft: !d.ready}, nil
}

func (d *closeDriver) Close(_ context.Context, _ scm.ChangeID, reason string) error {
	d.closes = append(d.closes, reason)
	if d.closeErr != nil {
		return d.closeErr
	}
	d.state = scm.StateClosed
	if d.after != 0 {
		d.state = d.after
	}
	return nil
}

// close reaches Driver.Close only through the operator principal: the
// role's own driver -- the one the reviewer turn holds -- never receives
// the call, whatever arguments are passed (#36, #47 review). And it
// closes only an open draft, believing only a re-read that says closed.
func TestScmCloseRunsOnlyAsTheOperatorPrincipal(t *testing.T) {
	out := &printer{}
	role := &closeDriver{name: "reviewer", state: scm.StateOpen}
	t.Cleanup(func() {
		if len(role.closes) != 0 {
			t.Errorf("the role's driver received Close(%v); the reviewer closed", role.closes)
		}
	})
	noOperator := func(context.Context) (scm.Driver, error) {
		return nil, errors.New("credentials.operator is not configured")
	}
	// Without an operator principal nothing closes, whatever is passed.
	for _, args := range [][]string{{"48"}, {"--operator", "48"}, {"48", "--operator"}, {"48", "reason"}} {
		if rc := scmVerb(context.Background(), role, noOperator, "close", args, out); rc != exitError {
			t.Fatalf("args %v: rc=%d; without the operator credential close must refuse", args, rc)
		}
	}
	if rc := scmVerb(context.Background(), role, nil, "close", []string{"48"}, out); rc != exitError {
		t.Fatal("no operator seam at all, yet close proceeded")
	}

	op := &closeDriver{name: "operator", state: scm.StateOpen}
	asOperator := func(context.Context) (scm.Driver, error) { return op, nil }
	run := func(o *closeDriver, args ...string) int {
		op = o
		return scmVerb(context.Background(), role, asOperator, "close", args, out)
	}
	if rc := run(op, "48"); rc != exitOK || len(op.closes) != 1 || op.closes[0] != "superseded by a newer submission" {
		t.Fatalf("rc=%d closes=%v", rc, op.closes)
	}
	o := &closeDriver{state: scm.StateOpen}
	if rc := run(o, "48", "duplicate", "of", "50"); rc != exitOK || o.closes[0] != "duplicate of 50" {
		t.Fatalf("rc=%d closes=%v", rc, o.closes)
	}
	for _, st := range []scm.ChangeState{scm.StateMerged, scm.StateClosed} {
		o := &closeDriver{state: st}
		if rc := run(o, "48"); rc != exitError || len(o.closes) != 0 {
			t.Fatalf("%s: rc=%d closes=%v; only an open change is closed", st, rc, o.closes)
		}
	}
	o = &closeDriver{state: scm.StateOpen, ready: true}
	if rc := run(o, "48"); rc != exitError || len(o.closes) != 0 {
		t.Fatalf("ready change: rc=%d closes=%v; a change that left the producer's hands is not a superseded draft", rc, o.closes)
	}
	if rc := run(&closeDriver{state: scm.StateOpen, after: scm.StateOpen}, "48"); rc != exitError {
		t.Fatal("the provider's word was taken as proof the change closed")
	}
	if rc := run(&closeDriver{state: scm.StateOpen, after: scm.StateMerged}, "48"); rc != exitError {
		t.Fatal("a change merged in the window was reported as retired")
	}
	if rc := run(&closeDriver{state: scm.StateOpen, closeErr: errors.New("403")}, "48"); rc != exitError {
		t.Fatal("a failed close exited 0")
	}
	if rc := run(&closeDriver{state: scm.StateOpen}); rc != exitError {
		t.Fatal("close with no id was accepted")
	}
}

// tokenDriver is a provider whose identity is its token: two files with
// the same content are one principal.
type tokenDriver struct {
	scm.Driver
	token  string
	closes *[]string
	closed *bool
}

func (d tokenDriver) Whoami(context.Context) (scm.Identity, error) {
	return scm.Identity{ID: "id-" + d.token, Login: d.token}, nil
}
func (d tokenDriver) Get(context.Context, scm.ChangeID) (scm.Change, error) {
	st := scm.StateOpen
	if *d.closed {
		st = scm.StateClosed
	}
	return scm.Change{ID: "48", State: st, Draft: true}, nil
}
func (d tokenDriver) Close(_ context.Context, _ scm.ChangeID, reason string) error {
	*d.closes = append(*d.closes, d.token+":"+reason)
	*d.closed = true
	return nil
}

// Immediately before a close, the three identities are resolved and
// compared by provider id. An operator file that holds the reviewer's
// token -- path-distinct, one authority -- is refused and nothing is
// closed; a role token that cannot be read is refused, not skipped (#47
// review).
func TestCloseRefusesAnOperatorThatIsTheReviewerByProviderID(t *testing.T) {
	root := t.TempDir()
	write := func(name, tok string) config.CredentialRef {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return config.CredentialRef{File: p}
	}
	var closes []string
	closed := false
	build := func(cfg *config.Config, token string) (scm.Driver, error) {
		return tokenDriver{token: token, closes: &closes, closed: &closed}, nil
	}
	cfg := &config.Config{Credentials: config.Credentials{
		Producer: write("producer.token", "p"), Reviewer: write("reviewer.token", "r"), Operator: write("operator.token", "r")}}
	role := &closeDriver{state: scm.StateOpen}
	out := &printer{}
	if rc := scmVerb(context.Background(), role, operatorPrincipal(cfg, build), "close", []string{"48"}, out); rc != exitError || len(closes) != 0 || len(role.closes) != 0 {
		t.Fatalf("rc=%d closes=%v role=%v; the reviewer's token behind the operator path closed", rc, closes, role.closes)
	}
	// A distinct third identity closes, as the operator.
	cfg.Credentials.Operator = write("operator.token", "o")
	if rc := scmVerb(context.Background(), role, operatorPrincipal(cfg, build), "close", []string{"48"}, out); rc != exitOK || len(closes) != 1 || !strings.HasPrefix(closes[0], "o:") || len(role.closes) != 0 {
		t.Fatalf("rc=%d closes=%v role=%v", rc, closes, role.closes)
	}
	// A producer token this process cannot read is a refusal, never a role
	// left out of the comparison.
	closes, closed = nil, false
	os.Remove(cfg.Credentials.Producer.File)
	if rc := scmVerb(context.Background(), role, operatorPrincipal(cfg, build), "close", []string{"48"}, out); rc != exitError || len(closes) != 0 {
		t.Fatalf("rc=%d closes=%v; an unresolved role was skipped", rc, closes)
	}
}

// The driver that closes is the driver that proved the identity. A
// rotation of operator.token between the proof and the close must not
// close as the new token: the file is rewritten from inside the fake's
// Whoami (the last read before the close), and the close is asserted to
// have run as the token that was validated (#47 review).
func TestCloseRunsAsTheTokenThatWasValidatedNotARereadOfTheFile(t *testing.T) {
	root := t.TempDir()
	write := func(name, tok string) config.CredentialRef {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return config.CredentialRef{File: p}
	}
	cfg := &config.Config{Credentials: config.Credentials{
		Producer: write("producer.token", "p"), Reviewer: write("reviewer.token", "r"), Operator: write("operator.token", "o1")}}
	var closes []string
	closed := false
	build := func(cfg *config.Config, token string) (scm.Driver, error) {
		return rotatingDriver{tokenDriver: tokenDriver{token: token, closes: &closes, closed: &closed}, cfg: cfg, t: t}, nil
	}
	role := &closeDriver{state: scm.StateOpen}
	if rc := scmVerb(context.Background(), role, operatorPrincipal(cfg, build), "close", []string{"48"}, &printer{}); rc != exitOK {
		t.Fatalf("rc=%d closes=%v", rc, closes)
	}
	if len(closes) != 1 || !strings.HasPrefix(closes[0], "o1:") {
		t.Fatalf("closes=%v; the close ran as a token other than the one validated", closes)
	}
	if b, _ := os.ReadFile(cfg.Credentials.Operator.File); strings.TrimSpace(string(b)) != "o2" {
		t.Fatal("the test's rotation did not happen; it proves nothing")
	}
}

// rotatingDriver rewrites operator.token the moment the operator's
// identity is read, which is after validation began and before any close.
type rotatingDriver struct {
	tokenDriver
	cfg *config.Config
	t   *testing.T
}

func (d rotatingDriver) Whoami(ctx context.Context) (scm.Identity, error) {
	if d.token == "o1" {
		if err := os.WriteFile(d.cfg.Credentials.Operator.File, []byte("o2\n"), 0o600); err != nil {
			d.t.Fatal(err)
		}
	}
	return d.tokenDriver.Whoami(ctx)
}
