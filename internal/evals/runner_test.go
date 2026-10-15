package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"yuno/internal/config"
	"yuno/internal/model"
	"yuno/internal/store"
)

const fakeToken = "test-token"

// outcome is how the fake api ends a run when nobody cancels it.
type outcome struct {
	status string
	reason string
	steps  int
}

type fakeRun struct {
	id        string
	caseID    string
	req       RunRequest
	polls     int
	status    string
	steps     int
	reason    string
	cancelled bool
	cancels   int
	lag       int // polls seen since the cancel was accepted
	total     int // steps in the registered script
	started   time.Time
	finished  *time.Time
}

// fakeAPI answers the endpoints the runner uses with canned bodies and advances each run
// one step per status poll until it is terminal.
type fakeAPI struct {
	mu sync.Mutex

	nextID  int
	agents  map[string]AgentRequest
	deleted []string
	scripts map[string][]model.ScriptStep
	runs    map[string]*fakeRun
	byAgent map[string]string // agent id -> case id

	// Knobs.
	outcomes      map[string]outcome // by case id; the default is a successful run
	stuck         map[string]bool    // case ids whose run never finishes
	failAgentFor  map[string]bool    // case ids whose agent creation returns 500
	pollErrors    int                // number of leading run polls that return 500
	cancelLag     int                // polls a cancelled run stays non-terminal for
	alwaysPollErr bool
	readyFailures int
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		agents: map[string]AgentRequest{}, scripts: map[string][]model.ScriptStep{},
		runs: map[string]*fakeRun{}, byAgent: map[string]string{},
		outcomes: map[string]outcome{
			"001-quick-run-happy": {status: store.RunSucceeded, steps: 4},
			"002-step-limit":      {status: store.RunFailed, reason: "step limit 2 exceeded", steps: 3},
			"003-cancel":          {status: store.RunSucceeded, steps: 4},
		},
		stuck: map[string]bool{}, failAgentFor: map[string]bool{},
	}
}

var suffixPattern = regexp.MustCompile(`-[0-9a-f]{6}$`)

func caseIDFromScript(scriptID string) string { return suffixPattern.ReplaceAllString(scriptID, "") }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.readyFailures > 0 {
			f.readyFailures--
			writeErr(w, 503, "not_ready", "gateway not connected")
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("POST /api/agents", func(w http.ResponseWriter, r *http.Request) {
		var req AgentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 422, "bad_request", err.Error())
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		for caseID := range f.failAgentFor {
			if strings.HasPrefix(req.Name, "eval-"+caseID+"-") {
				writeErr(w, 500, "internal", "materialise failed")
				return
			}
		}
		f.nextID++
		id := fmt.Sprintf("agent-%d", f.nextID)
		f.agents[id] = req
		f.byAgent[id] = suffixPattern.ReplaceAllString(strings.TrimPrefix(req.Name, "eval-"), "")
		writeJSON(w, 201, store.Agent{ID: id, Name: req.Name, Model: req.Model})
	})
	mux.HandleFunc("DELETE /api/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.deleted = append(f.deleted, r.PathValue("id"))
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /api/evals/scripts", func(w http.ResponseWriter, r *http.Request) {
		var reg struct {
			ScriptID string             `json:"script_id"`
			Steps    []model.ScriptStep `json:"steps"`
		}
		if err := json.NewDecoder(r.Body).Decode(&reg); err != nil || reg.ScriptID == "" {
			writeErr(w, 422, "bad_request", "bad script")
			return
		}
		f.mu.Lock()
		f.scripts[reg.ScriptID] = reg.Steps
		f.mu.Unlock()
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /api/runs", func(w http.ResponseWriter, r *http.Request) {
		var req RunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 422, "bad_request", err.Error())
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		steps, ok := f.scripts[req.ScriptID]
		if !ok {
			writeErr(w, 422, "unknown_script", "script is not registered")
			return
		}
		f.nextID++
		run := &fakeRun{
			id: fmt.Sprintf("run-%d", f.nextID), caseID: caseIDFromScript(req.ScriptID), req: req,
			status: store.RunQueued, total: len(steps), started: time.Now().Add(-2 * time.Second),
		}
		f.runs[run.id] = run
		writeJSON(w, 201, f.runBody(run))
	})
	mux.HandleFunc("GET /api/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		run, ok := f.runs[r.PathValue("id")]
		if !ok {
			writeErr(w, 404, "not_found", "no such run")
			return
		}
		if f.alwaysPollErr || f.pollErrors > 0 {
			if f.pollErrors > 0 {
				f.pollErrors--
			}
			writeErr(w, 500, "internal", "database unavailable")
			return
		}
		f.advance(run)
		writeJSON(w, 200, f.runBody(run))
	})
	mux.HandleFunc("POST /api/runs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		run, ok := f.runs[r.PathValue("id")]
		if !ok {
			writeErr(w, 404, "not_found", "no such run")
			return
		}
		if store.IsTerminal(run.status) {
			writeErr(w, 409, "conflict", "run is already finished")
			return
		}
		run.cancelled = true
		run.cancels++
		writeJSON(w, 202, f.runBody(run))
	})
	mux.HandleFunc("GET /api/runs/{id}/trace", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		run, ok := f.runs[r.PathValue("id")]
		if !ok {
			writeErr(w, 404, "not_found", "no such run")
			return
		}
		writeJSON(w, 200, f.traceFor(run))
	})
	return f.auth(mux)
}

