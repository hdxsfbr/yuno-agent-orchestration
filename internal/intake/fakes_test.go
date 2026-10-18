package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/engine"
	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/tools"
)

const (
	testKey   = "intake-test-key-not-a-secret"
	testToken = "glpat-not-a-real-token-00"
	// opaqueToken is not shaped like any token the redactor knows, so only the literal
	// replacement keeps it out of a stored error.
	opaqueToken = "plain-secret-value-4711"
	publicBase  = "https://yuno.test"
)

// epoch is the fixed clock of the tests (UTC).
var epoch = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// clock is the injected clock; tests move it.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// ---- the log ---------------------------------------------------------------------------

// logRecorder is a slog handler that keeps what it is given, message and attributes.
type logRecorder struct {
	mu    sync.Mutex
	lines *[]logLine
	attrs []slog.Attr
}

type logLine struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

func newLogRecorder() (*slog.Logger, *logRecorder) {
	r := &logRecorder{lines: &[]logLine{}}
	return slog.New(r), r
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, rec slog.Record) error {
	line := logLine{level: rec.Level, msg: rec.Message, attrs: map[string]string{}}
	for _, a := range r.attrs {
		line.attrs[a.Key] = a.Value.String()
	}
	rec.Attrs(func(a slog.Attr) bool {
		line.attrs[a.Key] = a.Value.String()
		return true
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	*r.lines = append(*r.lines, line)
	return nil
}

func (r *logRecorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logRecorder{lines: r.lines, attrs: append(append([]slog.Attr(nil), r.attrs...), attrs...)}
}

func (r *logRecorder) WithGroup(string) slog.Handler { return r }

// messages lists every message logged so far.
func (r *logRecorder) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(*r.lines))
	for i, l := range *r.lines {
		out[i] = l.msg
	}
	return out
}

// find returns the first line whose message contains text.
func (r *logRecorder) find(text string) (logLine, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range *r.lines {
		if strings.Contains(l.msg, text) {
			return l, true
		}
	}
	return logLine{}, false
}

// everything is every message and attribute joined, to search for text that must not be there.
func (r *logRecorder) everything() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, l := range *r.lines {
		b.WriteString(l.msg + "\n")
		for k, v := range l.attrs {
			b.WriteString(k + "=" + v + "\n")
		}
	}
	return b.String()
}

// ---- the issue tracker -----------------------------------------------------------------

// tracker is a fake of one repository's issues on GitLab or GitHub: it keeps the issues
// with their labels, the repository's labels and every comment, and serves the calls rules
// 2, 3 and 5 make. A call can be made to fail; a label that does not exist on the
// repository is refused by a swap, as GitHub does on some paths (D-053).
type tracker struct {
	mu     sync.Mutex
	github bool
	prefix string
	issues []*fakeIssue
	labels map[string]bool
	calls  []string
	bodies []map[string]any
	notes  []note
	fail   map[string]failure
	delay  time.Duration
	tokens []string
	srv    *httptest.Server
}

type fakeIssue struct {
	n      int64
	title  string
	body   string
	labels map[string]bool
	pr     bool
}

type note struct {
	issue int64
	body  string
}

type failure struct {
	status  int
	message string
}

func newTracker(t *testing.T, github bool) *tracker {
	t.Helper()
	tr := &tracker{github: github, labels: map[string]bool{}, fail: map[string]failure{}, prefix: "/api/v4/projects/group%2Fdemo"}
	if github {
		tr.prefix = "/repos/owner/demo"
	}
	tr.srv = httptest.NewServer(http.HandlerFunc(tr.serve))
	t.Cleanup(tr.srv.Close)
	return tr
}

// addIssue opens an issue carrying labels; the numbers count from 1.
func (tr *tracker) addIssue(title, body string, labels ...string) *fakeIssue {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	i := &fakeIssue{n: int64(len(tr.issues) + 1), title: title, body: body, labels: map[string]bool{}}
	for _, l := range labels {
		i.labels[l] = true
		tr.labels[l] = true
	}
	tr.issues = append(tr.issues, i)
	return i
}

