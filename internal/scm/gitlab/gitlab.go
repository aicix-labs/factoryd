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
	// ChangesCount is GitLab's own count, as a string ("3", "1000+"). It is
	// how "the diff is empty" is told apart from "the diff is not computed
	// yet": right after an MR is opened, the diffs endpoint returns nothing
	// while this says otherwise.
	ChangesCount string `json:"changes_count"`
	DiffRefs     *struct {
		HeadSHA string `json:"head_sha"`
		BaseSHA string `json:"base_sha"`
	} `json:"diff_refs"`
	IID          int    `json:"iid"`
	Title        string `json:"title"`
	Author       glUser `json:"author"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	SHA          string `json:"sha"`
	Draft        bool   `json:"draft"`
	WorkInProg   bool   `json:"work_in_progress"`
	State        string `json:"state"`
	// DetailedMergeStatus is GitLab's explicit reason an otherwise-open MR
	// cannot merge. HTTP refusal status is not reliable enough to distinguish
	// a true conflict from CI that can clear on its own.
	DetailedMergeStatus string    `json:"detailed_merge_status"`
	WebURL              string    `json:"web_url"`
	Labels              []string  `json:"labels"`
	UpdatedAt           time.Time `json:"updated_at"`
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
		AuthorID:     userID(m.Author.ID),
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
	// GitLab computes an MR's diff asynchronously. Observed live: the diffs
	// endpoint returned an empty list seconds after the MR was opened, while
	// the MR itself reported changes. An empty list is therefore accepted
	// only when the MR says it has no changes; otherwise the read is retried
	// for a bounded time and then fails. Reporting "no changes" for a diff
	// that is not ready would let a merge gate classify against nothing.
	// The MR itself is read lazily: only an empty list, or a renamed file
	// with an empty diff, needs it (its change count judges the first, its
	// diff refs prove the second). The common case makes no extra request.
	var m *glMR
	getMR := func() (glMR, error) {
		if m != nil {
			return *m, nil
		}
		got, err := d.get(ctx, id)
		if err != nil {
			return glMR{}, fmt.Errorf("gitlab diff %s: %w", id, err)
		}
		m = &got
		return got, nil
	}
	for attempt := 0; ; attempt++ {
		out, err := d.readDiffs(ctx, id, getMR)
		if err != nil {
			return nil, err
		}
		if len(out) > 0 {
			return out, nil
		}
		m, err := getMR()
		if err != nil {
			return nil, err
		}
		if m.ChangesCount == "0" && m.DiffRefs != nil {
			return out, nil // genuinely empty, and the diff is computed
		}
		if attempt >= diffSettleAttempts {
			return nil, fmt.Errorf("gitlab diff %s: the diff is not available yet (changes_count %q); refusing to report it as empty", id, m.ChangesCount)
		}
		if err := diffSettleSleep(ctx, attempt); err != nil {
			return nil, err
		}
	}
}

// diffSettleAttempts and diffSettleSleep bound the wait; tests shorten it.
var (
	diffSettleAttempts = 8
	diffSettleSleep    = func(ctx context.Context, attempt int) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(250*(attempt+1)) * time.Millisecond):
			return nil
		}
	}
)

func (d *Driver) readDiffs(ctx context.Context, id scm.ChangeID, getMR func() (glMR, error)) ([]scm.FileDiff, error) {
	type glDiff struct {
		OldPath     string `json:"old_path"`
		NewPath     string `json:"new_path"`
		NewFile     bool   `json:"new_file"`
		RenamedFile bool   `json:"renamed_file"`
		DeletedFile bool   `json:"deleted_file"`
		Diff        string `json:"diff"`
		// GitLab documents two incomplete states: a collapsed diff (content
		// withheld, fetch it separately) and one too large to render.
		Collapsed bool `json:"collapsed"`
		TooLarge  bool `json:"too_large"`
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
			switch {
			case f.TooLarge:
				fd.Incomplete, fd.IncompleteReason = true, "too_large: GitLab did not deliver the content"
			case f.Collapsed:
				fd.Incomplete, fd.IncompleteReason = true, "collapsed: GitLab withheld the content"
			case f.Diff == "" && f.DeletedFile:
				// nothing to deliver
			case f.Diff == "" && f.RenamedFile:
				// A rename with an empty diff is either path-only or a
				// rename whose content changes were not delivered. GitLab
				// gives no per-file counts, so purity is PROVED: the blob at
				// the old path on the base must be the blob at the new path
				// on the head. Anything else -- or a lookup that fails -- is
				// incomplete. A rename is where a blank patch hides the most.
				if m, err := getMR(); err != nil {
					fd.Incomplete, fd.IncompleteReason = true, "renamed with an empty diff; the merge request could not be read: "+err.Error()
				} else if reason := d.renameNotPure(ctx, m, f.OldPath, f.NewPath); reason != "" {
					fd.Incomplete, fd.IncompleteReason = true, reason
				}
			case f.Diff == "":
				fd.Incomplete, fd.IncompleteReason = true, "empty patch for a changed file"
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
	return d.whoami(ctx, d.c)
}

// WhoamiWith answers for a different secret through a client that shares
// everything but the PRIVATE-TOKEN header.
func (d *Driver) WhoamiWith(ctx context.Context, secret string) (scm.Identity, error) {
	if secret == "" {
		return scm.Identity{}, fmt.Errorf("gitlab whoami: empty secret; identity is undecided")
	}
	h := d.c.Header.Clone()
	h.Set("PRIVATE-TOKEN", secret)
	return d.whoami(ctx, &httpjson.Client{Base: d.c.Base, HTTP: d.c.HTTP, Header: h})
}

// GitCredential: GitLab's convention for a personal, project or group access
// token over HTTPS is the literal username "oauth2" with the token as the
// password. The host in the incident behind SPEC.md §1 item 11 had entries
// under both "oauth2" and a bot username; this is the one that is documented.
func (d *Driver) GitCredential(secret string) scm.GitCredential {
	return scm.GitCredential{Username: "oauth2", Secret: secret}
}

func (d *Driver) whoami(ctx context.Context, c *httpjson.Client) (scm.Identity, error) {
	var u glUser
	if _, err := c.Do(ctx, http.MethodGet, "/user", nil, &u); err != nil {
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

func (d *Driver) FindOpenBySource(ctx context.Context, branch string) (scm.Change, bool, error) {
	var mrs []glMR
	path := d.projPath("/merge_requests?state=opened&per_page=100&source_branch=" + url.QueryEscape(branch))
	if _, err := d.c.Do(ctx, http.MethodGet, path, nil, &mrs); err != nil {
		return scm.Change{}, false, fmt.Errorf("gitlab find-open %s: %w", branch, err)
	}
	for _, m := range mrs {
		if m.SourceBranch == branch {
			return m.toChange(), true, nil
		}
	}
	return scm.Change{}, false, nil
}

func (d *Driver) OpenDraft(ctx context.Context, spec scm.DraftSpec) (scm.Change, error) {
	if spec.SourceBranch == "" || spec.TargetBranch == "" || spec.Title == "" {
		return scm.Change{}, fmt.Errorf("gitlab open-draft: source, target and title are required")
	}
	title := spec.Title
	if !isDraftTitle(title) {
		title = draftPrefix + title
	}
	var m glMR
	_, err := d.c.Do(ctx, http.MethodPost, d.projPath("/merge_requests"), map[string]any{
		"source_branch": spec.SourceBranch, "target_branch": spec.TargetBranch,
		"title": title, "description": spec.Body,
	}, &m)
	if err != nil {
		return scm.Change{}, fmt.Errorf("gitlab open-draft %s: %w", spec.SourceBranch, err)
	}
	c := m.toChange()
	if !c.Draft {
		return scm.Change{}, fmt.Errorf("gitlab open-draft %s: the provider opened !%s as ready, not as a draft; refusing to continue", spec.SourceBranch, c.ID)
	}
	return c, nil
}

func (d *Driver) Close(ctx context.Context, id scm.ChangeID, comment string) error {
	if strings.TrimSpace(comment) != "" {
		if err := d.Comment(ctx, id, comment); err != nil {
			return err
		}
	}
	var m glMR
	if _, err := d.c.Do(ctx, http.MethodPut, d.mrPath(id, ""), map[string]string{"state_event": "close"}, &m); err != nil {
		return fmt.Errorf("gitlab close %s: %w", id, err)
	}
	if m.State != "closed" {
		return fmt.Errorf("gitlab close %s: provider reports state %q after close", id, m.State)
	}
	return nil
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

func (d *Driver) Merge(ctx context.Context, id scm.ChangeID, expectedHead string) (scm.ProviderMerge, error) {
	if expectedHead == "" {
		return scm.ProviderMerge{}, fmt.Errorf("gitlab merge %s: expected head sha is required", id)
	}
	m, err := d.get(ctx, id)
	if err != nil {
		return scm.ProviderMerge{}, err
	}
	if c := m.toChange(); c.Draft {
		return scm.RefusedByProvider(scm.RefusedDraft, "merge request %s is a draft (title %q)", id, m.Title), nil
	}
	if m.SHA != expectedHead {
		return scm.RefusedByProvider(scm.RefusedConflict,
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
			case http.StatusMethodNotAllowed, // 405: not in a mergeable state
				http.StatusNotAcceptable,       // 406: conflict, older instances
				http.StatusConflict,            // 409: the head moved
				http.StatusUnprocessableEntity: // 422: "Branch cannot be merged"
				//
				// All four are "the provider refused", and the status code does
				// not reliably say why.
				//
				// This mapping is recorded, not assumed. Against GitLab 18.8.2
				// a genuine conflict answers 422 with "Branch cannot be merged"
				// (see testdata/merge_refused_unmergeable.json), while 405 is
				// what comes back when mergeability has not been computed yet.
				// The hand-written fixture this replaced said 405 meant the
				// conflict and 422 meant CI, so a real conflict was reported as
				// RefusedPipeline -- the wrong reason, with the suite green.
				//
				// The detailed status read before PUT is only a hint. A push or
				// conflict can win that gap, so a possible CI retry must re-read
				// the MR after the refusal and prove it still names the exact head
				// we tried. A non-pipeline status needs no second read because it
				// cannot schedule work on the old fact.
				detailed := m.DetailedMergeStatus
				if mergeRefusalOutcome(detailed) == scm.RefusedPipeline {
					current, rereadErr := d.get(ctx, id)
					if rereadErr != nil {
						return scm.ProviderMerge{}, fmt.Errorf("gitlab re-reading %s after merge refusal: %w", id, rereadErr)
					}
					if current.SHA != expectedHead {
						return scm.RefusedByProvider(scm.RefusedConflict,
							"head moved after gitlab refused the merge: expected %s, merge request is at %s", expectedHead, current.SHA), nil
					}
					detailed = current.DetailedMergeStatus
				}
				// The post-refusal detailed status is the provider's stated reason.
				// Unlike the HTTP refusal status, it distinguishes CI that may
				// clear on its own from a conflict that needs an actor.
				outcome := mergeRefusalOutcome(detailed)
				return scm.RefusedByProvider(outcome,
					"gitlab refused the merge (HTTP %d, detailed_merge_status=%q): %s", he.Status, detailed, he.Message()), nil
			}
		}
		return scm.ProviderMerge{}, fmt.Errorf("gitlab merge %s: %w", id, err)
	}

	commit := out.MergeCommitSHA
	if commit == "" {
		commit = out.SquashCommit
	}
	// GitLab can answer 200 with the merge request in a non-merged state.
	// The status code is not the outcome.
	if out.State != "merged" || commit == "" {
		return scm.RefusedByProvider(scm.MergeUnknown,
			"gitlab returned 200 but state=%q merge_commit_sha=%q message=%q", out.State, commit, out.Message), nil
	}
	return scm.ProviderMerged(commit), nil
}

// mergeRefusalOutcome maps only GitLab's explicit CI/merge-computation states
// to RefusedPipeline. All other values, including unknown future statuses,
// stay RefusedConflict: guessing that an unrecognised refusal is self-clearing
// would retry a draft that needs a human to resolve it.
func mergeRefusalOutcome(detailed string) scm.MergeOutcome {
	switch strings.ToLower(strings.TrimSpace(detailed)) {
	case "ci_must_pass", "ci_still_running", "checking", "preparing":
		return scm.RefusedPipeline
	default:
		return scm.RefusedConflict
	}
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
			// The provider's authenticated author, overriding anything the
			// body may claim.
			a.PostedBy, a.PostedByID = n.Author.Username, userID(n.Author.ID)
			all = append(all, a)
		}
		page = nextPage(resp.Header)
	}
	return scm.SelectAudits(all, sha), nil
}

// renameNotPure returns "" when the rename is proved path-only, otherwise
// why it could not be.
func (d *Driver) renameNotPure(ctx context.Context, m glMR, oldPath, newPath string) string {
	if m.DiffRefs == nil || m.DiffRefs.BaseSHA == "" || m.DiffRefs.HeadSHA == "" {
		return "renamed with an empty diff and no diff refs to prove it path-only"
	}
	oldBlob, err := d.blobID(ctx, oldPath, m.DiffRefs.BaseSHA)
	if err != nil {
		return fmt.Sprintf("renamed with an empty diff; the old blob could not be read: %v", err)
	}
	newBlob, err := d.blobID(ctx, newPath, m.DiffRefs.HeadSHA)
	if err != nil {
		return fmt.Sprintf("renamed with an empty diff; the new blob could not be read: %v", err)
	}
	if oldBlob == "" || newBlob == "" || oldBlob != newBlob {
		return "renamed with content changes but no diff delivered"
	}
	return ""
}

func (d *Driver) blobID(ctx context.Context, path, ref string) (string, error) {
	var f struct {
		BlobID string `json:"blob_id"`
	}
	p := d.projPath("/repository/files/" + url.PathEscape(path) + "?ref=" + url.QueryEscape(ref))
	if _, err := d.c.Do(ctx, http.MethodGet, p, nil, &f); err != nil {
		return "", err
	}
	return f.BlobID, nil
}

func userID(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}
