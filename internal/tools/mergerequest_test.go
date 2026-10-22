package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"yuno/internal/obs"
	"yuno/internal/project"
	"yuno/internal/project/projecttest"
)

// fakeGitHub implements the slice of the GitHub REST API the tool uses, and counts the
// writes. Branches are reported from the bare repository the test pushes to.
type fakeGitHub struct {
	mu     sync.Mutex
	bare   string
	pulls  []map[string]any
	auth   string
	failPR int
	// branchLag answers the next lookups of a branch other than main 404, and prLag the
	// next pull request creations 422 with the head invalid, as the GitLab fake's lags.
	branchLag int
	prLag     int
	branches  func() []string
	// refuse answers every request 401; noPush reports permissions.push false (the
	// project check, DESIGN 5.17 rule 5).
	refuse bool
	noPush bool
	// scripted, calls and bodies as the GitLab fake's (DESIGN 5.20): routes are
	// "METHOD /path" below the repository, and "POST /graphql" for GraphQL.
	scripted map[string]scriptedAnswer
	calls    []string
	bodies   map[string]map[string]any
}

func newFakeGitHub(t *testing.T, bare string) (*fakeGitHub, *httptest.Server) {
	t.Helper()
	g := &fakeGitHub{bare: bare}
	g.branches = func() []string { return projecttest.Branches(t, bare) }
	const repo = "/repos/owner/demo"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.auth = r.Header.Get("Authorization")
		if g.refuse {
			writeJSONTest(w, 401, map[string]string{"message": "Bad credentials"})
			return
		}
		if r.URL.EscapedPath() == "/graphql" {
			if !scriptedRoute(w, r, r.Method+" /graphql", g.scripted, &g.calls, &g.bodies) {
				writeJSONTest(w, 404, map[string]string{"message": "Not Found"})
			}
			return
		}
		if r.URL.EscapedPath() != repo && !strings.HasPrefix(r.URL.EscapedPath(), repo+"/") {
			writeJSONTest(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		p := strings.TrimPrefix(r.URL.EscapedPath(), repo)
		if scriptedRoute(w, r, r.Method+" "+p, g.scripted, &g.calls, &g.bodies) {
			return
		}
		switch {
		case r.Method == http.MethodGet && p == "":
			writeJSONTest(w, 200, map[string]any{"name": "demo", "default_branch": "main",
				"permissions": map[string]bool{"admin": false, "push": !g.noPush, "pull": true}})
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/branches/"):
			name := unescape(strings.TrimPrefix(p, "/branches/"))
			lagging := g.branchLag > 0 && name != "main"
			if lagging {
				g.branchLag--
			}
			if lagging || !slices.Contains(g.branches(), name) {
				writeJSONTest(w, 404, map[string]string{"message": "Branch not found"})
				return
			}
			writeJSONTest(w, 200, map[string]any{"name": name})
		case r.Method == http.MethodGet && p == "/pulls":
			var out []map[string]any
			for _, pr := range g.pulls {
				if "owner:"+pr["head"].(string) == r.URL.Query().Get("head") && r.URL.Query().Get("state") == "open" {
					out = append(out, pr)
				}
			}
			if out == nil {
				out = []map[string]any{}
			}
			writeJSONTest(w, 200, out)
		case r.Method == http.MethodPost && p == "/pulls":
			if g.failPR > 0 {
				g.failPR--
				writeJSONTest(w, 502, map[string]string{"message": "bad gateway"})
				return
			}
			if g.prLag > 0 {
				g.prLag--
				writeJSONTest(w, 422, map[string]any{"message": "Validation Failed",
					"errors": []map[string]string{{"resource": "PullRequest", "field": "head", "code": "invalid"}}})
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			n := len(g.pulls) + 1
			body["number"] = n
			body["html_url"] = fmt.Sprintf("https://github.test/owner/demo/pull/%d", n)
			g.pulls = append(g.pulls, body)
			writeJSONTest(w, 201, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return g, srv
}

// staticProjects is a ProjectResolver that answers every run with one project, or
// with an error.
type staticProjects struct {
	pc  project.Config
	err error
	// tokenErr, when set, is what opening a project's token answers (read_merge_request in
	// a conversation).
	tokenErr error
}

func (s staticProjects) ForRunID(context.Context, string) (*project.Config, error) {
	if s.err != nil {
		return nil, s.err
	}
	pc := s.pc
	return &pc, nil
}

func (s staticProjects) Token(context.Context, string) (string, error) {
	if s.tokenErr != nil {
		return "", s.tokenErr
	}
	return s.pc.Token, nil
}

// forProject is a tool configured for one project (tests).
func forProject(pc project.Config, apiBase, repoPath string) *Tool {
	return NewOpenMergeRequest(MergeRequestConfig{Projects: staticProjects{pc: pc}, APIBase: apiBase, RepoPath: repoPath})
}

// gitlabOnBare makes the fake GitLab report the branches of a bare repository, since
// the tool pushes with git rather than through the commits API.
func gitlabOnBare(t *testing.T, g *fakeGitLab, bare string) {
	t.Helper()
	g.mu.Lock()
	g.branchesFn = func() []string { return projecttest.Branches(t, bare) }
	g.mu.Unlock()
}

// checkoutFor clones the bare repository where the tool expects a run's checkout and
// returns the workspace and the checkout.
func checkoutFor(t *testing.T, pc project.Config, runID string) (ws, checkout string) {
	t.Helper()
	ws = t.TempDir()
	must(t, os.MkdirAll(filepath.Join(ws, "runs", runID), 0o755))
	checkout = filepath.Join(ws, project.RepoRel(runID))
	must(t, pc.Clone(context.Background(), checkout))
	return ws, checkout
}

// T5.16.5 (GitLab): the checkout's changes reach the key's branch of the remote by
// git, the merge request is opened on the fake, a second call replays, and nothing
// that is stored carries the token.
func TestOpenMergeRequestGitLab(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n", "package.json": "{}", "package-lock.json": "{}"})
	g, srv := newFakeGitLab(t)
	gitlabOnBare(t, g, bare)
	pc := project.Config{RepoURL: bare, Token: "glpat-not-a-real-token-00", DefaultBranch: "main", Provider: project.ProviderGitLab}
	ws, checkout := checkoutFor(t, pc, "run-9")
	must(t, os.WriteFile(filepath.Join(checkout, "src.js"), []byte("console.log(1)\n"), 0o644))
	must(t, os.MkdirAll(filepath.Join(checkout, "node_modules", "x"), 0o755))
	must(t, os.WriteFile(filepath.Join(checkout, "node_modules", "x", "i.js"), []byte("x"), 0o644))

	tool := forProject(pc, srv.URL+"/api/v4/projects/group%2Fdemo", "")
	if tool.Risk != RiskSideEffecting || tool.Hits != HitsExternal || tool.Name != OpenMergeRequestName {
		t.Fatalf("tool = %s %s/%s", tool.Name, tool.Risk, tool.Hits)
	}
	if !strings.Contains(tool.Message(json.RawMessage(`{"title":"Add src"}`)), `Open merge request "Add src"`) {
		t.Fatalf("message = %q", tool.Message(json.RawMessage(`{"title":"Add src"}`)))
	}
	key := strings.Repeat("0123456789ab", 5) + "cdef"
	call := Call{RunID: "run-9", Workspace: ws, IdempotencyKey: key,
		Input: json.RawMessage(`{"run_token":"r","title":"Add src","description":"From run 9"}`)}

	res, err := tool.Invoke(context.Background(), call)
	if err != nil || res.IsError {
		t.Fatalf("first call: %+v %v", res, err)
	}
	var out mergeRequestOutput
	must(t, json.Unmarshal(res.Output, &out))
	if out.Branch != "yuno/0123456789ab" || out.Number != 1 || out.URL == "" || out.Provider != "gitlab" || out.Replayed {
		t.Fatalf("output = %s", res.Output)
	}
	if got := projecttest.FileAt(t, bare, out.Branch, "src.js"); got != "console.log(1)\n" {
		t.Fatalf("src.js on the branch = %q", got)
	}
	if got := projecttest.FileAt(t, bare, out.Branch, "node_modules/x/i.js"); got != "" {
		t.Fatal("node_modules was committed")
	}
	if projecttest.Subject(t, bare, out.Branch) != "Add src" {
		t.Fatal("the title is the commit message")
	}
	if len(g.commits) != 0 || len(g.mrs) != 1 || g.token != pc.Token {
		t.Fatalf("commits through the API %d (want none: git pushed), mrs %d, token %q", len(g.commits), len(g.mrs), g.token)
	}
	if g.mrs[0]["target_branch"] != "main" || g.mrs[0]["source_branch"] != out.Branch || g.mrs[0]["description"] != "From run 9" {
		t.Fatalf("mr = %v", g.mrs[0])
	}
	// T5.16.6: what is stored carries no token.
	_, stored := Render(res, 0)
	if strings.Contains(string(stored), pc.Token) || strings.Contains(string(obs.RedactJSON(call.Input)), pc.Token) {
		t.Fatal("the token is in a stored row")
	}

	// Same key again: the branch exists, no second push, the open request is found.
	must(t, os.WriteFile(filepath.Join(checkout, "src.js"), []byte("console.log(2)\n"), 0o644))
	again, err := tool.Invoke(context.Background(), call)
	var out2 mergeRequestOutput
	if err != nil || again.IsError || json.Unmarshal(again.Output, &out2) != nil {
		t.Fatalf("second call: %+v %v", again, err)
	}
	if !out2.Replayed || out2.Number != 1 || len(g.mrs) != 1 {
		t.Fatalf("second call: %s, mrs %d", again.Output, len(g.mrs))
	}
	if got := projecttest.FileAt(t, bare, out.Branch, "src.js"); got != "console.log(1)\n" {
		t.Fatal("a second push changed the branch")
	}
}

// A failure after the push (the request call fails) leaves the branch; the retry under
// the same key skips the push and opens the request.
func TestOpenMergeRequestRetryAfterPartialFailure(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"a.txt": "a"})
	g, srv := newFakeGitLab(t)
	gitlabOnBare(t, g, bare)
	g.failMR = 1
	pc := project.Config{RepoURL: bare, Token: "t", DefaultBranch: "main", Provider: project.ProviderGitLab}
	ws, checkout := checkoutFor(t, pc, "r")
	must(t, os.WriteFile(filepath.Join(checkout, "a.txt"), []byte("b"), 0o644))
	tool := forProject(pc, srv.URL+"/api/v4/projects/group%2Fdemo", "")
	call := Call{RunID: "r", Workspace: ws, IdempotencyKey: strings.Repeat("f", 64),
		Input: json.RawMessage(`{"run_token":"r","title":"T","description":""}`)}
	res, _ := tool.Invoke(context.Background(), call)
	if !res.IsError || !res.Transient {
		t.Fatalf("request failure should be a transient error: %+v", res)
	}
	if !slices.Contains(projecttest.Branches(t, bare), "yuno/ffffffffffff") {
		t.Fatal("the branch was not pushed before the failure")
	}
	res, err := tool.Invoke(context.Background(), call)
	if err != nil || res.IsError || len(g.mrs) != 1 {
		t.Fatalf("retry: %+v %v, mrs %d", res, err, len(g.mrs))
	}
}

// Problems the model can fix come back as error results: no checkout, a link, no
// changes.
func TestOpenMergeRequestModelErrors(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"a.txt": "a"})
	g, srv := newFakeGitLab(t)
	gitlabOnBare(t, g, bare)
	pc := project.Config{RepoURL: bare, Token: "t", DefaultBranch: "main", Provider: project.ProviderGitLab}
	tool := forProject(pc, srv.URL+"/api/v4/projects/group%2Fdemo", "")
	input := json.RawMessage(`{"run_token":"r","title":"T","description":""}`)

	ws := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(ws, "runs", "r1"), 0o755))
	res, err := tool.Invoke(context.Background(), Call{RunID: "r1", Workspace: ws, IdempotencyKey: strings.Repeat("1", 64), Input: input})
	if err != nil || !res.IsError || !strings.Contains(res.Text, "no project checkout at runs/r1/repo/") {
		t.Fatalf("no checkout: %+v %v", res, err)
	}

	ws2, _ := checkoutFor(t, pc, "r2")
	res, err = tool.Invoke(context.Background(), Call{RunID: "r2", Workspace: ws2, IdempotencyKey: strings.Repeat("2", 64), Input: input})
	if err != nil || !res.IsError || !strings.Contains(res.Text, "no changes to propose") {
		t.Fatalf("no changes: %+v %v", res, err)
	}
	if len(g.mrs) != 0 || len(projecttest.Branches(t, bare)) != 1 {
		t.Fatal("a request or branch for no changes")
	}

	ws3 := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(ws3, "runs", "r3"), 0o755))
	must(t, os.Symlink(filepath.Join(ws2, "runs", "r2", "repo"), filepath.Join(ws3, "runs", "r3", "repo")))
	res, err = tool.Invoke(context.Background(), Call{RunID: "r3", Workspace: ws3, IdempotencyKey: strings.Repeat("3", 64), Input: input})
	if err != nil || !res.IsError || !strings.Contains(res.Text, "is a link") {
		t.Fatalf("linked checkout: %+v %v", res, err)
	}

	if _, err := tool.Invoke(context.Background(), Call{RunID: "r2", Workspace: ws2, Input: input}); err == nil {
		t.Fatal("no idempotency key must be an internal error")
	}

	// DESIGN 5.17 rule 6: a run without a project is told so; a project whose token
	// does not open is an error result naming the Projects page, never a secret.
	none := NewOpenMergeRequest(MergeRequestConfig{Projects: staticProjects{err: project.ErrNoProject}})
	res, err = none.Invoke(context.Background(), Call{RunID: "r2", Workspace: ws2, IdempotencyKey: strings.Repeat("4", 64), Input: input})
	if err != nil || !res.IsError || res.Text != "this run has no project" {
		t.Fatalf("no project: %+v %v", res, err)
	}
	unreadable := NewOpenMergeRequest(MergeRequestConfig{Projects: staticProjects{err: fmt.Errorf("project: %w", project.ErrCannotRead)}})
	res, err = unreadable.Invoke(context.Background(), Call{RunID: "r2", Workspace: ws2, IdempotencyKey: strings.Repeat("5", 64), Input: input})
	if err != nil || !res.IsError || !strings.Contains(res.Text, "enter it again on the Projects page") || res.Transient {
		t.Fatalf("unreadable token: %+v %v", res, err)
	}
	unconfigured := NewOpenMergeRequest(MergeRequestConfig{})
	if res, err := unconfigured.Invoke(context.Background(), Call{RunID: "r2", Workspace: ws2, IdempotencyKey: strings.Repeat("6", 64), Input: input}); err != nil || !res.IsError || res.Text != "this run has no project" {
		t.Fatalf("no resolver: %+v %v", res, err)
	}
	// DESIGN 5.17 rule 11: the title is a commit subject, capped at 72 characters.
	NewRegistry().Register(tool)
	long := `{"run_token":"r","title":"` + strings.Repeat("x", 73) + `","description":""}`
	if err := tool.ValidateInput(json.RawMessage(long)); err == nil || !strings.Contains(err.Error(), "title") {
		t.Fatalf("a 73-character title passed: %v", err)
	}
	if err := tool.ValidateInput(json.RawMessage(`{"run_token":"r","title":"` + strings.Repeat("x", 72) + `","description":""}`)); err != nil {
		t.Fatalf("a 72-character title refused: %v", err)
	}
	if !strings.Contains(tool.Description, "at most 72 characters") || !strings.Contains(tool.Description, "never the task text") {
		t.Fatalf("description does not say how to title: %q", tool.Description)
	}
}

