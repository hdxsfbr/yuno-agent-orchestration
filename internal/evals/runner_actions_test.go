package evals

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"yuno/internal/store"
)

func runOne(t *testing.T, f *simAPI, c Case) CaseResult {
	t.Helper()
	return fastRunner(testConfig(f.srv.URL)).runCase(context.Background(), c)
}

func requireError(t *testing.T, res CaseResult, want string) {
	t.Helper()
	if res.Passed() || !strings.Contains(res.Error, want) {
		t.Fatalf("result = passed %v error %q, want a failure whose error contains %q", res.Passed(), res.Error, want)
	}
}

// ---- retrigger and the rate limit --------------------------------------------------

func TestRunnerRecordsTheRefusedSecondTrigger(t *testing.T) {
	c := shippedCase(t, "019-rate-limit")
	f := newSimAPI(t, c)
	res := runOne(t, f, c)
	if !res.Passed() || !res.E2E {
		t.Fatalf("result = %+v", res)
	}
	got := res.Observed.Retrigger
	if got == nil || got.Status != 429 || !strings.Contains(got.Message, "rate limit: 1 runs/hour for agent eval-019-rate-limit-scribe-") {
		t.Fatalf("retrigger = %+v", got)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// The agent is created with the guardrail the case states.
	if len(f.agents) != 1 {
		t.Fatalf("%d agents", len(f.agents))
	}
	for _, a := range f.agents {
		if a.Guardrails.MaxRunsPerHour != 1 {
			t.Errorf("the agent was created with guardrails %+v", a.Guardrails)
		}
	}
	// Two triggers of the same workflow; the second names a script nobody registered, so
	// it could not consume the first run's steps had the api accepted it.
	if len(f.runRequests) != 2 || f.runRequests[0].WorkflowID != f.runRequests[1].WorkflowID {
		t.Fatalf("run requests = %+v", f.runRequests)
	}
	first, second := f.runRequests[0], f.runRequests[1]
	if first.ScriptID == "" || second.ScriptID != first.ScriptID+"-again" || f.scripts[second.ScriptID] || first.Model != ScriptedModel {
		t.Errorf("script ids %q and %q (registered: %v)", first.ScriptID, second.ScriptID, f.scripts)
	}
	if string(first.Input) != string(second.Input) {
		t.Errorf("inputs differ: %s and %s", first.Input, second.Input)
	}
	if len(f.runs) != 1 {
		t.Errorf("the refused trigger created %d runs in all, want 1", len(f.runs))
	}
	if len(f.deletedAgents) != 1 || len(f.deletedWorkflows) != 1 {
		t.Errorf("cleanup: agents %v workflows %v", f.deletedAgents, f.deletedWorkflows)
	}
}

func TestRunnerFailsTheCaseWhenTheRateLimitDoesNotEngage(t *testing.T) {
	c := shippedCase(t, "019-rate-limit")
	f := newSimAPI(t, c)
	f.noRateLimit = true
	res := runOne(t, f, c)
	if res.Passed() || res.Observed.Retrigger == nil || res.Observed.Retrigger.Status != 201 {
		t.Fatalf("result = %+v", res)
	}
	var detail string
	for _, e := range res.Evaluators {
		if e.Name == EvalExpectation {
			detail = e.Detail
		}
	}
	if !strings.Contains(detail, "extra trigger answered 201") || !strings.Contains(detail, "want 429") {
		t.Errorf("expectation: %s", detail)
	}
	// The extra run the api accepted is not left running.
	f.mu.Lock()
	defer f.mu.Unlock()
	cancelled := 0
	for _, n := range f.cancels {
		cancelled += n
	}
	if cancelled != 1 {
		t.Errorf("%d cancels, want exactly the one for the accepted extra run", cancelled)
	}
}

func TestRunnerFailsWhenTheRefusalIsNotTheRateLimit(t *testing.T) {
	c := shippedCase(t, "019-rate-limit")
	f := newSimAPI(t, c)
	c.Expect.TriggerErrorContains = "some other wording"
	res := runOne(t, f, c)
	if res.Passed() {
		t.Fatal("a 429 with another message must not pass")
	}
	for _, e := range res.Evaluators {
		if e.Name == EvalExpectation && !strings.Contains(e.Detail, `does not contain "some other wording"`) {
			t.Errorf("expectation: %s", e.Detail)
		}
	}
}

func TestRunnerDoesNotRetriggerARunThatDidNotFinish(t *testing.T) {
	c := shippedCase(t, "019-rate-limit")
	f := newSimAPI(t, c)
	r := fastRunner(testConfig(f.srv.URL))
	r.caseTimeout = 30 * time.Millisecond
	// A run that never ends: the sim reports running for ever.
	f.mu.Lock()
	f.sims[c.ID].tr.Run.Status = store.RunRunning
	f.sims[c.ID].tr.Run.FinishedAt = nil
	f.mu.Unlock()
	res := r.runCase(context.Background(), c)
	f.mu.Lock()
	defer f.mu.Unlock()
	if res.Error != timeoutReason || len(f.runRequests) != 1 {
		t.Fatalf("error %q after %d triggers: a timed out run is not triggered again", res.Error, len(f.runRequests))
	}
}

// ---- schedule ----------------------------------------------------------------------

func TestRunnerRunsACaseThroughASchedule(t *testing.T) {
	c := shippedCase(t, "018-schedule-triggered-run")
	f := newSimAPI(t, c)
	before := time.Now()
	res := runOne(t, f, c)
	if !res.Passed() || !res.E2E || res.Error != "" {
		t.Fatalf("result = %+v", res)
	}
	if res.Observed.Schedule == nil || res.Observed.Schedule.Enabled || res.Observed.Schedule.RunID() != res.RunID {
		t.Errorf("observed schedule = %+v, run %s", res.Observed.Schedule, res.RunID)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.scheduleRequests) != 1 || len(f.runRequests) != 0 {
		t.Fatalf("%d schedules and %d direct triggers: a schedule case triggers only through the schedule", len(f.scheduleRequests), len(f.runRequests))
	}
	req := f.scheduleRequests[0]
	if req.Kind != ScheduleKindDelay || !req.Enabled || req.WorkflowID == "" || string(req.Input) != `{"topic":"nightly digest"}` {
		t.Errorf("schedule request = %+v", req)
	}
	// run_at is an absolute UTC time two seconds ahead of the request.
	if req.RunAt.Location() != time.UTC || req.RunAt.Before(before.Add(time.Second)) || req.RunAt.After(time.Now().Add(3*time.Second)) {
		t.Errorf("run_at = %s, want about two seconds after %s", req.RunAt, before)
	}
	if req.RunAt.Nanosecond() != 0 {
		t.Errorf("run_at = %s: a whole-second timestamp is what the api documents", req.RunAt.Format(time.RFC3339Nano))
	}
	// No agents, no script: a scheduled run cannot be scripted.
	if len(f.agents) != 0 || len(f.scripts) != 0 {
		t.Errorf("created %d agents and %d scripts for a workflow with no agent node", len(f.agents), len(f.scripts))
	}
	if len(f.deletedSchedules) != 1 || len(f.deletedWorkflows) != 1 {
		t.Errorf("cleanup: schedules %v workflows %v", f.deletedSchedules, f.deletedWorkflows)
	}
}

func TestRunnerReportsAScheduleThatNeverFires(t *testing.T) {
	c := shippedCase(t, "018-schedule-triggered-run")
	c.Actions.Schedule.RunInSeconds, c.Actions.Schedule.WaitSeconds = 1, 2
	f := newSimAPI(t, c)
	f.neverFire = true
	res := runOne(t, f, c)
	requireError(t, res, "created no run within 2s")
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.deletedSchedules) != 1 {
		t.Errorf("the schedule was not cleaned up: %v", f.deletedSchedules)
	}
}

