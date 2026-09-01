// Package github implements scm.Driver against the GitHub REST API (plus one
// GraphQL call, because draft state has no REST verb).
package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/scm/httpjson"
)

// Config configures a GitHub driver.
type Config struct {
	// BaseURL is the REST root; defaults to https://api.github.com.
	BaseURL string
	// GraphQLURL defaults to BaseURL with /graphql appended (api.github.com)
	// or, for GitHub Enterprise, the /api/graphql sibling.
	GraphQLURL string
	Owner      string
	Repo       string
	Token      string
	// MergeMethod is one of merge, squash, rebase. Defaults to merge.
	MergeMethod string
	HTTPClient  *http.Client
}

// Driver is the GitHub implementation of scm.Driver.
type Driver struct {
	c           *httpjson.Client
	graphqlURL  string
	owner       string
	repo        string
	mergeMethod string
}

var _ scm.Driver = (*Driver)(nil)

// New builds a Driver. It validates configuration eagerly: a driver missing an
// owner or a token is a configuration error the operator should see at boot,
// not a 404 during a merge.
func New(cfg Config) (*Driver, error) {
	if cfg.Owner == "" || cfg.Repo == "" {
		return nil, fmt.Errorf("github: owner and repo are required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("github: token is empty; refusing to operate unauthenticated")
	}
	base := cfg.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	gql := cfg.GraphQLURL
	if gql == "" {
		gql = strings.TrimSuffix(base, "/") + "/graphql"
	}
	mm := cfg.MergeMethod
	if mm == "" {
		mm = "merge"
	}
	switch mm {
	case "merge", "squash", "rebase":
	default:
		return nil, fmt.Errorf("github: merge_method %q is not one of merge, squash, rebase", mm)
	}

	h := http.Header{}
	h.Set("Authorization", "Bearer "+cfg.Token)
	h.Set("Accept", "application/vnd.github+json")
	h.Set("X-GitHub-Api-Version", "2022-11-28")
	h.Set("User-Agent", "factoryd")

	return &Driver{
		c:           &httpjson.Client{Base: base, HTTP: cfg.HTTPClient, Header: h},
		graphqlURL:  gql,
		owner:       cfg.Owner,
		repo:        cfg.Repo,
		mergeMethod: mm,
	}, nil
}

func (d *Driver) Provider() string { return "github" }

func (d *Driver) repoPath(suffix string) string {
	return fmt.Sprintf("/repos/%s/%s%s", d.owner, d.repo, suffix)
}

// ---------- wire types ----------

type ghUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
}

type ghLabel struct {
	Name string `json:"name"`
}

type ghRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type ghPull struct {
	NodeID    string    `json:"node_id"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	User      ghUser    `json:"user"`
	Head      ghRef     `json:"head"`
	Base      ghRef     `json:"base"`
	Draft     bool      `json:"draft"`
	State     string    `json:"state"`
	MergedAt  *string   `json:"merged_at"`
	HTMLURL   string    `json:"html_url"`
	Labels    []ghLabel `json:"labels"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (p ghPull) toChange() scm.Change {
	st := scm.StateUnknown
	switch p.State {
	case "open":
		st = scm.StateOpen
	case "closed":
		if p.MergedAt != nil && *p.MergedAt != "" {
			st = scm.StateMerged
		} else {
			st = scm.StateClosed
		}
	}
	labels := make([]string, 0, len(p.Labels))
	for _, l := range p.Labels {
		labels = append(labels, l.Name)
	}
	return scm.Change{
		ID:           scm.ChangeID(fmt.Sprint(p.Number)),
		Title:        p.Title,
		Author:       p.User.Login,
		SourceBranch: p.Head.Ref,
		TargetBranch: p.Base.Ref,
		HeadSHA:      p.Head.SHA,
		Draft:        p.Draft,
		State:        st,
		WebURL:       p.HTMLURL,
		Labels:       labels,
		UpdatedAt:    p.UpdatedAt,
	}
}

// ---------- read verbs ----------

func (d *Driver) ListOpen(ctx context.Context) ([]scm.Change, error) {
	path := d.repoPath("/pulls?per_page=100&state=open")
	var out []scm.Change
	for path != "" {
		var page []ghPull
		resp, err := d.c.Do(ctx, http.MethodGet, path, nil, &page)
		if err != nil {
			return nil, fmt.Errorf("github list-open: %w", err)
		}
		for _, p := range page {
			out = append(out, p.toChange())
		}
		path = httpjson.NextLink(resp.Header)
	}
	return out, nil
}

func (d *Driver) get(ctx context.Context, id scm.ChangeID) (ghPull, error) {
	var p ghPull
	_, err := d.c.Do(ctx, http.MethodGet, d.repoPath("/pulls/"+url.PathEscape(string(id))), nil, &p)
	if err != nil {
		return ghPull{}, fmt.Errorf("github get %s: %w", id, err)
	}
	if p.Number == 0 {
		return ghPull{}, fmt.Errorf("github get %s: response carried no pull request number", id)
	}
	return p, nil
}

func (d *Driver) Get(ctx context.Context, id scm.ChangeID) (scm.Change, error) {
	p, err := d.get(ctx, id)
	if err != nil {
		return scm.Change{}, err
	}
	return p.toChange(), nil
}

func (d *Driver) Diff(ctx context.Context, id scm.ChangeID) ([]scm.FileDiff, error) {
	type ghFile struct {
		Filename         string `json:"filename"`
		PreviousFilename string `json:"previous_filename"`
		Additions        int    `json:"additions"`
		Deletions        int    `json:"deletions"`
		Status           string `json:"status"`
		Patch            string `json:"patch"`
	}
	path := d.repoPath("/pulls/" + url.PathEscape(string(id)) + "/files?per_page=100")
	var out []scm.FileDiff
	for path != "" {
		var page []ghFile
		resp, err := d.c.Do(ctx, http.MethodGet, path, nil, &page)
		if err != nil {
			return nil, fmt.Errorf("github diff %s: %w", id, err)
		}
		for _, f := range page {
			out = append(out, scm.FileDiff{
				Path:    f.Filename,
				OldPath: f.PreviousFilename,
				Added:   f.Additions,
				Removed: f.Deletions,
				New:     f.Status == "added",
				Deleted: f.Status == "removed",
				Renamed: f.Status == "renamed",
				Patch:   f.Patch,
			})
		}
		path = httpjson.NextLink(resp.Header)
	}
	return out, nil
}

func (d *Driver) Pipeline(ctx context.Context, id scm.ChangeID) (scm.PipelineStatus, error) {
	p, err := d.get(ctx, id)
	if err != nil {
		return scm.PipelineStatus{}, err
	}
	if p.Head.SHA == "" {
		return scm.PipelineStatus{}, fmt.Errorf("github pipeline %s: pull request has no head sha", id)
	}

	type ghCheck struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
	}
	var body struct {
		TotalCount int       `json:"total_count"`
		CheckRuns  []ghCheck `json:"check_runs"`
	}
	_, err = d.c.Do(ctx, http.MethodGet,
		d.repoPath("/commits/"+url.PathEscape(p.Head.SHA)+"/check-runs?per_page=100"), nil, &body)
	if err != nil {
		return scm.PipelineStatus{}, fmt.Errorf("github pipeline %s: %w", id, err)
	}

	st := scm.PipelineStatus{SHA: p.Head.SHA, Count: len(body.CheckRuns)}
	if len(body.CheckRuns) == 0 {
		// Nothing ran. Not green -- see SPEC.md §9.4.
		st.State = scm.PipelineNone
		return st, nil
	}

	state := scm.PipelineSuccess
	worse := func(s scm.PipelineState) {
		rank := map[scm.PipelineState]int{
			scm.PipelineSuccess: 0, scm.PipelinePending: 1, scm.PipelineRunning: 2,
			scm.PipelineCanceled: 3, scm.PipelineFailed: 4,
		}
		if rank[s] > rank[state] {
			state = s
		}
	}
	for _, c := range body.CheckRuns {
		if st.WebURL == "" {
			st.WebURL = c.HTMLURL
		}
		switch c.Status {
		case "queued", "waiting", "requested", "pending":
			worse(scm.PipelinePending)
			continue
		case "in_progress":
			worse(scm.PipelineRunning)
			continue
		}
		switch c.Conclusion {
		case "success", "neutral", "skipped":
		case "cancelled", "canceled":
			worse(scm.PipelineCanceled)
		case "failure", "timed_out", "action_required", "startup_failure", "stale":
			worse(scm.PipelineFailed)
		default:
			// An unrecognised conclusion must not read as success.
			return scm.PipelineStatus{}, fmt.Errorf(
				"github pipeline %s: check run has unrecognised conclusion %q", id, c.Conclusion)
		}
	}
	st.State = state
	return st, nil
}

