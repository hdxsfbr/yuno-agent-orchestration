package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"yuno/api"
	"yuno/internal/store"
)

// contract loads api/openapi.yaml and a router over it.
func contract(t *testing.T) routers.Router {
	t.Helper()
	ctx := context.Background()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(api.OpenAPI)
	if err != nil {
		t.Fatalf("load openapi.yaml: %v", err)
	}
	if err := doc.Validate(ctx); err != nil {
		t.Fatalf("openapi.yaml is invalid: %v", err)
	}
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	return router
}

// checkConformance sends a request and validates the response against the contract,
// and the request too when it is meant to be valid. It returns the status and body.
func checkConformance(t *testing.T, router routers.Router, baseURL, method, path, body string, validRequest bool) (int, []byte) {
	t.Helper()
	ctx := context.Background()
	newReq := func() *http.Request {
		var rd io.Reader
		if body != "" {
			rd = bytes.NewReader([]byte(body))
		}
		req, err := http.NewRequest(method, baseURL+path, rd)
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		return req
	}
	// Route on the path alone; the document's server is "/".
	routeReq := newReq()
	routeReq.URL.Scheme, routeReq.URL.Host, routeReq.Host = "", "", ""
	route, params, err := router.FindRoute(routeReq)
	if err != nil {
		t.Fatalf("%s %s is not in the contract: %v", method, path, err)
	}
	reqInput := &openapi3filter.RequestValidationInput{Request: routeReq, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}}
	if err := openapi3filter.ValidateRequest(ctx, reqInput); validRequest && err != nil {
		t.Fatalf("%s %s: request does not match the contract: %v", method, path, err)
	}

	resp, err := http.DefaultClient.Do(newReq())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: reqInput,
		Status:                 resp.StatusCode,
		Header:                 resp.Header,
		Options:                &openapi3filter.Options{IncludeResponseStatus: true, MultiError: true},
	}
	in.SetBodyBytes(respBody)
	if err := openapi3filter.ValidateResponse(ctx, in); err != nil {
		t.Fatalf("%s %s -> %d: response does not match the contract: %v\n%s", method, path, resp.StatusCode, err, respBody)
	}
	return resp.StatusCode, respBody
}

