package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"yuno/internal/project"
	"yuno/internal/store"
)

// ProviderClient is the HTTP client every provider call uses: a 3xx answer is returned
// as it is rather than followed, so a token header never travels to another host (Go
// drops Authorization on a cross-host redirect but not GitLab's PRIVATE-TOKEN). A nil
// transport is the default one.
func ProviderClient(transport http.RoundTripper) *http.Client {
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// apiClient sends JSON requests to a provider's REST API. A non-2xx answer is an
// *errAPIStatus with the status returned alongside, so callers can treat 404 as
// "absent"; transport failures are classified for the retry policy (httperr.go).
type apiClient struct {
	what      string
	client    *http.Client
	authorize func(*http.Request)
}

// errAPIStatus carries a failed answer as a result. status is the HTTP status (0 for a
// transport failure) and message the provider's own text, the first line of its message.
type errAPIStatus struct {
	res     Result
	status  int
	message string
	// plain is the text without the untrusted wrapper, for logs and stored errors that
	// a person reads; res.Text, which the model reads, carries the wrapper.
	plain string
}

func (e *errAPIStatus) Error() string {
	if e.plain != "" {
		return e.plain
	}
	return e.res.Text
}

// providerMessage is the first line of the message a provider's error body carries: the
// "message" (or "error") of a JSON object, else the body itself.
func providerMessage(body []byte) string {
	text := strings.TrimSpace(string(body))
	var obj struct {
		Message any `json:"message"`
		Error   any `json:"error"`
	}
	if json.Unmarshal(body, &obj) == nil {
		for _, v := range []any{obj.Message, obj.Error} {
			if m, ok := v.(string); ok && strings.TrimSpace(m) != "" {
				text = strings.TrimSpace(m)
				break
			}
		}
	}
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = strings.TrimSpace(text[:i])
	}
	return cutText(text, 300)
}

func privateTokenAuth(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("PRIVATE-TOKEN", token) }
}

func bearerAuth(token string) func(*http.Request) {
	return func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Accept", "application/vnd.github+json")
		r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
}