func (d *Driver) Whoami(ctx context.Context) (scm.Identity, error) {
	var u ghUser
	if _, err := d.c.Do(ctx, http.MethodGet, "/user", nil, &u); err != nil {
		return scm.Identity{}, fmt.Errorf("github whoami: %w", err)
	}
	if u.ID == 0 || u.Login == "" {
		return scm.Identity{}, fmt.Errorf("github whoami: response carried no id or login")
	}
	return scm.Identity{ID: fmt.Sprint(u.ID), Login: u.Login, Name: u.Name}, nil
}

func (d *Driver) IsAncestor(ctx context.Context, sha, ref string) (bool, error) {
	if sha == "" || ref == "" {
		return false, fmt.Errorf("github is-ancestor: sha and ref are both required")
	}
	var body struct {
		Status string `json:"status"`
	}
	_, err := d.c.Do(ctx, http.MethodGet,
		d.repoPath("/compare/"+url.PathEscape(ref)+"..."+url.PathEscape(sha)), nil, &body)
	if err != nil {
		return false, fmt.Errorf("github is-ancestor %s..%s: %w", ref, sha, err)
	}
	switch body.Status {
	case "identical", "behind":
		// sha is reachable from ref.
		return true, nil
	case "ahead", "diverged":
		return false, nil
	default:
		return false, fmt.Errorf("github is-ancestor %s..%s: unrecognised comparison status %q", ref, sha, body.Status)
	}
}

// ---------- write verbs ----------

func (d *Driver) Comment(ctx context.Context, id scm.ChangeID, body string) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("github comment %s: body is empty", id)
	}
	in := map[string]string{"body": body}
	_, err := d.c.Do(ctx, http.MethodPost,
		d.repoPath("/issues/"+url.PathEscape(string(id))+"/comments"), in, nil)
	if err != nil {
		return fmt.Errorf("github comment %s: %w", id, err)
	}
	return nil
}

func (d *Driver) SetDraft(ctx context.Context, id scm.ChangeID, draft bool) error {
	p, err := d.get(ctx, id)
	if err != nil {
		return err
	}
	if p.NodeID == "" {
		return fmt.Errorf("github set-draft %s: pull request has no node id", id)
	}
	if p.Draft == draft {
		return nil
	}

	mutation := "markPullRequestReadyForReview"
	if draft {
		mutation = "convertPullRequestToDraft"
	}
	query := fmt.Sprintf(
		`mutation($id:ID!){ %s(input:{pullRequestId:$id}){ pullRequest { isDraft } } }`, mutation)

	var out struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	_, err = d.c.Do(ctx, http.MethodPost, d.graphqlURL, map[string]any{
		"query":     query,
		"variables": map[string]any{"id": p.NodeID},
	}, &out)
	if err != nil {
		return fmt.Errorf("github set-draft %s: %w", id, err)
	}
	// GraphQL reports failure inside a 200. Treating that as success is exactly
	// the v1 class of bug this project exists to eliminate.
	if len(out.Errors) > 0 {
		return fmt.Errorf("github set-draft %s: %s", id, out.Errors[0].Message)
	}
	return nil
}

