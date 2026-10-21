package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"yuno/internal/openclaw"
	"yuno/internal/store"
	"yuno/internal/tools"
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

const (
	draftMR1  = `{"merge_request_url":"https://git.example/mr/1","branch":"yuno/t1-endpoint"}`
	auditPass = `{"verdict":"ok","trace_path":"docs/plans/x/tickets/T1.md","feature":"x"}`
)

// T5.29.4 and DESIGN 5.29 rule 5: the wave pipeline's order and the manual test. The draft
// merge request waits for the operator at manual_test; a rejection sends the notes to the
// builder ("Bugs found in the manual test:"), the work is reviewed and drafted again, and
// an approval releases doc_audit and then integrator_ready.
func TestWavePipelineManualTest(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	wf := templatePipeline(t, env, "wave_pipeline.json")
	sc := env.script(map[string][]string{
		"builder":          {builderOK, builderOK},
		"code_review":      {reviewYes, reviewYes},
		"integrator_draft": {draftMR1, draftMR1},
		"doc_audit":        {auditPass},
		"integrator_ready": {mrDone},
	})
	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(waveTicket)})
	if r.Status != store.RunWaitingApproval || deref(r.CurrentNode) != "manual_test" {
		t.Fatalf("run = %s at %s %q", r.Status, deref(r.CurrentNode), deref(r.FailureReason))
	}
	a := pendingApproval(t, env, r.ID)
	for _, want := range []string{"Pull branch yuno/t1-endpoint", "run the CI tasks locally", "https://git.example/mr/1"} {
		if !strings.Contains(a.Message, want) {
			t.Fatalf("manual_test message lacks %q:\n%s", want, a.Message)
		}
	}
	if m := sc.message("integrator_draft", 1); !strings.Contains(m, "draft true") {
		t.Fatalf("integrator_draft task:\n%s", m)
	}
	if m := sc.message("builder", 1); strings.Contains(m, "Bugs found") {
		t.Fatalf("builder visit 1 mentions bugs:\n%s", m)
	}
	if _, err := env.e.DecideApproval(ctx, a.ID, DecisionReject, "andre", "the export button does nothing on Safari"); err != nil {
		t.Fatal(err)
	}
	r = env.resume(env.e, r.ID)
	if r.Status != store.RunWaitingApproval || deref(r.CurrentNode) != "manual_test" {
		t.Fatalf("after the rejection: %s at %s %q", r.Status, deref(r.CurrentNode), deref(r.FailureReason))
	}
	if m := sc.message("builder", 2); !strings.Contains(m, "Bugs found in the manual test:") || !strings.Contains(m, "the export button does nothing on Safari") {
		t.Fatalf("builder visit 2 lacks the bugs:\n%s", m)
	}
	if d := gateEntry(t, r, "manual_test"); d["decision"] != "rejected" {
		t.Fatalf("state.manual_test = %v", d)
	}
	if got := sc.message("doc_audit", 1); got != "" {
		t.Fatalf("doc_audit ran before the manual test passed:\n%s", got)
	}
	a = pendingApproval(t, env, r.ID)
	if _, err := env.e.DecideApproval(ctx, a.ID, DecisionApprove, "andre", ""); err != nil {
		t.Fatal(err)
	}
	r = env.resume(env.e, r.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	want := []string{"builder#1:succeeded", "code_review#1:succeeded", "integrator_draft#1:succeeded", "manual_test#1:failed",
		"builder#2:succeeded", "code_review#2:succeeded", "integrator_draft#2:succeeded", "manual_test#2:succeeded",
		"doc_audit#1:succeeded", "integrator_ready#1:succeeded"}
	if got := visitsOf(runNodes(t, env, r.ID)); !slices.Equal(got, want) {
		t.Fatalf("visits = %v", got)
	}
	if m := sc.message("integrator_ready", 1); !strings.Contains(m, "draft false") || !strings.Contains(m, "docs/plans/x/tickets/T1.md") || !strings.Contains(m, "yuno/t1-endpoint") ||
		!strings.Contains(m, `paths ["docs/plans/x"]`) {
		t.Fatalf("integrator_ready task:\n%s", m)
	}
	if st := stateOf(t, r); st["integrator_ready"] == nil || st["doc_audit"] == nil {
		t.Fatalf("state = %v", st)
	}
}

