package tools

import (
	"context"
	"encoding/json"
	"slices"
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
	// queued are the ids of the runs it stored.
	queued []string
}

// dropRunsAfter deletes the runs the delegator queued when the test ends.
func (d *fakeDelegator) dropRunsAfter(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, runID := range d.queued {
			_, _ = d.pool.Exec(context.Background(), `DELETE FROM runs WHERE id = $1`, runID)
		}
	})
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
	d.mu.Lock()
	d.queued = append(d.queued, run.ID)
	d.mu.Unlock()
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

// testProject creates a live project with a unique name; the token blob is opaque to
// these tests (the tools never read it).
func testProject(t *testing.T, pool *pgxpool.Pool, name string, mut func(p *store.Project)) *store.Project {
	t.Helper()
	p := &store.Project{Name: name, RepoURL: "https://gitlab.example/g/" + strings.ToLower(name) + ".git", Provider: "gitlab", DefaultBranch: "main"}
	if mut != nil {
		mut(p)
	}
	must(t, store.CreateProject(context.Background(), pool, p, []byte("sealed-elsewhere")))
	return p
}

// DESIGN 5.17 rule 8: list_projects shows every live project with its repository,
// provider, branch, network flag and last run; a deleted one is gone.
func TestListProjects(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	sfx := uniq()
	ran := testProject(t, pool, "Playground-"+sfx, func(p *store.Project) { p.SandboxNetwork = true; p.Provider = "github"; p.DefaultBranch = "develop" })
	idle := testProject(t, pool, "Idle-"+sfx, nil)
	gone := testProject(t, pool, "Gone-"+sfx, nil)
	must(t, store.DeleteProject(ctx, pool, gone.ID))
	wf := &store.Workflow{Name: "wf-" + sfx, Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	must(t, store.CreateWorkflow(ctx, pool, wf))
	settings := ran.Settings()
	old := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: store.TriggerManual, ProjectID: &ran.ID, Project: &settings}
	must(t, store.CreateRun(ctx, pool, old))
	tc := "python"
	newer := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: store.TriggerManual, ProjectID: &ran.ID, Project: &settings}
	must(t, store.CreateRun(ctx, pool, newer))
	must(t, store.SetRunToolchain(ctx, pool, newer.ID, tc))
	_, err := pool.Exec(ctx, `UPDATE runs SET status='succeeded', finished_at='2026-10-17T10:00:00Z' WHERE id=$1`, newer.ID)
	must(t, err)

	res := invoke(t, NewListProjects(pool), Call{AgentID: "x", Chat: true, Input: json.RawMessage(`{"chat_token":"x"}`)})
	if res.IsError {
		t.Fatalf("list_projects: %+v", res)
	}
	var out struct {
		Projects []struct {
			Name           string `json:"name"`
			Repository     string `json:"repository"`
			Provider       string `json:"provider"`
			DefaultBranch  string `json:"default_branch"`
			SandboxNetwork bool   `json:"sandbox_network"`
			LastRun        *struct {
				RunID      string  `json:"run_id"`
				Status     string  `json:"status"`
				Toolchain  *string `json:"toolchain"`
				FinishedAt *string `json:"finished_at"`
			} `json:"last_run"`
		} `json:"projects"`
	}
	must(t, json.Unmarshal(res.Output, &out))
	seen := map[string]bool{}
	for _, p := range out.Projects {
		seen[p.Name] = true
		switch p.Name {
		case ran.Name:
			if p.Repository != ran.RepoURL || p.Provider != "github" || p.DefaultBranch != "develop" || !p.SandboxNetwork || p.LastRun == nil ||
				p.LastRun.RunID != newer.ID || p.LastRun.Status != "succeeded" || p.LastRun.Toolchain == nil || *p.LastRun.Toolchain != "python" ||
				p.LastRun.FinishedAt == nil || *p.LastRun.FinishedAt != "2026-10-17T10:00:00Z" {
				t.Fatalf("ran = %+v %+v", p, p.LastRun)
			}
		case idle.Name:
			if p.LastRun != nil || p.SandboxNetwork || p.Provider != "gitlab" {
				t.Fatalf("idle = %+v", p)
			}
		}
	}
	if !seen[ran.Name] || !seen[idle.Name] || seen[gone.Name] {
		t.Fatalf("listed: %v", seen)
	}
	if strings.Contains(string(res.Output), "sealed-elsewhere") {
		t.Fatal("the token blob is listed")
	}
}

