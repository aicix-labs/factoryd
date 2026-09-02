package httpfixture_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/aicix-labs/factoryd/internal/scm/httpfixture"
)

const (
	liveHost  = "gitlab.internal.example.com"
	liveUser  = "real-bot-account"
	liveSHA   = "9f3c1a77bb2e4d5061f0a8c3e2b1d9a4f7c6e5b0"
	liveToken = "glpat-SUPERSECRETVALUE"
)

func redactor() *httpfixture.Redactor {
	r := httpfixture.NewRedactor()
	r.Map(liveHost, "gitlab.example.com")
	r.Map(liveUser, "factory-reviewer")
	r.Map(liveSHA, strings.Repeat("a", 40))
	return r
}

func TestRedactorRewritesMappedValues(t *testing.T) {
	in := `{"web_url":"https://` + liveHost + `/x","username":"` + liveUser + `","sha":"` + liveSHA + `"}`
	got := redactor().Apply(in)
	for _, leaked := range []string{liveHost, liveUser, liveSHA} {
		if strings.Contains(got, leaked) {
			t.Errorf("mapped value %q survived redaction: %s", leaked, got)
		}
	}
	if !strings.Contains(got, strings.Repeat("a", 40)) {
		t.Errorf("the head sha was not rewritten to its placeholder: %s", got)
	}
}

// The property the redactor exists for: a value nobody thought to map must
// still be rewritten. A redactor that only handles what it was told about
// leaks whatever it was not told about, and the fixture still looks fine.
func TestUnmappedIdentifiersAreStillRewritten(t *testing.T) {
	unmapped := "1122334455667788990011223344556677889900"
	got := redactor().Apply(`{"sha":"` + unmapped + `"}`)
	if strings.Contains(got, unmapped) {
		t.Fatalf("an unmapped object id passed through untouched: %s", got)
	}

	// And deterministically, so re-recording produces no spurious diff.
	if again := redactor().Apply(`{"sha":"` + unmapped + `"}`); again != got {
		t.Fatalf("redaction is not deterministic:\n%s\n%s", got, again)
	}
}

func TestTimestampsAreFixed(t *testing.T) {
	got := redactor().Apply(`{"a":"2026-04-25T01:14:23.199Z","b":"2025-01-02T03:04:05+02:00"}`)
	if strings.Contains(got, "2026-04-25") || strings.Contains(got, "2025-01-02") {
		t.Fatalf("a live timestamp survived: %s", got)
	}
	if strings.Count(got, httpfixture.FixedTimestamp) != 2 {
		t.Fatalf("timestamps were not normalised: %s", got)
	}
}

// A placeholder must not be re-rewritten into noise on a second pass.
func TestPlaceholdersAreStable(t *testing.T) {
	placeholder := strings.Repeat("b", 40)
	in := `{"sha":"` + placeholder + `"}`
	if got := redactor().Apply(in); got != in {
		t.Fatalf("a placeholder was rewritten: %s", got)
	}
}

func TestLeakDetection(t *testing.T) {
	secrets := []string{liveToken, liveHost}
	if got := httpfixture.Leaks(`{"ok":true}`, secrets); len(got) != 0 {
		t.Fatalf("Leaks reported %v on clean text", got)
	}
	got := httpfixture.Leaks(`{"auth":"`+liveToken+`"}`, secrets)
	if len(got) != 1 || got[0] != liveToken {
		t.Fatalf("Leaks returned %v, want the token", got)
	}
	if err := httpfixture.MustNotLeak(`{"h":"`+liveHost+`"}`, secrets); err == nil {
		t.Fatal("MustNotLeak passed text containing a secret")
	}
}

// ---------- the recorder ----------

func liveServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("X-Request-Id", "should-not-be-recorded")
		_, _ = w.Write([]byte(`{"id":5,"username":"` + liveUser + `","created_at":"2026-04-25T01:14:23.199Z"}`))
	})
	mux.HandleFunc("/api/v4/merge", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(405)
		_, _ = w.Write([]byte(`{"message":"Branch cannot be merged"}`))
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestRecorderCapturesAndRedacts(t *testing.T) {
	srv := liveServer(t)
	rec := httpfixture.NewRecorder(redactor(), []string{liveToken, liveUser})
	c := rec.Client()

	req, _ := http.NewRequestWithContext(context.Background(), "GET", srv.URL+"/api/v4/user", nil)
	req.Header.Set("PRIVATE-TOKEN", liveToken)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	ex := rec.Exchanges()
	if len(ex) != 1 {
		t.Fatalf("captured %d exchanges, want 1", len(ex))
	}
	if ex[0].Method != "GET" || ex[0].Path != "/api/v4/user" {
		t.Fatalf("captured %s %s", ex[0].Method, ex[0].Path)
	}
	body := string(ex[0].Body)
	if strings.Contains(body, liveUser) {
		t.Errorf("the live username was recorded: %s", body)
	}
	if !strings.Contains(body, "factory-reviewer") {
		t.Errorf("the username was not rewritten to its placeholder: %s", body)
	}
	if strings.Contains(body, "2026-04-25") {
		t.Errorf("a live timestamp was recorded: %s", body)
	}
	// Only the headers the drivers read.
	if _, ok := ex[0].Header["X-Request-Id"]; ok {
		t.Error("an irrelevant header was recorded")
	}
}

// Error responses are the point of recording, not a bonus. The drivers make
// their fail-closed decisions on these, and a hand-authored error body that
// does not match reality lets a driver mishandle a real refusal while the
// suite stays green.
func TestRecorderCapturesErrorResponses(t *testing.T) {
	srv := liveServer(t)
	rec := httpfixture.NewRecorder(redactor(), nil)

	req, _ := http.NewRequest("PUT", srv.URL+"/api/v4/merge", strings.NewReader(`{"sha":"`+liveSHA+`"}`))
	resp, err := rec.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	ex := rec.Exchanges()
	if len(ex) != 1 || ex[0].Status != 405 {
		t.Fatalf("captured %+v, want one 405", ex)
	}
	if !strings.Contains(string(ex[0].Body), "Branch cannot be merged") {
		t.Fatalf("the provider's refusal message was not recorded: %s", ex[0].Body)
	}
	// The request body becomes an assertion that the driver pinned the merge.
	found := false
	for _, a := range ex[0].RequestBodyContains {
		if strings.Contains(a, strings.Repeat("a", 40)) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the recorded request assertions do not pin the sha: %v", ex[0].RequestBodyContains)
	}
}

func TestRecorderWriteRoundTrips(t *testing.T) {
	srv := liveServer(t)
	rec := httpfixture.NewRecorder(redactor(), []string{liveToken})
	req, _ := http.NewRequest("GET", srv.URL+"/api/v4/user", nil)
	resp, err := rec.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	dir := t.TempDir()
	src := httpfixture.Source{Recorded: true, Provider: "gitlab", ProviderVersion: "18.8.2"}
	if err := rec.Write(dir, "whoami", src); err != nil {
		t.Fatal(err)
	}

	b, err := httpfixture.Load(dir, "whoami")
	if err != nil {
		t.Fatalf("a written fixture did not load back: %v", err)
	}
	if b.Source == nil || !b.Source.Recorded || b.Source.Provider != "gitlab" {
		t.Fatalf("provenance was not written: %+v", b.Source)
	}
	if len(b.Exchanges) != 1 {
		t.Fatalf("round trip kept %d exchanges", len(b.Exchanges))
	}

	// And the file must be valid, formatted JSON a human can review.
	raw, err := os.ReadFile(filepath.Join(dir, "whoami.json"))
	if err != nil {
		t.Fatal(err)
	}
	var any map[string]json.RawMessage
	if err := json.Unmarshal(raw, &any); err != nil {
		t.Fatalf("the written fixture is not valid JSON: %v", err)
	}
}

// Writing must fail rather than commit a secret. A leaked token in a fixture is
// not something anyone spots in review.
func TestRecorderRefusesToWriteASecret(t *testing.T) {
	srv := liveServer(t)
	// The username is deliberately NOT mapped, and is declared a secret.
	red := httpfixture.NewRedactor()
	red.Map(liveHost, "gitlab.example.com")
	rec := httpfixture.NewRecorder(red, []string{liveUser})

	req, _ := http.NewRequest("GET", srv.URL+"/api/v4/user", nil)
	resp, err := rec.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	dir := t.TempDir()
	err = rec.Write(dir, "whoami", httpfixture.Source{Recorded: true, Provider: "gitlab"})
	if err == nil {
		t.Fatal("Write committed a fixture containing an unredacted secret")
	}
	if !strings.Contains(err.Error(), liveUser) {
		t.Fatalf("the error does not name the leaked value: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "whoami.json")); statErr == nil {
		t.Fatal("the fixture was written anyway")
	}
}

func TestRecorderRefusesAnEmptyBundle(t *testing.T) {
	rec := httpfixture.NewRecorder(redactor(), nil)
	if err := rec.Write(t.TempDir(), "nothing", httpfixture.Source{Recorded: true, Provider: "gitlab"}); err == nil {
		t.Fatal("Write produced a fixture with no exchanges")
	}
}

// Provenance is required, and "hand-written" must be a stated decision rather
// than a missing field.
func TestLoadRequiresProvenance(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"no source":            `{"exchanges":[{"method":"GET","path":"/x","status":200}]}`,
		"handwritten no note":  `{"source":{"recorded":false},"exchanges":[{"method":"GET","path":"/x","status":200}]}`,
		"recorded no provider": `{"source":{"recorded":true},"exchanges":[{"method":"GET","path":"/x","status":200}]}`,
	}
	for name, body := range cases {
		if err := os.WriteFile(filepath.Join(dir, "b.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := httpfixture.Load(dir, "b"); err == nil {
			t.Errorf("%s: Load accepted it", name)
		}
	}

	ok := `{"source":{"recorded":false,"note":"synthetic: the provider cannot be made to do this on demand"},"exchanges":[{"method":"GET","path":"/x","status":200}]}`
	if err := os.WriteFile(filepath.Join(dir, "b.json"), []byte(ok), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := httpfixture.Load(dir, "b"); err != nil {
		t.Fatalf("a hand-written fixture with a stated reason was rejected: %v", err)
	}
}

// Numeric identifiers must be replaced in context. Blind substitution of "3"
// would rewrite every other 3 in the document, including parts of timestamps
// and unrelated ids, and the corrupted fixture would still be valid JSON.
func TestMapPatternReplacesInContextOnly(t *testing.T) {
	r := httpfixture.NewRedactor()
	r.MapPattern(regexp.MustCompile(`"iid":3\b`), `"iid":42`)
	r.MapPattern(regexp.MustCompile(`/merge_requests/3\b`), "/merge_requests/42")

	in := `{"iid":3,"url":"/merge_requests/3","user_id":3,"count":33}`
	got := r.Apply(in)

	if !strings.Contains(got, `"iid":42`) || !strings.Contains(got, "/merge_requests/42") {
		t.Fatalf("the identifier was not replaced: %s", got)
	}
	// Everything else that merely contains a 3 must be untouched.
	if !strings.Contains(got, `"user_id":3`) || !strings.Contains(got, `"count":33`) {
		t.Fatalf("an unrelated value was rewritten: %s", got)
	}
}