// wavePipelineDef reads the shipped wave pipeline's definition as it is.
func wavePipelineDef(t *testing.T) *workflow.Definition {
	t.Helper()
	raw, err := os.ReadFile("../../templates/wave_pipeline.json")
	if err != nil {
		t.Fatal(err)
	}
	var tpl struct {
		Definition workflow.Definition `json:"definition"`
	}
	if err := json.Unmarshal(raw, &tpl); err != nil {
		t.Fatal(err)
	}
	return &tpl.Definition
}

// P6: the draft step's result is the merge request the tool returned, or nothing at all
// when opening it was rejected or its approval expired. A status in place of the branch or
// the url (run 01a15a66 finished with branch "pending_approval"), or one of the two without
// the other, is refused, by the step's output schema and by the state schema alike.
func TestWavePipelineDraftResultIsARealRequest(t *testing.T) {
	def := wavePipelineDef(t)
	node, _ := def.Node("integrator_draft")
	var state struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if node == nil || json.Unmarshal(def.StateSchema, &state) != nil || state.Properties["integrator_draft"] == nil {
		t.Fatalf("integrator_draft %+v, state schema %s", node, def.StateSchema)
	}
	for name, raw := range map[string]json.RawMessage{"output schema": node.OutputSchema, "state schema": state.Properties["integrator_draft"]} {
		s, err := compileSchema(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, tc := range []struct {
			result string
			ok     bool
		}{
			{draftMR1, true},
			{`{"merge_request_url":"","branch":""}`, true},
			{`{"merge_request_url":"https://git.example/mr/1","branch":"pending_approval"}`, false},
			{`{"merge_request_url":"","branch":"pending_approval"}`, false},
			{`{"merge_request_url":"pending_approval","branch":"pending_approval"}`, false},
			{`{"merge_request_url":"https://git.example/mr/1","branch":"rejected"}`, false},
			{`{"merge_request_url":"https://git.example/mr/1","branch":""}`, false},
			{`{"merge_request_url":"","branch":"yuno/t1-endpoint"}`, false},
			{`{"merge_request_url":"https://git.example/mr/1","branch":"the branch of the draft"}`, false},
		} {
			if got := validateOutput(s, json.RawMessage(tc.result)); (got == "") != tc.ok {
				t.Errorf("%s: %s: %q, want valid %v", name, tc.result, got, tc.ok)
			}
		}
		if got := validateOutput(s, json.RawMessage(`{"merge_request_url":"https://git.example/mr/1","branch":"pending_approval"}`)); !strings.Contains(got, "/branch") {
			t.Errorf("%s: the refusal of a status does not name the branch: %q", name, got)
		}
	}
}

// P6: the manual test names the branch only when the draft merge request exists. When
// opening it was rejected or its approval expired, the message says the draft is missing
// and names no branch, and the ready step's task names none either.
func TestWavePipelineManualTestWithoutADraft(t *testing.T) {
	env := newEnv(t)
	wf := templatePipeline(t, env, "wave_pipeline.json")
	env.script(map[string][]string{
		"builder": {builderOK}, "code_review": {reviewYes}, "integrator_draft": {`{"merge_request_url":"","branch":""}`},
	})
	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(waveTicket)})
	if r.Status != store.RunWaitingApproval || deref(r.CurrentNode) != "manual_test" {
		t.Fatalf("run = %s at %s %q", r.Status, deref(r.CurrentNode), deref(r.FailureReason))
	}
	a := pendingApproval(t, env, r.ID)
	if !strings.Contains(a.Message, "The draft merge request is missing") || strings.Contains(a.Message, "Pull branch") ||
		strings.Contains(a.Message, "pending_approval") {
		t.Fatalf("manual_test message:\n%s", a.Message)
	}

	ready, _ := wavePipelineDef(t).Node("integrator_ready")
	for state, want := range map[string]string{
		`{"integrator_draft":{"merge_request_url":"","branch":""},"doc_audit":{"feature":"x","trace_path":"t.md"}}`:                                "the manual test the operator approved. ",
		`{"integrator_draft":{"merge_request_url":"https://git.example/mr/1","branch":"yuno/t1"},"doc_audit":{"feature":"x","trace_path":"t.md"}}`: "the manual test the operator approved on branch yuno/t1. ",
	} {
		task, err := workflow.Render(ready.Task, json.RawMessage(waveTicket), json.RawMessage(state))
		if err != nil || !strings.Contains(task, want) {
			t.Fatalf("integrator_ready task for %s (%v):\n%s", state, err, task)
		}
	}
}

