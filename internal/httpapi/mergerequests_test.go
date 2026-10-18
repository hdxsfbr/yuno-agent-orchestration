package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/routers"
	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/obs"
	"yuno/internal/project"
	"yuno/internal/store"
)

// mrProvider is GitLab's merge request API for the projects these tests create (all of
// them point at group/demo), scripted per route below the project ("GET
// /merge_requests/7"): the answers, every call with its body, and the tokens seen.
type mrProvider struct {
	mu     sync.Mutex
	routes map[string]mrAnswer
	calls  []string
	bodies map[string]map[string]any
	tokens []string
}

type mrAnswer struct {
	status int
	body   any
}

const mrProjectPath = "/api/v4/projects/group%2Fdemo"

func newMRProvider(t *testing.T) (*mrProvider, http.RoundTripper) {
	t.Helper()
	f := &mrProvider{routes: map[string]mrAnswer{}, bodies: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := r.Method + " " + strings.TrimPrefix(r.URL.EscapedPath(), mrProjectPath)
		var body map[string]any
		if r.ContentLength != 0 {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		f.mu.Lock()
		f.calls = append(f.calls, route)
		f.bodies[route] = body
		f.tokens = append(f.tokens, r.Header.Get("PRIVATE-TOKEN"))
		a, ok := f.routes[route]
		f.mu.Unlock()
		if !ok {
			a = mrAnswer{404, map[string]string{"message": "404 Not Found"}}
		}
		writeJSON(w, a.status, a.body)
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	return f, mrRewrite{target}
}

type mrRewrite struct{ target *url.URL }

func (rt mrRewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host, r2.Host = rt.target.Scheme, rt.target.Host, ""
	return http.DefaultTransport.RoundTrip(r2)
}

func (f *mrProvider) set(route string, status int, body any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[route] = mrAnswer{status, body}
}

// request scripts the three reads of one merge request.
func (f *mrProvider) request(n int64, state, pipeline string, unresolved int) {
	f.set(fmt.Sprintf("GET /merge_requests/%d", n), 200, map[string]any{"state": state, "sha": "sha-" + state})
	disc := []map[string]any{}
	for range unresolved {
		disc = append(disc, map[string]any{"notes": []map[string]any{{"resolvable": true, "resolved": false}}})
	}
	f.set(fmt.Sprintf("GET /merge_requests/%d/discussions", n), 200, disc)
	pipes := []map[string]any{}
	if pipeline != "" {
		pipes = append(pipes, map[string]any{"status": pipeline})
	}
	f.set(fmt.Sprintf("GET /merge_requests/%d/pipelines", n), 200, pipes)
}

func (f *mrProvider) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *mrProvider) body(route string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[route]
}

// reportingEngine is the test engine that can also send platform messages, the way the
// real engine does (mergereq.Reporter): the handlers report the final state through it.
type reportingEngine struct {
	*fakeEngine
	mu   sync.Mutex
	sent []platformMessage
}

type platformMessage struct{ runID, conversationID, key, text string }

func (e *reportingEngine) SendPlatformMessage(_ context.Context, runID, conversationID, key, text string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sent = append(e.sent, platformMessage{runID, conversationID, key, text})
	return false
}

func (e *reportingEngine) messages() []platformMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]platformMessage(nil), e.sent...)
}

// mrEnv is a server over a fake provider with one project whose token is sealed.
type mrEnv struct {
	t       *testing.T
	pool    *pgxpool.Pool
	prov    *mrProvider
	eng     *reportingEngine
	ts      *httptest.Server
	router  routers.Router
	project *store.Project
	token   string
	n       atomic.Int64
}

func newMREnv(t *testing.T) *mrEnv {
	t.Helper()
	pool := needDB(t)
	prov, tr := newMRProvider(t)
	eng := &reportingEngine{fakeEngine: &fakeEngine{pool: pool}}
	_, ts := newTestServerDeps(t, Deps{Cfg: testConfig(), Pool: pool, CheckTransport: tr}, eng)
	e := &mrEnv{t: t, pool: pool, prov: prov, eng: eng, ts: ts, router: contract(t)}
	e.token = acceptedTokenPrefix + uniqueSuffix()
	e.project = e.newProject(testCipher(t), e.token)
	return e
}

