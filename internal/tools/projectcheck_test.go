package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yuno/internal/project"
	"yuno/internal/project/projecttest"
)

const checkToken = "glpat-not-a-real-token-00"

// wantCheck compares a check result with the expected one; nil pointers mean the field
// is absent.
func wantCheck(t *testing.T, got CheckResult, ok bool, message, defaultBranch string, branchExists, canPush *bool) {
	t.Helper()
	same := func(a, b *bool) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	if got.OK != ok || got.Message != message || got.DefaultBranch != defaultBranch ||
		!same(got.BranchExists, branchExists) || !same(got.CanPush, canPush) {
		raw, _ := json.Marshal(got)
		t.Fatalf("check = %s\nwant ok %v, message %q, default branch %q, branch_exists %v, can_push %v",
			raw, ok, message, defaultBranch, deref(branchExists), deref(canPush))
	}
	if got.CheckedAt.IsZero() || time.Since(got.CheckedAt) > time.Minute {
		t.Fatalf("checked_at = %v", got.CheckedAt)
	}
	if strings.Contains(got.Message, checkToken) {
		t.Fatalf("the token is in the message: %q", got.Message)
	}
}

func deref(b *bool) any {
	if b == nil {
		return "absent"
	}
	return *b
}

// T5.17.3 against the GitLab fake: accepted and can push, no push rights, the branch
// missing, a refused token, a repository that is not there.
func TestCheckProjectGitLab(t *testing.T) {
	g, srv := newFakeGitLab(t)
	base := srv.URL + "/api/v4/projects/group%2Fdemo"
	pc := project.Config{RepoURL: "https://gitlab.test/group/demo.git", Token: checkToken, DefaultBranch: "main", Provider: project.ProviderGitLab}
	ctx := context.Background()

	wantCheck(t, CheckProject(ctx, srv.Client(), pc, base), true, "Token accepted: default branch main, push allowed.", "main", ptr(true), ptr(true))
	if g.token != checkToken {
		t.Fatalf("PRIVATE-TOKEN = %q", g.token)
	}

	g.mu.Lock()
	g.noPush = true
	g.mu.Unlock()
	wantCheck(t, CheckProject(ctx, srv.Client(), pc, base), true, "Token accepted, but it cannot push to the repository; it needs write access.", "main", ptr(true), ptr(false))
	g.mu.Lock()
	g.noPush = false
	g.mu.Unlock()

	// The access level comes with the project, so it is known for a missing branch too.
	develop := pc
	develop.DefaultBranch = "develop"
	wantCheck(t, CheckProject(ctx, srv.Client(), develop, base), true,
		"Token accepted, but branch develop does not exist; the repository's default branch is main.", "main", ptr(false), ptr(true))

	wantCheck(t, CheckProject(ctx, srv.Client(), pc, srv.URL+"/api/v4/projects/group%2Fother"), false,
		"Repository not found (404): check the URL and the token's scope.", "", nil, nil)

	g.mu.Lock()
	g.refuse = true
	g.mu.Unlock()
	wantCheck(t, CheckProject(ctx, srv.Client(), pc, base), false, "The token was refused (401).", "", nil, nil)
}

// T5.17.3 against the GitHub fake: permissions.push true and false, the branch
// missing, a repository that is not there, a refused token.
func TestCheckProjectGitHub(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n"})
	g, srv := newFakeGitHub(t, bare)
	base := srv.URL + "/repos/owner/demo"
	pc := project.Config{RepoURL: "https://github.com/owner/demo.git", Token: checkToken, DefaultBranch: "main", Provider: project.ProviderGitHub}
	ctx := context.Background()

	wantCheck(t, CheckProject(ctx, srv.Client(), pc, base), true, "Token accepted: default branch main, push allowed.", "main", ptr(true), ptr(true))
	if g.auth != "Bearer "+checkToken {
		t.Fatalf("Authorization = %q", g.auth)
	}

	g.mu.Lock()
	g.noPush = true
	g.mu.Unlock()
	wantCheck(t, CheckProject(ctx, srv.Client(), pc, base), true, "Token accepted, but it cannot push to the repository; it needs write access.", "main", ptr(true), ptr(false))

	develop := pc
	develop.DefaultBranch = "develop"
	wantCheck(t, CheckProject(ctx, srv.Client(), develop, base), true,
		"Token accepted, but branch develop does not exist; the repository's default branch is main.", "main", ptr(false), ptr(false))

	wantCheck(t, CheckProject(ctx, srv.Client(), pc, srv.URL+"/repos/owner/other"), false,
		"Repository not found (404): check the URL and the token's scope.", "", nil, nil)

	g.mu.Lock()
	g.refuse = true
	g.mu.Unlock()
	wantCheck(t, CheckProject(ctx, srv.Client(), pc, base), false, "The token was refused (401).", "", nil, nil)
}

