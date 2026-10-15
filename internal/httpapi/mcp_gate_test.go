package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"yuno/internal/deploytarget"
	"yuno/internal/engine"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// gateFixture is a run visit with a live token, a workspace holding runs/<id>/app.py,
// a counting deploy target and a server whose fake engine scripts the approvals.
type gateFixture struct {
	s     *Server
	ts    *httptest.Server
	fe    *fakeEngine
	run   *store.Run
	visit *store.RunNode
	agent *store.Agent
	posts *atomic.Int32
	token string
}

func newGateFixture(t *testing.T, allow []string, rules map[string]string, wait time.Duration) *gateFixture {
	t.Helper()
	pool := needDB(t)
	ctx := context.Background()
	g := &gateFixture{token: "r_gate" + strings.ReplaceAll(store.NewID(), "-", "")[20:], posts: &atomic.Int32{}}

	rel, err := deploytarget.Open(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	h := deploytarget.Handler(rel, slog.New(slog.DiscardHandler))
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			g.posts.Add(1)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(target.Close)

	ocID := "agt" + strings.ReplaceAll(store.NewID(), "-", "")[20:]
	g.agent = &store.Agent{Name: "gate-test", Model: "claude-opus-5-5", OpenClawAgentID: &ocID, ToolAllowlist: allow, InteractionRules: rules}
	mustDo(t, store.CreateAgent(ctx, pool, g.agent))
	wf := &store.Workflow{Name: "gate test", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(t, store.CreateWorkflow(ctx, pool, wf))
	g.run = &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: store.TriggerManual, Status: store.RunRunning}
	mustDo(t, store.CreateRun(ctx, pool, g.run))
	now := time.Now().UTC()
	g.visit = &store.RunNode{RunID: g.run.ID, NodeID: "deployer", AgentID: &g.agent.ID, Status: store.NodeRunning, StartedAt: &now}
	mustDo(t, store.CreateRunNode(ctx, pool, g.visit))

	cfg := testConfig()
	cfg.OpenClawStateDir = t.TempDir()
	cfg.MCPToolWait = wait
	cfg.DeployTargetURL = target.URL
	cfg.DeployTargetMaxMB = 1
	runDir := filepath.Join(cfg.OpenClawStateDir, "workspace-"+ocID, "runs", g.run.ID)
	mustDo(t, os.MkdirAll(runDir, 0o755))
	mustDo(t, os.WriteFile(filepath.Join(runDir, "app.py"), []byte("print('hi')"), 0o644))

	g.fe = &fakeEngine{pool: pool, tokens: map[string]engine.RunContext{g.token: {
		RunID: g.run.ID, RunNodeID: g.visit.ID, NodeID: "deployer", AgentID: g.agent.ID, OpenClawAgentID: ocID,
		ToolAllowlist: allow, InteractionRules: rules,
	}}}
	g.s, g.ts = newTestServerDeps(t, Deps{Cfg: cfg, Pool: pool}, g.fe)
	return g
}

// call sends one MCP tool call and returns the text and isError.
func (g *gateFixture) call(t *testing.T, tool string, args map[string]any) (string, bool) {
	t.Helper()
	cs, err := connectMCP(t, g.ts.URL, testMCPToken)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	return res.Content[0].(*mcp.TextContent).Text, res.IsError
}

func (g *gateFixture) deployArgs() map[string]any {
	return map[string]any{"run_token": g.token, "version": "1.0.0", "notes": "first release", "paths": []string{"app.py"}}
}

func (g *gateFixture) rows(t *testing.T) []store.ToolCall {
	t.Helper()
	rows, err := store.ListToolCalls(context.Background(), testPool, g.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func (g *gateFixture) wantKey(t *testing.T, args map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(args)
	key, err := tools.IdempotencyKey(g.run.ID, g.visit.ID, tools.DeployReleaseName, raw)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// checkRow asserts what every MCP-recorded row carries (common.md).
func checkRow(t *testing.T, row store.ToolCall, tool, status string) {
	t.Helper()
	if row.ToolName != tool || row.Status != status || !strings.HasPrefix(row.ToolUseID, "mcp-") || row.ModelCallID != nil {
		t.Fatalf("row = %s %s use %s model_call %v, want %s %s", row.ToolName, row.Status, row.ToolUseID, row.ModelCallID, tool, status)
	}
	if strings.Contains(string(row.Input), "r_gate") {
		t.Fatalf("run token stored in the input: %s", row.Input)
	}
}

// T5.3.3: a gated side effect blocks the MCP call; approved within the wait, it runs
// once and the result returns in the same call.
func TestMCPGateApprovedWithinWait(t *testing.T) {
	g := newGateFixture(t, []string{"read", "deploy_release"}, nil, 5*time.Second)
	g.fe.waitOutcome = "approve"
	args := g.deployArgs()
	text, isErr := g.call(t, "deploy_release", args)
	var out struct {
		ReleaseID string `json:"release_id"`
		Version   string `json:"version"`
		URL       string `json:"url"`
		Policy    string `json:"policy"`
	}
	if isErr || json.Unmarshal([]byte(text), &out) != nil || out.ReleaseID == "" || out.Version != "1.0.0" || out.Policy != "" {
		t.Fatalf("result %v %s", isErr, text)
	}
	if n := g.posts.Load(); n != 1 {
		t.Fatalf("deploy target called %d times", n)
	}
	rows := g.rows(t)
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	checkRow(t, rows[0], "deploy_release", store.ToolExecuted)
	if rows[0].IdempotencyKey == nil || *rows[0].IdempotencyKey != g.wantKey(t, args) {
		t.Fatalf("idempotency key %v", rows[0].IdempotencyKey)
	}
	apprs, _ := store.ListApprovals(context.Background(), testPool, "", g.run.ID, 10)
	if len(apprs) != 1 || apprs[0].ToolCallID == nil || *apprs[0].ToolCallID != rows[0].ID || apprs[0].Status != store.ApprovalApproved {
		t.Fatalf("approvals = %+v", apprs)
	}
	req := g.fe.approvalReqs[0]
	if req.ToolName != "deploy_release" || req.Token != g.token || req.Message != "Deploy release 1.0.0 (app.py)" ||
		strings.Contains(string(req.Input), g.token) {
		t.Fatalf("approval request = %+v", req)
	}
	run, _ := store.GetRun(context.Background(), testPool, g.run.ID)
	if run.ToolCallCount != 1 {
		t.Fatalf("tool_call_count = %d", run.ToolCallCount)
	}
}

// T5.3.4: rejected (or expired) → a rejected status result, not an error; no effect.
func TestMCPGateRejected(t *testing.T) {
	tests := []struct {
		outcome, reason, wantText, wantStatus string
	}{
		{"reject", "not on a Friday", `{"status":"rejected","reason":"not on a Friday"}`, store.ToolRejected},
		{"reject", "", `{"status":"rejected","reason":"rejected by the operator"}`, store.ToolRejected},
		{"expire", "", `{"status":"rejected","reason":"approval expired"}`, store.ToolExpired},
	}
	for _, tc := range tests {
		t.Run(tc.outcome+"/"+tc.reason, func(t *testing.T) {
			g := newGateFixture(t, []string{"deploy_release"}, nil, 5*time.Second)
			g.fe.waitOutcome, g.fe.waitReason = tc.outcome, tc.reason
			text, isErr := g.call(t, "deploy_release", g.deployArgs())
			if isErr || text != tc.wantText {
				t.Fatalf("result %v %s, want %s", isErr, text, tc.wantText)
			}
			if g.posts.Load() != 0 {
				t.Fatal("a refused side effect reached the deploy target")
			}
			rows := g.rows(t)
			checkRow(t, rows[0], "deploy_release", tc.wantStatus)
			want, _ := tools.CanonicalJSON(json.RawMessage(tc.wantText))
			if got, _ := tools.CanonicalJSON(rows[0].Output); string(got) != string(want) {
				t.Fatalf("row output %s", rows[0].Output)
			}
		})
	}
}

// T5.3.5 (handler side): the wait ends → pending_approval with the approval id, the
// row stays awaiting_approval; a later grant executes it once through the engine's
// fallback (Deps.Effects) under the same key.
func TestMCPGatePendingThenFallback(t *testing.T) {
	g := newGateFixture(t, []string{"deploy_release"}, nil, 150*time.Millisecond)
	g.fe.waitOutcome = "timeout"
	text, isErr := g.call(t, "deploy_release", g.deployArgs())
	apprs, _ := store.ListApprovals(context.Background(), testPool, "", g.run.ID, 10)
	if len(apprs) != 1 {
		t.Fatalf("approvals = %+v", apprs)
	}
	want := `{"status":"pending_approval","approval_id":"` + apprs[0].ID + `"}`
	if isErr || text != want {
		t.Fatalf("result %v %s, want %s", isErr, text, want)
	}
	rows := g.rows(t)
	checkRow(t, rows[0], "deploy_release", store.ToolAwaitingApproval)
	if g.posts.Load() != 0 {
		t.Fatal("executed before the approval")
	}

	ctx := context.Background()
	if _, err := store.DecideApprovalAt(ctx, testPool, apprs[0].ID, store.ApprovalApproved, "api", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	var effects engine.SideEffectExecutor = g.s.effects
	out, isErr, err := effects.ExecuteApproved(ctx, rows[0].ID)
	if err != nil || isErr || !strings.Contains(string(out), "release_id") || g.posts.Load() != 1 {
		t.Fatalf("fallback: %s %v %v (posts %d)", out, isErr, err, g.posts.Load())
	}
	if _, _, err := effects.ExecuteApproved(ctx, rows[0].ID); err != nil || g.posts.Load() != 1 {
		t.Fatalf("second fallback executed again: %v (posts %d)", err, g.posts.Load())
	}
	checkRow(t, g.rows(t)[0], "deploy_release", store.ToolExecuted)
}

// T5.3.6: with the operator's auto rule the side effect runs without an approval and
// the audit row says so; the same call again in the visit replays (T5.4.1).
func TestMCPGateAuto(t *testing.T) {
	g := newGateFixture(t, []string{"deploy_release"}, map[string]string{"deploy_release": "auto"}, time.Second)
	text, isErr := g.call(t, "deploy_release", g.deployArgs())
	if isErr || !strings.Contains(text, `"policy":"auto"`) || strings.Contains(text, "replayed") {
		t.Fatalf("first: %v %s", isErr, text)
	}
	text2, isErr := g.call(t, "deploy_release", g.deployArgs())
	if isErr || !strings.Contains(text2, `"replayed":true`) || !strings.Contains(text2, `"policy":"auto"`) {
		t.Fatalf("second: %v %s", isErr, text2)
	}
	if len(g.fe.approvalReqs) != 0 || g.posts.Load() != 1 {
		t.Fatalf("approvals requested %d, posts %d", len(g.fe.approvalReqs), g.posts.Load())
	}
	rows := g.rows(t)
	if len(rows) != 2 || *rows[0].IdempotencyKey != *rows[1].IdempotencyKey {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		checkRow(t, r, "deploy_release", store.ToolExecuted)
		if !strings.Contains(string(r.Output), `"policy": "auto"`) {
			t.Fatalf("row output %s", r.Output)
		}
	}
	se, _ := store.ListSideEffects(context.Background(), testPool, g.run.ID)
	if len(se) != 1 || se[0].ToolCallID != rows[0].ID {
		t.Fatalf("side effects = %+v", se)
	}
	evs, _ := store.RunEventsAfter(context.Background(), testPool, g.run.ID, 0, 100)
	n := 0
	for _, e := range evs {
		if e.Type == "tool_call" {
			n++
		}
	}
	if n < 2 {
		t.Fatalf("tool_call events = %d", n)
	}
}

// T5.7.7: a blocked tool (rule blocked, or missing from the allowlist) is refused
// server side and recorded as blocked.
func TestMCPGateBlocked(t *testing.T) {
	tests := []struct {
		name  string
		allow []string
		rules map[string]string
	}{
		{"rule blocked", []string{"deploy_release"}, map[string]string{"deploy_release": "blocked"}},
		{"not allowlisted", []string{"read"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateFixture(t, tc.allow, tc.rules, time.Second)
			text, isErr := g.call(t, "deploy_release", g.deployArgs())
			if !isErr || text != "tool deploy_release is blocked for this agent" {
				t.Fatalf("result %v %s", isErr, text)
			}
			if g.posts.Load() != 0 || len(g.fe.approvalReqs) != 0 {
				t.Fatal("a blocked tool ran or asked for approval")
			}
			checkRow(t, g.rows(t)[0], "deploy_release", store.ToolBlocked)
		})
	}
}

// T5.7.2: an input that violates the tool schema gets the schema message and a failed
// row; T5.7.1 an unknown token gets "invalid run token" and no row.
func TestMCPGateInvalidCalls(t *testing.T) {
	g := newGateFixture(t, []string{"deploy_release"}, nil, time.Second)
	args := g.deployArgs()
	delete(args, "version")
	text, isErr := g.call(t, "deploy_release", args)
	if !isErr || !strings.Contains(text, "missing property 'version'") {
		t.Fatalf("schema violation: %v %s", isErr, text)
	}
	rows := g.rows(t)
	checkRow(t, rows[0], "deploy_release", store.ToolFailed)
	if !rows[0].IsError || !strings.Contains(string(rows[0].Output), "missing property") {
		t.Fatalf("row = %+v", rows[0])
	}

	args = g.deployArgs()
	args["run_token"] = "r_unknown"
	if text, isErr := g.call(t, "deploy_release", args); !isErr || text != "invalid run token" {
		t.Fatalf("unknown token: %v %s", isErr, text)
	}
	if n := len(g.rows(t)); n != 1 {
		t.Fatalf("an unknown token recorded a row (%d rows)", n)
	}
}

// The approval cannot be requested (engine refuses): the model gets an error and the
// row is failed, never left awaiting.
func TestMCPGateRequestFails(t *testing.T) {
	g := newGateFixture(t, []string{"deploy_release"}, nil, time.Second)
	g.fe.requestErr = engine.ErrNotImplemented
	text, isErr := g.call(t, "deploy_release", g.deployArgs())
	if !isErr || !strings.Contains(text, "approval could not be requested") {
		t.Fatalf("result %v %s", isErr, text)
	}
	checkRow(t, g.rows(t)[0], "deploy_release", store.ToolFailed)
}

// finish rows are recorded by the MCP handler now (common.md), and T5.7.3: a long
// result is capped with the marker before it is returned and stored.
func TestMCPFinishRowAndTruncation(t *testing.T) {
	g := newGateFixture(t, nil, nil, time.Second)
	text, isErr := g.call(t, "finish", map[string]any{"run_token": g.token, "result": map[string]any{"summary": "ok"}})
	if isErr || text != `{"status":"accepted"}` {
		t.Fatalf("finish: %v %s", isErr, text)
	}
	g.fe.finishErr = &engine.FinishValidationError{Message: "result at /summary: " + strings.Repeat("x", 40000)}
	text, isErr = g.call(t, "finish", map[string]any{"run_token": g.token, "result": map[string]any{"summary": 1}})
	if !isErr || !strings.Contains(text, "[truncated ") || len(text) > testConfig().ToolOutputMaxBytes+64 {
		t.Fatalf("long result not capped: %v, %d bytes", isErr, len(text))
	}
	rows := g.rows(t)
	if len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
	checkRow(t, rows[0], "finish", store.ToolExecuted)
	checkRow(t, rows[1], "finish", store.ToolFailed)
	if rows[0].IdempotencyKey != nil || !strings.Contains(string(rows[1].Output), "[truncated ") {
		t.Fatalf("rows = %s / %s", rows[0].Output, rows[1].Output)
	}
	run, _ := store.GetRun(context.Background(), testPool, g.run.ID)
	if run.ToolCallCount != 2 {
		t.Fatalf("tool_call_count = %d", run.ToolCallCount)
	}
}
