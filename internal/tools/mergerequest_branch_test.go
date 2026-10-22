package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"yuno/internal/project"
	"yuno/internal/project/projecttest"
)

// branchFixture is open_merge_request on one provider's fake over a local bare repository,
// with the waits between the looks for the pushed branch recorded instead of slept.
type branchFixture struct {
	bare     string
	tool     *Tool
	call     Call
	waits    *noSleep
	lag      func(branches, creates int)
	requests func() int
}

func newBranchFixture(t *testing.T, provider project.Provider) *branchFixture {
	t.Helper()
	f := &branchFixture{bare: projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n"}), waits: &noSleep{}}
	pc := project.Config{RepoURL: f.bare, Token: "glpat-not-a-real-token-00", DefaultBranch: "main", Provider: provider}
	var apiBase, repoPath string
	switch provider {
	case project.ProviderGitHub:
		g, srv := newFakeGitHub(t, f.bare)
		apiBase, repoPath = srv.URL+"/repos/owner/demo", "owner/demo"
		f.lag = func(b, c int) { g.mu.Lock(); g.branchLag, g.prLag = b, c; g.mu.Unlock() }
		f.requests = func() int { g.mu.Lock(); defer g.mu.Unlock(); return len(g.pulls) }
	default:
		g, srv := newFakeGitLab(t)
		gitlabOnBare(t, g, f.bare)
		apiBase = srv.URL + "/api/v4/projects/group%2Fdemo"
		f.lag = func(b, c int) { g.mu.Lock(); g.branchLag, g.mrLag = b, c; g.mu.Unlock() }
		f.requests = func() int { g.mu.Lock(); defer g.mu.Unlock(); return len(g.mrs) }
	}
	f.tool = NewOpenMergeRequest(MergeRequestConfig{Projects: staticProjects{pc: pc}, APIBase: apiBase, RepoPath: repoPath, Sleep: f.waits.sleep})
	ws, checkout := checkoutFor(t, pc, "run-p6")
	must(t, os.WriteFile(filepath.Join(checkout, "src.js"), []byte("console.log(1)\n"), 0o644))
	f.call = Call{RunID: "run-p6", Workspace: ws, IdempotencyKey: strings.Repeat("abcdef012345", 5) + "6789",
		Input: json.RawMessage(`{"run_token":"r","title":"Add src","description":"d","draft":true}`)}
	return f
}

// commits is how many commits the call's branch has on top of main.
func (f *branchFixture) commits(t *testing.T) string {
	t.Helper()
	return projecttest.Git(t, f.bare, "rev-list", "--count", "main.."+GitLabBranch(f.call.IdempotencyKey))
}

// P6: a push the provider's API does not show at once (GitLab answers a create right after
// the push with HTTP 400 "source_branch does not exist" until it has refreshed its branch
// list) is looked up again before the request is opened, and a create refused for a missing
// source branch is tried again after the next wait: one request, one push.
func TestOpenMergeRequestWaitsForThePushedBranch(t *testing.T) {
	for _, provider := range []project.Provider{project.ProviderGitLab, project.ProviderGitHub} {
		t.Run(string(provider), func(t *testing.T) {
			f := newBranchFixture(t, provider)
			// The look before the push and the first one after it miss the branch; the first
			// create is refused for it.
			f.lag(2, 1)
			res, err := f.tool.Invoke(context.Background(), f.call)
			if err != nil || res.IsError {
				t.Fatalf("call: %+v %v", res, err)
			}
			var out mergeRequestOutput
			must(t, json.Unmarshal(res.Output, &out))
			if out.Number != 1 || out.Branch != GitLabBranch(f.call.IdempotencyKey) || f.requests() != 1 || f.commits(t) != "1" {
				t.Fatalf("output %s, requests %d, commits %s", res.Output, f.requests(), f.commits(t))
			}
			if want := []time.Duration{time.Second, 2 * time.Second}; !slices.Equal(f.waits.waits, want) {
				t.Fatalf("waits = %v, want %v", f.waits.waits, want)
			}
		})
	}
}

// P6: a pushed branch the provider never shows opens nothing and is a model error that
// names the branch; the same call again, once the provider shows it, opens the request
// without pushing a second time.
func TestOpenMergeRequestBranchNeverShown(t *testing.T) {
	for _, provider := range []project.Provider{project.ProviderGitLab, project.ProviderGitHub} {
		t.Run(string(provider), func(t *testing.T) {
			f := newBranchFixture(t, provider)
			f.lag(100, 0)
			res, err := f.tool.Invoke(context.Background(), f.call)
			if err != nil || !res.IsError {
				t.Fatalf("call: %+v %v", res, err)
			}
			name := map[project.Provider]string{project.ProviderGitLab: "GitLab", project.ProviderGitHub: "GitHub"}[provider]
			for _, want := range []string{"the branch " + GitLabBranch(f.call.IdempotencyKey) + " was pushed", name + " does not show it", "Call open_merge_request again with the same input"} {
				if !strings.Contains(res.Text, want) {
					t.Fatalf("text lacks %q: %s", want, res.Text)
				}
			}
			if strings.Contains(res.Text, "glpat-") || f.requests() != 0 || f.commits(t) != "1" {
				t.Fatalf("requests %d, commits %s, text %s", f.requests(), f.commits(t), res.Text)
			}
			if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}; !slices.Equal(f.waits.waits, want) {
				t.Fatalf("waits = %v, want %v", f.waits.waits, want)
			}

			f.lag(0, 0)
			res, err = f.tool.Invoke(context.Background(), f.call)
			if err != nil || res.IsError {
				t.Fatalf("again: %+v %v", res, err)
			}
			if f.requests() != 1 || f.commits(t) != "1" {
				t.Fatalf("again: requests %d, commits %s", f.requests(), f.commits(t))
			}
		})
	}
}

// missingSourceBranch knows the providers' refusals for a source branch they do not know,
// and nothing else.
func TestMissingSourceBranch(t *testing.T) {
	for _, tc := range []struct {
		status int
		plain  string
		want   bool
	}{
		{400, `gitlab: HTTP 400: {"message":{"source_branch":["does not exist"]}}`, true},
		{422, `gitlab: HTTP 422: {"message":["Source branch \"yuno/x\" does not exist"]}`, true},
		{422, `github: HTTP 422: {"message":"Validation Failed","errors":[{"resource":"PullRequest","field":"head","code":"invalid"}]}`, true},
		{422, `github: HTTP 422: {"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"custom","message":"A pull request already exists for owner:yuno/x."}]}`, false},
		{409, `gitlab: HTTP 409: {"message":["Another open merge request already exists for this source branch: !3"]}`, false},
		{500, `gitlab: HTTP 500: {"message":{"source_branch":["does not exist"]}}`, false},
	} {
		if got := missingSourceBranch(&errAPIStatus{status: tc.status, plain: tc.plain}); got != tc.want {
			t.Errorf("%d %s: %v, want %v", tc.status, tc.plain, got, tc.want)
		}
	}
	if missingSourceBranch(nil) || missingSourceBranch(context.Canceled) {
		t.Error("an error that is not a provider answer")
	}
}