// DESIGN 5.29 rules 5 and 7: rejecting the manual test again and again is bounded by
// max_loop_visits through the builder.
func TestWavePipelineManualTestRejectionsAreBounded(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	wf := templatePipeline(t, env, "wave_pipeline.json")
	env.script(map[string][]string{
		"builder":          {builderOK, builderOK, builderOK},
		"code_review":      {reviewYes, reviewYes, reviewYes},
		"integrator_draft": {draftMR1, draftMR1, draftMR1},
	})
	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(waveTicket)})
	for i := 0; i < 3; i++ {
		if r.Status != store.RunWaitingApproval {
			t.Fatalf("rejection %d: run = %s %q", i+1, r.Status, deref(r.FailureReason))
		}
		a := pendingApproval(t, env, r.ID)
		if _, err := env.e.DecideApproval(ctx, a.ID, DecisionReject, "andre", "still broken"); err != nil {
			t.Fatal(err)
		}
		r = env.resume(env.e, r.ID)
	}
	if r.Status != store.RunFailed || deref(r.FailureReason) != "loop limit 3 exceeded for builder" {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
}

// DESIGN 5.29 rule 6: the doc auditor finds a PRD gap and calls the registered escalate
// tool instead of finishing; the run fails with the reason and the merge request is never
// marked ready.
func TestWavePipelineDocAuditEscalates(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	wf := templatePipeline(t, env, "wave_pipeline.json")
	sc := env.script(map[string][]string{
		"builder": {builderOK}, "code_review": {reviewYes}, "integrator_draft": {draftMR1},
	})
	scripted := env.fc.onSend
	tool := tools.NewEscalate(tools.EscalateDeps{Escalate: env.e.EscalateRun})
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(waveTicket)})
	if err != nil {
		t.Fatal(err)
	}
	called := make(chan error, 1)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if node, _ := visitOf(p.SessionKey); node != "doc_audit" {
			return scripted(p, n)
		}
		go func() {
			_, err := tool.Exec(ctx, tools.Call{RunID: run.ID, Input: json.RawMessage(`{"run_token":"` + tokenIn(p.Message) +
				`","kind":"prd_gap","reason":"R4 never says who is authorised"}`)})
			called <- err
		}()
		return startedTurn(p)
	}
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunWaitingApproval || deref(r.CurrentNode) != "manual_test" {
		t.Fatalf("run = %s at %s %q", r.Status, deref(r.CurrentNode), deref(r.FailureReason))
	}
	a := pendingApproval(t, env, r.ID)
	if _, err := env.e.DecideApproval(ctx, a.ID, DecisionApprove, "andre", ""); err != nil {
		t.Fatal(err)
	}
	r = env.resume(env.e, r.ID)
	if err := recv(t, called, "the escalate call"); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if r.Status != store.RunFailed || deref(r.FailureReason) != "escalated (prd_gap): R4 never says who is authorised" {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if m := sc.message("doc_audit", 1); m != "" {
		t.Fatalf("the scripted sender saw doc_audit: %s", m)
	}
	for _, v := range visitsOf(runNodes(t, env, r.ID)) {
		if strings.HasPrefix(v, "integrator_ready") {
			t.Fatalf("integrator_ready ran: %v", v)
		}
	}
}

