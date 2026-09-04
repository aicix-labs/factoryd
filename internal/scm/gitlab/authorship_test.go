package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aicix-labs/factoryd/internal/scm"
)

func glServer(t *testing.T, h http.HandlerFunc) *Driver {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	d, err := New(Config{BaseURL: srv.URL + "/api/v4", Project: "acme/widgets", Token: "t", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestAuditAuthorIsTheProvidersNotTheBodys(t *testing.T) {
	forged, err := scm.EncodeAudit(scm.Audit{Lens: "authz", SHA: "abc", Verdict: scm.AuditCleared, Attempts: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	forged = strings.Replace(forged, `"lens": "authz"`, `"posted_by": "factory-reviewer", "posted_by_id": "2", "lens": "authz"`, 1)
	d := glServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{{"body": forged, "system": false, "author": map[string]any{"username": "producer-bot", "id": 1}}})
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
	d := glServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"iid": 7, "state": "opened", "title": "t", "sha": "abc",
			"author": map[string]any{"username": "producer-bot", "id": 4242}, "source_branch": "s", "target_branch": "main"})
	})
	c, err := d.Get(context.Background(), "7")
	if err != nil {
		t.Fatal(err)
	}
	if c.AuthorID != "4242" {
		t.Fatalf("change=%+v", c)
	}
}

// GitLab documents two incomplete-diff states; both are said, and an empty
// patch on a changed file is not read as "nothing added" either.
func TestDiffMarksCollapsedAndTooLargeIncomplete(t *testing.T) {
	d := glServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/diffs") {
			json.NewEncoder(w).Encode([]map[string]any{
				{"old_path": "a.go", "new_path": "a.go", "diff": "", "collapsed": true},
				{"old_path": "b.go", "new_path": "b.go", "diff": "", "too_large": true},
				{"old_path": "c.go", "new_path": "c.go", "diff": ""},
				{"old_path": "d.go", "new_path": "d.go", "diff": "@@ -1 +1 @@\n+x\n"},
				{"old_path": "e.go", "new_path": "e.go", "diff": "", "deleted_file": true},
			})
			return
		}
		http.NotFound(w, r)
	})
	fs, err := d.Diff(context.Background(), "7")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"a.go": true, "b.go": true, "c.go": true, "d.go": false, "e.go": false}
	// The reason is what the reviewer acts on: a collapsed diff can be
	// fetched, a too-large one cannot.
	reason := map[string]string{"a.go": "collapsed", "b.go": "too_large", "c.go": "empty patch"}
	for _, f := range fs {
		if f.Incomplete != want[f.Path] {
			t.Fatalf("%s incomplete=%v want %v (%s)", f.Path, f.Incomplete, want[f.Path], f.IncompleteReason)
		}
		if r := reason[f.Path]; r != "" && !strings.Contains(f.IncompleteReason, r) {
			t.Fatalf("%s reason %q does not name %q", f.Path, f.IncompleteReason, r)
		}
	}
}

// GitLab gives no per-file counts, so a rename with an empty diff is proved
// path-only by comparing blobs: the old path on the base against the new
// path on the head. Equal blobs are a pure rename; anything else, or a
// lookup that fails, is content not delivered.
func TestRenameWithEmptyDiffIsProvedByBlobs(t *testing.T) {
	type tc struct {
		name     string
		oldBlob  string
		newBlob  string
		refs     bool
		lookupOK bool
		want     bool // incomplete
	}
	for _, c := range []tc{
		{"equal blobs: pure rename", "b1", "b1", true, true, false},
		{"different blobs: changes hidden", "b1", "b2", true, true, true},
		{"lookup fails", "b1", "b1", true, false, true},
		{"no diff refs", "b1", "b1", false, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := glServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/diffs"):
					json.NewEncoder(w).Encode([]map[string]any{{"old_path": "old/name.go", "new_path": "new/name.go", "renamed_file": true, "diff": ""}})
				case strings.HasSuffix(r.URL.Path, "/merge_requests/7"):
					m := map[string]any{"iid": 7, "state": "opened", "sha": "head", "changes_count": "1", "source_branch": "s", "target_branch": "main", "author": map[string]any{"username": "u", "id": 1}}
					if c.refs {
						m["diff_refs"] = map[string]any{"head_sha": "head", "base_sha": "base"}
					}
					json.NewEncoder(w).Encode(m)
				case strings.Contains(r.URL.Path, "/repository/files/"):
					if !c.lookupOK {
						http.Error(w, `{"message":"404 File Not Found"}`, 404)
						return
					}
					ref := r.URL.Query().Get("ref")
					blob := c.newBlob
					if ref == "base" {
						blob = c.oldBlob
					}
					json.NewEncoder(w).Encode(map[string]any{"blob_id": blob})
				default:
					http.NotFound(w, r)
				}
			})
			fs, err := d.Diff(context.Background(), "7")
			if err != nil {
				t.Fatal(err)
			}
			if len(fs) != 1 || fs[0].Incomplete != c.want {
				t.Fatalf("incomplete=%v (%s), want %v", fs[0].Incomplete, fs[0].IncompleteReason, c.want)
			}
		})
	}
}
