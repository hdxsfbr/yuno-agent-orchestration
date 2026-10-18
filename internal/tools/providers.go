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
}

func (e *errAPIStatus) Error() string { return e.res.Text }

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
		return resp.StatusCode, &errAPIStatus{res: statusFailure(a.what, resp, raw), status: resp.StatusCode, message: providerMessage(raw)}
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
	Err               string
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
	Create(ctx context.Context, branch, base, title, description string) (*RequestRef, error)
	// Status reads the request now: ErrRequestNotFound on a 404.
	Status(ctx context.Context, number int64) (RequestStatus, error)
	// Merge merges the request: *ErrNotMergeable when the provider refuses,
	// ErrRequestNotFound on a 404.
	Merge(ctx context.Context, number int64) error
}

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

func (g *gitlabProvider) Create(ctx context.Context, branch, base, title, description string) (*RequestRef, error) {
	var mr struct {
		IID    int64  `json:"iid"`
		WebURL string `json:"web_url"`
	}
	body := map[string]any{"source_branch": branch, "target_branch": base, "title": title, "description": description,
		"remove_source_branch": true}
	if _, err := g.api.do(ctx, http.MethodPost, g.base+"/merge_requests", body, &mr); err != nil {
		return nil, err
	}
	return &RequestRef{Number: mr.IID, URL: mr.WebURL}, nil
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
	}
	if status, err := g.api.do(ctx, http.MethodGet, base, nil, &mr); err != nil {
		return RequestStatus{}, notFoundOr(status, err)
	}
	state, ok := gitlabStates[mr.State]
	if !ok {
		return RequestStatus{}, fmt.Errorf("gitlab: unknown merge request state %q", mr.State)
	}
	st := RequestStatus{State: state, HeadSHA: mr.SHA, ClosedAt: firstTime(parseProviderTime(mr.MergedAt), parseProviderTime(mr.ClosedAt))}

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

	var pipelines []struct {
		Status string `json:"status"`
	}
	if _, err := g.api.do(ctx, http.MethodGet, base+"/pipelines?per_page=1", nil, &pipelines); err != nil {
		return RequestStatus{}, err
	}
	st.CIStatus = store.CIStatusNone
	if len(pipelines) > 0 {
		if ci, ok := gitlabPipelines[pipelines[0].Status]; ok {
			st.CIStatus = ci
		} else {
			st.CIStatus = store.CIStatusPending
		}
	}
	return st, nil
}

// Merge is PUT merge_requests/<iid>/merge removing the source branch (rule 5).
func (g *gitlabProvider) Merge(ctx context.Context, number int64) error {
	_, err := g.api.do(ctx, http.MethodPut, fmt.Sprintf("%s/merge_requests/%d/merge", g.base, number),
		map[string]any{"should_remove_source_branch": true}, nil)
	return mergeError(err)
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

func (g *githubProvider) Create(ctx context.Context, branch, base, title, description string) (*RequestRef, error) {
	var pr struct {
		Number  int64  `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	body := map[string]any{"head": branch, "base": base, "title": title, "body": description}
	if _, err := g.api.do(ctx, http.MethodPost, g.base+"/pulls", body, &pr); err != nil {
		return nil, err
	}
	return &RequestRef{Number: pr.Number, URL: pr.HTMLURL}, nil
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
