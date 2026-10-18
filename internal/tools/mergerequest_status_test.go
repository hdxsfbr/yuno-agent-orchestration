package tools

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"yuno/internal/project"
)

const statusTestToken = "glpat-not-a-real-token-00"

func gitlabStatusProvider(srv *httptest.Server) RequestProvider {
	pc := project.Config{RepoURL: "https://gitlab.test/group/demo.git", Token: statusTestToken, DefaultBranch: "main", Provider: project.ProviderGitLab}
	return newRequestProvider(&pc, ProviderClient(nil), srv.URL+"/api/v4/projects/group%2Fdemo", "group/demo")
}

func githubStatusProvider(srv *httptest.Server) RequestProvider {
	pc := project.Config{RepoURL: "https://github.com/owner/demo", Token: "ghp_notARealTokenValue0123456789", DefaultBranch: "main", Provider: project.ProviderGitHub}
	return newRequestProvider(&pc, ProviderClient(nil), srv.URL+"/repos/owner/demo", "owner/demo")
}

type jm = map[string]any

func discussion(resolvable, resolved bool) jm {
	return jm{"notes": []jm{{"resolvable": resolvable, "resolved": resolved}}}
}

// T5.20.3 (GitLab): the mapping table for state, the pipeline's status and the
// unresolved threads, over the fake's scripted answers.
func TestGitLabRequestStatus(t *testing.T) {
	merged := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	closed := time.Date(2026, 10, 17, 9, 45, 0, 0, time.UTC)
	cases := []struct {
		name        string
		mr          jm
		discussions []jm
		pipelines   []jm
		state       string
		ci          string
		threads     int
		closedAt    *time.Time
	}{
		{name: "opened, no pipeline, no threads", mr: jm{"state": "opened", "sha": "abc"}, discussions: []jm{}, pipelines: []jm{},
			state: "open", ci: "none"},
		{name: "threads: only a resolvable, unresolved first note counts",
			mr: jm{"state": "opened", "sha": "abc"}, pipelines: []jm{},
			discussions: []jm{discussion(true, false), discussion(true, true), discussion(false, false),
				{"notes": []jm{{"resolvable": false, "resolved": false}, {"resolvable": true, "resolved": false}}}, {"notes": []jm{}}, discussion(true, false)},
			state: "open", ci: "none", threads: 2},
		{name: "merged, merged_at", mr: jm{"state": "merged", "sha": "def", "merged_at": merged.Format(time.RFC3339Nano)},
			discussions: []jm{}, pipelines: []jm{{"status": "success"}}, state: "merged", ci: "passed", closedAt: &merged},
		{name: "closed, closed_at", mr: jm{"state": "closed", "sha": "def", "closed_at": closed.Format("2006-01-02T15:04:05.000Z")},
			discussions: []jm{}, pipelines: []jm{}, state: "closed", ci: "none", closedAt: &closed},
		{name: "locked is closed", mr: jm{"state": "locked", "sha": "def"}, discussions: []jm{}, pipelines: []jm{}, state: "closed", ci: "none"},
		{name: "pipeline failed", mr: jm{"state": "opened"}, discussions: []jm{}, pipelines: []jm{{"status": "failed"}}, state: "open", ci: "failed"},
		{name: "pipeline canceled", mr: jm{"state": "opened"}, discussions: []jm{}, pipelines: []jm{{"status": "canceled"}}, state: "open", ci: "failed"},
		{name: "pipeline created", mr: jm{"state": "opened"}, discussions: []jm{}, pipelines: []jm{{"status": "created"}}, state: "open", ci: "pending"},
		{name: "pipeline waiting_for_resource", mr: jm{"state": "opened"}, discussions: []jm{}, pipelines: []jm{{"status": "waiting_for_resource"}}, state: "open", ci: "pending"},
		{name: "pipeline preparing", mr: jm{"state": "opened"}, discussions: []jm{}, pipelines: []jm{{"status": "preparing"}}, state: "open", ci: "pending"},
		{name: "pipeline pending", mr: jm{"state": "opened"}, discussions: []jm{}, pipelines: []jm{{"status": "pending"}}, state: "open", ci: "pending"},
		{name: "pipeline running", mr: jm{"state": "opened"}, discussions: []jm{}, pipelines: []jm{{"status": "running"}}, state: "open", ci: "pending"},
		{name: "pipeline manual", mr: jm{"state": "opened"}, discussions: []jm{}, pipelines: []jm{{"status": "manual"}}, state: "open", ci: "pending"},
		{name: "pipeline scheduled", mr: jm{"state": "opened"}, discussions: []jm{}, pipelines: []jm{{"status": "scheduled"}}, state: "open", ci: "pending"},
		{name: "pipeline skipped is none", mr: jm{"state": "opened"}, discussions: []jm{}, pipelines: []jm{{"status": "skipped"}}, state: "open", ci: "none"},
		{name: "only the first entry counts", mr: jm{"state": "opened"}, discussions: []jm{},
			pipelines: []jm{{"status": "running"}, {"status": "failed"}}, state: "open", ci: "pending"},
		{name: "a status this table does not know is never passed", mr: jm{"state": "opened"}, discussions: []jm{},
			pipelines: []jm{{"status": "something_new"}}, state: "open", ci: "pending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, srv := newFakeGitLab(t)
			script(&g.scripted, "GET /merge_requests/7", 200, tc.mr)
			script(&g.scripted, "GET /merge_requests/7/discussions", 200, tc.discussions)
			script(&g.scripted, "GET /merge_requests/7/pipelines", 200, tc.pipelines)
			st, err := gitlabStatusProvider(srv).Status(context.Background(), 7)
			if err != nil {
				t.Fatal(err)
			}
			if st.State != tc.state || st.CIStatus != tc.ci || st.UnresolvedThreads == nil || *st.UnresolvedThreads != tc.threads || st.Err != "" {
				t.Fatalf("status = %+v (threads %v)", st, st.UnresolvedThreads)
			}
			if sha, _ := tc.mr["sha"].(string); st.HeadSHA != sha {
				t.Fatalf("head sha = %q, want %q", st.HeadSHA, sha)
			}
			if (tc.closedAt == nil) != (st.ClosedAt == nil) || tc.closedAt != nil && !tc.closedAt.Equal(*st.ClosedAt) {
				t.Fatalf("closed at = %v, want %v", st.ClosedAt, tc.closedAt)
			}
			if g.token != statusTestToken {
				t.Fatalf("the token was not sent: %q", g.token)
			}
		})
	}
}

