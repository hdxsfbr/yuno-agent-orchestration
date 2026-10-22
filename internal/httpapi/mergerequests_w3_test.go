package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/routers"
	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/forgefake"
	"yuno/internal/mergereq"
	"yuno/internal/obs"
	"yuno/internal/project"
	"yuno/internal/project/projecttest"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// Chief platform control, DESIGN wave 3 rules 9 to 15, through the API against the fake
// forge: real pushes, pipelines of exact commits, the merge guard.

// forgeEnv is a server whose project lives on a fake forge (acme/app), with a run and the
// request record of its branch yuno/t1 once opened.
type forgeEnv struct {
	t       *testing.T
	pool    *pgxpool.Pool
	ts      *httptest.Server
	router  routers.Router
	forge   *httptest.Server
	token   string
	pc      project.Config
	project *store.Project
	run     *store.Run
}

func newForgeEnv(t *testing.T) *forgeEnv {
	t.Helper()
	pool := needDB(t)
	projecttest.NeedGit(t)
	e := &forgeEnv{t: t, pool: pool, router: contract(t), token: acceptedTokenPrefix + uniqueSuffix()}
	f, err := forgefake.New(e.token, t.TempDir())
	mustDo(t, err)
	e.forge = httptest.NewServer(f)
	t.Cleanup(e.forge.Close)
	e.forgeCall(http.MethodPost, "/_forge/repos", map[string]any{"path": "acme/app", "default_branch": "main"}, http.StatusCreated)
	e.pc = project.Config{RepoURL: e.forge.URL + "/acme/app.git", Token: e.token, DefaultBranch: "main", Provider: project.ProviderGitLab}

	eng := &reportingEngine{fakeEngine: &fakeEngine{pool: pool}}
	_, e.ts = newTestServerDeps(t, Deps{Cfg: testConfig(), Pool: pool}, eng)
	e.project = &store.Project{ID: store.NewID(), Name: "forge-" + uniqueSuffix(), RepoURL: e.pc.RepoURL, Provider: "gitlab", DefaultBranch: "main"}
	blob, err := testCipher(t).Seal(e.project.ID, e.token)
	mustDo(t, err)
	mustDo(t, store.CreateProject(context.Background(), pool, e.project, blob))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM merge_requests WHERE project_id = $1`, e.project.ID)
		_ = store.DeleteProject(context.Background(), pool, e.project.ID)
	})
	e.run = projectRun(t, e.project)
	return e
}

// forgeCall sends a request to the forge with its token and decodes a JSON answer.
func (e *forgeEnv) forgeCall(method, path string, in any, want int) map[string]any {
	e.t.Helper()
	var body io.Reader
	if in != nil {
		raw, _ := json.Marshal(in)
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.forge.URL+path, body)
	mustDo(e.t, err)
	req.Header.Set("PRIVATE-TOKEN", e.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	mustDo(e.t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		e.t.Fatalf("%s %s: %d, want %d: %s", method, path, resp.StatusCode, want, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// push commits files on top of from and pushes the commit to branch over the forge's Git
// HTTP, as a person or an agent does; it returns the commit.
func (e *forgeEnv) push(from, branch string, files map[string]string) string {
	e.t.Helper()
	u, err := url.Parse(e.pc.RepoURL)
	mustDo(e.t, err)
	u.User = url.UserPassword("oauth2", e.token)
	work := filepath.Join(e.t.TempDir(), "work")
	projecttest.Git(e.t, filepath.Dir(work), "clone", "--quiet", "--branch", from, u.String(), work)
	for name, body := range files {
		mustDo(e.t, os.WriteFile(filepath.Join(work, name), []byte(body), 0o644))
	}
	projecttest.Git(e.t, work, "add", "--all")
	projecttest.Git(e.t, work, "commit", "--quiet", "-m", "change")
	projecttest.Git(e.t, work, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
	return projecttest.Git(e.t, work, "rev-parse", "HEAD")
}

// open pushes yuno/t1 with files, opens its merge request on the forge and stores the
// record, never synced.
func (e *forgeEnv) open(files map[string]string) *store.MergeRequest {
	e.t.Helper()
	e.push("main", "yuno/t1", files)
	ref, err := tools.ProviderFor(&e.pc, nil).Create(context.Background(), "yuno/t1", "main", "A change", "", false)
	mustDo(e.t, err)
	mr := &store.MergeRequest{ProjectID: e.project.ID, RunID: e.run.ID, Provider: "gitlab", Number: ref.Number, URL: ref.URL,
		SourceBranch: "yuno/t1", TargetBranch: "main", Title: "A change"}
	ok, err := store.InsertMergeRequest(context.Background(), e.pool, mr)
	mustDo(e.t, err)
	if !ok {
		e.t.Fatal("record not inserted")
	}
	return mr
}

// pipeline records a pipeline on ref's head (or sha) on the forge.
func (e *forgeEnv) pipeline(state, ref string, jobs []map[string]any) {
	e.t.Helper()
	in := map[string]any{"state": state, "ref": ref}
	if jobs != nil {
		in["jobs"] = jobs
	}
	e.forgeCall(http.MethodPost, "/_forge/repos/acme/app/pipeline", in, http.StatusCreated)
}

func (e *forgeEnv) call(method, path, body string, validRequest bool) (int, []byte) {
	e.t.Helper()
	return checkConformance(e.t, e.router, e.ts.URL, method, path, body, validRequest)
}

func (e *forgeEnv) sync(id string) store.MergeRequest {
	e.t.Helper()
	code, body := e.call(http.MethodPost, "/api/merge-requests/"+id+"/sync", "", true)
	if code != http.StatusOK {
		e.t.Fatalf("sync = %d %s", code, body)
	}
	return decodeMR(e.t, body)
}

func (e *forgeEnv) tip(branch string) string {
	e.t.Helper()
	b := e.forgeCall(http.MethodGet, "/api/v4/projects/acme%2Fapp/repository/branches/"+url.PathEscape(branch), nil, http.StatusOK)
	commit, _ := b["commit"].(map[string]any)
	id, _ := commit["id"].(string)
	return id
}

func (e *forgeEnv) expectError(what string, code int, body []byte, wantStatus int, wantCode, wantIn string) apiError {
	e.t.Helper()
	err := decodeError(e.t, body)
	if code != wantStatus || err.Error.Code != wantCode || !strings.Contains(err.Error.Message, wantIn) {
		e.t.Fatalf("%s = %d %s, want %d %s with %q", what, code, body, wantStatus, wantCode, wantIn)
	}
	return err
}

// Rules 11, 12 and 15 end to end: a pass on the old head never lets the new head merge.
// The update from main pushes a merge commit and invalidates the check in one write; the
// merge then waits for CI on exactly the new head; a push from outside the platform after
// the check is refused by the provider (the merge names the head) and re-synced.
func TestStaleCheckInvalidated(t *testing.T) {
	e := newForgeEnv(t)
	rec := e.open(map[string]string{"feature.txt": "feature\n"})
	e.pipeline("passed", "main", nil) // the project has CI
	e.pipeline("passed", "yuno/t1", nil)
	h1 := e.tip("yuno/t1")

	got := e.sync(rec.ID)
	if got.CIStatus != store.CIStatusPassed || got.HeadSHA == nil || *got.HeadSHA != h1 || got.CISHA == nil || *got.CISHA != h1 ||
		got.CIPipelineID == nil || got.CIURL == nil || got.Mergeable == nil || !*got.Mergeable {
		t.Fatalf("first sync = %+v", got)
	}

	// Main moves on; the platform updates the branch from it.
	e.push("main", "main", map[string]string{"main.txt": "main\n"})
	code, body := e.call(http.MethodPost, "/api/merge-requests/"+rec.ID+"/update-from-default", `{"expected_head_sha":"`+h1+`"}`, true)
	var upd struct {
		Outcome      string             `json:"outcome"`
		Commit       string             `json:"commit"`
		MergeRequest store.MergeRequest `json:"merge_request"`
	}
	mustDo(t, json.Unmarshal(body, &upd))
	h2 := e.tip("yuno/t1")
	if code != http.StatusOK || upd.Outcome != "updated" || upd.Commit != h2 || h2 == h1 {
		t.Fatalf("update = %d %s (tip %s)", code, body, h2)
	}
	cur := upd.MergeRequest
	if cur.HeadSHA == nil || *cur.HeadSHA != h2 || cur.CISHA == nil || *cur.CISHA != h2 || cur.CIStatus != store.CIStatusPending ||
		cur.CIPipelineID != nil || cur.CIURL != nil || cur.UpdatedFromDefaultAt == nil {
		t.Fatalf("record after the update = %s", body)
	}
	var events int
	mustDo(t, e.pool.QueryRow(context.Background(), `SELECT count(*) FROM run_events WHERE run_id = $1 AND event = 'merge_request.updated'`, e.run.ID).Scan(&events))
	if events != 1 {
		t.Fatalf("merge_request.updated events = %d", events)
	}
	noToken(t, "update", body, e.token)

	merge := func() (int, []byte) {
		t.Helper()
		return e.call(http.MethodPost, "/api/merge-requests/"+rec.ID+"/merge", "", true)
	}
	code, body = merge()
	e.expectError("merge while pending", code, body, http.StatusConflict, "merge_request_ci_not_passed", "the pipeline is pending")

	// A sync finds no pipeline on the new head; the project has CI, so none is not enough.
	if got = e.sync(rec.ID); got.CIStatus != store.CIStatusNone || *got.CISHA != h2 {
		t.Fatalf("sync of the new head = %+v", got)
	}
	code, body = merge()
	e.expectError("merge with no pipeline on the head", code, body, http.StatusConflict, "merge_request_ci_not_passed", "no pipeline has run on "+h2[:7])

	// CI passes on the new head; then someone pushes to the branch before the merge.
	e.pipeline("passed", "yuno/t1", nil)
	if got = e.sync(rec.ID); got.CIStatus != store.CIStatusPassed || *got.CISHA != h2 {
		t.Fatalf("sync after CI passed = %+v", got)
	}
	h3 := e.push("yuno/t1", "yuno/t1", map[string]string{"late.txt": "late\n"})
	code, body = merge()
	moved := e.expectError("merge after a push", code, body, http.StatusConflict, "merge_request_head_moved", h2[:7])
	if current(moved) != h3 {
		t.Fatalf("head moved details = %s", body)
	}
	if after := e.get(rec.ID); after.State != store.MergeRequestOpen || *after.HeadSHA != h3 || *after.CISHA != h3 || after.CIStatus != store.CIStatusNone {
		t.Fatalf("record after the refused merge = %+v", after)
	}

	// CI passes on that head too: the merge goes through, with the head as its sha.
	e.pipeline("passed", "yuno/t1", nil)
	e.sync(rec.ID)
	code, body = merge()
	merged := decodeMR(t, body)
	if code != http.StatusOK || merged.State != store.MergeRequestMerged || merged.MergeSHA == nil || *merged.MergeSHA != e.tip("main") {
		t.Fatalf("merge = %d %s", code, body)
	}
}

// current is the head a merge_request_head_moved answer names in its details.
func current(err apiError) string {
	var d struct {
		Current string `json:"current"`
	}
	_ = json.Unmarshal(err.Error.Details, &d)
	return d.Current
}

func (e *forgeEnv) get(id string) *store.MergeRequest {
	e.t.Helper()
	mr, err := store.GetMergeRequest(context.Background(), e.pool, id)
	mustDo(e.t, err)
	return mr
}

// Rule 9 through the API: up to date, conflicts recorded with nothing pushed, a stale
// expected head and an invalid body.
func TestUpdateFromDefaultEndpoint(t *testing.T) {
	e := newForgeEnv(t)
	rec := e.open(map[string]string{"app.txt": "app from the branch\n"})
	h1 := e.tip("yuno/t1")
	update := func(sha string) (int, []byte) {
		t.Helper()
		return e.call(http.MethodPost, "/api/merge-requests/"+rec.ID+"/update-from-default", `{"expected_head_sha":"`+sha+`"}`, true)
	}
	code, body := update(h1)
	if code != http.StatusOK || !strings.Contains(string(body), `"outcome":"up_to_date"`) || e.tip("yuno/t1") != h1 {
		t.Fatalf("up to date = %d %s", code, body)
	}

	e.push("main", "main", map[string]string{"app.txt": "app from main\n"})
	code, body = update(h1)
	var res struct {
		Outcome      string             `json:"outcome"`
		Conflicts    []string           `json:"conflicts"`
		MergeRequest store.MergeRequest `json:"merge_request"`
	}
	mustDo(t, json.Unmarshal(body, &res))
	if code != http.StatusOK || res.Outcome != "conflicts" || len(res.Conflicts) != 1 || res.Conflicts[0] != "app.txt" ||
		res.MergeRequest.Conflicts == nil || res.MergeRequest.Conflicts.BaseSHA != e.tip("main") || e.tip("yuno/t1") != h1 {
		t.Fatalf("conflicts = %d %s", code, body)
	}

	stale := strings.Repeat("0", 40)
	code, body = update(stale)
	err := e.expectError("stale head", code, body, http.StatusConflict, "merge_request_head_moved", "nothing was pushed")
	if current(err) != h1 {
		t.Fatalf("details = %s", body)
	}
	for _, bad := range []string{`{}`, `{"expected_head_sha":"abc"}`, `{"expected_head_sha":"` + h1 + `","x":1}`} {
		code, body = e.call(http.MethodPost, "/api/merge-requests/"+rec.ID+"/update-from-default", bad, false)
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("%s = %d %s", bad, code, body)
		}
	}
	code, _ = e.call(http.MethodPost, "/api/merge-requests/no-such/update-from-default", `{"expected_head_sha":"`+h1+`"}`, true)
	if code != http.StatusNotFound {
		t.Fatalf("unknown = %d", code)
	}
}

// Rule 14: the diff, the checks of the head with their jobs, and a job's log, redacted and
// capped; the bounds of tail_bytes; a record never synced has no checks yet.
func TestMergeRequestDiffChecksLog(t *testing.T) {
	e := newForgeEnv(t)
	shaped := "glpat-" + strings.Repeat("x", 24)
	var big strings.Builder
	for i := 0; big.Len() < 300<<10; i++ {
		fmt.Fprintf(&big, "line %06d of a large generated file\n", i)
	}
	rec := e.open(map[string]string{"a-secret.txt": "token: " + shaped + "\nproject: " + e.token + "\n", "z-big.txt": big.String()})

	code, body := e.call(http.MethodGet, "/api/merge-requests/"+rec.ID+"/checks", "", true)
	e.expectError("checks before a sync", code, body, http.StatusConflict, "merge_request_head_unknown", "sync the request first")
	e.sync(rec.ID)

	code, body = e.call(http.MethodGet, "/api/merge-requests/"+rec.ID+"/diff", "", true)
	var diff mergereq.DiffRead
	mustDo(t, json.Unmarshal(body, &diff))
	if code != http.StatusOK || !diff.Truncated || len(diff.Files) != 2 || diff.Files[0].Path != "a-secret.txt" || diff.Files[0].Status != "added" ||
		!strings.Contains(diff.Files[0].Patch, obs.Redacted) || diff.Files[1].Path != "z-big.txt" || !strings.HasSuffix(diff.Files[1].Patch, "\n") {
		t.Fatalf("diff = %d (truncated %v, %d files)", code, diff.Truncated, len(diff.Files))
	}
	total := 0
	for _, f := range diff.Files {
		total += len(f.Path) + len(f.OldPath) + len(f.Patch)
	}
	if total > mergereq.MaxDiffBytes || total < mergereq.MaxDiffBytes-200 {
		t.Fatalf("diff carries %d bytes of paths and patches", total)
	}
	noToken(t, "diff", body, e.token)
	noToken(t, "diff", body, shaped)

	// A failed pipeline on the head with two jobs; the failing one logs a credential.
	var failLog strings.Builder
	for i := range 50 {
		fmt.Fprintf(&failLog, "step %d ok\n", i)
	}
	failLog.WriteString("auth with " + e.token + " and " + shaped + "\nFAIL: TestThing\n")
	e.pipeline("failed", "yuno/t1", []map[string]any{
		{"name": "build", "stage": "build", "state": "passed", "log": "built\n"},
		{"name": "unit", "stage": "test", "state": "failed", "log": failLog.String()},
	})
	code, body = e.call(http.MethodGet, "/api/merge-requests/"+rec.ID+"/checks", "", true)
	var checks tools.Checks
	mustDo(t, json.Unmarshal(body, &checks))
	head := *e.get(rec.ID).HeadSHA
	if code != http.StatusOK || checks.SHA != head || checks.Status != store.CIStatusFailed || checks.PipelineID == 0 || checks.URL == "" || len(checks.Jobs) != 2 ||
		checks.Jobs[0].Name != "build" || checks.Jobs[0].Stage != "build" || checks.Jobs[0].Status != tools.JobPassed ||
		checks.Jobs[1].Name != "unit" || checks.Jobs[1].Stage != "test" || checks.Jobs[1].Status != tools.JobFailed || checks.Jobs[1].URL == "" {
		t.Fatalf("checks = %d %s", code, body)
	}
	logPath := "/api/merge-requests/" + rec.ID + "/jobs/" + checks.Jobs[1].ID + "/log"

	code, body = e.call(http.MethodGet, logPath, "", true)
	var lg mergereq.LogRead
	mustDo(t, json.Unmarshal(body, &lg))
	if code != http.StatusOK || lg.Truncated || lg.Size != int64(failLog.Len()) || !strings.Contains(lg.Log, "FAIL: TestThing") ||
		!strings.Contains(lg.Log, "auth with "+obs.Redacted) || lg.Job != checks.Jobs[1].ID {
		t.Fatalf("log = %d %s", code, body)
	}
	noToken(t, "log", body, e.token)
	noToken(t, "log", body, shaped)

	// The tail: at most tail_bytes, starting at a line.
	code, body = e.call(http.MethodGet, logPath+"?tail_bytes=40", "", true)
	mustDo(t, json.Unmarshal(body, &lg))
	if code != http.StatusOK || !lg.Truncated || len(lg.Log) > 40 || lg.Log != "FAIL: TestThing\n" {
		t.Fatalf("tail = %d %s", code, body)
	}
	for _, q := range []string{"0", "1048577", "abc"} {
		code, body = e.call(http.MethodGet, logPath+"?tail_bytes="+q, "", false)
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("tail_bytes=%s = %d %s", q, code, body)
		}
	}
	for _, job := range []string{"9999", "status-1"} {
		code, body = e.call(http.MethodGet, "/api/merge-requests/"+rec.ID+"/jobs/"+job+"/log", "", true)
		e.expectError("job "+job, code, body, http.StatusNotFound, "not_found", "no log")
	}
	for _, p := range []string{"/diff", "/checks", "/jobs/1/log"} {
		if code, _ := e.call(http.MethodGet, "/api/merge-requests/no-such"+p, "", true); code != http.StatusNotFound {
			t.Fatalf("unknown %s = %d", p, code)
		}
	}
}

// Rule 15 over the scripted provider: the guard's refusals before the provider is asked,
// the sha the merge sends, and the provider's 409 for a head that moved.
func TestMergeNeedsExactSHA(t *testing.T) {
	e := newMREnv(t)
	merge := func(id string) (int, []byte) {
		t.Helper()
		return e.call(http.MethodPost, "/api/merge-requests/"+id+"/merge", "", true)
	}
	expect := func(what string, id string, status int, code, in string) apiError {
		t.Helper()
		got, body := merge(id)
		err := decodeError(t, body)
		if got != status || err.Error.Code != code || !strings.Contains(err.Error.Message, in) {
			t.Fatalf("%s = %d %s, want %d %s with %q", what, got, body, status, code, in)
		}
		return err
	}
	head := "1111111111111111111111111111111111111111"
	other := "2222222222222222222222222222222222222222"

	// Passed, but on another commit than the head; passed with the head unknown.
	_, stale := e.record(e.project, false, func(mr *store.MergeRequest) {
		mr.CIStatus, mr.HeadSHA, mr.CISHA = store.CIStatusPassed, &head, &other
	})
	expect("passed on another commit", stale.ID, http.StatusConflict, "merge_request_ci_not_passed", "not for the head commit 1111111")
	_, unknown := e.record(e.project, false, func(mr *store.MergeRequest) { mr.CIStatus = store.CIStatusPassed })
	expect("head unknown", unknown.ID, http.StatusConflict, "merge_request_ci_not_passed", "head commit of the request is not known")
	_, unchecked := e.record(e.project, false, func(mr *store.MergeRequest) { mr.CIStatus, mr.HeadSHA = store.CIStatusPassed, &head })
	expect("never checked", unchecked.ID, http.StatusConflict, "merge_request_ci_not_passed", "not for the head commit")
	if len(e.prov.seen()) != 0 {
		t.Fatalf("the provider was asked: %v", e.prov.seen())
	}

	// None on the head of a project whose main has a pipeline on a recent commit.
	e.prov.set("GET /repository/commits", 200, []map[string]any{{"id": "c2"}, {"id": "c1"}})
	e.prov.set("GET /pipelines", 200, []map[string]any{{"id": 7, "sha": "c1", "status": "success"}})
	_, none := e.record(e.project, false, checked(store.CIStatusNone))
	expect("none in a project with CI", none.ID, http.StatusConflict, "merge_request_ci_not_passed", "no pipeline has run on")
	if countCalls(e.prov.seen(), fmt.Sprintf("PUT /merge_requests/%d/merge", none.Number)) != 0 {
		t.Fatal("merged with no pipeline in a project with CI")
	}

	// The provider refuses the sha: the head moved. The record is re-synced, and the answer
	// names where the head is now.
	_, rec := e.record(e.project, false, func(mr *store.MergeRequest) {
		mr.CIStatus, mr.HeadSHA, mr.CISHA = store.CIStatusPassed, &head, &head
	})
	route := fmt.Sprintf("PUT /merge_requests/%d/merge", rec.Number)
	e.prov.set(route, 409, map[string]string{"message": "SHA does not match HEAD of source branch: " + other})
	e.prov.requestAt(rec.Number, "opened", other, "running", 0)
	err := expect("head moved", rec.ID, http.StatusConflict, "merge_request_head_moved", "no longer 1111111")
	if current(err) != other {
		t.Fatalf("details = %+v", err.Error.Details)
	}
	if body := e.prov.body(route); body["sha"] != head || body["should_remove_source_branch"] != true {
		t.Fatalf("merge body = %v", body)
	}
	if got := e.get(rec.ID); got.State != store.MergeRequestOpen || *got.HeadSHA != other || *got.CISHA != other || got.CIStatus != store.CIStatusPending {
		t.Fatalf("record after the refusal = %+v", got)
	}
}
