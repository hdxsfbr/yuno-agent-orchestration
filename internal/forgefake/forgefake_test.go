package forgefake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"yuno/internal/project"
	"yuno/internal/project/projecttest"
	"yuno/internal/tools"
)

const testToken = "forge-test-token"

// forge starts a fake forge with one repository, acme/app, and returns it with the
// project configuration the platform would hold for it.
func forge(t *testing.T) (*httptest.Server, project.Config) {
	t.Helper()
	projecttest.NeedGit(t)
	s, err := New(testToken, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	pc := project.Config{RepoURL: ts.URL + "/acme/app.git", Token: testToken, DefaultBranch: "main", Provider: project.ProviderGitLab}
	call(t, ts, http.MethodPost, "/_forge/repos", map[string]any{"path": "acme/app", "default_branch": "main"}, nil, http.StatusCreated)
	return ts, pc
}

// call sends a request with the token and decodes the JSON answer into out.
func call(t *testing.T, ts *httptest.Server, method, path string, in, out any, want int) {
	t.Helper()
	var body io.Reader
	if in != nil {
		raw, _ := json.Marshal(in)
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, ts.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("PRIVATE-TOKEN", testToken)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw bytes.Buffer
	_, _ = raw.ReadFrom(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s: %d, want %d: %s", method, path, resp.StatusCode, want, raw.String())
	}
	if out != nil {
		if err := json.Unmarshal(raw.Bytes(), out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, raw.String())
		}
	}
}

func TestAuthentication(t *testing.T) {
	ts, pc := forge(t)
	get := func(path string, set func(*http.Request)) int {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if set != nil {
			set(req)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	api := "/api/v4/projects/" + url.PathEscape("acme/app")
	for name, c := range map[string]struct {
		set  func(*http.Request)
		want int
	}{
		"no token":      {nil, http.StatusUnauthorized},
		"wrong token":   {func(r *http.Request) { r.Header.Set("PRIVATE-TOKEN", "nope") }, http.StatusUnauthorized},
		"wrong bearer":  {func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }, http.StatusUnauthorized},
		"private token": {func(r *http.Request) { r.Header.Set("PRIVATE-TOKEN", testToken) }, http.StatusOK},
		"bearer":        {func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testToken) }, http.StatusOK},
	} {
		if got := get(api, c.set); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
	if got := get("/_forge/repos/acme/app/issues", nil); got != http.StatusUnauthorized {
		t.Errorf("control without a token: %d", got)
	}
	if got := get("/healthz", nil); got != http.StatusOK {
		t.Errorf("healthz: %d", got)
	}
	if got := get("/acme/app.git/info/refs?service=git-upload-pack", nil); got != http.StatusUnauthorized {
		t.Errorf("git without credentials: %d", got)
	}
	bad := pc
	bad.Token = "nope"
	if err := bad.Clone(context.Background(), filepath.Join(t.TempDir(), "c")); err == nil {
		t.Error("a clone with a wrong token worked")
	}
	if _, err := New("", ""); err == nil {
		t.Error("a forge without a token was made")
	}
}

func TestIssuesThroughTheProviders(t *testing.T) {
	ts, pc := forge(t)
	ctx := context.Background()
	is := tools.IssueProviderFor(&pc, nil)

	a, err := is.Create(ctx, "First", "body one", []string{"yuno", "wave:1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := is.Create(ctx, "Second", "body two", []string{"yuno"}); err != nil {
		t.Fatal(err)
	}
	if _, err := is.Create(ctx, "Third", "unlabelled", nil); err != nil {
		t.Fatal(err)
	}
	if a.Number != 1 || !strings.HasSuffix(a.URL, "/acme/app/-/issues/1") {
		t.Fatalf("created issue %+v", a)
	}

	got, more, err := is.ListLabelled(ctx, "yuno", 1, 1)
	if err != nil || len(got) != 1 || !more || got[0].Title != "First" {
		t.Fatalf("page 1: %+v more=%v err=%v", got, more, err)
	}
	got, more, err = is.ListLabelled(ctx, "yuno", 2, 1)
	if err != nil || len(got) != 1 || !more || got[0].Title != "Second" {
		t.Fatalf("page 2: %+v more=%v err=%v", got, more, err)
	}
	got, more, err = is.ListLabelled(ctx, "yuno", 3, 1)
	if err != nil || len(got) != 0 || more {
		t.Fatalf("page 3: %+v more=%v err=%v", got, more, err)
	}
	if got, _, _ := is.ListLabelled(ctx, "wave:1", 1, 100); len(got) != 1 {
		t.Fatalf("wave:1: %+v", got)
	}

	// The pagination headers a client may rely on.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v4/projects/acme%2Fapp/issues?per_page=2&page=1&state=all", nil)
	req.Header.Set("PRIVATE-TOKEN", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if h := resp.Header; h.Get("X-Total") != "3" || h.Get("X-Total-Pages") != "2" || h.Get("X-Next-Page") != "2" || h.Get("X-Per-Page") != "2" {
		t.Fatalf("headers %v", h)
	}

	if err := is.SwapLabels(ctx, a.Number, "yuno:working", "yuno"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := is.ListLabelled(ctx, "yuno", 1, 100); len(got) != 1 || got[0].Title != "Second" {
		t.Fatalf("after the swap, yuno: %+v", got)
	}
	if got, _, _ := is.ListLabelled(ctx, "yuno:working", 1, 100); len(got) != 1 || got[0].Number != a.Number {
		t.Fatalf("after the swap, yuno:working: %+v", got)
	}
	if err := is.Comment(ctx, a.Number, "on it"); err != nil {
		t.Fatal(err)
	}
	if err := is.Close(ctx, a.Number); err != nil {
		t.Fatal(err)
	}
	if err := is.Close(ctx, a.Number); err != nil {
		t.Fatalf("closing a closed issue: %v", err)
	}
	if got, _, _ := is.ListLabelled(ctx, "yuno:working", 1, 100); len(got) != 0 {
		t.Fatalf("a closed issue is listed as open: %+v", got)
	}
	all, err := is.ListAll(ctx, "yuno:working", 10)
	if err != nil || len(all) != 1 || all[0].Number != a.Number {
		t.Fatalf("ListAll: %+v %v", all, err)
	}

	var seen []struct {
		IID    int      `json:"iid"`
		State  string   `json:"state"`
		Labels []string `json:"labels"`
	}
	call(t, ts, http.MethodGet, "/_forge/repos/acme/app/issues", nil, &seen, http.StatusOK)
	if len(seen) != 3 || seen[0].State != "closed" || strings.Join(seen[0].Labels, ",") != "wave:1,yuno:working" || seen[1].State != "opened" {
		t.Fatalf("control issues: %+v", seen)
	}
	var notes []map[string]any
	call(t, ts, http.MethodGet, "/api/v4/projects/acme%2Fapp/issues/1/notes", nil, &notes, http.StatusOK)
	if len(notes) != 1 || notes[0]["body"] != "on it" {
		t.Fatalf("notes: %v", notes)
	}

	// A label that exists is a 409 for the create call and is fine for EnsureLabels.
	call(t, ts, http.MethodPost, "/api/v4/projects/acme%2Fapp/labels", map[string]any{"name": "yuno"}, nil, http.StatusConflict)
	if err := is.EnsureLabels(ctx, []tools.IssueLabel{{Name: "yuno", Color: "#ff0000"}, {Name: "fresh", Color: "#00ff00"}}); err != nil {
		t.Fatal(err)
	}
	call(t, ts, http.MethodGet, "/api/v4/projects/acme%2Fapp/labels/fresh", nil, nil, http.StatusOK)
	call(t, ts, http.MethodGet, "/api/v4/projects/acme%2Fapp/issues/99", nil, nil, http.StatusNotFound)
}

// checkout clones the project, writes the files and pushes them to branch, the way a run
// publishes its work.
func push(t *testing.T, pc project.Config, branch string, files map[string]string) {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "co")
	if err := pc.Clone(ctx, dir); err != nil {
		t.Fatalf("clone: %v", err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pc.Propose(ctx, dir, branch, "work on "+branch); err != nil {
		t.Fatalf("push %s: %v", branch, err)
	}
}

func TestMergeRequestsThroughTheProviders(t *testing.T) {
	ts, pc := forge(t)
	ctx := context.Background()
	rp := tools.ProviderFor(&pc, nil)

	if ok, err := rp.BranchExists(ctx, "feature/one"); err != nil || ok {
		t.Fatalf("branch before the push: %v %v", ok, err)
	}
	push(t, pc, "feature/one", map[string]string{"one.txt": "one\n"})
	if ok, err := rp.BranchExists(ctx, "feature/one"); err != nil || !ok {
		t.Fatalf("branch after the push: %v %v", ok, err)
	}
	if ref, err := rp.FindOpen(ctx, "feature/one"); err != nil || ref != nil {
		t.Fatalf("FindOpen before: %v %v", ref, err)
	}
	ref, err := rp.Create(ctx, "feature/one", "main", "Add one", "the first", true)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Number != 1 || !strings.HasSuffix(ref.URL, "/acme/app/-/merge_requests/1") {
		t.Fatalf("ref %+v", ref)
	}
	if again, err := rp.FindOpen(ctx, "feature/one"); err != nil || again == nil || again.Number != 1 {
		t.Fatalf("FindOpen: %v %v", again, err)
	}
	if _, err := rp.Create(ctx, "feature/one", "main", "Again", "", false); err == nil {
		t.Fatal("a second open request for the branch was made")
	}
	d, err := rp.Details(ctx, 1)
	if err != nil || d.Title != "Draft: Add one" || d.SourceBranch != "feature/one" || d.TargetBranch != "main" || d.HeadSHA == "" {
		t.Fatalf("details %+v %v", d, err)
	}

	// A draft does not merge.
	var nm *tools.ErrNotMergeable
	if err := rp.Merge(ctx, 1, ""); !errors.As(err, &nm) {
		t.Fatalf("merging a draft: %v", err)
	}

	title, desc, ready := "Add one, done", "the first, finished", false
	if err := rp.Update(ctx, 1, tools.RequestUpdate{Title: &title, Description: &desc, Draft: &ready}); err != nil {
		t.Fatal(err)
	}
	if d, _ := rp.Details(ctx, 1); d.Title != "Add one, done" || d.Description != desc {
		t.Fatalf("after the update %+v", d)
	}
	if err := rp.Update(ctx, 7, tools.RequestUpdate{Title: &title}); !errors.Is(err, tools.ErrRequestNotFound) {
		t.Fatalf("updating a missing request: %v", err)
	}
	st, err := rp.Status(ctx, 1)
	if err != nil || st.State != "open" || st.CIStatus != "none" || st.HeadSHA != d.HeadSHA || st.UnresolvedThreads == nil || *st.UnresolvedThreads != 0 {
		t.Fatalf("status %+v %v", st, err)
	}

	// The request's pipelines are those of its source branch.
	call(t, ts, http.MethodPost, "/_forge/repos/acme/app/pipeline", map[string]any{"state": "passed", "ref": "feature/one"}, nil, http.StatusCreated)
	if ci, err := rp.CIStatus(ctx, 1, d.HeadSHA); err != nil || ci != "passed" {
		t.Fatalf("ci %q %v", ci, err)
	}

	if err := rp.Merge(ctx, 1, ""); err != nil {
		t.Fatal(err)
	}
	st, err = rp.Status(ctx, 1)
	if err != nil || st.State != "merged" || st.ClosedAt == nil || st.HeadSHA != d.HeadSHA {
		t.Fatalf("status after the merge %+v %v", st, err)
	}
	if ok, _ := rp.BranchExists(ctx, "feature/one"); ok {
		t.Error("the source branch survived a merge that removes it")
	}
	if err := rp.Merge(ctx, 1, ""); !errors.As(err, &nm) {
		t.Fatalf("merging a merged request: %v", err)
	}
	if err := rp.Merge(ctx, 9, ""); !errors.Is(err, tools.ErrRequestNotFound) {
		t.Fatalf("merging a missing request: %v", err)
	}

	// The default branch has the file: a fresh clone shows it.
	fresh := filepath.Join(t.TempDir(), "fresh")
	if err := pc.Clone(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(fresh, "one.txt")); err != nil || string(raw) != "one\n" {
		t.Fatalf("one.txt after the merge: %q %v", raw, err)
	}
}

func TestDefaultBranchPipelines(t *testing.T) {
	ts, pc := forge(t)
	ctx := context.Background()
	rp := tools.ProviderFor(&pc, nil)

	if st, err := rp.DefaultBranchStatus(ctx); err != nil || st.State != tools.BranchNone {
		t.Fatalf("no pipeline yet: %+v %v", st, err)
	}
	for _, c := range []struct{ state, want string }{
		{"passed", tools.BranchPassed}, {"failed", tools.BranchFailed}, {"running", tools.BranchRunning}, {"pending", tools.BranchPending},
	} {
		call(t, ts, http.MethodPost, "/_forge/repos/acme/app/pipeline", map[string]any{"state": c.state}, nil, http.StatusCreated)
		st, err := rp.DefaultBranchStatus(ctx)
		if err != nil || st.State != c.want || st.SHA == "" || st.At.IsZero() || st.URL == "" {
			t.Fatalf("%s: %+v %v", c.state, st, err)
		}
	}
	call(t, ts, http.MethodPost, "/_forge/repos/acme/app/pipeline", map[string]any{"state": "weird"}, nil, http.StatusBadRequest)
	call(t, ts, http.MethodPost, "/_forge/repos/acme/app/pipeline", map[string]any{"state": "passed", "ref": "nope"}, nil, http.StatusNotFound)
}

func TestMergeOpen(t *testing.T) {
	ts, pc := forge(t)
	ctx := context.Background()
	rp := tools.ProviderFor(&pc, nil)

	// Three requests off the same base: a draft, and two that touch different files (the
	// second needs a merge commit once the first is in).
	push(t, pc, "feature/a", map[string]string{"a.txt": "a\n"})
	push(t, pc, "feature/b", map[string]string{"b.txt": "b\n"})
	push(t, pc, "feature/c", map[string]string{"c.txt": "c\n"})
	for _, c := range []struct {
		branch string
		draft  bool
	}{{"feature/a", false}, {"feature/b", false}, {"feature/c", true}} {
		if _, err := rp.Create(ctx, c.branch, "main", "Add "+c.branch, "", c.draft); err != nil {
			t.Fatal(err)
		}
	}

	var merged []struct {
		IID      int     `json:"iid"`
		State    string  `json:"state"`
		MergedAt *string `json:"merged_at"`
	}
	call(t, ts, http.MethodPost, "/_forge/repos/acme/app/merge-open", nil, &merged, http.StatusOK)
	if len(merged) != 2 || merged[0].IID != 1 || merged[1].IID != 2 || merged[0].State != "merged" || merged[1].MergedAt == nil {
		t.Fatalf("merged %+v", merged)
	}
	if st, _ := rp.Status(ctx, 3); st.State != "open" {
		t.Fatalf("the draft %+v", st)
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	if err := pc.Clone(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(fresh, f)); err != nil {
			t.Fatalf("%s after merge-open: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(fresh, "c.txt")); err == nil {
		t.Fatal("the draft was merged")
	}
	// The clone is shallow, so the tip's message is what shows the merge commit.
	out, err := exec.Command("git", "-C", fresh, "log", "-1", "--format=%s").Output()
	if err != nil || !strings.Contains(string(out), "Merge branch 'feature/b' into 'main'") {
		t.Fatalf("no merge commit for the second request: %q %v", out, err)
	}

	// By iid: a draft is refused, a ready one merges, one that is gone is 404.
	call(t, ts, http.MethodPost, "/_forge/repos/acme/app/merge-open", map[string]any{"iid": 3}, nil, http.StatusConflict)
	ready := false
	if err := rp.Update(ctx, 3, tools.RequestUpdate{Draft: &ready}); err != nil {
		t.Fatal(err)
	}
	call(t, ts, http.MethodPost, "/_forge/repos/acme/app/merge-open", map[string]any{"iid": 3}, &merged, http.StatusOK)
	if len(merged) != 1 || merged[0].IID != 3 {
		t.Fatalf("by iid %+v", merged)
	}
	call(t, ts, http.MethodPost, "/_forge/repos/acme/app/merge-open", map[string]any{"iid": 3}, nil, http.StatusNotFound)
	call(t, ts, http.MethodPost, "/_forge/repos/acme/none/merge-open", nil, nil, http.StatusNotFound)
}

func TestMergeConflict(t *testing.T) {
	ts, pc := forge(t)
	ctx := context.Background()
	rp := tools.ProviderFor(&pc, nil)
	push(t, pc, "feature/x", map[string]string{"shared.txt": "x\n"})
	push(t, pc, "feature/y", map[string]string{"shared.txt": "y\n"})
	for _, b := range []string{"feature/x", "feature/y"} {
		if _, err := rp.Create(ctx, b, "main", b, "", false); err != nil {
			t.Fatal(err)
		}
	}
	var nm *tools.ErrNotMergeable
	if err := rp.Merge(ctx, 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := rp.Merge(ctx, 2, ""); !errors.As(err, &nm) {
		t.Fatalf("a conflicting merge: %v", err)
	}
	if st, _ := rp.Status(ctx, 2); st.State != "open" {
		t.Fatalf("the conflicting request %+v", st)
	}
	call(t, ts, http.MethodPost, "/_forge/repos/acme/app/merge-open", nil, nil, http.StatusConflict)
}

func TestControlCreate(t *testing.T) {
	ts, _ := forge(t)
	call(t, ts, http.MethodPost, "/_forge/repos", map[string]any{"path": "acme/app"}, nil, http.StatusConflict)
	call(t, ts, http.MethodPost, "/_forge/repos", map[string]any{"path": "../escape"}, nil, http.StatusBadRequest)
	call(t, ts, http.MethodPost, "/_forge/repos", map[string]any{"path": "solo"}, nil, http.StatusBadRequest)
	var made struct {
		ID            int    `json:"id"`
		DefaultBranch string `json:"default_branch"`
		Head          string `json:"head"`
	}
	call(t, ts, http.MethodPost, "/_forge/repos", map[string]any{"path": "group/sub/lib", "default_branch": "trunk"}, &made, http.StatusCreated)
	if made.DefaultBranch != "trunk" || len(made.Head) != 40 {
		t.Fatalf("made %+v", made)
	}
	// Reachable by path and by numeric id; a nested path is one encoded segment.
	call(t, ts, http.MethodGet, "/api/v4/projects/group%2Fsub%2Flib/repository/branches/trunk", nil, nil, http.StatusOK)
	call(t, ts, http.MethodGet, "/api/v4/projects/"+itoa(made.ID)+"/repository/branches/nope", nil, nil, http.StatusNotFound)
	var branches []struct {
		Name string `json:"name"`
	}
	call(t, ts, http.MethodGet, "/api/v4/projects/group%2Fsub%2Flib/repository/branches", nil, &branches, http.StatusOK)
	if len(branches) != 1 || branches[0].Name != "trunk" {
		t.Fatalf("branches %+v", branches)
	}
	call(t, ts, http.MethodGet, "/api/v4/projects/nope%2Fnope", nil, nil, http.StatusNotFound)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