// DESIGN 5.29 rule 5: auto_after sits on integrator_ready only, names the manual test, and
// is valid there; it is refused on a node the approval does not dominate.
func TestWavePipelineAutoAfterPlacement(t *testing.T) {
	raw, err := os.ReadFile("../../templates/wave_pipeline.json")
	if err != nil {
		t.Fatal(err)
	}
	var tpl struct {
		Definition workflow.Definition `json:"definition"`
	}
	if err := json.Unmarshal(raw, &tpl); err != nil {
		t.Fatal(err)
	}
	def := &tpl.Definition
	for _, n := range def.Nodes {
		if (n.AutoAfter != "") != (n.ID == "integrator_ready") {
			t.Errorf("node %s: auto_after = %q", n.ID, n.AutoAfter)
		}
		if n.ID == "gap_found" {
			t.Error("the gap_found approval is still in the pipeline")
		}
	}
	if n, _ := def.Node("integrator_ready"); n == nil || n.AutoAfter != "manual_test" {
		t.Fatalf("integrator_ready = %+v", n)
	}
	if mt, _ := def.Node("manual_test"); mt == nil || mt.Kind != workflow.KindApproval || mt.OnReject != "builder" {
		t.Fatalf("manual_test = %+v", mt)
	}
	if err := def.ValidateStructure(); err != nil {
		t.Fatalf("template invalid: %v", err)
	}
	for i := range def.Nodes {
		if def.Nodes[i].ID == "integrator_draft" {
			def.Nodes[i].AutoAfter = "manual_test" // reachable without passing the approval
		}
	}
	if err := def.ValidateStructure(); err == nil || !strings.Contains(err.Error(), "must precede") {
		t.Fatalf("misplaced auto_after accepted: %v", err)
	}
}

const slicerOut = `{"tickets":[
 {"id":"T1","title":"Endpoint","slice":"An endpoint.","files":["api/a.go","api/shared.go"],"tests":["a_test"]},
 {"id":"T2","title":"Client","slice":"A client.","files":["web/c.ts","api/shared.go"],"tests":["c_test"]},
 {"id":"T3","title":"Docs","slice":"Docs.","files":["README.md"],"tests":["d_test"]}]}`

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
		"integrator": {`{"merge_request_url":"https://git.example/mr/1","plan_id":"p1","issues_count":3}`},
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

const shipInput = `{"plan_id":"p1","feature":"x","wave":1,"last_wave":2}`

// shipRun starts a ship pipeline run whose integrator calls advance_wave once, through the
// real tool with a fake AdvanceWave that reports last, and finishes with the tool's answer.
// The tool-call gate (auto_after) is httpapi's and has its own tests (T5.29.4); here the
// call is made directly, as the escalate test does. It returns the run waiting at
// smoke_test, the count of advance calls and the scripted sender.
func shipRun(t *testing.T, env *testEnv, last bool) (*store.Run, *int32, *scripted) {
	t.Helper()
	r, calls, sc, _ := shipRunLessons(t, env, last, false, nil)
	return r, calls, sc
}

// lessonsGate is the integrator's open_merge_request for the lessons, as the MCP handler
// raises it: a recorded call and a pending tool approval the handler waits on.
type lessonsGate struct {
	wait   chan error
	status chan string
}

