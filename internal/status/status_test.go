package status_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/health"
	"github.com/aicix-labs/factoryd/internal/proc"
	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/state"
	"github.com/aicix-labs/factoryd/internal/status"
)

type lab struct {
	cfg     *config.Config
	root    string
	now     time.Time
	alive   map[int]bool
	open    []scm.Change
	lists   int
	listErr error
	deps    status.Deps
}

func newLab(t *testing.T) *lab {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"work", "submit", "inbox", "outbox"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	l := &lab{root: root, now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC), alive: map[int]bool{100: true, 200: true}}
	l.cfg = &config.Config{
		SchemaVersion: config.SchemaVersion, Scope: config.EmptyScope(), Name: "widgets", Provider: "github",
		GitHub: &config.GitHub{Owner: "acme", Repo: "widgets"}, TargetBranch: "main",
		Paths:  config.Paths{Root: root, ProducerWorkdir: filepath.Join(root, "work"), SubmitRepo: filepath.Join(root, "submit")},
		Health: config.DefaultHealth(),
	}
	l.deps = status.Deps{
		Alive: func(r proc.Ref) (bool, error) { return l.alive[r.PID], nil },
		Tree: func(pid int) (*proc.Node, error) {
			return &proc.Node{PID: pid, Children: []*proc.Node{{PID: pid + 1}}}, nil
		},
		Now: func() time.Time { return l.now },
		ListOpen: func(context.Context) ([]scm.Change, error) {
			l.lists++
			return l.open, l.listErr
		},
		ChangesTTL: time.Minute,
	}
	// A healthy factory: both supervisors registered and alive, a fresh
	// clean health document, one open draft.
	l.state(t, func(s *state.State) {
		s.Role(state.RoleProducer).Supervisor = &proc.Ref{PID: 100, StartToken: "t"}
		s.Role(state.RoleReviewer).Supervisor = &proc.Ref{PID: 200, StartToken: "t"}
		s.Role(state.RoleProducer).WatchMode = "inotify"
		s.Role(state.RoleReviewer).WatchMode = "inotify"
	})
	l.health(t, health.Report{Factory: "widgets", At: l.now.Add(-30 * time.Second), Healthy: true,
		Volumes: []health.Volume{{Path: root, FreePercent: 42}}})
	l.open = []scm.Change{{ID: "7", SourceBranch: "producer/fix-abc", TargetBranch: "main", Draft: true, Title: "fix", Author: "producer-bot", UpdatedAt: l.now}}
	return l
}

func (l *lab) state(t *testing.T, fn func(*state.State)) {
	t.Helper()
	if _, err := state.Update(l.cfg.StatePath(), l.cfg.Name, func(s *state.State) error { fn(s); return nil }); err != nil {
		t.Fatal(err)
	}
}

func (l *lab) health(t *testing.T, rep health.Report) {
	t.Helper()
	b, _ := json.Marshal(rep)
	if err := os.WriteFile(l.cfg.HealthPath(), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (l *lab) collect() status.Snapshot {
	return status.New(l.cfg, l.deps).Collect(context.Background())
}

// Positive control: a healthy factory reads as working, with nothing that
// needs the operator. Every negative case rests on this.
func TestHealthyFactoryIsWorking(t *testing.T) {
	l := newLab(t)
	s := l.collect()
	if !s.Working || len(s.NeedsMe) != 0 || len(s.Errors) != 0 {
		t.Fatalf("working=%v needs=%v errors=%v", s.Working, s.NeedsMe, s.Errors)
	}
	if s.Roles["producer"].Tree == nil || len(s.Roles["producer"].Tree.Children) != 1 {
		t.Fatal("the real process tree is not shown")
	}
	if len(s.Changes.Open) != 1 || s.Changes.Skipped {
		t.Fatalf("changes=%+v", s.Changes)
	}
}

func TestStatusNamesAnIntentionallyEmptyBriefQueue(t *testing.T) {
	l := newLab(t)
	l.health(t, health.Report{Factory: "widgets", At: l.now.Add(-30 * time.Second), Healthy: true,
		BriefQueue: &health.BriefQueueReport{Empty: true}, Volumes: []health.Volume{{Path: l.root, FreePercent: 42}}})
	s := l.collect()
	if !s.Working {
		t.Fatalf("an empty queue made the factory not working: %+v", s)
	}
	if !strings.Contains(status.Text(s), "brief queue empty; waiting for work") {
		t.Fatalf("status did not name intentional idle:\n%s", status.Text(s))
	}
}

type fileState struct {
	content string
	mtime   time.Time
}

func snapshotFiles(t *testing.T, root string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		fi, _ := d.Info()
		out[p] = fileState{string(b), fi.ModTime()}
		return nil
	})
	return out
}

