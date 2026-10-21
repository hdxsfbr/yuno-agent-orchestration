package engine

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

const (
	docAuditQuestion = "R4 never says who may export. Which reading is right?"
	docAuditNotes    = "Fix the code: reject an export from a user without the admin role, with a test."
	docAuditRework   = `{"verdict":"rework","trace_path":"docs/plans/x/tickets/T1.md","feature":"x","notes":"` + docAuditNotes + `"}`
)

// askingWavePipeline is the wave pipeline whose Doc Auditor holds ask_operator, as the
// seeded agent does.
func askingWavePipeline(t *testing.T, env *testEnv) *store.Workflow {
	t.Helper()
	ctx := context.Background()
	wf := templatePipeline(t, env, "wave_pipeline.json")
	var def workflow.Definition
	if err := json.Unmarshal(wf.Definition, &def); err != nil {
		t.Fatal(err)
	}
	node, ok := def.Node("doc_audit")
	if !ok {
		t.Fatal("no doc_audit node")
	}
	auditor, err := store.GetAgent(ctx, env.pool, node.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	auditor.ToolAllowlist = []string{"read", "ls", "write", "edit", "exec", "ask_operator", "escalate", "finish"}
	if err := store.UpdateAgent(ctx, env.pool, auditor); err != nil {
		t.Fatal(err)
	}
	return wf
}

// Run 01a15acf failed with "state schema: at /doc_audit/verdict: value must be 'ok'" after
// the Doc Auditor asked a question (D-084). A conflict the audit finds now asks the operator
// and the run waits: the step parks on the question, the answer wakes it, the audit finishes
// "rework" with the decision in its notes, the Builder gets them in its next task, and the
// loop runs through review, draft and manual test again to the end.
func TestWavePipelineDocAuditAsksThenSendsTheDecisionToTheBuilder(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	wf := askingWavePipeline(t, env)
	sc := env.script(map[string][]string{
		"builder":          {builderOK, builderOK},
		"code_review":      {reviewYes, reviewYes},
		"integrator_draft": {draftMR1, draftMR1},
		"integrator_ready": {mrDone},
	})
	other := env.fc.onSend
	asked := make(chan askOutcome, 1)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		node, visit := visitOf(p.SessionKey)
		if node != "doc_audit" {
			return other(p, n)
		}
		sc.mu.Lock()
		sc.messages[node] = append(sc.messages[node], p.Message)
		sc.mu.Unlock()
		go func() {
			switch visit {
			case 1: // asks, is told the answer is pending, and ends its turn: the step parks
				asked <- env.askAs(p, docAuditQuestion, []string{"Fix the code", "Change the documents", "Stop this ticket"}, 40*time.Millisecond)
				env.endTurn(p, "Waiting for the operator's decision.")
			case 2: // the continuation after the answer
				finishThenFinal(t, env, p, docAuditRework)
			default: // the audit after the rework
				finishThenFinal(t, env, p, auditPass)
			}
		}()
		return startedTurn(p)
	}

	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(waveTicket)})
	approveManualTest := func() {
		t.Helper()
		if r.Status != store.RunWaitingApproval || deref(r.CurrentNode) != "manual_test" {
			t.Fatalf("run = %s at %s %q", r.Status, deref(r.CurrentNode), deref(r.FailureReason))
		}
		if _, err := env.e.DecideApproval(ctx, pendingApproval(t, env, r.ID).ID, DecisionApprove, "andre", ""); err != nil {
			t.Fatal(err)
		}
		r = env.resume(env.e, r.ID)
	}

	// The audit asks; the run waits for the answer and is not failed.
	approveManualTest()
	if r.Status != store.RunWaitingApproval || r.PausedAt == nil || r.FinishedAt != nil || r.FailureReason != nil {
		t.Fatalf("while the question is open: run = %s paused_at %v %q", r.Status, r.PausedAt, deref(r.FailureReason))
	}
	if out := recv(t, asked, "the ask_operator call"); out.askErr != nil || out.decided != nil || out.parkErr != nil {
		t.Fatalf("the call = %+v", out)
	}
	q := pendingApproval(t, env, r.ID)
	if q.Kind != store.ApprovalKindQuestion || q.NodeID != "doc_audit" || q.Message != docAuditQuestion {
		t.Fatalf("pending approval = %+v", q)
	}
	nodes := runNodes(t, env, r.ID)
	if parked := nodes[len(nodes)-1]; parked.NodeID != "doc_audit" || parked.Status != store.NodeWaitingApproval || parked.ParkedAt == nil {
		t.Fatalf("last visit = %+v", parked)
	}
	if m := sc.message("doc_audit", 1); !strings.Contains(m, "Questions:") {
		t.Fatalf("the audit's task does not offer ask_operator:\n%s", m)
	}
	if m := sc.message("builder", 2); m != "" {
		t.Fatalf("the builder ran again before the operator answered:\n%s", m)
	}
	if m := sc.message("integrator_ready", 1); m != "" {
		t.Fatalf("integrator_ready ran while the audit waited:\n%s", m)
	}

	// The answer wakes the audit, which sends the decision to the Builder; the loop runs to
	// the manual test again.
	if _, err := env.e.AnswerQuestion(ctx, q.ID, "Fix the code", "andre"); err != nil {
		t.Fatal(err)
	}
	r = env.resume(env.e, r.ID)
	if r.Status != store.RunWaitingApproval || deref(r.CurrentNode) != "manual_test" {
		t.Fatalf("after the answer: run = %s at %s %q", r.Status, deref(r.CurrentNode), deref(r.FailureReason))
	}
	if m := sc.message("doc_audit", 2); !strings.Contains(m, answeredBlock(docAuditQuestion, "Fix the code")) {
		t.Fatalf("the continuation lacks the answer:\n%s", m)
	}
	if m := sc.message("builder", 1); strings.Contains(m, "documentation audit") {
		t.Fatalf("the builder's first task mentions the audit:\n%s", m)
	}
	if m := sc.message("builder", 2); !strings.Contains(m, "The operator decided, after the documentation audit: "+docAuditNotes) {
		t.Fatalf("the builder's second task lacks the decision:\n%s", m)
	}
	if audit, _ := stateOf(t, r)["doc_audit"].(map[string]any); audit["verdict"] != "rework" || audit["notes"] != docAuditNotes {
		t.Fatalf("state.doc_audit = %v", audit)
	}

	// The reworked change is reviewed, drafted and tested again, then audited without a conflict.
	approveManualTest()
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	want := []string{"builder#1:succeeded", "code_review#1:succeeded", "integrator_draft#1:succeeded", "manual_test#1:succeeded",
		"doc_audit#1:succeeded", "doc_audit#2:succeeded",
		"builder#2:succeeded", "code_review#2:succeeded", "integrator_draft#2:succeeded", "manual_test#2:succeeded",
		"doc_audit#3:succeeded", "integrator_ready#1:succeeded"}
	if got := visitsOf(runNodes(t, env, r.ID)); !slices.Equal(got, want) {
		t.Fatalf("visits = %v", got)
	}
	st := stateOf(t, r)
	if audit, _ := st["doc_audit"].(map[string]any); audit["verdict"] != "ok" || st["integrator_ready"] == nil {
		t.Fatalf("state = %v", st)
	}
	if n := env.count(`SELECT count(*) FROM run_nodes WHERE run_id=$1 AND status='failed'`, r.ID); n != 0 || r.FailureReason != nil {
		t.Fatalf("%d failed visits, failure reason %q", n, deref(r.FailureReason))
	}
	if n := env.openSpans(r.ID); n != 0 {
		t.Fatalf("%d spans left open", n)
	}
}
