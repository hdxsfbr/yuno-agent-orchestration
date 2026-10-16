package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"yuno/internal/store"
)

func TestCreateAgentValidation(t *testing.T) {
	_, ts := newTestServer(t, nil, &fakeEngine{}, nil)
	long := strings.Repeat("n", 81)
	tests := []struct {
		name      string
		body      string
		wantPaths []string
	}{
		{name: "name and model required", body: `{}`, wantPaths: []string{"name", "model"}},
		{name: "blank name", body: `{"name":"  ","model":"claude-opus-5-5"}`, wantPaths: []string{"name"}},
		{name: "long name", body: `{"name":"` + long + `","model":"claude-opus-5-5"}`, wantPaths: []string{"name"}},
		{name: "unknown model", body: `{"name":"a","model":"gpt-5"}`, wantPaths: []string{"model"}},
		{name: "scripted is not an agent model", body: `{"name":"a","model":"scripted"}`, wantPaths: []string{"model"}},
		{name: "enums", body: `{"name":"a","model":"claude-opus-5-5","effort":"huge","thinking":"ultra"}`, wantPaths: []string{"effort", "thinking"}},
		{name: "unknown tool", body: `{"name":"a","model":"claude-opus-5-5","tool_allowlist":["exec","finish","browser"]}`, wantPaths: []string{"tool_allowlist[2]"}},
		{name: "interaction rule value", body: `{"name":"a","model":"claude-opus-5-5","interaction_rules":{"finish":"auto","deploy_release":"maybe"}}`, wantPaths: []string{"interaction_rules.deploy_release"}},
		{name: "interaction rule for an unknown tool", body: `{"name":"a","model":"claude-opus-5-5","interaction_rules":{"rm_rf":"auto","gitlab_create_mr":"auto"}}`, wantPaths: []string{"interaction_rules.gitlab_create_mr", "interaction_rules.rm_rf"}},
		{name: "finish is always auto", body: `{"name":"a","model":"claude-opus-5-5","interaction_rules":{"finish":"blocked"}}`, wantPaths: []string{"interaction_rules.finish"}},
		{name: "require_approval is for platform tools", body: `{"name":"a","model":"claude-opus-5-5","interaction_rules":{"exec":"require_approval","write":"blocked"}}`, wantPaths: []string{"interaction_rules.exec"}},
		{name: "exec policy enums", body: `{"name":"a","model":"claude-opus-5-5","exec_policy":{"security":"open","ask":"never","ask_fallback":"full"}}`, wantPaths: []string{"exec_policy.security", "exec_policy.ask"}},
		{name: "channel", body: `{"name":"a","model":"claude-opus-5-5","channels":["telegram","slack"]}`, wantPaths: []string{"channels[1]"}},
		{name: "max tokens", body: `{"name":"a","model":"claude-opus-5-5","max_tokens":0}`, wantPaths: []string{"max_tokens"}},
		{name: "negative limits", body: `{"name":"a","model":"claude-opus-5-5","limits":{"max_steps":-1,"max_cost_usd":-0.5,"max_tool_calls":0}}`, wantPaths: []string{"limits.max_steps", "limits.max_cost_usd"}},
		{name: "wrong type", body: `{"name":"a","model":"claude-opus-5-5","limits":{"max_steps":"ten"}}`, wantPaths: []string{"limits.max_steps"}},
		{name: "unknown field", body: `{"name":"a","model":"claude-opus-5-5","colour":"red"}`, wantPaths: []string{"colour"}},
		{name: "malformed", body: `{"name":`, wantPaths: []string{""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, body := do(t, ts, http.MethodPost, "/api/agents", tc.body)
			if code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d: %s", code, body)
			}
			if got := detailPaths(t, body); !slices.Equal(got, tc.wantPaths) {
				t.Fatalf("paths = %q, want %q (%s)", got, tc.wantPaths, body)
			}
		})
	}
}