// Read-only is the contract (§8): status must never start, stop, or
// consume anything. Every file under the factory root is byte- and
// mtime-identical after collecting and after serving both endpoints, and
// the provider saw nothing but a listing.
func TestStatusIsReadOnly(t *testing.T) {
	l := newLab(t)
	// Triggers, a question, a stop sentinel: all things something might be
	// tempted to consume or clear.
	for _, f := range []string{"inbox/wake", "inbox/brief.md", "inbox/question.md", "outbox/answer.md"} {
		if err := os.WriteFile(filepath.Join(l.root, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(l.cfg.StopPath("producer"), []byte("reason: spin abort\n"), 0o644)
	before := snapshotFiles(t, l.root)

	srv, err := status.NewServer([]*status.Collector{status.New(l.cfg, l.deps)})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for _, path := range []string{"/", "/status.json"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: %d", path, resp.StatusCode)
		}
	}
	resp, _ := http.Post(ts.URL+"/", "text/plain", strings.NewReader("consume"))
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST accepted with %d; status must have no control surface", resp.StatusCode)
	}
	resp.Body.Close()
	l.collect()

	after := snapshotFiles(t, l.root)
	if len(after) != len(before) {
		t.Fatalf("file set changed: %d -> %d", len(before), len(after))
	}
	for p, b := range before {
		a, ok := after[p]
		if !ok {
			t.Fatalf("%s disappeared", p)
		}
		if a != b {
			t.Fatalf("%s changed: %+v -> %+v", p, b, a)
		}
	}
}

func TestNeedsMeNamesEachCondition(t *testing.T) {
	// "Needs me" and "working" are separate questions (§8). A factory waiting
	// on the operator by design -- a gated change, a question -- is working;
	// a provider status cannot reach says nothing about the factory. Each
	// case states which.
	cases := map[string]struct {
		mutate       func(l *lab, t *testing.T)
		want         string
		stillWorking bool
	}{
		"dead supervisor": {func(l *lab, t *testing.T) { l.alive[100] = false }, "producer supervisor pid 100 is dead", false},
		"halted, sentinel already gone": {func(l *lab, t *testing.T) {
			l.state(t, func(s *state.State) { r := s.Role(state.RoleReviewer); r.Halted, r.HaltReason = true, "spin abort" })
		}, "sentinel already cleared; restart to resume", false},
		"halted, sentinel present": {func(l *lab, t *testing.T) {
			l.state(t, func(s *state.State) { r := s.Role(state.RoleReviewer); r.Halted, r.HaltReason = true, "spin abort" })
			os.WriteFile(l.cfg.StopPath("reviewer"), []byte("reason: spin abort\n"), 0o644)
		}, "remove " + "/", false},
		"stop sentinel": {func(l *lab, t *testing.T) { os.WriteFile(l.cfg.StopPath("producer"), []byte("x"), 0o644) }, "stop sentinel", false},
		"never supervised": {func(l *lab, t *testing.T) {
			l.state(t, func(s *state.State) { s.Role(state.RoleProducer).Supervisor = nil })
		}, "producer has never been supervised", false},
		"operator-gated": {func(l *lab, t *testing.T) {
			l.state(t, func(s *state.State) {
				s.LastVerdict = &state.Verdict{ChangeID: "7", Kind: state.VerdictOperatorGated, Summary: "touches auth"}
			})
		}, "change 7 is operator-gated: touches auth", true},
		"active operator gate survives another changes merge": {func(l *lab, t *testing.T) {
			l.state(t, func(s *state.State) {
				c := s.SetCycle(state.CycleOpen, l.now)
				c.ChangeID, c.Family, c.Digest = "8", "producer/b", "producer/b-0123456789"
				c.ReviewDecision = &state.ReviewDecision{Kind: state.VerdictOperatorGated, Branch: c.Digest, DeclaredBranch: c.Family, SHA: "b8", Summary: "B needs an operator", At: l.now}
				// The global activity feed can legitimately be a later merge for A.
				s.LastVerdict = &state.Verdict{ChangeID: "7", Kind: state.VerdictMerged, Summary: "A landed"}
			})
		}, "change 8 is operator-gated: B needs an operator", true},
		"operator-gated queue continuing": {func(l *lab, t *testing.T) {
			l.state(t, func(s *state.State) {
				s.OperatorGates = []state.OperatorGate{{ChangeID: "7", Branch: "producer/fix-abc", Family: "producer/fix", SHA: "abc", Summary: "CI host issue", GatedAt: l.now}}
			})
		}, "operator-gated change 7 awaits you: CI host issue", true},
		"question waiting": {func(l *lab, t *testing.T) {
			os.WriteFile(filepath.Join(l.cfg.InboxDir(), "question.md"), []byte("?"), 0o644)
		}, "a question is waiting", true},
		"no health document": {func(l *lab, t *testing.T) { os.Remove(l.cfg.HealthPath()) }, "no health document", false},
		"stale health":       {func(l *lab, t *testing.T) { l.health(t, health.Report{At: l.now.Add(-time.Hour), Healthy: true}) }, "the health tick has stopped", false},
		"health finding": {func(l *lab, t *testing.T) {
			l.health(t, health.Report{At: l.now, Healthy: false, Findings: []health.Finding{{Key: "disk_low/x", Summary: "volume x is full"}}})
		}, "health: volume x is full", false},
		"provider unreachable": {func(l *lab, t *testing.T) { l.listErr = errors.New("401") }, "open changes are unknown: 401", true},
		"unreadable state":     {func(l *lab, t *testing.T) { os.WriteFile(l.cfg.StatePath(), []byte("{nope"), 0o644) }, "status could not read: state", false},
		// The one read error with no other not-working signal beside it:
		// the supervisor is alive, and only the tree cannot be read.
		"unreadable process tree": {func(l *lab, t *testing.T) {
			l.deps.Tree = func(int) (*proc.Node, error) { return nil, errors.New("/proc unreadable") }
		}, "status could not read: producer process tree", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			l := newLab(t)
			c.mutate(l, t)
			s := l.collect()
			if s.Working != c.stillWorking {
				t.Fatalf("working=%v, want %v; needs=%v", s.Working, c.stillWorking, s.NeedsMe)
			}
			found := false
			for _, n := range s.NeedsMe {
				if strings.Contains(n, c.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("needs-me %v does not contain %q", s.NeedsMe, c.want)
			}
		})
	}
}

// A provider-confirmed CI wait is work factoryd will resume itself. It must be
// visible, but never masquerade as an operator-owned block or an unhealthy
// factory.
func TestPipelineWaitIsVisibleWithoutBecomingAnOperatorNeed(t *testing.T) {
	l := newLab(t)
	l.state(t, func(s *state.State) {
		s.Role(state.RoleReviewer).PipelineWait = &state.PipelineWait{
			ChangeID: "7", SHA: "abc123", Reason: "ci_must_pass", At: l.now,
			AttemptLimit: 6, Deadline: l.now.Add(time.Hour),
		}
	})

	s := l.collect()
	if !s.Working || s.PipelineWait == nil {
		t.Fatalf("working=%v pipeline_wait=%+v", s.Working, s.PipelineWait)
	}
	if len(s.NeedsMe) != 0 {
		t.Fatalf("pipeline wait became an operator need: %v", s.NeedsMe)
	}
	if text := status.Text(s); !strings.Contains(text, "waiting on CI for 7 at abc123") {
		t.Fatalf("text did not expose the CI wait:\n%s", text)
	}
}

func TestExhaustedPipelineWaitIsAnOperatorNeed(t *testing.T) {
	l := newLab(t)
	l.state(t, func(s *state.State) {
		at := l.now.Add(-time.Hour)
		wait := &state.PipelineWait{ChangeID: "7", SHA: "abc123", Reason: "ci_must_pass", At: at, Attempts: 2, AttemptLimit: 2, Deadline: l.now.Add(-time.Minute)}
		s.Role(state.RoleReviewer).PipelineWait = wait
		if b := s.ExhaustReviewerPipelineWait(l.now); b == nil {
			t.Fatal("did not create an exhausted pipeline block")
		}
	})
	s := l.collect()
	if s.Working || s.PipelineWait == nil || s.PipelineWait.ExhaustedAt == nil {
		t.Fatalf("exhausted wait was not shown as not working: %+v", s)
	}
	if !strings.Contains(strings.Join(s.NeedsMe, "\n"), "CI wait exhausted and is not retried") {
		t.Fatalf("operator is not told that CI retry stopped: %v", s.NeedsMe)
	}
	if text := status.Text(s); !strings.Contains(text, "CI wait for 7 at abc123 exhausted") {
		t.Fatalf("status does not name the exhausted CI wait:\n%s", text)
	}
}

func TestTurnAndPendingAges(t *testing.T) {
	l := newLab(t)
	l.state(t, func(s *state.State) {
		s.Role(state.RoleReviewer).CurrentTurn = &state.Turn{ID: "r-1", Trigger: "wake", StartedAt: l.now.Add(-90 * time.Second)}
		s.Role(state.RoleProducer).SetPending([]state.Pending{{Label: "brief", Path: "/b", FirstSeen: l.now.Add(-2 * time.Hour)}})
	})
	s := l.collect()
	r := s.Roles["reviewer"]
	if r.Turn == nil || !r.Turn.Running || r.Turn.AgeText != "1m30s" {
		t.Fatalf("turn=%+v", r.Turn)
	}
	p := s.Roles["producer"]
	if len(p.Pending) != 1 || p.Pending[0].AgeText != "2h00m" {
		t.Fatalf("pending=%+v", p.Pending)
	}
	if !strings.Contains(status.Text(s), "waiting on brief(2h00m)") {
		t.Fatalf("text:\n%s", status.Text(s))
	}
}

// The provider is asked once per TTL, not once per page load; a failed
// refresh keeps the last good list visible beside the error.
func TestChangesAreCachedAndAFailedRefreshKeepsTheLastGoodList(t *testing.T) {
	l := newLab(t)
	c := status.New(l.cfg, l.deps)
	c.Collect(context.Background())
	c.Collect(context.Background())
	if l.lists != 1 {
		t.Fatalf("provider asked %d times inside the TTL", l.lists)
	}
	l.now = l.now.Add(2 * time.Minute)
	l.listErr = errors.New("502")
	s := c.Collect(context.Background())
	if l.lists != 2 {
		t.Fatalf("provider asked %d times, want a refresh after the TTL", l.lists)
	}
	if s.Changes.Err == "" || len(s.Changes.Open) != 1 || len(s.NeedsMe) == 0 {
		t.Fatalf("changes=%+v needs=%v; the last good list must stay visible, marked, and the operator told", s.Changes, s.NeedsMe)
	}
}

func TestNoProviderAccessIsSaidNotHidden(t *testing.T) {
	l := newLab(t)
	l.deps.ListOpen = nil
	s := l.collect()
	if !s.Changes.Skipped || !strings.Contains(status.Text(s), "not queried") {
		t.Fatalf("changes=%+v", s.Changes)
	}
}

// The JSON endpoint carries the same document the page is rendered from.
func TestJSONEndpointMatchesSnapshot(t *testing.T) {
	l := newLab(t)
	l.alive[200] = false
	srv, _ := status.NewServer([]*status.Collector{status.New(l.cfg, l.deps)})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/status.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got []status.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Working || len(got[0].NeedsMe) == 0 || !strings.Contains(got[0].NeedsMe[0], "reviewer supervisor") {
		t.Fatalf("got %+v", got)
	}
	page, _ := http.Get(ts.URL + "/")
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	for _, want := range []string{"NOT WORKING", "reviewer supervisor", "supervisor DEAD", "producer/fix-abc", "supervisor", "child"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("page lacks %q:\n%s", want, body)
		}
	}
}