// T5.16.5 (GitHub): the same flow against the pull request API, head qualified by the
// owner.
func TestOpenMergeRequestGitHub(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n"})
	g, srv := newFakeGitHub(t, bare)
	pc := project.Config{RepoURL: "https://github.com/owner/demo.git", Token: "ghp_notARealTokenValue0123456789", DefaultBranch: "main", Provider: project.ProviderGitHub}
	// git talks to the bare repository, the API to the fake.
	gitCfg := pc
	gitCfg.RepoURL = bare
	ws, checkout := checkoutFor(t, gitCfg, "run-7")
	must(t, os.WriteFile(filepath.Join(checkout, "README.md"), []byte("# demo\nmore\n"), 0o644))
	tool := forProject(gitCfg, srv.URL+"/repos/owner/demo", "owner/demo")
	if !strings.Contains(tool.Description, "pull request (GitHub)") {
		t.Fatalf("description does not name both providers: %q", tool.Description)
	}
	call := Call{RunID: "run-7", Workspace: ws, IdempotencyKey: strings.Repeat("ab", 32),
		Input: json.RawMessage(`{"run_token":"r","title":"More readme","description":"body"}`)}
	res, err := tool.Invoke(context.Background(), call)
	if err != nil || res.IsError {
		t.Fatalf("first call: %+v %v", res, err)
	}
	var out mergeRequestOutput
	must(t, json.Unmarshal(res.Output, &out))
	if out.Provider != "github" || out.Number != 1 || out.Branch != "yuno/abababababab" || !strings.Contains(out.URL, "/pull/1") {
		t.Fatalf("output = %s", res.Output)
	}
	if g.auth != "Bearer "+pc.Token {
		t.Fatalf("auth = %q", g.auth)
	}
	if g.pulls[0]["head"] != out.Branch || g.pulls[0]["base"] != "main" || g.pulls[0]["body"] != "body" {
		t.Fatalf("pull = %v", g.pulls[0])
	}
	if got := projecttest.FileAt(t, bare, out.Branch, "README.md"); got != "# demo\nmore\n" {
		t.Fatalf("README on the branch = %q", got)
	}
	again, err := tool.Invoke(context.Background(), call)
	var out2 mergeRequestOutput
	if err != nil || again.IsError || json.Unmarshal(again.Output, &out2) != nil || !out2.Replayed || len(g.pulls) != 1 {
		t.Fatalf("second call: %+v %v, pulls %d", again, err, len(g.pulls))
	}
}

