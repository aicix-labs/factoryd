package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// GitLab computes an MR's diff asynchronously. Observed live: the diffs
// endpoint returned an empty list seconds after the MR was opened while the
// MR reported changes. A driver that reported that as "no changes" would let
// the merge gate classify against nothing.
func settleServer(t *testing.T, emptyFor int32, changesCount string, diffRefs bool) (*Driver, *int32) {
	t.Helper()
	var diffCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/merge_requests/7/diffs"):
			n := atomic.AddInt32(&diffCalls, 1)
			if n <= emptyFor {
				w.Write([]byte(`[]`))
				return
			}
			json.NewEncoder(w).Encode([]map[string]any{
				{"old_path": "auth/a.go", "new_path": "auth/a.go", "diff": "@@ -1 +1 @@\n+x\n"},
				{"old_path": "b.go", "new_path": "b.go", "diff": "@@ -1 +1 @@\n+y\n"},
			})
		case strings.HasSuffix(r.URL.Path, "/merge_requests/7"):
			m := map[string]any{"iid": 7, "state": "opened", "sha": "abc", "changes_count": changesCount,
				"source_branch": "s", "target_branch": "main", "author": map[string]any{"username": "u", "id": 1}}
			if diffRefs {
				m["diff_refs"] = map[string]any{"head_sha": "abc"}
			}
			json.NewEncoder(w).Encode(m)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	d, err := New(Config{BaseURL: srv.URL + "/api/v4", Project: "acme/widgets", Token: "t", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return d, &diffCalls
}

func fastSettle(t *testing.T, attempts int) {
	t.Helper()
	oldA, oldS := diffSettleAttempts, diffSettleSleep
	diffSettleAttempts = attempts
	diffSettleSleep = func(context.Context, int) error { return nil }
	t.Cleanup(func() { diffSettleAttempts, diffSettleSleep = oldA, oldS })
}

func TestDiffWaitsForGitLabToComputeIt(t *testing.T) {
	fastSettle(t, 5)
	d, calls := settleServer(t, 2, "2", true)
	out, err := d.Diff(context.Background(), "7")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Path != "auth/a.go" {
		t.Fatalf("out=%+v", out)
	}
	if *calls != 3 {
		t.Fatalf("diff endpoint called %d times, want 3 (two empty, then ready)", *calls)
	}
}

func TestDiffNeverReadyIsAnErrorNotEmpty(t *testing.T) {
	fastSettle(t, 3)
	d, _ := settleServer(t, 1000, "2", false)
	out, err := d.Diff(context.Background(), "7")
	if err == nil || !strings.Contains(err.Error(), "not available yet") {
		t.Fatalf("out=%v err=%v; a diff that never computes must not read as empty", out, err)
	}
}

// A genuinely empty diff -- the MR says zero changes and the diff refs
// exist -- is returned as empty, at once.
func TestDiffGenuinelyEmptyIsEmpty(t *testing.T) {
	fastSettle(t, 3)
	d, calls := settleServer(t, 1000, "0", true)
	out, err := d.Diff(context.Background(), "7")
	if err != nil || len(out) != 0 {
		t.Fatalf("out=%v err=%v", out, err)
	}
	if *calls != 1 {
		t.Fatalf("diff endpoint called %d times for a genuinely empty diff", *calls)
	}
}

// "0" without diff refs is an MR whose diff has not been computed at all;
// it is not proof of emptiness.
func TestDiffZeroCountWithoutRefsIsNotReady(t *testing.T) {
	fastSettle(t, 2)
	d, _ := settleServer(t, 1000, "0", false)
	if _, err := d.Diff(context.Background(), "7"); err == nil {
		t.Fatal("an uncomputed diff was reported as empty")
	}
}
