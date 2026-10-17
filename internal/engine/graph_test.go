package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// ---- workflow harness ------------------------------------------------------------------

func (env *testEnv) createWorkflow(def workflow.Definition) *store.Workflow {
	env.t.Helper()
	raw, err := json.Marshal(def)
	if err != nil {
		env.t.Fatal(err)
	}
	wf := &store.Workflow{Name: "wf", Definition: raw}
	if err := store.CreateWorkflow(context.Background(), env.pool, wf); err != nil {
		env.t.Fatal(err)
	}
	return wf
}

// resume claims the queued run and executes it until the worker lets it go (terminal
// or paused at an approval node).
func (env *testEnv) resume(e *Engine, runID string) *store.Run {
	env.t.Helper()
	claimed, err := store.ClaimQueuedRun(context.Background(), env.pool)
	if err != nil || claimed.ID != runID {
		env.t.Fatalf("claim %s: %+v %v", runID, claimed, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.runRun(context.Background(), claimed)
	}()
	return env.wait(done, runID)
}

// triggerAndRun triggers a workflow run and executes it until the worker lets it go.
func (env *testEnv) triggerAndRun(req TriggerRequest) *store.Run {
	env.t.Helper()
	run, err := env.e.Trigger(context.Background(), req)
	if err != nil {
		env.t.Fatalf("trigger: %v", err)
	}
	return env.resume(env.e, run.ID)
}

// visitOf parses "agent:<id>:yuno-<run id>-<node>-<visit>" (node ids have no dashes).
func visitOf(key string) (string, int) {
	i := strings.LastIndex(key, "-")
	n, _ := strconv.Atoi(key[i+1:])
	rest := key[:i]
	return rest[strings.LastIndex(rest, "-")+1:], n
}

// scripted answers each node visit's turn with the next result for that node and
// records the task messages it was sent.
type scripted struct {
	mu       sync.Mutex
	messages map[string][]string // node → task message per visit
}

func (env *testEnv) script(results map[string][]string) *scripted {
	sc := &scripted{messages: map[string][]string{}}
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		node, visit := visitOf(p.SessionKey)
		sc.mu.Lock()
		sc.messages[node] = append(sc.messages[node], p.Message)
		sc.mu.Unlock()
		out := results[node]
		if visit > len(out) {
			env.t.Errorf("no scripted result for %s visit %d", node, visit)
			return startedTurn(p)
		}
		go finishThenFinal(env.t, env, p, out[visit-1])
		return startedTurn(p)
	}
	return sc
}

func (sc *scripted) message(node string, visit int) string {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if visit > len(sc.messages[node]) {
		return ""
	}
	return sc.messages[node][visit-1]
}