// addPullRequest opens a pull request carrying labels (GitHub lists them as issues).
func (tr *tracker) addPullRequest(title string, labels ...string) *fakeIssue {
	i := tr.addIssue(title, "diff", labels...)
	tr.mu.Lock()
	i.pr = true
	tr.mu.Unlock()
	return i
}

func (tr *tracker) issue(n int64) *fakeIssue {
	for _, i := range tr.issues {
		if i.n == n {
			return i
		}
	}
	return nil
}

// has reports whether issue n carries the label.
func (tr *tracker) has(n int64, label string) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.issue(n).labels[label]
}

// labelsOf lists the labels of issue n, sorted.
func (tr *tracker) labelsOf(n int64) []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var out []string
	for l := range tr.issue(n).labels {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// relabel is what a person does on the provider: remove some labels, add others.
func (tr *tracker) relabel(n int64, remove []string, add ...string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	i := tr.issue(n)
	for _, l := range remove {
		delete(i.labels, l)
	}
	for _, l := range add {
		i.labels[l] = true
		tr.labels[l] = true
	}
}

// commentsOn lists the comments on issue n in order.
func (tr *tracker) commentsOn(n int64) []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var out []string
	for _, c := range tr.notes {
		if c.issue == n {
			out = append(out, c.body)
		}
	}
	return out
}

// failOn makes every call "METHOD /path" (below the API base) answer status with message
// until clearFail.
func (tr *tracker) failOn(route string, status int, message string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.fail[route] = failure{status, message}
}

func (tr *tracker) clearFail() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.fail = map[string]failure{}
}

// seen lists the calls as "METHOD /path", in order.
func (tr *tracker) seen() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.calls...)
}

// count is how many calls "METHOD /path-prefix" were made.
func (tr *tracker) count(prefix string) int {
	n := 0
	for _, c := range tr.seen() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (tr *tracker) reset() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.calls, tr.bodies = nil, nil
}

func (tr *tracker) tokensSeen() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.tokens...)
}

