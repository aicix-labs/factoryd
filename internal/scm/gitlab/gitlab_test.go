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
				if !bytes.Contains(b.Exchanges[0].Body, replacement[0]) {
					t.Fatalf("recorded MR does not carry %s", replacement[0])
				}
				b.Exchanges[0].Body = bytes.Replace(b.Exchanges[0].Body, replacement[0], replacement[1], 1)
			}
			b.Exchanges[1].Status = http.StatusMethodNotAllowed

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