// DESIGN 5.17 rule 8, T5.17.6: delegate_task's project branches.
func TestDelegateTaskProject(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	sfx := uniq()
	chief := chiefAgent(t, pool, "Chief-"+sfx, "delegate_task")
	coder := chiefAgent(t, pool, "Coder-"+sfx)
	needs := &store.Workflow{Name: "Dev-" + sfx, Definition: json.RawMessage(`{"project":true,"nodes":[],"edges":[]}`)}
	must(t, store.CreateWorkflow(ctx, pool, needs))
	plain := &store.Workflow{Name: "Research-" + sfx, Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	must(t, store.CreateWorkflow(ctx, pool, plain))
	play := testProject(t, pool, "Playground-"+sfx, nil)
	d := &fakeDelegator{pool: pool}
	tool := NewDelegateTask(ChiefDeps{Pool: pool, Delegator: d})
	chat := func(input string) Result {
		return invoke(t, tool, Call{AgentID: chief.ID, Chat: true, Input: json.RawMessage(input)})
	}

	res := chat(`{"workflow_name":"Dev-` + sfx + `","input":{"task":"x"}}`)
	if !res.IsError || !strings.HasPrefix(res.Text, "workflow Dev-"+sfx+" needs a project; choose one of: ") || !strings.Contains(res.Text, play.Name) {
		t.Fatalf("needs a project: %+v", res)
	}
	res = chat(`{"workflow_name":"Dev-` + sfx + `","input":{"task":"x"},"project":"Nowhere-` + sfx + `"}`)
	if !res.IsError || !strings.HasPrefix(res.Text, "no project named Nowhere-"+sfx+"; choose one of: ") || !strings.Contains(res.Text, play.Name) {
		t.Fatalf("unknown project: %+v", res)
	}
	res = chat(`{"workflow_name":"Research-` + sfx + `","input":{"task":"x"},"project":"` + play.Name + `"}`)
	if !res.IsError || res.Text != "workflow Research-"+sfx+" does not use a project" {
		t.Fatalf("plain workflow with a project: %+v", res)
	}
	if len(d.got) != 0 {
		t.Fatalf("refused calls delegated: %+v", d.got)
	}
	res = chat(`{"workflow_name":"dev-` + sfx + `","input":{"task":"x"},"project":"  playground-` + sfx + ` "}`)
	if res.IsError || len(d.got) != 1 || d.got[0].WorkflowID != needs.ID || d.got[0].ProjectID != play.ID {
		t.Fatalf("project workflow: %+v %+v", res, d.got)
	}
	res = chat(`{"workflow_name":"Research-` + sfx + `","input":{"task":"x"}}`)
	if res.IsError || len(d.got) != 2 || d.got[1].ProjectID != "" {
		t.Fatalf("plain workflow: %+v %+v", res, d.got)
	}
	res = chat(`{"agent_name":"Coder-` + sfx + `","task":"add a test","project":"` + play.Name + `"}`)
	if res.IsError || len(d.got) != 3 || d.got[2].AgentID != coder.ID || d.got[2].ProjectID != play.ID {
		t.Fatalf("agent with a project: %+v %+v", res, d.got)
	}
	res = chat(`{"agent_name":"Coder-` + sfx + `","task":"add a test","project":""}`)
	if res.IsError || len(d.got) != 4 || d.got[3].ProjectID != "" {
		t.Fatalf("agent with an empty project: %+v %+v", res, d.got)
	}
	// The approval sentence names the project (DESIGN 5.17 rule 8).
	if msg := tool.Message(json.RawMessage(`{"workflow_name":"Dev","input":{},"project":"Playground"}`)); msg != "Run the workflow Dev on project Playground" {
		t.Fatalf("message = %q", msg)
	}
	if msg := tool.Message(json.RawMessage(`{"agent_name":"Coder","task":"t","project":"Playground"}`)); msg != "Delegate to Coder on project Playground: t" {
		t.Fatalf("message = %q", msg)
	}
}

// DESIGN 5.17 rule 8 and 5.20 rule 6: run_status carries the run's project, the merge
// request record it opened, and its retries (5.21 rule 7).
func TestRunStatusProjectAndMergeRequest(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	f := newFixture(t, pool, nil)
	play := testProject(t, pool, "Playground-"+uniq(), nil)
	settings := play.Settings()
	_, err := pool.Exec(ctx, `UPDATE runs SET project_id=$2, project_snapshot=$3 WHERE id=$1`, f.run.ID, play.ID, settings)
	must(t, err)
	tool := NewRunStatus(pool, "")
	res := invoke(t, tool, Call{Input: json.RawMessage(`{"run_id":"` + f.run.ID + `"}`)})
	var out map[string]any
	must(t, json.Unmarshal(res.Output, &out))
	if out["project"] != play.Name || out["merge_request_url"] != nil {
		t.Fatalf("before the request: %s", res.Output)
	}
	inserted, err := store.InsertMergeRequest(ctx, pool, &store.MergeRequest{ProjectID: play.ID, RunID: f.run.ID, Provider: "gitlab",
		Number: 3, URL: "https://gitlab.test/g/p/-/merge_requests/3", SourceBranch: "yuno/abc123", TargetBranch: "main", Title: "Add it"})
	must(t, err)
	if !inserted {
		t.Fatal("the record was not inserted")
	}
	res = invoke(t, tool, Call{Input: json.RawMessage(`{"run_id":"` + f.run.ID + `"}`)})
	must(t, json.Unmarshal(res.Output, &out))
	if out["merge_request_url"] != "https://gitlab.test/g/p/-/merge_requests/3" {
		t.Fatalf("after the request: %s", res.Output)
	}
	mr, _ := out["merge_request"].(map[string]any)
	if mr["number"] != float64(3) || mr["state"] != "open" || mr["ci_status"] != "none" || mr["unresolved_threads"] != nil {
		t.Fatalf("merge_request = %v", out["merge_request"])
	}
	if out["retries"] != float64(0) {
		t.Fatalf("retries = %v", out["retries"])
	}
}

// T5.22.8 (DESIGN 5.22 rule 8): delegate_task's merge_request resolves a number or a link
// against the named project's records; the id goes on the delegation; the refusals are the
// texts of the rule, in the provider's own words, and nothing is delegated for them.
func TestDelegateTaskMergeRequest(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	sfx := uniq()
	chief := chiefAgent(t, pool, "Chief-"+sfx, "delegate_task")
	coder := chiefAgent(t, pool, "Coder-"+sfx)
	dev := &store.Workflow{Name: "Dev-" + sfx, Definition: json.RawMessage(`{"project":true,"nodes":[],"edges":[]}`)}
	must(t, store.CreateWorkflow(ctx, pool, dev))
	plain := &store.Workflow{Name: "Research-" + sfx, Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	must(t, store.CreateWorkflow(ctx, pool, plain))
	play := testProject(t, pool, "Playground-"+sfx, nil)
	hub := testProject(t, pool, "Hub-"+sfx, func(p *store.Project) { p.Provider = "github" })
	other := testProject(t, pool, "Other-"+sfx, nil)
	t.Cleanup(func() {
		for _, p := range []*store.Project{play, hub, other} {
			_, _ = pool.Exec(ctx, `DELETE FROM merge_requests WHERE project_id = $1`, p.ID)
		}
	})
	rec := func(p *store.Project, n int64, state string) *store.MergeRequest {
		return projectRecord(t, p, projectRun(t, p, nil), n, state)
	}
	open12, merged13 := rec(play, 12, store.MergeRequestOpen), rec(play, 13, store.MergeRequestMerged)
	open4, closed5 := rec(hub, 4, store.MergeRequestOpen), rec(hub, 5, store.MergeRequestClosed)
	rec(other, 20, store.MergeRequestOpen)

	d := &fakeDelegator{pool: pool}
	d.dropRunsAfter(t)
	tool := NewDelegateTask(ChiefDeps{Pool: pool, Delegator: d})
	chat := func(input string) Result {
		return invoke(t, tool, Call{AgentID: chief.ID, Chat: true, Input: json.RawMessage(input)})
	}
	wf := func(project, mr string) string {
		return `{"workflow_name":"Dev-` + sfx + `","input":{"task":"fix it"},"project":"` + project + `","merge_request":"` + mr + `"}`
	}

	// The number in every form, and the link, by workflow; the id and the project reach the engine.
	for i, mr := range []string{"12", "!12", "#12", " 12 ", "https://gitlab.example/g/playground/-/merge_requests/12", "https://gitlab.example/g/playground/-/merge_requests/12/diffs"} {
		res := chat(wf(play.Name, mr))
		if res.IsError || len(d.got) != i+1 {
			t.Fatalf("merge_request %q: %+v", mr, res)
		}
		if g := d.got[i]; g.MergeRequestID != open12.ID || g.ProjectID != play.ID || g.WorkflowID != dev.ID {
			t.Fatalf("merge_request %q: delegation = %+v", mr, g)
		}
	}
	// A pull request on GitHub, by link, and an agent instead of a workflow.
	n := len(d.got)
	res := chat(`{"agent_name":"Coder-` + sfx + `","task":"address the review","project":"` + hub.Name + `","merge_request":"https://github.com/owner/demo/pull/4/files"}`)
	if res.IsError || len(d.got) != n+1 || d.got[n].MergeRequestID != open4.ID || d.got[n].ProjectID != hub.ID || d.got[n].AgentID != coder.ID {
		t.Fatalf("agent with a pull request: %+v %+v", res, d.got)
	}
	// Left out or blank: no request, as before.
	n = len(d.got)
	for _, input := range []string{`{"workflow_name":"Dev-` + sfx + `","input":{"task":"x"},"project":"` + play.Name + `"}`, wf(play.Name, "  ")} {
		if res := chat(input); res.IsError || len(d.got) != n+1 || d.got[n].MergeRequestID != "" {
			t.Fatalf("%s: %+v %+v", input, res, d.got)
		}
		n++
	}

	n = len(d.got)
	for _, tc := range []struct{ name, input, want string }{
		{"unknown on GitLab", wf(play.Name, "99"), "no merge request !99 on project " + play.Name},
		{"unknown on GitHub", wf(hub.Name, "#99"), "no pull request #99 on project " + hub.Name},
		{"the other project's request, by link", wf(play.Name, "https://gitlab.example/g/other/-/merge_requests/20"), "no merge request !20 on project " + play.Name},
		{"merged", wf(play.Name, "13"), merged13.Label() + " is merged"},
		{"closed on GitHub", wf(hub.Name, "5"), closed5.Label() + " is closed"},
		{"not a number", wf(play.Name, "the second one"), "merge_request must be a merge request number or URL"},
		{"an issue link", wf(play.Name, "https://gitlab.example/g/playground/-/issues/12"), "merge_request must be a merge request number or URL"},
		{"an agent without a project", `{"agent_name":"Coder-` + sfx + `","task":"t","merge_request":"12"}`, "merge_request needs a project"},
		{"an agent with an empty project", `{"agent_name":"Coder-` + sfx + `","task":"t","project":"","merge_request":"12"}`, "merge_request needs a project"},
		{"a workflow that takes no project", `{"workflow_name":"Research-` + sfx + `","input":{},"merge_request":"12"}`, "merge_request needs a project"},
		{"a number, not a string", `{"workflow_name":"Dev-` + sfx + `","input":{},"project":"` + play.Name + `","merge_request":12}`, "merge_request"},
	} {
		res := chat(tc.input)
		if !res.IsError || res.Blocked || !strings.Contains(res.Text, tc.want) {
			t.Errorf("%s: %+v, want %q", tc.name, res, tc.want)
		}
		if tc.name != "a number, not a string" && res.Text != tc.want {
			t.Errorf("%s: text = %q, want exactly %q", tc.name, res.Text, tc.want)
		}
	}
	if len(d.got) != n {
		t.Fatalf("refused calls delegated: %+v", d.got[n:])
	}

	// The engine's refusals of rule 1 reach the model as the delegator words them.
	d.err = &ModelError{Message: "the run was not started: merge_request_id: " + open12.Label() + " is being updated by run run-1"}
	if res := chat(wf(play.Name, "12")); !res.IsError || res.Text != "the run was not started: merge_request_id: merge request !12 is being updated by run run-1" {
		t.Fatalf("engine refusal = %+v", res)
	}
}

// The tool's schema names merge_request as a string (DESIGN 5.22 rule 8).
func TestDelegateTaskSchemaHasMergeRequest(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
	}
	must(t, json.Unmarshal(delegateTaskSchema, &schema))
	mr, ok := schema.Properties["merge_request"]
	if !ok || mr.Type != "string" || !strings.Contains(mr.Description, "#12") || !strings.Contains(mr.Description, "link") || !strings.Contains(mr.Description, "Needs project") {
		t.Fatalf("merge_request = %+v", mr)
	}
	if !slices.IsSorted(ChatToolNames) || !slices.Contains(ChatToolNames, ReadMergeRequestName) {
		t.Fatalf("ChatToolNames = %v", ChatToolNames)
	}
}

// T5.22.8 (approval sentence, DESIGN 5.22 rule 8): the sentence of 5.15 rule 7 ends with
// ", updating <merge request !n|pull request #n>". It is built from the input alone: the
// sigil the person wrote, or the link's, and "!" for a bare number.
func TestDescribeDelegationUpdating(t *testing.T) {
	tool := NewDelegateTask(ChiefDeps{})
	for input, want := range map[string]string{
		`{"workflow_name":"Dev","input":{},"project":"Playground","merge_request":"12"}`:                                               "Run the workflow Dev on project Playground, updating merge request !12",
		`{"workflow_name":"Dev","input":{},"project":"Playground","merge_request":"!12"}`:                                              "Run the workflow Dev on project Playground, updating merge request !12",
		`{"workflow_name":"Dev","input":{},"project":"Playground","merge_request":"#12"}`:                                              "Run the workflow Dev on project Playground, updating pull request #12",
		`{"workflow_name":"Dev","input":{},"project":"Playground","merge_request":"https://gitlab.test/g/p/-/merge_requests/7/diffs"}`: "Run the workflow Dev on project Playground, updating merge request !7",
		`{"workflow_name":"Dev","input":{},"project":"Playground","merge_request":"https://github.com/o/r/pull/7"}`:                    "Run the workflow Dev on project Playground, updating pull request #7",
		`{"agent_name":"Coder","task":"fix it","project":"Playground","merge_request":"!3"}`:                                           "Delegate to Coder on project Playground: fix it, updating merge request !3",
		`{"workflow_name":"Dev","input":{},"project":"Playground"}`:                                                                    "Run the workflow Dev on project Playground",
		`{"workflow_name":"Dev","input":{},"project":"Playground","merge_request":" "}`:                                                "Run the workflow Dev on project Playground",
		`{"workflow_name":"Dev","input":{},"project":"Playground","merge_request":"soon"}`:                                             "Run the workflow Dev on project Playground",
	} {
		if got := tool.Message(json.RawMessage(input)); got != want {
			t.Errorf("%s\n got %q\nwant %q", input, got, want)
		}
	}
}

// T5.22.8 and T5.23.9: run_status says whether the run opened the request or updates one
// (role), keeps merge_request_url, and gives an issue run's issue with its link.
func TestRunStatusRoleAndIssue(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	sfx := uniq()
	play := testProject(t, pool, "Playground-"+sfx, nil)
	hub := testProject(t, pool, "Hub-"+sfx, func(p *store.Project) {
		p.Provider = "github"
		p.RepoURL = "https://github.com/owner/" + strings.ToLower(sfx) + ".git"
	})
	t.Cleanup(func() {
		for _, p := range []*store.Project{play, hub} {
			_, _ = pool.Exec(ctx, `DELETE FROM merge_requests WHERE project_id = $1`, p.ID)
		}
	})
	status := func(run *store.Run) map[string]any {
		t.Helper()
		res := invoke(t, NewRunStatus(pool, ""), Call{Input: json.RawMessage(`{"run_id":"` + run.ID + `"}`)})
		if res.IsError {
			t.Fatalf("run_status: %+v", res)
		}
		var out map[string]any
		must(t, json.Unmarshal(res.Output, &out))
		return out
	}

	// The run that opened a request: role opened.
	opener := projectRun(t, play, nil)
	rec := projectRecord(t, play, opener, 3, store.MergeRequestOpen)
	mr, _ := status(opener)["merge_request"].(map[string]any)
	if mr["role"] != "opened" || mr["number"] != float64(3) || mr["url"] != rec.URL || mr["state"] != "open" || status(opener)["merge_request_url"] != rec.URL {
		t.Fatalf("opener = %v", status(opener))
	}
	// A run that names it: role updates, the request's url, no record of its own.
	updater := projectRun(t, play, func(r *store.Run) { r.MergeRequestID = &rec.ID })
	out := status(updater)
	mr, _ = out["merge_request"].(map[string]any)
	if mr["role"] != "updates" || mr["number"] != float64(3) || mr["url"] != rec.URL || out["merge_request_url"] != rec.URL || mr["ci_status"] != "none" {
		t.Fatalf("updater = %v", out)
	}
	// A run with neither.
	if out := status(projectRun(t, play, nil)); out["merge_request"] != nil || out["merge_request_url"] != nil {
		t.Fatalf("no request = %v", out)
	}

	// An issue run: the number from the ref and the link from the project's snapshot.
	issueRun := func(p *store.Project, ref string) *store.Run {
		return projectRun(t, p, func(r *store.Run) { r.Trigger, r.TriggerRef = store.TriggerIssue, &ref })
	}
	out = status(issueRun(play, play.ID+"#7"))
	issue, _ := out["issue"].(map[string]any)
	if out["trigger"] != "issue" || issue["number"] != float64(7) || issue["url"] != strings.TrimSuffix(play.RepoURL, ".git")+"/-/issues/7" {
		t.Fatalf("gitlab issue run = %v", out)
	}
	out = status(issueRun(hub, hub.ID+"#12"))
	issue, _ = out["issue"].(map[string]any)
	if issue["number"] != float64(12) || issue["url"] != strings.TrimSuffix(hub.RepoURL, ".git")+"/issues/12" {
		t.Fatalf("github issue run = %v", out)
	}
	for _, ref := range []string{"nohash-" + sfx, play.ID + "#", play.ID + "#x", play.ID + "#0"} {
		if out := status(issueRun(play, ref)); out["issue"] != nil {
			t.Fatalf("ref %q gave an issue: %v", ref, out["issue"])
		}
	}
	if out := status(opener); out["issue"] != nil {
		t.Fatalf("a manual run has an issue: %v", out["issue"])
	}
}

// T5.23.9: list_projects carries intake: {enabled, label} for every project.
func TestListProjectsIntake(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	sfx := uniq()
	plain := testProject(t, pool, "Plain-"+sfx, nil)
	watched := testProject(t, pool, "Watched-"+sfx, nil)
	_, err := pool.Exec(ctx, `UPDATE projects SET intake_enabled = true, intake_label = 'triage' WHERE id = $1`, watched.ID)
	must(t, err)
	res := invoke(t, NewListProjects(pool), Call{AgentID: "x", Chat: true, Input: json.RawMessage(`{"chat_token":"x"}`)})
	var out struct {
		Projects []struct {
			Name   string          `json:"name"`
			Intake json.RawMessage `json:"intake"`
		} `json:"projects"`
	}
	must(t, json.Unmarshal(res.Output, &out))
	got := map[string]string{}
	for _, p := range out.Projects {
		got[p.Name] = string(p.Intake)
	}
	if got[plain.Name] != `{"enabled":false,"label":"yuno"}` || got[watched.Name] != `{"enabled":true,"label":"triage"}` {
		t.Fatalf("intake = %v", got)
	}
}
