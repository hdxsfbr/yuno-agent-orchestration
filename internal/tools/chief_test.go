package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/store"
)

// fakeDelegator records delegations and answers with a queued run row.
type fakeDelegator struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	got  []Delegation
	err  error
}

func (d *fakeDelegator) Delegate(ctx context.Context, req Delegation) (*store.Run, error) {
	d.mu.Lock()
	d.got = append(d.got, req)
	err := d.err
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	wf := &store.Workflow{Name: "delegated", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	if err := store.CreateWorkflow(ctx, d.pool, wf); err != nil {
		return nil, err
	}
	ref := req.TriggerRef
	run := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: store.TriggerChannel}
	if ref != "" {
		run.TriggerRef = &ref
	}
	if err := store.CreateRun(ctx, d.pool, run); err != nil {
		return nil, err
	}
	return run, nil
}

// fakeNow is a settable clock for the delegation rate limit.
type fakeNow struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeNow) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeNow) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// chiefAgent creates an agent with a name unique to the test.
func chiefAgent(t *testing.T, pool *pgxpool.Pool, name string, allow ...string) *store.Agent {
	t.Helper()
	a := &store.Agent{Name: name, Role: "role of " + name, Model: "claude-sonnet-5-5", ToolAllowlist: allow}
	must(t, store.CreateAgent(context.Background(), pool, a))
	return a
}

func uniq() string { return strings.ReplaceAll(store.NewID(), "-", "")[20:] }

// invoke validates the input against the tool's schema, as the MCP gate does, then runs
// the executor once.
func invoke(t *testing.T, tool *Tool, call Call) Result {
	t.Helper()
	if tool.schema == nil {
		NewRegistry().Register(tool) // compiles the schema
	}
	if err := tool.ValidateInput(call.Input); err != nil {
		return Result{IsError: true, Text: err.Error()}
	}
	res, err := tool.Invoke(context.Background(), call)
	if err != nil {
		t.Fatalf("%s: %v", tool.Name, err)
	}
	return res
}

// DESIGN 5.15 rule 4: list_agents shows every agent but the caller, with its role,
// model, tools and whether it is bound to a channel.
func TestListAgentsExcludesCaller(t *testing.T) {
	pool := needDB(t)
	sfx := uniq()
	caller := chiefAgent(t, pool, "Caller-"+sfx, "list_agents")
	other := chiefAgent(t, pool, "Other-"+sfx, "read", "exec", "finish")
	res := invoke(t, NewListAgents(pool), Call{AgentID: caller.ID, Chat: true, Input: json.RawMessage(`{"chat_token":"x"}`)})
	if res.IsError {
		t.Fatalf("list_agents: %+v", res)
	}
	var out struct {
		Agents []struct {
			Name, Role, Model string
			Tools             []string
			BoundToChannel    bool `json:"bound_to_channel"`
		} `json:"agents"`
	}
	must(t, json.Unmarshal(res.Output, &out))
	found := false
	for _, a := range out.Agents {
		if a.Name == caller.Name {
			t.Fatalf("the caller is listed: %+v", a)
		}
		if a.Name == other.Name {
			found = true
			if a.Role != other.Role || a.Model != "claude-sonnet-5-5" || strings.Join(a.Tools, ",") != "read,exec,finish" || a.BoundToChannel {
				t.Fatalf("other = %+v", a)
			}
		}
	}
	if !found {
		t.Fatalf("%s not listed", other.Name)
	}
}

