package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"yuno/internal/project"
	"yuno/internal/store"
)

// Chief platform control, DESIGN 5.34 rules 4, 13, 14 and 15: the request shapes of the
// provider reads and of the merge with the head's sha, for GitLab and GitHub.

// shapeFake answers scripted routes ("GET /merge_requests/7" below the base, a raw text
// answer when the body is a string) and records every request with its query, headers
// and JSON body.
type shapeFake struct {
	mu       sync.Mutex
	prefix   string
	routes   map[string]shapeAnswer
	requests []shapeRequest
	srv      *httptest.Server
}

type shapeAnswer struct {
	status   int
	body     any
	location string
}

type shapeRequest struct {
	route string
	query url.Values
	auth  string
	token string
	body  map[string]any
}

func newShapeFake(t *testing.T, prefix string) *shapeFake {
	t.Helper()
	f := &shapeFake{prefix: prefix, routes: map[string]shapeAnswer{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := r.Method + " " + strings.TrimPrefix(r.URL.EscapedPath(), f.prefix)
		req := shapeRequest{route: route, query: r.URL.Query(), auth: r.Header.Get("Authorization"), token: r.Header.Get("PRIVATE-TOKEN")}
		if r.ContentLength != 0 {
			_ = json.NewDecoder(r.Body).Decode(&req.body)
		}
		f.mu.Lock()
		f.requests = append(f.requests, req)
		a, ok := f.routes[route+"?"+r.URL.RawQuery]
		if !ok {
			a, ok = f.routes[route]
		}
		f.mu.Unlock()
		switch {
		case !ok:
			writeJSONTest(w, 404, jm{"message": "404 Not Found"})
		case a.location != "":
			w.Header().Set("Location", a.location)
			w.WriteHeader(a.status)
		default:
			if text, isText := a.body.(string); isText {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(a.status)
				_, _ = w.Write([]byte(text))
				return
			}
			writeJSONTest(w, a.status, a.body)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *shapeFake) on(route string, status int, body any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[route] = shapeAnswer{status: status, body: body}
}

func (f *shapeFake) redirect(route, location string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[route] = shapeAnswer{status: http.StatusFound, location: location}
}

// take returns the requests recorded since the last take.
func (f *shapeFake) take() []shapeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.requests
	f.requests = nil
	return out
}

func routes(reqs []shapeRequest) []string {
	var out []string
	for _, r := range reqs {
		out = append(out, r.route)
	}
	return out
}

func wantQuery(t *testing.T, r shapeRequest, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got := r.query.Get(k); got != v {
			t.Fatalf("%s: query %s = %q, want %q (%v)", r.route, k, got, v, r.query)
		}
	}
}

const head40 = "0123456789abcdef0123456789abcdef01234567"

func TestGitLabWave3Requests(t *testing.T) {
	f := newShapeFake(t, "/api/v4/projects/group%2Fdemo")
	pc := project.Config{RepoURL: "https://gitlab.test/group/demo.git", Token: statusTestToken, DefaultBranch: "main", Provider: project.ProviderGitLab}
	p := newRequestProvider(&pc, ProviderClient(nil), f.srv.URL+"/api/v4/projects/group%2Fdemo", "group/demo")
	ctx := context.Background()

	// Status reads the pipelines of exactly the head, never the request's newest.
	f.on("GET /merge_requests/7", 200, jm{"state": "opened", "sha": head40, "detailed_merge_status": "mergeable", "has_conflicts": false})
	f.on("GET /merge_requests/7/discussions", 200, []jm{})
	f.on("GET /pipelines", 200, []jm{{"id": 9, "sha": head40, "ref": "yuno/x", "status": "success", "web_url": "https://gitlab.test/p/9"}})
	st, err := p.Status(ctx, 7)
	if err != nil || st.CIStatus != store.CIStatusPassed || st.PipelineID != 9 || st.CIURL != "https://gitlab.test/p/9" || st.Mergeable == nil || !*st.Mergeable ||
		st.MergeStatus != "mergeable" {
		t.Fatalf("status = %+v %v", st, err)
	}
	reqs := f.take()
	if got := routes(reqs); strings.Join(got, ",") != "GET /merge_requests/7,GET /merge_requests/7/discussions,GET /pipelines" {
		t.Fatalf("status calls = %v", got)
	}
	wantQuery(t, reqs[2], map[string]string{"sha": head40, "order_by": "id", "sort": "desc", "per_page": "1"})
	if reqs[2].token != statusTestToken {
		t.Fatal("the token was not sent")
	}
	for detailed, want := range map[string]*bool{"checking": nil, "unchecked": nil, "conflict": new(false), "ci_must_pass": new(true)} {
		got := gitlabMergeable(detailed, false)
		if (got == nil) != (want == nil) || got != nil && *got != *want {
			t.Fatalf("mergeable(%s) = %v", detailed, got)
		}
	}
	if got := gitlabMergeable("mergeable", true); got == nil || *got {
		t.Fatal("has_conflicts is not mergeable")
	}

	// CIStatus with an empty head asks nothing; with a head, that head's pipelines.
	if ci, err := p.CIStatus(ctx, 7, ""); err != nil || ci != store.CIStatusNone || len(f.take()) != 0 {
		t.Fatalf("no head = %q %v", ci, err)
	}

	// Checks: the pipeline, then its jobs.
	f.on("GET /pipelines/9/jobs", 200, []jm{
		{"id": 31, "name": "build", "stage": "build", "status": "success", "web_url": "https://gitlab.test/j/31"},
		{"id": 32, "name": "unit", "stage": "test", "status": "failed", "web_url": "https://gitlab.test/j/32"},
		{"id": 33, "name": "deploy", "stage": "deploy", "status": "manual"},
		{"id": 34, "name": "lint", "stage": "test", "status": "created"},
	})
	c, err := p.Checks(ctx, 7, head40)
	if err != nil || c.SHA != head40 || c.Status != store.CIStatusPassed || c.PipelineID != 9 || len(c.Jobs) != 4 {
		t.Fatalf("checks = %+v %v", c, err)
	}
	wantJobs := []CheckJob{{"31", "build", "build", JobPassed, "https://gitlab.test/j/31"}, {"32", "unit", "test", JobFailed, "https://gitlab.test/j/32"},
		{"33", "deploy", "deploy", JobManual, ""}, {"34", "lint", "test", JobPending, ""}}
	for i, j := range wantJobs {
		if c.Jobs[i] != j {
			t.Fatalf("job %d = %+v, want %+v", i, c.Jobs[i], j)
		}
	}
	reqs = f.take()
	wantQuery(t, reqs[0], map[string]string{"sha": head40})
	wantQuery(t, reqs[1], map[string]string{"per_page": "100"})
	f.on("GET /pipelines", 200, []jm{})
	if c, err := p.Checks(ctx, 7, head40); err != nil || c.Status != store.CIStatusNone || len(c.Jobs) != 0 || len(f.take()) != 1 {
		t.Fatalf("no pipeline = %+v %v", c, err)
	}

	// Diff: pages of 100 until a short one, or the limit.
	page1 := make([]jm, 0, 100)
	for i := range 100 {
		page1 = append(page1, jm{"old_path": fmt.Sprintf("f%d", i), "new_path": fmt.Sprintf("f%d", i), "diff": "@@ -1 +1 @@\n-a\n+b\n"})
	}
	f.on("GET /merge_requests/7/diffs?page=1&per_page=100", 200, page1)
	f.on("GET /merge_requests/7/diffs?page=2&per_page=100", 200, []jm{
		{"old_path": "new.txt", "new_path": "new.txt", "new_file": true, "diff": "@@ +1 @@\n+x\n"},
		{"old_path": "gone.txt", "new_path": "gone.txt", "deleted_file": true, "diff": ""},
		{"old_path": "old.txt", "new_path": "moved.txt", "renamed_file": true, "diff": ""},
	})
	d, err := p.Diff(ctx, 7, 1<<20)
	if err != nil || d.More || len(d.Files) != 103 || d.Files[100] != (DiffFile{Path: "new.txt", Status: "added", Patch: "@@ +1 @@\n+x\n"}) ||
		d.Files[101] != (DiffFile{Path: "gone.txt", Status: "deleted"}) || d.Files[102] != (DiffFile{Path: "moved.txt", OldPath: "old.txt", Status: "renamed"}) ||
		d.Files[0].Status != "modified" {
		t.Fatalf("diff = %d files, more %v, %v", len(d.Files), d.More, err)
	}
	if got := routes(f.take()); len(got) != 2 {
		t.Fatalf("diff calls = %v", got)
	}
	if d, err := p.Diff(ctx, 7, 10); err != nil || !d.More || len(d.Files) != 100 || len(f.take()) != 1 {
		t.Fatalf("diff at the limit = %d files, more %v, %v", len(d.Files), d.More, err)
	}
	if _, err := p.Diff(ctx, 8, 10); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("diff of a missing request = %v", err)
	}

	// JobLog: the trace as text, its tail; an id that is not a number asks nothing.
	f.on("GET /jobs/55/trace", 200, "line one\nline two\nthe end\n")
	lg, err := p.JobLog(ctx, "55", 8)
	if err != nil || string(lg.Text) != "the end\n" || lg.Size != 26 || !lg.Truncated {
		t.Fatalf("log = %+v %v", lg, err)
	}
	if lg, err := p.JobLog(ctx, "55", 1000); err != nil || lg.Truncated || string(lg.Text) != "line one\nline two\nthe end\n" {
		t.Fatalf("whole log = %+v %v", lg, err)
	}
	f.take()
	for _, id := range []string{"../55", "status-3", "", "55/trace"} {
		if _, err := p.JobLog(ctx, id, 10); !errors.Is(err, ErrJobNotFound) {
			t.Fatalf("job %q = %v", id, err)
		}
	}
	if _, err := p.JobLog(ctx, "56", 10); !errors.Is(err, ErrJobNotFound) || len(f.take()) != 1 {
		t.Fatalf("unknown job = %v", err)
	}
	f.on("GET /jobs/57/trace", 500, jm{"message": "boom"})
	if _, err := p.JobLog(ctx, "57", 10); err == nil || errors.Is(err, ErrJobNotFound) || !TransientError(err) {
		t.Fatalf("a 500 = %v", err)
	}

	// Merge sends the head; a 409 to it is the head that moved.
	f.on("PUT /merge_requests/7/merge", 200, jm{"state": "merged"})
	f.take()
	if err := p.Merge(ctx, 7, head40); err != nil {
		t.Fatal(err)
	}
	if body := f.take()[0].body; body["sha"] != head40 || body["should_remove_source_branch"] != true {
		t.Fatalf("merge body = %v", body)
	}
	f.on("PUT /merge_requests/7/merge", 409, jm{"message": "SHA does not match HEAD of source branch: abc"})
	var moved *ErrHeadMoved
	if err := p.Merge(ctx, 7, head40); !errors.As(err, &moved) || moved.Message != "SHA does not match HEAD of source branch: abc" {
		t.Fatalf("409 = %v", err)
	}

	// CIConfigured: the branch's last 20 commits, then its pipelines.
	f.take()
	f.on("GET /repository/commits", 200, []jm{{"id": "c3"}, {"id": "c2"}})
	f.on("GET /pipelines", 200, []jm{{"id": 1, "sha": "old"}, {"id": 2, "sha": "c2"}})
	if ok, err := p.CIConfigured(ctx, "main"); err != nil || !ok {
		t.Fatalf("configured = %v %v", ok, err)
	}
	reqs = f.take()
	wantQuery(t, reqs[0], map[string]string{"ref_name": "main", "per_page": "20"})
	wantQuery(t, reqs[1], map[string]string{"ref": "main", "order_by": "id", "sort": "desc", "per_page": "100"})
	f.on("GET /pipelines", 200, []jm{{"id": 1, "sha": "old"}})
	if ok, err := p.CIConfigured(ctx, "main"); err != nil || ok {
		t.Fatalf("only older commits have pipelines = %v %v", ok, err)
	}
	f.on("GET /repository/commits", 200, []jm{})
	f.take()
	if ok, err := p.CIConfigured(ctx, "main"); err != nil || ok || len(f.take()) != 1 {
		t.Fatalf("no commits = %v %v", ok, err)
	}
}