// shipRunLessons is shipRun whose integrator, when lessonsMR is set, also calls the
// approval-gated open_merge_request with paths ["CLAUDE.md"] after advance_wave and waits
// for the operator's decision before it finishes (DESIGN 5.29 rule 13). The finish
// carries lessons.
func shipRunLessons(t *testing.T, env *testEnv, last, lessonsMR bool, lessons []string) (*store.Run, *int32, *scripted, *lessonsGate) {
	t.Helper()
	if lessons == nil {
		lessons = []string{"pin the migration order", "smoke test the export"}
	}
	lj, _ := json.Marshal(lessons)
	lg := &lessonsGate{wait: make(chan error, 1), status: make(chan string, 1)}
	ctx := context.Background()
	wf := templatePipeline(t, env, "ship_pipeline.json")
	sc := env.script(map[string][]string{})
	var calls int32
	var runID atomic.Value
	tool := tools.NewAdvanceWave(tools.AdvanceWaveDeps{
		AdvanceWave: func(_ context.Context, id string) (tools.AdvanceResult, error) {
			atomic.AddInt32(&calls, 1)
			return tools.AdvanceResult{PlanID: "p1", Wave: 1, Closed: 2, Released: 1, Last: last}, nil
		},
		RunIDForToken: func(string) (string, bool) { id, _ := runID.Load().(string); return id, id != "" },
	})
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		node, _ := visitOf(p.SessionKey)
		sc.mu.Lock()
		sc.messages[node] = append(sc.messages[node], p.Message)
		sc.mu.Unlock()
		if node != "integrator" {
			t.Errorf("unexpected visit of %s", node)
			return startedTurn(p)
		}
		go func() {
			res, err := tool.Exec(ctx, tools.Call{RunID: runID.Load().(string), Input: json.RawMessage(`{"run_token":"` + tokenIn(p.Message) + `"}`)})
			if err != nil || !strings.Contains(string(res.Output), `"plan_id":"p1"`) {
				t.Errorf("advance_wave: %v %+v", err, res)
			}
			if lessonsMR {
				rc, ok := env.e.ResolveRunToken(tokenIn(p.Message))
				if !ok {
					t.Error("token not live")
					return
				}
				key := "key-lessons"
				call := &store.ToolCall{RunID: rc.RunID, RunNodeID: rc.RunNodeID, ToolUseID: "mcp-" + store.NewID(), ToolName: "open_merge_request",
					Input:  json.RawMessage(`{"run_token":"` + tokenIn(p.Message) + `","title":"Lessons from x, wave 1","description":"Lessons of wave 1.","paths":["CLAUDE.md"]}`),
					Status: store.ToolAwaitingApproval, IdempotencyKey: &key}
				if err := store.InsertToolCall(ctx, env.pool, call); err != nil {
					t.Error(err)
					return
				}
				a, err := env.e.RequestApproval(ctx, ApprovalRequest{Token: tokenIn(p.Message), ToolName: "open_merge_request", ToolCallID: call.ID,
					Message: "Open a merge request: Lessons from x, wave 1 (1 file)", Input: call.Input})
				if err != nil {
					t.Errorf("RequestApproval: %v", err)
					return
				}
				got, err := env.e.WaitApproval(ctx, a.ID)
				lg.wait <- err
				if err == nil {
					lg.status <- got.Status
				}
			}
			finishThenFinal(t, env, p, `{"wave":1,"last":`+strconv.FormatBool(last)+`,"lessons":`+string(lj)+`}`)
		}()
		return startedTurn(p)
	}
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(shipInput)})
	if err != nil {
		t.Fatal(err)
	}
	runID.Store(run.ID)
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunWaitingApproval || deref(r.CurrentNode) != "smoke_test" {
		t.Fatalf("run = %s at %s %q", r.Status, deref(r.CurrentNode), deref(r.FailureReason))
	}
	return r, &calls, sc, lg
}