// T5.20.4 (provider side): a 404 for the request is ErrRequestNotFound, any other
// failure is an error that is not, with the right transient class; a state the table
// lacks is an error.
func TestRequestStatusFailures(t *testing.T) {
	g, srv := newFakeGitLab(t)
	p := gitlabStatusProvider(srv)
	script(&g.scripted, "GET /merge_requests/7", 404, jm{"message": "404 Not found"})
	if _, err := p.Status(context.Background(), 7); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("404 = %v", err)
	}
	script(&g.scripted, "GET /merge_requests/7", 500, jm{"message": "boom"})
	if _, err := p.Status(context.Background(), 7); err == nil || errors.Is(err, ErrRequestNotFound) || !TransientError(err) || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("500 = %v", err)
	}
	script(&g.scripted, "GET /merge_requests/7", 401, jm{"message": "401 Unauthorized"})
	if _, err := p.Status(context.Background(), 7); err == nil || TransientError(err) {
		t.Fatalf("401 = %v", err)
	}
	script(&g.scripted, "GET /merge_requests/7", 200, jm{"state": "wat"})
	if _, err := p.Status(context.Background(), 7); err == nil || !strings.Contains(err.Error(), "unknown merge request state") {
		t.Fatalf("unknown state = %v", err)
	}
	// A 404 on a later call is not the request being gone.
	script(&g.scripted, "GET /merge_requests/7", 200, jm{"state": "opened"})
	script(&g.scripted, "GET /merge_requests/7/discussions", 404, jm{"message": "404"})
	if _, err := p.Status(context.Background(), 7); err == nil || errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("404 on the discussions = %v", err)
	}
	// A refused connection is transient too.
	srv.Close()
	if _, err := p.Status(context.Background(), 7); err == nil || !TransientError(err) {
		t.Fatalf("connection refused = %v", err)
	}
}