func (f *fakeAPI) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Header.Get("Authorization") != "Bearer "+fakeToken {
			writeErr(w, 401, "unauthorized", "missing or wrong bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// advance moves a run one step along per poll; callers hold f.mu.
func (f *fakeAPI) advance(run *fakeRun) {
	if store.IsTerminal(run.status) {
		return
	}
	finish := func(status, reason string) {
		now := time.Now()
		run.status, run.reason, run.finished = status, reason, &now
	}
	if run.cancelled {
		run.lag++
		if run.lag > f.cancelLag {
			finish(store.RunCancelled, "cancelled by api")
		}
		return
	}
	run.polls++
	run.status = store.RunRunning
	if f.stuck[run.caseID] {
		run.steps = 1
		return
	}
	out := f.outcomes[run.caseID]
	if out.status == "" {
		out = outcome{status: store.RunSucceeded, steps: run.total}
	}
	run.steps = min(run.polls, out.steps)
	if run.polls > out.steps {
		finish(out.status, out.reason)
	}
}

func (f *fakeAPI) runBody(run *fakeRun) store.Run {
	body := store.Run{
		ID: run.id, WorkflowID: "wf-" + run.id, Trigger: store.TriggerEval, Status: run.status,
		Limits: run.req.Limits, StepCount: run.steps, ToolCallCount: max(run.steps-1, 0),
		CostUSDMicros: 1500, CreatedAt: run.started, StartedAt: &run.started, FinishedAt: run.finished,
	}
	if run.reason != "" {
		body.FailureReason = &run.reason
	}
	return body
}

func (f *fakeAPI) traceFor(run *fakeRun) store.Trace {
	body := f.runBody(run)
	tr := store.Trace{Run: &body, Spans: []store.Span{{ID: "s-" + run.id, RunID: run.id, Kind: "run", Name: "run"}}}
	for i := 0; i < run.steps; i++ {
		tr.ModelCalls = append(tr.ModelCalls, store.ModelCall{
			ID: fmt.Sprintf("m%d", i), RunID: run.id, Provider: "scripted", Model: "scripted-1", InputTokens: 100, OutputTokens: 10,
		})
		if i < run.steps-1 {
			done := run.started.Add(time.Second)
			tr.ToolCalls = append(tr.ToolCalls, store.ToolCall{
				ID: fmt.Sprintf("t%d", i), RunID: run.id, ToolName: "exec", Status: store.ToolExecuted, FinishedAt: &done,
			})
		}
	}
	return tr
}

func (f *fakeAPI) cancelsFor(caseID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.runs {
		if r.caseID == caseID {
			n += r.cancels
		}
	}
	return n
}

func (f *fakeAPI) deletedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deleted)
}

