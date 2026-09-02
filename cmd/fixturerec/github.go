package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/scm/conformance"
	"github.com/aicix-labs/factoryd/internal/scm/github"
	"github.com/aicix-labs/factoryd/internal/scm/httpfixture"
	"github.com/aicix-labs/factoryd/internal/scm/httpjson"
)

const (
	placeholderOwner = "acme"
	placeholderRepo  = "widgets"
)

type githubTarget struct {
	api      *httpjson.Client
	base     string
	owner    string
	repo     string
	token    string
	username string
	ver      string
}

func newGitHubTarget(ctx context.Context, base, owner, repo, token string) (*githubTarget, error) {
	if base == "" {
		base = "https://api.github.com"
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Accept", "application/vnd.github+json")
	h.Set("X-GitHub-Api-Version", "2022-11-28")
	h.Set("User-Agent", "factoryd-fixturerec")

	t := &githubTarget{
		api:  &httpjson.Client{Base: base, Header: h},
		base: base, owner: owner, repo: repo, token: token,
		ver: "2022-11-28",
	}
	var me struct {
		Login string `json:"login"`
	}
	if _, err := t.api.Do(ctx, http.MethodGet, "/user", nil, &me); err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	t.username = me.Login
	if _, err := t.api.Do(ctx, http.MethodGet, t.r(""), nil, nil); err != nil {
		return nil, fmt.Errorf("github: scratch repo %s/%s: %w", owner, repo, err)
	}
	return t, nil
}

func (t *githubTarget) describe() string     { return t.owner + "/" + t.repo }
func (t *githubTarget) version() string      { return "api " + t.ver }
func (t *githubTarget) providerName() string { return "github" }

func (t *githubTarget) driver(hc *http.Client) (scm.Driver, error) {
	return github.New(github.Config{
		BaseURL: t.base, Owner: t.owner, Repo: t.repo, Token: t.token, HTTPClient: hc,
	})
}

func (t *githubTarget) r(suffix string) string {
	return fmt.Sprintf("/repos/%s/%s%s", t.owner, t.repo, suffix)
}

func (t *githubTarget) cleanup(ctx context.Context) error {
	var prs []struct {
		Number int `json:"number"`
		Head   struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if _, err := t.api.Do(ctx, http.MethodGet, t.r("/pulls?state=open&per_page=100"), nil, &prs); err != nil {
		return err
	}
	for _, pr := range prs {
		_, _ = t.api.Do(ctx, http.MethodPatch,
			t.r(fmt.Sprintf("/pulls/%d", pr.Number)), map[string]string{"state": "closed"}, nil)
	}
	var refs []struct {
		Ref string `json:"ref"`
	}
	if _, err := t.api.Do(ctx, http.MethodGet, t.r("/git/refs/heads"), nil, &refs); err != nil {
		return err
	}
	for _, ref := range refs {
		name := ref.Ref[len("refs/heads/"):]
		if name == conformance.TargetBranch {
			continue
		}
		_, _ = t.api.Do(ctx, http.MethodDelete, t.r("/git/refs/heads/"+name), nil, nil)
	}
	return nil
}

// putFile creates or updates a file on a branch and returns the new commit sha.
func (t *githubTarget) putFile(ctx context.Context, branch, path, content, message string) (string, error) {
	var existing struct {
		SHA string `json:"sha"`
	}
	_, _ = t.api.Do(ctx, http.MethodGet,
		t.r("/contents/"+path+"?ref="+url.QueryEscape(branch)), nil, &existing)

	body := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
		"branch":  branch,
	}
	if existing.SHA != "" {
		body["sha"] = existing.SHA
	}
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if _, err := t.api.Do(ctx, http.MethodPut, t.r("/contents/"+path), body, &out); err != nil {
		return "", err
	}
	return out.Commit.SHA, nil
}

func (t *githubTarget) deleteFile(ctx context.Context, branch, path, message string) (string, error) {
	var existing struct {
		SHA string `json:"sha"`
	}
	if _, err := t.api.Do(ctx, http.MethodGet,
		t.r("/contents/"+path+"?ref="+url.QueryEscape(branch)), nil, &existing); err != nil {
		// Not found is passed through so the caller can tell "already absent"
		// from a real failure.
		return "", err
	}
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	_, err := t.api.Do(ctx, http.MethodDelete, t.r("/contents/"+path), map[string]any{
		"message": message, "sha": existing.SHA, "branch": branch,
	}, &out)
	return out.Commit.SHA, err
}

func (t *githubTarget) headOf(ctx context.Context, branch string) (string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	_, err := t.api.Do(ctx, http.MethodGet, t.r("/git/ref/heads/"+branch), nil, &ref)
	return ref.Object.SHA, err
}

func (t *githubTarget) branchFrom(ctx context.Context, name, from string) error {
	sha, err := t.headOf(ctx, from)
	if err != nil {
		return err
	}
	_, err = t.api.Do(ctx, http.MethodPost, t.r("/git/refs"), map[string]string{
		"ref": "refs/heads/" + name, "sha": sha,
	}, nil)
	return err
}

func (t *githubTarget) openPR(ctx context.Context, branch, title string, draft bool) (int, string, error) {
	var out struct {
		Number int `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	_, err := t.api.Do(ctx, http.MethodPost, t.r("/pulls"), map[string]any{
		"title": title, "head": branch, "base": conformance.TargetBranch, "draft": draft,
	}, &out)
	if err != nil {
		return 0, "", err
	}
	_, _ = t.api.Do(ctx, http.MethodPost,
		t.r(fmt.Sprintf("/issues/%d/labels", out.Number)),
		map[string]any{"labels": []string{"factory"}}, nil)
	return out.Number, out.Head.SHA, nil
}

func (t *githubTarget) comment(ctx context.Context, n int, body string) error {
	_, err := t.api.Do(ctx, http.MethodPost,
		t.r(fmt.Sprintf("/issues/%d/comments", n)), map[string]string{"body": body}, nil)
	return err
}

// seedTarget puts the default branch into the exact state every scenario
// assumes, whatever the previous run left behind.
//
// Complete, not create-if-missing. A scenario that merges lands its rename on
// the default branch, so without removing new/name.go the next run's change
// only modifies a file that is already there -- and GitHub reports no rename at
// all. The diff fixture then describes something the suite does not assert, and
// the recording is wrong in a way that looks like a driver bug.
func (t *githubTarget) seedTarget(ctx context.Context) error {
	want := map[string]string{
		gateFile:      "package gate\n\ncontext\nold\n",
		"old/name.go": "package old\n",
	}
	for path, content := range want {
		if _, err := t.putFile(ctx, conformance.TargetBranch, path, content, "seed: reset "+path); err != nil {
			return fmt.Errorf("seeding %s: %w", path, err)
		}
	}
	for _, path := range []string{"new/name.go", "second.txt", "later.txt"} {
		if _, err := t.deleteFile(ctx, conformance.TargetBranch, path, "seed: remove "+path); err != nil {
			if httpjson.StatusOf(err) == http.StatusNotFound {
				continue
			}
			return fmt.Errorf("seeding: removing %s: %w", path, err)
		}
	}
	return t.verifyBase(ctx)
}

// verifyBase confirms the default branch really carries the base state.
// Without it, "the seed had nothing to do" and "the seed silently failed" are
// the same observation.
func (t *githubTarget) verifyBase(ctx context.Context) error {
	for _, c := range []struct {
		path       string
		wantExists bool
	}{
		{gateFile, true}, {"old/name.go", true}, {"new/name.go", false},
	} {
		var out struct {
			SHA string `json:"sha"`
		}
		_, err := t.api.Do(ctx, http.MethodGet,
			t.r("/contents/"+c.path+"?ref="+url.QueryEscape(conformance.TargetBranch)), nil, &out)
		exists := err == nil
		if err != nil && httpjson.StatusOf(err) != http.StatusNotFound {
			return err
		}
		if exists != c.wantExists {
			return fmt.Errorf("github: after seeding, %s exists=%v, want %v", c.path, exists, c.wantExists)
		}
	}
	return nil
}

func (t *githubTarget) prepare(ctx context.Context, sc scenario) (*world, error) {
	if err := t.seedTarget(ctx); err != nil {
		return nil, err
	}

	w := &world{Secrets: []string{t.token, t.owner + "/" + t.repo, t.username}}
	branch := conformance.SourceBranch

	changeBranch := func() (string, error) {
		if err := t.branchFrom(ctx, branch, conformance.TargetBranch); err != nil {
			return "", err
		}
		if _, err := t.putFile(ctx, branch, gateFile, changeContent, conformance.ChangeTitle); err != nil {
			return "", err
		}
		if _, err := t.putFile(ctx, branch, "new/name.go", "package old\n", "rename: add"); err != nil {
			return "", err
		}
		return t.deleteFile(ctx, branch, "old/name.go", "rename: remove")
	}

	switch sc.setup {
	case "none":
		w.Redactor = t.redactor(0, "", "", true)
		return w, nil

	case "two_open_one_draft":
		if _, err := changeBranch(); err != nil {
			return nil, err
		}
		n, sha, err := t.openPR(ctx, branch, conformance.ChangeTitle, false)
		if err != nil {
			return nil, err
		}
		if err := t.branchFrom(ctx, "producer/second-thing", conformance.TargetBranch); err != nil {
			return nil, err
		}
		if _, err := t.putFile(ctx, "producer/second-thing", "second.txt", "2\n", "second thing"); err != nil {
			return nil, err
		}
		n2, _, err := t.openPR(ctx, "producer/second-thing", "second thing", true)
		if err != nil {
			return nil, err
		}
		w.ChangeID = scm.ChangeID(fmt.Sprint(n))
		w.HeadSHA = sha
		w.Redactor = t.redactor(n, sha, "", false)
		w.Redactor.MapPattern(regexp.MustCompile(fmt.Sprintf(`"number":%d\b`, n2)), `"number":43`)
		return w, nil

	case "open_change", "change_with_audits", "mergeable_change":
		if _, err := changeBranch(); err != nil {
			return nil, err
		}
		n, sha, err := t.openPR(ctx, branch, conformance.ChangeTitle, false)
		if err != nil {
			return nil, err
		}
		if sc.setup == "change_with_audits" {
			if err := t.seedAudits(ctx, n, sha); err != nil {
				return nil, err
			}
		}
		if sc.setup == "mergeable_change" {
			// GitHub computes mergeability asynchronously; a merge attempted
			// while it is still null is refused for a reason that has nothing
			// to do with the change.
			if err := t.waitMergeable(ctx, n); err != nil {
				return nil, err
			}
		}
		w.ChangeID = scm.ChangeID(fmt.Sprint(n))
		w.HeadSHA = sha
		w.Redactor = t.redactor(n, sha, "", false)
		return w, nil

	case "draft_change":
		if _, err := changeBranch(); err != nil {
			return nil, err
		}
		n, sha, err := t.openPR(ctx, branch, conformance.ChangeTitle, true)
		if err != nil {
			return nil, err
		}
		w.ChangeID = scm.ChangeID(fmt.Sprint(n))
		w.HeadSHA = sha
		w.Redactor = t.redactor(n, sha, "", false)
		return w, nil

	case "open_change_head_moved":
		if _, err := changeBranch(); err != nil {
			return nil, err
		}
		n, _, err := t.openPR(ctx, branch, conformance.ChangeTitle, false)
		if err != nil {
			return nil, err
		}
		moved, err := t.putFile(ctx, branch, "later.txt", "later\n", "a later commit")
		if err != nil {
			return nil, err
		}
		w.ChangeID = scm.ChangeID(fmt.Sprint(n))
		w.HeadSHA = moved
		w.Redactor = t.redactor(n, "", moved, false)
		return w, nil

	case "conflicting_change":
		if err := t.branchFrom(ctx, "producer/first", conformance.TargetBranch); err != nil {
			return nil, err
		}
		if _, err := t.putFile(ctx, "producer/first", gateFile,
			"package gate\n\ncontext\nFIRST\n", "first edit"); err != nil {
			return nil, err
		}
		if err := t.branchFrom(ctx, branch, conformance.TargetBranch); err != nil {
			return nil, err
		}
		sha, err := t.putFile(ctx, branch, gateFile,
			"package gate\n\ncontext\nSECOND\n", "conflicting edit")
		if err != nil {
			return nil, err
		}
		firstN, _, err := t.openPR(ctx, "producer/first", "first", false)
		if err != nil {
			return nil, err
		}
		n, prSHA, err := t.openPR(ctx, branch, conformance.ChangeTitle, false)
		if err != nil {
			return nil, err
		}
		if prSHA != "" {
			sha = prSHA
		}
		if err := t.waitMergeable(ctx, firstN); err != nil {
			return nil, err
		}
		if _, err := t.api.Do(ctx, http.MethodPut,
			t.r(fmt.Sprintf("/pulls/%d/merge", firstN)), map[string]string{"merge_method": "merge"}, nil); err != nil {
			return nil, fmt.Errorf("landing the first change to create the conflict: %w", err)
		}
		if err := t.waitUnmergeable(ctx, n); err != nil {
			return nil, err
		}
		w.ChangeID = scm.ChangeID(fmt.Sprint(n))
		w.HeadSHA = sha
		w.Redactor = t.redactor(n, sha, "", false)
		return w, nil

	default:
		return nil, fmt.Errorf("github: no setup for %q", sc.setup)
	}
}

// mergeableState polls until GitHub has finished computing mergeability.
// Reading it too early yields null, and a fixture recorded then describes a
// state the driver will rarely see.
func (t *githubTarget) mergeableState(ctx context.Context, n int, want func(*bool) bool) error {
	for i := 0; i < 30; i++ {
		var pr struct {
			Mergeable *bool `json:"mergeable"`
		}
		if _, err := t.api.Do(ctx, http.MethodGet, t.r(fmt.Sprintf("/pulls/%d", n)), nil, &pr); err != nil {
			return err
		}
		if want(pr.Mergeable) {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("github: pull request %d never reached the expected mergeable state", n)
}

func (t *githubTarget) waitMergeable(ctx context.Context, n int) error {
	return t.mergeableState(ctx, n, func(m *bool) bool { return m != nil && *m })
}

func (t *githubTarget) waitUnmergeable(ctx context.Context, n int) error {
	return t.mergeableState(ctx, n, func(m *bool) bool { return m != nil && !*m })
}

func (t *githubTarget) seedAudits(ctx context.Context, n int, head string) error {
	if err := t.comment(ctx, n, "looks good to me"); err != nil {
		return err
	}
	stale, err := scm.EncodeAudit(scm.Audit{
		Lens: "stale-lens", Verdict: scm.AuditCleared, SHA: conformance.StaleSHA,
		Attempts: []string{"tried the previous head"}, Notes: "no escape found",
	})
	if err != nil {
		return err
	}
	if err := t.comment(ctx, n, stale); err != nil {
		return err
	}
	current, err := scm.EncodeAudit(scm.Audit{
		Lens: "scope-escape", Verdict: scm.AuditCleared, SHA: head,
		Attempts: []string{
			"reverted the guard and confirmed the new test goes red",
			"fed the guard a path that only mentions the pattern in a comment",
		},
		Notes: "no escape found",
	})
	if err != nil {
		return err
	}
	return t.comment(ctx, n, current)
}

func (t *githubTarget) redactor(number int, head, stale string, asReviewer bool) *httpfixture.Redactor {
	r := httpfixture.NewRedactor()
	if number > 0 {
		r.MapPattern(regexp.MustCompile(fmt.Sprintf(`"number":%d\b`, number)), fmt.Sprintf(`"number":%d`, placeholderChangeIID))
		r.MapPattern(regexp.MustCompile(fmt.Sprintf(`/pulls/%d\b`, number)), fmt.Sprintf("/pulls/%d", placeholderChangeIID))
		r.MapPattern(regexp.MustCompile(fmt.Sprintf(`/issues/%d\b`, number)), fmt.Sprintf("/issues/%d", placeholderChangeIID))
		r.MapPattern(regexp.MustCompile(fmt.Sprintf(`/pull/%d\b`, number)), fmt.Sprintf("/pull/%d", placeholderChangeIID))
	}
	// node_id is a string and id is a number. One pattern for both stripped the
	// quotes and turned a string field into a bare number, which produced a
	// fixture that no longer decodes -- a corrupted recording that was still
	// valid JSON.
	r.MapPattern(regexp.MustCompile(`"node_id":\s*"[^"]*"`), `"node_id":"PR_kwDO42"`)
	r.MapPattern(regexp.MustCompile(`"(id|user_id|repository_id|installation_id)":\s*\d+`), `"$1":1001`)
	if head != "" {
		r.Map(head, conformance.HeadSHA)
	}
	if stale != "" {
		r.Map(stale, conformance.StaleSHA)
	}
	r.Map(t.owner+"/"+t.repo, placeholderOwner+"/"+placeholderRepo)
	r.Map(t.repo, placeholderRepo)
	r.Map(t.owner, placeholderOwner)
	if asReviewer {
		r.Map(t.username, conformance.ReviewerName)
	} else {
		r.Map(t.username, conformance.ProducerName)
	}
	return r
}
