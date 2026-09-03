package main

import (
	"context"
	"fmt"
	"net/http"
	"regexp"

	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/scm/conformance"
	"github.com/aicix-labs/factoryd/internal/scm/httpfixture"
)

// world is the live state a scenario needs, described in the terms the
// conformance suite asserts on.
type world struct {
	// ChangeID is the live pull/merge request the scenario acts on.
	ChangeID scm.ChangeID
	// HeadSHA is that change's head. MergeSHA is set once a merge has landed.
	HeadSHA  string
	MergeSHA string
	// Redactor carries the mappings from these live values to the suite's
	// placeholders.
	Redactor *httpfixture.Redactor
	// Secrets must never survive into a written fixture.
	Secrets []string
}

// target is one provider's live scratch repository.
type target interface {
	describe() string
	version() string
	// prepare puts the repository into the state a scenario needs and returns
	// the mappings for whatever it created.
	prepare(ctx context.Context, sc scenario) (*world, error)
	// driver builds the driver under test, wired to the recording client.
	driver(hc *http.Client) (scm.Driver, error)
	// cleanup removes what prepare created, so the next run starts level.
	cleanup(ctx context.Context) error
	providerName() string
}

// scenario is one fixture: what state it needs and what verb it records.
type scenario struct {
	name string
	// setup names the shape the repository must be in. The per-provider
	// prepare switches on it.
	setup string
	// act drives the verb under test. Everything it does goes through the
	// recording transport.
	act func(ctx context.Context, d scm.Driver, w *world) error
	// synthetic, when non-empty, says why this fixture cannot be recorded and
	// must stay hand-written.
	synthetic string
}