func pull(state string, merged bool, extra jm) jm {
	out := jm{"state": state, "merged": merged, "head": jm{"sha": "feedface"}}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func checks(runs ...jm) jm { return jm{"total_count": len(runs), "check_runs": runs} }

func combined(state string, statuses int) jm {
	list := make([]jm, statuses)
	for i := range list {
		list[i] = jm{"state": state}
	}
	return jm{"state": state, "total_count": statuses, "statuses": list}
}

func graphqlThreads(resolved ...bool) jm {
	nodes := make([]jm, len(resolved))
	for i, r := range resolved {
		nodes[i] = jm{"isResolved": r}
	}
	return jm{"data": jm{"repository": jm{"pullRequest": jm{"reviewThreads": jm{"nodes": nodes}}}}}
}

// T5.20.3 (GitHub): state, review threads through GraphQL, and the check runs and
// combined status folded into one pipeline status, including none.
func TestGitHubRequestStatus(t *testing.T) {
	mergedAt := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	run := func(status, conclusion string) jm { return jm{"status": status, "conclusion": conclusion} }
	cases := []struct {
		name     string
		pull     jm
		checks   jm
		combined jm
		state    string
		ci       string
		closedAt *time.Time
	}{
		{name: "open, nothing reports: none", pull: pull("open", false, nil), checks: checks(), combined: combined("pending", 0), state: "open", ci: "none"},
		{name: "merged", pull: pull("closed", true, jm{"merged_at": mergedAt.Format(time.RFC3339)}), checks: checks(run("completed", "success")),
			combined: combined("success", 0), state: "merged", ci: "passed", closedAt: &mergedAt},
		{name: "closed without merge", pull: pull("closed", false, jm{"closed_at": mergedAt.Format(time.RFC3339)}), checks: checks(), combined: combined("pending", 0),
			state: "closed", ci: "none", closedAt: &mergedAt},
		{name: "green checks, skipped and neutral count as passed", pull: pull("open", false, nil),
			checks: checks(run("completed", "success"), run("completed", "skipped"), run("completed", "neutral")), combined: combined("pending", 0), state: "open", ci: "passed"},
		{name: "failure", pull: pull("open", false, nil), checks: checks(run("completed", "success"), run("completed", "failure")), combined: combined("success", 1), state: "open", ci: "failed"},
		{name: "timed_out", pull: pull("open", false, nil), checks: checks(run("completed", "timed_out")), combined: combined("pending", 0), state: "open", ci: "failed"},
		{name: "cancelled", pull: pull("open", false, nil), checks: checks(run("completed", "cancelled")), combined: combined("pending", 0), state: "open", ci: "failed"},
		{name: "action_required", pull: pull("open", false, nil), checks: checks(run("completed", "action_required")), combined: combined("pending", 0), state: "open", ci: "failed"},
		{name: "queued", pull: pull("open", false, nil), checks: checks(run("completed", "success"), run("queued", "")), combined: combined("pending", 0), state: "open", ci: "pending"},
		{name: "in_progress", pull: pull("open", false, nil), checks: checks(run("in_progress", "")), combined: combined("pending", 0), state: "open", ci: "pending"},
		{name: "failed beats pending", pull: pull("open", false, nil), checks: checks(run("queued", ""), run("completed", "failure")), combined: combined("pending", 1), state: "open", ci: "failed"},
		{name: "combined failure with green checks", pull: pull("open", false, nil), checks: checks(run("completed", "success")), combined: combined("failure", 1), state: "open", ci: "failed"},
		{name: "combined pending with a status", pull: pull("open", false, nil), checks: checks(), combined: combined("pending", 1), state: "open", ci: "pending"},
		{name: "combined success alone", pull: pull("open", false, nil), checks: checks(), combined: combined("success", 2), state: "open", ci: "passed"},
		{name: "green checks and a pending status", pull: pull("open", false, nil), checks: checks(run("completed", "success")), combined: combined("pending", 1), state: "open", ci: "pending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, srv := newFakeGitHub(t, "")
			script(&g.scripted, "GET /pulls/5", 200, tc.pull)
			script(&g.scripted, "POST /graphql", 200, graphqlThreads(true, false, false))
			script(&g.scripted, "GET /commits/feedface/check-runs", 200, tc.checks)
			script(&g.scripted, "GET /commits/feedface/status", 200, tc.combined)
			st, err := githubStatusProvider(srv).Status(context.Background(), 5)
			if err != nil {
				t.Fatal(err)
			}
			if st.State != tc.state || st.CIStatus != tc.ci || st.UnresolvedThreads == nil || *st.UnresolvedThreads != 2 || st.HeadSHA != "feedface" || st.Err != "" {
				t.Fatalf("status = %+v (threads %v)", st, st.UnresolvedThreads)
			}
			if (tc.closedAt == nil) != (st.ClosedAt == nil) || tc.closedAt != nil && !tc.closedAt.Equal(*st.ClosedAt) {
				t.Fatalf("closed at = %v, want %v", st.ClosedAt, tc.closedAt)
			}
			if g.auth != "Bearer ghp_notARealTokenValue0123456789" {
				t.Fatalf("auth = %q", g.auth)
			}
			// The GraphQL call carries the owner, the repository and the number, and the first 100 threads.
			body := g.bodies["POST /graphql"]
			vars, _ := body["variables"].(map[string]any)
			if vars["owner"] != "owner" || vars["name"] != "demo" || vars["number"] != float64(5) || !strings.Contains(body["query"].(string), "reviewThreads(first: 100)") {
				t.Fatalf("graphql body = %v", body)
			}
		})
	}
}

