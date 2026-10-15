package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
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
	_, ts := newTestServer(t, pool, fe, adminGateway())

	// A run with one of every trace row, so the trace exercises every schema.
	run, err := createTestRun(ctx, pool, store.TriggerManual, json.RawMessage(`{"repo":"demo"}`), store.Limits{MaxSteps: 10})
	if err != nil {
		t.Fatal(err)
	}
	seedTrace(t, run.ID)

	steps := []struct {
		method, path, body string
		want               int
	}{
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
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