// shipWithLessonsMR approves the smoke test and runs the integrator to its end while the
// lessons merge request is decided as decision (DESIGN 5.29 rule 13, D-075).
func shipWithLessonsMR(t *testing.T, decision string, lessons []string) (*testEnv, *store.Run, *int32, *lessonsGate) {
	t.Helper()
	ctx := context.Background()
	env := newEnv(t)
	r, calls, _, lg := shipRunLessons(t, env, false, decision != "", lessons)
	if _, err := env.e.DecideApproval(ctx, pendingApproval(t, env, r.ID).ID, DecisionApprove, "andre", ""); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimQueuedRun(ctx, env.pool)
	if err != nil || claimed.ID != r.ID {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.e.runRun(ctx, claimed)
	}()
	if decision != "" {
		var ap *store.Approval
		eventually(t, "the lessons approval", func() bool {
			pending, _ := store.ListPendingApprovals(ctx, env.pool, r.ID)
			if len(pending) == 1 {
				ap = &pending[0]
			}
			return ap != nil
		})
		call, err := store.GetToolCall(ctx, env.pool, deref(ap.ToolCallID))
		var in struct{ Paths []string }
		if err == nil {
			err = json.Unmarshal(call.Input, &in)
		}
		if err != nil || ap.Kind != store.ApprovalKindTool || ap.NodeID != "integrator" || call.ToolName != "open_merge_request" ||
			!slices.Equal(in.Paths, []string{"CLAUDE.md"}) {
			t.Fatalf("approval = %+v call %+v %v", ap, call, err)
		}
		if _, err := env.e.DecideApproval(ctx, ap.ID, decision, "andre", "not now"); err != nil {
			t.Fatal(err)
		}
		if err := recv(t, lg.wait, "the lessons decision"); err != nil {
			t.Fatalf("wait: %v", err)
		}
	}
	return env, env.wait(done, r.ID), calls, lg
}

// DESIGN 5.29 rule 13 (D-075): the integrator opens the lessons merge request through the
// normal approval gate; approved or rejected, the ship run still succeeds, and the
// rejection reaches the agent as a decision, not a failure.
func TestShipPipelineLessonsMergeRequest(t *testing.T) {
	for _, decision := range []string{DecisionApprove, DecisionReject} {
		t.Run(decision, func(t *testing.T) {
			env, r, calls, lg := shipWithLessonsMR(t, decision, nil)
			if r.Status != store.RunSucceeded {
				t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
			}
			want := store.ApprovalApproved
			if decision == DecisionReject {
				want = store.ApprovalRejected
			}
			if got := recv(t, lg.status, "the status"); got != want {
				t.Fatalf("status = %s", got)
			}
			if n := atomic.LoadInt32(calls); n != 1 {
				t.Fatalf("advance_wave calls = %d", n)
			}
			if n := env.count(`SELECT count(*) FROM approvals a JOIN tool_calls c ON c.id=a.tool_call_id WHERE a.run_id=$1 AND a.kind='tool' AND c.tool_name='open_merge_request'`, r.ID); n != 1 {
				t.Fatalf("lessons approvals = %d", n)
			}
		})
	}
}

// DESIGN 5.29 rule 13: a wave without lessons opens no merge request.
func TestShipPipelineNoLessonsNoMergeRequest(t *testing.T) {
	env, r, _, _ := shipWithLessonsMR(t, "", []string{})
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if n := env.count(`SELECT count(*) FROM approvals WHERE run_id=$1 AND kind='tool'`, r.ID); n != 0 {
		t.Fatalf("tool approvals = %d", n)
	}
}

