// Package gitlab implements scm.Driver against the GitLab REST API.
//
// One case here is the origin of the whole project: GitLab answers an
// unmergeable merge request with HTTP 405 and the body
// {"message":"Branch cannot be merged"}. v1 printed that message and returned
// exit status 0, and the caller read $? as success. Here it is a typed refusal.
package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/scm/httpjson"
)

// Config configures a GitLab driver.
type Config struct {
	// BaseURL is the API root; defaults to https://gitlab.com/api/v4.
	BaseURL string
	// Project is the numeric id or the full path ("group/subgroup/project").
	Project string
	Token   string
	// RemoveSourceBranch asks GitLab to delete the source branch on merge.
	RemoveSourceBranch bool
	HTTPClient         *http.Client
}

// Driver is the GitLab implementation of scm.Driver.
type Driver struct {
	c        *httpjson.Client
	project  string // already URL-escaped
	rmSource bool
}

var _ scm.Driver = (*Driver)(nil)

// New builds a Driver.
func New(cfg Config) (*Driver, error) {
	if cfg.Project == "" {
		return nil, fmt.Errorf("gitlab: project is required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("gitlab: token is empty; refusing to operate unauthenticated")
	}
	base := cfg.BaseURL
	if base == "" {
		base = "https://gitlab.com/api/v4"
	}
	h := http.Header{}
	h.Set("PRIVATE-TOKEN", cfg.Token)
	h.Set("User-Agent", "factoryd")

	return &Driver{
		c:        &httpjson.Client{Base: base, HTTP: cfg.HTTPClient, Header: h},
		project:  url.PathEscape(cfg.Project),
		rmSource: cfg.RemoveSourceBranch,
	}, nil
}

func (d *Driver) Provider() string { return "gitlab" }

func (d *Driver) projPath(suffix string) string {
	return "/projects/" + d.project + suffix
}

func (d *Driver) mrPath(id scm.ChangeID, suffix string) string {
	return d.projPath("/merge_requests/" + url.PathEscape(string(id)) + suffix)
}

// ---------- wire types ----------

type glMR struct {
	IID          int       `json:"iid"`
	Title        string    `json:"title"`
	Author       glUser    `json:"author"`
	SourceBranch string    `json:"source_branch"`
	TargetBranch string    `json:"target_branch"`
	SHA          string    `json:"sha"`
	Draft        bool      `json:"draft"`
	WorkInProg   bool      `json:"work_in_progress"`
	State        string    `json:"state"`
	WebURL       string    `json:"web_url"`
	Labels       []string  `json:"labels"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type glUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

func (m glMR) toChange() scm.Change {
	st := scm.StateUnknown
	switch m.State {
	case "opened", "locked":
		st = scm.StateOpen
	case "merged":
		st = scm.StateMerged
	case "closed":
		st = scm.StateClosed
	}
	return scm.Change{
		ID:           scm.ChangeID(strconv.Itoa(m.IID)),
		Title:        m.Title,
		Author:       m.Author.Username,
		SourceBranch: m.SourceBranch,
		TargetBranch: m.TargetBranch,
		HeadSHA:      m.SHA,
		// GitLab drafts are a title prefix; older instances report only
		// work_in_progress. Either flag means draft.
		Draft:     m.Draft || m.WorkInProg || isDraftTitle(m.Title),
		State:     st,
		WebURL:    m.WebURL,
		Labels:    m.Labels,
		UpdatedAt: m.UpdatedAt,
	}
}

const draftPrefix = "Draft: "

func isDraftTitle(t string) bool {
	u := strings.ToLower(strings.TrimSpace(t))
	return strings.HasPrefix(u, "draft:") || strings.HasPrefix(u, "wip:")
}

func stripDraftTitle(t string) string {
	s := strings.TrimSpace(t)
	for isDraftTitle(s) {
		if i := strings.Index(s, ":"); i >= 0 {
			s = strings.TrimSpace(s[i+1:])
		} else {
			break
		}
	}
	return s
}

// nextPage reads GitLab's pagination header. GitLab returns an empty
// x-next-page on the last page rather than omitting it.
func nextPage(h http.Header) string {
	return strings.TrimSpace(h.Get("x-next-page"))
}

// ---------- read verbs ----------

func (d *Driver) ListOpen(ctx context.Context) ([]scm.Change, error) {
	var out []scm.Change
	page := "1"
	for page != "" {
		var mrs []glMR
		resp, err := d.c.Do(ctx, http.MethodGet,
			d.projPath("/merge_requests?per_page=100&state=opened&page="+url.QueryEscape(page)), nil, &mrs)
		if err != nil {
			return nil, fmt.Errorf("gitlab list-open: %w", err)
		}
		for _, m := range mrs {
			out = append(out, m.toChange())
		}
		page = nextPage(resp.Header)
	}
	return out, nil
}

func (d *Driver) get(ctx context.Context, id scm.ChangeID) (glMR, error) {
	var m glMR
	if _, err := d.c.Do(ctx, http.MethodGet, d.mrPath(id, ""), nil, &m); err != nil {
		return glMR{}, fmt.Errorf("gitlab get %s: %w", id, err)
	}
	if m.IID == 0 {
		return glMR{}, fmt.Errorf("gitlab get %s: response carried no merge request iid", id)
	}
	return m, nil
}

func (d *Driver) Get(ctx context.Context, id scm.ChangeID) (scm.Change, error) {
	m, err := d.get(ctx, id)
	if err != nil {
		return scm.Change{}, err
	}
	return m.toChange(), nil
}

func (d *Driver) Diff(ctx context.Context, id scm.ChangeID) ([]scm.FileDiff, error) {
	type glDiff struct {
		OldPath     string `json:"old_path"`
		NewPath     string `json:"new_path"`
		NewFile     bool   `json:"new_file"`
		RenamedFile bool   `json:"renamed_file"`
		DeletedFile bool   `json:"deleted_file"`
		Diff        string `json:"diff"`
	}
	var out []scm.FileDiff
	page := "1"
	for page != "" {
		var diffs []glDiff
		resp, err := d.c.Do(ctx, http.MethodGet,
			d.mrPath(id, "/diffs?per_page=100&page="+url.QueryEscape(page)), nil, &diffs)
		if err != nil {
			return nil, fmt.Errorf("gitlab diff %s: %w", id, err)
		}
		for _, f := range diffs {
			added, removed := countHunkLines(f.Diff)
			fd := scm.FileDiff{
				Path:    f.NewPath,
				Added:   added,
				Removed: removed,
				New:     f.NewFile,
				Deleted: f.DeletedFile,
				Renamed: f.RenamedFile,
				Patch:   f.Diff,
			}
			if f.RenamedFile {
				fd.OldPath = f.OldPath
			}
			if f.DeletedFile {
				fd.Path = f.OldPath
			}
			out = append(out, fd)
		}
		page = nextPage(resp.Header)
	}
	return out, nil
}

// countHunkLines derives added/removed counts from a unified diff, because
// GitLab does not report them. Lines inside the hunk body only; "+++"/"---"
// file headers are not content.
func countHunkLines(patch string) (added, removed int) {
	inHunk := false
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case !inHunk:
			continue
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
			continue
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return added, removed
}

func (d *Driver) Pipeline(ctx context.Context, id scm.ChangeID) (scm.PipelineStatus, error) {
	m, err := d.get(ctx, id)
	if err != nil {
		return scm.PipelineStatus{}, err
	}
	type glPipeline struct {
		ID     int64  `json:"id"`
		SHA    string `json:"sha"`
		Status string `json:"status"`
		WebURL string `json:"web_url"`
	}
	var pipelines []glPipeline
	if _, err := d.c.Do(ctx, http.MethodGet, d.mrPath(id, "/pipelines?per_page=100"), nil, &pipelines); err != nil {
		return scm.PipelineStatus{}, fmt.Errorf("gitlab pipeline %s: %w", id, err)
	}

	st := scm.PipelineStatus{SHA: m.SHA}
	// Only pipelines for the current head say anything about the current head.
	var latest *glPipeline
	for i := range pipelines {
		if pipelines[i].SHA != m.SHA {
			continue
		}
		st.Count++
		if latest == nil || pipelines[i].ID > latest.ID {
			latest = &pipelines[i]
		}
	}
	if latest == nil {
		// No pipeline for this commit. Nothing ran, so nothing passed.
		st.State = scm.PipelineNone
		return st, nil
	}
	st.WebURL = latest.WebURL

	switch latest.Status {
	case "success":
		st.State = scm.PipelineSuccess
	case "failed":
		st.State = scm.PipelineFailed
	case "canceled", "cancelled", "canceling":
		st.State = scm.PipelineCanceled
	case "running":
		st.State = scm.PipelineRunning
	case "created", "waiting_for_resource", "preparing", "pending", "scheduled", "manual":
		st.State = scm.PipelinePending
	case "skipped":
		// A skipped pipeline is not a passed pipeline.
		st.State = scm.PipelineNone
	default:
		return scm.PipelineStatus{}, fmt.Errorf(
			"gitlab pipeline %s: unrecognised pipeline status %q", id, latest.Status)
	}
	return st, nil
}

func (d *Driver) Whoami(ctx context.Context) (scm.Identity, error) {
	var u glUser
	if _, err := d.c.Do(ctx, http.MethodGet, "/user", nil, &u); err != nil {
		return scm.Identity{}, fmt.Errorf("gitlab whoami: %w", err)
	}
	if u.ID == 0 || u.Username == "" {
		return scm.Identity{}, fmt.Errorf("gitlab whoami: response carried no id or username")
	}
	return scm.Identity{ID: strconv.FormatInt(u.ID, 10), Login: u.Username, Name: u.Name}, nil
}

func (d *Driver) IsAncestor(ctx context.Context, sha, ref string) (bool, error) {
	if sha == "" || ref == "" {
		return false, fmt.Errorf("gitlab is-ancestor: sha and ref are both required")
	}
	var base struct {
		ID string `json:"id"`
	}
	path := d.projPath("/repository/merge_base?refs%5B%5D=" + url.QueryEscape(ref) + "&refs%5B%5D=" + url.QueryEscape(sha))
	if _, err := d.c.Do(ctx, http.MethodGet, path, nil, &base); err != nil {
		return false, fmt.Errorf("gitlab is-ancestor %s..%s: %w", ref, sha, err)
	}
	if base.ID == "" {
		return false, fmt.Errorf("gitlab is-ancestor %s..%s: no merge base returned", ref, sha)
	}
	// sha is an ancestor of ref exactly when the merge base is sha itself.
	return base.ID == sha, nil
}

// ---------- write verbs ----------

func (d *Driver) Comment(ctx context.Context, id scm.ChangeID, body string) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("gitlab comment %s: body is empty", id)
	}
	if _, err := d.c.Do(ctx, http.MethodPost, d.mrPath(id, "/notes"), map[string]string{"body": body}, nil); err != nil {
		return fmt.Errorf("gitlab comment %s: %w", id, err)
	}
	return nil
}

func (d *Driver) SetDraft(ctx context.Context, id scm.ChangeID, draft bool) error {
	m, err := d.get(ctx, id)
	if err != nil {
		return err
	}
	cur := m.toChange().Draft
	if cur == draft {
		return nil
	}
	title := stripDraftTitle(m.Title)
	if draft {
		title = draftPrefix + title
	}
	if title == "" {
		return fmt.Errorf("gitlab set-draft %s: refusing to set an empty title", id)
	}

	var updated glMR
	if _, err := d.c.Do(ctx, http.MethodPut, d.mrPath(id, ""), map[string]string{"title": title}, &updated); err != nil {
		return fmt.Errorf("gitlab set-draft %s: %w", id, err)
	}
	// GitLab answers 200 with the object; confirm the change took rather than
	// trusting the status code.
	if got := updated.toChange().Draft; got != draft {
		return fmt.Errorf("gitlab set-draft %s: requested draft=%v but merge request is draft=%v (title %q)",
			id, draft, got, updated.Title)
	}
	return nil
}

func (d *Driver) Merge(ctx context.Context, id scm.ChangeID, expectedHead string) (scm.MergeResult, error) {
	if expectedHead == "" {
		return scm.MergeResult{}, fmt.Errorf("gitlab merge %s: expected head sha is required", id)
	}
	m, err := d.get(ctx, id)
	if err != nil {
		return scm.MergeResult{}, err
	}
	if c := m.toChange(); c.Draft {
		return scm.Refused(scm.RefusedDraft, "merge request %s is a draft (title %q)", id, m.Title), nil
	}
	if m.SHA != expectedHead {
		return scm.Refused(scm.RefusedConflict,
			"head moved: expected %s, merge request is at %s", expectedHead, m.SHA), nil
	}

	var out struct {
		State          string `json:"state"`
		MergeCommitSHA string `json:"merge_commit_sha"`
		SquashCommit   string `json:"squash_commit_sha"`
		Message        string `json:"message"`
	}
	body := map[string]any{
		"sha":                         expectedHead,
		"should_remove_source_branch": d.rmSource,
		"squash":                      false,
	}
	_, err = d.c.Do(ctx, http.MethodPut, d.mrPath(id, "/merge"), body, &out)
	if err != nil {
		var he *httpjson.Error
		if httpjson.AsError(err, &he) {
			switch he.Status {
			case http.StatusMethodNotAllowed:
				// "Branch cannot be merged" -- the v1 bug, now a typed refusal.
				return scm.Refused(scm.RefusedConflict, "gitlab refused the merge: %s", he.Message()), nil
			case http.StatusNotAcceptable:
				return scm.Refused(scm.RefusedConflict, "gitlab reported a conflict: %s", he.Message()), nil
			case http.StatusConflict:
				return scm.Refused(scm.RefusedConflict, "head sha mismatch: %s", he.Message()), nil
			case http.StatusUnprocessableEntity:
				return scm.Refused(scm.RefusedPipeline, "gitlab would not merge: %s", he.Message()), nil
			}
		}
		return scm.MergeResult{}, fmt.Errorf("gitlab merge %s: %w", id, err)
	}

	commit := out.MergeCommitSHA
	if commit == "" {
		commit = out.SquashCommit
	}
	// GitLab can answer 200 with the merge request in a non-merged state.
	// The status code is not the outcome.
	if out.State != "merged" || commit == "" {
		return scm.Refused(scm.MergeUnknown,
			"gitlab returned 200 but state=%q merge_commit_sha=%q message=%q", out.State, commit, out.Message), nil
	}
	return scm.MergeResult{Outcome: scm.Merged, MergeCommit: commit}, nil
}

// ---------- audits ----------

func (d *Driver) PostAudit(ctx context.Context, id scm.ChangeID, sha string, a scm.Audit) error {
	a.SHA = sha
	body, err := scm.EncodeAudit(a)
	if err != nil {
		return fmt.Errorf("gitlab post-audit %s: %w", id, err)
	}
	return d.Comment(ctx, id, body)
}

func (d *Driver) Audits(ctx context.Context, id scm.ChangeID, sha string) ([]scm.Audit, error) {
	type glNote struct {
		Body   string `json:"body"`
		Author glUser `json:"author"`
		System bool   `json:"system"`
	}
	var all []scm.Audit
	page := "1"
	for page != "" {
		var notes []glNote
		resp, err := d.c.Do(ctx, http.MethodGet,
			d.mrPath(id, "/notes?per_page=100&page="+url.QueryEscape(page)), nil, &notes)
		if err != nil {
			return nil, fmt.Errorf("gitlab audits %s: %w", id, err)
		}
		for _, n := range notes {
			if n.System {
				continue
			}
			a, ok, err := scm.ParseAudit(n.Body)
			if err != nil {
				return nil, fmt.Errorf("gitlab audits %s: malformed audit note: %w", id, err)
			}
			if !ok {
				continue
			}
			if a.PostedBy == "" {
				a.PostedBy = n.Author.Username
			}
			all = append(all, a)
		}
		page = nextPage(resp.Header)
	}
	return scm.SelectAudits(all, sha), nil
}