func TestOpenAPIConformance(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	router := contract(t)
	fe := &fakeEngine{pool: pool}
	tplDir := t.TempDir()
	_, checkTransport := newFakeProvider(t)
	srv, ts := newTestServerDeps(t, Deps{Cfg: testConfig(), Pool: pool, Gateway: adminGateway(), TemplatesDir: tplDir,
		CheckTransport: checkTransport}, fe)
	conformanceTemplates(t, srv, tplDir)

	// A run with one of every trace row, so the trace exercises every schema.
	run, err := createTestRun(ctx, pool, store.TriggerManual, json.RawMessage(`{"repo":"demo"}`), store.Limits{MaxSteps: 10})
	if err != nil {
		t.Fatal(err)
	}
	seedTrace(t, run.ID)

	steps := []conformanceStep{
		{http.MethodPost, "/api/agents", `{"name":"Conformance","model":"claude-opus-5-5","effort":"high","thinking":"adaptive",
			"tool_allowlist":["exec","finish"],"interaction_rules":{"finish":"auto"},"limits":{"max_steps":20},
			"guardrails":{"max_runs_per_hour":5,"blocked_input_patterns":[{"name":"rm","tool":"exec","regex":"rm -rf"}]},
			"memory_policy":{"enabled":true,"max_facts":10},"exec_policy":{"security":"allowlist","ask":"on-miss","allowlist":["ls"]},
			"skills":[],"channels":["telegram"]}`, http.StatusCreated},
		{http.MethodPost, "/api/agents", `{"name":""}`, http.StatusUnprocessableEntity},
		{http.MethodGet, "/api/agents", "", http.StatusOK},
		{http.MethodGet, "/api/runs", "", http.StatusOK},
		{http.MethodGet, "/api/runs?status=queued&limit=5", "", http.StatusOK},
		{http.MethodGet, "/api/runs/" + run.ID, "", http.StatusOK},
		{http.MethodGet, "/api/runs/no-such-run", "", http.StatusNotFound},
		{http.MethodGet, "/api/runs/" + run.ID + "/trace", "", http.StatusOK},
		{http.MethodGet, "/api/tools", "", http.StatusOK},
		{http.MethodGet, "/api/status", "", http.StatusOK},
		{http.MethodGet, "/api/openclaw/status", "", http.StatusOK},
		{http.MethodPost, "/api/runs", `{"agent_id":"ag","task":"t","model":"scripted/scripted-1","script_id":"c1"}`, http.StatusCreated},
		{http.MethodPost, "/api/runs/" + run.ID + "/cancel", "", http.StatusAccepted},
		{http.MethodPost, "/api/evals/scripts", `{"script_id":"conf-1","steps":[{"text":"hi"}]}`, http.StatusNoContent},
	}
	steps = append(steps, workflowConformanceSteps(t, run.ID)...)
	steps = append(steps, projectConformanceSteps(t)...)
	steps = append(steps, projectWave8ConformanceSteps(t)...)
	steps = append(steps, mergeRequestConformanceSteps(t)...)
	steps = append(steps, retryConformanceSteps(t, fe)...)
	steps = append(steps, iterationConformanceSteps(t)...)
	steps = append(steps, intakeConformanceSteps(t)...)
	steps = append(steps, artifactConformanceSteps(t)...)
	steps = append(steps, questionConformanceSteps(t)...)
	steps = append(steps, branchConformanceSteps(t)...)
	var agentID string
	for _, st := range steps {
		code, body := checkConformance(t, router, ts.URL, st.method, st.path, st.body, st.want < 300)
		if code != st.want {
			t.Fatalf("%s %s: status %d, want %d: %s", st.method, st.path, code, st.want, body)
		}
		if st.method == http.MethodPost && st.path == "/api/agents" && code == http.StatusCreated {
			var a store.Agent
			_ = json.Unmarshal(body, &a)
			agentID = a.ID
		}
	}
	for _, st := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/api/agents/" + agentID, "", http.StatusOK},
		{http.MethodPut, "/api/agents/" + agentID, `{"name":"Conformance 2","model":"claude-sonnet-5-5"}`, http.StatusOK},
		{http.MethodGet, "/api/agents/" + agentID + "/runtime", "", http.StatusOK},
		{http.MethodPost, "/api/agents/" + agentID + "/materialize", "", http.StatusOK},
		{http.MethodDelete, "/api/agents/" + agentID, "", http.StatusNoContent},
		{http.MethodGet, "/api/agents/" + agentID, "", http.StatusNotFound},
	} {
		if code, body := checkConformance(t, router, ts.URL, st.method, st.path, st.body, st.want < 300); code != st.want {
			t.Fatalf("%s %s: status %d, want %d: %s", st.method, st.path, code, st.want, body)
		}
	}
}