// quietLogger discards runner logs so test output stays readable.
func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testConfig(url string) config.Config {
	return config.Config{EvalAPIURL: url, APIToken: fakeToken, DefaultModel: "claude-test", EvalProvider: "scripted"}
}

func fastRunner(cfg config.Config) *Runner {
	r := NewRunner(cfg, quietLogger())
	r.poll = 2 * time.Millisecond
	r.caseTimeout = 5 * time.Second
	r.readyTimeout = 5 * time.Second
	return r
}

func startFake(t *testing.T) (*fakeAPI, *httptest.Server) {
	t.Helper()
	f := newFakeAPI()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return f, srv
}

func shippedCases(t *testing.T) []Case {
	t.Helper()
	cases, err := LoadCases(shippedCasesDir, SplitPublic)
	if err != nil {
		t.Fatal(err)
	}
	return cases
}

func byID(results []CaseResult) map[string]CaseResult {
	m := map[string]CaseResult{}
	for _, r := range results {
		m[r.ID] = r
	}
	return m
}

func TestRunnerRunsTheShippedCasesEndToEnd(t *testing.T) {
	f, srv := startFake(t)
	cases := shippedCases(t)
	results, err := fastRunner(testConfig(srv.URL)).Execute(context.Background(), cases)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results", len(results))
	}
	for _, r := range results {
		if !r.Passed() || !r.E2E {
			t.Errorf("%s: passed=%v e2e=%v error=%q evaluators=%+v", r.ID, r.Passed(), r.E2E, r.Error, r.Evaluators)
		}
		if r.RunID == "" || r.AgentID == "" || r.ScriptID == "" {
			t.Errorf("%s: missing ids %+v", r.ID, r)
		}
		if r.Rubric.Score != 1 {
			t.Errorf("%s: rubric = %+v", r.ID, r.Rubric)
		}
	}

	res := byID(results)
	if r := res["002-step-limit"]; r.Status != "failed" || r.FailureReason != "step limit 2 exceeded" || r.Reason() != r.FailureReason {
		t.Errorf("002 = %+v", r)
	}
	if r := res["003-cancel"]; r.Status != "cancelled" {
		t.Errorf("003 status = %s", r.Status)
	}
	if r := res["001-quick-run-happy"]; r.CostUSDMicros != 1500 || r.ModelCalls != 4 || r.ToolCalls != 3 || r.DurationMs <= 0 {
		t.Errorf("001 facts = %+v", r)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	namePattern := regexp.MustCompile(`^eval-(\d{3}-[a-z-]+)-([0-9a-f]{6})$`)
	if len(f.agents) != 3 {
		t.Fatalf("created %d agents", len(f.agents))
	}
	for id, a := range f.agents {
		m := namePattern.FindStringSubmatch(a.Name)
		if m == nil {
			t.Errorf("agent name %q does not match eval-<id>-<random>", a.Name)
			continue
		}
		if a.Model != "claude-test" {
			t.Errorf("%s: agent model = %q, want the default model", a.Name, a.Model)
		}
		var c Case
		for _, cc := range cases {
			if cc.ID == m[1] {
				c = cc
			}
		}
		if a.SystemPrompt != c.Agent.SystemPrompt || a.Role != c.Agent.Name ||
			strings.Join(a.ToolAllowlist, ",") != strings.Join(c.Agent.ToolAllowlist, ",") || a.Limits != c.Agent.Limits.Store() {
			t.Errorf("%s: agent does not carry the case's prompt, tools and limits: %+v", a.Name, a)
		}
		// The run for this agent uses the scripted model and a script id built from the same suffix.
		var run *fakeRun
		for _, r := range f.runs {
			if r.req.AgentID == id {
				run = r
			}
		}
		if run == nil {
			t.Errorf("%s: no run for the agent", a.Name)
			continue
		}
		if run.req.Model != ScriptedModel || run.req.ScriptID != m[1]+"-"+m[2] || run.req.Task != c.Task || run.req.Limits != c.Agent.Limits.Store() {
			t.Errorf("%s: run request = %+v", a.Name, run.req)
		}
		if len(f.scripts[run.req.ScriptID]) != len(c.Script) {
			t.Errorf("%s: registered %d steps, want %d", a.Name, len(f.scripts[run.req.ScriptID]), len(c.Script))
		}
		if run.cancels != map[bool]int{true: 1, false: 0}[m[1] == "003-cancel"] {
			t.Errorf("%s: cancel requested %d times", m[1], run.cancels)
		}
	}
	if len(f.deleted) != 3 {
		t.Errorf("deleted %d agents, want 3", len(f.deleted))
	}
	// The $RUN_TOKEN placeholder travels to the api untouched.
	for sid, steps := range f.scripts {
		if strings.HasPrefix(sid, "001-") {
			raw, _ := json.Marshal(steps)
			if !strings.Contains(string(raw), `"run_token":"$RUN_TOKEN"`) || !strings.Contains(string(raw), `runs/$RUN_ID/notes.md`) {
				t.Errorf("001 script lost its placeholders: %s", raw)
			}
		}
	}
}

