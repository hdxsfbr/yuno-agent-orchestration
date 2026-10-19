package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"yuno/internal/project"
)

// T5.29.8: the default branch's status on both providers, over the issue fake's routes.

func branchGitLab(t *testing.T) (*issueAPI, RequestProvider) {
	t.Helper()
	f := newIssueAPI(t, "/api/v4/projects/group%2Fdemo")
	pc := &project.Config{RepoURL: "https://gitlab.test/group/demo.git", Token: issueTestToken, Provider: project.ProviderGitLab}
	return f, newRequestProvider(pc, ProviderClient(nil), f.base, pc.Path())
}

func branchGitHub(t *testing.T) (*issueAPI, RequestProvider) {
	t.Helper()
	f := newIssueAPI(t, "/repos/owner/demo")
	pc := &project.Config{RepoURL: "https://github.com/owner/demo.git", Token: issueTestToken, Provider: project.ProviderGitHub}
	return f, newRequestProvider(pc, ProviderClient(nil), f.base, pc.Path())
}

func TestGitLabDefaultBranchStatus(t *testing.T) {
	at := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		pipelines []jm
		want      string
	}{
		{"success", []jm{{"status": "success"}}, "passed"},
		{"failed", []jm{{"status": "failed"}}, "failed"},
		{"canceled", []jm{{"status": "canceled"}}, "failed"},
		{"running", []jm{{"status": "running"}}, "running"},
		{"pending", []jm{{"status": "pending"}}, "pending"},
		{"created", []jm{{"status": "created"}}, "pending"},
		{"manual", []jm{{"status": "manual"}}, "pending"},
		{"unknown is never passed", []jm{{"status": "something_new"}}, "pending"},
		{"skipped ran nothing", []jm{{"status": "skipped"}}, "none"},
		{"no pipeline", []jm{}, "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, p := branchGitLab(t)
			f.on("GET ", 200, jm{"default_branch": "trunk"})
			if len(tc.pipelines) > 0 {
				tc.pipelines[0]["sha"] = "abc123"
				tc.pipelines[0]["web_url"] = "https://gitlab.test/group/demo/-/pipelines/9"
				tc.pipelines[0]["created_at"] = at.Format(time.RFC3339Nano)
			}
			f.on("GET /pipelines", 200, tc.pipelines)
			st, err := p.DefaultBranchStatus(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if st.State != tc.want {
				t.Fatalf("state = %q, want %q", st.State, tc.want)
			}
			if len(tc.pipelines) > 0 && tc.want != "none" && (st.SHA != "abc123" || !st.At.Equal(at) || !strings.HasSuffix(st.URL, "/pipelines/9")) {
				t.Fatalf("status = %+v", st)
			}
			if tc.want == "none" && (st.SHA != "" || !st.At.IsZero() || st.URL != "") {
				t.Fatalf("none carries data: %+v", st)
			}
			q := f.call(t, 1).Query
			if q.Get("ref") != "trunk" || q.Get("order_by") != "id" || q.Get("sort") != "desc" || q.Get("per_page") != "1" {
				t.Fatalf("pipelines query = %v", q)
			}
		})
	}

	f, p := branchGitLab(t)
	f.on("GET ", 500, jm{"message": "boom"})
	_, err := p.DefaultBranchStatus(context.Background())
	if err == nil || !TransientError(err) || strings.Contains(err.Error(), issueTestToken) {
		t.Fatalf("500 = %v", err)
	}
	f.on("GET ", 200, jm{"default_branch": "main"})
	f.on("GET /pipelines", 401, jm{"message": "401 Unauthorized"})
	if _, err = p.DefaultBranchStatus(context.Background()); err == nil || TransientError(err) || strings.Contains(err.Error(), issueTestToken) {
		t.Fatalf("401 = %v", err)
	}
}

func TestGitHubDefaultBranchStatus(t *testing.T) {
	early := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	late := early.Add(10 * time.Minute)
	check := func(status, conclusion string, at time.Time, url string) jm {
		return jm{"status": status, "conclusion": conclusion, "completed_at": at.Format(time.RFC3339), "html_url": url}
	}
	cases := []struct {
		name     string
		checks   []jm
		state    string
		statuses []jm
		want     string
		at       time.Time
	}{
		{"all success", []jm{check("completed", "success", early, "u1"), check("completed", "success", late, "u2")}, "success", nil, "passed", late},
		{"a check failed", []jm{check("completed", "success", early, "u1"), check("completed", "failure", late, "u2")}, "success", nil, "failed", late},
		{"a check timed out", []jm{check("completed", "timed_out", early, "u1")}, "pending", nil, "failed", early},
		{"a check in progress", []jm{check("completed", "success", early, "u1"), check("in_progress", "", late, "u2")}, "success", nil, "running", late},
		{"a check queued", []jm{check("queued", "", early, "u1")}, "pending", nil, "running", early},
		{"combined failure", nil, "failure", []jm{{"state": "failure", "updated_at": late.Format(time.RFC3339), "target_url": "s1"}}, "failed", late},
		{"combined pending", []jm{check("completed", "success", early, "u1")}, "pending", []jm{{"state": "pending", "updated_at": late.Format(time.RFC3339), "target_url": "s1"}}, "running", late},
		{"statuses only, success", nil, "success", []jm{{"state": "success", "updated_at": early.Format(time.RFC3339), "target_url": "s1"}}, "passed", early},
		{"failure beats running", []jm{check("in_progress", "", early, "u1"), check("completed", "failure", early, "u2")}, "pending", nil, "failed", early},
		{"nothing at all", nil, "pending", nil, "none", time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, p := branchGitHub(t)
			f.on("GET ", 200, jm{"default_branch": "trunk"})
			f.on("GET /commits/trunk", 200, jm{"sha": "abc123"})
			f.on("GET /commits/abc123/check-runs", 200, jm{"check_runs": tc.checks})
			f.on("GET /commits/abc123/status", 200, jm{"state": tc.state, "total_count": len(tc.statuses), "statuses": tc.statuses})
			st, err := p.DefaultBranchStatus(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if st.State != tc.want {
				t.Fatalf("state = %q, want %q", st.State, tc.want)
			}
			if tc.want == "none" {
				if st.SHA != "" || !st.At.IsZero() || st.URL != "" {
					t.Fatalf("none carries data: %+v", st)
				}
				return
			}
			if st.SHA != "abc123" || !st.At.Equal(tc.at) {
				t.Fatalf("status = %+v, want sha abc123 at %v", st, tc.at)
			}
			if f.call(t, 0).Auth != "Bearer "+issueTestToken {
				t.Fatalf("auth = %q", f.call(t, 0).Auth)
			}
		})
	}

	f, p := branchGitHub(t)
	f.on("GET ", 200, jm{"default_branch": "main"})
	f.on("GET /commits/main", 404, jm{"message": "Not Found"})
	if _, err := p.DefaultBranchStatus(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 404") || strings.Contains(err.Error(), issueTestToken) {
		t.Fatalf("404 = %v", err)
	}
}