// T5.20.3 (GitHub GraphQL): a refusal leaves the count unknown and sets Err, and the
// rest of the status is still returned; a 404 for the pull request is ErrRequestNotFound.
func TestGitHubReviewThreadsRefused(t *testing.T) {
	refusals := []struct {
		name   string
		status int
		body   any
		want   string
	}{
		{"http 403", 403, jm{"message": "Resource not accessible by personal access token"}, "HTTP 403"},
		{"http 401", 401, jm{"message": "Bad credentials"}, "HTTP 401"},
		{"errors member", 200, jm{"data": nil, "errors": []jm{{"message": "Resource not accessible by personal access token\nsecond line"}}}, "Resource not accessible by personal access token"},
		{"no pull request in the data", 200, jm{"data": jm{"repository": jm{"pullRequest": nil}}}, "not returned"},
		{"server error", 502, jm{"message": "bad gateway"}, "HTTP 502"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			g, srv := newFakeGitHub(t, "")
			script(&g.scripted, "GET /pulls/5", 200, pull("open", false, nil))
			script(&g.scripted, "POST /graphql", tc.status, tc.body)
			script(&g.scripted, "GET /commits/feedface/check-runs", 200, checks(jm{"status": "completed", "conclusion": "success"}))
			script(&g.scripted, "GET /commits/feedface/status", 200, combined("pending", 0))
			st, err := githubStatusProvider(srv).Status(context.Background(), 5)
			if err != nil {
				t.Fatalf("a GraphQL refusal must not fail the status: %v", err)
			}
			if st.UnresolvedThreads != nil || !strings.Contains(st.Err, tc.want) || strings.Contains(st.Err, "second line") {
				t.Fatalf("threads %v, err %q", st.UnresolvedThreads, st.Err)
			}
			if st.State != "open" || st.CIStatus != "passed" || st.HeadSHA != "feedface" {
				t.Fatalf("the rest of the status is lost: %+v", st)
			}
		})
	}

	g, srv := newFakeGitHub(t, "")
	script(&g.scripted, "GET /pulls/5", 404, jm{"message": "Not Found"})
	if _, err := githubStatusProvider(srv).Status(context.Background(), 5); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("404 = %v", err)
	}
	script(&g.scripted, "GET /pulls/5", 200, pull("open", false, nil))
	script(&g.scripted, "POST /graphql", 200, graphqlThreads())
	script(&g.scripted, "GET /commits/feedface/check-runs", 500, jm{"message": "boom"})
	if _, err := githubStatusProvider(srv).Status(context.Background(), 5); err == nil || !TransientError(err) {
		t.Fatalf("a failed check-runs call must fail the status transiently: %v", err)
	}
	script(&g.scripted, "GET /pulls/5", 200, jm{"state": "draft?"})
	if _, err := githubStatusProvider(srv).Status(context.Background(), 5); err == nil || !strings.Contains(err.Error(), "unknown pull request state") {
		t.Fatalf("unknown state = %v", err)
	}
}

// ProviderFor aims at the provider's real API shape: GitHub's GraphQL sits beside the
// repos root, another host's under /api (the REST root is /api/v3).
func TestProviderForURLs(t *testing.T) {
	gh := &githubProvider{base: "https://api.github.com/repos/owner/demo", owner: "owner", repo: "demo"}
	if got := gh.graphqlURL(); got != "https://api.github.com/graphql" {
		t.Fatalf("github.com graphql = %q", got)
	}
	ghe := &githubProvider{base: "https://ghe.example.com/api/v3/repos/o/r", owner: "o", repo: "r"}
	if got := ghe.graphqlURL(); got != "https://ghe.example.com/api/graphql" {
		t.Fatalf("enterprise graphql = %q", got)
	}
	odd := &githubProvider{base: "https://api.github.com/repos/repos/x", owner: "repos", repo: "x"}
	if got := odd.graphqlURL(); got != "https://api.github.com/graphql" {
		t.Fatalf("an owner named repos = %q", got)
	}

	// Through ProviderFor and a transport that sends everything to the fakes.
	g, srv := newFakeGitHub(t, "")
	script(&g.scripted, "GET /pulls/5", 200, pull("open", false, nil))
	script(&g.scripted, "POST /graphql", 200, graphqlThreads(false))
	script(&g.scripted, "GET /commits/feedface/check-runs", 200, checks())
	script(&g.scripted, "GET /commits/feedface/status", 200, combined("pending", 0))
	target, _ := url.Parse(srv.URL)
	client := ProviderClient(rewriteTo{target})
	pc := project.Config{RepoURL: "https://github.com/owner/demo.git", Token: "ghp_notARealTokenValue0123456789", DefaultBranch: "main", Provider: project.ProviderGitHub}
	st, err := ProviderFor(&pc, client).Status(context.Background(), 5)
	if err != nil || st.UnresolvedThreads == nil || *st.UnresolvedThreads != 1 || st.CIStatus != "none" {
		t.Fatalf("through ProviderFor = %+v %v", st, err)
	}
}

