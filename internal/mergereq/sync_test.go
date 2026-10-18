package mergereq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/obs"
	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/tools"
)

const (
	testKey   = "mergereq-test-key-not-a-secret"
	testToken = "glpat-not-a-real-token-00"
	// opaqueToken is not shaped like any token the redactor knows, so only the literal
	// match keeps it out of a stored error.
	opaqueToken = "plain-secret-value-4711"
)

var epoch = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

// fakeProvider is GitLab's merge request API for one project, scripted per route
// ("GET /merge_requests/7", below the project): the answers, every call, the tokens
// seen, and an optional delay.
type fakeProvider struct {
	mu     sync.Mutex
	routes map[string]answer
	calls  []string
	tokens []string
	delay  time.Duration
}

type answer struct {
	status int
	body   any
}

const projectPath = "/api/v4/projects/group%2Fdemo"

func newFakeProvider(t *testing.T) (*fakeProvider, *http.Client) {
	t.Helper()
	f := &fakeProvider{routes: map[string]answer{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		route := r.Method + " " + strings.TrimPrefix(r.URL.EscapedPath(), projectPath)
		f.calls = append(f.calls, route)
		f.tokens = append(f.tokens, r.Header.Get("PRIVATE-TOKEN"))
		a, ok := f.routes[route]
		delay := f.delay
		f.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		if !ok {
			a = answer{404, map[string]string{"message": "404 Not Found"}}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.status)
		_ = json.NewEncoder(w).Encode(a.body)
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	return f, tools.ProviderClient(rewriteTransport{target})
}

// rewriteTransport sends a request to target instead of its own host.
type rewriteTransport struct{ target *url.URL }

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host, r2.Host = rt.target.Scheme, rt.target.Host, ""
	return http.DefaultTransport.RoundTrip(r2)
}

func (f *fakeProvider) set(route string, status int, body any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[route] = answer{status, body}
}

// request scripts the three reads of one merge request.
func (f *fakeProvider) request(n int, state, sha, pipeline string, unresolved int) {
	mr := map[string]any{"state": state, "sha": sha}
	if state == "merged" {
		mr["merged_at"] = epoch.Add(-time.Minute).Format(time.RFC3339)
	}
	f.set(fmt.Sprintf("GET /merge_requests/%d", n), 200, mr)
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

func (f *fakeProvider) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeProvider) tokensSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tokens...)
}

// fakeReporter records the platform messages the loop sends; deferred makes it say the
// gateway could not be reached.
type fakeReporter struct {
	mu       sync.Mutex
	sent     []message
	deferred bool
}

type message struct{ runID, conversationID, key, text string }

func (r *fakeReporter) SendPlatformMessage(_ context.Context, runID, conversationID, key, text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, message{runID, conversationID, key, text})
	return r.deferred
}

func (r *fakeReporter) messages() []message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]message(nil), r.sent...)
}

func (r *fakeReporter) setDeferred(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deferred = v
}

type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	cipher *project.Cipher
	fake   *fakeProvider
	rep    *fakeReporter
	client *http.Client
	logs   *bytes.Buffer
	sync   *Sync
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := needDB(t)
	cipher, err := project.NewCipher(testKey)
	must(t, err)
	fake, client := newFakeProvider(t)
	e := &env{t: t, pool: pool, cipher: cipher, fake: fake, rep: &fakeReporter{}, client: client, logs: &bytes.Buffer{}}
	e.sync = e.newSync()
	return e
}

// newSync builds a Sync over the environment; a second call is a restart.
func (e *env) newSync() *Sync {
	return &Sync{Pool: e.pool, Projects: &project.Resolver{DB: e.pool, Cipher: e.cipher}, Client: e.client, Reporter: e.rep,
		Log: slog.New(slog.NewTextHandler(e.logs, nil)), Now: func() time.Time { return epoch }}
}

// project stores a GitLab project whose token is sealed for it.
func (e *env) project(token string) *store.Project {
	e.t.Helper()
	return e.projectSealedBy(e.cipher, token)
}

func (e *env) projectSealedBy(c *project.Cipher, token string) *store.Project {
	e.t.Helper()
	p := &store.Project{ID: store.NewID(), Name: "p-" + store.NewID(), RepoURL: "https://gitlab.test/group/demo.git", Provider: "gitlab", DefaultBranch: "main"}
	blob, err := c.Seal(p.ID, token)
	must(e.t, err)
	must(e.t, store.CreateProject(context.Background(), e.pool, p, blob))
	return p
}