func cond(t *testing.T, s string) *workflow.Condition {
	t.Helper()
	var c workflow.Condition
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

var anyObject = json.RawMessage(`{"type":"object"}`)

// line builds start → nodes… → end with unconditional edges.
func line(nodes ...workflow.Node) workflow.Definition {
	d := workflow.Definition{Nodes: []workflow.Node{{ID: "start", Kind: workflow.KindStart}}}
	prev := "start"
	for _, n := range nodes {
		if n.Kind == "" {
			n.Kind = workflow.KindAgent
		}
		if n.Kind == workflow.KindAgent && len(n.OutputSchema) == 0 {
			n.OutputSchema = anyObject
		}
		d.Nodes = append(d.Nodes, n)
		d.Edges = append(d.Edges, workflow.Edge{From: prev, To: n.ID})
		prev = n.ID
	}
	d.Nodes = append(d.Nodes, workflow.Node{ID: "end", Kind: workflow.KindEnd})
	d.Edges = append(d.Edges, workflow.Edge{From: prev, To: "end"})
	return d
}

// devPipeline stores templates/dev_pipeline.json with its agent names resolved to new
// agents, as instantiating the template does.
func devPipeline(t *testing.T, env *testEnv) *store.Workflow {
	t.Helper()
	raw, err := os.ReadFile("../../templates/dev_pipeline.json")
	if err != nil {
		t.Fatal(err)
	}
	var tpl struct {
		Definition workflow.Definition `json:"definition"`
	}
	if err := json.Unmarshal(raw, &tpl); err != nil {
		t.Fatal(err)
	}
	agents := map[string]string{}
	for i := range tpl.Definition.Nodes {
		n := &tpl.Definition.Nodes[i]
		if n.AgentName == "" {
			continue
		}
		if agents[n.AgentName] == "" {
			agents[n.AgentName] = env.agent(func(a *store.Agent) { a.Name = n.AgentName }).ID
		}
		n.AgentID, n.AgentName = agents[n.AgentName], ""
	}
	return env.createWorkflow(tpl.Definition)
}

func runNodes(t *testing.T, env *testEnv, runID string) []store.RunNode {
	t.Helper()
	nodes, err := store.ListRunNodes(context.Background(), env.pool, runID)
	if err != nil {
		t.Fatal(err)
	}
	return nodes
}

func visitsOf(nodes []store.RunNode) []string {
	var out []string
	for _, n := range nodes {
		out = append(out, fmt.Sprintf("%s#%d:%s", n.NodeID, n.Visit, n.Status))
	}
	return out
}

func stateOf(t *testing.T, r *store.Run) map[string]any {
	t.Helper()
	var st map[string]any
	if err := json.Unmarshal(r.State, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func pendingApproval(t *testing.T, env *testEnv, runID string) *store.Approval {
	t.Helper()
	pending, err := store.ListPendingApprovals(context.Background(), env.pool, runID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending approvals = %+v, %v", pending, err)
	}
	return &pending[0]
}

// ---- T5.6.8, T5.6.9, T5.2.8: the shipped dev pipeline ----------------------------------

// T5.6.8: the dev pipeline template loops twice (reaching max_loop_visits 3 once),
// waits for the deploy approval, then deploys. T5.6.9: a revisit replaces the state.
func TestDevPipelineEndToEnd(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	wf := devPipeline(t, env)
	sc := env.script(map[string][]string{
		"coder": {
			`{"summary":"first cut","files":["fizz.py"],"draft":true}`,
			`{"summary":"with tests","files":["fizz.py","test_fizz.py"]}`,
			`{"summary":"zero handled","files":["fizz.py","test_fizz.py"],"tests_passed":true}`,
		},
		"reviewer": {
			`{"verdict":"changes_requested","feedback":"Add tests."}`,
			`{"verdict":"changes_requested","feedback":"Cover zero."}`,
			`{"verdict":"approved","feedback":"Looks good."}`,
		},
		"deployer": {`{"status":"deployed","release_id":"rel-1","version":"1.0.0"}`},
	})
	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(`{"task":"Write fizzbuzz."}`),
		Model: "scripted/scripted-1", ScriptID: "dev-1"})

	// Paused at the gate: the worker let the run go.
	if r.Status != store.RunWaitingApproval || r.PausedAt == nil || deref(r.CurrentNode) != "gate" {
		t.Fatalf("at the gate: %s paused=%v current=%s", r.Status, r.PausedAt, deref(r.CurrentNode))
	}
	want := []string{"coder#1:succeeded", "reviewer#1:succeeded", "coder#2:succeeded", "reviewer#2:succeeded",
		"coder#3:succeeded", "reviewer#3:succeeded", "gate#1:waiting_approval"}
	if got := visitsOf(runNodes(t, env, r.ID)); !slices.Equal(got, want) {
		t.Fatalf("visits = %v", got)
	}
	gate := pendingApproval(t, env, r.ID)
	if gate.Kind != store.ApprovalKindNode || gate.NodeID != "gate" ||
		gate.Message != "Ship this change (release 1.0.0 or a merge request)? Coder: zero handled Reviewer: Looks good." {
		t.Fatalf("gate approval = %+v", gate)
	}
	if !gate.ExpiresAt.Equal(gate.RequestedAt.Add(86400 * time.Second)) {
		t.Fatalf("expiry %v for request %v", gate.ExpiresAt, gate.RequestedAt)
	}
	env.clock.Advance(time.Hour) // a human takes their time; it does not count
	if _, err := env.e.DecideApproval(ctx, gate.ID, DecisionApprove, "andre", "ship it"); err != nil {
		t.Fatal(err)
	}
	if q, _ := store.GetRun(ctx, env.pool, r.ID); q.Status != store.RunQueued || q.PausedAt != nil || q.PausedTotalMs < 3_600_000 {
		t.Fatalf("after the decision: %s paused_at=%v paused_total=%d", q.Status, q.PausedAt, q.PausedTotalMs)
	}
	r = env.resume(env.e, r.ID)
	if r.Status != store.RunSucceeded || deref(r.CurrentNode) != "end" {
		t.Fatalf("run = %s %s at %s", r.Status, deref(r.FailureReason), deref(r.CurrentNode))
	}

	// T5.6.9: state[node] is the latest visit's output, replaced rather than merged.
	st := stateOf(t, r)
	coder := st["coder"].(map[string]any)
	if coder["summary"] != "zero handled" || coder["draft"] != nil {
		t.Fatalf("state.coder = %v", coder)
	}
	if st["reviewer"].(map[string]any)["verdict"] != "approved" || st["deployer"].(map[string]any)["release_id"] != "rel-1" {
		t.Fatalf("state = %s", r.State)
	}

	msgs, _ := store.ListAgentMessages(ctx, env.pool, r.ID)
	var hops []string
	for _, m := range msgs {
		hops = append(hops, m.FromNode+"→"+m.ToNode)
	}
	if !slices.Equal(hops, []string{"coder→reviewer", "reviewer→coder", "coder→reviewer", "reviewer→coder",
		"coder→reviewer", "reviewer→gate", "deployer→end"}) {
		t.Fatalf("agent messages = %v", hops)
	}
	if !strings.Contains(string(msgs[1].Payload), "Add tests.") {
		t.Fatalf("hand-off payload = %s", msgs[1].Payload)
	}
	if n := env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='agent_message'`, r.ID); n != 7 {
		t.Fatalf("agent_message events = %d", n)
	}

	// Task rendering: the feedback block only on a revisit, $RUN_ID, input_from context.
	first, second := sc.message("coder", 1), sc.message("coder", 2)
	if strings.Contains(first, "The reviewer asked for changes") || !strings.Contains(first, "Write fizzbuzz.") ||
		!strings.Contains(first, "runs/"+r.ID+"/") || strings.Contains(first, "<no value>") {
		t.Fatalf("coder visit 1 task:\n%s", first)
	}
	if !strings.Contains(second, "The reviewer asked for changes. Address every point before finishing:\nAdd tests.") ||
		!strings.Contains(second, "Context from earlier nodes:\nstart (run input): {\"task\":\"Write fizzbuzz.\"}\nreviewer: {\"verdict\":\"changes_requested\",\"feedback\":\"Add tests.\"}") {
		t.Fatalf("coder visit 2 task:\n%s", second)
	}
	if rv := sc.message("reviewer", 2); !strings.Contains(rv, "The coder reports: with tests") || !strings.Contains(rv, "Files: fizz.py test_fizz.py") {
		t.Fatalf("reviewer visit 2 task:\n%s", rv)
	}
	if d := sc.message("deployer", 1); !strings.Contains(d, "release version 1.0.0") || !strings.Contains(d, "script_id: dev-1") {
		t.Fatalf("deployer task:\n%s", d)
	}

	// One session per visit, each with the run's model override, all deleted.
	keys := map[string]bool{}
	for _, p := range env.fc.sendParams() {
		keys[p.SessionKey] = true
	}
	if len(keys) != 7 || len(env.fc.callsFor("sessions.patch")) != 7 || len(env.fc.callsFor("sessions.delete")) != 7 {
		t.Fatalf("sessions %d, patches %d, deletes %d", len(keys), len(env.fc.callsFor("sessions.patch")), len(env.fc.callsFor("sessions.delete")))
	}
	if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND kind='run'`, r.ID); n != 1 {
		t.Fatalf("root spans = %d (the resumed worker continues the paused run's span)", n)
	}
	if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND ended_at IS NULL`, r.ID); n != 0 {
		t.Fatalf("%d spans left open", n)
	}
}

// T5.2.8: the feedback loop is bounded by max_loop_visits.
func TestLoopLimit(t *testing.T) {
	env := newEnv(t)
	wf := devPipeline(t, env)
	changes := `{"verdict":"changes_requested","feedback":"Again."}`
	env.script(map[string][]string{
		"coder":    {`{"summary":"a","files":[]}`, `{"summary":"b","files":[]}`, `{"summary":"c","files":[]}`},
		"reviewer": {changes, changes, changes},
	})
	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(`{"task":"x"}`), Limits: store.Limits{MaxLoopVisits: 2}})
	if r.Status != store.RunFailed || deref(r.FailureReason) != "loop limit 2 exceeded for coder" {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if got := visitsOf(runNodes(t, env, r.ID)); !slices.Equal(got, []string{"coder#1:succeeded", "reviewer#1:succeeded", "coder#2:succeeded", "reviewer#2:succeeded"}) {
		t.Fatalf("visits = %v", got)
	}
}

// ---- T5.1.3/T5.6.4, T5.1.4: edge choice -----------------------------------------------

func branching(t *testing.T, env *testEnv, edges ...workflow.Edge) *store.Workflow {
	a := env.agent(nil)
	agent := func(id string) workflow.Node {
		return workflow.Node{ID: id, Kind: workflow.KindAgent, AgentID: a.ID, Task: "do " + id, OutputSchema: anyObject}
	}
	def := workflow.Definition{
		Nodes: []workflow.Node{{ID: "start", Kind: workflow.KindStart}, agent("a"), agent("b"), agent("c"), {ID: "end", Kind: workflow.KindEnd}},
		Edges: append([]workflow.Edge{{From: "start", To: "a"}}, append(edges, workflow.Edge{From: "b", To: "end"}, workflow.Edge{From: "c", To: "end"})...),
	}
	return env.createWorkflow(def)
}

func TestFirstTrueEdgeWins(t *testing.T) {
	tests := []struct {
		name, output, status, reason string
		visits                       []string
	}{
		// T5.1.3 / T5.6.4: both edges hold; the first in definition order is taken.
		{name: "first of two true edges", output: `{"x":1}`, status: store.RunSucceeded, visits: []string{"a#1:succeeded", "b#1:succeeded"}},
		{name: "second edge when the first is false", output: `{"x":2}`, status: store.RunSucceeded, visits: []string{"a#1:succeeded", "c#1:succeeded"}},
		// T5.1.4: no true edge fails the run with the named reason.
		{name: "no true edge", output: `{"y":1}`, status: store.RunFailed, reason: "no matching edge from a", visits: []string{"a#1:succeeded"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			wf := branching(t, env,
				workflow.Edge{From: "a", To: "b", When: cond(t, `{"path":"/a/x","op":"eq","value":1}`)},
				workflow.Edge{From: "a", To: "c", When: cond(t, `{"path":"/a/x","op":"exists"}`)},
			)
			env.script(map[string][]string{"a": {tc.output}, "b": {`{}`}, "c": {`{}`}})
			r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID})
			if r.Status != tc.status || deref(r.FailureReason) != tc.reason {
				t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
			}
			if got := visitsOf(runNodes(t, env, r.ID)); !slices.Equal(got, tc.visits) {
				t.Fatalf("visits = %v", got)
			}
			if st := stateOf(t, r); st["a"] == nil {
				t.Fatalf("state without a's output: %s", r.State)
			}
		})
	}
}

// A workflow with only start→end succeeds at once with zero model calls.
func TestStartToEnd(t *testing.T) {
	env := newEnv(t)
	wf := env.createWorkflow(workflow.Definition{
		Nodes: []workflow.Node{{ID: "start", Kind: workflow.KindStart}, {ID: "end", Kind: workflow.KindEnd}},
		Edges: []workflow.Edge{{From: "start", To: "end"}},
	})
	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID})
	if r.Status != store.RunSucceeded || r.StepCount != 0 || len(env.fc.sendParams()) != 0 {
		t.Fatalf("run = %s steps=%d sends=%d", r.Status, r.StepCount, len(env.fc.sendParams()))
	}
}

// ---- 5.6 rule 12: on_failure ------------------------------------------------------------

func TestOnFailureEdge(t *testing.T) {
	tests := []struct {
		name, status, reason string
		limits               store.Limits
		visits               []string
	}{
		{name: "a node error follows on_failure", status: store.RunSucceeded,
			visits: []string{"a#1:failed", "fix#1:succeeded"}},
		// A node-scoped limit: the on_failure node would get a fresh budget, yet it must
		// not run.
		{name: "a limit violation does not", limits: store.Limits{MaxNodeIterations: 1}, status: store.RunFailed,
			reason: "node iteration limit 1 exceeded", visits: []string{"a#1:failed"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			ag := env.agent(nil)
			def := line(workflow.Node{ID: "a", AgentID: ag.ID, Task: "try", OnFailure: "fix"})
			def.Nodes = append(def.Nodes, workflow.Node{ID: "fix", Kind: workflow.KindAgent, AgentID: ag.ID, Task: "repair", OutputSchema: anyObject})
			def.Edges = append(def.Edges, workflow.Edge{From: "fix", To: "end"})
			wf := env.createWorkflow(def)
			env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
				node, _ := visitOf(p.SessionKey)
				if node == "fix" {
					go finishThenFinal(t, env, p, `{"fixed":true}`)
					return startedTurn(p)
				}
				go func() { // a replies without calling finish
					env.fc.push("session.message", assistantMsg(p.SessionKey, p.IdempotencyKey, []any{textPart("I give up.")}, "stop", 10, 5, 0))
					env.fc.push("chat", chatEvent(p.SessionKey, p.IdempotencyKey, "final", map[string]any{"stopReason": "stop",
						"message": map[string]any{"role": "assistant", "content": []any{textPart("I give up.")}}}))
				}()
				return startedTurn(p)
			}
			r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, Limits: tc.limits})
			if r.Status != tc.status || deref(r.FailureReason) != tc.reason {
				t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
			}
			nodes := runNodes(t, env, r.ID)
			if got := visitsOf(nodes); !slices.Equal(got, tc.visits) {
				t.Fatalf("visits = %v", got)
			}
		})
	}
}

// ---- T5.6.5: template errors -------------------------------------------------------------

func TestTemplateErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("at trigger", func(t *testing.T) {
		env := newEnv(t)
		ag := env.agent(nil)
		wf := env.createWorkflow(line(workflow.Node{ID: "a", AgentID: ag.ID, Task: "Do {{.input.task"},
			workflow.Node{ID: "gate", Kind: workflow.KindApproval, Message: "{{nofunc}}"}))
		_, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(`{"task":"t"}`)})
		var ve *ValidationError
		if !errors.As(err, &ve) || len(ve.Messages) != 2 || !strings.HasPrefix(ve.Messages[0], "nodes[1].task: template: ") ||
			!strings.HasPrefix(ve.Messages[1], "nodes[2].message: template: ") {
			t.Fatalf("err = %v", err)
		}
		if n := env.count(`SELECT count(*) FROM runs`); n != 0 {
			t.Fatalf("%d runs created", n)
		}
	})
	t.Run("at run time", func(t *testing.T) {
		env := newEnv(t)
		ag := env.agent(nil)
		// Renders against the empty state at trigger; fails once a's output is there.
		wf := env.createWorkflow(line(workflow.Node{ID: "a", AgentID: ag.ID, Task: "list"},
			workflow.Node{ID: "b", AgentID: ag.ID, Task: "{{with .state.a}}{{index .items 5}}{{end}}"}))
		env.script(map[string][]string{"a": {`{"items":[1]}`}})
		r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID})
		if r.Status != store.RunFailed || !strings.HasPrefix(deref(r.FailureReason), "task template: ") ||
			!strings.Contains(deref(r.FailureReason), "index out of range") {
			t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
		}
		if got := visitsOf(runNodes(t, env, r.ID)); !slices.Equal(got, []string{"a#1:succeeded", "b#1:failed"}) {
			t.Fatalf("visits = %v", got)
		}
		if len(env.fc.sendParams()) != 1 {
			t.Fatal("a turn was started for the node whose task did not render")
		}
	})
}

// ---- T5.6.11 and trigger-time validation -------------------------------------------------

func TestWorkflowTriggerValidation(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	wf := devPipeline(t, env)
	var ve *ValidationError
	_, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(`{"version": 2}`)})
	if !errors.As(err, &ve) || !slices.Contains(ve.Messages, "input: missing property 'task'") ||
		!slices.Contains(ve.Messages, "input.version: got number, want string") {
		t.Fatalf("T5.6.11 input schema: %v", err)
	}
	if _, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: "0199f0a2-0000-7000-8000-00000000dead"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown workflow: %v", err)
	}

	raw, _ := os.ReadFile("../../templates/dev_pipeline.json")
	var tpl struct {
		Definition workflow.Definition `json:"definition"`
	}
	_ = json.Unmarshal(raw, &tpl)
	asTemplate := env.createWorkflow(tpl.Definition)
	_, err = env.e.Trigger(ctx, TriggerRequest{WorkflowID: asTemplate.ID, Input: json.RawMessage(`{"task":"x"}`)})
	if !errors.As(err, &ve) || !slices.Equal(ve.Messages, []string{"nodes[1].agent_id: template must be instantiated",
		"nodes[2].agent_id: template must be instantiated", "nodes[4].agent_id: template must be instantiated"}) {
		t.Fatalf("template with agent names: %v", err)
	}

	ag := env.agent(nil)
	bad := line(workflow.Node{ID: "a", AgentID: "0199f0a2-0000-7000-8000-00000000dead", Task: "t"},
		workflow.Node{ID: "b", AgentID: ag.ID, Task: "t", OutputSchema: json.RawMessage(`{"type":"nope"}`)})
	bad.Edges = append(bad.Edges, workflow.Edge{From: "a", To: "ghost"})
	_, err = env.e.Trigger(ctx, TriggerRequest{WorkflowID: env.createWorkflow(bad).ID, Limits: store.Limits{MaxSteps: -1}})
	if !errors.As(err, &ve) || !slices.Contains(ve.Messages, "nodes[1].agent_id: no agent with id '0199f0a2-0000-7000-8000-00000000dead'") ||
		!slices.Contains(ve.Messages, "edges[3].to: unknown node 'ghost'") || !hasPrefix(ve.Messages, "nodes[2].output_schema: ") {
		t.Fatalf("structure and agents: %v", err)
	}

	// Limits: the request, then the workflow's own, then the defaults, then the clamp.
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(`{"task":"x"}`),
		Limits: store.Limits{MaxSteps: 500, MaxDurationSeconds: 120}})
	if err != nil {
		t.Fatal(err)
	}
	if run.Limits.MaxSteps != 200 || run.Limits.MaxDurationSeconds != 120 || run.Limits.MaxLoopVisits != 3 || run.Limits.MaxToolCalls != 80 {
		t.Fatalf("limits = %+v", run.Limits)
	}
	if string(run.Input) != `{"task": "x"}` || run.WorkflowVersion != 1 || run.Status != store.RunQueued {
		t.Fatalf("run = %+v", run)
	}
}

func hasPrefix(list []string, prefix string) bool {
	return slices.ContainsFunc(list, func(s string) bool { return strings.HasPrefix(s, prefix) })
}

// An agent deleted after the run started still runs its node (DESIGN 5.6 edge cases).
func TestDeletedAgentStillRuns(t *testing.T) {
	env := newEnv(t)
	ag, other := env.agent(nil), env.agent(func(a *store.Agent) { a.Name = "Second" })
	wf := env.createWorkflow(line(workflow.Node{ID: "a", AgentID: ag.ID, Task: "one"}, workflow.Node{ID: "b", AgentID: other.ID, Task: "two"}))
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if node, _ := visitOf(p.SessionKey); node == "a" {
			if err := store.DeleteAgent(context.Background(), env.pool, other.ID); err != nil {
				t.Error(err)
			}
		}
		go finishThenFinal(t, env, p, `{"ok":true}`)
		return startedTurn(p)
	}
	if r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID}); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
}

// ---- T5.6.7 approval nodes, T5.2.9 approval expiry ----------------------------------------

func gated(t *testing.T, env *testEnv, onReject bool, limits store.Limits) *store.Workflow {
	ag := env.agent(nil)
	gate := workflow.Node{ID: "gate", Kind: workflow.KindApproval, Message: "Ship {{.input.what}}?"}
	if onReject {
		gate.OnReject = "fix"
	}
	def := line(gate, workflow.Node{ID: "ship", AgentID: ag.ID, Task: "ship it"})
	if onReject {
		def.Nodes = append(def.Nodes, workflow.Node{ID: "fix", Kind: workflow.KindAgent, AgentID: ag.ID, Task: "fix it", OutputSchema: anyObject})
		def.Edges = append(def.Edges, workflow.Edge{From: "fix", To: "end"})
	}
	def.Limits = limits
	return env.createWorkflow(def)
}

func TestApprovalNode(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name, decision string
		onReject       bool
		status, reason string
		visits         []string
	}{
		{name: "approve continues on the first true edge", decision: DecisionApprove, status: store.RunSucceeded,
			visits: []string{"gate#1:succeeded", "ship#1:succeeded"}},
		{name: "reject without on_reject fails", decision: DecisionReject, status: store.RunFailed, reason: "rejected at gate",
			visits: []string{"gate#1:failed"}},
		{name: "reject follows on_reject", decision: DecisionReject, onReject: true, status: store.RunSucceeded,
			visits: []string{"gate#1:failed", "fix#1:succeeded"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			wf := gated(t, env, tc.onReject, store.Limits{})
			env.script(map[string][]string{"ship": {`{"shipped":true}`}, "fix": {`{"fixed":true}`}})
			r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(`{"what":"v2"}`)})
			if r.Status != store.RunWaitingApproval || r.PausedAt == nil || deref(r.ResumeNodeVisitID) == "" {
				t.Fatalf("paused run = %+v", r)
			}
			a := pendingApproval(t, env, r.ID)
			if a.Kind != store.ApprovalKindNode || a.Message != "Ship v2?" || a.RunNodeID != deref(r.ResumeNodeVisitID) || a.ExternalID != nil {
				t.Fatalf("approval = %+v", a)
			}
			if len(env.fc.callsFor("plugin.approval.request")) != 0 {
				t.Fatal("an approval node involved the gateway")
			}
			// The api restarts while the run waits: a new engine decides and resumes it.
			e2 := New(env.e.deps)
			e2.tickInterval = env.e.tickInterval
			env.e = e2 // the scripted turns submit their results to the engine running them
			decided, err := e2.DecideApproval(ctx, a.ID, tc.decision, "andre", "because")
			if err != nil || decided.DecidedBy == nil || *decided.DecidedBy != "andre" || deref(decided.Reason) != "because" {
				t.Fatalf("decide: %+v %v", decided, err)
			}
			r = env.resume(e2, r.ID)
			if r.Status != tc.status || deref(r.FailureReason) != tc.reason {
				t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
			}
			if got := visitsOf(runNodes(t, env, r.ID)); !slices.Equal(got, tc.visits) {
				t.Fatalf("visits = %v", got)
			}
			for _, typ := range []string{"approval.requested", "approval.decided"} {
				if env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event=$2`, r.ID, typ) != 1 {
					t.Errorf("%s events", typ)
				}
			}
			if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND kind='approval_wait' AND ended_at IS NOT NULL`, r.ID); n != 1 {
				t.Fatalf("closed approval spans = %d", n)
			}
			if _, err := e2.DecideApproval(ctx, a.ID, DecisionApprove, "andre", ""); !errors.Is(err, ErrNotPending) {
				t.Fatalf("T5.3.8 second decision: %v", err)
			}
		})
	}
}

// T5.2.9: an approval pending past approval_timeout_seconds expires and fails the run.
func TestApprovalNodeExpiry(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	wf := gated(t, env, true, store.Limits{ApprovalTimeoutSeconds: 5})
	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID})
	a := pendingApproval(t, env, r.ID)
	env.clock.Advance(5*time.Second - time.Millisecond)
	env.e.sweepApprovals(ctx)
	if got, _ := store.GetApproval(ctx, env.pool, a.ID); got.Status != store.ApprovalPending {
		t.Fatalf("expired one ms early: %s", got.Status)
	}
	env.clock.Advance(time.Millisecond)
	env.e.sweepApprovals(ctx)
	got, _ := store.GetApproval(ctx, env.pool, a.ID)
	if got.Status != store.ApprovalExpired || deref(got.Reason) != "approval expired" {
		t.Fatalf("approval = %s %q", got.Status, deref(got.Reason))
	}
	r, _ = store.GetRun(ctx, env.pool, r.ID)
	if r.Status != store.RunFailed || deref(r.FailureReason) != "approval expired" || r.FinishedAt == nil {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if gotV := visitsOf(runNodes(t, env, r.ID)); !slices.Equal(gotV, []string{"gate#1:failed"}) {
		t.Fatalf("visits = %v (on_reject is not followed on expiry)", gotV)
	}
	if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND ended_at IS NULL`, r.ID); n != 0 {
		t.Fatalf("%d spans left open", n)
	}
	if _, err := env.e.DecideApproval(ctx, a.ID, DecisionApprove, "andre", ""); !errors.Is(err, ErrNotPending) {
		t.Fatalf("decision after expiry: %v", err)
	}
}

