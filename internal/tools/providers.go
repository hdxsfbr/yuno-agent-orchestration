package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
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

// errAPIStatus carries a failed answer as a result.
type errAPIStatus struct{ res Result }

func (e *errAPIStatus) Error() string { return e.res.Text }

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
		return resp.StatusCode, &errAPIStatus{res: statusFailure(a.what, resp, raw)}
	}
	if out != nil {
		if r := decodeJSONBody(a.what, raw, out); r != nil {
			return resp.StatusCode, &errAPIStatus{res: *r}
		}
	}
	return resp.StatusCode, nil
}

// gitlabProvider is GitLab's merge request API for one project (DESIGN 5.16 rule 11):
// base is .../api/v4/projects/<encoded path>.
type gitlabProvider struct {
	api  *apiClient
	base string
}

func (g *gitlabProvider) branchExists(ctx context.Context, branch string) (bool, error) {
	status, err := g.api.do(ctx, http.MethodGet, g.base+"/repository/branches/"+url.PathEscape(branch), nil, nil)
	if err != nil && status == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

func (g *gitlabProvider) findOpen(ctx context.Context, branch string) (*requestRef, error) {
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
	return &requestRef{Number: open[0].IID, URL: open[0].WebURL}, nil
}

func (g *gitlabProvider) create(ctx context.Context, branch, base, title, description string) (*requestRef, error) {
	var mr struct {
		IID    int64  `json:"iid"`
		WebURL string `json:"web_url"`
	}
	body := map[string]any{"source_branch": branch, "target_branch": base, "title": title, "description": description,
		"remove_source_branch": true}
	if _, err := g.api.do(ctx, http.MethodPost, g.base+"/merge_requests", body, &mr); err != nil {
		return nil, err
	}
	return &requestRef{Number: mr.IID, URL: mr.WebURL}, nil
}

// githubProvider is GitHub's pull request API for one repository (DESIGN 5.16 rule
// 11): base is .../repos/<owner>/<repo>; owner qualifies the head branch.
type githubProvider struct {
	api   *apiClient
	base  string
	owner string
}

// ownerOf is the owner part of "owner/repo".
func ownerOf(path string) string {
	owner, _, _ := strings.Cut(path, "/")
	return owner
}

func (g *githubProvider) branchExists(ctx context.Context, branch string) (bool, error) {
	status, err := g.api.do(ctx, http.MethodGet, g.base+"/branches/"+url.PathEscape(branch), nil, nil)
	if err != nil && status == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

func (g *githubProvider) findOpen(ctx context.Context, branch string) (*requestRef, error) {
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
	return &requestRef{Number: open[0].Number, URL: open[0].HTMLURL}, nil
}

func (g *githubProvider) create(ctx context.Context, branch, base, title, description string) (*requestRef, error) {
	var pr struct {
		Number  int64  `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	body := map[string]any{"head": branch, "base": base, "title": title, "body": description}
	if _, err := g.api.do(ctx, http.MethodPost, g.base+"/pulls", body, &pr); err != nil {
		return nil, err
	}
	return &requestRef{Number: pr.Number, URL: pr.HTMLURL}, nil
}