// run queues a run of the project; chat makes it a run delegated from a conversation.
func (e *env) run(p *store.Project, chat bool) *store.Run {
	e.t.Helper()
	ctx := context.Background()
	wf := &store.Workflow{Name: "wf " + store.NewID(), Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	must(e.t, store.CreateWorkflow(ctx, e.pool, wf))
	settings := p.Settings()
	run := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: store.TriggerManual,
		ProjectID: &p.ID, Project: &settings}
	if chat {
		ref := store.ConversationRef("conv-" + store.ShortID(store.NewID()))
		run.Trigger, run.TriggerRef = store.TriggerChannel, &ref
	}
	must(e.t, store.CreateRun(ctx, e.pool, run))
	return run
}

// record stores an open record of the run, created age before the test clock.
func (e *env) record(p *store.Project, run *store.Run, number int64, age time.Duration) *store.MergeRequest {
	e.t.Helper()
	mr := &store.MergeRequest{ProjectID: p.ID, RunID: run.ID, Provider: "gitlab", Number: number,
		URL: fmt.Sprintf("https://gitlab.test/group/demo/-/merge_requests/%d", number), SourceBranch: "yuno/abc", TargetBranch: "main",
		Title: "A change", CreatedAt: epoch.Add(-age)}
	ok, err := store.InsertMergeRequest(context.Background(), e.pool, mr)
	must(e.t, err)
	if !ok {
		e.t.Fatalf("record %d not inserted", number)
	}
	return mr
}

func (e *env) get(id string) *store.MergeRequest {
	e.t.Helper()
	mr, err := store.GetMergeRequest(context.Background(), e.pool, id)
	must(e.t, err)
	return mr
}