// projectConformanceSteps exercises every projects path (DESIGN 5.17 rule 3), and a
// run that names a project for the Run schema's project members.
func projectConformanceSteps(t *testing.T) []conformanceStep {
	t.Helper()
	sfx := uniqueSuffix()
	p := seedProject(t, testCipher(t), "conf-"+sfx, acceptedTokenPrefix+sfx)
	run := projectRun(t, p)
	gone := seedProject(t, testCipher(t), "conf-gone-"+sfx, acceptedTokenPrefix+sfx)
	repo := `"repo_url":"https://gitlab.test/group/conf-` + sfx + `.git"`
	return []conformanceStep{
		{http.MethodGet, "/api/projects", "", http.StatusOK},
		{http.MethodGet, "/api/projects/" + p.ID, "", http.StatusOK},
		{http.MethodGet, "/api/projects/no-such-project", "", http.StatusNotFound},
		{http.MethodGet, "/api/runs/" + run.ID, "", http.StatusOK},
		{http.MethodGet, "/api/runs?project_id=" + p.ID + "&limit=5", "", http.StatusOK},
		{http.MethodGet, "/api/runs?project_id=no-such-project", "", http.StatusOK},
		{http.MethodPost, "/api/projects", `{"name":"conf-new-` + sfx + `",` + repo + `,"provider":"","default_branch":"main",
			"sandbox_network":true,"description":"d","token":"` + acceptedTokenPrefix + sfx + `"}`, http.StatusCreated},
		{http.MethodPost, "/api/projects", `{"name":"",` + repo + `}`, http.StatusUnprocessableEntity},
		{http.MethodPut, "/api/projects/" + p.ID, `{"name":"conf-` + sfx + `",` + repo + `,"provider":"gitlab","description":"edited"}`, http.StatusOK},
		{http.MethodPut, "/api/projects/" + p.ID, `{"name":"conf-` + sfx + `","repo_url":"http://gitlab.test/x"}`, http.StatusUnprocessableEntity},
		{http.MethodPut, "/api/projects/no-such-project", `{"name":"x",` + repo + `}`, http.StatusNotFound},
		{http.MethodPost, "/api/projects/" + p.ID + "/check", "", http.StatusOK},
		{http.MethodPost, "/api/projects/no-such-project/check", "", http.StatusNotFound},
		{http.MethodDelete, "/api/projects/" + gone.ID, "", http.StatusNoContent},
		{http.MethodDelete, "/api/projects/" + gone.ID, "", http.StatusNotFound},
		{http.MethodGet, "/api/projects/" + gone.ID, "", http.StatusNotFound},
	}
}

