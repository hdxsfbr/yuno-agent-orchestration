package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// fakeProvider is the slice of GitLab's API the project check reads (DESIGN 5.17 rule
// 5): any project path, its default branch main, and main pushable. Tokens starting
// with acceptedTokenPrefix are accepted, every other one is refused with 401. The
// transport sends every request to it, whatever host the project names.
type fakeProvider struct {
	mu     sync.Mutex
	tokens []string
	paths  []string
}

const acceptedTokenPrefix = "glpat-not-a-real-token-"

func newFakeProvider(t *testing.T) (*fakeProvider, http.RoundTripper) {
	t.Helper()
	f := &fakeProvider{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("PRIVATE-TOKEN")
		f.mu.Lock()
		f.tokens = append(f.tokens, tok)
		f.paths = append(f.paths, r.URL.EscapedPath())
		f.mu.Unlock()
		if !strings.HasPrefix(tok, acceptedTokenPrefix) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "401 Unauthorized"})
			return
		}
		rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/api/v4/projects/")
		if !ok || strings.Contains(rest, "/") && !strings.HasSuffix(rest, "/repository/branches/main") {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Not Found"})
			return
		}
		if strings.HasSuffix(rest, "/repository/branches/main") {
			writeJSON(w, http.StatusOK, map[string]any{"name": "main", "can_push": true})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": 7, "default_branch": "main"})
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	return f, rewriteTransport{target: target}
}

func (f *fakeProvider) seen() (tokens, paths []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.tokens), slices.Clone(f.paths)
}

// rewriteTransport sends a request to target instead of its own host.
type rewriteTransport struct{ target *url.URL }

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host, r2.Host = rt.target.Scheme, rt.target.Host, ""
	return http.DefaultTransport.RoundTrip(r2)
}

func decodeProject(t *testing.T, b []byte) store.Project {
	t.Helper()
	var p store.Project
	if err := json.Unmarshal(b, &p); err != nil || p.ID == "" {
		t.Fatalf("not a project: %s", b)
	}
	return p
}

// noToken fails when a response body carries the token anywhere (DESIGN 5.17 rule 2).
func noToken(t *testing.T, what string, body []byte, token string) {
	t.Helper()
	if bytes.Contains(body, []byte(token)) {
		t.Fatalf("%s carries the token: %s", what, body)
	}
}

func testCipher(t *testing.T) *project.Cipher {
	t.Helper()
	c, err := project.NewCipher(testProjectTokenKey)
	mustDo(t, err)
	return c
}

// seedProject stores a project directly, its token sealed with c (nil: no token).
func seedProject(t *testing.T, c *project.Cipher, name, token string) *store.Project {
	t.Helper()
	p := &store.Project{ID: store.NewID(), Name: name, RepoURL: "https://gitlab.test/group/" + name + ".git",
		Provider: "gitlab", DefaultBranch: "main"}
	var blob []byte
	if c != nil {
		var err error
		blob, err = c.Seal(p.ID, token)
		mustDo(t, err)
	}
	mustDo(t, store.CreateProject(context.Background(), testPool, p, blob))
	t.Cleanup(func() { _ = store.DeleteProject(context.Background(), testPool, p.ID) })
	return p
}

// projectRun queues a run that names the project, with its snapshot (DESIGN 5.17 rule 4).
func projectRun(t *testing.T, p *store.Project) *store.Run {
	t.Helper()
	ctx := context.Background()
	w := &store.Workflow{Name: "project run", Definition: json.RawMessage(`{"nodes":[{"id":"agent","type":"agent"}],"edges":[],"project":true}`)}
	mustDo(t, store.CreateWorkflow(ctx, testPool, w))
	settings := p.Settings()
	run := &store.Run{WorkflowID: w.ID, WorkflowVersion: 1, WorkflowSnapshot: w.Definition, Trigger: store.TriggerManual,
		ProjectID: &p.ID, Project: &settings}
	mustDo(t, store.CreateRun(ctx, testPool, run))
	return run
}