// T5.20.3 and T5.20.5: a pass takes each open record to what the provider says, reports
// each final state once with its key, leaves the runs that report to no conversation
// alone, and a restart sends nothing again.
func TestTickOnceSettlesRecordsAndReportsOnce(t *testing.T) {
	e := newEnv(t)
	p := e.project(testToken)
	// The project of the failing record has a token no redaction pattern knows.
	opaque := e.project(opaqueToken)
	merged, closed, gone, plain, broken, open := e.run(p, true), e.run(p, true), e.run(p, true), e.run(p, false), e.run(opaque, true), e.run(p, true)
	recMerged := e.record(p, merged, 1, time.Hour)
	recClosed := e.record(p, closed, 2, 2*time.Hour)
	recGone := e.record(p, gone, 3, 3*time.Hour)
	recPlain := e.record(p, plain, 4, 4*time.Hour)
	recBroken := e.record(opaque, broken, 5, 5*time.Hour)
	recOpen := e.record(p, open, 6, 6*time.Hour)

	e.fake.request(1, "merged", "sha-merged", "success", 0)
	e.fake.request(2, "closed", "sha-closed", "", 1)
	e.fake.set("GET /merge_requests/3", 404, map[string]string{"message": "404 Not found"})
	e.fake.request(4, "merged", "sha-plain", "success", 0)
	e.fake.set("GET /merge_requests/5", 500, map[string]string{"message": "boom " + testToken + " " + opaqueToken})
	e.fake.request(6, "opened", "sha-open", "running", 3)

	synced, failed := e.sync.TickOnce(context.Background())
	if synced != 5 || failed != 1 {
		t.Fatalf("synced %d, failed %d; logs:\n%s", synced, failed, e.logs)
	}

	got := e.get(recMerged.ID)
	if got.State != store.MergeRequestMerged || got.CIStatus != store.CIStatusPassed || got.ClosedAt == nil || !got.ClosedAt.Equal(epoch.Add(-time.Minute)) ||
		got.HeadSHA == nil || *got.HeadSHA != "sha-merged" || got.SyncedAt == nil || !got.SyncedAt.Equal(epoch) || got.LastSyncError != nil ||
		got.ReportedState == nil || *got.ReportedState != "merged" || got.MergedBy != nil {
		t.Fatalf("merged = %+v", got)
	}
	got = e.get(recClosed.ID)
	if got.State != store.MergeRequestClosed || got.ClosedAt == nil || !got.ClosedAt.Equal(epoch) || got.UnresolvedThreads == nil || *got.UnresolvedThreads != 1 ||
		got.ReportedState == nil || *got.ReportedState != "closed" {
		t.Fatalf("closed = %+v", got)
	}
	// T5.20.4: a 404 closes the record with the text, and that is a final state too.
	got = e.get(recGone.ID)
	if got.State != store.MergeRequestClosed || got.LastSyncError == nil || *got.LastSyncError != "not found on the provider" || got.ClosedAt == nil ||
		!got.ClosedAt.Equal(epoch) || got.ReportedState == nil || *got.ReportedState != "closed" {
		t.Fatalf("not found = %+v", got)
	}
	got = e.get(recPlain.ID)
	if got.State != store.MergeRequestMerged || got.ReportedState != nil {
		t.Fatalf("a run that reports to no conversation was reported: %+v", got)
	}
	// A provider error is recorded and leaves the record as it was, the token redacted.
	got = e.get(recBroken.ID)
	if got.State != store.MergeRequestOpen || got.LastSyncError == nil || !strings.Contains(*got.LastSyncError, "HTTP 500") ||
		strings.Contains(*got.LastSyncError, testToken) || strings.Contains(*got.LastSyncError, opaqueToken) || got.SyncedAt == nil || got.ReportedState != nil {
		t.Fatalf("broken = %+v", got)
	}
	got = e.get(recOpen.ID)
	if got.State != store.MergeRequestOpen || got.CIStatus != store.CIStatusPending || *got.UnresolvedThreads != 3 || got.ClosedAt != nil || got.ReportedState != nil {
		t.Fatalf("open = %+v", got)
	}

	want := map[string]string{
		"report-" + merged.ID + "-mr-merged": "[platform] Run " + merged.ID + ": merge request !1 was merged.",
		"report-" + closed.ID + "-mr-closed": "[platform] Run " + closed.ID + ": merge request !2 was closed without being merged.",
		"report-" + gone.ID + "-mr-closed":   "[platform] Run " + gone.ID + ": merge request !3 was closed without being merged.",
	}
	sent := e.rep.messages()
	if len(sent) != len(want) {
		t.Fatalf("messages = %+v", sent)
	}
	for _, m := range sent {
		if text, ok := want[m.key]; !ok || text != m.text || !strings.HasPrefix(m.conversationID, "conv-") || m.runID == "" || !strings.Contains(m.key, m.runID) {
			t.Fatalf("message = %+v", m)
		}
	}
	if strings.Contains(e.logs.String(), testToken) || strings.Contains(e.logs.String(), opaqueToken) {
		t.Fatalf("a log line carries the token:\n%s", e.logs)
	}
	for _, tok := range e.fake.tokensSeen() {
		if tok != testToken && tok != opaqueToken {
			t.Fatalf("the provider saw token %q", tok)
		}
	}

	// A restart (a new Sync over the same database): the settled records are not listed,
	// nothing is sent again; the two still open are refreshed.
	before := len(e.fake.seen())
	synced, failed = e.newSync().TickOnce(context.Background())
	if synced != 1 || failed != 1 {
		t.Fatalf("second pass synced %d, failed %d", synced, failed)
	}
	if len(e.rep.messages()) != 3 {
		t.Fatalf("a restart re-sent: %+v", e.rep.messages())
	}
	if n := len(e.fake.seen()) - before; n != 4 { // one read for the broken record, three for the open one
		t.Fatalf("second pass made %d provider calls: %v", n, e.fake.seen()[before:])
	}
}

// Rule 4: a send that was deferred (the gateway unreachable) leaves reported_state null and
// is sent again, under the same key, by a later pass, and only until it goes out.
func TestDeferredReportIsSentAgain(t *testing.T) {
	e := newEnv(t)
	p := e.project(testToken)
	run := e.run(p, true)
	rec := e.record(p, run, 1, time.Hour)
	e.fake.request(1, "merged", "sha", "success", 0)
	e.rep.setDeferred(true)

	e.sync.TickOnce(context.Background())
	if got := e.get(rec.ID); got.State != store.MergeRequestMerged || got.ReportedState != nil {
		t.Fatalf("after a deferred send = %+v", got)
	}
	e.sync.TickOnce(context.Background()) // the record is final; the catch-up sends again
	if sent := e.rep.messages(); len(sent) != 2 || sent[0].key != sent[1].key || sent[0].key != "report-"+run.ID+"-mr-merged" {
		t.Fatalf("messages = %+v", sent)
	}
	e.rep.setDeferred(false)
	e.sync.TickOnce(context.Background())
	if got := e.get(rec.ID); got.ReportedState == nil || *got.ReportedState != "merged" || len(e.rep.messages()) != 3 {
		t.Fatalf("after the gateway came back = %+v, %d messages", got, len(e.rep.messages()))
	}
	e.sync.TickOnce(context.Background())
	e.newSync().TickOnce(context.Background())
	if len(e.rep.messages()) != 3 {
		t.Fatalf("sent after it was reported: %+v", e.rep.messages())
	}
}