func TestAgentLifecycle(t *testing.T) {
	pool := needDB(t)
	fe := &fakeEngine{pool: pool}
	_, ts := newTestServer(t, pool, fe, nil)

	code, body := do(t, ts, http.MethodPost, "/api/agents", `{"name":"Coder","role":"writes code","model":"claude-opus-5-5",
		"thinking":"adaptive","tool_allowlist":["exec","read","finish"],"interaction_rules":{"finish":"auto"},
		"exec_policy":{"security":"allowlist","ask":"on-miss","ask_fallback":"deny","allowlist":["python3 -m pytest*"]}}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var a store.Agent
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.OpenClawAgentID == nil || *a.OpenClawAgentID != "t-"+a.ID || a.MaterializedHash != "hash-1" {
		t.Fatalf("created agent not materialised: %s", body)
	}
	if a.Effort != "medium" || a.MaxTokens != defaultMaxTokens || a.Thinking != "adaptive" {
		t.Fatalf("defaults: effort %q max_tokens %d thinking %q", a.Effort, a.MaxTokens, a.Thinking)
	}

	// PUT accepts the fetched object back, read-only fields included.
	_, body = do(t, ts, http.MethodGet, "/api/agents/"+a.ID, "")
	var edited map[string]any
	_ = json.Unmarshal(body, &edited)
	delete(edited, "deleted_at")
	edited["name"] = "Coder v2"
	raw, _ := json.Marshal(edited)
	code, body = do(t, ts, http.MethodPut, "/api/agents/"+a.ID, string(raw))
	if code != http.StatusOK || !strings.Contains(string(body), `"name":"Coder v2"`) {
		t.Fatalf("update: %d %s", code, body)
	}
	if len(fe.materialized) != 2 {
		t.Fatalf("materialised %d times, want 2", len(fe.materialized))
	}

	if code, body = do(t, ts, http.MethodPost, "/api/agents/"+a.ID+"/materialize", ""); code != http.StatusOK || !strings.Contains(string(body), `"in_sync":true`) {
		t.Fatalf("materialize: %d %s", code, body)
	}
	if code, body = do(t, ts, http.MethodGet, "/api/agents/"+a.ID+"/runtime", ""); code != http.StatusOK || !strings.Contains(string(body), `"files":["AGENTS.md","SOUL.md"]`) {
		t.Fatalf("runtime: %d %s", code, body)
	}

	code, body = do(t, ts, http.MethodGet, "/api/agents", "")
	if code != http.StatusOK || !strings.Contains(string(body), `"id":"`+a.ID+`"`) {
		t.Fatalf("list: %d", code)
	}

	if code, _ = do(t, ts, http.MethodDelete, "/api/agents/"+a.ID, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	for _, req := range [][2]string{
		{http.MethodGet, "/api/agents/" + a.ID},
		{http.MethodDelete, "/api/agents/" + a.ID},
		{http.MethodPut, "/api/agents/" + a.ID},
		{http.MethodPost, "/api/agents/" + a.ID + "/materialize"},
		{http.MethodGet, "/api/agents/" + a.ID + "/runtime"},
	} {
		body := ""
		if req[0] == http.MethodPut {
			body = `{"name":"x","model":"claude-opus-5-5"}`
		}
		if code, b := do(t, ts, req[0], req[1], body); code != http.StatusNotFound || decodeError(t, b).Error.Code != "not_found" {
			t.Fatalf("%s %s after delete: %d %s", req[0], req[1], code, b)
		}
	}
	if code, _ = do(t, ts, http.MethodGet, "/api/agents", ""); code != http.StatusOK {
		t.Fatalf("list after delete: %d", code)
	}
}

func TestCreateAgentMaterializeFailure(t *testing.T) {
	pool := needDB(t)
	fe := &fakeEngine{pool: pool, materializeErr: errors.New("config.patch: base hash mismatch")}
	_, ts := newTestServer(t, pool, fe, nil)
	code, body := do(t, ts, http.MethodPost, "/api/agents", `{"name":"Reviewer","model":"claude-sonnet-5-5"}`)
	if code != http.StatusBadGateway {
		t.Fatalf("status %d: %s", code, body)
	}
	e := decodeError(t, body)
	var details struct {
		ID string `json:"id"`
	}
	if e.Error.Code != "materialize_failed" || json.Unmarshal(e.Error.Details, &details) != nil || details.ID == "" {
		t.Fatalf("body %s", body)
	}
	// The row exists, unmaterialised.
	code, body = do(t, ts, http.MethodGet, "/api/agents/"+details.ID, "")
	if code != http.StatusOK || strings.Contains(string(body), "openclaw_agent_id") {
		t.Fatalf("get after failed materialise: %d %s", code, body)
	}
	// Update re-materialises with the same rule.
	code, body = do(t, ts, http.MethodPut, "/api/agents/"+details.ID, `{"name":"Reviewer","model":"claude-sonnet-5-5","role":"reviews"}`)
	if code != http.StatusBadGateway || !strings.Contains(string(body), details.ID) {
		t.Fatalf("update: %d %s", code, body)
	}
	fe.runtimeErr = errors.New("gateway disconnected")
	if code, _ = do(t, ts, http.MethodGet, "/api/agents/"+details.ID+"/runtime", ""); code != http.StatusBadGateway {
		t.Fatalf("runtime with gateway down: %d", code)
	}
}

// The new platform tools may be allowlisted and given rules; a side-effecting tool may
// be set to auto (D-009).
func TestAgentAcceptsPlatformToolRules(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	body := `{"name":"Deployer","model":"claude-opus-5-5",
		"tool_allowlist":["read","deploy_release","http_fetch","memory_save","memory_recall","finish"],
		"interaction_rules":{"deploy_release":"auto","http_fetch":"require_approval","memory_save":"blocked","exec":"blocked","finish":"auto"}}`
	code, resp := do(t, ts, http.MethodPost, "/api/agents", body)
	var a store.Agent
	if code != http.StatusCreated || json.Unmarshal(resp, &a) != nil || a.InteractionRules["deploy_release"] != "auto" {
		t.Fatalf("status %d: %s", code, resp)
	}
}

// Deleting an agent takes it out of OpenClaw too; a runtime that refuses does not fail
// the request (audit #5).
func TestDeleteAgentRemovesRuntime(t *testing.T) {
	pool := needDB(t)
	fe := &fakeEngine{pool: pool}
	_, ts := newTestServer(t, pool, fe, nil)
	first, second := testAgent(t, "delete-runtime"), testAgent(t, "delete-refused")
	if code, b := do(t, ts, http.MethodDelete, "/api/agents/"+first, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, b)
	}
	fe.removeErr = errors.New("agents.delete: gateway disconnected")
	if code, b := do(t, ts, http.MethodDelete, "/api/agents/"+second, ""); code != http.StatusNoContent {
		t.Fatalf("delete with the runtime refusing: %d %s", code, b)
	}
	if code, _ := do(t, ts, http.MethodDelete, "/api/agents/"+first, ""); code != http.StatusNotFound {
		t.Fatalf("second delete: %d", code)
	}
	if want := []string{first, second}; !slices.Equal(fe.removed, want) {
		t.Fatalf("removed from the runtime = %v, want %v", fe.removed, want)
	}
}

// The agents list says which channel each agent answers (from the binding) and how many
// of its node visits are in flight (audit #17, #18).
func TestListAgentsChannelsAndActivity(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	unbindAll := func() { _, _ = pool.Exec(ctx, `DELETE FROM channel_bindings`) }
	unbindAll()
	t.Cleanup(unbindAll)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	busy, idle := testAgent(t, "list-busy"), testAgent(t, "list-idle")
	if _, err := store.BindChannel(ctx, pool, store.ChannelTelegram, busy); err != nil {
		t.Fatal(err)
	}
	visit := func(runStatus, nodeStatus string) {
		t.Helper()
		run, err := createTestRun(ctx, pool, store.TriggerManual, nil, store.Limits{})
		mustDo(t, err)
		_, err = pool.Exec(ctx, `UPDATE runs SET status=$2 WHERE id=$1`, run.ID, runStatus)
		mustDo(t, err)
		now := time.Now()
		mustDo(t, store.CreateRunNode(ctx, pool, &store.RunNode{RunID: run.ID, NodeID: "agent", AgentID: &busy, Visit: 1, Status: nodeStatus, StartedAt: &now}))
	}
	visit(store.RunRunning, store.NodeRunning)
	visit(store.RunRunning, store.NodeRunning)
	visit(store.RunWaitingApproval, store.NodeWaitingApproval)
	visit(store.RunRunning, store.NodeSucceeded) // an earlier visit of a running run
	visit(store.RunFailed, store.NodeRunning)    // left behind in a finished run

	code, body := do(t, ts, http.MethodGet, "/api/agents", "")
	var items []struct {
		ID       string   `json:"id"`
		Channels []string `json:"channels"`
		Activity *struct {
			Running         int `json:"running"`
			WaitingApproval int `json:"waiting_approval"`
		} `json:"activity"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &items) != nil {
		t.Fatalf("list: %d %s", code, body)
	}
	seen := 0
	for _, it := range items {
		if it.Activity == nil || it.Channels == nil {
			t.Fatalf("item without channels or activity: %+v", it)
		}
		switch it.ID {
		case busy:
			seen++
			if !slices.Equal(it.Channels, []string{"telegram"}) || it.Activity.Running != 2 || it.Activity.WaitingApproval != 1 {
				t.Errorf("busy agent = %+v %+v", it.Channels, *it.Activity)
			}
		case idle:
			seen++
			if len(it.Channels) != 0 || it.Activity.Running != 0 || it.Activity.WaitingApproval != 0 {
				t.Errorf("idle agent = %+v %+v", it.Channels, *it.Activity)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("listed %d of the two agents", seen)
	}
}