// DESIGN 5.15 rule 4: list_workflows shows operator workflows (not templates or quick
// runs) with their description, the agents their nodes use and the input schema.
func TestListWorkflowsShapes(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	sfx := uniq()
	coder := chiefAgent(t, pool, "Coder-"+sfx)
	reviewer := chiefAgent(t, pool, "Reviewer-"+sfx)
	def := `{"input_schema":{"type":"object","properties":{"task":{"type":"string"}},"required":["task"]},
		"nodes":[{"id":"start","kind":"start"},{"id":"c","kind":"agent","agent_id":"` + coder.ID + `"},
		{"id":"r","kind":"agent","agent_id":"` + reviewer.ID + `"},{"id":"c2","kind":"agent","agent_id":"` + coder.ID + `"},
		{"id":"end","kind":"end"}],"edges":[]}`
	wf := &store.Workflow{Name: "Flow-" + sfx, Description: "builds and reviews", Definition: json.RawMessage(def)}
	must(t, store.CreateWorkflow(ctx, pool, wf))
	key := "tpl_" + sfx
	must(t, store.CreateWorkflow(ctx, pool, &store.Workflow{Name: "Template-" + sfx, Definition: json.RawMessage(def), TemplateKey: &key}))

	res := invoke(t, NewListWorkflows(pool), Call{AgentID: coder.ID, Input: json.RawMessage(`{"run_token":"r"}`)})
	var out struct {
		Workflows []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Agents      []string        `json:"agents"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"workflows"`
	}
	must(t, json.Unmarshal(res.Output, &out))
	found := false
	for _, w := range out.Workflows {
		if w.Name == "Template-"+sfx {
			t.Fatal("a template is listed")
		}
		if w.Name == wf.Name {
			found = true
			if w.Description != "builds and reviews" || strings.Join(w.Agents, ",") != coder.Name+","+reviewer.Name ||
				!strings.Contains(string(w.InputSchema), `"required":["task"]`) {
				t.Fatalf("workflow = %+v %s", w, w.InputSchema)
			}
		}
	}
	if !found {
		t.Fatalf("%s not listed: %s", wf.Name, res.Output)
	}
}