func (a *apiClient) do(ctx context.Context, method, u string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, err
	}
	if a.authorize != nil {
		a.authorize(req)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, &errAPIStatus{res: transportFailure(a.what, err)}
	}
	defer resp.Body.Close()
	raw, err := readBody(resp, 4<<20)
	if err != nil {
		return resp.StatusCode, &errAPIStatus{res: transportFailure(a.what, err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, &errAPIStatus{res: statusFailure(a.what, resp, raw), plain: statusPlain(a.what, resp, raw), status: resp.StatusCode, message: providerMessage(raw)}
	}
	if out != nil {
		if r := decodeJSONBody(a.what, raw, out); r != nil {
			return resp.StatusCode, &errAPIStatus{res: *r}
		}
	}
	return resp.StatusCode, nil
}

// RequestRef is an open merge or pull request as the provider reports it.
type RequestRef struct {
	Number int64
	URL    string
}

// RequestStatus is what the provider says about a merge or pull request now (DESIGN 5.20
// rule 3). State is one of the store's merge request states and CIStatus one of its
// pipeline statuses; HeadSHA is empty when the provider gave none; UnresolvedThreads is
// nil when the provider could not tell. Err is a problem that left part of the status
// unknown without failing the rest (a GitHub token without GraphQL access): the caller
// records it and still takes the other members.
type RequestStatus struct {
	State             string
	CIStatus          string
	HeadSHA           string
	UnresolvedThreads *int
	ClosedAt          *time.Time
	// MergeSHA is the commit a merged request made, "" when the provider gave none.
	MergeSHA string
	Err      string
}

// ErrRequestNotFound is a provider's 404 for the request itself (DESIGN 5.20 rule 3): the
// text is what the record's last_sync_error says.
var ErrRequestNotFound = errors.New("not found on the provider")

// ErrNotMergeable is a provider's refusal to merge (DESIGN 5.20 rule 5): 405, 406, 409
// or 422, with the first line of its message.
type ErrNotMergeable struct{ Message string }

func (e *ErrNotMergeable) Error() string { return e.Message }

// TransientError reports whether err is a provider failure the retry policy calls
// transient (DESIGN 5.5 rule 4): a 5xx answer, a timeout or a refused connection.
func TransientError(err error) bool {
	var se *errAPIStatus
	if errors.As(err, &se) {
		return se.res.Transient
	}
	return isTransientNetErr(err)
}

// RequestProvider is a provider's merge request API for one project (DESIGN 5.16 rule
// 11, 5.20 rules 3 and 5). A non-2xx answer is an error whose text names the provider
// and the status; the provider's answer is never a Result for the model here.
type RequestProvider interface {
	BranchExists(ctx context.Context, branch string) (bool, error)
	FindOpen(ctx context.Context, branch string) (*RequestRef, error)
	// Create opens the request; draft opens it as a draft (DESIGN 5.29 rule 4).
	Create(ctx context.Context, branch, base, title, description string, draft bool) (*RequestRef, error)
	// Update replaces the request's title and description and moves it between draft and
	// ready (DESIGN 5.29 rule 4); a nil member is left as it is. ErrRequestNotFound on a 404.
	Update(ctx context.Context, number int64, u RequestUpdate) error
	// Status reads the request now: ErrRequestNotFound on a 404.
	Status(ctx context.Context, number int64) (RequestStatus, error)
	// Merge merges the request: *ErrNotMergeable when the provider refuses,
	// ErrRequestNotFound on a 404.
	Merge(ctx context.Context, number int64) error
	// Details reads what the request says (DESIGN 5.22 rule 6): ErrRequestNotFound on
	// a 404 for the request itself.
	Details(ctx context.Context, number int64) (RequestDetails, error)
	// Threads reads the request's unresolved review threads, oldest first (the first
	// 100 the provider lists); *ErrThreadsUnavailable when the provider refuses to list
	// them (a GitHub token without GraphQL access).
	Threads(ctx context.Context, number int64) ([]Thread, error)
	// CIStatus is the pipeline status of the request now, one of the store's ci_status
	// values, by the mapping tables of Status. headSHA is the request's head commit
	// (Details gives it); GitLab reads the request's own pipelines and ignores it.
	CIStatus(ctx context.Context, number int64, headSHA string) (string, error)
	// DefaultBranchStatus is the pipeline state of the project's default branch now (DESIGN
	// 5.29 rule 12): GitLab's newest pipeline of that ref, GitHub's check runs and combined
	// status of its head commit.
	DefaultBranchStatus(ctx context.Context) (BranchStatus, error)
}

// The states of a BranchStatus (DESIGN 5.29 rule 12).
const (
	BranchPassed  = "passed"
	BranchFailed  = "failed"
	BranchRunning = "running"
	BranchPending = "pending"
	BranchNone    = "none"
)

// BranchStatus is the pipeline state of a branch head: State is one of passed, failed,
// running, pending or none (no pipeline or check at all); SHA, At and URL describe the
// pipeline or check behind it and are empty for none.
type BranchStatus struct {
	State string
	SHA   string
	At    time.Time
	URL   string
}

// RequestUpdate is a change to an open request (DESIGN 5.29 rule 4): nil members stay.
// Title is the plain title; the draft marker is the provider's business.
type RequestUpdate struct {
	Title       *string
	Description *string
	Draft       *bool
}

// draftPrefix is how GitLab marks a draft merge request in its title.
const draftPrefix = "Draft: "

// cutDraft returns the title without its draft marker and whether it had one.
func cutDraft(title string) (string, bool) {
	t := strings.TrimSpace(title)
	l := strings.ToLower(t)
	for _, m := range []string{"draft:", "[draft]", "(draft)"} {
		if strings.HasPrefix(l, m) {
			return strings.TrimSpace(t[len(m):]), true
		}
	}
	return t, false
}

// RequestDetails is what a merge or pull request says about itself (DESIGN 5.22 rule 6).
// State is one of the store's merge request states; HeadSHA is empty when the provider
// gave none.
type RequestDetails struct {
	Title        string
	Description  string
	State        string
	SourceBranch string
	TargetBranch string
	URL          string
	HeadSHA      string
}

// Thread is an unresolved review thread: its first comment, where it points (Path empty
// and Line nil for a thread that is not on a line of the diff) and when it was written
// (nil when the provider gave no time).
type Thread struct {
	ID        string
	Path      string
	Line      *int
	Author    string
	Body      string
	CreatedAt *time.Time
}

// ErrThreadsUnavailable is the provider's refusal to list the review threads (DESIGN
// 5.22 rule 6: a GitHub token without GraphQL access): the request is still readable,
// its threads are unknown. Message is the provider's first line.
type ErrThreadsUnavailable struct{ Message string }

func (e *ErrThreadsUnavailable) Error() string { return e.Message }

// ProviderFor builds the provider client of a project (DESIGN 5.16 rule 11) over client,
// which should come from ProviderClient so that no redirect is followed. The token is
// pc.Token.
func ProviderFor(pc *project.Config, client *http.Client) RequestProvider {
	return newRequestProvider(pc, client, pc.APIBase(), pc.Path())
}

// newRequestProvider is ProviderFor with the API base and the repository path given, so
// the tool's tests can aim the API at a fake while git talks to a local repository.
func newRequestProvider(pc *project.Config, client *http.Client, base, path string) RequestProvider {
	if client == nil {
		client = ProviderClient(nil)
	}
	switch pc.Provider {
	case project.ProviderGitHub:
		return &githubProvider{api: &apiClient{what: "github", client: client, authorize: bearerAuth(pc.Token)}, base: base,
			owner: ownerOf(path), repo: repoOf(path)}
	default:
		return &gitlabProvider{api: &apiClient{what: "gitlab", client: client, authorize: privateTokenAuth(pc.Token)}, base: base}
	}
}

// notFoundOr turns a 404 into ErrRequestNotFound and leaves any other error as it is.
func notFoundOr(status int, err error) error {
	if status == http.StatusNotFound {
		return ErrRequestNotFound
	}
	return err
}

// mergeError maps the provider's answer to a merge (DESIGN 5.20 rule 5): 405, 406, 409
// and 422 are refusals with the provider's first line, 404 is ErrRequestNotFound, the
// rest is the error as it came.
func mergeError(err error) error {
	var se *errAPIStatus
	if !errors.As(err, &se) {
		return err
	}
	switch se.status {
	case http.StatusNotFound:
		return ErrRequestNotFound
	case http.StatusMethodNotAllowed, http.StatusNotAcceptable, http.StatusConflict, http.StatusUnprocessableEntity:
		msg := se.message
		if msg == "" {
			msg = fmt.Sprintf("the provider refused the merge (HTTP %d)", se.status)
		}
		return &ErrNotMergeable{Message: msg}
	case http.StatusUnauthorized, http.StatusForbidden:
		// DESIGN 5.20 rule 5: the token may read the request (the sync works) but may
		// not merge into the target branch (GitLab answers 401 for a Developer-role
		// token on a protected branch, GitHub 403); the operator changes the role.
		return &ErrNotMergeable{Message: fmt.Sprintf("the project's token is not allowed to merge this request (HTTP %d); give it a role that may merge into the target branch", se.status)}
	}
	return err
}

// parseProviderTime reads a provider's timestamp; nil when absent or unreadable.
func parseProviderTime(s *string) *time.Time {
	if s == nil || *s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

// firstTime is the first time that is not nil.
func firstTime(ts ...*time.Time) *time.Time {
	for _, t := range ts {
		if t != nil {
			return t
		}
	}
	return nil
}

// gitlabProvider is GitLab's merge request API for one project (DESIGN 5.16 rule 11):
// base is .../api/v4/projects/<encoded path>.
type gitlabProvider struct {
	api  *apiClient
	base string
}

func (g *gitlabProvider) BranchExists(ctx context.Context, branch string) (bool, error) {
	status, err := g.api.do(ctx, http.MethodGet, g.base+"/repository/branches/"+url.PathEscape(branch), nil, nil)
	if err != nil && status == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

func (g *gitlabProvider) FindOpen(ctx context.Context, branch string) (*RequestRef, error) {
	var open []struct {
		IID    int64  `json:"iid"`
		WebURL string `json:"web_url"`
	}
	q := url.Values{"source_branch": {branch}, "state": {"opened"}}
	if _, err := g.api.do(ctx, http.MethodGet, g.base+"/merge_requests?"+q.Encode(), nil, &open); err != nil {
		return nil, err
	}
	if len(open) == 0 {
		return nil, nil
	}
	return &RequestRef{Number: open[0].IID, URL: open[0].WebURL}, nil
}

func (g *gitlabProvider) Create(ctx context.Context, branch, base, title, description string, draft bool) (*RequestRef, error) {
	var mr struct {
		IID    int64  `json:"iid"`
		WebURL string `json:"web_url"`
	}
	if plain, _ := cutDraft(title); draft {
		title = draftPrefix + plain
	}
	body := map[string]any{"source_branch": branch, "target_branch": base, "title": title, "description": description,
		"remove_source_branch": true}
	if _, err := g.api.do(ctx, http.MethodPost, g.base+"/merge_requests", body, &mr); err != nil {
		return nil, err
	}
	return &RequestRef{Number: mr.IID, URL: mr.WebURL}, nil
}

// Update is DESIGN 5.29 rule 4 for GitLab: one PUT with the title (the plain title behind
// "Draft: " while the request is a draft, without it once ready) and the description. The
// current title is read first only when the new state needs it.
func (g *gitlabProvider) Update(ctx context.Context, number int64, u RequestUpdate) error {
	if u.Title == nil && u.Description == nil && u.Draft == nil {
		return nil
	}
	body := map[string]any{}
	if u.Description != nil {
		body["description"] = *u.Description
	}
	if u.Title != nil || u.Draft != nil {
		plain, draft := "", false
		if u.Title == nil || u.Draft == nil {
			var cur struct {
				Title string `json:"title"`
			}
			if status, err := g.api.do(ctx, http.MethodGet, fmt.Sprintf("%s/merge_requests/%d", g.base, number), nil, &cur); err != nil {
				return notFoundOr(status, err)
			}
			plain, draft = cutDraft(cur.Title)
		}
		if u.Title != nil {
			plain, _ = cutDraft(*u.Title)
		}
		if u.Draft != nil {
			draft = *u.Draft
		}
		if draft {
			body["title"] = draftPrefix + plain
		} else {
			body["title"] = plain
		}
	}
	if status, err := g.api.do(ctx, http.MethodPut, fmt.Sprintf("%s/merge_requests/%d", g.base, number), body, nil); err != nil {
		return notFoundOr(status, err)
	}
	return nil
}

// gitlabStates maps a merge request's state to the record's (DESIGN 5.20 rule 3); locked
// is closed.
var gitlabStates = map[string]string{
	"opened": store.MergeRequestOpen, "merged": store.MergeRequestMerged,
	"closed": store.MergeRequestClosed, "locked": store.MergeRequestClosed,
}

// gitlabPipelines maps a pipeline's status to the record's ci_status. A status this table
// does not know counts as pending: a gate never reads an unknown pipeline as passed.
var gitlabPipelines = map[string]string{
	"success": store.CIStatusPassed,
	"failed":  store.CIStatusFailed, "canceled": store.CIStatusFailed,
	"created": store.CIStatusPending, "waiting_for_resource": store.CIStatusPending, "preparing": store.CIStatusPending,
	"pending": store.CIStatusPending, "running": store.CIStatusPending, "manual": store.CIStatusPending,
	"scheduled": store.CIStatusPending,
	"skipped":   store.CIStatusNone,
}

// Status is DESIGN 5.20 rule 3 for GitLab: the merge request, its discussions' first
// 100 and its pipelines' first entry. Only the merge request's own 404 is
// ErrRequestNotFound; any other failed call fails the status.
func (g *gitlabProvider) Status(ctx context.Context, number int64) (RequestStatus, error) {
	base := fmt.Sprintf("%s/merge_requests/%d", g.base, number)
	var mr struct {
		State    string  `json:"state"`
		SHA      string  `json:"sha"`
		MergedAt *string `json:"merged_at"`
		ClosedAt *string `json:"closed_at"`
		// MergeCommit and SquashCommit name the merge's commit on the target branch.
		MergeCommit  string `json:"merge_commit_sha"`
		SquashCommit string `json:"squash_commit_sha"`
	}
	if status, err := g.api.do(ctx, http.MethodGet, base, nil, &mr); err != nil {
		return RequestStatus{}, notFoundOr(status, err)
	}
	state, ok := gitlabStates[mr.State]
	if !ok {
		return RequestStatus{}, fmt.Errorf("gitlab: unknown merge request state %q", mr.State)
	}
	st := RequestStatus{State: state, HeadSHA: mr.SHA, ClosedAt: firstTime(parseProviderTime(mr.MergedAt), parseProviderTime(mr.ClosedAt))}
	if state == store.MergeRequestMerged {
		st.MergeSHA = mr.MergeCommit
		if st.MergeSHA == "" {
			st.MergeSHA = mr.SquashCommit
		}
	}

	var discussions []struct {
		Notes []struct {
			Resolvable bool `json:"resolvable"`
			Resolved   bool `json:"resolved"`
		} `json:"notes"`
	}
	if _, err := g.api.do(ctx, http.MethodGet, base+"/discussions?per_page=100", nil, &discussions); err != nil {
		return RequestStatus{}, err
	}
	unresolved := 0
	for _, d := range discussions {
		if len(d.Notes) > 0 && d.Notes[0].Resolvable && !d.Notes[0].Resolved {
			unresolved++
		}
	}
	st.UnresolvedThreads = &unresolved

	ci, err := g.pipelineStatus(ctx, base)
	if err != nil {
		return RequestStatus{}, err
	}
	st.CIStatus = ci
	return st, nil
}

// pipelineStatus is the request's ci_status from its newest pipeline (DESIGN 5.20 rule
// 3), shared by Status and CIStatus; base is .../merge_requests/<iid>.
func (g *gitlabProvider) pipelineStatus(ctx context.Context, base string) (string, error) {
	var pipelines []struct {
		Status string `json:"status"`
	}
	if _, err := g.api.do(ctx, http.MethodGet, base+"/pipelines?per_page=1", nil, &pipelines); err != nil {
		return "", err
	}
	if len(pipelines) == 0 {
		return store.CIStatusNone, nil
	}
	if ci, ok := gitlabPipelines[pipelines[0].Status]; ok {
		return ci, nil
	}
	return store.CIStatusPending, nil
}

// Merge is PUT merge_requests/<iid>/merge removing the source branch (rule 5).
func (g *gitlabProvider) Merge(ctx context.Context, number int64) error {
	_, err := g.api.do(ctx, http.MethodPut, fmt.Sprintf("%s/merge_requests/%d/merge", g.base, number),
		map[string]any{"should_remove_source_branch": true}, nil)
	return mergeError(err)
}

// Details is DESIGN 5.22 rule 6 for GitLab: GET merge_requests/<iid>. Only the request's
// own 404 is ErrRequestNotFound.
func (g *gitlabProvider) Details(ctx context.Context, number int64) (RequestDetails, error) {
	var mr struct {
		Title        string `json:"title"`
		Description  string `json:"description"`
		State        string `json:"state"`
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		WebURL       string `json:"web_url"`
		SHA          string `json:"sha"`
	}
	if status, err := g.api.do(ctx, http.MethodGet, fmt.Sprintf("%s/merge_requests/%d", g.base, number), nil, &mr); err != nil {
		return RequestDetails{}, notFoundOr(status, err)
	}
	state, ok := gitlabStates[mr.State]
	if !ok {
		return RequestDetails{}, fmt.Errorf("gitlab: unknown merge request state %q", mr.State)
	}
	return RequestDetails{Title: mr.Title, Description: mr.Description, State: state, SourceBranch: mr.SourceBranch,
		TargetBranch: mr.TargetBranch, URL: mr.WebURL, HeadSHA: mr.SHA}, nil
}

// Threads is DESIGN 5.22 rule 6 for GitLab: the first 100 discussions, those whose first
// note is resolvable and not resolved, oldest first. A note on a line of the diff gives
// the path (position.new_path) and the line (position.new_line, else old_line); any other
// note has neither.
func (g *gitlabProvider) Threads(ctx context.Context, number int64) ([]Thread, error) {
	var discussions []struct {
		ID    string `json:"id"`
		Notes []struct {
			Body       string `json:"body"`
			CreatedAt  string `json:"created_at"`
			Resolvable bool   `json:"resolvable"`
			Resolved   bool   `json:"resolved"`
			Author     struct {
				Username string `json:"username"`
			} `json:"author"`
			Position *struct {
				NewPath string `json:"new_path"`
				OldPath string `json:"old_path"`
				NewLine *int   `json:"new_line"`
				OldLine *int   `json:"old_line"`
			} `json:"position"`
		} `json:"notes"`
	}
	if _, err := g.api.do(ctx, http.MethodGet, fmt.Sprintf("%s/merge_requests/%d/discussions?per_page=100", g.base, number), nil, &discussions); err != nil {
		return nil, err
	}
	out := []Thread{}
	for _, d := range discussions {
		if len(d.Notes) == 0 || !d.Notes[0].Resolvable || d.Notes[0].Resolved {
			continue
		}
		n := d.Notes[0]
		t := Thread{ID: d.ID, Author: n.Author.Username, Body: n.Body, CreatedAt: parseProviderTime(&n.CreatedAt)}
		if n.Position != nil {
			t.Path = n.Position.NewPath
			if t.Path == "" {
				t.Path = n.Position.OldPath
			}
			t.Line = n.Position.NewLine
			if t.Line == nil {
				t.Line = n.Position.OldLine
			}
		}
		out = append(out, t)
	}
	sortThreads(out)
	return out, nil
}

// CIStatus is the request's newest pipeline through the mapping of Status.
func (g *gitlabProvider) CIStatus(ctx context.Context, number int64, _ string) (string, error) {
	return g.pipelineStatus(ctx, fmt.Sprintf("%s/merge_requests/%d", g.base, number))
}

// sortThreads orders threads oldest first (DESIGN 5.22 rule 6). The providers list them
// in the order they were opened, so a list where a time is missing keeps that order.
func sortThreads(ts []Thread) {
	for _, t := range ts {
		if t.CreatedAt == nil {
			return
		}
	}
	sort.SliceStable(ts, func(i, j int) bool { return ts[i].CreatedAt.Before(*ts[j].CreatedAt) })
}

// githubProvider is GitHub's pull request API for one repository (DESIGN 5.16 rule
// 11): base is .../repos/<owner>/<repo>; owner qualifies the head branch.
type githubProvider struct {
	api   *apiClient
	base  string
	owner string
	repo  string
}

// ownerOf is the owner part of "owner/repo".
func ownerOf(path string) string {
	owner, _, _ := strings.Cut(path, "/")
	return owner
}

// repoOf is the repository part of "owner/repo".
func repoOf(path string) string {
	_, repo, _ := strings.Cut(path, "/")
	return repo
}

func (g *githubProvider) BranchExists(ctx context.Context, branch string) (bool, error) {
	status, err := g.api.do(ctx, http.MethodGet, g.base+"/branches/"+url.PathEscape(branch), nil, nil)
	if err != nil && status == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

func (g *githubProvider) FindOpen(ctx context.Context, branch string) (*RequestRef, error) {
	var open []struct {
		Number  int64  `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	q := url.Values{"head": {g.owner + ":" + branch}, "state": {"open"}}
	if _, err := g.api.do(ctx, http.MethodGet, g.base+"/pulls?"+q.Encode(), nil, &open); err != nil {
		return nil, err
	}
	if len(open) == 0 {
		return nil, nil
	}
	return &RequestRef{Number: open[0].Number, URL: open[0].HTMLURL}, nil
}

func (g *githubProvider) Create(ctx context.Context, branch, base, title, description string, draft bool) (*RequestRef, error) {
	var pr struct {
		Number  int64  `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	body := map[string]any{"head": branch, "base": base, "title": title, "body": description}
	if draft {
		body["draft"] = true
	}
	if _, err := g.api.do(ctx, http.MethodPost, g.base+"/pulls", body, &pr); err != nil {
		return nil, err
	}
	return &RequestRef{Number: pr.Number, URL: pr.HTMLURL}, nil
}

const (
	markReadyMutation    = `mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { isDraft } } }`
	convertDraftMutation = `mutation($id: ID!) { convertPullRequestToDraft(input: {pullRequestId: $id}) { pullRequest { isDraft } } }`
)

// Update is DESIGN 5.29 rule 4 for GitHub: PATCH for the title and body, then, when the
// draft state differs from the pull request's, the GraphQL mutation that moves it (it
// needs the pull request's node_id, read first).
func (g *githubProvider) Update(ctx context.Context, number int64, u RequestUpdate) error {
	endpoint := fmt.Sprintf("%s/pulls/%d", g.base, number)
	if u.Title != nil || u.Description != nil {
		body := map[string]any{}
		if u.Title != nil {
			body["title"] = *u.Title
		}
		if u.Description != nil {
			body["body"] = *u.Description
		}
		if status, err := g.api.do(ctx, http.MethodPatch, endpoint, body, nil); err != nil {
			return notFoundOr(status, err)
		}
	}
	if u.Draft == nil {
		return nil
	}
	var pr struct {
		NodeID string `json:"node_id"`
		Draft  bool   `json:"draft"`
	}
	if status, err := g.api.do(ctx, http.MethodGet, endpoint, nil, &pr); err != nil {
		return notFoundOr(status, err)
	}
	if pr.Draft == *u.Draft {
		return nil
	}
	if pr.NodeID == "" {
		return errors.New("github: the pull request has no node_id")
	}
	query := markReadyMutation
	if *u.Draft {
		query = convertDraftMutation
	}
	var out struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	body := map[string]any{"query": query, "variables": map[string]any{"id": pr.NodeID}}
	if _, err := g.api.do(ctx, http.MethodPost, g.graphqlURL(), body, &out); err != nil {
		return err
	}
	if len(out.Errors) > 0 {
		return errors.New("github graphql: " + providerMessage([]byte(out.Errors[0].Message)))
	}
	return nil
}

// graphqlURL is the GraphQL endpoint beside a REST base ".../repos/<owner>/<repo>":
// https://api.github.com/graphql, or https://<host>/api/graphql on another host, whose
// REST root is /api/v3.
func (g *githubProvider) graphqlURL() string {
	root := strings.TrimSuffix(strings.TrimSuffix(g.base, "/"+g.owner+"/"+g.repo), "/repos")
	if ghe, ok := strings.CutSuffix(root, "/api/v3"); ok {
		return ghe + "/api/graphql"
	}
	return root + "/graphql"
}

const reviewThreadsQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) { pullRequest(number: $number) { reviewThreads(first: 100) { nodes { isResolved } } } }
}`

// unresolvedThreads counts the pull request's unresolved review threads (the first 100)
// through GraphQL. A refusal, whether an HTTP error or GraphQL's own errors member (a
// token without GraphQL access), is returned as an error for the caller to record
// beside an unknown count.
func (g *githubProvider) unresolvedThreads(ctx context.Context, number int64) (int, error) {
	var out struct {
		Data struct {
			Repository *struct {
				PullRequest *struct {
					ReviewThreads struct {
						Nodes []struct {
							IsResolved bool `json:"isResolved"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	body := map[string]any{"query": reviewThreadsQuery, "variables": map[string]any{"owner": g.owner, "name": g.repo, "number": number}}
	if _, err := g.api.do(ctx, http.MethodPost, g.graphqlURL(), body, &out); err != nil {
		return 0, err
	}
	if len(out.Errors) > 0 {
		return 0, errors.New("github graphql: " + providerMessage([]byte(out.Errors[0].Message)))
	}
	if out.Data.Repository == nil || out.Data.Repository.PullRequest == nil {
		return 0, errors.New("github graphql: the pull request was not returned")
	}
	n := 0
	for _, t := range out.Data.Repository.PullRequest.ReviewThreads.Nodes {
		if !t.IsResolved {
			n++
		}
	}
	return n, nil
}

// Status is DESIGN 5.20 rule 3 for GitHub: the pull request, its review threads through
// GraphQL (a refusal leaves the count unknown and sets Err), then the head's check runs
// and combined status. Only the pull request's own 404 is ErrRequestNotFound.
func (g *githubProvider) Status(ctx context.Context, number int64) (RequestStatus, error) {
	var pr struct {
		State    string  `json:"state"`
		Merged   bool    `json:"merged"`
		MergedAt *string `json:"merged_at"`
		ClosedAt *string `json:"closed_at"`
		MergeSHA string  `json:"merge_commit_sha"`
		Head     struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if status, err := g.api.do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d", g.base, number), nil, &pr); err != nil {
		return RequestStatus{}, notFoundOr(status, err)
	}
	st := RequestStatus{HeadSHA: pr.Head.SHA, ClosedAt: firstTime(parseProviderTime(pr.MergedAt), parseProviderTime(pr.ClosedAt))}
	switch {
	case pr.Merged:
		st.State = store.MergeRequestMerged
		st.MergeSHA = pr.MergeSHA
	case pr.State == "closed":
		st.State = store.MergeRequestClosed
	case pr.State == "open":
		st.State = store.MergeRequestOpen
	default:
		return RequestStatus{}, fmt.Errorf("github: unknown pull request state %q", pr.State)
	}

	if n, err := g.unresolvedThreads(ctx, number); err != nil {
		st.Err = "review threads unknown: " + err.Error()
	} else {
		st.UnresolvedThreads = &n
	}

	ci, err := g.ciStatus(ctx, pr.Head.SHA)
	if err != nil {
		return RequestStatus{}, err
	}
	st.CIStatus = ci
	return st, nil
}

// ciStatus reads the head's check runs (the first 100) and combined commit status and
// folds them into one pipeline status (DESIGN 5.20 rule 3): any failing check or a
// failed combined status is failed; else any check not completed, or a pending combined
// status that has statuses, is pending; else none when there is nothing, else passed.
func (g *githubProvider) ciStatus(ctx context.Context, sha string) (string, error) {
	if sha == "" {
		return store.CIStatusNone, nil
	}
	var checks struct {
		CheckRuns []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if _, err := g.api.do(ctx, http.MethodGet, g.base+"/commits/"+url.PathEscape(sha)+"/check-runs?per_page=100", nil, &checks); err != nil {
		return "", err
	}
	var combined struct {
		State      string            `json:"state"`
		TotalCount int               `json:"total_count"`
		Statuses   []json.RawMessage `json:"statuses"`
	}
	if _, err := g.api.do(ctx, http.MethodGet, g.base+"/commits/"+url.PathEscape(sha)+"/status", nil, &combined); err != nil {
		return "", err
	}
	statuses := max(combined.TotalCount, len(combined.Statuses))

	failed, pending := combined.State == "failure" || combined.State == "error", combined.State == "pending" && statuses > 0
	for _, c := range checks.CheckRuns {
		switch c.Conclusion {
		case "failure", "timed_out", "cancelled", "action_required":
			failed = true
		}
		if c.Status != "" && c.Status != "completed" {
			pending = true
		}
	}
	switch {
	case failed:
		return store.CIStatusFailed, nil
	case pending:
		return store.CIStatusPending, nil
	case len(checks.CheckRuns) == 0 && statuses == 0:
		return store.CIStatusNone, nil
	}
	return store.CIStatusPassed, nil
}

// Merge is PUT pulls/<n>/merge with the merge method merge (rule 5).
func (g *githubProvider) Merge(ctx context.Context, number int64) error {
	_, err := g.api.do(ctx, http.MethodPut, fmt.Sprintf("%s/pulls/%d/merge", g.base, number), map[string]any{"merge_method": "merge"}, nil)
	return mergeError(err)
}

// Details is DESIGN 5.22 rule 6 for GitHub: GET pulls/<n>. Only the pull request's own 404
// is ErrRequestNotFound.
func (g *githubProvider) Details(ctx context.Context, number int64) (RequestDetails, error) {
	var pr struct {
		Title   string `json:"title"`
		Body    string `json:"body"`
		State   string `json:"state"`
		Merged  bool   `json:"merged"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if status, err := g.api.do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d", g.base, number), nil, &pr); err != nil {
		return RequestDetails{}, notFoundOr(status, err)
	}
	d := RequestDetails{Title: pr.Title, Description: pr.Body, SourceBranch: pr.Head.Ref, TargetBranch: pr.Base.Ref,
		URL: pr.HTMLURL, HeadSHA: pr.Head.SHA}
	switch {
	case pr.Merged:
		d.State = store.MergeRequestMerged
	case pr.State == "closed":
		d.State = store.MergeRequestClosed
	case pr.State == "open":
		d.State = store.MergeRequestOpen
	default:
		return RequestDetails{}, fmt.Errorf("github: unknown pull request state %q", pr.State)
	}
	return d, nil
}

// reviewThreadsDetailQuery is the query of DESIGN 5.22 rule 6, with the thread's own id for the
// thread's identity.
const reviewThreadsDetailQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) { pullRequest(number: $number) { reviewThreads(first: 100) { nodes {
    id isResolved path line comments(first: 1) { nodes { author { login } body createdAt url } }
  } } } }
}`

// Threads is DESIGN 5.22 rule 6 for GitHub: the first 100 review threads through GraphQL,
// the unresolved ones, oldest first. A refusal (an HTTP 4xx answer, GraphQL's errors
// member, no pull request in the data) is *ErrThreadsUnavailable with the provider's
// first line, since the pull request itself is readable without GraphQL; a 5xx answer or
// a transport failure is the error as it came.
func (g *githubProvider) Threads(ctx context.Context, number int64) ([]Thread, error) {
	var out struct {
		Data struct {
			Repository *struct {
				PullRequest *struct {
					ReviewThreads struct {
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							Path       string `json:"path"`
							Line       *int   `json:"line"`
							Comments   struct {
								Nodes []struct {
									Author *struct {
										Login string `json:"login"`
									} `json:"author"`
									Body      string `json:"body"`
									CreatedAt string `json:"createdAt"`
									URL       string `json:"url"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	body := map[string]any{"query": reviewThreadsDetailQuery, "variables": map[string]any{"owner": g.owner, "name": g.repo, "number": number}}
	if _, err := g.api.do(ctx, http.MethodPost, g.graphqlURL(), body, &out); err != nil {
		var se *errAPIStatus
		if errors.As(err, &se) && se.status >= 400 && se.status < 500 {
			msg := se.message
			if msg == "" {
				msg = fmt.Sprintf("HTTP %d", se.status)
			}
			return nil, &ErrThreadsUnavailable{Message: msg}
		}
		return nil, err
	}
	if len(out.Errors) > 0 {
		return nil, &ErrThreadsUnavailable{Message: providerMessage([]byte(out.Errors[0].Message))}
	}
	if out.Data.Repository == nil || out.Data.Repository.PullRequest == nil {
		return nil, &ErrThreadsUnavailable{Message: "the pull request was not returned"}
	}
	threads := []Thread{}
	for _, n := range out.Data.Repository.PullRequest.ReviewThreads.Nodes {
		if n.IsResolved || len(n.Comments.Nodes) == 0 {
			continue
		}
		c := n.Comments.Nodes[0]
		t := Thread{ID: n.ID, Path: n.Path, Line: n.Line, Body: c.Body, CreatedAt: parseProviderTime(&c.CreatedAt)}
		if t.ID == "" {
			t.ID = c.URL
		}
		if c.Author != nil {
			t.Author = c.Author.Login
		}
		threads = append(threads, t)
	}
	sortThreads(threads)
	return threads, nil
}

// CIStatus is the head commit's check runs and combined status through the mapping of
// Status (an empty head is none).
func (g *githubProvider) CIStatus(ctx context.Context, _ int64, headSHA string) (string, error) {
	return g.ciStatus(ctx, headSHA)
}

// gitlabBranchStates maps a GitLab pipeline status to a BranchStatus state; a status not
// listed is pending (created, waiting_for_resource, scheduled, manual, preparing); skipped
// ran nothing, as in gitlabPipelines.
var gitlabBranchStates = map[string]string{
	"success":   BranchPassed,
	"failed":    BranchFailed,
	"canceled":  BranchFailed,
	"canceling": BranchFailed,
	"skipped":   BranchNone,
	"running":   BranchRunning,
}

// DefaultBranchStatus is GET projects/<id> for the default branch, then GET
// pipelines?ref=<branch>&order_by=id&sort=desc&per_page=1 (DESIGN 5.29 rule 12); no
// pipeline is none.
func (g *gitlabProvider) DefaultBranchStatus(ctx context.Context) (BranchStatus, error) {
	var proj struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := g.api.do(ctx, http.MethodGet, g.base, nil, &proj); err != nil {
		return BranchStatus{}, err
	}
	if proj.DefaultBranch == "" {
		return BranchStatus{State: BranchNone}, nil
	}
	var pipelines []struct {
		Status    string  `json:"status"`
		SHA       string  `json:"sha"`
		WebURL    string  `json:"web_url"`
		CreatedAt *string `json:"created_at"`
	}
	q := url.Values{"ref": {proj.DefaultBranch}, "order_by": {"id"}, "sort": {"desc"}, "per_page": {"1"}}
	if _, err := g.api.do(ctx, http.MethodGet, g.base+"/pipelines?"+q.Encode(), nil, &pipelines); err != nil {
		return BranchStatus{}, err
	}
	if len(pipelines) == 0 {
		return BranchStatus{State: BranchNone}, nil
	}
	pl := pipelines[0]
	state, ok := gitlabBranchStates[pl.Status]
	if !ok {
		state = BranchPending
	}
	if state == BranchNone {
		return BranchStatus{State: BranchNone}, nil
	}
	st := BranchStatus{State: state, SHA: pl.SHA, URL: pl.WebURL}
	if t := parseProviderTime(pl.CreatedAt); t != nil {
		st.At = *t
	}
	return st, nil
}

// DefaultBranchStatus reads the repository's default branch, its head commit, and the
// combined status and check runs of that commit (DESIGN 5.29 rule 12): any failure is
// failed, else any check or status still going is running, else passed; nothing at all is
// none. At is the newest check or status time.
func (g *githubProvider) DefaultBranchStatus(ctx context.Context) (BranchStatus, error) {
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := g.api.do(ctx, http.MethodGet, g.base, nil, &repo); err != nil {
		return BranchStatus{}, err
	}
	if repo.DefaultBranch == "" {
		return BranchStatus{State: BranchNone}, nil
	}
	var head struct {
		SHA string `json:"sha"`
	}
	if _, err := g.api.do(ctx, http.MethodGet, g.base+"/commits/"+url.PathEscape(repo.DefaultBranch), nil, &head); err != nil {
		return BranchStatus{}, err
	}
	if head.SHA == "" {
		return BranchStatus{State: BranchNone}, nil
	}
	var checks struct {
		CheckRuns []struct {
			Status      string  `json:"status"`
			Conclusion  string  `json:"conclusion"`
			HTMLURL     string  `json:"html_url"`
			StartedAt   *string `json:"started_at"`
			CompletedAt *string `json:"completed_at"`
		} `json:"check_runs"`
	}
	if _, err := g.api.do(ctx, http.MethodGet, g.base+"/commits/"+url.PathEscape(head.SHA)+"/check-runs?per_page=100", nil, &checks); err != nil {
		return BranchStatus{}, err
	}
	var combined struct {
		State      string `json:"state"`
		TotalCount int    `json:"total_count"`
		Statuses   []struct {
			State     string  `json:"state"`
			TargetURL string  `json:"target_url"`
			UpdatedAt *string `json:"updated_at"`
			CreatedAt *string `json:"created_at"`
		} `json:"statuses"`
	}
	if _, err := g.api.do(ctx, http.MethodGet, g.base+"/commits/"+url.PathEscape(head.SHA)+"/status", nil, &combined); err != nil {
		return BranchStatus{}, err
	}
	st := BranchStatus{SHA: head.SHA}
	var failed, running bool
	newest := func(t *time.Time, u string) {
		if t != nil && t.After(st.At) {
			st.At, st.URL = *t, u
		}
	}
	for _, c := range checks.CheckRuns {
		switch c.Conclusion {
		case "failure", "timed_out", "cancelled", "action_required":
			failed = true
		}
		if c.Status != "" && c.Status != "completed" {
			running = true
		}
		newest(firstTime(parseProviderTime(c.CompletedAt), parseProviderTime(c.StartedAt)), c.HTMLURL)
	}
	for _, s := range combined.Statuses {
		switch s.State {
		case "failure", "error":
			failed = true
		case "pending":
			running = true
		}
		newest(firstTime(parseProviderTime(s.UpdatedAt), parseProviderTime(s.CreatedAt)), s.TargetURL)
	}
	if combined.State == "failure" || combined.State == "error" {
		failed = true
	}
	switch {
	case len(checks.CheckRuns) == 0 && max(combined.TotalCount, len(combined.Statuses)) == 0:
		return BranchStatus{State: BranchNone}, nil
	case failed:
		st.State = BranchFailed
	case running:
		st.State = BranchRunning
	default:
		st.State = BranchPassed
	}
	return st, nil
}