// Response validation accepts properties a schema does not list, so the fields and the
// event the channel and scheduler work added are checked to be documented.
func TestOpenAPIDocumentsChannelAndSchedulerFields(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	// propsOf are a schema's properties, those of its allOf members included.
	var propsOf func(*openapi3.SchemaRef) map[string]bool
	propsOf = func(ref *openapi3.SchemaRef) map[string]bool {
		out := map[string]bool{}
		if ref == nil || ref.Value == nil {
			return out
		}
		for name := range ref.Value.Properties {
			out[name] = true
		}
		for _, member := range ref.Value.AllOf {
			for name := range propsOf(member) {
				out[name] = true
			}
		}
		return out
	}
	for schema, props := range map[string][]string{
		"Approval":            {"conversation_id", "reported_at", "options", "answer"},
		"Status":              {"scheduler"},
		"Conversation":        {"session_key", "message_count", "last_message_at"},
		"ConversationMessage": {"direction", "text", "usage", "run_id"},
		// DESIGN 5.15 (wave 5).
		"ToolCall": {"conversation_id"},
		"Tool":     {"chat"},
		"Run":      {"reported_at", "toolchain", "project_id", "project", "retries", "merge_request", "merge_request_id", "checkout_sha"},
		"Workflow": {"from_template"},
		// DESIGN 5.17 (wave 7).
		"ProjectSettings": {"name", "repo_url", "provider", "default_branch", "sandbox_network"},
		"ProjectLastRun":  {"id", "status", "toolchain", "created_at", "finished_at"},
		// DESIGN 5.18 and 5.19 add the instructions and the two caps to the input, and the
		// spend to what a project returns.
		"ProjectInput": {"name", "repo_url", "provider", "default_branch", "sandbox_network", "description", "token",
			"instructions", "max_cost_usd_per_day", "max_cost_usd_per_month", "spend", "intake"},
		"ProjectCheck": {"ok", "message", "default_branch", "branch_exists", "can_push", "checked_at"},
		// DESIGN 5.18 to 5.21 (wave 8).
		"Project":      {"instructions", "max_cost_usd_per_day", "max_cost_usd_per_month", "spend", "intake"},
		"ProjectSpend": {"day_usd", "month_usd", "day_until", "month_until"},
		"ProjectFact":  {"id", "project_id", "key", "value", "agent_id", "agent_name", "source_run_id", "created_at", "updated_at", "expires_at"},
		"MergeRequest": {"id", "project_id", "run_id", "provider", "number", "url", "source_branch", "target_branch", "title", "state",
			"ci_status", "head_sha", "unresolved_threads", "last_sync_error", "synced_at", "reported_state", "merged_by", "created_at",
			"updated_at", "closed_at"},
		"RunRetry": {"limits"},
		// DESIGN 5.22 and 5.23 (wave 9).
		"RunCreate":          {"merge_request_id"},
		"ScheduleInput":      {"merge_request_id"},
		"Schedule":           {"merge_request_id", "merge_request_number"},
		"ProjectIntakeInput": {"enabled", "label", "workflow_id"},
		"ProjectIntake":      {"enabled", "label", "workflow_id", "workflow_name", "polled_at", "last_error"},
		"IntakePollResult":   {"taken", "closed", "error"},
		// DESIGN 5.24 and 5.25 (wave 10).
		"RunNode":    {"question_id", "branch_id"},
		"AnswerBody": {"answer"},
		"Artifact":   {"id", "run_id", "node_id", "visit", "path", "size", "sha256", "content_type", "generated", "created_at", "updated_at"},
		// DESIGN 5.26 (wave 11).
		"RunBranch":    {"id", "run_id", "fork_visit_id", "fork_id", "head", "status", "current_node", "failure_reason", "started_at", "finished_at"},
		"Trace":        {"branches"},
		"WorkflowNode": {"join", "checkout"},
	} {
		ref := doc.Components.Schemas[schema]
		if ref == nil || ref.Value == nil {
			t.Errorf("schema %s is missing", schema)
			continue
		}
		documented := propsOf(ref)
		for _, p := range props {
			if !documented[p] {
				t.Errorf("schema %s does not document %s", schema, p)
			}
		}
	}
	// DESIGN 5.17 rules 7 and 12: Status lost its project; ApprovalItem has run_status.
	if doc.Components.Schemas["Status"].Value.Properties["project"] != nil {
		t.Error("schema Status still documents project")
	}
	if item := doc.Components.Schemas["ApprovalItem"].Value; len(item.AllOf) != 2 || item.AllOf[1].Value.Properties["run_status"] == nil {
		t.Error("schema ApprovalItem does not document run_status")
	}
	events := doc.Paths.Find("/api/events")
	if events == nil || events.Get == nil || !strings.Contains(events.Get.Description, "conversation.message") ||
		!strings.Contains(events.Get.Description, "conversation.tool_call") {
		t.Error("GET /api/events does not document the conversation events")
	}
	if events == nil || events.Get == nil || !strings.Contains(events.Get.Description, "run.retry") {
		t.Error("GET /api/events does not document run.retry")
	}
	if events == nil || events.Get == nil || !strings.Contains(events.Get.Description, "run.artifacts") {
		t.Error("GET /api/events does not document run.artifacts")
	}
	if p := doc.Paths.Find("/api/conversations/{id}/tool-calls"); p == nil || p.Get == nil {
		t.Error("GET /api/conversations/{id}/tool-calls is not documented")
	}
}