func TestRunnerReportsAScheduleThatRecordsAnError(t *testing.T) {
	c := shippedCase(t, "018-schedule-triggered-run")
	f := newSimAPI(t, c)
	f.scheduleError = "workflow deleted"
	requireError(t, runOne(t, f, c), "recorded an error instead of a run: workflow deleted")
}

func TestRunnerReportsAnApiWithoutSchedules(t *testing.T) {
	c := shippedCase(t, "018-schedule-triggered-run")
	f := newSimAPI(t, c)
	f.scheduleStatus = 404
	res := runOne(t, f, c)
	requireError(t, res, "create schedule")
	if !strings.Contains(res.Error, "404") {
		t.Errorf("error = %q", res.Error)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.deletedWorkflows) != 1 {
		t.Error("the workflow of a case that failed to schedule must still be deleted")
	}
}

func TestRunnerRejectsAScheduledRunWithTheWrongTrigger(t *testing.T) {
	c := shippedCase(t, "018-schedule-triggered-run")
	f := newSimAPI(t, c)
	c.Expect.Trigger = "manual"
	res := runOne(t, f, c)
	if res.Passed() {
		t.Fatal("the run was triggered by a schedule, the case says manual")
	}
}

// ---- the tool list -----------------------------------------------------------------

func TestRunnerChecksTheToolList(t *testing.T) {
	c := shippedCase(t, "023-gitlab-tool-absent")
	f := newSimAPI(t, c)
	res := runOne(t, f, c)
	if !res.Passed() || !res.Observed.ToolsListed || slices.Contains(res.Observed.Tools, "gitlab_create_mr") {
		t.Fatalf("result = %+v observed %+v", res, res.Observed)
	}

	// GitLab configured: the tool is listed and the case fails, naming it.
	f = newSimAPI(t, c)
	f.tools = append(f.sims[c.ID].toolList(), "gitlab_create_mr")
	res = runOne(t, f, c)
	if res.Passed() {
		t.Fatal("a listed gitlab_create_mr must fail the case")
	}
	for _, e := range res.Evaluators {
		if e.Name == EvalExpectation && !strings.Contains(e.Detail, "tool gitlab_create_mr is listed by GET /api/tools, want it absent") {
			t.Errorf("expectation: %s", e.Detail)
		}
	}

	// An empty listing proves nothing about gitlab_create_mr, so tools_present fails it.
	f = newSimAPI(t, c)
	f.tools = []string{}
	res = runOne(t, f, c)
	if res.Passed() {
		t.Fatal("an empty tool list must not pass")
	}
}