// T5.20.9 and rule 8: a deleted project's record still syncs while its token row exists;
// a token that cannot be read is recorded on the record and the record skipped.
func TestDeletedProjectSyncsAndUnreadableTokenIsRecorded(t *testing.T) {
	e := newEnv(t)
	deleted := e.project(testToken)
	runA := e.run(deleted, false)
	recA := e.record(deleted, runA, 1, time.Hour)
	must(t, store.DeleteProject(context.Background(), e.pool, deleted.ID))

	other, err := project.NewCipher("another-key-not-a-secret")
	must(t, err)
	rekeyed := e.projectSealedBy(other, testToken)
	runB := e.run(rekeyed, false)
	recB := e.record(rekeyed, runB, 2, 2*time.Hour)

	e.fake.request(1, "merged", "sha", "success", 0)
	e.fake.request(2, "merged", "sha", "success", 0)
	synced, failed := e.sync.TickOnce(context.Background())
	if synced != 1 || failed != 1 {
		t.Fatalf("synced %d, failed %d", synced, failed)
	}
	if got := e.get(recA.ID); got.State != store.MergeRequestMerged {
		t.Fatalf("the deleted project's record did not sync: %+v", got)
	}
	got := e.get(recB.ID)
	if got.State != store.MergeRequestOpen || got.LastSyncError == nil || !strings.Contains(*got.LastSyncError, project.ErrCannotRead.Error()) || got.SyncedAt == nil {
		t.Fatalf("unreadable token = %+v", got)
	}
	for _, c := range e.fake.seen() {
		if strings.Contains(c, "/merge_requests/2") {
			t.Fatalf("the provider was asked for a record whose token cannot be read: %v", e.fake.seen())
		}
	}
}

// Rule 3: at most 50 per pass, only the last 14 days, the oldest sync first; one slow or
// failing provider call never stops the pass.
func TestPassLimitWindowAndTimeout(t *testing.T) {
	e := newEnv(t)
	p := e.project(testToken)
	for n := int64(1); n <= 52; n++ {
		e.record(p, e.run(p, false), n, time.Hour)
		e.fake.request(int(n), "opened", "sha", "running", 0)
	}
	stale := e.record(p, e.run(p, false), 99, 15*24*time.Hour)
	e.fake.request(99, "merged", "sha", "success", 0)

	synced, failed := e.sync.TickOnce(context.Background())
	if synced != PassLimit || failed != 0 {
		t.Fatalf("synced %d, failed %d, want %d", synced, failed, PassLimit)
	}
	if got := e.get(stale.ID); got.State != store.MergeRequestOpen || got.SyncedAt != nil {
		t.Fatalf("a record older than 14 days was synced: %+v", got)
	}
	// The two left over come first in the next pass (never synced), then the oldest sync.
	e.fake.mu.Lock()
	e.fake.calls = nil
	e.fake.mu.Unlock()
	e.sync.TickOnce(context.Background())
	first := map[string]bool{}
	for _, c := range e.fake.seen()[:6] {
		first[c] = true
	}
	if !first["GET /merge_requests/51"] && !first["GET /merge_requests/52"] {
		t.Fatalf("the unsynced records were not first: %v", e.fake.seen()[:6])
	}

	// A provider that does not answer within the timeout is a recorded failure.
	e2 := newEnv(t)
	p2 := e2.project(testToken)
	slow := e2.record(p2, e2.run(p2, false), 1, time.Hour)
	e2.fake.delay = 2 * time.Second
	s := e2.newSync()
	s.Timeout = 50 * time.Millisecond
	start := time.Now()
	synced, failed = s.TickOnce(context.Background())
	if synced != 0 || failed != 1 || time.Since(start) > time.Second {
		t.Fatalf("slow provider: synced %d, failed %d after %s", synced, failed, time.Since(start))
	}
	if got := e2.get(slow.ID); got.LastSyncError == nil || !strings.Contains(*got.LastSyncError, "timed out") {
		t.Fatalf("slow = %+v", got)
	}
}