func (tr *tracker) reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (tr *tracker) serve(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.EscapedPath(), tr.prefix)
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	route := r.Method + " " + p
	tr.mu.Lock()
	tr.calls = append(tr.calls, route)
	tr.bodies = append(tr.bodies, body)
	tr.tokens = append(tr.tokens, r.Header.Get("PRIVATE-TOKEN")+r.Header.Get("Authorization"))
	f, failing := tr.fail[route]
	delay := tr.delay
	tr.mu.Unlock()
	if delay > 0 && r.Method == http.MethodGet && p == "/issues" {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if failing {
		tr.reply(w, f.status, map[string]string{"message": f.message})
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.github {
		tr.serveGitHub(w, r, p, body)
	} else {
		tr.serveGitLab(w, r, p, body)
	}
}

// matching lists the open issues that carry the label, oldest first, at most per_page.
func (tr *tracker) matching(r *http.Request) []*fakeIssue {
	label := r.URL.Query().Get("labels")
	limit, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	var out []*fakeIssue
	for _, i := range tr.issues {
		if i.labels[label] && (limit == 0 || len(out) < limit) {
			out = append(out, i)
		}
	}
	return out
}

func (tr *tracker) serveGitLab(w http.ResponseWriter, r *http.Request, p string, body map[string]any) {
	switch {
	case r.Method == http.MethodGet && p == "/labels":
		out := []map[string]any{}
		for l := range tr.labels {
			if strings.Contains(l, r.URL.Query().Get("search")) {
				out = append(out, map[string]any{"name": l})
			}
		}
		tr.reply(w, 200, out)
	case r.Method == http.MethodPost && p == "/labels":
		name, _ := body["name"].(string)
		if tr.labels[name] {
			tr.reply(w, 409, map[string]string{"message": "Label already exists"})
			return
		}
		tr.labels[name] = true
		tr.reply(w, 201, map[string]any{"name": name})
	case r.Method == http.MethodGet && p == "/issues":
		out := []map[string]any{}
		for _, i := range tr.matching(r) {
			out = append(out, map[string]any{"iid": i.n, "title": i.title, "description": i.body,
				"web_url": fmt.Sprintf("https://gitlab.test/group/demo/-/issues/%d", i.n)})
		}
		tr.reply(w, 200, out)
	case r.Method == http.MethodPut && strings.HasPrefix(p, "/issues/"):
		i := tr.issue(parseNumber(strings.TrimPrefix(p, "/issues/")))
		if i == nil {
			tr.reply(w, 404, map[string]string{"message": "404 Not found"})
			return
		}
		add, _ := body["add_labels"].(string)
		remove, _ := body["remove_labels"].(string)
		for _, l := range splitLabels(add) {
			if !tr.labels[l] {
				tr.reply(w, 422, map[string]string{"message": "label " + l + " does not exist"})
				return
			}
		}
		for _, l := range splitLabels(add) {
			i.labels[l] = true
		}
		for _, l := range splitLabels(remove) {
			delete(i.labels, l)
		}
		tr.reply(w, 200, map[string]any{"iid": i.n})
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/issues/") && strings.HasSuffix(p, "/notes"):
		n := parseNumber(strings.TrimSuffix(strings.TrimPrefix(p, "/issues/"), "/notes"))
		text, _ := body["body"].(string)
		tr.notes = append(tr.notes, note{n, text})
		tr.reply(w, 201, map[string]any{"id": len(tr.notes)})
	default:
		tr.reply(w, 404, map[string]string{"message": "404 Not Found"})
	}
}

func (tr *tracker) serveGitHub(w http.ResponseWriter, r *http.Request, p string, body map[string]any) {
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/labels/"):
		name, _ := url.PathUnescape(strings.TrimPrefix(p, "/labels/"))
		if !tr.labels[name] {
			tr.reply(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		tr.reply(w, 200, map[string]any{"name": name})
	case r.Method == http.MethodPost && p == "/labels":
		name, _ := body["name"].(string)
		if tr.labels[name] {
			tr.reply(w, 422, map[string]any{"message": "Validation Failed",
				"errors": []map[string]string{{"resource": "Label", "code": "already_exists", "field": "name"}}})
			return
		}
		tr.labels[name] = true
		tr.reply(w, 201, map[string]any{"name": name})
	case r.Method == http.MethodGet && p == "/issues":
		out := []map[string]any{}
		for _, i := range tr.matching(r) {
			e := map[string]any{"number": i.n, "title": i.title, "body": i.body,
				"html_url": fmt.Sprintf("https://github.com/owner/demo/issues/%d", i.n)}
			if i.pr {
				e["pull_request"] = map[string]any{"url": "x"}
			}
			out = append(out, e)
		}
		tr.reply(w, 200, out)
	case r.Method == http.MethodDelete && strings.Contains(p, "/labels/"):
		n, name, _ := strings.Cut(strings.TrimPrefix(p, "/issues/"), "/labels/")
		label, _ := url.PathUnescape(name)
		i := tr.issue(parseNumber(n))
		if i == nil || !i.labels[label] {
			tr.reply(w, 404, map[string]string{"message": "Label does not exist"})
			return
		}
		delete(i.labels, label)
		tr.reply(w, 200, []map[string]any{})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/labels") && strings.HasPrefix(p, "/issues/"):
		i := tr.issue(parseNumber(strings.TrimSuffix(strings.TrimPrefix(p, "/issues/"), "/labels")))
		if i == nil {
			tr.reply(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		raw, _ := body["labels"].([]any)
		for _, l := range raw {
			if name, _ := l.(string); !tr.labels[name] {
				tr.reply(w, 422, map[string]string{"message": "Label " + name + " does not exist"})
				return
			}
		}
		for _, l := range raw {
			i.labels[l.(string)] = true
		}
		tr.reply(w, 200, []map[string]any{})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/comments") && strings.HasPrefix(p, "/issues/"):
		n := parseNumber(strings.TrimSuffix(strings.TrimPrefix(p, "/issues/"), "/comments"))
		text, _ := body["body"].(string)
		tr.notes = append(tr.notes, note{n, text})
		tr.reply(w, 201, map[string]any{"id": len(tr.notes)})
	default:
		tr.reply(w, 404, map[string]string{"message": "Not Found"})
	}
}

func parseNumber(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func splitLabels(s string) []string {
	var out []string
	for _, l := range strings.Split(s, ",") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// routeTransport sends a request to the fake that stands for its host.
type routeTransport struct {
	mu    sync.Mutex
	hosts map[string]*url.URL
}

func (rt *routeTransport) route(host string, tr *tracker) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	u, _ := url.Parse(tr.srv.URL)
	rt.hosts[host] = u
}

func (rt *routeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	target := rt.hosts[r.URL.Host]
	rt.mu.Unlock()
	if target == nil {
		return nil, fmt.Errorf("no fake for host %q", r.URL.Host)
	}
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host, r2.Host = target.Scheme, target.Host, ""
	return http.DefaultTransport.RoundTrip(r2)
}

// ---- the engine -------------------------------------------------------------------------

// fakeEngine is the part of the engine the loop uses: it creates the run the real trigger
// transaction would (the project's snapshot, the input, the reference) through
// store.CreateRun, so the live-run index is the real one. refuse scripts a refusal per
// request; before runs ahead of the insert (to play another process winning a race).
type fakeEngine struct {
	pool *pgxpool.Pool

	mu       sync.Mutex
	requests []engine.TriggerRequest
	refuse   func(engine.TriggerRequest) error
	before   func(engine.TriggerRequest)
}

func (f *fakeEngine) Trigger(ctx context.Context, req engine.TriggerRequest) (*store.Run, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	refuse, before := f.refuse, f.before
	f.mu.Unlock()
	if before != nil {
		before(req)
	}
	if refuse != nil {
		if err := refuse(req); err != nil {
			return nil, err
		}
	}
	wf, err := store.GetWorkflow(ctx, f.pool, req.WorkflowID)
	if err != nil {
		return nil, fmt.Errorf("workflow %s: %w", req.WorkflowID, err)
	}
	p, err := store.GetProject(ctx, f.pool, req.ProjectID)
	if err != nil {
		return nil, err
	}
	settings := p.Settings()
	ref := req.TriggerRef
	run := &store.Run{WorkflowID: wf.ID, WorkflowVersion: wf.Version, WorkflowSnapshot: wf.Definition, Trigger: req.Trigger,
		TriggerRef: &ref, Input: req.Input, ProjectID: &p.ID, Project: &settings}
	if err := store.CreateRun(ctx, f.pool, run); err != nil {
		return nil, fmt.Errorf("create run: %w", err)
	}
	return store.GetRun(ctx, f.pool, run.ID)
}

func (f *fakeEngine) triggered() []engine.TriggerRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]engine.TriggerRequest(nil), f.requests...)
}

// ---- the environment -------------------------------------------------------------------

// env is one project with intake enabled on a fake repository, and a loop aimed at it.
type env struct {
	t       *testing.T
	pool    *pgxpool.Pool
	cipher  *project.Cipher
	tr      *tracker
	eng     *fakeEngine
	loop    *Loop
	log     *logRecorder
	clock   *clock
	hosts   *routeTransport
	project *store.Project
	wf      *store.Workflow
	token   string
}

type envOpts struct {
	github bool
	token  string
	// capDay and capMonth are the project's budget caps in micro-USD, nil for none.
	capDay, capMonth *store.USDMicros
}

func newEnv(t *testing.T, o envOpts) *env {
	t.Helper()
	pool := needDB(t)
	if o.token == "" {
		o.token = testToken
	}
	cipher, err := project.NewCipher(testKey)
	must(t, err)
	logger, rec := newLogRecorder()
	e := &env{t: t, pool: pool, cipher: cipher, tr: newTracker(t, o.github), log: rec, clock: &clock{t: epoch},
		hosts: &routeTransport{hosts: map[string]*url.URL{}}, token: o.token}
	e.eng = &fakeEngine{pool: pool}
	e.wf = &store.Workflow{Name: "Dev pipeline", Definition: json.RawMessage(`{"nodes":[],"edges":[],"project":true}`)}
	must(t, store.CreateWorkflow(context.Background(), pool, e.wf))
	host, repo, provider := "gitlab.test", "https://gitlab.test/group/demo.git", "gitlab"
	if o.github {
		host, repo, provider = "api.github.com", "https://github.com/owner/demo.git", "github"
	}
	e.hosts.route(host, e.tr)
	e.project = e.addProject("demo", repo, provider, o.token, o.capDay, o.capMonth)
	e.loop = &Loop{Pool: pool, Projects: &project.Resolver{DB: pool, Cipher: cipher}, Client: tools.ProviderClient(e.hosts),
		Engine: e.eng, Log: logger, Now: e.clock.Now, PublicBaseURL: publicBase}
	return e
}

// addProject stores another project with intake enabled on the environment's workflow.
func (e *env) addProject(name, repo, provider, token string, capDay, capMonth *store.USDMicros) *store.Project {
	e.t.Helper()
	p := &store.Project{ID: store.NewID(), Name: name, RepoURL: repo, Provider: provider, DefaultBranch: "main",
		IntakeEnabled: true, IntakeLabel: "yuno", IntakeWorkflowID: &e.wf.ID, BudgetDayUSDMicros: capDay, BudgetMonthUSDMicros: capMonth}
	blob, err := e.cipher.Seal(p.ID, token)
	must(e.t, err)
	must(e.t, store.CreateProject(context.Background(), e.pool, p, blob))
	return p
}

// runs lists the issue runs of one issue, oldest first.
func (e *env) runs(projectID string, n int64) []store.Run {
	e.t.Helper()
	all, err := store.ListRuns(context.Background(), e.pool, store.RunFilter{Trigger: store.TriggerIssue, Limit: 500})
	must(e.t, err)
	ref := store.IssueTriggerRef(projectID, n)
	var out []store.Run
	for i := len(all) - 1; i >= 0; i-- { // ListRuns is newest first
		if all[i].TriggerRef != nil && *all[i].TriggerRef == ref {
			out = append(out, all[i])
		}
	}
	return out
}

func (e *env) allIssueRuns() []store.Run {
	e.t.Helper()
	all, err := store.ListRuns(context.Background(), e.pool, store.RunFilter{Trigger: store.TriggerIssue, Limit: 500})
	must(e.t, err)
	return all
}

// end finishes a run the way the engine does: status, reason, finish time.
func (e *env) end(run *store.Run, status, reason string) {
	e.t.Helper()
	var r any
	if reason != "" {
		r = reason
	}
	_, err := e.pool.Exec(context.Background(), `UPDATE runs SET status=$2, failure_reason=$3, finished_at=$4 WHERE id=$1`,
		run.ID, status, r, epoch)
	must(e.t, err)
}

// get re-reads a run.
func (e *env) get(id string) *store.Run {
	e.t.Helper()
	r, err := store.GetRun(context.Background(), e.pool, id)
	must(e.t, err)
	return r
}

// polled re-reads the project's intake view.
func (e *env) polled() *store.ProjectIntake {
	e.t.Helper()
	p, err := store.GetProject(context.Background(), e.pool, e.project.ID)
	must(e.t, err)
	return p.Intake
}

// lastError is the project's intake_last_error, "" for none.
func (e *env) lastError() string {
	if in := e.polled(); in.LastError != nil {
		return *in.LastError
	}
	return ""
}

// spend records a run of the project that cost micro-USD, created at the given time.
func (e *env) spend(micros int64, at time.Time) {
	e.t.Helper()
	ctx := context.Background()
	settings := e.project.Settings()
	run := &store.Run{WorkflowID: e.wf.ID, WorkflowVersion: 1, WorkflowSnapshot: e.wf.Definition, Trigger: store.TriggerManual,
		ProjectID: &e.project.ID, Project: &settings}
	must(e.t, store.CreateRun(ctx, e.pool, run))
	_, err := e.pool.Exec(ctx, `UPDATE runs SET cost_usd_micros=$2, created_at=$3 WHERE id=$1`, run.ID, micros, at)
	must(e.t, err)
}

// poll runs one pass for the environment's project.
func (e *env) poll() Result {
	e.t.Helper()
	res, err := e.loop.PollProject(context.Background(), e.project.ID)
	if err != nil {
		e.t.Fatalf("PollProject: %v", err)
	}
	return res
}

func usd(v float64) *store.USDMicros {
	m := store.USDMicros(v * 1e6)
	return &m
}
