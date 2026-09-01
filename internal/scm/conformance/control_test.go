package conformance_test

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"testing"

	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/scm/conformance"
	"github.com/aicix-labs/factoryd/internal/scm/github"
	"github.com/aicix-labs/factoryd/internal/scm/gitlab"
)

// This file is the conformance suite's own evidence. A suite that has never
// been shown to reject anything is a check that cannot fail (SPEC.md §9).
//
// Every mutant below breaks exactly one behaviour the suite claims to police,
// and the test asserts which scenarios go red. The paired clean run is the
// positive control: without it, a red could just mean the harness is broken.

func githubFactory(dir string) conformance.Factory {
	return conformance.Factory{
		Provider:   "github",
		FixtureDir: dir,
		New: func(hc *http.Client) (scm.Driver, error) {
			return github.New(github.Config{
				BaseURL: "https://api.github.com", Owner: "acme", Repo: "widgets",
				Token: "test-token", HTTPClient: hc,
			})
		},
	}
}

func gitlabFactory(dir string) conformance.Factory {
	return conformance.Factory{
		Provider:   "gitlab",
		FixtureDir: dir,
		New: func(hc *http.Client) (scm.Driver, error) {
			return gitlab.New(gitlab.Config{
				BaseURL: "https://gitlab.example.com/api/v4",
				Project: "acme/widgets", Token: "test-token", HTTPClient: hc,
			})
		},
	}
}

func providers() map[string]conformance.Factory {
	return map[string]conformance.Factory{
		"github": githubFactory("../github/testdata"),
		"gitlab": gitlabFactory("../gitlab/testdata"),
	}
}

// mutate returns f with its driver wrapped by w.
func mutate(f conformance.Factory, w func(scm.Driver) scm.Driver) conformance.Factory {
	base := f.New
	f.New = func(hc *http.Client) (scm.Driver, error) {
		d, err := base(hc)
		if err != nil {
			return nil, err
		}
		return w(d), nil
	}
	return f
}

func failedNames(rs []conformance.Result) []string {
	var out []string
	for _, r := range conformance.Failed(rs) {
		out = append(out, r.Scenario)
	}
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCleanDriversPass is the positive control. If this ever fails, the red
// results below prove nothing about the mutants.
func TestCleanDriversPass(t *testing.T) {
	for name, f := range providers() {
		if got := failedNames(conformance.Check(context.Background(), f)); len(got) != 0 {
			t.Errorf("%s: unmutated driver failed %v", name, got)
		}
	}
}

// ---------- mutants ----------

type mergeAlwaysSucceeds struct{ scm.Driver }

// Merge reports a merge whatever the provider said -- v1's actual behaviour,
// where "Branch cannot be merged" came back as exit status 0.
func (m mergeAlwaysSucceeds) Merge(ctx context.Context, id scm.ChangeID, head string) (scm.ProviderMerge, error) {
	r, err := m.Driver.Merge(ctx, id, head)
	if r.Outcome != scm.Merged {
		return scm.ProviderMerged(conformance.MergeSHA), nil
	}
	return r, err
}

type ancestryAlwaysTrue struct{ scm.Driver }

// IsAncestor takes the provider's word for what landed.
func (m ancestryAlwaysTrue) IsAncestor(ctx context.Context, sha, ref string) (bool, error) {
	_, _ = m.Driver.IsAncestor(ctx, sha, ref)
	return true, nil
}

type noPipelineIsGreen struct{ scm.Driver }

// Pipeline treats "nothing ran" as "nothing failed".
func (m noPipelineIsGreen) Pipeline(ctx context.Context, id scm.ChangeID) (scm.PipelineStatus, error) {
	p, err := m.Driver.Pipeline(ctx, id)
	if p.State == scm.PipelineNone {
		p.State = scm.PipelineSuccess
	}
	return p, err
}

type setDraftUnimplemented struct{ scm.Driver }

// SetDraft is a stub. This is the exact v1 divergence: a verb that exists on
// one provider and not the other, with nothing to notice.
func (m setDraftUnimplemented) SetDraft(context.Context, scm.ChangeID, bool) error {
	return errors.New("not implemented for this provider")
}

type listOpenDropsPages struct{ scm.Driver }

// ListOpen forgets pagination and returns only the first page.
func (m listOpenDropsPages) ListOpen(ctx context.Context) ([]scm.Change, error) {
	cs, err := m.Driver.ListOpen(ctx)
	if len(cs) > 1 {
		cs = cs[:1]
	}
	return cs, err
}

type auditsIgnoreSHA struct{ scm.Driver }

// Audits returns audits of other commits as if they covered this one.
func (m auditsIgnoreSHA) Audits(ctx context.Context, id scm.ChangeID, sha string) ([]scm.Audit, error) {
	as, err := m.Driver.Audits(ctx, id, sha)
	return append(as, scm.Audit{
		Lens: "stale-lens", Verdict: scm.AuditCleared, SHA: conformance.StaleSHA,
		Attempts: []string{"tried the previous head"},
	}), err
}

type auditSkipsValidation struct{ scm.Driver }

// PostAudit rubber-stamps an audit that records no attempts.
func (m auditSkipsValidation) PostAudit(ctx context.Context, id scm.ChangeID, sha string, a scm.Audit) error {
	if len(a.Attempts) == 0 {
		return m.Driver.Comment(ctx, id, "adversarial pass: CLEARED")
	}
	return m.Driver.PostAudit(ctx, id, sha, a)
}

func TestSuiteRejectsBrokenDrivers(t *testing.T) {
	cases := []struct {
		name string
		wrap func(scm.Driver) scm.Driver
		want []string // sorted
	}{
		{
			name: "a refused merge reported as merged",
			wrap: func(d scm.Driver) scm.Driver { return mergeAlwaysSucceeds{d} },
			want: []string{"merge_head_moved", "merge_refused_draft",
				"merge_refused_unmergeable", "merge_reported_but_absent"},
		},
		{
			name: "the provider's word taken as proof the merge landed",
			wrap: func(d scm.Driver) scm.Driver { return ancestryAlwaysTrue{d} },
			want: []string{"merge_unverified"},
		},
		{
			name: "no pipeline read as a green pipeline",
			wrap: func(d scm.Driver) scm.Driver { return noPipelineIsGreen{d} },
			want: []string{"pipeline_none"},
		},
		{
			name: "a verb stubbed out on one provider",
			wrap: func(d scm.Driver) scm.Driver { return setDraftUnimplemented{d} },
			want: []string{"set_draft_ready"},
		},
		{
			name: "pagination dropped",
			wrap: func(d scm.Driver) scm.Driver { return listOpenDropsPages{d} },
			want: []string{"list_open"},
		},
		{
			name: "audits of a stale commit counted against the head",
			wrap: func(d scm.Driver) scm.Driver { return auditsIgnoreSHA{d} },
			want: []string{"audits"},
		},
		{
			name: "an audit with no attempts accepted",
			wrap: func(d scm.Driver) scm.Driver { return auditSkipsValidation{d} },
			want: []string{"audit_requires_attempts"},
		},
	}

	for provider, f := range providers() {
		for _, c := range cases {
			t.Run(provider+"/"+c.name, func(t *testing.T) {
				got := failedNames(conformance.Check(context.Background(), mutate(f, c.wrap)))
				if !equal(got, c.want) {
					t.Errorf("suite failed %v, want exactly %v", got, c.want)
				}
			})
		}
	}
}