// Rule 3, GitHub's partial answer: a refused GraphQL call leaves the thread count null,
// records the text, and still takes the state and the checks.
func TestPartialStatusRecordsTheError(t *testing.T) {
	e := newEnv(t)
	p := e.project(testToken)
	run := e.run(p, false)
	rec := e.record(p, run, 5, time.Hour)
	if _, err := e.pool.Exec(context.Background(), `UPDATE merge_requests SET provider = 'github', unresolved_threads = 4 WHERE id = $1`, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE runs SET project_snapshot = jsonb_set(project_snapshot, '{provider}', '"github"') WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	// GitHub's API for a github project on another host is <host>/api/v3/repos/group/demo,
	// its GraphQL <host>/api/graphql.
	g := &githubFake{t: t}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	target, _ := url.Parse(g.srv.URL)
	s := e.newSync()
	s.Client = tools.ProviderClient(rewriteTransport{target})

	synced, failed := s.TickOnce(context.Background())
	if synced != 1 || failed != 0 {
		t.Fatalf("synced %d, failed %d; logs:\n%s", synced, failed, e.logs)
	}
	got := e.get(rec.ID)
	if got.UnresolvedThreads != nil || got.LastSyncError == nil || !strings.Contains(*got.LastSyncError, "review threads unknown") ||
		got.CIStatus != store.CIStatusPassed || got.State != store.MergeRequestOpen || got.HeadSHA == nil || *got.HeadSHA != "feedface" {
		t.Fatalf("after a GraphQL refusal = %+v", got)
	}
	// The fake's GraphQL answer carries the project's own token (testToken, a glpat- value
	// that is also this project's real one): the stored text must have lost it.
	if strings.Contains(*got.LastSyncError, testToken) || strings.Contains(*got.LastSyncError, "glpat-") || !strings.Contains(*got.LastSyncError, obs.Redacted) {
		t.Fatalf("the token reached the error, or was not redacted: %q", *got.LastSyncError)
	}
	if strings.Contains(e.logs.String(), testToken) {
		t.Fatalf("a log line carries the token:\n%s", e.logs)
	}
	// The next sync that GraphQL answers clears the error and fills the count.
	g.graphqlOK = true
	if _, failed := s.TickOnce(context.Background()); failed != 0 {
		t.Fatal("second pass failed")
	}
	got = e.get(rec.ID)
	if got.UnresolvedThreads == nil || *got.UnresolvedThreads != 1 || got.LastSyncError != nil {
		t.Fatalf("after GraphQL answered = %+v", got)
	}
}

// githubFake answers the pull request, check runs, status and GraphQL calls of one pull.
type githubFake struct {
	t         *testing.T
	srv       *httptest.Server
	graphqlOK bool
}

func (g *githubFake) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	write := func(status int, v any) { w.WriteHeader(status); _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/graphql":
		if !g.graphqlOK {
			// The provider echoes the token the request carried, as an upstream proxy might.
			write(200, map[string]any{"errors": []map[string]string{{"message": "Resource not accessible by personal access token " + testToken}}})
			return
		}
		write(200, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
			"reviewThreads": map[string]any{"nodes": []map[string]bool{{"isResolved": false}, {"isResolved": true}}}}}}})
	case r.URL.Path == "/api/v3/repos/group/demo/pulls/5":
		write(200, map[string]any{"state": "open", "merged": false, "head": map[string]string{"sha": "feedface"}})
	case r.URL.Path == "/api/v3/repos/group/demo/commits/feedface/check-runs":
		write(200, map[string]any{"total_count": 1, "check_runs": []map[string]string{{"status": "completed", "conclusion": "success"}}})
	case r.URL.Path == "/api/v3/repos/group/demo/commits/feedface/status":
		write(200, map[string]any{"state": "pending", "total_count": 0, "statuses": []any{}})
	default:
		g.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		write(404, map[string]string{"message": "Not Found"})
	}
}

