package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"yuno/internal/openclaw"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// templatePipeline stores templates/<file> with its agent names resolved to new agents,
// as instantiating the template does (the project requirement is dropped: these tests
// cover the graph).
func templatePipeline(t *testing.T, env *testEnv, file string) *store.Workflow {
	t.Helper()
	raw, err := os.ReadFile("../../templates/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var tpl struct {
		Definition workflow.Definition `json:"definition"`
	}
	if err := json.Unmarshal(raw, &tpl); err != nil {
		t.Fatal(err)
	}
	tpl.Definition.Project = false
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

const (
	builderOK  = `{"summary":"built","files":["a.go"],"tests_passed":true}`
	reviewNo   = `{"verdict":"changes_requested","comments":"cover the empty case"}`
	reviewYes  = `{"verdict":"approved","comments":"fine"}`
	auditOK    = `{"verdict":"ok","reason":"all met","trace":[]}`
	auditGap   = `{"verdict":"gap","reason":"R3 has no code","trace":[]}`
	mrDone     = `{"merge_request_url":"https://git.example/mr/1"}`
	waveTicket = `{"task":"T1.1 add the endpoint"}`
)

// T5.28.6: the wave pipeline's review loop is bounded by max_loop_visits 3.
func TestWavePipelineReviewLoopIsBounded(t *testing.T) {
	env := newEnv(t)
	wf := templatePipeline(t, env, "wave_pipeline.json")
	sc := env.script(map[string][]string{
		"builder":     {builderOK, builderOK, builderOK},
		"code_review": {reviewNo, reviewNo, reviewNo},
	})
	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(waveTicket)})
	if r.Status != store.RunFailed || deref(r.FailureReason) != "loop limit 3 exceeded for builder" {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	want := []string{"builder#1:succeeded", "code_review#1:succeeded", "builder#2:succeeded", "code_review#2:succeeded",
		"builder#3:succeeded", "code_review#3:succeeded"}
	if got := visitsOf(runNodes(t, env, r.ID)); !slices.Equal(got, want) {
		t.Fatalf("visits = %v", got)
	}
	if m := sc.message("builder", 1); strings.Contains(m, "asked for changes") || !strings.Contains(m, "T1.1 add the endpoint") {
		t.Fatalf("builder visit 1:\n%s", m)
	}
	if m := sc.message("builder", 2); !strings.Contains(m, "cover the empty case") {
		t.Fatalf("builder visit 2 lacks the review comments:\n%s", m)
	}
}

// T5.28.6: a gap found by the doc audit waits at gap_found; approving opens the merge
// request anyway, rejecting fails the run "rejected at gap_found".
func TestWavePipelineGapPath(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, decision, status, reason string
		visits                         []string
	}{
		{"reject", DecisionReject, store.RunFailed, "rejected at gap_found: because",
			[]string{"builder#1:succeeded", "code_review#1:succeeded", "doc_audit#1:succeeded", "gap_found#1:failed"}},
		{"approve", DecisionApprove, store.RunSucceeded, "",
			[]string{"builder#1:succeeded", "code_review#1:succeeded", "doc_audit#1:succeeded", "gap_found#1:succeeded", "integrator#1:succeeded"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			wf := templatePipeline(t, env, "wave_pipeline.json")
			sc := env.script(map[string][]string{
				"builder": {builderOK}, "code_review": {reviewYes}, "doc_audit": {auditGap}, "integrator": {mrDone},
			})
			r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(waveTicket)})
			if r.Status != store.RunWaitingApproval || deref(r.CurrentNode) != "gap_found" {
				t.Fatalf("run = %s at %s", r.Status, deref(r.CurrentNode))
			}
			a := pendingApproval(t, env, r.ID)
			if !strings.Contains(a.Message, "R3 has no code") {
				t.Fatalf("message = %q", a.Message)
			}
			if _, err := env.e.DecideApproval(ctx, a.ID, tc.decision, "andre", "because"); err != nil {
				t.Fatal(err)
			}
			r = env.resume(env.e, r.ID)
			if r.Status != tc.status || deref(r.FailureReason) != tc.reason {
				t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
			}
			if got := visitsOf(runNodes(t, env, r.ID)); !slices.Equal(got, tc.visits) {
				t.Fatalf("visits = %v", got)
			}
			if tc.decision == DecisionApprove && !strings.Contains(sc.message("integrator", 1), "R3 has no code") {
				t.Fatalf("integrator task lacks the audit reason:\n%s", sc.message("integrator", 1))
			}
		})
	}
}

const slicerOut = `{"tickets":[
 {"id":"T1","title":"Endpoint","files":["api/a.go","api/shared.go"],"tests":["a_test"]},
 {"id":"T2","title":"Client","files":["web/c.ts","api/shared.go"],"tests":["c_test"]},
 {"id":"T3","title":"Docs","files":["README.md"],"tests":["d_test"]}]}`