func TestRunnerCancelsOnceWhenTheStepThresholdIsReached(t *testing.T) {
	f, srv := startFake(t)
	var c Case
	for _, cc := range shippedCases(t) {
		if cc.ID == "003-cancel" {
			c = cc
		}
	}
	c.Actions.CancelAfterSteps = 2
	f.outcomes["003-cancel"] = outcome{status: store.RunSucceeded, steps: 6}
	// The api takes a while to abort, as the real one does: the runner keeps polling
	// the still-running run and must not ask again.
	f.cancelLag = 8
	res := fastRunner(testConfig(srv.URL)).runCase(context.Background(), c)
	if res.Status != "cancelled" || !res.Passed() {
		t.Fatalf("result = %+v", res)
	}
	if n := f.cancelsFor("003-cancel"); n != 1 {
		t.Fatalf("cancel requested %d times, want 1", n)
	}
	for _, r := range f.runs {
		if r.steps < 2 {
			t.Errorf("cancel was requested at step %d, before the threshold of 2", r.steps)
		}
	}
}

func TestRunnerDoesNotCancelCasesWithoutTheAction(t *testing.T) {
	f, srv := startFake(t)
	for _, c := range shippedCases(t) {
		if c.ID == "001-quick-run-happy" {
			fastRunner(testConfig(srv.URL)).runCase(context.Background(), c)
		}
	}
	if n := f.cancelsFor("001-quick-run-happy"); n != 0 {
		t.Fatalf("cancel requested %d times", n)
	}
}

func TestRunnerTimeoutIsAFailureWithAReason(t *testing.T) {
	f, srv := startFake(t)
	f.stuck["001-quick-run-happy"] = true
	var c Case
	for _, cc := range shippedCases(t) {
		if cc.ID == "001-quick-run-happy" {
			c = cc
		}
	}
	r := fastRunner(testConfig(srv.URL))
	r.caseTimeout = 150 * time.Millisecond

	res := r.runCase(context.Background(), c)
	if res.Error != "runner timeout" || res.Reason() != "runner timeout" {
		t.Fatalf("error = %q", res.Error)
	}
	if res.Passed() || res.E2E {
		t.Errorf("a timed out case must fail: passed=%v e2e=%v", res.Passed(), res.E2E)
	}
	if res.Status != "running" {
		t.Errorf("status = %q, want the status at the time the runner gave up", res.Status)
	}
	if term := res.Evaluators[0]; term.Name != EvalTermination || term.Passed {
		t.Errorf("termination should fail on a run that never ended: %+v", term)
	}
	if n := f.cancelsFor("001-quick-run-happy"); n != 1 {
		t.Errorf("a timed out run should be cancelled once, got %d", n)
	}
	if f.deletedCount() != 1 {
		t.Errorf("agent not cleaned up after a timeout")
	}
}