func (e *mrEnv) newProject(c *project.Cipher, token string) *store.Project {
	e.t.Helper()
	p := &store.Project{ID: store.NewID(), Name: "mr-" + uniqueSuffix(), RepoURL: "https://gitlab.test/group/demo.git",
		Provider: "gitlab", DefaultBranch: "main"}
	blob, err := c.Seal(p.ID, token)
	mustDo(e.t, err)
	mustDo(e.t, store.CreateProject(context.Background(), e.pool, p, blob))
	e.t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM merge_requests WHERE project_id = $1`, p.ID)
		_ = store.DeleteProject(context.Background(), e.pool, p.ID)
	})
	return p
}

// record stores a run of the project (delegated from a conversation when chat) and its
// merge request record; mutate adjusts the record before it is stored.
func (e *mrEnv) record(p *store.Project, chat bool, mutate func(*store.MergeRequest)) (*store.Run, *store.MergeRequest) {
	e.t.Helper()
	ctx := context.Background()
	wf := &store.Workflow{Name: "mr " + uniqueSuffix(), Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(e.t, store.CreateWorkflow(ctx, e.pool, wf))
	settings := p.Settings()
	run := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: store.TriggerManual,
		ProjectID: &p.ID, Project: &settings}
	if chat {
		ref := store.ConversationRef("conv-" + uniqueSuffix())
		run.Trigger, run.TriggerRef = store.TriggerChannel, &ref
	}
	mustDo(e.t, store.CreateRun(ctx, e.pool, run))
	n := e.n.Add(1)
	mr := &store.MergeRequest{ProjectID: p.ID, RunID: run.ID, Provider: "gitlab", Number: n,
		URL: fmt.Sprintf("https://gitlab.test/group/demo/-/merge_requests/%d", n), SourceBranch: "yuno/abc", TargetBranch: "main", Title: "A change"}
	if mutate != nil {
		mutate(mr)
	}
	ok, err := store.InsertMergeRequest(ctx, e.pool, mr)
	mustDo(e.t, err)
	if !ok {
		e.t.Fatal("record not inserted")
	}
	return run, mr
}

// call sends a request and validates the response against the contract.
func (e *mrEnv) call(method, path, body string, validRequest bool) (int, []byte) {
	e.t.Helper()
	return checkConformance(e.t, e.router, e.ts.URL, method, path, body, validRequest)
}

func (e *mrEnv) get(id string) *store.MergeRequest {
	e.t.Helper()
	mr, err := store.GetMergeRequest(context.Background(), e.pool, id)
	mustDo(e.t, err)
	return mr
}

func decodeMR(t *testing.T, b []byte) store.MergeRequest {
	t.Helper()
	var mr store.MergeRequest
	if err := json.Unmarshal(b, &mr); err != nil || mr.ID == "" {
		t.Fatalf("not a merge request: %s", b)
	}
	return mr
}

// Rule 6: the list is newest first and filters by project, run and state; limit and
// state are validated; GET one is 404 for an unknown id.
func TestMergeRequestListAndGet(t *testing.T) {
	e := newMREnv(t)
	base := time.Now().UTC().Add(-time.Hour)
	setAt := func(i int, state string) func(*store.MergeRequest) {
		return func(mr *store.MergeRequest) {
			mr.CreatedAt = base.Add(time.Duration(i) * time.Minute)
			mr.State = state
		}
	}
	runA, a := e.record(e.project, false, setAt(1, store.MergeRequestOpen))
	_, b := e.record(e.project, false, setAt(2, store.MergeRequestMerged))
	_, c := e.record(e.project, false, setAt(3, store.MergeRequestOpen))

	list := func(query string) []store.MergeRequest {
		t.Helper()
		code, body := e.call(http.MethodGet, "/api/merge-requests?"+query, "", true)
		if code != http.StatusOK {
			t.Fatalf("list %s: %d %s", query, code, body)
		}
		var out []store.MergeRequest
		mustDo(t, json.Unmarshal(body, &out))
		return out
	}
	ids := func(l []store.MergeRequest) string {
		var s []string
		for _, mr := range l {
			s = append(s, mr.ID)
		}
		return strings.Join(s, ",")
	}
	pid := "project_id=" + e.project.ID
	if got, want := ids(list(pid)), strings.Join([]string{c.ID, b.ID, a.ID}, ","); got != want {
		t.Fatalf("list = %s, want %s", got, want)
	}
	if got := ids(list(pid + "&state=open")); got != c.ID+","+a.ID {
		t.Fatalf("open = %s", got)
	}
	if got := ids(list(pid + "&state=merged")); got != b.ID {
		t.Fatalf("merged = %s", got)
	}
	if got := ids(list(pid + "&limit=1")); got != c.ID {
		t.Fatalf("limit 1 = %s", got)
	}
	if got := ids(list("run_id=" + runA.ID)); got != a.ID {
		t.Fatalf("by run = %s", got)
	}
	if got := list("project_id=no-such-project"); got == nil || len(got) != 0 {
		t.Fatalf("no match = %#v", got)
	}
	// Like the runs list, an invalid query is a 422 (the contract documents only the 200).
	for _, q := range []string{"state=bogus", "limit=0", "limit=201", "limit=abc"} {
		code, body := do(t, e.ts, http.MethodGet, "/api/merge-requests?"+q, "")
		if code != http.StatusUnprocessableEntity || len(detailPaths(t, body)) != 1 {
			t.Fatalf("%s: %d %s", q, code, body)
		}
	}
	if code, _ := e.call(http.MethodGet, "/api/merge-requests?limit=200", "", true); code != http.StatusOK {
		t.Fatalf("limit 200: %d", code)
	}

	code, body := e.call(http.MethodGet, "/api/merge-requests/"+a.ID, "", true)
	got := decodeMR(t, body)
	if code != http.StatusOK || got.RunID != runA.ID || got.Number != a.Number || got.State != store.MergeRequestOpen || got.CIStatus != store.CIStatusNone {
		t.Fatalf("get = %d %s", code, body)
	}
	if code, _ := e.call(http.MethodGet, "/api/merge-requests/no-such", "", true); code != http.StatusNotFound {
		t.Fatalf("unknown: %d", code)
	}
	noToken(t, "list", body, e.token)
}

// T5.20.10: Run.merge_request on a get and a list, absent on a run without a record.
func TestRunCarriesItsMergeRequest(t *testing.T) {
	e := newMREnv(t)
	with, rec := e.record(e.project, false, func(mr *store.MergeRequest) { mr.CIStatus = store.CIStatusPassed })
	without := projectRun(t, e.project)

	var run store.Run
	code, body := e.call(http.MethodGet, "/api/runs/"+with.ID, "", true)
	mustDo(t, json.Unmarshal(body, &run))
	if code != http.StatusOK || run.MergeRequest == nil || run.MergeRequest.ID != rec.ID || run.MergeRequest.Number != rec.Number || run.MergeRequest.CIStatus != store.CIStatusPassed {
		t.Fatalf("get run = %d %s", code, body)
	}
	var raw map[string]json.RawMessage
	_, body = e.call(http.MethodGet, "/api/runs/"+without.ID, "", true)
	mustDo(t, json.Unmarshal(body, &raw))
	if _, ok := raw["merge_request"]; ok {
		t.Fatalf("a run without a record carries merge_request: %s", body)
	}

	// The trace's run carries it too (DESIGN 5.20 rule 6), so the run page needs no second fetch.
	var trace struct {
		Run store.Run `json:"run"`
	}
	code, body = e.call(http.MethodGet, "/api/runs/"+with.ID+"/trace", "", true)
	mustDo(t, json.Unmarshal(body, &trace))
	if code != http.StatusOK || trace.Run.MergeRequest == nil || trace.Run.MergeRequest.ID != rec.ID || trace.Run.MergeRequest.CIStatus != store.CIStatusPassed {
		t.Fatalf("trace of the run with a record = %d %s", code, body)
	}
	var rawTrace struct {
		Run map[string]json.RawMessage `json:"run"`
	}
	_, body = e.call(http.MethodGet, "/api/runs/"+without.ID+"/trace", "", true)
	mustDo(t, json.Unmarshal(body, &rawTrace))
	if _, ok := rawTrace.Run["merge_request"]; ok || rawTrace.Run["id"] == nil {
		t.Fatalf("the trace of a run without a record carries merge_request: %s", body)
	}

	code, body = e.call(http.MethodGet, "/api/runs?limit=500", "", true)
	var runs []store.Run
	mustDo(t, json.Unmarshal(body, &runs))
	found := map[string]*store.MergeRequest{}
	for i := range runs {
		found[runs[i].ID] = runs[i].MergeRequest
	}
	if code != http.StatusOK || found[with.ID] == nil || found[with.ID].ID != rec.ID {
		t.Fatalf("list runs: %d, merge_request of the run = %+v", code, found[with.ID])
	}
	if mr, ok := found[without.ID]; !ok || mr != nil {
		t.Fatalf("list runs: run without a record = %v (listed %v)", mr, ok)
	}
}

// Rule 3: POST sync refreshes the record at once and answers it; a provider error is on
// the record, not an HTTP error; a 404 closes it with the text; a final record is left
// alone; an unknown id is 404.
func TestSyncEndpoint(t *testing.T) {
	e := newMREnv(t)
	_, rec := e.record(e.project, false, nil)
	e.prov.request(rec.Number, "opened", "running", 2)

	code, body := e.call(http.MethodPost, "/api/merge-requests/"+rec.ID+"/sync", "", true)
	got := decodeMR(t, body)
	if code != http.StatusOK || got.State != store.MergeRequestOpen || got.CIStatus != store.CIStatusPending || got.UnresolvedThreads == nil ||
		*got.UnresolvedThreads != 2 || got.SyncedAt == nil || got.HeadSHA == nil || *got.HeadSHA != "sha-opened" || got.LastSyncError != nil {
		t.Fatalf("sync = %d %s", code, body)
	}
	e.prov.set(fmt.Sprintf("GET /merge_requests/%d", rec.Number), 500, map[string]string{"message": "boom " + e.token})
	code, body = e.call(http.MethodPost, "/api/merge-requests/"+rec.ID+"/sync", "", true)
	got = decodeMR(t, body)
	if code != http.StatusOK || got.LastSyncError == nil || !strings.Contains(*got.LastSyncError, "HTTP 500") || got.CIStatus != store.CIStatusPending {
		t.Fatalf("provider error = %d %s", code, body)
	}
	noToken(t, "sync", body, e.token)

	e.prov.set(fmt.Sprintf("GET /merge_requests/%d", rec.Number), 404, map[string]string{"message": "404"})
	code, body = e.call(http.MethodPost, "/api/merge-requests/"+rec.ID+"/sync", "", true)
	got = decodeMR(t, body)
	if code != http.StatusOK || got.State != store.MergeRequestClosed || got.LastSyncError == nil || *got.LastSyncError != "not found on the provider" || got.ClosedAt == nil {
		t.Fatalf("not found = %d %s", code, body)
	}
	before := len(e.prov.seen())
	code, _ = e.call(http.MethodPost, "/api/merge-requests/"+rec.ID+"/sync", "", true)
	if code != http.StatusOK || len(e.prov.seen()) != before {
		t.Fatalf("a closed record was refreshed: %d, calls %d -> %d", code, before, len(e.prov.seen()))
	}
	if code, _ := e.call(http.MethodPost, "/api/merge-requests/no-such/sync", "", true); code != http.StatusNotFound {
		t.Fatalf("unknown: %d", code)
	}
}

// T5.20.6: the preconditions of a merge and the provider's refusals.
func TestMergePreconditionsAndRefusals(t *testing.T) {
	e := newMREnv(t)
	merge := func(id string) (int, apiError) {
		t.Helper()
		code, body := e.call(http.MethodPost, "/api/merge-requests/"+id+"/merge", "", true)
		return code, decodeError(t, body)
	}
	expect := func(id string, status int, code, message string) {
		t.Helper()
		gotStatus, err := merge(id)
		if gotStatus != status || err.Error.Code != code || err.Error.Message != message {
			t.Fatalf("merge %s = %d %s %q, want %d %s %q", id, gotStatus, err.Error.Code, err.Error.Message, status, code, message)
		}
	}
	for _, state := range []string{store.MergeRequestMerged, store.MergeRequestClosed} {
		_, rec := e.record(e.project, false, func(mr *store.MergeRequest) { mr.State = state })
		expect(rec.ID, http.StatusConflict, "merge_request_not_open", "the request is "+state)
	}
	for _, ci := range []string{store.CIStatusFailed, store.CIStatusPending} {
		_, rec := e.record(e.project, false, func(mr *store.MergeRequest) { mr.CIStatus = ci })
		expect(rec.ID, http.StatusConflict, "merge_request_ci_not_passed", "the pipeline is "+ci+"; merge on the provider to override")
	}
	if len(e.prov.seen()) != 0 {
		t.Fatalf("the provider was asked about a refused precondition: %v", e.prov.seen())
	}
	if code, _ := e.call(http.MethodPost, "/api/merge-requests/no-such/merge", "", true); code != http.StatusNotFound {
		t.Fatalf("unknown: %d", code)
	}

	// The provider's own refusals: 405, 406, 409, 422 with its first line.
	for _, status := range []int{405, 406, 409, 422} {
		_, rec := e.record(e.project, true, func(mr *store.MergeRequest) { mr.CIStatus = store.CIStatusPassed })
		e.prov.set(fmt.Sprintf("PUT /merge_requests/%d/merge", rec.Number), status,
			map[string]string{"message": "Branch cannot be merged\nthe second line stays with the provider"})
		expect(rec.ID, http.StatusConflict, "merge_request_not_mergeable", "Branch cannot be merged")
		if got := e.get(rec.ID); got.State != store.MergeRequestOpen || got.MergedBy != nil {
			t.Fatalf("%d: a refused merge changed the record: %+v", status, got)
		}
	}
	if len(e.eng.messages()) != 0 {
		t.Fatalf("a refused merge was reported: %+v", e.eng.messages())
	}

	// A token that cannot be read is something the operator fixes on the Projects page.
	other, err := project.NewCipher("another-key-not-a-secret")
	mustDo(t, err)
	rekeyed := e.newProject(other, e.token)
	_, rec := e.record(rekeyed, false, nil)
	expect(rec.ID, http.StatusConflict, "merge_request_not_mergeable", "The stored token cannot be read; enter it again on the Projects page.")
}

// T5.20.7: a merge records merged with closed_at and merged_by, removes the source branch
// on the provider, and reports the final state once; a second POST finds it merged.
func TestMergeSucceeds(t *testing.T) {
	e := newMREnv(t)
	for _, ci := range []string{store.CIStatusPassed, store.CIStatusNone} {
		run, rec := e.record(e.project, true, func(mr *store.MergeRequest) { mr.CIStatus = ci })
		route := fmt.Sprintf("PUT /merge_requests/%d/merge", rec.Number)
		e.prov.set(route, 200, map[string]any{"state": "merged"})
		e.prov.request(rec.Number, "merged", "success", 0) // what the read after the 2xx finds
		before := time.Now().UTC().Add(-time.Second)

		code, body := e.call(http.MethodPost, "/api/merge-requests/"+rec.ID+"/merge", "", true)
		got := decodeMR(t, body)
		if code != http.StatusOK || got.State != store.MergeRequestMerged || got.MergedBy == nil || *got.MergedBy != "operator" ||
			got.ClosedAt == nil || got.ClosedAt.Before(before) || got.ReportedState == nil || *got.ReportedState != "merged" {
			t.Fatalf("merge (%s) = %d %s", ci, code, body)
		}
		if e.prov.body(route)["should_remove_source_branch"] != true {
			t.Fatalf("merge body = %v", e.prov.body(route))
		}
		noToken(t, "merge", body, e.token)
		if got := e.get(rec.ID); got.State != store.MergeRequestMerged || got.MergedBy == nil || got.ClosedAt == nil {
			t.Fatalf("stored = %+v", got)
		}
		sent := e.eng.messages()
		last := sent[len(sent)-1]
		wantText := fmt.Sprintf("[platform] Run %s: merge request !%d was merged.", run.ID, rec.Number)
		if last.key != "report-"+run.ID+"-mr-merged" || last.text != wantText || last.runID != run.ID || !strings.HasPrefix(last.conversationID, "conv-") {
			t.Fatalf("report = %+v", last)
		}

		puts := countCalls(e.prov.seen(), route)
		code, body = e.call(http.MethodPost, "/api/merge-requests/"+rec.ID+"/merge", "", true)
		if err := decodeError(t, body); code != http.StatusConflict || err.Error.Code != "merge_request_not_open" || err.Error.Message != "the request is merged" {
			t.Fatalf("second merge = %d %s", code, body)
		}
		if countCalls(e.prov.seen(), route) != puts {
			t.Fatal("a second POST merged again on the provider")
		}
	}
	if len(e.eng.messages()) != 2 {
		t.Fatalf("messages = %+v", e.eng.messages())
	}
}

// Rule 5: a 2xx from the provider's merge call is not the merge. The request is read once
// more and the record is marked merged only when the provider reports it merged;
// otherwise it stays open with the provider's answer in last_sync_error, nothing is
// reported, and the answer is 200 with the record.
func TestMergeIsRecordedOnlyWhenTheProviderReportsItMerged(t *testing.T) {
	e := newMREnv(t)
	ready := func(chat bool) (*store.Run, *store.MergeRequest, string, string) {
		run, rec := e.record(e.project, chat, func(mr *store.MergeRequest) { mr.CIStatus = store.CIStatusPassed })
		return run, rec, fmt.Sprintf("PUT /merge_requests/%d/merge", rec.Number), fmt.Sprintf("GET /merge_requests/%d", rec.Number)
	}
	merge := func(rec *store.MergeRequest) (int, store.MergeRequest, []byte) {
		t.Helper()
		code, body := e.call(http.MethodPost, "/api/merge-requests/"+rec.ID+"/merge", "", true)
		return code, decodeMR(t, body), body
	}
	untouched := func(what string, got store.MergeRequest, wantError string) {
		t.Helper()
		if got.State != store.MergeRequestOpen || got.MergedBy != nil || got.ClosedAt != nil || got.ReportedState != nil ||
			got.LastSyncError == nil || !strings.Contains(*got.LastSyncError, wantError) || got.SyncedAt == nil {
			t.Fatalf("%s: record = %+v (error %v), want open with %q", what, got, got.LastSyncError, wantError)
		}
	}

	// The call is accepted and the provider still reports the request open (GitLab merges
	// asynchronously, or when the pipeline passes).
	run, rec, put, get := ready(true)
	e.prov.set(put, 200, map[string]any{"state": "opened"})
	e.prov.request(rec.Number, "opened", "success", 0)
	code, got, body := merge(rec)
	if code != http.StatusOK || got.ID != rec.ID {
		t.Fatalf("accepted but open: %d %s", code, body)
	}
	untouched("accepted but open", got, "")
	if *got.LastSyncError != "the provider accepted the merge but reports the request as open" {
		t.Fatalf("last_sync_error = %q", *got.LastSyncError)
	}
	noToken(t, "merge", body, e.token)
	if stored := e.get(rec.ID); stored.State != store.MergeRequestOpen || stored.MergedBy != nil || len(e.eng.messages()) != 0 {
		t.Fatalf("stored = %+v, messages %+v", stored, e.eng.messages())
	}
	if countCalls(e.prov.seen(), put) != 1 || countCalls(e.prov.seen(), get) != 1 {
		t.Fatalf("calls = %v: one merge, then one read", e.prov.seen())
	}

	// The loop (here, the operator's Sync) settles it once the provider's state is final:
	// merged by the provider's own doing, so merged_by stays empty, and the report goes out.
	e.prov.request(rec.Number, "merged", "success", 0)
	code, body = e.call(http.MethodPost, "/api/merge-requests/"+rec.ID+"/sync", "", true)
	synced := decodeMR(t, body)
	if code != http.StatusOK || synced.State != store.MergeRequestMerged || synced.LastSyncError != nil || synced.ReportedState == nil {
		t.Fatalf("sync after the merge landed = %d %s", code, body)
	}
	if sent := e.eng.messages(); len(sent) != 1 || sent[0].key != "report-"+run.ID+"-mr-merged" {
		t.Fatalf("messages = %+v", sent)
	}

	// Pressing Merge again on a record that stayed open merges and records it.
	_, rec2, put2, _ := ready(false)
	e.prov.set(put2, 200, map[string]any{"state": "opened"})
	e.prov.request(rec2.Number, "opened", "success", 0)
	if _, got, _ := merge(rec2); got.State != store.MergeRequestOpen {
		t.Fatalf("first press = %+v", got)
	}
	e.prov.request(rec2.Number, "merged", "success", 0)
	code, got, body = merge(rec2)
	if code != http.StatusOK || got.State != store.MergeRequestMerged || got.MergedBy == nil || *got.MergedBy != "operator" || got.LastSyncError != nil || got.ClosedAt == nil {
		t.Fatalf("second press = %d %s", code, body)
	}

	// Any other state is no merge either; so is a status that cannot be read, which
	// leaves the provider's text on the record with the token redacted.
	_, closed, putClosed, _ := ready(false)
	e.prov.set(putClosed, 200, map[string]any{})
	e.prov.request(closed.Number, "closed", "success", 0)
	code, got, body = merge(closed)
	if code != http.StatusOK {
		t.Fatalf("accepted but closed: %d %s", code, body)
	}
	untouched("accepted but closed", got, "the provider accepted the merge but reports the request as closed")

	_, blind, putBlind, getBlind := ready(false)
	e.prov.set(putBlind, 202, map[string]any{})
	e.prov.set(getBlind, 502, map[string]string{"message": "bad gateway " + e.token})
	code, got, body = merge(blind)
	if code != http.StatusOK {
		t.Fatalf("accepted, unreadable: %d %s", code, body)
	}
	untouched("accepted, unreadable", got, "the provider accepted the merge but the request could not be read back")
	if !strings.Contains(*got.LastSyncError, "HTTP 502") {
		t.Fatalf("last_sync_error = %q", *got.LastSyncError)
	}
	noToken(t, "unreadable merge", body, e.token)
	if len(e.eng.messages()) != 1 { // the sync's report, and no other
		t.Fatalf("messages = %+v", e.eng.messages())
	}
}

// DESIGN 5.20 rule 5: the provider's first line of a refusal reaches the operator in the
// 409 body, so it passes the same redaction as the 502 body: a token it echoes, the
// project's own or one shaped like a token, is gone.
func TestMergeRefusalRedactsTheToken(t *testing.T) {
	e := newMREnv(t)
	opaque := "plain-secret-value-" + uniqueSuffix() // no redaction pattern knows this shape
	hidden := e.newProject(testCipher(t), opaque)
	for name, tc := range map[string]struct {
		p     *store.Project
		token string
	}{"a token with a known shape": {e.project, e.token}, "an opaque token": {hidden, opaque}} {
		_, rec := e.record(tc.p, false, func(mr *store.MergeRequest) { mr.CIStatus = store.CIStatusPassed })
		e.prov.set(fmt.Sprintf("PUT /merge_requests/%d/merge", rec.Number), 405,
			map[string]string{"message": "Branch cannot be merged for " + tc.token + " (token " + tc.token + ")\nsecond line"})
		code, body := e.call(http.MethodPost, "/api/merge-requests/"+rec.ID+"/merge", "", true)
		err := decodeError(t, body)
		if code != http.StatusConflict || err.Error.Code != "merge_request_not_mergeable" || !strings.HasPrefix(err.Error.Message, "Branch cannot be merged for ") {
			t.Fatalf("%s: %d %s", name, code, body)
		}
		noToken(t, name, body, tc.token)
		if !strings.Contains(err.Error.Message, obs.Redacted) {
			t.Fatalf("%s: the message shows no redaction: %q", name, err.Error.Message)
		}
		if got := e.get(rec.ID); got.State != store.MergeRequestOpen {
			t.Fatalf("%s: a refused merge changed the record: %+v", name, got)
		}
	}
}

func countCalls(calls []string, route string) int {
	n := 0
	for _, c := range calls {
		if c == route {
			n++
		}
	}
	return n
}

// Rule 5: a 5xx or a 404 from the merge call is followed by one re-sync, since the merge
// may have landed; permanent failures of the provider's own are 502 with its text.
func TestMergeProviderFailures(t *testing.T) {
	e := newMREnv(t)
	ready := func(chat bool) (*store.Run, *store.MergeRequest, string) {
		run, rec := e.record(e.project, chat, func(mr *store.MergeRequest) { mr.CIStatus = store.CIStatusPassed })
		return run, rec, fmt.Sprintf("PUT /merge_requests/%d/merge", rec.Number)
	}

	// A 503 and the request is still open after the re-sync: 502 provider_unavailable.
	_, rec, route := ready(false)
	e.prov.set(route, 503, map[string]string{"message": "down"})
	e.prov.request(rec.Number, "opened", "success", 0)
	code, body := e.call(http.MethodPost, "/api/merge-requests/"+rec.ID+"/merge", "", true)
	if err := decodeError(t, body); code != http.StatusBadGateway || err.Error.Code != "provider_unavailable" {
		t.Fatalf("503 = %d %s", code, body)
	}
	if got := e.get(rec.ID); got.State != store.MergeRequestOpen || got.SyncedAt == nil {
		t.Fatalf("the record was not re-synced: %+v", got)
	}
	if n := countCalls(e.prov.seen(), fmt.Sprintf("GET /merge_requests/%d", rec.Number)); n != 1 {
		t.Fatalf("re-syncs = %d, want one", n)
	}

	// A 503 and the re-sync finds it merged (the merge landed): the merged record, reported.
	run, rec, route := ready(true)
	e.prov.set(route, 503, map[string]string{"message": "down"})
	e.prov.request(rec.Number, "merged", "success", 0)
	code, body = e.call(http.MethodPost, "/api/merge-requests/"+rec.ID+"/merge", "", true)
	got := decodeMR(t, body)
	if code != http.StatusOK || got.State != store.MergeRequestMerged || got.ReportedState == nil {
		t.Fatalf("landed merge = %d %s", code, body)
	}
	if sent := e.eng.messages(); len(sent) != 1 || sent[0].key != "report-"+run.ID+"-mr-merged" {
		t.Fatalf("messages = %+v", sent)
	}

	// A 404 on the merge: the request is gone; the re-sync closes the record.
	_, rec, route = ready(false)
	e.prov.set(route, 404, map[string]string{"message": "404 Not found"})
	code, body = e.call(http.MethodPost, "/api/merge-requests/"+rec.ID+"/merge", "", true)
	if err := decodeError(t, body); code != http.StatusConflict || err.Error.Code != "merge_request_not_open" || err.Error.Message != "the request is closed" {
		t.Fatalf("404 = %d %s", code, body)
	}

	// 403: the provider answered and refused for its own reason; nothing to re-sync.
	_, rec, route = ready(false)
	e.prov.set(route, 403, map[string]string{"message": "403 Forbidden " + e.token})
	before := len(e.prov.seen())
	code, body = e.call(http.MethodPost, "/api/merge-requests/"+rec.ID+"/merge", "", true)
	err := decodeError(t, body)
	if code != http.StatusBadGateway || err.Error.Code != "provider_error" || !strings.Contains(err.Error.Message, "HTTP 403") {
		t.Fatalf("403 = %d %s", code, body)
	}
	noToken(t, "403 response", body, e.token)
	if len(e.prov.seen())-before != 1 {
		t.Fatalf("calls after a 403: %v", e.prov.seen()[before:])
	}
	if got := e.get(rec.ID); got.State != store.MergeRequestOpen {
		t.Fatalf("record = %+v", got)
	}
}

// mergeRequestConformanceSteps exercises the four merge request paths and the Run
// schema's merge_request (DESIGN 5.20 rule 6) against the contract. It seeds against the
// fake provider TestOpenAPIConformance serves as the check transport, which does not know
// merge requests: a sync finds the request gone and closes the record.
func mergeRequestConformanceSteps(t *testing.T) []conformanceStep {
	t.Helper()
	ctx := context.Background()
	sfx := uniqueSuffix()
	p := seedProject(t, testCipher(t), "conf-mr-"+sfx, acceptedTokenPrefix+sfx)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM merge_requests WHERE project_id = $1`, p.ID)
	})
	var n int64
	seed := func(mutate func(*store.MergeRequest)) (*store.Run, *store.MergeRequest) {
		t.Helper()
		run := projectRun(t, p)
		n++
		mr := &store.MergeRequest{ProjectID: p.ID, RunID: run.ID, Provider: "gitlab", Number: n,
			URL: fmt.Sprintf("https://gitlab.test/group/conf/-/merge_requests/%d", n), SourceBranch: "yuno/abc", TargetBranch: "main", Title: "Conformance"}
		mutate(mr)
		ok, err := store.InsertMergeRequest(ctx, testPool, mr)
		mustDo(t, err)
		if !ok {
			t.Fatal("record not inserted")
		}
		return run, mr
	}
	run, shown := seed(func(mr *store.MergeRequest) {
		now := time.Now().UTC()
		threads, sha, msg := 2, "abc123", "gitlab: HTTP 502"
		mr.CIStatus, mr.HeadSHA, mr.UnresolvedThreads, mr.LastSyncError, mr.SyncedAt = store.CIStatusPending, &sha, &threads, &msg, &now
	})
	_, merged := seed(func(mr *store.MergeRequest) {
		now := time.Now().UTC()
		by, reported := "operator", store.MergeRequestMerged
		mr.State, mr.ClosedAt, mr.MergedBy, mr.ReportedState = store.MergeRequestMerged, &now, &by, &reported
	})
	_, failing := seed(func(mr *store.MergeRequest) { mr.CIStatus = store.CIStatusFailed })
	_, syncing := seed(func(mr *store.MergeRequest) {})
	return []conformanceStep{
		{http.MethodGet, "/api/merge-requests?project_id=" + p.ID, "", http.StatusOK},
		{http.MethodGet, "/api/merge-requests?project_id=" + p.ID + "&state=open&limit=2", "", http.StatusOK},
		{http.MethodGet, "/api/merge-requests?run_id=" + run.ID, "", http.StatusOK},
		{http.MethodGet, "/api/merge-requests/" + shown.ID, "", http.StatusOK},
		{http.MethodGet, "/api/merge-requests/" + merged.ID, "", http.StatusOK},
		{http.MethodGet, "/api/merge-requests/no-such-request", "", http.StatusNotFound},
		{http.MethodGet, "/api/runs/" + run.ID, "", http.StatusOK},
		{http.MethodGet, "/api/runs/" + run.ID + "/trace", "", http.StatusOK},
		{http.MethodPost, "/api/merge-requests/" + syncing.ID + "/sync", "", http.StatusOK},
		{http.MethodPost, "/api/merge-requests/" + merged.ID + "/sync", "", http.StatusOK},
		{http.MethodPost, "/api/merge-requests/no-such-request/sync", "", http.StatusNotFound},
		{http.MethodPost, "/api/merge-requests/" + merged.ID + "/merge", "", http.StatusConflict},
		{http.MethodPost, "/api/merge-requests/" + failing.ID + "/merge", "", http.StatusConflict},
		{http.MethodPost, "/api/merge-requests/no-such-request/merge", "", http.StatusNotFound},
	}
}
