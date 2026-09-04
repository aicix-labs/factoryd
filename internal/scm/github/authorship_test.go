package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aicix-labs/factoryd/internal/scm"
)

func ghServer(t *testing.T, h http.HandlerFunc) *Driver {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	d, err := New(Config{BaseURL: srv.URL, Owner: "acme", Repo: "widgets", Token: "t", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A comment body is written by whoever posts it. The audit's author is the
// provider's authenticated comment author, and nothing the body claims can
// override it -- an audit that could name its own author could be forged by
// the producer for its own head.
func TestAuditAuthorIsTheProvidersNotTheBodys(t *testing.T) {
	forged, err := scm.EncodeAudit(scm.Audit{Lens: "authz", SHA: "abc", Verdict: scm.AuditCleared, Attempts: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	// Plant a posted_by claim inside the machine block as an older or
	// malicious writer might.
	forged = strings.Replace(forged, `"lens": "authz"`, `"posted_by": "factory-reviewer", "posted_by_id": "2", "lens": "authz"`, 1)
	d := ghServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{{"body": forged, "user": map[string]any{"login": "producer-bot", "id": 1}}})
	})
	as, err := d.Audits(context.Background(), "7", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(as) != 1 || as[0].PostedBy != "producer-bot" || as[0].PostedByID != "1" {
		t.Fatalf("audits=%+v; the body's claim must not survive", as)
	}
}

func TestGetCarriesTheAuthorsStableID(t *testing.T) {
	d := ghServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "open", "title": "t", "draft": false,
			"user": map[string]any{"login": "producer-bot", "id": 4242},
			"head": map[string]any{"ref": "s", "sha": "abc"}, "base": map[string]any{"ref": "main"}})
	})
	c, err := d.Get(context.Background(), "7")
	if err != nil {
		t.Fatal(err)
	}
	if c.AuthorID != "4242" || c.Author != "producer-bot" {
		t.Fatalf("change=%+v", c)
	}
}

// GitHub omits the patch of a large or binary file. Such a file is
// incomplete, not "nothing added".
func TestDiffMarksAFileWithoutAPatchIncomplete(t *testing.T) {
	d := ghServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"filename": "big.bin", "status": "modified", "additions": 0, "deletions": 0},
			{"filename": "huge.go", "status": "added", "additions": 9000, "deletions": 0},
			{"filename": "ok.go", "status": "modified", "additions": 1, "deletions": 0, "patch": "@@ -1 +1 @@\n+x\n"},
			{"filename": "gone.go", "status": "removed", "additions": 0, "deletions": 3},
		})
	})
	fs, err := d.Diff(context.Background(), "7")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"big.bin": true, "huge.go": true, "ok.go": false, "gone.go": false}
	for _, f := range fs {
		if f.Incomplete != want[f.Path] {
			t.Fatalf("%s incomplete=%v want %v (%s)", f.Path, f.Incomplete, want[f.Path], f.IncompleteReason)
		}
	}
}

// A rename with content changes and no patch is a rename whose content was
// not delivered -- where a blank patch hides the most. A pure path-only
// rename is proved by zero additions and deletions, and is complete.
func TestRenameWithChangesAndNoPatchIsIncomplete(t *testing.T) {
	d := ghServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"filename": "new/secret.go", "previous_filename": "old/plain.go", "status": "renamed", "additions": 12, "deletions": 3},
			{"filename": "new/same.go", "previous_filename": "old/same.go", "status": "renamed", "additions": 0, "deletions": 0},
		})
	})
	fs, err := d.Diff(context.Background(), "7")
	if err != nil {
		t.Fatal(err)
	}
	if !fs[0].Incomplete || !strings.Contains(fs[0].IncompleteReason, "renamed with content changes") {
		t.Fatalf("changed rename without a patch read as complete: %+v", fs[0])
	}
	if fs[1].Incomplete {
		t.Fatalf("pure rename read as incomplete: %+v", fs[1])
	}
}
