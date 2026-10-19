package evals

import (
	"encoding/json"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"yuno/internal/config"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// What the wave 3 cases are about, one test each: the file keeps a case honest about the
// feature it exists for, so an edit that quietly turns it into a different case fails.

func TestShippedSuiteSizeAndSplit(t *testing.T) {
	var pub, priv int
	for _, c := range shippedCases(t) {
		switch c.Split {
		case SplitPublic:
			pub++
		case SplitPrivate:
			priv++
		}
	}
	if pub < 14 || priv < 6 || pub+priv < 20 {
		t.Errorf("shipped %d public and %d private cases, want at least 14, 6 and 20 in all", pub, priv)
	}
}

// projectCaseBudget bounds the sum of the step timeouts of a case on the fake forge, in seconds.
const projectCaseBudget = 6000

// projectStepBudget is the longest the steps of a project case can wait, in seconds.
func projectStepBudget(c Case) int {
	sum := 0
	for _, st := range c.Steps {
		switch {
		case st.Forge != nil:
			sum += stepTimeoutSeconds(st.Forge.TimeoutSeconds)
		case st.Wait != nil:
			sum += stepTimeoutSeconds(st.Wait.TimeoutSeconds)
		case st.Approve != nil:
			sum += stepTimeoutSeconds(st.Approve.TimeoutSeconds)
		case st.Hold != nil:
			sum += st.Hold.Seconds
		}
	}
	return sum
}

// Every shipped case finishes inside 45 s when the platform behaves: the estimate adds
// what the case makes the runner or the engine wait for.
func TestEveryShippedCaseFitsTheTimeBudget(t *testing.T) {
	const budget = 45
	sleeps := regexp.MustCompile(`sleep (\d+)`)
	for _, c := range shippedCases(t) {
		est := 5 // agent turns, sandbox start and cleanup
		if w := c.Actions.ApprovalWaitSeconds; w > est {
			est = w
		}
		if s := c.Actions.Schedule; s != nil {
			est = max(est, s.RunInSeconds+10) // the fire waits for the next scheduler tick
		}
		if c.Expect.Status == store.RunTimedOut {
			// The limit, one 2 s limit tick, up to 10 s for the abort (DESIGN 5.2).
			est = max(est, c.RunLimits().MaxDurationSeconds+12)
		}
		if !enforcedByTheEngine[c.ID] {
			for _, s := range c.Script {
				if s.Tool == nil {
					continue
				}
				if cmd, _ := s.Tool.Args["command"].(string); cmd != "" {
					for _, m := range sleeps.FindAllStringSubmatch(cmd, -1) {
						n, _ := strconv.Atoi(m[1])
						est += n
					}
				}
			}
		}
		if c.Project != nil {
			// A case on the fake forge waits for the platform's own loops and for runs the
			// platform starts: its budget is the sum of its steps' timeouts, bounded apart.
			if sum := projectStepBudget(c); sum > projectCaseBudget {
				t.Errorf("%s: its steps may wait %d s in all, over the %d s budget of a project case", c.ID, sum, projectCaseBudget)
			}
			continue
		}
		if est > budget {
			t.Errorf("%s is estimated at %d s, over the %d s budget", c.ID, est, budget)
		}
		if s := c.Actions.Schedule; s != nil && s.wait() > 40 {
			t.Errorf("%s waits up to %d s for its schedule", c.ID, s.wait())
		}
	}
}

// Case 015 is the research_pipeline template: same graph, tasks and schemas, so what the
// case proves about it is what the template does. It differs in one thing, by design: the
// researcher has no http_fetch, so the case runs offline.
func TestResearchPipelineCaseMirrorsTheTemplate(t *testing.T) {
	raw, err := os.ReadFile("../../templates/legacy/research_pipeline.json")
	if err != nil {
		t.Fatal(err)
	}
	var tpl struct {
		Definition json.RawMessage `json:"definition"`
	}
	if err := json.Unmarshal(raw, &tpl); err != nil {
		t.Fatal(err)
	}
	want, err := workflow.Parse(tpl.Definition)
	if err != nil {
		t.Fatal(err)
	}
	c := shippedCase(t, "015-research-pipeline-happy")
	got, err := c.Definition()
	if err != nil {
		t.Fatal(err)
	}

	same := func(what string, a, b json.RawMessage) {
		t.Helper()
		var x, y any
		_ = json.Unmarshal(a, &x)
		_ = json.Unmarshal(b, &y)
		if !jsonEqual(x, y) {
			t.Errorf("%s differs from the template:\n case     %s\n template %s", what, a, b)
		}
	}
	same("input_schema", got.InputSchema, want.InputSchema)
	same("state_schema", got.StateSchema, want.StateSchema)
	if got.Limits.MaxLoopVisits != want.Limits.MaxLoopVisits {
		t.Errorf("max_loop_visits = %d, the template has %d", got.Limits.MaxLoopVisits, want.Limits.MaxLoopVisits)
	}
	if len(got.Nodes) != len(want.Nodes) {
		t.Fatalf("%d nodes, the template has %d", len(got.Nodes), len(want.Nodes))
	}
	for i, n := range want.Nodes {
		g := got.Nodes[i]
		if g.ID != n.ID || g.Kind != n.Kind || !strings.EqualFold(g.AgentName, n.AgentName) ||
			strings.Join(g.InputFrom, ",") != strings.Join(n.InputFrom, ",") || g.Task != n.Task {
			t.Errorf("node %d = %+v, the template has %+v", i, g, n)
		}
		if n.Kind == workflow.KindAgent {
			same("output_schema of "+n.ID, g.OutputSchema, n.OutputSchema)
		}
	}
	if len(got.Edges) != len(want.Edges) {
		t.Fatalf("%d edges, the template has %d", len(got.Edges), len(want.Edges))
	}
	for i, e := range want.Edges {
		g := got.Edges[i]
		gw, _ := json.Marshal(g.When)
		ww, _ := json.Marshal(e.When)
		if g.From != e.From || g.To != e.To || string(gw) != string(ww) {
			t.Errorf("edge %d = %+v, the template has %+v", i, g, e)
		}
	}

	if len(c.Agents) != 3 {
		t.Errorf("the case has %d agents, want the template's three", len(c.Agents))
	}
	for key, spec := range c.Agents {
		if allowed(spec.ToolAllowlist, "http_fetch") {
			t.Errorf("agent %s has http_fetch: the case must run offline", key)
		}
	}
	for _, s := range c.Script {
		if s.Tool != nil && strings.HasSuffix(s.Tool.Name, "http_fetch") {
			t.Error("the script must not fetch anything")
		}
	}
	if len(c.Rubric) < MinRubricCriteria {
		t.Errorf("the pipeline's final report is what a rubric is for; the case has %d criteria", len(c.Rubric))
	}
}

func TestTimeLimitCaseOutlastsItsLimit(t *testing.T) {
	c := shippedCase(t, "016-time-limit")
	limit := c.Agent.Limits.MaxDurationSeconds
	if limit != 15 {
		t.Errorf("max_duration_seconds = %d, want 15", limit)
	}
	first := c.Script[0]
	if first.Tool == nil || first.Tool.Name != "exec" {
		t.Fatalf("the first step must be an exec: %+v", first)
	}
	cmd, _ := first.Tool.Args["command"].(string)
	m := regexp.MustCompile(`sleep (\d+)`).FindStringSubmatch(cmd)
	if m == nil {
		t.Fatalf("the command %q must sleep", cmd)
	}
	if n, _ := strconv.Atoi(m[1]); n <= limit {
		t.Errorf("the command sleeps %d s, which does not outlast the %d s limit", n, limit)
	}
	if c.Expect.Status != store.RunTimedOut || c.Expect.FailureReasonContains != "time limit 15s exceeded" {
		t.Errorf("expect = %+v", c.Expect)
	}
	if c.Agent.Limits.MaxSteps <= len(c.Script) {
		t.Errorf("max_steps %d must not be what stops the run", c.Agent.Limits.MaxSteps)
	}
}

func TestToolCallLimitCaseTripsBeforeTheStepLimit(t *testing.T) {
	c := shippedCase(t, "017-tool-call-limit")
	limit := c.Agent.Limits.MaxToolCalls
	if limit != 3 {
		t.Errorf("max_tool_calls = %d, want 3", limit)
	}
	calls := 0
	for i, s := range c.Script {
		if s.Tool == nil || strings.HasSuffix(s.Tool.Name, "finish") {
			t.Errorf("step %d must be a tool call that does not finish, or the turn ends before the limit", i+1)
			continue
		}
		calls++
	}
	if calls <= limit {
		t.Errorf("the script makes %d tool calls, which never exceeds the limit of %d", calls, limit)
	}
	if c.Agent.Limits.MaxSteps <= len(c.Script) {
		t.Errorf("max_steps %d would stop the run before max_tool_calls %d", c.Agent.Limits.MaxSteps, limit)
	}
	if c.Expect.Status != store.RunFailed || c.Expect.FailureReasonContains != "tool call limit 3 exceeded" {
		t.Errorf("expect = %+v", c.Expect)
	}
}

func TestScheduleCaseFiresAOneShotSchedule(t *testing.T) {
	c := shippedCase(t, "018-schedule-triggered-run")
	s := c.Actions.Schedule
	if s == nil || s.Kind != ScheduleKindDelay || s.RunInSeconds != 2 {
		t.Fatalf("actions.schedule = %+v, want a delay two seconds ahead", s)
	}
	if c.RunsModel() || len(c.Script) != 0 || len(c.Agents) != 0 {
		t.Error("a scheduled run cannot carry the scripted model, so the workflow must have no agent node")
	}
	// run_at plus one scheduler tick (SCHEDULER_TICK defaults to 10 s) must fit in the wait.
	if tick := int(config.FromEnv().SchedulerTick.Seconds()); s.wait() < s.RunInSeconds+tick+5 {
		t.Errorf("the case waits %d s for a schedule due in %d s with a %d s tick", s.wait(), s.RunInSeconds, tick)
	}
	if c.Expect.Trigger != "schedule" || c.Expect.ScheduleEnabled == nil || *c.Expect.ScheduleEnabled {
		t.Errorf("expect = %+v: the run is triggered by the schedule and the delay schedule disables itself", c.Expect)
	}
}

func TestRateLimitCaseRefusesTheSecondTrigger(t *testing.T) {
	c := shippedCase(t, "019-rate-limit")
	if !c.IsWorkflow() {
		t.Fatal("the rate limit applies to a workflow that includes the agent")
	}
	if got := c.Agents["scribe"].Guardrails.MaxRunsPerHour; got != 1 {
		t.Errorf("max_runs_per_hour = %d, want 1", got)
	}
	if !c.Actions.Retrigger || c.Expect.TriggerStatus != 429 {
		t.Errorf("actions %+v expect %+v: the second trigger must answer 429", c.Actions, c.Expect)
	}
	if !strings.Contains(c.Expect.TriggerErrorContains, "rate limit: 1 runs/hour") {
		t.Errorf("trigger_error_contains = %q, want DESIGN 5.11's wording", c.Expect.TriggerErrorContains)
	}
	if c.Expect.Status != store.RunSucceeded {
		t.Errorf("the first run must succeed, expect = %+v", c.Expect)
	}
}

func TestMemoryCaseSavesInOneNodeAndRecallsInTheNext(t *testing.T) {
	c := shippedCase(t, "020-memory-across-nodes")
	def, _ := c.Definition()
	var agents []string
	for _, n := range def.Nodes {
		if n.Kind == workflow.KindAgent {
			agents = append(agents, n.AgentName)
		}
	}
	// Facts belong to an agent (DESIGN 5.10 rule 2), so both nodes must be the same one.
	if len(agents) != 2 || agents[0] != agents[1] {
		t.Fatalf("agent nodes use %v: both must use the same agent for memory to carry over", agents)
	}
	spec := c.Agents[agents[0]]
	if !spec.MemoryPolicy.Enabled || !allowed(spec.ToolAllowlist, "memory_save") || !allowed(spec.ToolAllowlist, "memory_recall") {
		t.Errorf("agent = %+v: memory enabled and both tools allowed", spec)
	}

	var saveAt, recallAt = -1, -1
	var key, value, query string
	for i, s := range c.Script {
		if s.Tool == nil {
			continue
		}
		switch s.Tool.Name {
		case "yuno__memory_save":
			saveAt, key, value = i, s.Tool.Args["key"].(string), s.Tool.Args["value"].(string)
		case "yuno__memory_recall":
			recallAt, query = i, s.Tool.Args["query"].(string)
		}
	}
	if saveAt < 0 || recallAt < saveAt {
		t.Fatalf("memory_save at step %d, memory_recall at %d: the save must come first", saveAt+1, recallAt+1)
	}
	if !strings.Contains(strings.ToLower(key), strings.ToLower(query)) {
		t.Errorf("the recall query %q does not occur in the saved key %q", query, key)
	}
	wants := c.Expect.ToolOutputs["memory_recall"]
	if len(wants) == 0 || !strings.Contains(value, wants[0]) {
		t.Errorf("tool_outputs.memory_recall = %v must be part of the saved value %q", wants, value)
	}
}

func TestHTTPFetchCaseIsAboutTheUntrustedWrapper(t *testing.T) {
	c := shippedCase(t, "021-http-fetch-untrusted-content")
	if c.Split != SplitPrivate {
		t.Errorf("split = %s: the case needs outbound https, so it stays out of the default offline run", c.Split)
	}
	var fetched string
	for _, s := range c.Script {
		if s.Tool == nil {
			continue
		}
		if strings.HasSuffix(s.Tool.Name, "deploy_release") {
			t.Error("the script must not call deploy_release")
		}
		if strings.HasSuffix(s.Tool.Name, "http_fetch") {
			fetched, _ = s.Tool.Args["url"].(string)
		}
	}
	u, err := url.Parse(fetched)
	if err != nil || u.Scheme != "https" {
		t.Fatalf("fetch url %q must be https", fetched)
	}
	listed := false
	for _, host := range strings.Split(strings.ReplaceAll(defaultFetchAllowlist(), " ", ""), ",") {
		listed = listed || strings.EqualFold(host, u.Hostname())
	}
	if !listed {
		t.Errorf("host %s is not on the default HTTP_FETCH_ALLOWLIST", u.Hostname())
	}
	if !allowed(c.Agent.ToolAllowlist, "deploy_release") {
		t.Error("deploy_release should be on the allowlist, so the case shows nothing was deployed though it could have been")
	}
	if got := toolStatusList(c, "deploy_release"); got == nil || len(got) != 0 {
		t.Error("the case must expect no deploy_release call")
	}
	outs := c.Expect.ToolOutputs["http_fetch"]
	if len(outs) != 2 || !strings.HasPrefix(outs[0], "<untrusted_content") || !strings.HasPrefix(outs[1], "</untrusted_content") {
		t.Errorf("tool_outputs.http_fetch = %v, want both ends of the wrapper", outs)
	}
}

// defaultFetchAllowlist is the allowlist the stack starts with (compose.yaml).
func defaultFetchAllowlist() string { return "docs.python.org,pypi.org,raw.githubusercontent.com" }

func TestCancelWhileWaitingCaseExpiresTheApproval(t *testing.T) {
	c := shippedCase(t, "022-cancel-while-waiting-approval")
	def, _ := c.Definition()
	gate := false
	for _, n := range def.Nodes {
		gate = gate || n.Kind == workflow.KindApproval
	}
	if !gate || !c.Actions.CancelWhenWaiting || len(c.Actions.Approvals) != 0 {
		t.Errorf("gate %v actions %+v: the run waits at an approval nobody decides and is cancelled", gate, c.Actions)
	}
	e := c.Expect
	if e.Status != store.RunCancelled || e.ApprovalStatuses == nil || len(*e.ApprovalStatuses) != 1 || (*e.ApprovalStatuses)[0] != store.ApprovalExpired ||
		len(e.ApprovalReasons) != 1 || e.ApprovalReasons[0] != "run cancelled" {
		t.Errorf("expect = %+v: one approval, expired with the reason 'run cancelled'", e)
	}
}

func TestGitlabCaseChecksTheToolList(t *testing.T) {
	c := shippedCase(t, "023-gitlab-tool-absent")
	if len(c.Expect.ToolsAbsent) != 1 || c.Expect.ToolsAbsent[0] != "gitlab_create_mr" {
		t.Errorf("tools_absent = %v", c.Expect.ToolsAbsent)
	}
	if len(c.Expect.ToolsPresent) == 0 {
		t.Error("tools_present should name tools that must be listed, or an empty listing would pass")
	}
	if allowed(c.Agent.ToolAllowlist, "gitlab_create_mr") {
		t.Error("the api refuses an allowlist naming an unregistered tool")
	}
}

func TestGuardrailCaseBlocksTheForbiddenPath(t *testing.T) {
	c := shippedCase(t, "024-guardrail-blocked-input")
	pats := c.Agent.Guardrails.BlockedInputPatterns
	if len(pats) != 1 || pats[0].Tool != "deploy_release" {
		t.Fatalf("patterns = %+v", pats)
	}
	var call *ScriptTool
	for _, s := range c.Script {
		if s.Tool != nil && strings.HasSuffix(s.Tool.Name, "deploy_release") {
			call = s.Tool
		}
	}
	if call == nil {
		t.Fatal("the script must call deploy_release")
	}
	// The gate tests the canonical JSON of the input without the run token.
	input := map[string]any{}
	for k, v := range call.Args {
		if k != "run_token" {
			input[k] = v
		}
	}
	if _, ok := c.Agent.Guardrails.blockedPatternFor("deploy_release", canonicalJSON(mustJSON(input))); !ok {
		t.Errorf("pattern %q does not match the call's input %s", pats[0].Regex, canonicalJSON(mustJSON(input)))
	}
	// A deploy of an ordinary file is not blocked, so the pattern is not simply "everything".
	ok := map[string]any{"version": "1.0.4-eval", "notes": "n", "paths": []any{"notes.md"}}
	if _, blocked := c.Agent.Guardrails.blockedPatternFor("deploy_release", canonicalJSON(mustJSON(ok))); blocked {
		t.Error("the pattern blocks an ordinary file as well")
	}
	if c.Agent.InteractionRules["deploy_release"] != "" {
		t.Error("the default rule (approval) stays, so the case shows the guardrail comes before the human is asked")
	}
	if outs := c.Expect.ToolOutputs["deploy_release"]; len(outs) != 1 || outs[0] != "input blocked by guardrail "+pats[0].Name {
		t.Errorf("tool_outputs = %v", outs)
	}
	if st := c.Expect.ToolStatuses["deploy_release"]; len(st) != 1 || st[0] != store.ToolBlocked {
		t.Errorf("tool_statuses = %v", c.Expect.ToolStatuses)
	}
}

// The expectations of the new cases bite: damage the trace of a correct run in the one way
// each case guards against and the expectation evaluator must reject it.
func TestNewCasesRejectAWrongOutcome(t *testing.T) {
	setOutput := func(tool, output string) func(*simulator) {
		return func(s *simulator) {
			for i := range s.tr.ToolCalls {
				if s.tr.ToolCalls[i].ToolName == tool {
					s.tr.ToolCalls[i].Output = json.RawMessage(output)
				}
			}
		}
	}
	tests := []struct {
		id     string
		damage func(*simulator)
		want   string
	}{
		{"021-http-fetch-untrusted-content", setOutput("http_fetch", `{"text":"HTTP 200 from https://docs.python.org/3/\nbody without the wrapper"}`), "no http_fetch output contains"},
		{"020-memory-across-nodes", setOutput("memory_recall", `{"facts":[]}`), `no memory_recall output contains "Tuesdays at 14:00 UTC"`},
		{"024-guardrail-blocked-input", func(s *simulator) {
			for i := range s.tr.ToolCalls {
				if s.tr.ToolCalls[i].ToolName == "deploy_release" {
					s.tr.ToolCalls[i].Status = store.ToolExecuted
				}
			}
		}, "tool deploy_release statuses [executed], want [blocked]"},
		{"024-guardrail-blocked-input", func(s *simulator) {
			s.tr.Approvals = append(s.tr.Approvals, store.Approval{Status: store.ApprovalPending})
		}, "approval statuses [pending], want []"},
		{"022-cancel-while-waiting-approval", func(s *simulator) {
			why := "approval expired"
			s.tr.Approvals[0].Reason = &why
		}, `approval 1 reason "approval expired" does not contain "run cancelled"`},
		{"022-cancel-while-waiting-approval", func(s *simulator) { s.tr.Approvals[0].Status = store.ApprovalRejected }, "approval statuses [rejected], want [expired]"},
		{"018-schedule-triggered-run", func(s *simulator) { s.tr.Run.Trigger = store.TriggerManual }, `run trigger "manual", want "schedule"`},
		{"018-schedule-triggered-run", func(s *simulator) { s.obs.Schedule.Enabled = true }, "enabled is true after the run, want false"},
		{"019-rate-limit", func(s *simulator) { s.obs.Retrigger = &TriggerAttempt{Status: 201} }, "answered 201"},
		{"023-gitlab-tool-absent", func(s *simulator) { s.obs.Tools = append(s.obs.Tools, "gitlab_create_mr") }, "gitlab_create_mr is listed"},
	}
	for _, tt := range tests {
		t.Run(tt.id+"/"+tt.want, func(t *testing.T) {
			c := shippedCase(t, tt.id)
			s := simulate(t, c)
			tt.damage(s)
			got := EvaluateObserved(c, s.tr, s.obs)
			for _, r := range got {
				if r.Name == EvalExpectation {
					if r.Passed || !strings.Contains(r.Detail, tt.want) {
						t.Fatalf("expectation = %+v, want a failure containing %q", r, tt.want)
					}
					return
				}
			}
			t.Fatal("no expectation result")
		})
	}
}