func scenarios() []scenario {
	return []scenario{
		{name: "whoami", setup: "none", act: func(ctx context.Context, d scm.Driver, _ *world) error {
			_, err := d.Whoami(ctx)
			return err
		}},
		{name: "list_open", setup: "two_open_one_draft", act: func(ctx context.Context, d scm.Driver, _ *world) error {
			_, err := d.ListOpen(ctx)
			return err
		}},
		{name: "get", setup: "open_change", act: func(ctx context.Context, d scm.Driver, w *world) error {
			_, err := d.Get(ctx, w.ChangeID)
			return err
		}},
		{name: "diff", setup: "open_change", act: func(ctx context.Context, d scm.Driver, w *world) error {
			_, err := d.Diff(ctx, w.ChangeID)
			return err
		}},
		{name: "pipeline_none", setup: "open_change", act: func(ctx context.Context, d scm.Driver, w *world) error {
			_, err := d.Pipeline(ctx, w.ChangeID)
			return err
		}},
		{name: "comment", setup: "open_change", act: func(ctx context.Context, d scm.Driver, w *world) error {
			return d.Comment(ctx, w.ChangeID, "changes-requested: the guard matches the mention, not the command")
		}},
		{name: "set_draft_ready", setup: "draft_change", act: func(ctx context.Context, d scm.Driver, w *world) error {
			return d.SetDraft(ctx, w.ChangeID, false)
		}},
		{name: "post_audit", setup: "open_change", act: func(ctx context.Context, d scm.Driver, w *world) error {
			return d.PostAudit(ctx, w.ChangeID, w.HeadSHA, scm.Audit{
				Lens:    "scope-escape",
				Verdict: scm.AuditCleared,
				Attempts: []string{
					"reverted the guard and confirmed the new test goes red",
					"fed the guard a path that only mentions the pattern in a comment",
				},
				Notes: "no escape found",
			})
		}},
		{name: "audits", setup: "change_with_audits", act: func(ctx context.Context, d scm.Driver, w *world) error {
			_, err := d.Audits(ctx, w.ChangeID, w.HeadSHA)
			return err
		}},
		{name: "find_open_found", setup: "open_change", act: func(ctx context.Context, d scm.Driver, w *world) error {
			_, _, err := d.FindOpenBySource(ctx, conformance.SourceBranch)
			return err
		}},
		{name: "find_open_absent", setup: "open_change", act: func(ctx context.Context, d scm.Driver, w *world) error {
			_, _, err := d.FindOpenBySource(ctx, "producer/nothing-here")
			return err
		}},
		{name: "open_draft", setup: "branch_only", act: func(ctx context.Context, d scm.Driver, w *world) error {
			c, err := d.OpenDraft(ctx, scm.DraftSpec{
				SourceBranch: conformance.SourceBranch, TargetBranch: conformance.TargetBranch,
				Title: conformance.ChangeTitle, Body: "opened by factoryd submit",
			})
			if err != nil {
				return err
			}
			// The new change's id is created by the exchange being recorded.
			w.Redactor.MapPattern(regexp.MustCompile(fmt.Sprintf(`"(iid|number)":%s\b`, c.ID)), `"$1":42`)
			w.Redactor.MapPattern(regexp.MustCompile(fmt.Sprintf(`/(merge_requests|pulls|pull|issues)/%s\b`, c.ID)), `/$1/42`)
			return nil
		}},
		{name: "close", setup: "draft_change", act: func(ctx context.Context, d scm.Driver, w *world) error {
			return d.Close(ctx, w.ChangeID, "superseded by a newer submission")
		}},
		{name: "merge_refused_draft", setup: "draft_change", act: func(ctx context.Context, d scm.Driver, w *world) error {
			_, err := scm.MergeVerified(ctx, d, w.ChangeID, w.HeadSHA, conformance.TargetBranch)
			return err
		}},
		{name: "merge_head_moved", setup: "open_change_head_moved", act: func(ctx context.Context, d scm.Driver, w *world) error {
			// Deliberately pinned to the sha the caller believed was current.
			_, err := scm.MergeVerified(ctx, d, w.ChangeID, conformance.HeadSHA, conformance.TargetBranch)
			return err
		}},
		{name: "merge_refused_unmergeable", setup: "conflicting_change", act: func(ctx context.Context, d scm.Driver, w *world) error {
			_, err := scm.MergeVerified(ctx, d, w.ChangeID, w.HeadSHA, conformance.TargetBranch)
			return err
		}},
		{name: "merge_success", setup: "mergeable_change", act: func(ctx context.Context, d scm.Driver, w *world) error {
			r, err := scm.MergeVerified(ctx, d, w.ChangeID, w.HeadSHA, conformance.TargetBranch)
			if err != nil {
				return err
			}
			if r.Outcome != scm.Merged {
				return fmt.Errorf("the live merge did not succeed: %v (%s)", r.Outcome, r.Reason)
			}
			// The merge commit did not exist when the redactor was built -- the
			// exchange being recorded is what created it. Registering it here
			// works because redaction happens at write time.
			w.Redactor.Map(r.MergeCommit, conformance.MergeSHA)
			return nil
		}},

		// The two cases a provider cannot be asked to produce. Both are
		// defensive: they describe a provider answering inconsistently, which
		// is precisely what a healthy one will not do on request. They stay
		// hand-written, and say so in their own source block rather than
		// looking like fixtures nobody got round to.
		{
			name:      "merge_reported_but_absent",
			synthetic: "no provider will return 200 with a body that says it did not merge on demand",
		},
		{
			name:      "merge_unverified",
			synthetic: "requires a provider to report a merge commit that is not on the target branch",
		},
		{
			name:      "pipeline_success",
			synthetic: "needs a green CI run on the scratch project; record once a runner is attached",
		},
		{
			name:      "pipeline_failed",
			synthetic: "needs a red CI run on the scratch project; record once a runner is attached",
		},
		{
			name:      "audit_requires_attempts",
			synthetic: "asserts the driver rejects an invalid audit before any request; there is nothing to record",
		},
	}
}

// runScenario prepares the world, records the verb, and writes the fixture.
func runScenario(ctx context.Context, t target, sc scenario, outDir string, dryRun bool) error {
	if err := t.cleanup(ctx); err != nil {
		return fmt.Errorf("cleanup before setup: %w", err)
	}
	w, err := t.prepare(ctx, sc)
	if err != nil {
		return fmt.Errorf("setup %q: %w", sc.setup, err)
	}

	rec := httpfixture.NewRecorder(w.Redactor, w.Secrets)
	d, err := t.driver(rec.Client())
	if err != nil {
		return err
	}
	if err := sc.act(ctx, d, w); err != nil {
		return fmt.Errorf("recording: %w", err)
	}
	if dryRun {
		fmt.Printf("         (dry run: %d exchanges)\n", len(rec.Exchanges()))
		return nil
	}
	return rec.Write(outDir, sc.name, httpfixture.Source{
		Recorded: true, Provider: t.providerName(), ProviderVersion: t.version(),
	})
}
