package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aicix-labs/factoryd/internal/scm"
	"github.com/aicix-labs/factoryd/internal/scm/conformance"
	"github.com/aicix-labs/factoryd/internal/scm/gitlab"
	"github.com/aicix-labs/factoryd/internal/scm/httpfixture"
	"github.com/aicix-labs/factoryd/internal/scm/httpjson"
)

// The placeholder repository the conformance suite asserts on.
const (
	placeholderProject   = "acme/widgets"
	placeholderHost      = "gitlab.example.com"
	placeholderChangeIID = 42
)

type gitlabTarget struct {
	api       *httpjson.Client
	base      string
	host      string
	project   string // live path, e.g. grp/scratch
	escaped   string
	token     string
	ver       string
	username  string
	numericID int
	// baseSHA is the verified commit every scenario branches from.
	baseSHA string
}

func newGitLabTarget(ctx context.Context, base, project, token string) (*gitlabTarget, error) {
	if base == "" {
		base = "https://gitlab.com/api/v4"
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("PRIVATE-TOKEN", token)
	h.Set("User-Agent", "factoryd-fixturerec")

	t := &gitlabTarget{
		api:     &httpjson.Client{Base: base, Header: h},
		base:    base,
		host:    u.Host,
		project: project,
		escaped: url.PathEscape(project),
		token:   token,
	}

	var v struct {
		Version string `json:"version"`
	}
	if _, err := t.api.Do(ctx, http.MethodGet, "/version", nil, &v); err != nil {
		return nil, fmt.Errorf("gitlab: %w", err)
	}
	t.ver = v.Version

	var me struct {
		Username string `json:"username"`
	}
	if _, err := t.api.Do(ctx, http.MethodGet, "/user", nil, &me); err != nil {
		return nil, err
	}
	t.username = me.Username

	var p struct {
		ID int `json:"id"`
	}
	if _, err := t.api.Do(ctx, http.MethodGet, "/projects/"+t.escaped, nil, &p); err != nil {
		return nil, fmt.Errorf("gitlab: scratch project %s: %w", project, err)
	}
	t.numericID = p.ID
	return t, nil
}

func (t *gitlabTarget) describe() string     { return t.project + " on " + t.host }
func (t *gitlabTarget) version() string      { return t.ver }
func (t *gitlabTarget) providerName() string { return "gitlab" }

func (t *gitlabTarget) driver(hc *http.Client) (scm.Driver, error) {
	return gitlab.New(gitlab.Config{
		BaseURL: t.base, Project: t.project, Token: t.token, HTTPClient: hc,
	})
}

func (t *gitlabTarget) p(suffix string) string { return "/projects/" + t.escaped + suffix }

// ---------- live setup ----------

// cleanup closes every open merge request and deletes every branch but the
// default, so each scenario starts from the same place. Recording against
// leftover state from the previous scenario produces a fixture that only
// reproduces by accident.
func (t *gitlabTarget) cleanup(ctx context.Context) error {
	var mrs []struct {
		IID int `json:"iid"`
	}
	if _, err := t.api.Do(ctx, http.MethodGet, t.p("/merge_requests?state=opened&per_page=100"), nil, &mrs); err != nil {
		return err
	}
	for _, mr := range mrs {
		_, _ = t.api.Do(ctx, http.MethodPut,
			t.p(fmt.Sprintf("/merge_requests/%d", mr.IID)), map[string]string{"state_event": "close"}, nil)
	}

	type branch struct {
		Name    string `json:"name"`
		Default bool   `json:"default"`
	}
	var branches []branch
	if _, err := t.api.Do(ctx, http.MethodGet, t.p("/repository/branches?per_page=100"), nil, &branches); err != nil {
		return err
	}

	for _, b := range branches {
		if b.Default {
			continue
		}
		if err := t.removeBranch(ctx, b.Name); err != nil {
			return err
		}
	}
	return nil
}

// removeBranch deletes a branch and confirms it is gone.
//
// Confirmation is per branch, by GET, not by re-listing. The branch listing is
// served from a cache that lags a delete by tens of seconds, so checking the
// list reports branches that no longer exist and misses ones that do -- a check
// that answers a different question than the one being asked. A GET on the
// branch itself is authoritative.
func (t *gitlabTarget) removeBranch(ctx context.Context, name string) error {
	esc := url.PathEscape(name)
	_, err := t.api.Do(ctx, http.MethodDelete, t.p("/repository/branches/"+esc), nil, nil)
	if err != nil && httpjson.StatusOf(err) != http.StatusNotFound {
		return fmt.Errorf("deleting branch %s: %w", name, err)
	}
	for i := 0; i < 10; i++ {
		_, err := t.api.Do(ctx, http.MethodGet, t.p("/repository/branches/"+esc), nil, nil)
		if httpjson.StatusOf(err) == http.StatusNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("branch %s still exists after being deleted", name)
}

// mainHead returns the current head of the default branch.
func (t *gitlabTarget) mainHead(ctx context.Context) (string, error) {
	var b struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if _, err := t.api.Do(ctx, http.MethodGet,
		t.p("/repository/branches/"+url.PathEscape(conformance.TargetBranch)), nil, &b); err != nil {
		return "", err
	}
	return b.Commit.ID, nil
}

// ensureBranch creates name at fromSHA, removing any existing branch first.
//
// Branches are created from an explicit commit rather than by naming a start
// branch. GitLab resolves a start branch through a cache that lags a commit by
// a second or two, so a branch created that way can be rooted at a commit older
// than the one just pushed -- which shows up later as a conflict that has
// nothing to do with the scenario. Recording that would describe a provider
// behaviour that is really a race in the setup.
func (t *gitlabTarget) ensureBranch(ctx context.Context, name, fromSHA string) error {
	if err := t.removeBranch(ctx, name); err != nil {
		return err
	}
	_, err := t.api.Do(ctx, http.MethodPost,
		t.p("/repository/branches?branch="+url.QueryEscape(name)+"&ref="+url.QueryEscape(fromSHA)), nil, nil)
	if err != nil {
		return fmt.Errorf("creating %s at %s: %w", name, fromSHA, err)
	}
	return nil
}

type glCommitAction struct {
	Action       string `json:"action"`
	FilePath     string `json:"file_path"`
	PreviousPath string `json:"previous_path,omitempty"`
	Content      string `json:"content,omitempty"`
}

// commit creates a commit on branch, starting it from startBranch if it does
// not exist.
func (t *gitlabTarget) commit(ctx context.Context, branch, startBranch, message string, actions []glCommitAction) (string, error) {
	body := map[string]any{
		"branch":         branch,
		"commit_message": message,
		"actions":        actions,
	}
	if startBranch != "" {
		body["start_branch"] = startBranch
	}
	var out struct {
		ID string `json:"id"`
	}
	if _, err := t.api.Do(ctx, http.MethodPost, t.p("/repository/commits"), body, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func (t *gitlabTarget) openMR(ctx context.Context, source, title string) (int, string, error) {
	var out struct {
		IID int    `json:"iid"`
		SHA string `json:"sha"`
	}
	_, err := t.api.Do(ctx, http.MethodPost, t.p("/merge_requests"), map[string]any{
		"source_branch": source,
		"target_branch": conformance.TargetBranch,
		"title":         title,
		"labels":        "factory",
	}, &out)
	if err != nil {
		return 0, "", err
	}
	status, sha, err := t.settleMergeStatus(ctx, out.IID)
	if err != nil {
		return 0, "", err
	}
	_ = status
	if sha == "" {
		sha = out.SHA
	}
	return out.IID, sha, nil
}

// settleMergeStatus waits until GitLab has decided whether a merge request can
// merge, and returns that decision.
//
// GitLab computes mergeability lazily and leaves detailed_merge_status at
// "unchecked" until something asks for it -- polling the merge request alone
// does not always trigger it. Requesting the merge ref does. Without this a
// merge attempt is refused with a bare 405 that says nothing about the change,
// and a fixture recorded from it would teach the driver the wrong lesson: it
// looks exactly like a real refusal.
func (t *gitlabTarget) settleMergeStatus(ctx context.Context, iid int) (string, string, error) {
	// Forces the check. It fails for a conflicting merge request, which is
	// itself the answer, so the error is deliberately ignored.
	_, _ = t.api.Do(ctx, http.MethodGet,
		t.p(fmt.Sprintf("/merge_requests/%d/merge_ref", iid)), nil, nil)

	for i := 0; i < 40; i++ {
		var mr struct {
			SHA    string `json:"sha"`
			Status string `json:"detailed_merge_status"`
		}
		if _, err := t.api.Do(ctx, http.MethodGet,
			t.p(fmt.Sprintf("/merge_requests/%d", iid)), nil, &mr); err != nil {
			return "", "", err
		}
		switch mr.Status {
		case "", "unchecked", "checking", "preparing":
			time.Sleep(2 * time.Second)
			continue
		case "commits_status":
			// Transient here, despite reading like a verdict. GitLab generates
			// the merge request's diff asynchronously, and until it exists the
			// "source branch has commits" check fails. Treating it as final
			// records a refusal that describes the setup, not the provider.
			time.Sleep(2 * time.Second)
			continue
		}
		return mr.Status, mr.SHA, nil
	}
	return "", "", fmt.Errorf("gitlab: merge request %d never settled its merge status", iid)
}

func (t *gitlabTarget) note(ctx context.Context, iid int, body string) error {
	_, err := t.api.Do(ctx, http.MethodPost,
		t.p(fmt.Sprintf("/merge_requests/%d/notes", iid)), map[string]string{"body": body}, nil)
	return err
}

// gateFile is the file every scenario's change edits, named to match the diff
// the conformance suite asserts on.
const gateFile = "internal/gate/scope.go"

// fileContent returns a file's contents on a ref, and whether it exists.
func (t *gitlabTarget) fileContent(ctx context.Context, ref, path string) (string, bool, error) {
	var out struct {
		Content string `json:"content"`
	}
	_, err := t.api.Do(ctx, http.MethodGet,
		t.p("/repository/files/"+url.PathEscape(path)+"?ref="+url.QueryEscape(ref)), nil, &out)
	if err != nil {
		if httpjson.StatusOf(err) == http.StatusNotFound {
			return "", false, nil
		}
		return "", false, err
	}
	raw, derr := base64.StdEncoding.DecodeString(out.Content)
	if derr != nil {
		return "", true, derr
	}
	return string(raw), true, nil
}

// seedTarget puts the default branch into the exact state every scenario
// assumes, whatever the previous run left behind.
//
// This has to be idempotent and complete, not "create if missing". A scenario
// that merges lands a rename on the default branch, so the next run's diff is
// no longer a rename and the fixture silently stops describing what the suite
// asserts. Recording against leftover state produces a fixture that reproduces
// by accident.
func (t *gitlabTarget) seedTarget(ctx context.Context) (string, error) {
	want := map[string]string{
		gateFile:      "package gate\n\ncontext\nold\n",
		"old/name.go": "package old\n",
	}
	absent := []string{"new/name.go", "second.txt", "later.txt"}

	var actions []glCommitAction
	for path, content := range want {
		have, exists, err := t.fileContent(ctx, conformance.TargetBranch, path)
		if err != nil {
			return "", err
		}
		switch {
		case !exists:
			actions = append(actions, glCommitAction{Action: "create", FilePath: path, Content: content})
		case have != content:
			actions = append(actions, glCommitAction{Action: "update", FilePath: path, Content: content})
		}
	}
	for _, path := range absent {
		if _, exists, err := t.fileContent(ctx, conformance.TargetBranch, path); err != nil {
			return "", err
		} else if exists {
			actions = append(actions, glCommitAction{Action: "delete", FilePath: path})
		}
	}

	if len(actions) > 0 {
		sort.Slice(actions, func(i, j int) bool { return actions[i].FilePath < actions[j].FilePath })
		// The commit's own id is the base to branch from. Re-reading the branch
		// would go through a cache that lags the write, and a branch rooted at
		// the older commit conflicts with the target for reasons that have
		// nothing to do with the scenario.
		sha, err := t.commit(ctx, conformance.TargetBranch, "", "seed: reset to the base state", actions)
		if err != nil {
			return "", err
		}
		return sha, t.verifyBase(ctx, sha)
	}

	sha, err := t.mainHead(ctx)
	if err != nil {
		return "", err
	}
	return sha, t.verifyBase(ctx, sha)
}

// verifyBase confirms the commit really carries the state every scenario
// assumes.
//
// The check reads at the commit sha, not at a branch name: a sha is immutable,
// so this cannot be answered from a stale view the way a branch read can. It is
// the positive control on the seed -- without it, "the seed made no changes"
// and "the seed read a stale branch and did nothing" are indistinguishable.
func (t *gitlabTarget) verifyBase(ctx context.Context, sha string) error {
	for i := 0; i < 10; i++ {
		gate, ok, err := t.fileContent(ctx, sha, gateFile)
		if err != nil {
			return err
		}
		_, oldExists, err := t.fileContent(ctx, sha, "old/name.go")
		if err != nil {
			return err
		}
		_, newExists, err := t.fileContent(ctx, sha, "new/name.go")
		if err != nil {
			return err
		}
		if ok && gate == "package gate\n\ncontext\nold\n" && oldExists && !newExists {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("gitlab: commit %s does not carry the base state the scenarios assume", sha)
}

// changeContent is the edit the conformance suite counts: three lines added,
// one removed.
const changeContent = "package gate\n\ncontext\nnew1\nnew2\nnew3\n"

func (t *gitlabTarget) prepare(ctx context.Context, sc scenario) (*world, error) {
	base, err := t.seedTarget(ctx)
	if err != nil {
		return nil, fmt.Errorf("seeding the target branch: %w", err)
	}
	t.baseSHA = base

	w := &world{Secrets: []string{t.token, t.host, t.project, t.username}}
	const branch = conformance.SourceBranch

	switch sc.setup {
	case "none":
		w.Redactor = t.redactor(0, "", "", true)
		return w, nil

	case "two_open_one_draft":
		if _, err := t.changeBranch(ctx, branch); err != nil {
			return nil, err
		}
		iid, sha, err := t.openMR(ctx, branch, conformance.ChangeTitle)
		if err != nil {
			return nil, err
		}
		if _, err := t.branchWithEdit(ctx, "producer/second-thing", "second thing",
			[]glCommitAction{{Action: "create", FilePath: "second.txt", Content: "2\n"}}); err != nil {
			return nil, err
		}
		iid2, _, err := t.openMR(ctx, "producer/second-thing", "Draft: second thing")
		if err != nil {
			return nil, err
		}
		w.ChangeID = scm.ChangeID(fmt.Sprint(iid))
		w.HeadSHA = sha
		w.Redactor = t.redactor(iid, sha, "", false)
		w.Redactor.MapPattern(regexp.MustCompile(fmt.Sprintf(`"iid":%d\b`, iid2)), `"iid":43`)
		return w, nil

	case "open_change", "change_with_audits", "mergeable_change":
		sha, err := t.changeBranch(ctx, branch)
		if err != nil {
			return nil, err
		}
		iid, head, err := t.openMR(ctx, branch, conformance.ChangeTitle)
		if err != nil {
			return nil, err
		}
		if sha != "" {
			head = sha
		}
		if sc.setup == "change_with_audits" {
			if err := t.seedAudits(ctx, iid, head); err != nil {
				return nil, err
			}
		}
		if sc.setup == "mergeable_change" {
			// Assert the precondition instead of discovering it as a 405 from
			// the verb under test. A merge refused because the setup was not
			// ready would be recorded as a provider behaviour it is not.
			status, settled, err := t.settleMergeStatus(ctx, iid)
			if err != nil {
				return nil, err
			}
			if status != "mergeable" {
				return nil, fmt.Errorf("merge request %d is %q, not mergeable; the setup is not ready to record a successful merge", iid, status)
			}
			if settled != "" {
				head = settled
			}
		}
		w.ChangeID = scm.ChangeID(fmt.Sprint(iid))
		w.HeadSHA = head
		w.Redactor = t.redactor(iid, head, "", false)
		return w, nil

	case "branch_only":
		head, err := t.changeBranch(ctx, branch)
		if err != nil {
			return nil, err
		}
		w.HeadSHA = head
		w.Redactor = t.redactor(0, head, "", false)
		return w, nil

	case "draft_change":
		sha, err := t.changeBranch(ctx, branch)
		if err != nil {
			return nil, err
		}
		iid, head, err := t.openMR(ctx, branch, "Draft: "+conformance.ChangeTitle)
		if err != nil {
			return nil, err
		}
		if sha != "" {
			head = sha
		}
		w.ChangeID = scm.ChangeID(fmt.Sprint(iid))
		w.HeadSHA = head
		w.Redactor = t.redactor(iid, head, "", false)
		return w, nil

	case "open_change_head_moved":
		if _, err := t.changeBranch(ctx, branch); err != nil {
			return nil, err
		}
		iid, _, err := t.openMR(ctx, branch, conformance.ChangeTitle)
		if err != nil {
			return nil, err
		}
		// Move the head after the change is open, so the driver's pin is stale.
		moved, err := t.commit(ctx, branch, "", "a later commit", []glCommitAction{
			{Action: "create", FilePath: "later.txt", Content: "later\n"},
		})
		if err != nil {
			return nil, err
		}
		w.ChangeID = scm.ChangeID(fmt.Sprint(iid))
		w.HeadSHA = moved
		// The moved head is what the API will report; the suite calls it the
		// stale sha because it is not what the caller pinned.
		w.Redactor = t.redactor(iid, "", moved, false)
		return w, nil

	case "conflicting_change":
		// Two branches touching the same line. Merge one, and the other can no
		// longer merge -- which is how GitLab is made to say
		// "Branch cannot be merged" for real.
		if _, err := t.branchWithEdit(ctx, "producer/first", "first edit",
			[]glCommitAction{{Action: "update", FilePath: gateFile, Content: "package gate\n\ncontext\nFIRST\n"}}); err != nil {
			return nil, err
		}
		head, err := t.branchWithEdit(ctx, branch, "conflicting edit",
			[]glCommitAction{{Action: "update", FilePath: gateFile, Content: "package gate\n\ncontext\nSECOND\n"}})
		if err != nil {
			return nil, err
		}
		firstIID, _, err := t.openMR(ctx, "producer/first", "first")
		if err != nil {
			return nil, err
		}
		iid, mrHead, err := t.openMR(ctx, branch, conformance.ChangeTitle)
		if err != nil {
			return nil, err
		}
		if mrHead != "" {
			head = mrHead
		}
		status, _, err := t.settleMergeStatus(ctx, firstIID)
		if err != nil {
			return nil, err
		}
		if _, err := t.api.Do(ctx, http.MethodPut,
			t.p(fmt.Sprintf("/merge_requests/%d/merge", firstIID)), map[string]any{}, nil); err != nil {
			return nil, fmt.Errorf("landing MR %d (source %s, merge status %q, changes %s) to create the conflict: %w",
				firstIID, "producer/first", status, t.changesCount(ctx, firstIID), err)
		}
		// Let GitLab recompute mergeability against the moved target.
		if err := t.waitUnmergeable(ctx, iid); err != nil {
			return nil, err
		}
		w.ChangeID = scm.ChangeID(fmt.Sprint(iid))
		w.HeadSHA = head
		w.Redactor = t.redactor(iid, head, "", false)
		return w, nil

	default:
		return nil, fmt.Errorf("gitlab: no setup for %q", sc.setup)
	}
}

// branchWithEdit creates branch at the current head of the default branch and
// puts one commit on it.
func (t *gitlabTarget) branchWithEdit(ctx context.Context, branch, message string, actions []glCommitAction) (string, error) {
	if t.baseSHA == "" {
		return "", fmt.Errorf("gitlab: no verified base commit; seedTarget has not run")
	}
	if err := t.ensureBranch(ctx, branch, t.baseSHA); err != nil {
		return "", err
	}
	return t.commit(ctx, branch, "", message, actions)
}

func (t *gitlabTarget) changeBranch(ctx context.Context, branch string) (string, error) {
	return t.branchWithEdit(ctx, branch, conformance.ChangeTitle, []glCommitAction{
		{Action: "update", FilePath: gateFile, Content: changeContent},
		{Action: "move", FilePath: "new/name.go", PreviousPath: "old/name.go", Content: "package old\n"},
	})
}

func (t *gitlabTarget) waitUnmergeable(ctx context.Context, iid int) error {
	for i := 0; i < 20; i++ {
		status, _, err := t.settleMergeStatus(ctx, iid)
		if err != nil {
			return err
		}
		if strings.Contains(status, "conflict") || status == "broken_status" || status == "cannot_be_merged" {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("gitlab: merge request %d never became unmergeable; the conflict setup did not take", iid)
}

// changesCount reports how many files a merge request touches, for diagnostics.
func (t *gitlabTarget) changesCount(ctx context.Context, iid int) string {
	var mr struct {
		ChangesCount string `json:"changes_count"`
	}
	if _, err := t.api.Do(ctx, http.MethodGet, t.p(fmt.Sprintf("/merge_requests/%d", iid)), nil, &mr); err != nil {
		return "unknown"
	}
	return mr.ChangesCount
}

func (t *gitlabTarget) seedAudits(ctx context.Context, iid int, head string) error {
	if err := t.note(ctx, iid, "looks good to me"); err != nil {
		return err
	}
	stale, err := scm.EncodeAudit(scm.Audit{
		Lens: "stale-lens", Verdict: scm.AuditCleared, SHA: conformance.StaleSHA,
		Attempts: []string{"tried the previous head"}, Notes: "no escape found",
	})
	if err != nil {
		return err
	}
	if err := t.note(ctx, iid, stale); err != nil {
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
	return t.note(ctx, iid, current)
}

// redactor maps this run's live values onto the placeholders the conformance
// suite asserts on.
func (t *gitlabTarget) redactor(iid int, head, stale string, asReviewer bool) *httpfixture.Redactor {
	r := httpfixture.NewRedactor()

	// Identifiers, anchored to their syntax. A bare numeric replacement would
	// rewrite unrelated numbers.
	if iid > 0 {
		r.MapPattern(regexp.MustCompile(fmt.Sprintf(`"iid":%d\b`, iid)), fmt.Sprintf(`"iid":%d`, placeholderChangeIID))
		r.MapPattern(regexp.MustCompile(fmt.Sprintf(`/merge_requests/%d\b`, iid)), fmt.Sprintf("/merge_requests/%d", placeholderChangeIID))
	}
	r.MapPattern(regexp.MustCompile(fmt.Sprintf(`"project_id":%d\b`, t.numericID)), `"project_id":1`)
	r.MapPattern(regexp.MustCompile(`"(id|author_id|user_id|target_project_id|source_project_id)":\d+`), `"$1":1001`)

	// Object ids.
	if head != "" {
		r.Map(head, conformance.HeadSHA)
	}
	if stale != "" {
		r.Map(stale, conformance.StaleSHA)
	}

	// Instance identity.
	r.Map(t.escaped, url.PathEscape(placeholderProject))
	r.Map(t.project, placeholderProject)
	r.Map(t.host, placeholderHost)
	if asReviewer {
		r.Map(t.username, conformance.ReviewerName)
	} else {
		r.Map(t.username, conformance.ProducerName)
	}
	return r
}
