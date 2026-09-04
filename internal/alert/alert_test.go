package alert_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aicix-labs/factoryd/internal/alert"
	"github.com/aicix-labs/factoryd/internal/config"
)

func sample() alert.Alert {
	return alert.Alert{Factory: "widgets", At: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC), Kind: "supervisor_dead/producer",
		Severity: "alert", Summary: "producer supervisor is not running", Count: 3}
}

func TestFileAppendsOneJSONLinePerAlert(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "alerts.log")
	f := &alert.File{Path: path}
	for i := 0; i < 2; i++ {
		if err := f.Deliver(context.Background(), sample()); err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2:\n%s", len(lines), body)
	}
	var got alert.Alert
	if err := json.Unmarshal([]byte(lines[1]), &got); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}
	if got.Kind != "supervisor_dead/producer" || got.Count != 3 || !got.At.Equal(sample().At) {
		t.Fatalf("round trip lost fields: %+v", got)
	}
}

func TestFileReportsAnUnwritablePath(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f := &alert.File{Path: filepath.Join(blocker, "alerts.log")}
	if err := f.Deliver(context.Background(), sample()); err == nil {
		t.Fatal("delivered into a path under a regular file")
	}
}

func TestCommandReceivesTheAlertOnStdin(t *testing.T) {
	out := filepath.Join(t.TempDir(), "got.json")
	c := &alert.Command{Argv: []string{"sh", "-c", "cat > " + out}, Env: map[string]string{"PATH": "/usr/bin:/bin"}, Timeout: 5 * time.Second}
	if err := c.Deliver(context.Background(), sample()); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got alert.Alert
	if err := json.Unmarshal(body, &got); err != nil || got.Summary != sample().Summary {
		t.Fatalf("stdin was not the alert: %v %s", err, body)
	}
}

func TestCommandNonZeroExitIsNotDeliveryAndCarriesStderr(t *testing.T) {
	c := &alert.Command{Argv: []string{"sh", "-c", "echo pager is down >&2; exit 7"}, Env: map[string]string{"PATH": "/usr/bin:/bin"}, Timeout: 5 * time.Second}
	err := c.Deliver(context.Background(), sample())
	if err == nil {
		t.Fatal("exit 7 counted as delivered")
	}
	if !strings.Contains(err.Error(), "pager is down") {
		t.Fatalf("stderr not carried: %v", err)
	}
}

func TestCommandTimesOut(t *testing.T) {
	c := &alert.Command{Argv: []string{"sleep", "5"}, Env: map[string]string{"PATH": "/usr/bin:/bin"}, Timeout: 200 * time.Millisecond}
	start := time.Now()
	err := c.Deliver(context.Background(), sample())
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err=%v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("the timeout did not bound the command")
	}
}

func TestCommandResolvesAgainstDeclaredPathOnly(t *testing.T) {
	c := &alert.Command{Argv: []string{"sh", "-c", "true"}, Env: map[string]string{"PATH": t.TempDir()}, Timeout: 5 * time.Second}
	if err := c.Deliver(context.Background(), sample()); err == nil {
		t.Fatal("sh resolved from somewhere other than the declared PATH")
	}
}

type fake struct {
	name string
	err  error
	got  int
}

func (f *fake) Name() string                               { return f.name }
func (f *fake) Deliver(context.Context, alert.Alert) error { f.got++; return f.err }

// One failing transport must not suppress the others, and the failure must
// be named, not folded away.
func TestFanoutIsIndependentAndNamesFailures(t *testing.T) {
	a := &fake{name: "a"}
	b := &fake{name: "b", err: errors.New("b is down")}
	c := &fake{name: "c"}
	f := &alert.Fanout{Transports: []alert.Transport{a, b, c}}
	ds, err := f.Deliver(context.Background(), sample())
	if err != nil {
		t.Fatalf("a partial failure is not a total one: %v", err)
	}
	if a.got != 1 || b.got != 1 || c.got != 1 {
		t.Fatalf("deliveries a=%d b=%d c=%d; b's failure suppressed a later transport", a.got, b.got, c.got)
	}
	if len(ds) != 3 || ds[1].Transport != "b" || ds[1].Err == nil || ds[0].Err != nil || ds[2].Err != nil {
		t.Fatalf("deliveries misreported: %+v", ds)
	}
}

func TestFanoutAllFailedIsAnError(t *testing.T) {
	f := &alert.Fanout{Transports: []alert.Transport{&fake{name: "a", err: errors.New("x")}, &fake{name: "b", err: errors.New("y")}}}
	_, err := f.Deliver(context.Background(), sample())
	if err == nil || !strings.Contains(err.Error(), "a: x") || !strings.Contains(err.Error(), "b: y") {
		t.Fatalf("err=%v; an alert that reached nobody must say so, naming each transport", err)
	}
}

func TestNewRefusesAKindItCannotDeliver(t *testing.T) {
	cfg := &config.Config{Alerts: []config.Alert{{Kind: "webhook"}}}
	if _, err := alert.New(cfg); err == nil {
		t.Fatal("a transport with no implementation was built")
	}
	if _, err := alert.New(&config.Config{}); err == nil {
		t.Fatal("no transports was accepted")
	}
}

// A sink that appends stdin to a file must get one alert per line, the same
// shape the file transport writes. Found live: two alerts through
// `cat >> log` read as zero lines.
func TestCommandBodyIsNewlineTerminated(t *testing.T) {
	out := filepath.Join(t.TempDir(), "sink.log")
	c := &alert.Command{Argv: []string{"sh", "-c", "cat >> " + out}, Env: map[string]string{"PATH": "/usr/bin:/bin"}, Timeout: 5 * time.Second}
	for i := 0; i < 2; i++ {
		if err := c.Deliver(context.Background(), sample()); err != nil {
			t.Fatal(err)
		}
	}
	body, _ := os.ReadFile(out)
	if n := strings.Count(string(body), "\n"); n != 2 {
		t.Fatalf("%d newline(s) for 2 alerts:\n%s", n, body)
	}
}