// T5.17.2: create, get, list, PUT back without a token, PUT a new token, last_run,
// DELETE then GET 404. No response body carries a token.
func TestProjectCRUD(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	sfx := uniqueSuffix()
	name := "Demo " + sfx
	token := acceptedTokenPrefix + sfx

	code, body := do(t, ts, http.MethodPost, "/api/projects", `{"name":"  `+name+`  ","repo_url":" https://gitlab.test/group/demo-`+sfx+`.git ",
		"provider":"","token":"`+token+`","description":"The demo service"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	noToken(t, "create", body, token)
	p := decodeProject(t, body)
	if p.Name != name || p.RepoURL != "https://gitlab.test/group/demo-"+sfx+".git" || p.Provider != "gitlab" ||
		p.DefaultBranch != "main" || p.SandboxNetwork || p.Description != "The demo service" || !p.TokenSet ||
		p.CreatedAt.IsZero() || p.LastRun != nil {
		t.Fatalf("created = %+v", p)
	}
	t.Cleanup(func() { _ = store.DeleteProject(ctx, pool, p.ID) })
	blob, err := store.GetProjectTokenEnc(ctx, pool, p.ID)
	mustDo(t, err)
	if got, err := testCipher(t).Open(p.ID, blob); err != nil || got != token {
		t.Fatalf("stored token opens to %q, %v", got, err)
	}

	code, body = do(t, ts, http.MethodGet, "/api/projects/"+p.ID, "")
	if code != http.StatusOK || decodeProject(t, body).Name != name {
		t.Fatalf("get: %d %s", code, body)
	}
	noToken(t, "get", body, token)
	if strings.Contains(string(body), `"token"`) || !strings.Contains(string(body), `"token_set":true`) {
		t.Fatalf("get: the representation is not token_set only: %s", body)
	}
	code, body = do(t, ts, http.MethodGet, "/api/projects", "")
	var list []store.Project
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil || !slices.ContainsFunc(list, func(x store.Project) bool { return x.ID == p.ID }) {
		t.Fatalf("list: %d %s", code, body)
	}
	noToken(t, "list", body, token)

	// PUT back what was fetched, read-only fields included, with no token: the stored
	// blob is untouched.
	var back map[string]any
	_, body = do(t, ts, http.MethodGet, "/api/projects/"+p.ID, "")
	mustDo(t, json.Unmarshal(body, &back))
	back["description"] = "Edited"
	back["default_branch"] = "develop"
	back["sandbox_network"] = true
	raw, _ := json.Marshal(back)
	code, body = do(t, ts, http.MethodPut, "/api/projects/"+p.ID, string(raw))
	if code != http.StatusOK {
		t.Fatalf("put: %d %s", code, body)
	}
	got := decodeProject(t, body)
	if got.Description != "Edited" || got.Provider != "gitlab" || got.DefaultBranch != "develop" || !got.SandboxNetwork || !got.TokenSet {
		t.Fatalf("put = %+v", got)
	}
	after, err := store.GetProjectTokenEnc(ctx, pool, p.ID)
	mustDo(t, err)
	if !bytes.Equal(after, blob) {
		t.Fatal("a PUT without a token changed the stored token")
	}
	// An empty token keeps it too.
	code, body = do(t, ts, http.MethodPut, "/api/projects/"+p.ID, `{"name":"`+name+`","repo_url":"https://gitlab.test/group/demo-`+sfx+`.git","token":""}`)
	if code != http.StatusOK || !decodeProject(t, body).TokenSet {
		t.Fatalf("put with an empty token: %d %s", code, body)
	}
	after, _ = store.GetProjectTokenEnc(ctx, pool, p.ID)
	if !bytes.Equal(after, blob) {
		t.Fatal("a PUT with an empty token changed the stored token")
	}

	// A new token replaces the stored one and is not echoed.
	fresh := acceptedTokenPrefix + "rotated-" + sfx
	code, body = do(t, ts, http.MethodPut, "/api/projects/"+p.ID, `{"name":"`+name+`","repo_url":"https://gitlab.test/group/demo-`+sfx+`.git","token":"`+fresh+`"}`)
	if code != http.StatusOK || !decodeProject(t, body).TokenSet {
		t.Fatalf("put a new token: %d %s", code, body)
	}
	noToken(t, "put a new token", body, fresh)
	after, _ = store.GetProjectTokenEnc(ctx, pool, p.ID)
	if got, err := testCipher(t).Open(p.ID, after); err != nil || got != fresh {
		t.Fatalf("rotated token opens to %q, %v", got, err)
	}
	for _, path := range []string{"/api/projects/" + p.ID, "/api/projects"} {
		_, body = do(t, ts, http.MethodGet, path, "")
		noToken(t, "GET "+path, body, fresh)
		noToken(t, "GET "+path, body, token)
	}

	// last_run is the newest run that names the project.
	cur, err := store.GetProject(ctx, pool, p.ID)
	mustDo(t, err)
	run := projectRun(t, cur)
	code, body = do(t, ts, http.MethodGet, "/api/projects/"+p.ID, "")
	if lr := decodeProject(t, body).LastRun; code != http.StatusOK || lr == nil || lr.ID != run.ID || lr.Status != store.RunQueued {
		t.Fatalf("last_run: %d %s", code, body)
	}

	if code, body := do(t, ts, http.MethodDelete, "/api/projects/"+p.ID, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, body)
	}
	if code, _ := do(t, ts, http.MethodGet, "/api/projects/"+p.ID, ""); code != http.StatusNotFound {
		t.Fatalf("get deleted: %d", code)
	}
	_, body = do(t, ts, http.MethodGet, "/api/projects", "")
	list = nil
	if json.Unmarshal(body, &list) != nil || slices.ContainsFunc(list, func(x store.Project) bool { return x.ID == p.ID }) {
		t.Fatalf("the deleted project is still listed: %s", body)
	}
	for _, st := range []struct{ method, path, body string }{
		{http.MethodDelete, "/api/projects/" + p.ID, ""},
		{http.MethodPut, "/api/projects/" + p.ID, `{"name":"x","repo_url":"https://gitlab.test/g/x.git"}`},
		{http.MethodPost, "/api/projects/" + p.ID + "/check", ""},
	} {
		if code, body := do(t, ts, st.method, st.path, st.body); code != http.StatusNotFound || decodeError(t, body).Error.Code != "not_found" {
			t.Fatalf("%s %s on a deleted project: %d %s", st.method, st.path, code, body)
		}
	}
	// The run keeps its snapshot and the row its token (DESIGN 5.17 rule 3).
	if got, err := store.GetRun(ctx, pool, run.ID); err != nil || got.Project == nil || got.Project.Name != name {
		t.Fatalf("run after delete = %+v, %v", got, err)
	}
	if b, err := store.GetProjectTokenEnc(ctx, pool, p.ID); err != nil || len(b) == 0 {
		t.Fatalf("token after delete: %d bytes, %v", len(b), err)
	}
}

// T5.17.2: the 422 details of DESIGN 5.17 rule 3, path-qualified, in request order.
func TestProjectValidation(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	sfx := uniqueSuffix()
	tok := `"token":"` + acceptedTokenPrefix + sfx + `"`
	repo := `"repo_url":"https://gitlab.test/group/v-` + sfx + `.git"`
	tests := []struct {
		name, body string
		wantPaths  []string
		wantMsg    string
	}{
		{name: "missing name", body: `{` + repo + `,` + tok + `}`, wantPaths: []string{"name"}, wantMsg: "is required"},
		{name: "blank name", body: `{"name":"   ",` + repo + `,` + tok + `}`, wantPaths: []string{"name"}},
		{name: "long name", body: `{"name":"` + strings.Repeat("n", 65) + `",` + repo + `,` + tok + `}`, wantPaths: []string{"name"}},
		{name: "missing URL", body: `{"name":"v-` + sfx + `",` + tok + `}`, wantPaths: []string{"repo_url"}, wantMsg: "is required"},
		{name: "http URL", body: `{"name":"v-` + sfx + `","repo_url":"http://gitlab.test/group/demo.git",` + tok + `}`, wantPaths: []string{"repo_url"}, wantMsg: "must be an https URL"},
		{name: "URL with credentials", body: `{"name":"v-` + sfx + `","repo_url":"https://oauth2:glpat-in-the-url-not-real@gitlab.test/group/demo.git",` + tok + `}`,
			wantPaths: []string{"repo_url"}, wantMsg: "must not carry credentials"},
		{name: "URL that does not parse, with credentials", body: `{"name":"v-` + sfx + `","repo_url":"https://oauth2:glpat-in-the-url-not-real@git lab.test/x",` + tok + `}`,
			wantPaths: []string{"repo_url"}, wantMsg: "is not a valid URL"},
		{name: "URL without a repository", body: `{"name":"v-` + sfx + `","repo_url":"https://gitlab.test/",` + tok + `}`, wantPaths: []string{"repo_url"}},
		{name: "unknown provider", body: `{"name":"v-` + sfx + `",` + repo + `,"provider":"bitbucket",` + tok + `}`, wantPaths: []string{"provider"}},
		{name: "bad branch", body: `{"name":"v-` + sfx + `",` + repo + `,"default_branch":"feature branch",` + tok + `}`, wantPaths: []string{"default_branch"}},
		{name: "branch like an option", body: `{"name":"v-` + sfx + `",` + repo + `,"default_branch":"--force",` + tok + `}`, wantPaths: []string{"default_branch"}},
		{name: "long description", body: `{"name":"v-` + sfx + `",` + repo + `,"description":"` + strings.Repeat("d", 1025) + `",` + tok + `}`, wantPaths: []string{"description"}},
		{name: "missing token on create", body: `{"name":"v-` + sfx + `",` + repo + `}`, wantPaths: []string{"token"}, wantMsg: "is required"},
		{name: "token with a line break", body: `{"name":"v-` + sfx + `",` + repo + `,"token":"glpat-not-a-real\ntoken-00"}`, wantPaths: []string{"token"}},
		{name: "unknown field", body: `{"name":"v-` + sfx + `",` + repo + `,` + tok + `,"owner":"me"}`, wantPaths: []string{"owner"}},
		{name: "every field at once", body: `{"name":"","repo_url":"http://x/y","provider":"svn","default_branch":"a b"}`,
			wantPaths: []string{"name", "repo_url", "provider", "default_branch", "token"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, body := do(t, ts, http.MethodPost, "/api/projects", tc.body)
			if code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d: %s", code, body)
			}
			if got := detailPaths(t, body); !slices.Equal(got, tc.wantPaths) {
				t.Fatalf("paths = %q, want %q: %s", got, tc.wantPaths, body)
			}
			if tc.wantMsg != "" && !strings.Contains(string(body), tc.wantMsg) {
				t.Fatalf("message %q missing: %s", tc.wantMsg, body)
			}
			noToken(t, tc.name, body, "glpat-in-the-url-not-real")
		})
	}

	// A name another live project holds, ignoring case and surrounding spaces; a
	// deleted project's name is free again.
	code, body := do(t, ts, http.MethodPost, "/api/projects", `{"name":"Dup-`+sfx+`",`+repo+`,`+tok+`}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	first := decodeProject(t, body)
	t.Cleanup(func() { _ = store.DeleteProject(context.Background(), pool, first.ID) })
	code, body = do(t, ts, http.MethodPost, "/api/projects", `{"name":"  dup-`+sfx+` ",`+repo+`,`+tok+`}`)
	if code != http.StatusUnprocessableEntity || !slices.Equal(detailPaths(t, body), []string{"name"}) ||
		!strings.Contains(string(body), `a project named \"dup-`+sfx+`\" already exists`) {
		t.Fatalf("duplicate: %d %s", code, body)
	}
	code, body = do(t, ts, http.MethodPost, "/api/projects", `{"name":"Other-`+sfx+`",`+repo+`,`+tok+`}`)
	if code != http.StatusCreated {
		t.Fatalf("create other: %d %s", code, body)
	}
	other := decodeProject(t, body)
	t.Cleanup(func() { _ = store.DeleteProject(context.Background(), pool, other.ID) })
	code, body = do(t, ts, http.MethodPut, "/api/projects/"+other.ID, `{"name":"DUP-`+sfx+`",`+repo+`}`)
	if code != http.StatusUnprocessableEntity || !slices.Equal(detailPaths(t, body), []string{"name"}) {
		t.Fatalf("rename onto another project's name: %d %s", code, body)
	}
	// Its own name in another case is not a clash.
	code, body = do(t, ts, http.MethodPut, "/api/projects/"+first.ID, `{"name":"DUP-`+sfx+`",`+repo+`}`)
	if code != http.StatusOK || decodeProject(t, body).Name != "DUP-"+sfx {
		t.Fatalf("rename in another case: %d %s", code, body)
	}
	// An update validates like a create, except for the token.
	code, body = do(t, ts, http.MethodPut, "/api/projects/"+first.ID, `{"name":"","repo_url":"http://x/y"}`)
	if code != http.StatusUnprocessableEntity || !slices.Equal(detailPaths(t, body), []string{"name", "repo_url"}) {
		t.Fatalf("invalid update: %d %s", code, body)
	}
	mustDo(t, store.DeleteProject(context.Background(), pool, first.ID))
	code, body = do(t, ts, http.MethodPost, "/api/projects", `{"name":"dup-`+sfx+`",`+repo+`,`+tok+`}`)
	if code != http.StatusCreated {
		t.Fatalf("a deleted project's name: %d %s", code, body)
	}
	again := decodeProject(t, body)
	t.Cleanup(func() { _ = store.DeleteProject(context.Background(), pool, again.ID) })
}

// Without PROJECT_TOKEN_KEY no token can be stored (503) and the check says the
// stored one cannot be read; an update that carries no token still saves.
func TestProjectTokensUnavailable(t *testing.T) {
	pool := needDB(t)
	cfg := testConfig()
	cfg.ProjectTokenKey = ""
	_, ts := newTestServerDeps(t, Deps{Cfg: cfg, Pool: pool}, nil)
	router := contract(t)
	sfx := uniqueSuffix()
	repo := `"repo_url":"https://gitlab.test/group/nokey-` + sfx + `.git"`

	code, body := checkConformance(t, router, ts.URL, http.MethodPost, "/api/projects",
		`{"name":"nokey-`+sfx+`",`+repo+`,"token":"`+acceptedTokenPrefix+sfx+`"}`, true)
	if e := decodeError(t, body); code != http.StatusServiceUnavailable || e.Error.Code != "project_tokens_unavailable" ||
		e.Error.Message != "PROJECT_TOKEN_KEY is not set; no token can be stored" {
		t.Fatalf("create without a key: %d %s", code, body)
	}
	if _, err := store.GetProjectByName(context.Background(), pool, "nokey-"+sfx); err == nil {
		t.Fatal("the project was stored without a key")
	}

	p := seedProject(t, testCipher(t), "nokey-"+sfx, acceptedTokenPrefix+sfx)
	code, body = checkConformance(t, router, ts.URL, http.MethodPut, "/api/projects/"+p.ID,
		`{"name":"nokey-`+sfx+`",`+repo+`,"token":"`+acceptedTokenPrefix+"new-"+sfx+`"}`, true)
	if code != http.StatusServiceUnavailable || decodeError(t, body).Error.Code != "project_tokens_unavailable" {
		t.Fatalf("update with a token without a key: %d %s", code, body)
	}
	code, body = checkConformance(t, router, ts.URL, http.MethodPut, "/api/projects/"+p.ID,
		`{"name":"nokey-`+sfx+`",`+repo+`,"description":"still editable"}`, true)
	if code != http.StatusOK || decodeProject(t, body).Description != "still editable" {
		t.Fatalf("update without a token without a key: %d %s", code, body)
	}
	code, body = checkConformance(t, router, ts.URL, http.MethodPost, "/api/projects/"+p.ID+"/check", "", true)
	var res tools.CheckResult
	if err := json.Unmarshal(body, &res); code != http.StatusOK || err != nil || res.OK ||
		res.Message != "PROJECT_TOKEN_KEY is not set; the stored token cannot be read." {
		t.Fatalf("check without a key: %d %s", code, body)
	}
}

// T5.17.2/rule 5: the check endpoint reads the stored token and asks the provider
// (here the fake behind CheckTransport); a token that does not open, a refused token
// and a project with no token are results, not HTTP errors.
func TestProjectCheckEndpoint(t *testing.T) {
	pool := needDB(t)
	fake, tr := newFakeProvider(t)
	_, ts := newTestServerDeps(t, Deps{Cfg: testConfig(), Pool: pool, CheckTransport: tr}, nil)
	sfx := uniqueSuffix()
	token := acceptedTokenPrefix + sfx
	check := func(id string) tools.CheckResult {
		t.Helper()
		code, body := do(t, ts, http.MethodPost, "/api/projects/"+id+"/check", "")
		var res tools.CheckResult
		if err := json.Unmarshal(body, &res); code != http.StatusOK || err != nil || res.CheckedAt.IsZero() {
			t.Fatalf("check: %d %s", code, body)
		}
		noToken(t, "check", body, token)
		return res
	}

	code, body := do(t, ts, http.MethodPost, "/api/projects", `{"name":"check-`+sfx+`","repo_url":"https://gitlab.test/group/check-`+sfx+`.git","token":"`+token+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	p := decodeProject(t, body)
	t.Cleanup(func() { _ = store.DeleteProject(context.Background(), pool, p.ID) })
	res := check(p.ID)
	if !res.OK || res.Message != "Token accepted: default branch main, push allowed." || res.DefaultBranch != "main" ||
		res.BranchExists == nil || !*res.BranchExists || res.CanPush == nil || !*res.CanPush {
		t.Fatalf("check = %+v", res)
	}
	tokens, paths := fake.seen()
	if !slices.Equal(tokens, []string{token, token}) ||
		!slices.Equal(paths, []string{"/api/v4/projects/group%2Fcheck-" + sfx, "/api/v4/projects/group%2Fcheck-" + sfx + "/repository/branches/main"}) {
		t.Fatalf("the provider saw tokens %q, paths %q", tokens, paths)
	}

	refused := seedProject(t, testCipher(t), "refused-"+sfx, "glpat-revoked-not-a-real-token-00")
	if res := check(refused.ID); res.OK || res.Message != "The token was refused (401)." {
		t.Fatalf("refused = %+v", res)
	}

	other, err := project.NewCipher("another-key-not-a-secret")
	mustDo(t, err)
	rekeyed := seedProject(t, other, "rekeyed-"+sfx, token)
	if res := check(rekeyed.ID); res.OK || res.Message != "The stored token cannot be read; enter it again on the Projects page." {
		t.Fatalf("rekeyed = %+v", res)
	}

	empty := seedProject(t, nil, "empty-"+sfx, "")
	if res := check(empty.ID); res.OK || res.Message != "No token is stored; enter one on the Projects page." {
		t.Fatalf("no token = %+v", res)
	}

	if code, body := do(t, ts, http.MethodPost, "/api/projects/no-such-project/check", ""); code != http.StatusNotFound {
		t.Fatalf("unknown project: %d %s", code, body)
	}
}

// DESIGN 5.17 rule 9: the api's start creates the project PROJECT_* describe once and
// logs it, warns when the key is missing, and says nothing when there is nothing to do.
func TestBootstrapProjectAtStart(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	sfx := uniqueSuffix()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	cfg := testConfig()
	cfg.ProjectRepoURL, cfg.ProjectGitToken = "https://gitlab.test/boot/b"+sfx+".git", acceptedTokenPrefix+sfx

	bootstrapProject(ctx, pool, cfg, log)
	p, err := store.GetProjectByRepoURL(ctx, pool, cfg.ProjectRepoURL)
	if err != nil {
		t.Fatalf("no project after the bootstrap: %v\n%s", err, buf.String())
	}
	t.Cleanup(func() { _ = store.DeleteProject(ctx, pool, p.ID) })
	if p.Name != "b"+sfx || !strings.Contains(buf.String(), "project bootstrapped from PROJECT_*") || !strings.Contains(buf.String(), p.Name) {
		t.Fatalf("project %+v, log %s", p, buf.String())
	}
	if !p.TokenSet || strings.Contains(buf.String(), cfg.ProjectGitToken) {
		t.Fatalf("token_set %v, log %s", p.TokenSet, buf.String())
	}
	buf.Reset()
	bootstrapProject(ctx, pool, cfg, log)
	if buf.Len() != 0 {
		t.Fatalf("a second start logged: %s", buf.String())
	}

	noKey := cfg
	noKey.ProjectTokenKey = ""
	noKey.ProjectRepoURL = "https://gitlab.test/boot/c" + sfx + ".git"
	bootstrapProject(ctx, pool, noKey, log)
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "PROJECT_TOKEN_KEY is not set") {
		t.Fatalf("no key: %s", buf.String())
	}
	if _, err := store.GetProjectByRepoURL(ctx, pool, noKey.ProjectRepoURL); err == nil {
		t.Fatal("a project was created without a key")
	}
	if strings.Contains(buf.String(), cfg.ProjectGitToken) {
		t.Fatalf("the token is in the log: %s", buf.String())
	}

	buf.Reset()
	bootstrapProject(ctx, pool, testConfig(), log)
	if buf.Len() != 0 {
		t.Fatalf("nothing to do logged: %s", buf.String())
	}
}

// DESIGN 5.17 rule 12: the inbox carries the status of each approval's run, so a tool
// approval on a run that has ended can say that approving still executes the call.
func TestApprovalItemsCarryRunStatus(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	run, err := createTestRun(ctx, pool, store.TriggerManual, nil, store.Limits{})
	mustDo(t, err)
	node := &store.RunNode{RunID: run.ID, NodeID: "agent", Status: store.NodeRunning}
	mustDo(t, store.CreateRunNode(ctx, pool, node))
	mustDo(t, store.InsertApproval(ctx, pool, &store.Approval{RunID: run.ID, RunNodeID: node.ID, Kind: store.ApprovalKindNode,
		NodeID: "agent", Message: "ship it", ExpiresAt: time.Now().Add(time.Hour)}))
	list := func() []store.ApprovalItem {
		t.Helper()
		code, body := do(t, ts, http.MethodGet, "/api/approvals?run_id="+run.ID, "")
		var items []store.ApprovalItem
		if code != http.StatusOK || json.Unmarshal(body, &items) != nil || len(items) != 1 {
			t.Fatalf("approvals: %d %s", code, body)
		}
		return items
	}
	if got := list()[0].RunStatus; got != store.RunQueued {
		t.Fatalf("run_status = %q, want queued", got)
	}
	_, err = pool.Exec(ctx, `UPDATE runs SET status='succeeded', finished_at=now() WHERE id=$1`, run.ID)
	mustDo(t, err)
	if got := list()[0].RunStatus; got != store.RunSucceeded {
		t.Fatalf("run_status = %q, want succeeded", got)
	}
}

// delegate_task's project reaches the engine's trigger request (DESIGN 5.17 rule 8).
func TestDelegatorPassesProject(t *testing.T) {
	fe := &fakeEngine{}
	s, _ := newTestServer(t, nil, fe, nil)
	_, err := delegator{s}.Delegate(context.Background(), tools.Delegation{AgentID: "ag-1", Task: "fix the bug", ProjectID: "pr-1",
		TriggerRef: "conversation:c-1"})
	mustDo(t, err)
	if len(fe.triggers) != 1 || fe.triggers[0].ProjectID != "pr-1" || fe.triggers[0].Trigger != store.TriggerChannel {
		t.Fatalf("triggers = %+v", fe.triggers)
	}
}

// DESIGN 5.17 rule 2: an update without a token keeps the stored one, except when it
// moves the repository to another host or provider, which needs the token again.
func TestProjectUpdateNeedsTokenOnHostChange(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	sfx := uniqueSuffix()
	p := seedProject(t, testCipher(t), "move-"+sfx, acceptedTokenPrefix+sfx)
	put := func(body string) (int, []byte) { return do(t, ts, http.MethodPut, "/api/projects/"+p.ID, body) }

	code, body := put(`{"name":"move-` + sfx + `","repo_url":"https://elsewhere.test/group/demo.git"}`)
	if code != http.StatusUnprocessableEntity || !slices.Equal(detailPaths(t, body), []string{"token"}) {
		t.Fatalf("new host without a token: %d %s", code, body)
	}
	code, body = put(`{"name":"move-` + sfx + `","repo_url":"https://gitlab.test/group/demo.git","provider":"github"}`)
	if code != http.StatusUnprocessableEntity || !slices.Equal(detailPaths(t, body), []string{"token"}) {
		t.Fatalf("new provider without a token: %d %s", code, body)
	}
	// Another path on the same host keeps the token; so does a case change of the host.
	code, body = put(`{"name":"move-` + sfx + `","repo_url":"https://GitLab.test/other/demo.git"}`)
	if code != http.StatusOK || !decodeProject(t, body).TokenSet {
		t.Fatalf("same host: %d %s", code, body)
	}
	code, body = put(`{"name":"move-` + sfx + `","repo_url":"https://elsewhere.test/group/demo.git","token":"` + acceptedTokenPrefix + sfx + `-2"}`)
	if code != http.StatusOK || decodeProject(t, body).RepoURL != "https://elsewhere.test/group/demo.git" {
		t.Fatalf("new host with a token: %d %s", code, body)
	}
	noToken(t, "moves", body, acceptedTokenPrefix+sfx+"-2")
}