// T5.28.4: the waves node's finish is refused for a ticket in two waves and two tickets
// sharing a file (workflow's checks_test.go covers the other refusals); the first valid one is
// taken. T5.28.3: the plan pipeline runs to its end through both approvals, and the
// Architect's second visit carries the operator's rejection reason.
func TestPlanPipelineEndToEnd(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	wf := templatePipeline(t, env, "plan_pipeline.json")
	sc := env.script(map[string][]string{
		"analyst":    {`{"prd_path":"docs/plans/x/PRD.md","summary":"prd","open_questions":[]}`},
		"scout":      {`{"gaps":[],"constraints":["go test ./..."]}`},
		"architect":  {`{"architecture_path":"docs/plans/x/ARCHITECTURE.md","summary":"v1"}`, `{"architecture_path":"docs/plans/x/ARCHITECTURE.md","summary":"v2"}`},
		"slicer":     {slicerOut},
		"integrator": {mrDone},
	})
	scripted := env.fc.onSend
	refusals := make(chan string, 8)
	bad := []string{
		`{"waves":[{"n":1,"tickets":["T1","T3"]},{"n":2,"tickets":["T2","T3"]}]}`, // T3 twice
		`{"waves":[{"n":1,"tickets":["T1","T2"]},{"n":2,"tickets":["T3"]}]}`,      // T1 and T2 share api/shared.go
	}
	good := `{"waves":[{"n":1,"tickets":["T1","T3"]},{"n":2,"tickets":["T2"]}]}`
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if node, _ := visitOf(p.SessionKey); node != "waves" {
			return scripted(p, n)
		}
		go func() {
			token := tokenIn(p.Message)
			for _, b := range bad { // two refusals, then the third attempt passes
				var fe *FinishValidationError
				if err := env.e.AcceptFinish(ctx, token, json.RawMessage(b)); !errors.As(err, &fe) {
					t.Errorf("finish %s: %v", b, err)
					continue
				}
				refusals <- fe.Message
			}
			finishThenFinal(t, env, p, good)
		}()
		return startedTurn(p)
	}

	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(`{"task":"x","feature":"x"}`)})
	if r.Status != store.RunWaitingApproval || deref(r.CurrentNode) != "design_gate" {
		t.Fatalf("run = %s at %s %q", r.Status, deref(r.CurrentNode), deref(r.FailureReason))
	}
	a := pendingApproval(t, env, r.ID)
	if !strings.Contains(a.Message, "v1") || !strings.Contains(a.Message, "docs/plans/x/ARCHITECTURE.md") {
		t.Fatalf("design message = %q", a.Message)
	}
	if m := sc.message("architect", 1); strings.Contains(m, "rejected") {
		t.Fatalf("architect visit 1 mentions a rejection:\n%s", m)
	}
	if _, err := env.e.DecideApproval(ctx, a.ID, DecisionReject, "andre", "too much coupling to billing"); err != nil {
		t.Fatal(err)
	}
	r = env.resume(env.e, r.ID)
	if r.Status != store.RunWaitingApproval || deref(r.CurrentNode) != "design_gate" {
		t.Fatalf("after the rejection: %s at %s %q", r.Status, deref(r.CurrentNode), deref(r.FailureReason))
	}
	if m := sc.message("architect", 2); !strings.Contains(m, "too much coupling to billing") {
		t.Fatalf("architect visit 2 lacks the reason:\n%s", m)
	}
	a = pendingApproval(t, env, r.ID)
	if !strings.Contains(a.Message, "v2") {
		t.Fatalf("second design message = %q", a.Message)
	}
	if d := gateEntry(t, r, "design_gate"); d["decision"] != "rejected" || d["reason"] != "too much coupling to billing" {
		t.Fatalf("state.design_gate while rejected = %v", d)
	}
	if _, err := env.e.DecideApproval(ctx, a.ID, DecisionApprove, "andre", ""); err != nil {
		t.Fatal(err)
	}
	r = env.resume(env.e, r.ID)
	// Review L7: the approval replaces the rejection the gate left in state.
	if d := gateEntry(t, r, "design_gate"); d["decision"] != "approved" || d["reason"] != "" {
		t.Fatalf("state.design_gate after the approval = %v", d)
	}
	if r.Status != store.RunWaitingApproval || deref(r.CurrentNode) != "plan_gate" {
		t.Fatalf("run = %s at %s %q", r.Status, deref(r.CurrentNode), deref(r.FailureReason))
	}
	close(refusals)
	var got []string
	for m := range refusals {
		got = append(got, m)
	}
	if len(got) != 2 || !strings.Contains(got[0], "T3 is in wave 1 and wave 2") || !strings.Contains(got[1], "both list api/shared.go") {
		t.Fatalf("refusals = %q", got)
	}
	a = pendingApproval(t, env, r.ID)
	if !strings.Contains(a.Message, "Wave 1: T1 T3") || !strings.Contains(a.Message, "T2 Client") {
		t.Fatalf("plan message = %q", a.Message)
	}
	if _, err := env.e.DecideApproval(ctx, a.ID, DecisionApprove, "andre", ""); err != nil {
		t.Fatal(err)
	}
	r = env.resume(env.e, r.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
}