func TestRunnerReportsAToolListThatFails(t *testing.T) {
	c := shippedCase(t, "023-gitlab-tool-absent")
	f := newSimAPI(t, c)
	f.toolsStatus = 500
	requireError(t, runOne(t, f, c), "list tools")
}

func TestRunnerDoesNotListToolsWhenNoCaseNeedsIt(t *testing.T) {
	c := shippedCase(t, "019-rate-limit")
	f := newSimAPI(t, c)
	f.toolsStatus = 500 // would fail the case if the runner asked
	if res := runOne(t, f, c); !res.Passed() {
		t.Fatalf("result = %+v", res)
	}
}

// ---- cancel while waiting ----------------------------------------------------------

func TestRunnerCancelsARunThatWaitsForApproval(t *testing.T) {
	c := shippedCase(t, "022-cancel-while-waiting-approval")
	f := newSimAPI(t, c)
	res := runOne(t, f, c)
	if !res.Passed() || res.Status != store.RunCancelled || !res.E2E {
		t.Fatalf("result = %+v", res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, n := range f.cancels {
		total += n
	}
	if total != 1 || f.decisions != 0 {
		t.Errorf("%d cancels and %d approval decisions, want one cancel and none", total, f.decisions)
	}
}

// ---- model mode through the runner -------------------------------------------------

func TestRunnerGradesTheRubricWithTheModelAndGatesOnIt(t *testing.T) {
	c := shippedCase(t, "015-research-pipeline-happy")
	f := newSimAPI(t, c)
	cfg := testConfig(f.srv.URL)
	cfg.EvalProvider = ProviderAnthropic

	met := func(n int) fakeReply {
		var grades []string
		for i := 1; i <= 4; i++ {
			s := 0
			if i <= n {
				s = 1
			}
			grades = append(grades, `{"index":`+strconv.Itoa(i)+`,"score":`+strconv.Itoa(s)+`,"reason":"judged"}`)
		}
		return answer(`{"grades":[` + strings.Join(grades, ",") + `]}`)
	}

	f.messages = &fakeMessages{replies: []fakeReply{met(3)}}
	res := fastRunner(cfg).runCase(context.Background(), c)
	if res.Rubric.Mode != RubricModeModel || res.Rubric.Score != 0.75 || !res.Passed() {
		t.Fatalf("3 of 4 must pass the rubric: %+v", res.Rubric)
	}
	if f.messages.count() != 1 {
		t.Errorf("%d model calls for one case", f.messages.count())
	}

	f.messages = &fakeMessages{replies: []fakeReply{met(2)}}
	res = fastRunner(cfg).runCase(context.Background(), c)
	if res.Rubric.Score != 0.5 || res.Passed() || !res.DeterministicPassed() {
		t.Fatalf("2 of 4 passes the evaluators and fails the rubric: passed %v deterministic %v rubric %+v", res.Passed(), res.DeterministicPassed(), res.Rubric)
	}

	// The same run graded by the local checks, which the correct run meets in full.
	res = fastRunner(testConfig(f.srv.URL)).runCase(context.Background(), c)
	if res.Rubric.Mode != RubricModeHeuristic || res.Rubric.Score != 1 || !res.Passed() {
		t.Fatalf("heuristic rubric = %+v", res.Rubric)
	}
}

func TestRunnerFailsACaseWhoseModelGradingFails(t *testing.T) {
	c := shippedCase(t, "015-research-pipeline-happy")
	f := newSimAPI(t, c)
	f.messages = &fakeMessages{replies: []fakeReply{{401, `{"type":"error","error":{"type":"authentication_error","message":"the model proxy has no upstream API key"}}`}}}
	cfg := testConfig(f.srv.URL)
	cfg.EvalProvider = ProviderAnthropic
	res := fastRunner(cfg).runCase(context.Background(), c)
	if res.Passed() || !res.DeterministicPassed() || res.Rubric.Score != 0 || !strings.Contains(res.Rubric.Detail, "no upstream API key") {
		t.Fatalf("result = passed %v, rubric %+v", res.Passed(), res.Rubric)
	}
}

func TestACaseThatNeverRanHasAnUnscoredRubric(t *testing.T) {
	c := shippedCase(t, "015-research-pipeline-happy")
	// Nothing listens there, so the first request fails and the case never gets a trace.
	res := fastRunner(testConfig("http://127.0.0.1:1")).runCase(context.Background(), c)
	if res.Error == "" || res.Rubric.Criteria != len(c.Rubric) || res.Rubric.Score != 0 || !strings.HasPrefix(res.Rubric.Detail, "not scored") || res.Passed() {
		t.Fatalf("result = %+v", res)
	}
	if got := rubricCell(res.Rubric); !strings.Contains(got, "not scored") {
		t.Errorf("cell = %q", got)
	}
}

// ---- the suite ---------------------------------------------------------------------

func TestRunSuiteInModelModeLabelsTheReport(t *testing.T) {
	c := shippedCase(t, "015-research-pipeline-happy")
	f := newSimAPI(t, c)
	f.messages = &fakeMessages{replies: []fakeReply{answer(`{"grades":[{"index":1,"score":1,"reason":"a"},{"index":2,"score":1,"reason":"b"},{"index":3,"score":1,"reason":"c"},{"index":4,"score":1,"reason":"d"}]}`)}}

	cfg := testConfig(f.srv.URL)
	cfg.EvalProvider = "Anthropic"
	cfg.EvalSplit = "public"
	cfg.EvalCasesDir = filepath.Join(t.TempDir(), "cases")
	body, err := os.ReadFile(c.Path)
	if err != nil {
		t.Fatal(err)
	}
	writeCase(t, cfg.EvalCasesDir, "v1/"+filepath.Base(c.Path), string(body))
	cfg.EvalReportsDir = filepath.Join(t.TempDir(), "reports")

	var out strings.Builder
	if err := runSuite(context.Background(), cfg, quietLogger(), &out, fastTune); err != nil {
		t.Fatalf("suite failed: %v", err)
	}
	if want := "public split: 1 cases, deterministic pass 1.000, e2e success 1.000\n"; out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
	md, _ := filepath.Glob(filepath.Join(cfg.EvalReportsDir, "*.md"))
	js, _ := filepath.Glob(filepath.Join(cfg.EvalReportsDir, "*.json"))
	if len(md) != 1 || len(js) != 1 {
		t.Fatalf("reports: %v %v", md, js)
	}
	text, _ := os.ReadFile(md[0])
	for _, want := range []string{"1.00 (4/4, model)", "anthropic (runs replayed from their scripts, rubrics graded by claude-opus-5-5)", "## Rubric by criterion"} {
		if !strings.Contains(string(text), want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
	var rep Report
	raw, _ := os.ReadFile(js[0])
	if err := json.Unmarshal(raw, &rep); err != nil || rep.Provider != "anthropic" || rep.Cases[0].Rubric.Mode != "model" ||
		rep.Cases[0].Rubric.Model != RubricModel || len(rep.Cases[0].Rubric.Items) != 4 || rep.Summary.Rubric.Model != 1 {
		t.Fatalf("json report = %v %+v %+v", err, rep.ReportMeta, rep.Cases[0].Rubric)
	}
}

func TestRunSuiteWithTheScriptedProviderNeverCallsTheModel(t *testing.T) {
	c := shippedCase(t, "015-research-pipeline-happy")
	f := newSimAPI(t, c)
	f.messages = &fakeMessages{replies: []fakeReply{answer(`{}`)}}
	cfg := testConfig(f.srv.URL)
	cfg.EvalCasesDir = filepath.Join(t.TempDir(), "cases")
	cfg.EvalSplit = "public"
	body, _ := os.ReadFile(c.Path)
	writeCase(t, cfg.EvalCasesDir, "v1/"+filepath.Base(c.Path), string(body))
	cfg.EvalReportsDir = filepath.Join(t.TempDir(), "reports")
	if err := runSuite(context.Background(), cfg, quietLogger(), io.Discard, fastTune); err != nil {
		t.Fatal(err)
	}
	if f.messages.count() != 0 {
		t.Errorf("the scripted provider asked the model %d times", f.messages.count())
	}
}