// A redirect is never followed (DESIGN 5.17 rules 2 and 5): the token header would
// otherwise travel to the host the provider points at.
func TestCheckProjectNeverFollowsRedirects(t *testing.T) {
	pc := project.Config{RepoURL: "https://gitlab.test/group/demo.git", Token: checkToken, DefaultBranch: "main", Provider: project.ProviderGitLab}
	var sinkHits int
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sinkHits++
		if r.Header.Get("PRIVATE-TOKEN") != "" {
			t.Errorf("the token reached the redirect target")
		}
		writeJSONTest(w, 200, map[string]any{"default_branch": "main"})
	}))
	t.Cleanup(sink.Close)
	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(hop.Close)
	wantCheck(t, CheckProject(context.Background(), nil, pc, hop.URL), false, "GitLab answered 302.", "", nil, nil)
	if sinkHits != 0 {
		t.Fatalf("the redirect was followed %d times", sinkHits)
	}
}

// Provider errors are results, not errors, and carry no token: a 5xx whose body
// echoes it, a connection refused, an answer that is not JSON, the timeout.
func TestCheckProjectFailures(t *testing.T) {
	pc := project.Config{RepoURL: "https://gitlab.test/group/demo.git", Token: checkToken, DefaultBranch: "main", Provider: project.ProviderGitLab}
	ctx := context.Background()

	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream failed for token "+r.Header.Get("PRIVATE-TOKEN"), http.StatusBadGateway)
	}))
	t.Cleanup(echo.Close)
	wantCheck(t, CheckProject(ctx, echo.Client(), pc, echo.URL), false, "GitLab answered 502.", "", nil, nil)

	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	res := CheckProject(ctx, nil, pc, closedURL)
	wantCheck(t, res, false, res.Message, "", nil, nil)
	if !strings.HasPrefix(res.Message, "Could not reach GitLab: ") || !strings.HasSuffix(res.Message, ".") {
		t.Fatalf("message = %q", res.Message)
	}

	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>sign in</html>"))
	}))
	t.Cleanup(html.Close)
	wantCheck(t, CheckProject(ctx, html.Client(), pc, html.URL), false,
		"GitLab's answer is not JSON; check the repository URL and the provider.", "", nil, nil)

	// A provider that never answers: the check gives up with the caller's deadline as
	// it would at ProjectCheckTimeout.
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(hang.Close)
	t.Cleanup(func() { close(release) })
	short, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	start := time.Now()
	wantCheck(t, CheckProject(short, hang.Client(), pc, hang.URL), false, "GitLab did not answer within 10 s.", "", nil, nil)
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("the check waited %s", waited)
	}

	// The repository found, the branch lookup failing otherwise than 404: still ok.
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/repository/branches/") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		writeJSONTest(w, 200, map[string]any{"default_branch": "main"})
	}))
	t.Cleanup(flaky.Close)
	wantCheck(t, CheckProject(ctx, flaky.Client(), pc, flaky.URL), true,
		"Token accepted, but branch main could not be checked: GitLab answered 500.", "main", nil, nil)
}

// The base defaults to the project's APIBase: GitLab's encoded path on the host.
func TestCheckProjectDefaultBase(t *testing.T) {
	var got string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got == "" {
			got = r.URL.String()
		}
		return &http.Response{StatusCode: 401, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
	})
	pc := project.Config{RepoURL: "https://gitlab.example.com/group/sub/demo.git", Token: checkToken, DefaultBranch: "main", Provider: project.ProviderGitLab}
	res := CheckProject(context.Background(), &http.Client{Transport: rt}, pc, "")
	if got != "https://gitlab.example.com/api/v4/projects/group%2Fsub%2Fdemo" || res.OK || res.Message != "The token was refused (401)." {
		t.Fatalf("requested %q, result %+v", got, res)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