func TestRunnerContinuesAfterAnError(t *testing.T) {
	f, srv := startFake(t)
	f.failAgentFor["002-step-limit"] = true
	results, err := fastRunner(testConfig(srv.URL)).Execute(context.Background(), shippedCases(t))
	if err != nil {
		t.Fatal(err)
	}
	res := byID(results)
	if len(results) != 3 || !res["001-quick-run-happy"].Passed() || !res["003-cancel"].Passed() {
		t.Fatalf("the other cases should still run and pass: %+v", results)
	}
	bad := res["002-step-limit"]
	if bad.Passed() || bad.E2E || !strings.Contains(bad.Error, "create agent") || !strings.Contains(bad.Error, "500") {
		t.Fatalf("002 = %+v", bad)
	}
	if len(bad.Evaluators) != 3 {
		t.Fatalf("a failed case still reports every evaluator: %+v", bad.Evaluators)
	}
	for _, e := range bad.Evaluators {
		if e.Passed || !strings.Contains(e.Detail, "create agent") {
			t.Errorf("evaluator %s = %+v", e.Name, e)
		}
	}
	if f.deletedCount() != 2 {
		t.Errorf("deleted %d agents, want 2 (none was created for the failed case)", f.deletedCount())
	}
}

func TestRunnerRecoversFromPanics(t *testing.T) {
	f, srv := startFake(t)
	r := fastRunner(testConfig(srv.URL))
	r.evaluate = func(c Case, tr store.Trace) []EvalResult {
		if c.ID == "001-quick-run-happy" {
			panic("evaluator exploded")
		}
		return Evaluate(c, tr)
	}
	results, err := r.Execute(context.Background(), shippedCases(t))
	if err != nil {
		t.Fatal(err)
	}
	res := byID(results)
	if len(results) != 3 || !res["002-step-limit"].Passed() || !res["003-cancel"].Passed() {
		t.Fatalf("the suite should continue after a panic: %+v", results)
	}
	bad := res["001-quick-run-happy"]
	if bad.Passed() || !strings.Contains(bad.Error, "runner panic: evaluator exploded") || len(bad.Evaluators) != 3 {
		t.Fatalf("001 = %+v", bad)
	}
	if f.deletedCount() != 3 {
		t.Errorf("deleted %d agents, want 3", f.deletedCount())
	}
}

func TestRunnerToleratesTransientPollErrors(t *testing.T) {
	f, srv := startFake(t)
	f.pollErrors = maxPollFailures - 1
	c := shippedCases(t)[0]
	if res := fastRunner(testConfig(srv.URL)).runCase(context.Background(), c); !res.Passed() {
		t.Fatalf("a few failed polls should not fail the case: %+v", res)
	}
}

func TestRunnerGivesUpOnPersistentPollErrors(t *testing.T) {
	f, srv := startFake(t)
	f.alwaysPollErr = true
	c := shippedCases(t)[0]
	res := fastRunner(testConfig(srv.URL)).runCase(context.Background(), c)
	if res.Passed() || !strings.Contains(res.Error, "poll run") {
		t.Fatalf("result = %+v", res)
	}
	if f.deletedCount() != 1 {
		t.Error("agent not cleaned up")
	}
	if n := f.cancelsFor(c.ID); n != 1 {
		t.Errorf("the run nobody polls any more must be cancelled, got %d cancels", n)
	}
}