// seedTrace inserts one node, model call, tool call, span, agent message and approval.
func seedTrace(t *testing.T, runID string) {
	t.Helper()
	ctx := context.Background()
	pool := testPool
	now := time.Now().UTC()
	node := &store.RunNode{RunID: runID, NodeID: "coder", Status: store.NodeRunning, StartedAt: &now,
		Messages: json.RawMessage(`[{"role":"user","content":"task"}]`)}
	mustDo(t, store.CreateRunNode(ctx, pool, node))
	reqID := "req_1"
	mc := &store.ModelCall{RunID: runID, RunNodeID: node.ID, Provider: "scripted", Model: "scripted-1", RequestID: &reqID,
		InputTokens: 100, OutputTokens: 10, CostUSDMicros: 150, DurationMs: 12, StartedAt: now, FinishedAt: &now}
	mustDo(t, store.InsertModelCall(ctx, pool, mc))
	mustDo(t, store.InsertToolCall(ctx, pool, &store.ToolCall{RunID: runID, RunNodeID: node.ID, ModelCallID: &mc.ID,
		ToolUseID: "call_1", ToolName: "exec", Input: json.RawMessage(`{"command":"ls"}`), Status: store.ToolExecuted,
		StartedAt: &now, FinishedAt: &now}))
	mustDo(t, store.InsertSpan(ctx, pool, &store.Span{RunID: runID, Kind: "node", Name: "coder", Attrs: json.RawMessage(`{"visit":1}`)}))
	mustDo(t, store.InsertAgentMessage(ctx, pool, &store.AgentMessage{RunID: runID, FromNode: "coder", ToNode: "reviewer",
		Payload: json.RawMessage(`{"diff":"+1"}`)}))
	mustDo(t, store.InsertApproval(ctx, pool, &store.Approval{RunID: runID, RunNodeID: node.ID, Kind: "exec", NodeID: "coder",
		Message: "run rm", ExpiresAt: now.Add(time.Hour)}))
	key := strings.Repeat("c", 48) + store.ShortID(store.NewID()) + store.ShortID(store.NewID())
	gated := &store.ToolCall{RunID: runID, RunNodeID: node.ID, ToolUseID: "mcp-1", ToolName: "deploy_release",
		Input: json.RawMessage(`{"version":"1.0.0","paths":["app.py"]}`), Status: store.ToolAwaitingApproval, IdempotencyKey: &key, StartedAt: &now}
	mustDo(t, store.InsertToolCall(ctx, pool, gated))
	mustDo(t, store.InsertApproval(ctx, pool, &store.Approval{RunID: runID, RunNodeID: node.ID, ToolCallID: &gated.ID, Kind: "tool",
		NodeID: "coder", Message: "Deploy release 1.0.0 (app.py)", ExpiresAt: now.Add(time.Hour)}))
	_, _, err := store.RunSideEffect(ctx, pool, key+"-done", "deploy_release", runID, gated.ID, func(context.Context) (json.RawMessage, error) {
		return json.RawMessage(`{"release_id":"r1","version":"1.0.0","url":"/deploy/releases/r1/"}`), nil
	})
	mustDo(t, err)
}

// conformanceTemplates loads one template naming an existing agent and one naming a
// missing agent.
func conformanceTemplates(t *testing.T, s *Server, dir string) {
	t.Helper()
	sfx := uniqueSuffix()
	testAgent(t, "Conformer-"+sfx)
	writeTemplate(t, dir, "conf_ok_"+sfx, "Conformance", "conformer-"+sfx)
	writeTemplate(t, dir, "conf_missing_"+sfx, "Conformance missing", "Absent-"+sfx)
	mustDo(t, s.LoadTemplates(context.Background()))
}

type conformanceStep struct {
	method, path, body string
	want               int
}

