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
)

// fakeGitLab implements the slice of the GitLab v4 API the tool uses, and counts the
// writes.
type fakeGitLab struct {
	mu       sync.Mutex
	branches map[string]bool
	files    map[string]bool // on the default branch
	mrs      []map[string]any
	commits  []map[string]any
	token    string
	failMR   int // answer the next MR creations with 500
	// branchesFn, when set, answers branch lookups from a repository the test pushes
	// to with git (open_merge_request) instead of the map.
	branchesFn func() []string
	// refuse answers every request 401 (a wrong token); noPush reports the branches as
	// not pushable (the project check, DESIGN 5.17 rule 5).
	refuse bool
	noPush bool
}

func (g *fakeGitLab) hasBranch(name string) bool {
	if g.branchesFn != nil {
		return slices.Contains(g.branchesFn(), name)
	}
	return g.branches[name]
}

func newFakeGitLab(t *testing.T) (*fakeGitLab, *httptest.Server) {
	t.Helper()
	g := &fakeGitLab{branches: map[string]bool{"main": true}, files: map[string]bool{"README.md": true}}
	const project = "/api/v4/projects/group%2Fdemo"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.token = r.Header.Get("PRIVATE-TOKEN")
		if g.refuse {
			writeJSONTest(w, 401, map[string]string{"message": "401 Unauthorized"})
			return
		}
		p := r.URL.EscapedPath()
		if !strings.HasPrefix(p, project) {
			http.NotFound(w, r)
			return
		}
		p = strings.TrimPrefix(p, project)
		switch {
		case r.Method == http.MethodGet && p == "":
			// A project access token reports its role under project_access (Developer
			// is 30); noPush makes it a Reporter.
			level := 30
			if g.noPush {
				level = 20
			}
			writeJSONTest(w, 200, map[string]any{"id": 7, "default_branch": "main", "path_with_namespace": "group/demo",
				"permissions": map[string]any{"project_access": map[string]any{"access_level": level}, "group_access": nil}})
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/repository/branches/"):
			name := unescape(strings.TrimPrefix(p, "/repository/branches/"))
			if !g.hasBranch(name) {
				writeJSONTest(w, 404, map[string]string{"message": "404 Branch Not Found"})
				return
			}
			writeJSONTest(w, 200, map[string]any{"name": name, "can_push": !g.noPush, "protected": g.noPush})
		case r.Method == http.MethodHead && strings.HasPrefix(p, "/repository/files/"):
			if !g.files[unescape(strings.TrimPrefix(p, "/repository/files/"))] || r.URL.Query().Get("ref") != "main" {
				w.WriteHeader(404)
				return
			}
			w.WriteHeader(200)
		case r.Method == http.MethodPost && p == "/repository/commits":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			g.commits = append(g.commits, body)
			g.branches[body["branch"].(string)] = true
			writeJSONTest(w, 201, map[string]any{"id": "abc123"})
		case r.Method == http.MethodGet && p == "/merge_requests":
			var out []map[string]any
			for _, mr := range g.mrs {
				if mr["source_branch"] == r.URL.Query().Get("source_branch") && r.URL.Query().Get("state") == "opened" {
					out = append(out, mr)
				}
			}
			if out == nil {
				out = []map[string]any{}
			}
			writeJSONTest(w, 200, out)
		case r.Method == http.MethodPost && p == "/merge_requests":
			if g.failMR > 0 {
				g.failMR--
				writeJSONTest(w, 500, map[string]string{"message": "boom"})
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			iid := len(g.mrs) + 1
			body["iid"] = iid
			body["web_url"] = fmt.Sprintf("https://gitlab.test/group/demo/-/merge_requests/%d", iid)
			g.mrs = append(g.mrs, body)
			writeJSONTest(w, 201, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return g, srv
}

func writeJSONTest(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func unescape(s string) string {
	return strings.NewReplacer("%2F", "/", "%2f", "/").Replace(s)
}

// T5.7.5 (gitlab_create_mr) against a fake GitLab: one commit with every file on the
// key's branch, one MR; the same key again finds both and replays.
func TestGitLabCreateMR(t *testing.T) {
	g, srv := newFakeGitLab(t)
	ws := t.TempDir()
	runDir := filepath.Join(ws, "runs", "run-9")
	must(t, os.MkdirAll(filepath.Join(runDir, "src"), 0o755))
	must(t, os.WriteFile(filepath.Join(runDir, "src", "app.py"), []byte("print(1)"), 0o644))
	must(t, os.WriteFile(filepath.Join(runDir, "README.md"), []byte("# demo"), 0o644))
	tool := NewGitLabCreateMR(GitLabConfig{BaseURL: srv.URL + "/", ProjectID: "group/demo", Token: "glpat-test-token-000000", MaxBytes: 1 << 20})
	if tool.Risk != RiskSideEffecting || tool.Hits != HitsExternal {
		t.Fatalf("gitlab_create_mr = %s/%s", tool.Risk, tool.Hits)
	}
	key := strings.Repeat("0123456789ab", 5) + "cdef"
	call := Call{RunID: "run-9", Workspace: ws, IdempotencyKey: key,
		Input: json.RawMessage(`{"run_token":"r","title":"Add app","description":"From run 9","paths":["src","README.md"]}`)}

	res, err := tool.Invoke(context.Background(), call)
	if err != nil || res.IsError {
		t.Fatalf("first call: %+v %v", res, err)
	}
	var out gitlabOutput
	must(t, json.Unmarshal(res.Output, &out))
	if out.Branch != "yuno/0123456789ab" || out.IID != 1 || out.URL == "" || out.Replayed {
		t.Fatalf("output = %s", res.Output)
	}
	if len(g.commits) != 1 || len(g.mrs) != 1 || g.token != "glpat-test-token-000000" {
		t.Fatalf("commits %d, mrs %d, token %q", len(g.commits), len(g.mrs), g.token)
	}
	actions := map[string]string{}
	for _, a := range g.commits[0]["actions"].([]any) {
		m := a.(map[string]any)
		actions[m["file_path"].(string)] = m["action"].(string)
	}
	if actions["src/app.py"] != "create" || actions["README.md"] != "update" || g.commits[0]["start_branch"] != "main" {
		t.Fatalf("commit = %v", g.commits[0])
	}
	if g.mrs[0]["target_branch"] != "main" || g.mrs[0]["source_branch"] != out.Branch {
		t.Fatalf("mr = %v", g.mrs[0])
	}

	again, err := tool.Invoke(context.Background(), call)
	var out2 gitlabOutput
	if err != nil || again.IsError || json.Unmarshal(again.Output, &out2) != nil {
		t.Fatalf("second call: %+v %v", again, err)
	}
	if !out2.Replayed || out2.IID != 1 || len(g.commits) != 1 || len(g.mrs) != 1 {
		t.Fatalf("second call made writes: %s, commits %d, mrs %d", again.Output, len(g.commits), len(g.mrs))
	}
}

// A failure after the commit (the MR call fails) leaves the branch; the retry under
// the same key skips the commit and opens the MR.
func TestGitLabRetryAfterPartialFailure(t *testing.T) {
	g, srv := newFakeGitLab(t)
	g.failMR = 1
	ws := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(ws, "runs", "r"), 0o755))
	must(t, os.WriteFile(filepath.Join(ws, "runs", "r", "a.txt"), []byte("a"), 0o644))
	tool := NewGitLabCreateMR(GitLabConfig{BaseURL: srv.URL + "/api/v4", ProjectID: "group/demo", Token: "t", MaxBytes: 1 << 20})
	call := Call{RunID: "r", Workspace: ws, IdempotencyKey: strings.Repeat("f", 64),
		Input: json.RawMessage(`{"run_token":"r","title":"T","description":"","paths":["a.txt"]}`)}
	res, _ := tool.Invoke(context.Background(), call)
	if !res.IsError || !res.Transient {
		t.Fatalf("MR failure should be a transient error: %+v", res)
	}
	res, err := tool.Invoke(context.Background(), call)
	if err != nil || res.IsError || len(g.commits) != 1 || len(g.mrs) != 1 {
		t.Fatalf("retry: %+v %v, commits %d, mrs %d", res, err, len(g.commits), len(g.mrs))
	}
}

func TestGitLabRegisteredOnlyWhenConfigured(t *testing.T) {
	if _, ok := NewPlatformRegistry(PlatformDeps{}).Get(GitLabCreateMRName); ok {
		t.Fatal("registered without GitLab settings")
	}
	r := NewPlatformRegistry(PlatformDeps{GitLab: &GitLabConfig{BaseURL: "https://gitlab.test", ProjectID: "1", Token: "t"}})
	if _, ok := r.Get(GitLabCreateMRName); !ok {
		t.Fatal("not registered with GitLab settings")
	}
	for _, name := range []string{FinishName, DeployReleaseName, HTTPFetchName, MemorySaveName, MemoryRecallName} {
		if _, ok := r.Get(name); !ok {
			t.Errorf("%s not registered", name)
		}
	}
}