func TestRunnerStopsWhenTheContextEnds(t *testing.T) {
	f, srv := startFake(t)
	f.stuck["001-quick-run-happy"] = true
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(60 * time.Millisecond)
		cancel()
	}()
	results, err := fastRunner(testConfig(srv.URL)).Execute(ctx, shippedCases(t))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(results) != 1 || results[0].Passed() {
		t.Fatalf("results = %+v, want the interrupted case recorded as failed and no more", results)
	}
	if f.deletedCount() != 1 {
		t.Errorf("the interrupted case's agent must still be deleted, got %d deletions", f.deletedCount())
	}
	if n := f.cancelsFor("001-quick-run-happy"); n != 1 {
		t.Errorf("the interrupted case's run must be cancelled, got %d cancels", n)
	}
}

func TestRunnerWaitsForTheAPI(t *testing.T) {
	f, srv := startFake(t)
	f.readyFailures = 2
	c := NewClient(srv.URL, fakeToken)
	if err := c.WaitReady(context.Background(), 5*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	left := f.readyFailures
	f.mu.Unlock()
	if left != 0 {
		t.Errorf("readyz was not polled until ready, %d failures left", left)
	}

	f.mu.Lock()
	f.readyFailures = 1000
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := c.WaitReady(ctx, 5*time.Millisecond); err == nil || !strings.Contains(err.Error(), "api not ready") {
		t.Errorf("err = %v", err)
	}
}

func TestClientErrors(t *testing.T) {
	f, srv := startFake(t)
	ctx := context.Background()

	if _, err := NewClient(srv.URL, "wrong").GetRun(ctx, "x"); statusOf(err) != 401 {
		t.Errorf("wrong token: %v", err)
	}
	c := NewClient(srv.URL, fakeToken)
	_, err := c.GetRun(ctx, "missing")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 404 || ae.Code != "not_found" || !strings.Contains(err.Error(), "no such run") {
		t.Errorf("404: %v", err)
	}
	if err := c.CancelRun(ctx, "missing"); statusOf(err) != 404 {
		t.Errorf("cancel of an unknown run should be an error: %v", err)
	}
	if err := c.DeleteAgent(ctx, "gone"); err != nil {
		t.Errorf("delete: %v", err)
	}
	if err := c.RegisterScript(ctx, "", nil); statusOf(err) != 422 {
		t.Errorf("bad script: %v", err)
	}

	// A run that already finished answers 409 on cancel, which is not a failure.
	f.mu.Lock()
	f.runs["done"] = &fakeRun{id: "done", status: store.RunSucceeded}
	f.mu.Unlock()
	if err := c.CancelRun(ctx, "done"); err != nil {
		t.Errorf("409 on cancel should be ignored: %v", err)
	}

	if _, err := NewClient("http://127.0.0.1:1", "").GetRun(ctx, "x"); err == nil {
		t.Error("connection failure should be an error")
	}
}

// ---- Run: the whole suite, report and exit status ----

func suiteConfig(t *testing.T, url string) config.Config {
	cfg := testConfig(url)
	cfg.EvalSplit = "public"
	cfg.EvalCasesDir = shippedCasesDir
	cfg.EvalReportsDir = filepath.Join(t.TempDir(), "reports")
	return cfg
}

func fastTune(r *Runner) {
	r.poll = 2 * time.Millisecond
	r.caseTimeout = 5 * time.Second
}

func TestRunSuiteWritesTheReportAndPasses(t *testing.T) {
	_, srv := startFake(t)
	cfg := suiteConfig(t, srv.URL)
	var out bytes.Buffer
	if err := runSuite(context.Background(), cfg, quietLogger(), &out, fastTune); err != nil {
		t.Fatalf("suite failed: %v", err)
	}
	if want := "public split: 3 cases, deterministic pass 1.000, e2e success 1.000\n"; out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
	md, _ := filepath.Glob(filepath.Join(cfg.EvalReportsDir, "*.md"))
	js, _ := filepath.Glob(filepath.Join(cfg.EvalReportsDir, "*.json"))
	if len(md) != 1 || len(js) != 1 {
		t.Fatalf("reports: %v %v", md, js)
	}
	body, _ := os.ReadFile(md[0])
	for _, want := range []string{"001-quick-run-happy", "002-step-limit", "003-cancel", "step limit 2 exceeded", "| termination | 3 | 3 | 1.000 |"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("report missing %q:\n%s", want, body)
		}
	}
	var rep Report
	raw, _ := os.ReadFile(js[0])
	if err := json.Unmarshal(raw, &rep); err != nil || len(rep.Cases) != 3 || rep.Provider != "scripted" || rep.Split != "public" {
		t.Fatalf("json report = %v %+v", err, rep.ReportMeta)
	}
}