// DESIGN 5.15 rules 5 and 6: delegate_task's validation branches, names, self
// delegation, the trigger_ref and the rate limit.
func TestDelegateTask(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	sfx := uniq()
	chief := chiefAgent(t, pool, "Chief-"+sfx, "delegate_task")
	coder := chiefAgent(t, pool, "Coder-"+sfx)
	wf := &store.Workflow{Name: "Pipeline-" + sfx, Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	must(t, store.CreateWorkflow(ctx, pool, wf))
	clock := &fakeNow{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	d := &fakeDelegator{pool: pool}
	tool := NewDelegateTask(ChiefDeps{Pool: pool, Delegator: d, PublicBaseURL: "https://yuno.example/", Limiter: NewDelegationLimiter(clock.now)})
	conv := "conv-" + sfx
	chat := func(input string) Result {
		return invoke(t, tool, Call{AgentID: chief.ID, Chat: true, ConversationID: conv, Input: json.RawMessage(input)})
	}

	for _, tc := range []struct{ name, input, want string }{
		{"neither form", `{"chat_token":"x"}`, errDelegateShape},
		{"both forms", `{"agent_name":"a","task":"t","workflow_name":"w","input":{}}`, errDelegateShape},
		{"agent without a task", `{"agent_name":"Coder-` + sfx + `"}`, errDelegateShape},
		{"agent with an empty task", `{"agent_name":"Coder-` + sfx + `","task":"  "}`, errDelegateShape},
		{"agent with an input", `{"agent_name":"Coder-` + sfx + `","task":"t","input":{}}`, errDelegateShape},
		{"workflow without an input", `{"workflow_name":"Pipeline-` + sfx + `"}`, errDelegateShape},
		{"workflow with a task", `{"workflow_name":"Pipeline-` + sfx + `","input":{},"task":"t"}`, errDelegateShape},
		{"input not an object", `{"workflow_name":"Pipeline-` + sfx + `","input":"go"}`, "at '/input'"},
		{"unknown property", `{"agent_name":"a","task":"t","priority":1}`, "priority"},
		{"unknown agent", `{"agent_name":"Nobody-` + sfx + `","task":"t"}`, "no agent named Nobody-" + sfx},
		{"unknown workflow", `{"workflow_name":"Nothing-` + sfx + `","input":{}}`, "no workflow named Nothing-" + sfx},
		{"self", `{"agent_name":"chief-` + sfx + `","task":"do it yourself"}`, "cannot delegate to yourself"},
	} {
		res := chat(tc.input)
		if !res.IsError || !strings.Contains(res.Text, tc.want) || res.Blocked {
			t.Errorf("%s: %+v, want error %q", tc.name, res, tc.want)
		}
	}
	if len(d.got) != 0 {
		t.Fatalf("refused calls delegated: %+v", d.got)
	}

	// An agent by name, case-insensitively; the conversation becomes the trigger ref.
	res := chat(`{"agent_name":"CODER-` + sfx + `","task":"Write hello.py"}`)
	var out struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
		URL    string `json:"url"`
	}
	must(t, json.Unmarshal(res.Output, &out))
	if res.IsError || out.Status != store.RunQueued || out.URL != "https://yuno.example/runs/"+out.RunID {
		t.Fatalf("delegate = %+v %s", res, res.Output)
	}
	if g := d.got[0]; g.AgentID != coder.ID || g.Task != "Write hello.py" || g.WorkflowID != "" ||
		g.TriggerRef != "conversation:"+conv || g.CallerAgentID != chief.ID {
		t.Fatalf("delegation = %+v", g)
	}
	// A workflow by name with its input; a call with no conversation has no ref.
	res = invoke(t, tool, Call{AgentID: chief.ID, Chat: true, Input: json.RawMessage(`{"workflow_name":"pipeline-` + sfx + `","input":{"task":"x"}}`)})
	if res.IsError {
		t.Fatalf("workflow: %+v", res)
	}
	if g := d.got[1]; g.WorkflowID != wf.ID || string(g.Input) != `{"task":"x"}` || g.TriggerRef != "" {
		t.Fatalf("delegation = %+v", g)
	}
	// A run-token call (a delegated run delegating) reports to no conversation.
	res = invoke(t, tool, Call{AgentID: chief.ID, RunID: "r1", ConversationID: conv, Input: json.RawMessage(`{"agent_name":"Coder-` + sfx + `","task":"t"}`)})
	if res.IsError || d.got[2].TriggerRef != "" {
		t.Fatalf("run-token delegation = %+v %+v", res, d.got[2])
	}
	// A refusal by the engine reaches the model and gives the delegation back.
	d.err = &ModelError{Message: "the run was not started: input.task: missing"}
	if res := chat(`{"agent_name":"Coder-` + sfx + `","task":"t"}`); !res.IsError || !strings.Contains(res.Text, "input.task") {
		t.Fatalf("engine refusal = %+v", res)
	}
	d.err = nil

	// Ten delegations in ten minutes; the eleventh is blocked, not failed; the window
	// slides. Three slots are taken: the agent and workflow delegations and the run-token
	// one; the engine's refusal gave its slot back.
	const reserved = 3
	for i := reserved; i < DelegationMax; i++ {
		clock.advance(time.Second)
		if res := chat(`{"agent_name":"Coder-` + sfx + `","task":"t"}`); res.IsError {
			t.Fatalf("delegation %d: %+v", i+1, res)
		}
	}
	res = chat(`{"agent_name":"Coder-` + sfx + `","task":"t"}`)
	if !res.IsError || !res.Blocked || res.Text != "delegation limit reached; try later" {
		t.Fatalf("eleventh = %+v", res)
	}
	clock.advance(DelegationWindow - 5*time.Second)
	if res := chat(`{"agent_name":"Coder-` + sfx + `","task":"t"}`); res.IsError {
		t.Fatalf("after the window slid: %+v", res)
	}
	// Another agent has its own window.
	other := chiefAgent(t, pool, "Second chief-"+sfx)
	if res := invoke(t, tool, Call{AgentID: other.ID, Chat: true, Input: json.RawMessage(`{"agent_name":"Coder-` + sfx + `","task":"t"}`)}); res.IsError {
		t.Fatalf("another agent: %+v", res)
	}
}

func TestDelegationLimiterRelease(t *testing.T) {
	clock := &fakeNow{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	l := NewDelegationLimiter(clock.now)
	for i := 0; i < DelegationMax; i++ {
		release, ok := l.Reserve("a")
		if !ok {
			t.Fatalf("reserve %d refused", i+1)
		}
		if i == DelegationMax-1 {
			release() // the run could not be queued: the slot comes back
			release() // twice is a no-op
		}
	}
	if _, ok := l.Reserve("a"); !ok {
		t.Fatal("released slot not reusable")
	}
	if _, ok := l.Reserve("a"); ok {
		t.Fatal("eleventh reserved")
	}
}

// DESIGN 5.15 rule 8: run_status for an unknown id, a run waiting for an approval and
// a terminal run with its state.
func TestRunStatus(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	tool := NewRunStatus(pool, "")
	if res := invoke(t, tool, Call{Input: json.RawMessage(`{"chat_token":"x","run_id":"no-such-run"}`)}); !res.IsError || res.Text != "no run no-such-run" {
		t.Fatalf("unknown = %+v", res)
	}

	f := newFixture(t, pool, nil)
	_, err := store.InsertApprovalAndPause(ctx, pool, &store.Approval{RunID: f.run.ID, RunNodeID: f.visit.ID, Kind: store.ApprovalKindTool,
		NodeID: "deployer", Message: "Deploy release 1.0.0", Status: store.ApprovalPending, ExpiresAt: time.Now().Add(time.Hour)}, time.Now())
	must(t, err)
	res := invoke(t, tool, Call{Input: json.RawMessage(`{"run_id":"` + f.run.ID + `"}`)})
	var waiting map[string]any
	must(t, json.Unmarshal(res.Output, &waiting))
	pa, _ := waiting["pending_approval"].(map[string]any)
	if waiting["status"] != store.RunWaitingApproval || pa["kind"] != "tool" || pa["message"] != "Deploy release 1.0.0" ||
		waiting["url"] != "/runs/"+f.run.ID || waiting["state"] != nil {
		t.Fatalf("waiting = %s", res.Output)
	}

	// Terminal: the state is returned, capped at 4 KB.
	big := strings.Repeat("x", 5000)
	state, _ := json.Marshal(map[string]any{"coder": map[string]any{"summary": "ok", "notes": big}})
	_, err = pool.Exec(ctx, `UPDATE runs SET status = 'succeeded', state = $2, started_at = $3, finished_at = $4, step_count = 7,
		tool_call_count = 3, cost_usd_micros = 12500 WHERE id = $1`, f.run.ID, state,
		time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC), time.Date(2026, 10, 16, 12, 1, 30, 0, time.UTC))
	must(t, err)
	res = invoke(t, tool, Call{Input: json.RawMessage(`{"run_id":"` + f.run.ID + `"}`)})
	var done map[string]any
	must(t, json.Unmarshal(res.Output, &done))
	text, _ := done["state"].(string)
	if done["status"] != "succeeded" || done["duration_seconds"] != float64(90) || done["steps"] != float64(7) ||
		done["tool_calls"] != float64(3) || done["cost_usd"] != 0.0125 || done["pending_approval"] != nil ||
		len(text) > 4200 || !strings.Contains(text, "[truncated") {
		t.Fatalf("terminal = %.400s", res.Output)
	}
	_, err = pool.Exec(ctx, `UPDATE runs SET state = '{"coder":{"summary":"ok"}}', status = 'failed', failure_reason = 'tests failed' WHERE id = $1`, f.run.ID)
	must(t, err)
	res = invoke(t, tool, Call{Input: json.RawMessage(`{"run_id":"` + f.run.ID + `"}`)})
	must(t, json.Unmarshal(res.Output, &done))
	if st, _ := done["state"].(map[string]any); st == nil || done["failure_reason"] != "tests failed" {
		t.Fatalf("failed = %s", res.Output)
	}
}

// The registry marks exactly ChatToolNames as chat-capable (DESIGN 5.15 rule 2).
func TestChatToolsAreMarked(t *testing.T) {
	r := NewPlatformRegistry(PlatformDeps{GitLab: &GitLabConfig{BaseURL: "https://gitlab.example", ProjectID: "1", Token: "t"}})
	var chat []string
	for _, tool := range r.List() {
		if tool.Chat {
			chat = append(chat, tool.Name)
		}
		if tool.Chat && tool.Risk == RiskSideEffecting {
			t.Errorf("%s is chat-capable and side-effecting", tool.Name)
		}
	}
	if strings.Join(chat, ",") != strings.Join(ChatToolNames, ",") {
		t.Fatalf("chat tools = %v, want %v", chat, ChatToolNames)
	}
	if !IsChatTool("yuno__delegate_task") || IsChatTool("finish") {
		t.Fatal("IsChatTool")
	}
}