// DESIGN 5.29 rule 13: a wave that is not the last ends after the integrator. The smoke
// test message names the wave and the feature; the integrator's task asks for project
// notes and one advance_wave; the call needs no approval of its own (auto_after).
func TestShipPipelineWaveNotLast(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	r, calls, sc := shipRun(t, env, false)
	a := pendingApproval(t, env, r.ID)
	for _, want := range []string{"Wave 1 of x", "Smoke test the deployed app", "passed"} {
		if !strings.Contains(a.Message, want) {
			t.Fatalf("smoke_test message lacks %q:\n%s", want, a.Message)
		}
	}
	if m := sc.message("integrator", 1); m != "" {
		t.Fatalf("the integrator ran before the smoke test:\n%s", m)
	}
	if _, err := env.e.DecideApproval(ctx, a.ID, DecisionApprove, "andre", ""); err != nil {
		t.Fatal(err)
	}
	r = env.resume(env.e, r.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if got := visitsOf(runNodes(t, env, r.ID)); !slices.Equal(got, []string{"smoke_test#1:succeeded", "integrator#1:succeeded"}) {
		t.Fatalf("visits = %v", got)
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Fatalf("advance_wave calls = %d", n)
	}
	if m := sc.message("integrator", 1); !strings.Contains(m, "memory_save") || !strings.Contains(m, `scope "project"`) ||
		!strings.Contains(m, "advance_wave exactly once") || !strings.Contains(m, "kind other") || !strings.Contains(m, "wave 1") {
		t.Fatalf("integrator task:\n%s", m)
	}
	if st := stateOf(t, r); st["integrator"] == nil {
		t.Fatalf("state = %v", st)
	}
}

// DESIGN 5.29 rules 13 and 15: after the last wave the operator checks the live app
// against the PRD; approving ends the run, rejecting fails it with the operator's reason
// (the plan side turns that into a pause).
func TestShipPipelineLastWaveAccepts(t *testing.T) {
	ctx := context.Background()
	for _, decision := range []string{DecisionApprove, DecisionReject} {
		env := newEnv(t)
		r, calls, _ := shipRun(t, env, true)
		a := pendingApproval(t, env, r.ID)
		if _, err := env.e.DecideApproval(ctx, a.ID, DecisionApprove, "andre", ""); err != nil {
			t.Fatal(err)
		}
		r = env.resume(env.e, r.ID)
		if r.Status != store.RunWaitingApproval || deref(r.CurrentNode) != "accept_feature" {
			t.Fatalf("run = %s at %s %q", r.Status, deref(r.CurrentNode), deref(r.FailureReason))
		}
		a = pendingApproval(t, env, r.ID)
		for _, want := range []string{"docs/plans/x/PRD.md", "live app", "Approve to finish", "Reject with what is missing"} {
			if !strings.Contains(a.Message, want) {
				t.Fatalf("accept_feature message lacks %q:\n%s", want, a.Message)
			}
		}
		if _, err := env.e.DecideApproval(ctx, a.ID, decision, "andre", "the export is missing"); err != nil {
			t.Fatal(err)
		}
		r = env.resume(env.e, r.ID)
		if decision == DecisionApprove {
			want := []string{"smoke_test#1:succeeded", "integrator#1:succeeded", "accept_feature#1:succeeded"}
			if got := visitsOf(runNodes(t, env, r.ID)); r.Status != store.RunSucceeded || !slices.Equal(got, want) {
				t.Fatalf("approved: run = %s %q visits %v", r.Status, deref(r.FailureReason), got)
			}
		} else if r.Status != store.RunFailed || deref(r.FailureReason) != "rejected at accept_feature: the export is missing" {
			t.Fatalf("rejected: run = %s %q", r.Status, deref(r.FailureReason))
		}
		if n := atomic.LoadInt32(calls); n != 1 {
			t.Fatalf("advance_wave calls = %d", n)
		}
	}
}

// DESIGN 5.29 rule 13: a rejected smoke test ends the run without the integrator, so
// nothing is advanced (no on_reject; the plan side pauses the waves with the reason).
func TestShipPipelineSmokeTestRejected(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	r, calls, sc := shipRun(t, env, false)
	a := pendingApproval(t, env, r.ID)
	if _, err := env.e.DecideApproval(ctx, a.ID, DecisionReject, "andre", "login is broken"); err != nil {
		t.Fatal(err)
	}
	r = env.resume(env.e, r.ID)
	if r.Status != store.RunFailed || deref(r.FailureReason) != "rejected at smoke_test: login is broken" {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if m := sc.message("integrator", 1); m != "" || atomic.LoadInt32(calls) != 0 {
		t.Fatalf("the integrator ran after a rejection (calls %d):\n%s", atomic.LoadInt32(calls), m)
	}
}

// DESIGN 5.29 rule 13 (review L4): the smoke test message lists the wave's merged merge
// requests and links main's pipeline when the input carries them, and reads cleanly when
// it carries neither.
func TestShipPipelineSmokeTestMessage(t *testing.T) {
	raw, err := os.ReadFile("../../templates/ship_pipeline.json")
	if err != nil {
		t.Fatal(err)
	}
	var tpl struct {
		Definition workflow.Definition `json:"definition"`
	}
	if err := json.Unmarshal(raw, &tpl); err != nil {
		t.Fatal(err)
	}
	node, ok := tpl.Definition.Node("smoke_test")
	if !ok {
		t.Fatal("no smoke_test node")
	}
	render := func(input string) string {
		t.Helper()
		out, err := workflow.Render(node.Message, json.RawMessage(input), nil)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	full := render(`{"plan_id":"p1","feature":"x","wave":1,"last_wave":2,"main_url":"https://gitlab.test/g/d/-/pipelines/9",` +
		`"merge_requests":[{"title":"Add login","url":"https://gitlab.test/g/d/-/merge_requests/3"},{"title":"Add logout"}]}`)
	for _, want := range []string{"Merged in this wave:\n- Add login (https://gitlab.test/g/d/-/merge_requests/3)\n- Add logout\n",
		"Pipeline on the default branch: https://gitlab.test/g/d/-/pipelines/9\n", "Smoke test the deployed app now."} {
		if !strings.Contains(full, want) {
			t.Errorf("message lacks %q:\n%s", want, full)
		}
	}
	bare := render(shipInput)
	want := "Wave 1 of x is merged and the pipeline on the default branch passed.\n\nSmoke test the deployed app now."
	if !strings.HasPrefix(bare, want) || strings.Contains(bare, "Merged in this wave") || strings.Contains(bare, "Pipeline on the default branch:") ||
		strings.Contains(bare, "<no value>") {
		t.Errorf("message without the fields:\n%s", bare)
	}
}

// DESIGN 5.29 rules 13 and 14: auto_after sits on the integrator only and names the smoke
// test; the edges route on state.integrator.last; the template validates and a misplaced
// auto_after is refused.
func TestShipPipelineStructure(t *testing.T) {
	raw, err := os.ReadFile("../../templates/ship_pipeline.json")
	if err != nil {
		t.Fatal(err)
	}
	var tpl struct {
		Name       string              `json:"name"`
		Definition workflow.Definition `json:"definition"`
	}
	if err := json.Unmarshal(raw, &tpl); err != nil {
		t.Fatal(err)
	}
	def := &tpl.Definition
	if tpl.Name != "Ship pipeline" || !def.Project {
		t.Fatalf("name %q project %v", tpl.Name, def.Project)
	}
	for _, n := range def.Nodes {
		if (n.AutoAfter != "") != (n.ID == "integrator") {
			t.Errorf("node %s: auto_after = %q", n.ID, n.AutoAfter)
		}
		if n.Kind == workflow.KindApproval && n.OnReject != "" {
			t.Errorf("approval %s has on_reject %q", n.ID, n.OnReject)
		}
	}
	if n, _ := def.Node("integrator"); n == nil || n.AutoAfter != "smoke_test" || n.AgentName != "Integrator" {
		t.Fatalf("integrator = %+v", n)
	}
	if err := def.ValidateStructure(); err != nil {
		t.Fatalf("template invalid: %v", err)
	}
	if msgs := def.ValidateTemplates(json.RawMessage(shipInput)); len(msgs) > 0 {
		t.Fatalf("templates: %v", msgs)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"merge_requests", "main_url"} {
		if _, ok := schema.Properties[k]; !ok || slices.Contains(schema.Required, k) {
			t.Errorf("input %s: declared %v, required %v", k, ok, slices.Contains(schema.Required, k))
		}
	}
	if def.Limits.MaxSteps > 60 {
		t.Fatalf("limits = %+v", def.Limits)
	}
	for i := range def.Nodes {
		if def.Nodes[i].ID == "integrator" {
			def.Nodes[i].AutoAfter = "accept_feature" // an approval that comes after the integrator
		}
	}
	if err := def.ValidateStructure(); err == nil {
		t.Fatal("a misplaced auto_after was accepted")
	}
}