func TestOpenMergeRequestRegisteredAlways(t *testing.T) {
	// T5.17.9: the project comes from the run, so the tool exists without any project.
	tool, ok := NewPlatformRegistry(PlatformDeps{}).Get(OpenMergeRequestName)
	if !ok {
		t.Fatal("open_merge_request not registered without a project")
	}
	if tool.Risk != RiskSideEffecting {
		t.Fatalf("risk = %s", tool.Risk)
	}
}

// M3 of the wave 1 review: with paths only those are committed; bad paths are refused to
// the model; the paths are part of the input the idempotency key hashes.
func TestOpenMergeRequestPaths(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n"})
	g, srv := newFakeGitLab(t)
	gitlabOnBare(t, g, bare)
	pc := project.Config{RepoURL: bare, Token: "glpat-not-a-real-token-00", DefaultBranch: "main", Provider: project.ProviderGitLab}
	ws, checkout := checkoutFor(t, pc, "run-p")
	must(t, os.MkdirAll(filepath.Join(checkout, "docs", "plans", "x"), 0o755))
	must(t, os.WriteFile(filepath.Join(checkout, "docs", "plans", "x", "PRD.md"), []byte("prd\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(checkout, "stray.txt"), []byte("stray\n"), 0o644))
	tool := forProject(pc, srv.URL+"/api/v4/projects/group%2Fdemo", "")
	NewRegistry().Register(tool)

	for _, bad := range []string{`["/etc/passwd"]`, `["../x"]`, `["."]`, `[".git/config"]`, `["docs/../.git"]`, `[]`, `["a b"]`} {
		in := json.RawMessage(`{"run_token":"r","title":"T","description":"","paths":` + bad + `}`)
		res, err := tool.Invoke(context.Background(), Call{RunID: "run-p", Workspace: ws, IdempotencyKey: strings.Repeat("a", 64), Input: in})
		if err == nil && !res.IsError {
			t.Errorf("paths %s were accepted", bad)
		}
	}
	if len(g.mrs) != 0 || len(projecttest.Branches(t, bare)) != 1 {
		t.Fatal("a refused call opened a request or pushed")
	}
	res, err := tool.Invoke(context.Background(), Call{RunID: "run-p", Workspace: ws, IdempotencyKey: strings.Repeat("b", 64),
		Input: json.RawMessage(`{"run_token":"r","title":"Plan","description":"d","paths":["docs/plans/x/","docs/plans/x/PRD.md"]}`)})
	if err != nil || res.IsError {
		t.Fatalf("with paths: %+v %v", res, err)
	}
	var out mergeRequestOutput
	must(t, json.Unmarshal(res.Output, &out))
	if projecttest.FileAt(t, bare, out.Branch, "docs/plans/x/PRD.md") != "prd\n" || projecttest.FileAt(t, bare, out.Branch, "stray.txt") != "" {
		t.Fatal("the branch does not hold exactly the named paths")
	}
	a, err := IdempotencyKey("r", "v", OpenMergeRequestName, json.RawMessage(`{"title":"T","paths":["a"]}`))
	must(t, err)
	b, err := IdempotencyKey("r", "v", OpenMergeRequestName, json.RawMessage(`{"title":"T","paths":["b"]}`))
	must(t, err)
	if a == b {
		t.Fatal("paths are not part of the idempotency key")
	}
	if !strings.Contains(tool.Description, "paths") {
		t.Fatal("the description does not mention paths")
	}
}