// SyncOne is the handler's refresh: the record afterwards, provider errors on the record
// and not returned, a record that is not open untouched, an unknown id ErrNotFound.
func TestSyncOne(t *testing.T) {
	e := newEnv(t)
	p := e.project(testToken)
	rec := e.record(p, e.run(p, false), 1, time.Hour)
	e.fake.request(1, "opened", "abc", "failed", 2)

	got, err := e.sync.SyncOne(context.Background(), rec.ID)
	if err != nil || got.CIStatus != store.CIStatusFailed || *got.UnresolvedThreads != 2 || got.SyncedAt == nil || got.State != store.MergeRequestOpen {
		t.Fatalf("sync = %+v %v", got, err)
	}
	e.fake.set("GET /merge_requests/1", 502, map[string]string{"message": "bad gateway"})
	got, err = e.sync.SyncOne(context.Background(), rec.ID)
	if err != nil || got.LastSyncError == nil || !strings.Contains(*got.LastSyncError, "HTTP 502") || got.CIStatus != store.CIStatusFailed {
		t.Fatalf("provider error = %+v %v", got, err)
	}

	merged := e.record(p, e.run(p, false), 2, time.Hour)
	if _, err := store.MarkMergeRequestMerged(context.Background(), e.pool, merged.ID, "operator", epoch); err != nil {
		t.Fatal(err)
	}
	before := len(e.fake.seen())
	got, err = e.sync.SyncOne(context.Background(), merged.ID)
	if err != nil || got.State != store.MergeRequestMerged || len(e.fake.seen()) != before {
		t.Fatalf("a merged record was refreshed: %+v %v", got, err)
	}
	if _, err := e.sync.SyncOne(context.Background(), "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown id: %v", err)
	}
}

// Rule 5 and 4: MarkMerged records the merge and reports it once; a record a sync merged
// first is returned as it is and not reported twice.
func TestMarkMerged(t *testing.T) {
	e := newEnv(t)
	p := e.project(testToken)
	run := e.run(p, true)
	rec := e.record(p, run, 1, time.Hour)

	got, err := e.sync.MarkMerged(context.Background(), rec.ID, "operator")
	if err != nil || got.State != store.MergeRequestMerged || got.MergedBy == nil || *got.MergedBy != "operator" || got.ClosedAt == nil ||
		!got.ClosedAt.Equal(epoch) || got.ReportedState == nil || *got.ReportedState != "merged" {
		t.Fatalf("merged = %+v %v", got, err)
	}
	if sent := e.rep.messages(); len(sent) != 1 || sent[0].key != "report-"+run.ID+"-mr-merged" || sent[0].text != "[platform] Run "+run.ID+": merge request !1 was merged." {
		t.Fatalf("messages = %+v", sent)
	}
	if got, err := e.sync.MarkMerged(context.Background(), rec.ID, "operator"); err != nil || got.State != store.MergeRequestMerged || len(e.rep.messages()) != 1 {
		t.Fatalf("second merge = %+v %v, %d messages", got, err, len(e.rep.messages()))
	}

	// A sync saw the merge first: its report stands, merged_by stays empty.
	race := e.record(p, e.run(p, true), 2, time.Hour)
	e.fake.request(2, "merged", "sha", "success", 0)
	if _, err := e.sync.SyncOne(context.Background(), race.ID); err != nil {
		t.Fatal(err)
	}
	got, err = e.sync.MarkMerged(context.Background(), race.ID, "operator")
	if err != nil || got.MergedBy != nil || len(e.rep.messages()) != 2 {
		t.Fatalf("raced merge = %+v %v, %d messages", got, err, len(e.rep.messages()))
	}
}

// Rule 5: after a 2xx from the merge call the record becomes merged only when the provider
// reports the request merged; any other answer leaves it open with the text on the record,
// unreported, for the loop to settle.
func TestConfirmMerged(t *testing.T) {
	e := newEnv(t)
	p := e.project(testToken)
	ctx := context.Background()
	confirm := func(run *store.Run, rec *store.MergeRequest) *store.MergeRequest {
		t.Helper()
		pc, err := e.sync.Projects.ForRunID(ctx, run.ID)
		must(t, err)
		got, err := e.sync.ConfirmMerged(ctx, rec, pc, "operator")
		must(t, err)
		return got
	}

	run := e.run(p, true)
	rec := e.record(p, run, 1, time.Hour)
	e.fake.request(1, "merged", "sha", "success", 0)
	got := confirm(run, rec)
	if got.State != store.MergeRequestMerged || got.MergedBy == nil || *got.MergedBy != "operator" || got.ClosedAt == nil || !got.ClosedAt.Equal(epoch) ||
		got.ReportedState == nil || got.LastSyncError != nil || len(e.rep.messages()) != 1 {
		t.Fatalf("provider says merged = %+v, %d messages", got, len(e.rep.messages()))
	}

	open := func(number int64, script func(), want string) {
		t.Helper()
		run := e.run(p, true)
		rec := e.record(p, run, number, time.Hour)
		script()
		got := confirm(run, rec)
		if got.State != store.MergeRequestOpen || got.MergedBy != nil || got.ClosedAt != nil || got.ReportedState != nil || got.SyncedAt == nil ||
			got.LastSyncError == nil || !strings.HasPrefix(*got.LastSyncError, want) || strings.Contains(*got.LastSyncError, testToken) {
			t.Fatalf("record = %+v (error %v), want open with %q", got, got.LastSyncError, want)
		}
		if n := len(e.rep.messages()); n != 1 {
			t.Fatalf("%d messages: an open record was reported", n)
		}
	}
	open(2, func() { e.fake.request(2, "opened", "sha", "running", 0) },
		"the provider accepted the merge but reports the request as open")
	open(3, func() { e.fake.request(3, "closed", "sha", "failed", 0) },
		"the provider accepted the merge but reports the request as closed")
	open(4, func() { e.fake.set("GET /merge_requests/4", 404, map[string]string{"message": "404"}) },
		"the provider accepted the merge but the request could not be read back: not found on the provider")
	open(5, func() {
		e.fake.set("GET /merge_requests/5", 502, map[string]string{"message": "bad gateway " + testToken})
	},
		"the provider accepted the merge but the request could not be read back: gitlab: HTTP 502")
}