// workflowConformanceSteps exercises the approvals, workflows and templates endpoints.
func workflowConformanceSteps(t *testing.T, runID string) []conformanceStep {
	t.Helper()
	ctx := context.Background()
	apprs, err := store.ListApprovals(ctx, testPool, "", runID, 10)
	mustDo(t, err)
	var toolAppr, execAppr string
	for _, a := range apprs {
		switch a.Kind {
		case "tool":
			toolAppr = a.ID
		case "exec":
			execAppr = a.ID
		}
	}
	agentID := testAgent(t, "wf-conformance")
	wf := &store.Workflow{Name: "conformance", Definition: json.RawMessage(validDefinition(agentID))}
	mustDo(t, store.CreateWorkflow(ctx, testPool, wf))
	keys := map[string]string{}
	tpls, _ := store.ListWorkflows(ctx, testPool)
	for _, w := range tpls {
		if w.TemplateKey != nil && strings.HasPrefix(*w.TemplateKey, "conf_ok_") {
			keys["ok"] = *w.TemplateKey
		}
		if w.TemplateKey != nil && strings.HasPrefix(*w.TemplateKey, "conf_missing_") {
			keys["missing"] = *w.TemplateKey
		}
	}
	return []conformanceStep{
		{http.MethodGet, "/api/approvals?run_id=" + runID, "", http.StatusOK},
		{http.MethodGet, "/api/approvals?status=pending&limit=5", "", http.StatusOK},
		{http.MethodGet, "/api/approvals?status=maybe", "", http.StatusUnprocessableEntity},
		{http.MethodPost, "/api/approvals/" + toolAppr + "/approve", `{"always":true}`, http.StatusUnprocessableEntity},
		{http.MethodPost, "/api/approvals/" + toolAppr + "/approve", `{"reason":"ship it"}`, http.StatusOK},
		{http.MethodPost, "/api/approvals/" + toolAppr + "/reject", `{"reason":"too late"}`, http.StatusConflict},
		{http.MethodPost, "/api/approvals/" + execAppr + "/reject", "", http.StatusOK},
		{http.MethodPost, "/api/approvals/no-such-approval/approve", "", http.StatusNotFound},
		{http.MethodGet, "/api/workflows", "", http.StatusOK},
		{http.MethodPost, "/api/workflows", `{"name":"Conformance flow","description":"d","definition":` + validDefinition(agentID) + `}`, http.StatusCreated},
		{http.MethodPost, "/api/workflows", `{"name":"","definition":{"nodes":[],"edges":[]}}`, http.StatusUnprocessableEntity},
		{http.MethodGet, "/api/workflows/" + wf.ID, "", http.StatusOK},
		{http.MethodGet, "/api/workflows/no-such-workflow", "", http.StatusNotFound},
		{http.MethodPut, "/api/workflows/" + wf.ID, `{"name":"conformance 2","definition":` + validDefinition(agentID) + `}`, http.StatusOK},
		{http.MethodPost, "/api/workflows/validate", `{"definition":` + validDefinition(agentID) + `}`, http.StatusOK},
		{http.MethodPost, "/api/workflows/validate", `{"definition":{"nodes":[{"id":"start","kind":"start"}],"edges":[]}}`, http.StatusUnprocessableEntity},
		{http.MethodGet, "/api/workflow-templates", "", http.StatusOK},
		{http.MethodPost, "/api/workflow-templates/" + keys["ok"] + "/instantiate", `{"name":"From template"}`, http.StatusCreated},
		{http.MethodPost, "/api/workflow-templates/" + keys["missing"] + "/instantiate", "", http.StatusUnprocessableEntity},
		{http.MethodPost, "/api/workflow-templates/no_such_template/instantiate", "", http.StatusNotFound},
		{http.MethodDelete, "/api/workflows/" + wf.ID, "", http.StatusNoContent},
	}
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Every /api operation declares the bearer scheme, described as optional without
// API_TOKEN; health, readiness and the document (at both paths) are open (audit #28).
func TestOpenAPISecurity(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	ref := doc.Components.SecuritySchemes["bearerAuth"]
	if ref == nil || ref.Value == nil {
		t.Fatal("no bearerAuth security scheme")
	}
	if s := ref.Value; s.Type != "http" || s.Scheme != "bearer" || !strings.Contains(s.Description, "API_TOKEN is unset") ||
		!strings.Contains(s.Description, "Optional") {
		t.Fatalf("bearerAuth = %+v", s)
	}
	checked := 0
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			reqs := doc.Security
			if op.Security != nil {
				reqs = *op.Security
			}
			bearer := false
			for _, r := range reqs {
				if _, ok := r["bearerAuth"]; ok {
					bearer = true
				}
			}
			// /api/openapi.yaml is open, as the auth middleware leaves it (platform control A1
			// documents it).
			if isAPI := strings.HasPrefix(path, "/api/") && path != "/api/openapi.yaml"; bearer != isAPI {
				t.Errorf("%s %s: bearerAuth applies = %v, want %v", method, path, bearer, isAPI)
			}
			checked++
		}
	}
	if checked < 40 {
		t.Fatalf("checked %d operations", checked)
	}
}
