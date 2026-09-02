package watch_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aicix-labs/factoryd/internal/watch"
)

func dirs(t *testing.T) (root, inbox, outbox string) {
	t.Helper()
	root = t.TempDir()
	inbox = filepath.Join(root, "inbox")
	outbox = filepath.Join(root, "outbox")
	for _, d := range []string{inbox, outbox} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, inbox, outbox
}

func specs(inbox, outbox string) []watch.Spec {
	return []watch.Spec{
		{Label: "wake", Dir: inbox, Pattern: "wake"},
		{Label: "verdict", Dir: outbox, Pattern: "*.json"},
	}
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Both modes must satisfy every behavioural test. Running the whole suite twice
// is the point: the fallback is not a lesser watcher, and a bug that exists only
// in poll mode would otherwise surface on the one host that lacks inotify.
func eachMode(t *testing.T, fn func(t *testing.T, opts watch.Options)) {
	t.Helper()
	t.Run("inotify", func(t *testing.T) {
		fn(t, watch.Options{Interval: 30 * time.Second})
	})
	t.Run("poll", func(t *testing.T) {
		fn(t, watch.Options{Interval: 20 * time.Millisecond, ForcePoll: true})
	})
}

func TestModeIsReported(t *testing.T) {
	_, inbox, outbox := dirs(t)

	w, err := watch.New(specs(inbox, outbox), watch.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if w.Mode() != watch.ModeInotify {
		t.Fatalf("mode = %q on Linux, want inotify (reason: %q)", w.Mode(), w.ModeReason())
	}
	if w.ModeReason() != "" {
		t.Fatalf("inotify mode carries a fallback reason: %q", w.ModeReason())
	}

	// A degraded watcher must say so. Silent degradation is how a factory ends
	// up believing it is event-driven while it is not.
	p, err := watch.New(specs(inbox, outbox), watch.Options{ForcePoll: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.Mode() != watch.ModePoll {
		t.Fatalf("forced mode = %q, want poll", p.Mode())
	}
	if p.ModeReason() == "" {
		t.Fatal("a watcher in poll mode gave no reason for it")
	}
}

// A trigger written before Wait is called must still be seen. A watcher that
// only reported new events would miss it -- that is the two-hour-twenty gap in
// v1, where a signal arrived while nothing was armed.
func TestTriggerWrittenBeforeWaitIsSeen(t *testing.T) {
	eachMode(t, func(t *testing.T, opts watch.Options) {
		_, inbox, outbox := dirs(t)
		touch(t, filepath.Join(inbox, "wake"))

		w, err := watch.New(specs(inbox, outbox), opts)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		got, err := w.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Label != "wake" {
			t.Fatalf("Wait returned %+v, want one wake trigger", got)
		}
	})
}

func TestTriggerWrittenDuringWaitIsSeen(t *testing.T) {
	eachMode(t, func(t *testing.T, opts watch.Options) {
		_, inbox, outbox := dirs(t)
		w, err := watch.New(specs(inbox, outbox), opts)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()

		go func() {
			time.Sleep(30 * time.Millisecond)
			touch(t, filepath.Join(outbox, "42.json"))
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		start := time.Now()
		got, err := w.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Label != "verdict" {
			t.Fatalf("Wait returned %+v, want one verdict trigger", got)
		}
		if filepath.Base(got[0].Path) != "42.json" {
			t.Fatalf("trigger path = %s", got[0].Path)
		}
		elapsed := time.Since(start)
		t.Logf("%s mode saw the trigger in %v", w.Mode(), elapsed)
		// In inotify mode the interval is 30s, so anything near it means the
		// event path did not fire and the periodic re-check carried the test.
		// Without this the suite would pass identically on a watcher whose
		// inotify half was dead.
		if w.Mode() == watch.ModeInotify && elapsed > opts.Interval/2 {
			t.Fatalf("inotify mode took %v with a %v interval; the event path did not fire", elapsed, opts.Interval)
		}
	})
}

// Wait must never remove a trigger. Consuming is the agent turn's job, and the
// spin guard is built on being able to see that a turn did not do it.
func TestWaitDoesNotConsume(t *testing.T) {
	eachMode(t, func(t *testing.T, opts watch.Options) {
		_, inbox, outbox := dirs(t)
		p := filepath.Join(inbox, "wake")
		touch(t, p)

		w, err := watch.New(specs(inbox, outbox), opts)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for i := 0; i < 3; i++ {
			if _, err := w.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("after Wait #%d the trigger is gone: %v", i+1, err)
			}
		}
	})
}

func TestWaitBlocksWhenNothingIsPending(t *testing.T) {
	eachMode(t, func(t *testing.T, opts watch.Options) {
		_, inbox, outbox := dirs(t)
		w, err := watch.New(specs(inbox, outbox), opts)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
		defer cancel()
		got, err := w.Wait(ctx)
		if err == nil {
			t.Fatalf("Wait returned %+v with nothing pending; it must block", got)
		}
		if !os.IsTimeout(err) && err != context.DeadlineExceeded {
			t.Fatalf("Wait returned %v, want a context deadline", err)
		}
	})
}

func TestCancelUnblocksWait(t *testing.T) {
	eachMode(t, func(t *testing.T, opts watch.Options) {
		_, inbox, outbox := dirs(t)
		w, err := watch.New(specs(inbox, outbox), opts)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := w.Wait(ctx); done <- err }()

		time.Sleep(20 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if err != context.Canceled {
				t.Fatalf("Wait returned %v after cancel, want context.Canceled", err)
			}
		case <-time.After(3 * time.Second):
			// An idle supervisor that cannot be stopped is one you end up
			// killing by command-line pattern -- the v1 failure this project
			// exists to remove.
			t.Fatal("Wait did not return within 3s of cancellation")
		}
	})
}

func TestPendingIgnoresNonMatchingFiles(t *testing.T) {
	_, inbox, outbox := dirs(t)
	touch(t, filepath.Join(inbox, "notes.txt"))
	touch(t, filepath.Join(outbox, "answer.md"))
	if err := os.MkdirAll(filepath.Join(outbox, "sub.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	w, err := watch.New(specs(inbox, outbox), watch.Options{ForcePoll: true})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	got, err := w.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("Pending returned %+v, want nothing (a .txt, an .md, and a directory named *.json)", got)
	}

	// Positive control: the same watcher must find a file that does match, or
	// the zero above only proves the watcher never looks.
	touch(t, filepath.Join(outbox, "42.json"))
	got, err = w.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("Pending returned %+v, want the one matching file", got)
	}
}

// A directory that disappears must be an error, not an empty result. Reporting
// "no triggers" for a deleted inbox makes a broken factory look like a quiet one.
func TestVanishedDirectoryIsAnError(t *testing.T) {
	_, inbox, outbox := dirs(t)
	w, err := watch.New(specs(inbox, outbox), watch.Options{ForcePoll: true})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := os.RemoveAll(inbox); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Pending(); err == nil {
		t.Fatal("Pending reported no triggers after its directory was removed")
	}
}

func TestNewRejectsUnusableSpecs(t *testing.T) {
	_, inbox, _ := dirs(t)
	cases := map[string][]watch.Spec{
		"no specs":        {},
		"missing label":   {{Dir: inbox, Pattern: "wake"}},
		"missing pattern": {{Label: "wake", Dir: inbox}},
		"nonexistent dir": {{Label: "wake", Dir: filepath.Join(inbox, "nope"), Pattern: "wake"}},
		"dir is a file":   {{Label: "wake", Dir: filepath.Join(inbox, "f"), Pattern: "wake"}},
		"bad pattern":     {{Label: "wake", Dir: inbox, Pattern: "[a-"}},
	}
	touch(t, filepath.Join(inbox, "f"))
	for name, sp := range cases {
		if _, err := watch.New(sp, watch.Options{}); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
}