// T5.2.4 across an approval node: the wait does not count, the time before it does, and
// the worker that resumes the run keeps its start time.
func TestTimeLimitAcrossAnApprovalNode(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	ag := env.agent(nil)
	wf := env.createWorkflow(line(workflow.Node{ID: "build", AgentID: ag.ID, Task: "build"},
		workflow.Node{ID: "gate", Kind: workflow.KindApproval, Message: "go?"},
		workflow.Node{ID: "ship", AgentID: ag.ID, Task: "ship"}))
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		switch node, _ := visitOf(p.SessionKey); node {
		case "build":
			env.clock.Advance(50 * time.Second)
			go finishThenFinal(t, env, p, `{"built":true}`)
		case "ship":
			env.clock.Advance(15 * time.Second) // 65 s of work in all: past the limit
		}
		return startedTurn(p)
	}
	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, Limits: store.Limits{MaxDurationSeconds: 60}})
	if r.Status != store.RunWaitingApproval {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	env.clock.Advance(3 * time.Hour)
	if _, err := env.e.DecideApproval(ctx, pendingApproval(t, env, r.ID).ID, DecisionApprove, "andre", ""); err != nil {
		t.Fatal(err)
	}
	r = env.resume(env.e, r.ID)
	if r.Status != store.RunTimedOut || deref(r.FailureReason) != "time limit 60s exceeded" {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if got := visitsOf(runNodes(t, env, r.ID)); !slices.Equal(got, []string{"build#1:succeeded", "gate#1:succeeded", "ship#1:failed"}) {
		t.Fatalf("visits = %v", got)
	}
}