// A GitHub pull request is "pull request #<n>".
func TestReportText(t *testing.T) {
	mr := &store.MergeRequest{RunID: "r1", Provider: "github", Number: 12, State: store.MergeRequestMerged}
	if got := ReportText(mr); got != "[platform] Run r1: pull request #12 was merged." {
		t.Fatal(got)
	}
	mr.State = store.MergeRequestClosed
	if got := ReportText(mr); got != "[platform] Run r1: pull request #12 was closed without being merged." {
		t.Fatal(got)
	}
	mr.Provider = "gitlab"
	if got := ReportText(mr); got != "[platform] Run r1: merge request !12 was closed without being merged." {
		t.Fatal(got)
	}
}

// cleanError keeps one redacted line.
func TestCleanError(t *testing.T) {
	got := cleanError("gitlab: HTTP 500: boom "+opaqueToken+" and "+testToken+"\nsecond line", opaqueToken)
	if strings.Contains(got, opaqueToken) || strings.Contains(got, testToken) || strings.Contains(got, "second") {
		t.Fatal(got)
	}
	if long := cleanError(strings.Repeat("é", 400), ""); len(long) > errorMax || !strings.HasSuffix(long, "é") {
		t.Fatalf("long error: %d bytes", len(long))
	}
}

// The column behind last_sync_error refuses invalid UTF-8 and NUL bytes, and a write that
// fails ends the pass: cleanError always returns text that can be stored, whatever the
// provider's body held and wherever the cut falls.
func TestCleanErrorAlwaysStorable(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"nul bytes":              {"gitlab: HTTP 500: a\x00b\x00", "gitlab: HTTP 500: ab"},
		"invalid bytes":          {"gitlab: HTTP 500: bad \xff\xfe gateway", "gitlab: HTTP 500: bad  gateway"},
		"a rune cut by the host": {"gitlab: HTTP 500: caf\xc3", "gitlab: HTTP 500: caf"},
		"invalid after a nul":    {"x\xc3\x00\xa9", "x"},
	} {
		if got := cleanError(tc.in, ""); got != tc.want || !utf8.ValidString(got) {
			t.Fatalf("%s: %q, want %q", name, got, tc.want)
		}
	}
	// Every alignment of a character across the cap, for 2, 3 and 4 byte characters, with
	// the invalid bytes of a body cut elsewhere in front of it.
	for _, ch := range []string{"é", "€", "😀"} {
		for pad := errorMax - len(ch) - 1; pad <= errorMax+1; pad++ {
			in := strings.Repeat("a", pad) + ch + strings.Repeat("z", 20)
			got := cleanError(in, "")
			if !utf8.ValidString(got) || len(got) > errorMax || !strings.HasPrefix(in, got) || strings.ContainsRune(got, utf8.RuneError) {
				t.Fatalf("%q at %d: %d bytes, valid %v", ch, pad, len(got), utf8.ValidString(got))
			}
			if want := min(len(in), errorMax); len(got) < want-(len(ch)-1) {
				t.Fatalf("%q at %d: cut too early, %d of %d bytes", ch, pad, len(got), want)
			}
		}
	}
	// The redaction sees the cleaned text: a token split by a stray byte is still found.
	if got := cleanError("boom "+opaqueToken[:6]+"\x00"+opaqueToken[6:], opaqueToken); strings.Contains(got, opaqueToken[:6]) {
		t.Fatalf("token split by a NUL survived: %q", got)
	}
}