func gateEntry(t *testing.T, r *store.Run, gate string) map[string]string {
	t.Helper()
	var st map[string]json.RawMessage
	if err := json.Unmarshal(r.State, &st); err != nil {
		t.Fatal(err)
	}
	var d map[string]string
	if err := json.Unmarshal(st[gate], &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// Review M5: a result in the final reply that a check refuses is refused like a finish
// call (the reason goes back in a new turn); the corrected finish passes.
func TestReplyResultRefusedByCheckIsRetried(t *testing.T) {
	env := newEnv(t)
	ag := env.agent(nil)
	wf := env.createWorkflow(line(
		workflow.Node{ID: "slicer", AgentID: ag.ID, Task: "slice"},
		workflow.Node{ID: "waves", AgentID: ag.ID, Task: "wave", InputFrom: []string{"slicer"}, Checks: []string{workflow.CheckWavesPartition}}))
	var mu sync.Mutex
	sends := map[string]int{}
	var messages []string
	firstWaves := "" // the first message of the waves visit carries the run token
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		node, _ := visitOf(p.SessionKey)
		mu.Lock()
		sends[node]++
		turn := sends[node]
		messages = append(messages, node+": "+p.Message)
		if node == "waves" && turn == 1 {
			firstWaves = p.Message
		}
		first := firstWaves
		mu.Unlock()
		go func() {
			switch {
			case node == "slicer":
				finishThenFinal(t, env, p, `{"tickets":[{"id":"T1"},{"id":"T2"}]}`)
			case turn == 1: // the result only in the reply, and it misses T2
				text := `{"waves":[{"n":1,"tickets":["T1"]}]}`
				k, r := p.SessionKey, p.IdempotencyKey
				final := assistantMsg(k, r, []any{textPart(text)}, "stop", 100, 5, 0)["message"]
				env.fc.addHistory(k, final)
				env.fc.push("chat", chatEvent(k, r, "final", map[string]any{"stopReason": "stop", "message": final}))
			default:
				p.Message = first // the agent still holds the token from its first message
				finishThenFinal(t, env, p, `{"waves":[{"n":1,"tickets":["T1","T2"]}]}`)
			}
		}()
		return startedTurn(p)
	}
	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID})
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	mu.Lock()
	defer mu.Unlock()
	if sends["waves"] != 2 {
		t.Fatalf("waves turns = %d", sends["waves"])
	}
	found := false
	for _, m := range messages {
		found = found || (strings.HasPrefix(m, "waves:") && strings.Contains(m, "Your result was refused") && strings.Contains(m, "tickets not placed in any wave: T2"))
	}
	if !found {
		t.Fatalf("no turn carried the reason:\n%s", strings.Join(messages, "\n---\n"))
	}
}

// Review M6: a strict state_schema that does not list the gate still lets a run through
// a rejected and then approved gate.
func TestStrictStateSchemaIgnoresGateEntries(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	ag := env.agent(nil)
	def := line(
		workflow.Node{ID: "work", AgentID: ag.ID, Task: "work {{with .state.gate}}{{.reason}}{{end}}"},
		workflow.Node{ID: "gate", Kind: workflow.KindApproval, Message: "ok?", OnReject: "work"},
		workflow.Node{ID: "after", AgentID: ag.ID, Task: "after"})
	def.StateSchema = json.RawMessage(`{"type":"object","properties":{"work":{"type":"object"},"after":{"type":"object"}},"additionalProperties":false}`)
	wf := env.createWorkflow(def)
	env.script(map[string][]string{"work": {`{"a":1}`, `{"a":2}`}, "after": {`{"b":1}`}})
	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID})
	for _, d := range []string{DecisionReject, DecisionApprove} {
		if r.Status != store.RunWaitingApproval {
			t.Fatalf("before %s: run = %s %q", d, r.Status, deref(r.FailureReason))
		}
		a := pendingApproval(t, env, r.ID)
		if _, err := env.e.DecideApproval(ctx, a.ID, d, "andre", "again"); err != nil {
			t.Fatal(err)
		}
		r = env.resume(env.e, r.ID)
	}
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
}