func TestRunSuiteFailsBelowTheGateButStillWritesTheReport(t *testing.T) {
	f, srv := startFake(t)
	f.outcomes["002-step-limit"] = outcome{status: store.RunSucceeded, steps: 3} // not the expected failure
	cfg := suiteConfig(t, srv.URL)
	var out bytes.Buffer
	err := runSuite(context.Background(), cfg, quietLogger(), &out, fastTune)
	if err == nil || !strings.Contains(err.Error(), "public pass rate 0.667 is below 0.9") {
		t.Fatalf("err = %v", err)
	}
	if want := "public split: 3 cases, deterministic pass 0.667, e2e success 0.667\n"; out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
	md, _ := filepath.Glob(filepath.Join(cfg.EvalReportsDir, "*.md"))
	if len(md) != 1 {
		t.Fatalf("report not written: %v", md)
	}
	body, _ := os.ReadFile(md[0])
	if !strings.Contains(string(body), "## Failures") || !strings.Contains(string(body), "002-step-limit") {
		t.Errorf("report should list the failure:\n%s", body)
	}
}

func TestRunSuiteRejectsBadConfiguration(t *testing.T) {
	_, srv := startFake(t)
	tests := []struct {
		name   string
		mutate func(*config.Config)
		want   string
	}{
		{"unsupported provider", func(c *config.Config) { c.EvalProvider = "anthropic" }, "not supported"},
		{"unknown split", func(c *config.Config) { c.EvalSplit = "everything" }, "unknown eval split"},
		{"missing cases directory", func(c *config.Config) { c.EvalCasesDir = filepath.Join(t.TempDir(), "nope") }, "read eval cases"},
		{"no private cases", func(c *config.Config) { c.EvalSplit = "private" }, "no eval cases"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := suiteConfig(t, srv.URL)
			tt.mutate(&cfg)
			err := runSuite(context.Background(), cfg, quietLogger(), io.Discard, fastTune)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
			if entries, _ := os.ReadDir(cfg.EvalReportsDir); len(entries) != 0 {
				t.Errorf("no report should be written on a configuration error")
			}
		})
	}
}

func TestRunSuiteInterruptedIsNotACleanExit(t *testing.T) {
	f, srv := startFake(t)
	f.stuck["001-quick-run-happy"] = true
	cfg := suiteConfig(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(60 * time.Millisecond)
		cancel()
	}()
	err := runSuite(ctx, cfg, quietLogger(), io.Discard, fastTune)
	if err == nil || errors.Is(err, context.Canceled) || !errors.Is(err, errInterrupted) {
		t.Fatalf("err = %v: an interrupted suite must fail the process, not look like a clean cancel", err)
	}
}

func TestRunSuiteWithAnAPIThatNeverBecomesReady(t *testing.T) {
	f, srv := startFake(t)
	f.readyFailures = 1 << 20
	cfg := suiteConfig(t, srv.URL)
	err := runSuite(context.Background(), cfg, quietLogger(), io.Discard, func(r *Runner) { r.readyTimeout = 30 * time.Millisecond })
	if err == nil || !strings.Contains(err.Error(), "api not ready") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunUsesTheExportedEntryPoint(t *testing.T) {
	// Run is what cmd/yuno calls. With a bad provider it must fail before touching the api.
	cfg := suiteConfig(t, "http://127.0.0.1:1")
	cfg.EvalProvider = "anthropic"
	if err := Run(context.Background(), cfg, quietLogger()); err == nil {
		t.Fatal("expected an error")
	}
}
