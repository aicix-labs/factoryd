package gitlab_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/scm/conformance"
	"github.com/aicix-labs/factoryd/internal/scm/gitlab"
	"github.com/aicix-labs/factoryd/internal/scm/httpfixture"
)

// Factory is exported to the conformance control test.
func Factory() conformance.Factory {
	return conformance.Factory{
		Provider:   "gitlab",
		FixtureDir: "testdata",
		// The provider's own wording, taken from the recorded fixture.
		GitUsername:        "oauth2",
		UnmergeableMessage: "Branch cannot be merged",
		New: func(hc *http.Client) (scm.Driver, error) {
			return gitlab.New(gitlab.Config{
				BaseURL: "https://gitlab.example.com/api/v4",
				Project: "acme/widgets", Token: "test-token", HTTPClient: hc,
			})
		},
	}
}

func TestConformance(t *testing.T) {
	for _, r := range conformance.Check(context.Background(), Factory()) {
		if r.Err != nil {
			t.Errorf("%s: %v", r.Scenario, r.Err)
		}
	}
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	cases := map[string]gitlab.Config{
		"no project": {Token: "t"},
		"no token":   {Project: "acme/widgets"},
	}
	for name, cfg := range cases {
		if _, err := gitlab.New(cfg); err == nil {
			t.Errorf("%s: New accepted an invalid config", name)
		}
	}
}

// GitLab's HTTP refusal codes do not reliably distinguish conflicts from CI.
// The issue-58 response is HTTP 405 even though the provider facts say the MR
// itself can merge and is merely waiting on CI. Check every documented
// self-clearing detailed_merge_status rather than guessing from that status
// code. The unmodified recorded fixture remains the control: its explicit
// "conflict" status is covered by the conformance suite.
func TestMergeClassifiesExplicitCIPipelineStatusesAsRetryable(t *testing.T) {
	for _, detailed := range []string{"ci_must_pass", "ci_still_running", "checking", "preparing"} {
		t.Run(detailed, func(t *testing.T) {
			b := pipelineRefusalFixture(t, detailed)

			tr := httpfixture.NewTransport(b)
			d, err := gitlab.New(gitlab.Config{BaseURL: "https://gitlab.example.com/api/v4", Project: "acme/widgets", Token: "test-token", HTTPClient: tr.Client()})
			if err != nil {
				t.Fatal(err)
			}
			r, err := scm.MergeVerified(context.Background(), d, conformance.ChangeID, conformance.HeadSHA, conformance.TargetBranch)
			if err != nil {
				t.Fatal(err)
			}
			if r.Outcome != scm.RefusedPipeline || !strings.Contains(r.Reason, `HTTP 405`) || !strings.Contains(r.Reason, `detailed_merge_status="`+detailed+`"`) {
				t.Fatalf("merge result=%+v, want explicit CI pipeline refusal", r)
			}
			if err := tr.Done(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The before-PUT status can be invalidated while GitLab processes the merge.
// A post-refusal head move or conflict must remain a terminal refusal even when
// the old snapshot said CI was pending.
func TestMergeDoesNotRetryAnInvalidatedPipelineRefusal(t *testing.T) {
	cases := map[string]func(t *testing.T, post *httpfixture.Exchange){
		"head moved": func(t *testing.T, post *httpfixture.Exchange) {
			t.Helper()
			post.Body = replaceFixtureField(t, post.Body, []byte(`"sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`), []byte(`"sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"`))
		},
		"now conflict": func(t *testing.T, post *httpfixture.Exchange) {
			t.Helper()
			post.Body = replaceFixtureField(t, post.Body, []byte(`"detailed_merge_status": "ci_must_pass"`), []byte(`"detailed_merge_status": "conflict"`))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := pipelineRefusalFixture(t, "ci_must_pass")
			mutate(t, &b.Exchanges[len(b.Exchanges)-1])
			tr := httpfixture.NewTransport(b)
			d, err := gitlab.New(gitlab.Config{BaseURL: "https://gitlab.example.com/api/v4", Project: "acme/widgets", Token: "test-token", HTTPClient: tr.Client()})
			if err != nil {
				t.Fatal(err)
			}
			r, err := scm.MergeVerified(context.Background(), d, conformance.ChangeID, conformance.HeadSHA, conformance.TargetBranch)
			if err != nil {
				t.Fatal(err)
			}
			if r.Outcome != scm.RefusedConflict {
				t.Fatalf("merge result=%+v, want post-refusal conflict", r)
			}
			if err := tr.Done(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func pipelineRefusalFixture(t *testing.T, detailed string) *httpfixture.Bundle {
	t.Helper()
	b, err := httpfixture.Load("testdata", "merge_refused_unmergeable")
	if err != nil {
		t.Fatal(err)
	}
	replacements := [][2][]byte{
		{[]byte(`"detailed_merge_status": "conflict"`), []byte(`"detailed_merge_status": "` + detailed + `"`)},
		{[]byte(`"has_conflicts": true`), []byte(`"has_conflicts": false`)},
		{[]byte(`"merge_status": "cannot_be_merged"`), []byte(`"merge_status": "can_be_merged"`)},
	}
	for _, replacement := range replacements {
		b.Exchanges[0].Body = replaceFixtureField(t, b.Exchanges[0].Body, replacement[0], replacement[1])
	}
	b.Exchanges[1].Status = http.StatusMethodNotAllowed
	// The new read is intentionally a separate exchange. The transport's
	// strict ordering proves the driver asked once before PUT and once after
	// GitLab refused it, rather than reusing the original snapshot.
	post := b.Exchanges[0]
	post.RequestBodyContains = nil
	b.Exchanges = append(b.Exchanges, post)
	return b
}

func replaceFixtureField(t *testing.T, body, old, new []byte) []byte {
	t.Helper()
	if !bytes.Contains(body, old) {
		t.Fatalf("recorded MR does not carry %s", old)
	}
	return bytes.Replace(body, old, new, 1)
}