func TestGitHubWave3Requests(t *testing.T) {
	f := newShapeFake(t, "/repos/owner/demo")
	logs := newShapeFake(t, "")
	pc := project.Config{RepoURL: "https://github.com/owner/demo", Token: "ghp_notARealTokenValue0123456789", DefaultBranch: "main", Provider: project.ProviderGitHub}
	p := newRequestProvider(&pc, ProviderClient(nil), f.srv.URL+"/repos/owner/demo", "owner/demo")
	ctx := context.Background()

	checkRuns := jm{"total_count": 2, "check_runs": []jm{
		{"id": 1, "name": "build", "status": "completed", "conclusion": "success", "html_url": "https://github.test/c/1"},
		{"id": 2, "name": "test", "status": "in_progress", "html_url": "https://github.test/c/2"},
	}}
	combined := jm{"state": "failure", "total_count": 1, "statuses": []jm{
		{"id": 3, "context": "ci/external", "state": "failure", "target_url": "https://ci.test/3"},
	}}
	f.on("GET /pulls/5", 200, jm{"state": "open", "merged": false, "head": jm{"sha": head40}, "mergeable": true, "mergeable_state": "unstable"})
	f.on("POST /graphql", 200, jm{"data": jm{"repository": jm{"pullRequest": jm{"reviewThreads": jm{"nodes": []jm{}}}}}})
	f.on("GET /commits/"+head40+"/check-runs", 200, checkRuns)
	f.on("GET /commits/"+head40+"/status", 200, combined)

	st, err := p.Status(ctx, 5)
	if err != nil || st.CIStatus != store.CIStatusFailed || st.CIURL != "https://ci.test/3" || st.Mergeable == nil || !*st.Mergeable ||
		st.MergeStatus != "unstable" || st.PipelineID != 0 {
		t.Fatalf("status = %+v %v", st, err)
	}
	f.take()

	c, err := p.Checks(ctx, 5, head40)
	if err != nil || c.Status != store.CIStatusFailed || c.URL != "https://ci.test/3" || len(c.Jobs) != 3 {
		t.Fatalf("checks = %+v %v", c, err)
	}
	want := []CheckJob{{"1", "build", "", JobPassed, "https://github.test/c/1"}, {"2", "test", "", JobRunning, "https://github.test/c/2"},
		{"status-3", "ci/external", "", JobFailed, "https://ci.test/3"}}
	for i, j := range want {
		if c.Jobs[i] != j {
			t.Fatalf("job %d = %+v, want %+v", i, c.Jobs[i], j)
		}
	}
	if got := routes(f.take()); strings.Join(got, ",") != "GET /commits/"+head40+"/check-runs,GET /commits/"+head40+"/status" {
		t.Fatalf("checks calls = %v", got)
	}
	// Still going, nothing failed: the running check decides the URL.
	f.on("GET /commits/"+head40+"/status", 200, jm{"state": "success", "total_count": 0, "statuses": []jm{}})
	if c, err := p.Checks(ctx, 5, head40); err != nil || c.Status != store.CIStatusPending || c.URL != "https://github.test/c/2" {
		t.Fatalf("pending checks = %+v %v", c, err)
	}

	// Diff: pull request files, a page of 100.
	f.on("GET /pulls/5/files", 200, []jm{
		{"filename": "a.go", "status": "modified", "patch": "@@ -1 +1 @@\n-a\n+b"},
		{"filename": "b.go", "status": "removed"},
		{"filename": "c.go", "previous_filename": "old.go", "status": "renamed", "patch": ""},
		{"filename": "d.bin", "status": "added"},
	})
	f.take()
	d, err := p.Diff(ctx, 5, 1<<20)
	if err != nil || d.More || len(d.Files) != 4 || d.Files[0] != (DiffFile{Path: "a.go", Status: "modified", Patch: "@@ -1 +1 @@\n-a\n+b"}) ||
		d.Files[1].Status != "deleted" || d.Files[2] != (DiffFile{Path: "c.go", OldPath: "old.go", Status: "renamed"}) || d.Files[3].Status != "added" {
		t.Fatalf("diff = %+v %v", d, err)
	}
	wantQuery(t, f.take()[0], map[string]string{"page": "1", "per_page": "100"})

	// JobLog: the API answers a redirect; the log's own address is read without the token.
	logs.on("GET /blob/77", 200, "step 1\nstep 2\ndone\n")
	f.redirect("GET /actions/jobs/77/logs", logs.srv.URL+"/blob/77?sig=abc")
	lg, err := p.JobLog(ctx, "77", 1<<20)
	if err != nil || string(lg.Text) != "step 1\nstep 2\ndone\n" || lg.Truncated {
		t.Fatalf("log = %+v %v", lg, err)
	}
	api, blob := f.take(), logs.take()
	if len(api) != 1 || !strings.HasPrefix(api[0].auth, "Bearer ") || len(blob) != 1 || blob[0].auth != "" || blob[0].query.Get("sig") != "abc" {
		t.Fatalf("log requests: api %+v, blob %+v", api, blob)
	}
	f.on("GET /actions/jobs/78/logs", 410, jm{"message": "Gone"})
	if _, err := p.JobLog(ctx, "78", 10); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("expired log = %v", err)
	}
	if _, err := p.JobLog(ctx, "status-3", 10); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("a commit status = %v", err)
	}

	// Merge sends the head; 409 is the head that moved.
	f.on("PUT /pulls/5/merge", 200, jm{"merged": true})
	f.take()
	if err := p.Merge(ctx, 5, head40); err != nil {
		t.Fatal(err)
	}
	if body := f.take()[0].body; body["sha"] != head40 || body["merge_method"] != "merge" {
		t.Fatalf("merge body = %v", body)
	}
	f.on("PUT /pulls/5/merge", 409, jm{"message": "Head branch was modified. Review and try the merge again."})
	var moved *ErrHeadMoved
	if err := p.Merge(ctx, 5, head40); !errors.As(err, &moved) || !strings.HasPrefix(moved.Message, "Head branch was modified") {
		t.Fatalf("409 = %v", err)
	}

	// CIConfigured: newest first, until a commit has a check or a status.
	f.on("GET /commits", 200, []jm{{"sha": "c1"}, {"sha": "c2"}, {"sha": "c3"}})
	f.on("GET /commits/c1/check-runs", 200, jm{"total_count": 0, "check_runs": []jm{}})
	f.on("GET /commits/c1/status", 200, jm{"total_count": 0, "statuses": []jm{}})
	f.on("GET /commits/c2/check-runs", 200, jm{"total_count": 0, "check_runs": []jm{}})
	f.on("GET /commits/c2/status", 200, jm{"total_count": 1, "statuses": []jm{{"state": "success"}}})
	f.take()
	if ok, err := p.CIConfigured(ctx, "main"); err != nil || !ok {
		t.Fatalf("configured = %v %v", ok, err)
	}
	reqs := f.take()
	wantQuery(t, reqs[0], map[string]string{"sha": "main", "per_page": "20"})
	if len(reqs) != 5 {
		t.Fatalf("configured calls = %v", routes(reqs))
	}
	f.on("GET /commits", 200, []jm{{"sha": "c1"}})
	if ok, err := p.CIConfigured(ctx, "main"); err != nil || ok {
		t.Fatalf("no CI = %v %v", ok, err)
	}
}
