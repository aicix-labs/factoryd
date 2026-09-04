package status_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
		SchemaVersion: config.SchemaVersion, Name: "widgets", Provider: "github",
		GitHub: &config.GitHub{Owner: "acme", Repo: "widgets"}, TargetBranch: "main",
		Paths:  config.Paths{Root: root, ProducerWorkdir: filepath.Join(root, "work"), SubmitRepo: filepath.Join(root, "submit")},
		Health: config.DefaultHealth(),
	}
	l.deps = status.Deps{
		Alive: func(r proc.Ref) (bool, error) { return l.alive[r.PID], nil },
		Tree: func(pid int) (*proc.Node, error) {
			return &proc.Node{PID: pid, Command: "factoryd supervise", Children: []*proc.Node{{PID: pid + 1, Command: "claude -p"}}}, nil
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
		"dead supervisor": {func(l *lab, t *testing.T) { l.alive[100] = false }, "producer supervisor pid 100 (start token t) is dead", false},
		"halted": {func(l *lab, t *testing.T) {
			l.state(t, func(s *state.State) { r := s.Role(state.RoleReviewer); r.Halted, r.HaltReason = true, "spin abort" })
		}, "reviewer supervisor halted: spin abort", false},
		"stop sentinel": {func(l *lab, t *testing.T) { os.WriteFile(l.cfg.StopPath("producer"), []byte("x"), 0o644) }, "stop sentinel", false},
		"never supervised": {func(l *lab, t *testing.T) {
			l.state(t, func(s *state.State) { s.Role(state.RoleProducer).Supervisor = nil })
		}, "producer has never been supervised", false},
		"operator-gated": {func(l *lab, t *testing.T) {
			l.state(t, func(s *state.State) {
				s.LastVerdict = &state.Verdict{ChangeID: "7", Kind: state.VerdictOperatorGated, Summary: "touches auth"}
			})
		}, "change 7 is operator-gated: touches auth", true},
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
	for _, want := range []string{"NOT WORKING", "reviewer supervisor", "supervisor DEAD", "producer/fix-abc", "factoryd supervise"} {
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