func TestServerRefusesNoFactories(t *testing.T) {
	if _, err := status.NewServer(nil); !errors.Is(err, status.ErrNoFactories) {
		t.Fatalf("err=%v", err)
	}
}

// After a failed refresh the throttle must still hold: a reload inside the
// TTL asks the provider nothing. Keyed on the last good list's time it
// would ask on every reload -- precisely while the provider is down.
func TestFailedRefreshDoesNotDisableTheThrottle(t *testing.T) {
	l := newLab(t)
	c := status.New(l.cfg, l.deps)
	c.Collect(context.Background()) // good, lists=1
	l.now = l.now.Add(2 * time.Minute)
	l.listErr = errors.New("502")
	c.Collect(context.Background()) // refresh fails, lists=2
	l.now = l.now.Add(10 * time.Second)
	for i := 0; i < 5; i++ {
		c.Collect(context.Background())
	}
	if l.lists != 2 {
		t.Fatalf("provider asked %d times; reloads inside the TTL after a failure must not ask again", l.lists)
	}
	l.now = l.now.Add(time.Minute)
	c.Collect(context.Background())
	if l.lists != 3 {
		t.Fatalf("provider asked %d times; after the TTL it must be asked again", l.lists)
	}
}

// A health record that exists and cannot be read is not "the tick never
// ran". It is named, it is an error the page shows, and the factory is
// not working.
func TestUnreadableHealthIsNamedNotAbsent(t *testing.T) {
	l := newLab(t)
	if err := os.WriteFile(l.cfg.HealthPath(), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := l.collect()
	if !s.Health.Present || s.Health.Err == "" || s.Working {
		t.Fatalf("health=%+v working=%v", s.Health, s.Working)
	}
	joined := strings.Join(s.NeedsMe, "\n")
	if strings.Contains(joined, "never run here") || !strings.Contains(joined, "could not read: health document") {
		t.Fatalf("needs=%v", s.NeedsMe)
	}
	if !strings.Contains(status.Text(s), "UNREADABLE") {
		t.Fatalf("text:\n%s", status.Text(s))
	}
	if os.Getuid() != 0 {
		if err := os.Chmod(l.cfg.HealthPath(), 0); err != nil {
			t.Fatal(err)
		}
		s = l.collect()
		if !s.Health.Present || !strings.Contains(s.Health.Err, "permission denied") || s.Working {
			t.Fatalf("health=%+v working=%v", s.Health, s.Working)
		}
	}
}

// The page is unauthenticated by design, so nothing that reaches it may
// carry a command line. A secret planted in the supervisor's recorded argv
// and in a live child's arguments must appear in none of HTML, JSON, or
// text; the child is shown by executable name only.
func TestNoProcessSuppliedLabelReachesAnyOutput(t *testing.T) {
	l := newLab(t)
	const secret = "SECRET-TOKEN-7f3a9c"
	// The child controls three channels: its arguments, its comm (writable
	// via /proc/self/comm and prctl), and its executable path (a copy of a
	// binary named after the secret). All three carry the secret here.
	commSecret := "SECRETcomm7f3a" // comm is at most 15 bytes
	bin := filepath.Join(t.TempDir(), secret)
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	b, err := os.ReadFile(sleep)
	if err != nil {
		t.Skip(err)
	}
	if err := os.WriteFile(bin, b, 0o755); err != nil {
		t.Fatal(err)
	}
	// The recorded turn process is a plain sh that stays alive; the
	// secret-bearing process is ITS child, so it is labelled "child" -- the
	// one label that nothing factoryd recorded could override. A label
	// taken from comm or the exe path would surface exactly there.
	pidFile := filepath.Join(t.TempDir(), "pid")
	// An inner sh -c has its own $$; the argv secret rides as a trailing
	// comment inside the inner script, where it cannot swallow the outer
	// "& wait".
	inner := "printf %s " + commSecret + " > /proc/self/comm; echo $$ > " + pidFile + "; exec " + bin + " 30 # " + secret
	child := exec.Command("sh", "-c", "sh -c '"+inner+"' & wait")
	if err := child.Start(); err != nil {
		t.Skipf("no sh: %v", err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	var grandchild int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			fmt.Sscan(string(b), &grandchild)
		}
		if grandchild != 0 {
			if c, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", grandchild)); strings.Contains(string(c), "SECRET") {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if c, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", grandchild)); grandchild == 0 || !strings.Contains(string(c), "SECRET") {
		t.Skipf("could not make a process's comm carry a secret (pid %d, %q); the negative control cannot run here", grandchild, strings.TrimSpace(string(c)))
	}
	defer func() {
		if p, err := os.FindProcess(grandchild); err == nil {
			p.Kill()
		}
	}()
	self, err := proc.Self("supervise")
	if err != nil {
		t.Fatal(err)
	}
	self.Command = "factoryd supervise --token=" + secret
	l.state(t, func(s *state.State) {
		s.Role(state.RoleProducer).Supervisor = &self
		s.Role(state.RoleProducer).CurrentTurn = &state.Turn{ID: "p-1", StartedAt: l.now, Process: &proc.Ref{PID: child.Process.Pid, Command: "claude --key=" + secret}}
	})
	l.alive[self.PID] = true
	l.deps.Tree = proc.Tree // the real tree, under this test process

	srv, _ := status.NewServer([]*status.Collector{status.New(l.cfg, l.deps)})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	var outputs = map[string]string{}
	for _, path := range []string{"/", "/status.json"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		outputs[path] = string(b)
	}
	outputs["text"] = status.Text(l.collect())
	for name, out := range outputs {
		for _, needle := range []string{secret, commSecret, "SECRET"} {
			if strings.Contains(out, needle) {
				t.Fatalf("%s carries a process-supplied secret %q:\n%s", name, needle, out)
			}
		}
	}
	// Positive control: both ARE shown -- by pid; the recorded turn process
	// as "turn", its secret-bearing child as "child", never as anything the
	// process said about itself.
	j := outputs["/status.json"]
	if !strings.Contains(j, fmt.Sprintf(`"pid": %d`, child.Process.Pid)) || !strings.Contains(j, `"label": "turn"`) ||
		!strings.Contains(j, fmt.Sprintf(`"pid": %d`, grandchild)) || !strings.Contains(j, `"label": "child"`) {
		t.Fatalf("the live processes are not shown by pid with factoryd's own labels:\n%s", j)
	}
}

// The remedy names the sentinel that exists, and only then (#26).
func TestHaltRemedyFollowsTheSentinel(t *testing.T) {
	l := newLab(t)
	l.state(t, func(s *state.State) { r := s.Role(state.RoleReviewer); r.Halted, r.HaltReason = true, "fail_abort" })
	os.WriteFile(l.cfg.StopPath("reviewer"), []byte("x"), 0o644)
	s := l.collect()
	if !strings.Contains(strings.Join(s.NeedsMe, "\n"), "remove "+l.cfg.StopPath("reviewer")+" and restart") {
		t.Fatalf("needs=%v; with the sentinel present the remedy must name it", s.NeedsMe)
	}
	os.Remove(l.cfg.StopPath("reviewer"))
	s = l.collect()
	joined := strings.Join(s.NeedsMe, "\n")
	if strings.Contains(joined, "remove ") || !strings.Contains(joined, "sentinel already cleared; restart to resume") {
		t.Fatalf("needs=%v; with the sentinel gone the remedy must not tell the operator to remove it", s.NeedsMe)
	}
}

// A cleared halt is information, not a need: the factory reads as working
// and the recovery is shown (#30).
func TestClearedHaltIsInformationNotANeed(t *testing.T) {
	l := newLab(t)
	l.state(t, func(s *state.State) {
		s.Role(state.RoleProducer).LastHalt = &state.Halt{Reason: "fail_abort", At: l.now.Add(-time.Hour), ClearedAt: l.now.Add(-30 * time.Minute)}
	})
	s := l.collect()
	if !s.Working || len(s.NeedsMe) != 0 {
		t.Fatalf("working=%v needs=%v; a cleared halt must not read as a halt", s.Working, s.NeedsMe)
	}
	if !strings.Contains(status.Text(s), "recovered from a halt (fail_abort)") {
		t.Fatalf("text:\n%s", status.Text(s))
	}
}

// A leak that was killed is shown as hygiene, never hidden (#33).
func TestLeftoverTurnsAreShownAsHygiene(t *testing.T) {
	l := newLab(t)
	l.state(t, func(s *state.State) { s.Role(state.RoleReviewer).LeftoverTurns = 3 })
	s := l.collect()
	if !s.Working || len(s.NeedsMe) != 0 {
		t.Fatalf("working=%v needs=%v; a killed leak is not a need", s.Working, s.NeedsMe)
	}
	if !strings.Contains(status.Text(s), "3 turn(s) left processes behind") {
		t.Fatalf("text:\n%s", status.Text(s))
	}
}

// A blocked submission is a need, not information (#42).
func TestBlockedSubmissionIsANeed(t *testing.T) {
	l := newLab(t)
	l.state(t, func(s *state.State) {
		s.Role(state.RoleProducer).Blocked = &state.Block{Disposition: "blocked", Reason: "change 53 is no longer a draft", At: l.now}
	})
	s := l.collect()
	if s.Working {
		t.Fatal("a factory with a blocked submission reads as working")
	}
	found := false
	for _, n := range s.NeedsMe {
		if strings.Contains(n, "not retried") && strings.Contains(n, "change 53") {
			found = true
		}
	}
	if !found {
		t.Fatalf("needs=%v", s.NeedsMe)
	}
	if !strings.Contains(status.Text(s), "submission blocked (change 53 is no longer a draft)") {
		t.Fatalf("text:\n%s", status.Text(s))
	}
}

// A "needs you" entry keeps the structure its author gave it (#46): the
// first paragraph -- the verdict and its reason -- is the visible summary,
// the rest is behind a disclosure, newlines survive in a pre-wrap block,
// and provider-authored text stays escaped. A one-paragraph entry is a
// plain block, not a disclosure with nothing behind it.
func TestNeedsYouKeepsParagraphsAndCollapsesTheRest(t *testing.T) {
	l := newLab(t)
	long := "OPERATOR-GATED (scope policy; not fixable by resubmit).\n\nChange 57 adds deploy/x.sh; it matches deny regex ^deploy/ and no allow regex.\nA human must take the script or widen allow_regexes.\n\n<b>not markup</b>"
	l.state(t, func(s *state.State) {
		s.Role(state.RoleProducer).Blocked = &state.Block{Disposition: "blocked", Reason: long, At: l.now}
	})
	srv, _ := status.NewServer([]*status.Collector{status.New(l.cfg, l.deps)})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(body)
	if !strings.Contains(page, `<details class="need"><summary>`) {
		t.Fatalf("a multi-paragraph need is not a disclosure:\n%s", page)
	}
	// The summary is the first paragraph and nothing more.
	sum := page[strings.Index(page, "<summary>")+len("<summary>") : strings.Index(page, "</summary>")]
	if !strings.Contains(sum, "not fixable by resubmit") || strings.Contains(sum, "deny regex") {
		t.Fatalf("summary = %q; want the first paragraph alone", sum)
	}
	// The rest keeps its newline between the two sentences of paragraph 2.
	rest := page[strings.Index(page, `<div class="rest">`)+len(`<div class="rest">`) : strings.Index(page, "</details>")]
	if !strings.Contains(rest, "no allow regex.\nA human must") {
		t.Fatalf("rest lost its line structure: %q", rest)
	}
	if strings.Contains(page, "<b>not markup</b>") || !strings.Contains(page, "&lt;b&gt;not markup&lt;/b&gt;") {
		t.Fatal("provider-authored text was not escaped")
	}
	if !strings.Contains(page, ".need{white-space:pre-wrap") {
		t.Fatal("the need block is not pre-wrap; newlines would collapse")
	}
	if strings.Contains(page, "<li>"+sum) {
		t.Fatal("needs are still list items")
	}

	// One paragraph: a plain block, no disclosure.
	l2 := newLab(t)
	l2.state(t, func(s *state.State) {
		s.Role(state.RoleProducer).Blocked = &state.Block{Disposition: "blocked", Reason: "one line only", At: l2.now}
	})
	srv2, _ := status.NewServer([]*status.Collector{status.New(l2.cfg, l2.deps)})
	ts2 := httptest.NewServer(srv2.Handler())
	defer ts2.Close()
	resp2, _ := http.Get(ts2.URL + "/")
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if strings.Contains(string(body2), "<details") || !strings.Contains(string(body2), `<div class="need">`) {
		t.Fatalf("a one-paragraph need rendered wrong:\n%s", body2)
	}
}