// A provider's error body cut at 300 bytes in the middle of a character used to leave
// invalid UTF-8 that the database refused, so the pass ended at that record and began
// there again on every tick. The text is written, and the pass goes on to the next record.
func TestTickOnceWritesAnErrorCutInsideACharacterAndGoesOn(t *testing.T) {
	e := newEnv(t)
	p := e.project(testToken)
	stuck := e.record(p, e.run(p, false), 1, 2*time.Hour) // the longest-waiting record sorts first
	next := e.record(p, e.run(p, false), 2, time.Hour)
	// statusFailure cuts the raw body {"message":"..."} at byte 300: put the first byte of
	// the two-byte é at offset 299 so that the cut leaves half of it behind.
	const prefix = `{"message":"`
	e.fake.set("GET /merge_requests/1", 500, map[string]string{"message": strings.Repeat("a", 300-1-len(prefix)) + "é end"})
	e.fake.request(2, "opened", "sha-next", "running", 1)

	synced, failed := e.sync.TickOnce(context.Background())
	if synced != 1 || failed != 1 {
		t.Fatalf("synced %d, failed %d; logs:\n%s", synced, failed, e.logs)
	}
	got := e.get(stuck.ID)
	if got.State != store.MergeRequestOpen || got.LastSyncError == nil || !utf8.ValidString(*got.LastSyncError) ||
		!strings.Contains(*got.LastSyncError, "HTTP 500") || strings.Contains(*got.LastSyncError, "é") || got.SyncedAt == nil {
		t.Fatalf("stuck record = %+v", got)
	}
	if !strings.HasSuffix(*got.LastSyncError, "a") {
		t.Fatalf("the text does not end where the character began: %q", (*got.LastSyncError)[max(0, len(*got.LastSyncError)-6):])
	}
	// The next record was refreshed in the same pass, and the next pass does not stall.
	if n := e.get(next.ID); n.SyncedAt == nil || n.HeadSHA == nil || *n.HeadSHA != "sha-next" {
		t.Fatalf("the pass stopped at the stuck record: %+v\nlogs:\n%s", n, e.logs)
	}
	if strings.Contains(e.logs.String(), "sync a merge request failed") {
		t.Fatalf("a database error ended the pass:\n%s", e.logs)
	}
}

// The loop runs a pass at once and every interval, and stops with its context.
func TestRunStopsWithContext(t *testing.T) {
	e := newEnv(t)
	p := e.project(testToken)
	rec := e.record(p, e.run(p, false), 1, time.Hour)
	e.fake.request(1, "opened", "abc", "running", 0)
	s := e.newSync()
	s.Interval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(e.fake.seen()) < 6 && time.Now().Before(deadline) { // two passes of three calls
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop with its context")
	}
	if len(e.fake.seen()) < 6 || e.get(rec.ID).SyncedAt == nil {
		t.Fatalf("the loop did not run twice: %v", e.fake.seen())
	}
}

// Backfill (rule 7) through the sync: idempotent, logged.
func TestBackfill(t *testing.T) {
	e := newEnv(t)
	p := e.project(testToken)
	run := e.run(p, false)
	at := epoch.Add(-time.Hour)
	must(t, store.InsertToolCall(context.Background(), e.pool, &store.ToolCall{RunID: run.ID, ToolUseID: "a", ToolName: "open_merge_request", Status: store.ToolExecuted,
		Input:  json.RawMessage(`{"title":"Old one"}`),
		Output: json.RawMessage(`{"number":3,"url":"https://gitlab.test/group/demo/-/merge_requests/3","branch":"yuno/abc","provider":"gitlab"}`), StartedAt: &at, FinishedAt: &at}))
	n, err := e.sync.Backfill(context.Background())
	if err != nil || n != 1 || !strings.Contains(e.logs.String(), "merge requests backfilled") {
		t.Fatalf("backfill = %d %v\n%s", n, err, e.logs)
	}
	if n, err := e.sync.Backfill(context.Background()); err != nil || n != 0 {
		t.Fatalf("second backfill = %d %v", n, err)
	}
	// The first pass settles it.
	e.fake.request(3, "merged", "sha", "success", 0)
	if synced, _ := e.sync.TickOnce(context.Background()); synced != 1 {
		t.Fatalf("pass after the backfill synced %d", synced)
	}
	if mr, _ := store.MergeRequestByRun(context.Background(), e.pool, run.ID); mr == nil || mr.State != store.MergeRequestMerged || mr.Title != "Old one" {
		t.Fatalf("record = %+v", mr)
	}
}