func (d *Driver) Merge(ctx context.Context, id scm.ChangeID, expectedHead string) (scm.ProviderMerge, error) {
	if expectedHead == "" {
		return scm.ProviderMerge{}, fmt.Errorf("github merge %s: expected head sha is required", id)
	}
	p, err := d.get(ctx, id)
	if err != nil {
		return scm.ProviderMerge{}, err
	}
	if p.Draft {
		return scm.RefusedByProvider(scm.RefusedDraft, "pull request %s is a draft", id), nil
	}
	if p.Head.SHA != expectedHead {
		return scm.RefusedByProvider(scm.RefusedConflict,
			"head moved: expected %s, pull request is at %s", expectedHead, p.Head.SHA), nil
	}

	var out struct {
		SHA     string `json:"sha"`
		Merged  bool   `json:"merged"`
		Message string `json:"message"`
	}
	_, err = d.c.Do(ctx, http.MethodPut,
		d.repoPath("/pulls/"+url.PathEscape(string(id))+"/merge"),
		map[string]string{"sha": expectedHead, "merge_method": d.mergeMethod}, &out)
	if err != nil {
		var he *httpjson.Error
		if httpjson.AsError(err, &he) {
			switch he.Status {
			case http.StatusMethodNotAllowed: // not mergeable
				return scm.RefusedByProvider(scm.RefusedConflict, "github refused the merge: %s", he.Message()), nil
			case http.StatusConflict: // head sha mismatch
				return scm.RefusedByProvider(scm.RefusedConflict, "github reported a conflict: %s", he.Message()), nil
			}
		}
		return scm.ProviderMerge{}, fmt.Errorf("github merge %s: %w", id, err)
	}

	// A 200 that does not say merged, or says merged without a commit, is not a
	// merge. v1 read the exit status and called this success.
	if !out.Merged || out.SHA == "" {
		return scm.RefusedByProvider(scm.MergeUnknown,
			"github returned 200 but merged=%v sha=%q message=%q", out.Merged, out.SHA, out.Message), nil
	}
	return scm.ProviderMerged(out.SHA), nil
}

// ---------- audits ----------

func (d *Driver) PostAudit(ctx context.Context, id scm.ChangeID, sha string, a scm.Audit) error {
	a.SHA = sha
	body, err := scm.EncodeAudit(a)
	if err != nil {
		return fmt.Errorf("github post-audit %s: %w", id, err)
	}
	return d.Comment(ctx, id, body)
}

func (d *Driver) Audits(ctx context.Context, id scm.ChangeID, sha string) ([]scm.Audit, error) {
	type ghComment struct {
		Body string `json:"body"`
		User ghUser `json:"user"`
	}
	path := d.repoPath("/issues/" + url.PathEscape(string(id)) + "/comments?per_page=100")
	var all []scm.Audit
	for path != "" {
		var page []ghComment
		resp, err := d.c.Do(ctx, http.MethodGet, path, nil, &page)
		if err != nil {
			return nil, fmt.Errorf("github audits %s: %w", id, err)
		}
		for _, c := range page {
			a, ok, err := scm.ParseAudit(c.Body)
			if err != nil {
				return nil, fmt.Errorf("github audits %s: malformed audit comment: %w", id, err)
			}
			if !ok {
				continue
			}
			if a.PostedBy == "" {
				a.PostedBy = c.User.Login
			}
			all = append(all, a)
		}
		path = httpjson.NextLink(resp.Header)
	}
	return scm.SelectAudits(all, sha), nil
}