// rewriteTo sends every request to target, whatever host it names.
type rewriteTo struct{ target *url.URL }

func (rt rewriteTo) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host, r2.Host = rt.target.Scheme, rt.target.Host, ""
	return http.DefaultTransport.RoundTrip(r2)
}

// DESIGN 5.20 rule 5 (provider side): GitLab merges with should_remove_source_branch,
// GitHub with merge_method merge; 405, 406, 409 and 422 are refusals carrying the first
// line of the provider's message; 404 is not found; 5xx is transient; other statuses
// are permanent errors.
func TestMergeRequestMerge(t *testing.T) {
	g, gsrv := newFakeGitLab(t)
	script(&g.scripted, "PUT /merge_requests/7/merge", 200, jm{"state": "merged"})
	if err := gitlabStatusProvider(gsrv).Merge(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if g.bodies["PUT /merge_requests/7/merge"]["should_remove_source_branch"] != true {
		t.Fatalf("gitlab merge body = %v", g.bodies)
	}
	h, hsrv := newFakeGitHub(t, "")
	script(&h.scripted, "PUT /pulls/5/merge", 200, jm{"merged": true, "sha": "abc"})
	if err := githubStatusProvider(hsrv).Merge(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if h.bodies["PUT /pulls/5/merge"]["merge_method"] != "merge" {
		t.Fatalf("github merge body = %v", h.bodies)
	}

	for _, status := range []int{405, 406, 409, 422} {
		for _, side := range []struct {
			name string
			p    func() RequestProvider
			set  func(int, any)
		}{
			{"gitlab", func() RequestProvider { return gitlabStatusProvider(gsrv) }, func(s int, b any) { script(&g.scripted, "PUT /merge_requests/7/merge", s, b) }},
			{"github", func() RequestProvider { return githubStatusProvider(hsrv) }, func(s int, b any) { script(&h.scripted, "PUT /pulls/7/merge", s, b) }},
		} {
			side.set(status, jm{"message": "Branch cannot be merged\nconflicts in README.md", "documentation_url": "https://docs"})
			err := side.p().Merge(context.Background(), 7)
			var nm *ErrNotMergeable
			if !errors.As(err, &nm) || nm.Message != "Branch cannot be merged" {
				t.Fatalf("%s %d: err = %v", side.name, status, err)
			}
		}
	}
	// A refusal whose body is not JSON: its first line.
	script(&g.scripted, "PUT /merge_requests/7/merge", 405, "405 Method Not Allowed")
	var nm *ErrNotMergeable
	if err := gitlabStatusProvider(gsrv).Merge(context.Background(), 7); !errors.As(err, &nm) || !strings.Contains(nm.Message, "405 Method Not Allowed") {
		t.Fatalf("plain refusal = %v", err)
	}
	script(&g.scripted, "PUT /merge_requests/7/merge", 404, jm{"message": "404 Not found"})
	if err := gitlabStatusProvider(gsrv).Merge(context.Background(), 7); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("404 = %v", err)
	}
	script(&g.scripted, "PUT /merge_requests/7/merge", 503, jm{"message": "down"})
	if err := gitlabStatusProvider(gsrv).Merge(context.Background(), 7); err == nil || !TransientError(err) || errors.As(err, &nm) {
		t.Fatalf("503 = %v", err)
	}
	script(&g.scripted, "PUT /merge_requests/7/merge", 403, jm{"message": "403 Forbidden"})
	if err := gitlabStatusProvider(gsrv).Merge(context.Background(), 7); err == nil || TransientError(err) || errors.As(err, &nm) || errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("403 = %v", err)
	}
	// The token never travels into an error text.
	if strings.Contains(statusErr(gitlabStatusProvider(gsrv).Merge(context.Background(), 7)), statusTestToken) {
		t.Fatal("the token is in the error")
	}
}

func statusErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
