package engine

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// onEvent runs hook inside the engine's emit of every event, after the event is stored,
// on the goroutine that emits it: a decision or a crash injected there happens exactly
// between the step that published the event and the engine's next step.
func (env *testEnv) onEvent(hook func(typ string, data json.RawMessage)) {
	rec := env.e.deps.Recorder
	orig := rec.Append
	rec.Append = func(ctx context.Context, runID, typ string, data json.RawMessage) (int64, error) {
		seq, err := orig(ctx, runID, typ, data)
		hook(typ, data)
		return seq, err
	}
}

// Review finding 3: the walk hands the run back to an approval node it passed before and
// the worker stops before the node's new visit exists (current_node is the gate, no new
// visit). The recovered run must ask again, not reuse the earlier visit's approval.
func TestRecoveredLoopGateAsksAgain(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.agent(nil)
	wf := env.createWorkflow(workflow.Definition{
		Nodes: []workflow.Node{{ID: "start", Kind: workflow.KindStart},
			{ID: "gate", Kind: workflow.KindApproval, Message: "Another round?"},
			{ID: "work", Kind: workflow.KindAgent, AgentID: ag.ID, Task: "work", OutputSchema: anyObject},
			{ID: "end", Kind: workflow.KindEnd}},
		Edges: []workflow.Edge{{From: "start", To: "gate"}, {From: "gate", To: "work"},
			{From: "work", To: "gate", When: cond(t, `{"path":"/work/again","op":"eq","value":true}`)},
			{From: "work", To: "end"}},
	})
	env.script(map[string][]string{"work": {`{"again":true}`, `{"again":false}`}})
	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID})
	first := pendingApproval(t, env, r.ID)
	if _, err := env.e.DecideApproval(ctx, first.ID, DecisionApprove, "andre", ""); err != nil {
		t.Fatal(err)
	}

	// The worker that continues the run dies right after work's output sent the walk
	// back to the gate: the hand-off is recorded, the gate's second visit is not.
	wctx, crash := context.WithCancel(ctx)
	defer crash()
	env.onEvent(func(typ string, _ json.RawMessage) {
		if typ == "agent_message" {
			crash()
		}
	})
	claimed, err := store.ClaimQueuedRun(ctx, env.pool)
	if err != nil || claimed.ID != r.ID {
		t.Fatalf("claim: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.e.runRun(wctx, claimed)
	}()
	recv(t, done, "the worker to stop")
	r, _ = store.GetRun(ctx, env.pool, r.ID)
	if r.Status != store.RunRunning || deref(r.CurrentNode) != "gate" {
		t.Fatalf("after the crash: %s at %s", r.Status, deref(r.CurrentNode))
	}
	if got := visitsOf(runNodes(t, env, r.ID)); !slices.Equal(got, []string{"gate#1:succeeded", "work#1:succeeded"}) {
		t.Fatalf("visits after the crash = %v", got)
	}

	if n, _, err := store.RequeueStaleRuns(ctx, env.pool, -time.Hour); err != nil || n != 1 {
		t.Fatalf("requeue: %d %v", n, err)
	}
	r = env.resume(env.e, r.ID)
	if r.Status != store.RunWaitingApproval || deref(r.CurrentNode) != "gate" {
		t.Fatalf("recovered run = %s at %s; the gate must ask again", r.Status, deref(r.CurrentNode))
	}
	second := pendingApproval(t, env, r.ID)
	if second.ID == first.ID || second.RunNodeID != deref(r.ResumeNodeVisitID) {
		t.Fatalf("second approval = %+v, run resumes at %s", second, deref(r.ResumeNodeVisitID))
	}
	if _, err := env.e.DecideApproval(ctx, second.ID, DecisionApprove, "andre", ""); err != nil {
		t.Fatal(err)
	}
	if r = env.resume(env.e, r.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	want := []string{"gate#1:succeeded", "work#1:succeeded", "gate#2:succeeded", "work#2:succeeded"}
	if got := visitsOf(runNodes(t, env, r.ID)); !slices.Equal(got, want) {
		t.Fatalf("visits = %v", got)
	}
}

// Review finding 4: a decision made the moment approval.requested is published must
// find the run paused, so it re-queues it; the run never waits on a decided approval.
func TestGateDecidedAsSoonAsItIsPublished(t *testing.T) {
	env := newEnv(t)
	wf := gated(t, env, false, store.Limits{})
	env.script(map[string][]string{"ship": {`{"shipped":true}`}})
	var once sync.Once
	env.onEvent(func(typ string, data json.RawMessage) {
		if typ != "approval.requested" {
			return
		}
		once.Do(func() {
			var ev struct {
				Approval store.Approval `json:"approval"`
			}
			if err := json.Unmarshal(data, &ev); err != nil {
				t.Error(err)
			}
			if _, err := env.e.DecideApproval(context.Background(), ev.Approval.ID, DecisionApprove, "andre", ""); err != nil {
				t.Errorf("decide: %v", err)
			}
		})
	})
	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID})
	if r.Status != store.RunQueued || r.PausedAt != nil {
		t.Fatalf("run after the worker let it go = %s (paused_at %v); the decision must have re-queued it", r.Status, r.PausedAt)
	}
	r = env.resume(env.e, r.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if got := visitsOf(runNodes(t, env, r.ID)); !slices.Equal(got, []string{"gate#1:succeeded", "ship#1:succeeded"}) {
		t.Fatalf("visits = %v", got)
	}
	if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND ended_at IS NULL`, r.ID); n != 0 {
		t.Fatalf("%d spans left open", n)
	}
}

// Review finding 5: a tool approval decided between RequestApproval and WaitApproval.
// The wait returns the decision and the visit it paused resumes, so the run is running
// again while the call goes on.
func TestToolApprovalDecidedBeforeTheWait(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	env.e.deps.Effects = &countingEffects{}
	ag := env.agent(nil)
	type observed struct {
		paused, resumed string // run status after RequestApproval, and once the wait returned
		decided         *store.Approval
		err             error
	}
	seen := make(chan observed, 1)
	statusOf := func(runID string) string {
		r, err := store.GetRun(ctx, env.pool, runID)
		if err != nil {
			return err.Error()
		}
		return r.Status
	}
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			var o observed
			defer func() {
				seen <- o
				finishThenFinal(t, env, p, `{"summary":"deployed"}`)
			}()
			token := tokenIn(p.Message)
			rc, ok := env.e.ResolveRunToken(token)
			if !ok {
				t.Error("token not live")
				return
			}
			key := "key-" + rc.RunNodeID
			call := &store.ToolCall{RunID: rc.RunID, RunNodeID: rc.RunNodeID, ToolUseID: "mcp-" + store.NewID(), ToolName: "deploy_release",
				Input: json.RawMessage(`{"version":"1.0.0"}`), Status: store.ToolAwaitingApproval, IdempotencyKey: &key}
			if err := store.InsertToolCall(ctx, env.pool, call); err != nil {
				t.Error(err)
				return
			}
			a, err := env.e.RequestApproval(ctx, ApprovalRequest{Token: token, ToolName: "deploy_release", ToolCallID: call.ID, Input: call.Input})
			if err != nil {
				t.Errorf("RequestApproval: %v", err)
				return
			}
			o.paused = statusOf(rc.RunID)
			if _, err := env.e.DecideApproval(ctx, a.ID, DecisionApprove, "andre", ""); err != nil {
				t.Errorf("decide: %v", err)
				return
			}
			o.decided, o.err = env.e.WaitApproval(ctx, a.ID)
			deadline := time.Now().Add(3 * time.Second)
			for o.resumed = statusOf(rc.RunID); o.resumed != store.RunRunning && time.Now().Before(deadline); {
				time.Sleep(10 * time.Millisecond)
				o.resumed = statusOf(rc.RunID)
			}
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "deploy"})
	o := recv(t, seen, "the gated call")
	if o.err != nil || o.decided == nil || o.decided.Status != store.ApprovalApproved {
		t.Fatalf("wait = %+v, %v", o.decided, o.err)
	}
	if o.paused != store.RunWaitingApproval || o.resumed != store.RunRunning {
		t.Fatalf("run status: %s after the request, %s after the wait; the visit must resume", o.paused, o.resumed)
	}
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
}
