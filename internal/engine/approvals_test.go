package engine

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// countingEffects is the SideEffectExecutor the engine falls back to; it counts calls.
type countingEffects struct {
	mu  sync.Mutex
	ids []string
}

func (c *countingEffects) ExecuteApproved(_ context.Context, toolCallID string) (json.RawMessage, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, toolCallID)
	return json.RawMessage(`{"release_id":"rel-9","version":"1.0.0"}`), false, nil
}

func (c *countingEffects) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.ids)
}

// gate plays the MCP handler of a gated tool (agent B's side of DESIGN 5.3 rule 5): it
// records the tool call, requests the approval and waits for it.
type gate struct {
	approval chan *store.Approval
	decided  chan *store.Approval
	waitErr  chan error
	call     *store.ToolCall
	p        openclaw.ChatSendParams
}

func (env *testEnv) gatedCall(p openclaw.ChatSendParams, waitCtx context.Context) *gate {
	g := &gate{approval: make(chan *store.Approval, 1), decided: make(chan *store.Approval, 1), waitErr: make(chan error, 1), p: p}
	ctx := context.Background()
	token := tokenIn(p.Message)
	rc, ok := env.e.ResolveRunToken(token)
	if !ok {
		env.t.Error("token not live")
		return g
	}
	key := "key-" + rc.RunNodeID
	g.call = &store.ToolCall{RunID: rc.RunID, RunNodeID: rc.RunNodeID, ToolUseID: "mcp-" + store.NewID(), ToolName: "deploy_release",
		Input: json.RawMessage(`{"version":"1.0.0","paths":["app.py"]}`), Status: store.ToolAwaitingApproval, IdempotencyKey: &key}
	if err := store.InsertToolCall(ctx, env.pool, g.call); err != nil {
		env.t.Error(err)
		return g
	}
	a, err := env.e.RequestApproval(ctx, ApprovalRequest{Token: token, ToolName: "deploy_release", ToolCallID: g.call.ID,
		Message: "Deploy release 1.0.0 (1 file)", Input: g.call.Input})
	if err != nil {
		env.t.Errorf("RequestApproval: %v", err)
		return g
	}
	g.approval <- a
	got, err := env.e.WaitApproval(waitCtx, a.ID)
	g.waitErr <- err
	if err == nil {
		g.decided <- got
	}
	return g
}

// recv bounds a wait in the test goroutine, so a broken rule fails instead of hanging.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func runStatus(t *testing.T, env *testEnv, runID string) string {
	t.Helper()
	r, err := store.GetRun(context.Background(), env.pool, runID)
	if err != nil {
		t.Fatal(err)
	}
	return r.Status
}

// T5.3.3 / T5.3.4: decided while the MCP call waits → the decision goes to the call
// and the engine never executes the effect itself. The wait does not count toward the
// time limit (T5.2.4).
func TestToolApprovalDecidedWhileWaiting(t *testing.T) {
	tests := []struct {
		decision, status, gateway string
	}{
		{DecisionApprove, store.ApprovalApproved, "allow-once"},
		{DecisionReject, store.ApprovalRejected, "deny"},
	}
	for _, tc := range tests {
		t.Run(tc.decision, func(t *testing.T) {
			env := newEnv(t)
			ctx := context.Background()
			eff := &countingEffects{}
			env.e.deps.Effects = eff
			env.e.deps.Cfg.MCPToolWait = 540 * time.Second
			a := env.agent(func(a *store.Agent) { a.Limits.MaxDurationSeconds = 600 })
			gates := make(chan *gate, 1)
			env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
				go func() {
					wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
					defer cancel()
					g := env.gatedCall(p, wctx)
					gates <- g
					if <-g.waitErr == nil {
						finishThenFinal(t, env, p, `{"summary":"handled"}`)
					}
				}()
				return startedTurn(p)
			}
			run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "deploy"})
			eventually(t, "waiting for approval", func() bool { return runStatus(t, env, run.ID) == store.RunWaitingApproval })
			var ap *store.Approval
			eventually(t, "approval requested", func() bool {
				pending, _ := store.ListPendingApprovals(ctx, env.pool, run.ID)
				if len(pending) == 1 {
					ap = &pending[0]
				}
				return ap != nil && ap.ExternalID != nil
			})
			if ap.Kind != store.ApprovalKindTool || ap.Message != "Deploy release 1.0.0 (1 file)" || ap.NodeID != "agent" ||
				!ap.ExpiresAt.Equal(ap.RequestedAt.Add(86400*time.Second)) {
				t.Fatalf("approval = %+v", ap)
			}
			reqs := env.fc.callsFor("plugin.approval.request")
			var p openclaw.PluginApprovalRequestParams
			if len(reqs) != 1 || json.Unmarshal(reqs[0], &p) != nil {
				t.Fatalf("plugin.approval.request = %s", reqs)
			}
			sends := env.fc.sendParams()
			if !p.TwoPhase || p.TimeoutMs != 540_000 || p.ToolName != "deploy_release" || p.ToolCallID != *ap.ToolCallID ||
				p.MCPTool != (openclaw.PluginMCPTool{Server: "yuno", Tool: "deploy_release"}) || p.SessionKey != sends[0].SessionKey ||
				!slices.Equal(p.AllowedDecisions, []string{"allow-once", "deny"}) || !strings.Contains(p.Detail, `"version":"1.0.0"`) {
				t.Fatalf("plugin approval params = %+v", p)
			}
			eventually(t, "node waiting", func() bool { return runNodes(t, env, run.ID)[0].Status == store.NodeWaitingApproval })

			env.clock.Advance(2 * time.Hour) // far past the 600 s limit: a pause does not count
			time.Sleep(100 * time.Millisecond)
			if s := runStatus(t, env, run.ID); s != store.RunWaitingApproval {
				t.Fatalf("status during the wait = %s", s)
			}
			decided, err := env.e.DecideApproval(ctx, ap.ID, tc.decision, "andre", "checked")
			if err != nil || decided.Status != tc.status {
				t.Fatalf("decide: %+v %v", decided, err)
			}
			g := recv(t, gates, "the gated call")
			got := recv(t, g.decided, "the decision")
			if got.ID != ap.ID || got.Status != tc.status {
				t.Fatalf("waiter got %+v", got)
			}
			r := env.wait(done, run.ID)
			if r.Status != store.RunSucceeded || r.PausedTotalMs < 7_200_000 {
				t.Fatalf("run = %s %q paused_total=%d", r.Status, deref(r.FailureReason), r.PausedTotalMs)
			}
			if len(eff.calls()) != 0 {
				t.Fatalf("the engine executed the effect too: %v", eff.calls())
			}
			if d := env.fc.pluginDecision(*ap.ExternalID); d != tc.gateway {
				t.Fatalf("gateway decision = %q", d)
			}
			if !strings.Contains(string(r.State), `"agent"`) || strings.Contains(string(r.State), "pending_effects") {
				t.Fatalf("state = %s", r.State)
			}
			if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND kind='approval_wait' AND ended_at IS NOT NULL`, run.ID); n != 1 {
				t.Fatalf("closed approval spans = %d", n)
			}
			if n := env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='run.status' AND data->'run'->>'status'='waiting_approval'`, run.ID); n == 0 {
				t.Fatal("no run.status event for the pause")
			}
		})
	}
}

// T5.3.5: the MCP wait gives up → the turn continues with the run running; a later
// grant is executed by the engine once and recorded in state.pending_effects, as is a
// later rejection.
func TestToolApprovalAfterTheWait(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	eff := &countingEffects{}
	env.e.deps.Effects = eff
	a := env.agent(nil)
	var mu sync.Mutex
	var gs []*gate
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			for range 2 {
				wctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond) // MCP_TOOL_WAIT, shortened
				g := env.gatedCall(p, wctx)
				cancel()
				mu.Lock()
				gs = append(gs, g)
				mu.Unlock()
				if err := <-g.waitErr; !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("wait = %v", err)
				}
			}
			finishThenFinal(t, env, p, `{"summary":"deploy pending approval"}`)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "deploy"})
	r := env.wait(done, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	pending, _ := store.ListPendingApprovals(ctx, env.pool, run.ID)
	if len(pending) != 2 {
		t.Fatalf("pending approvals after the run = %d", len(pending))
	}
	grant, deny := recv(t, gs[0].approval, "approval 1"), recv(t, gs[1].approval, "approval 2")
	if _, err := env.e.DecideApproval(ctx, grant.ID, DecisionApprove, "andre", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.DecideApproval(ctx, deny.ID, DecisionReject, "andre", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.DecideApproval(ctx, grant.ID, DecisionApprove, "andre", ""); !errors.Is(err, ErrNotPending) {
		t.Fatalf("T5.3.8 second grant: %v", err)
	}
	env.e.bg.Wait()
	if got := eff.calls(); !slices.Equal(got, []string{gs[0].call.ID}) {
		t.Fatalf("executor calls = %v", got)
	}
	r, _ = store.GetRun(ctx, env.pool, run.ID)
	var st struct {
		PendingEffects []struct {
			ApprovalID string          `json:"approval_id"`
			ToolCallID string          `json:"tool_call_id"`
			Status     string          `json:"status"`
			Output     json.RawMessage `json:"output"`
		} `json:"pending_effects"`
		Agent json.RawMessage `json:"agent"`
	}
	if err := json.Unmarshal(r.State, &st); err != nil || len(st.PendingEffects) != 2 || len(st.Agent) == 0 {
		t.Fatalf("state = %s", r.State)
	}
	byApproval := map[string]int{st.PendingEffects[0].ApprovalID: 0, st.PendingEffects[1].ApprovalID: 1}
	ex, rj := st.PendingEffects[byApproval[grant.ID]], st.PendingEffects[byApproval[deny.ID]]
	if ex.Status != "executed" || ex.ToolCallID != gs[0].call.ID || !strings.Contains(string(ex.Output), `"release_id": "rel-9"`) {
		t.Fatalf("executed entry = %+v %s", ex, ex.Output)
	}
	if rj.Status != "rejected" || rj.ToolCallID != gs[1].call.ID || !strings.Contains(string(rj.Output), `"reason": "rejected by the operator"`) {
		t.Fatalf("rejected entry = %+v %s", rj, rj.Output)
	}
	rejected, _ := store.GetToolCallByUseID(ctx, env.pool, run.ID, gs[1].call.ToolUseID)
	if rejected.Status != store.ToolRejected {
		t.Fatalf("rejected tool call row = %s", rejected.Status)
	}
	if n := env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='run.status' AND data->'run'->'state' ? 'pending_effects'`, run.ID); n < 2 {
		t.Fatalf("run.status events carrying pending_effects = %d", n)
	}
}

// T5.3.7: cancelling a run that waits for an approval expires it with reason "run
// cancelled", denies it on the gateway and releases a waiting MCP call.
func TestCancelWhileWaitingForApproval(t *testing.T) {
	ctx := context.Background()
	t.Run("tool approval", func(t *testing.T) {
		env := newEnv(t)
		a := env.agent(nil)
		gates := make(chan *gate, 1)
		env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
			go func() {
				wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				gates <- env.gatedCall(p, wctx)
			}()
			return startedTurn(p)
		}
		run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "deploy"})
		eventually(t, "waiting", func() bool { return runStatus(t, env, run.ID) == store.RunWaitingApproval })
		if _, err := env.e.Cancel(ctx, run.ID, "andre"); err != nil {
			t.Fatal(err)
		}
		r := env.wait(done, run.ID)
		if r.Status != store.RunCancelled || deref(r.FailureReason) != "cancelled by andre" {
			t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
		}
		g := recv(t, gates, "the gated call")
		ap := recv(t, g.approval, "the approval")
		got := recv(t, g.decided, "the decision")
		if got.Status != store.ApprovalExpired || deref(got.Reason) != "run cancelled" {
			t.Fatalf("waiter got %s %q", got.Status, deref(got.Reason))
		}
		if d := env.fc.pluginDecision(*ap.ExternalID); d != "deny" {
			t.Fatalf("gateway decision = %q", d)
		}
	})
	t.Run("exec approval", func(t *testing.T) {
		env := newEnv(t)
		a := env.agent(nil)
		env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
			go env.fc.push("session.approval", map[string]any{"sessionKey": p.SessionKey, "phase": "pending", "approval": map[string]any{
				"id": "exec-7", "status": "pending", "presentation": map[string]any{"kind": "exec", "commandText": "make deploy"}}})
			return startedTurn(p)
		}
		run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "deploy"})
		eventually(t, "waiting", func() bool { return runStatus(t, env, run.ID) == store.RunWaitingApproval })
		if _, err := env.e.Cancel(ctx, run.ID, "andre"); err != nil {
			t.Fatal(err)
		}
		if r := env.wait(done, run.ID); r.Status != store.RunCancelled {
			t.Fatalf("run = %s", r.Status)
		}
		ap := pendingOrDecided(t, env, run.ID)
		if ap.Status != store.ApprovalExpired || deref(ap.Reason) != "run cancelled" {
			t.Fatalf("approval = %s %q", ap.Status, deref(ap.Reason))
		}
		res := env.fc.callsFor("exec.approval.resolve")
		if len(res) != 1 || !strings.Contains(string(res[0]), `"id":"exec-7"`) || !strings.Contains(string(res[0]), `"decision":"deny"`) {
			t.Fatalf("exec.approval.resolve = %s", res)
		}
	})
	t.Run("approval node", func(t *testing.T) {
		env := newEnv(t)
		wf := gated(t, env, false, store.Limits{})
		r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID})
		ap := pendingApproval(t, env, r.ID)
		got, err := env.e.Cancel(ctx, r.ID, "andre")
		if err != nil || got.Status != store.RunCancelled || deref(got.FailureReason) != "cancelled by andre" {
			t.Fatalf("cancel = %+v %v", got, err)
		}
		a, _ := store.GetApproval(ctx, env.pool, ap.ID)
		if a.Status != store.ApprovalExpired || deref(a.Reason) != "run cancelled" {
			t.Fatalf("approval = %s %q", a.Status, deref(a.Reason))
		}
		if v := visitsOf(runNodes(t, env, r.ID)); !slices.Equal(v, []string{"gate#1:failed"}) {
			t.Fatalf("visits = %v", v)
		}
		if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND ended_at IS NULL`, r.ID); n != 0 {
			t.Fatalf("%d spans left open", n)
		}
		if _, err := env.e.DecideApproval(ctx, ap.ID, DecisionApprove, "andre", ""); !errors.Is(err, ErrNotPending) {
			t.Fatalf("decision on a cancelled run's approval: %v", err)
		}
	})
}

func pendingOrDecided(t *testing.T, env *testEnv, runID string) *store.Approval {
	t.Helper()
	all, err := store.ListApprovals(context.Background(), env.pool, "", runID, 10)
	if err != nil || len(all) != 1 {
		t.Fatalf("approvals = %+v %v", all, err)
	}
	return &all[0]
}

// T5.3.8: decisions on decided approvals are ErrNotPending (409); unknown ids are
// store.ErrNotFound (404); unknown decisions are refused.
func TestDecisionErrors(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	if _, err := env.e.DecideApproval(ctx, "0199f0a2-0000-7000-8000-00000000dead", DecisionApprove, "andre", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown approval: %v", err)
	}
	wf := gated(t, env, false, store.Limits{})
	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID})
	ap := pendingApproval(t, env, r.ID)
	var ve *ValidationError
	if _, err := env.e.DecideApproval(ctx, ap.ID, "maybe", "andre", ""); !errors.As(err, &ve) {
		t.Fatalf("unknown decision: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = env.e.DecideApproval(ctx, ap.ID, DecisionApprove, "andre", "")
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrNotPending):
			t.Fatalf("concurrent decision: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d concurrent decisions applied", ok)
	}
	if _, err := env.e.RequestApproval(ctx, ApprovalRequest{Token: "r_unknownunknownunknownunk", ToolName: "deploy_release"}); !errors.Is(err, ErrInvalidRunToken) {
		t.Fatalf("unknown token: %v", err)
	}
}

// T5.3.9: the gateway's own timeout (resolvedBy null) is no decision: the approval
// stays pending and a later web decision applies, skipping the settled gateway copy. A
// decision on another approval client is a decision by "openclaw".
func TestGatewayApprovalEvents(t *testing.T) {
	tests := []struct {
		name       string
		gateway    func(env *testEnv, ext string)
		webDecides bool
		status     string
		decidedBy  string
	}{
		{name: "gateway timeout is ignored", gateway: func(env *testEnv, ext string) { env.fc.gatewayResolves(ext, "deny", nil) },
			webDecides: true, status: store.ApprovalApproved, decidedBy: "andre"},
		{name: "decision in another client", gateway: func(env *testEnv, ext string) { env.fc.gatewayResolves(ext, "allow-once", "telegram:42") },
			status: store.ApprovalApproved, decidedBy: "openclaw"},
		{name: "denial in another client", gateway: func(env *testEnv, ext string) { env.fc.gatewayResolves(ext, "deny", "control-ui") },
			status: store.ApprovalRejected, decidedBy: "openclaw"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			go env.e.listenGateway(ctx)
			a := env.agent(nil)
			gates := make(chan *gate, 1)
			env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
				go func() {
					wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
					defer cancel()
					g := env.gatedCall(p, wctx)
					gates <- g
					if <-g.waitErr == nil {
						finishThenFinal(t, env, p, `{"summary":"done"}`)
					}
				}()
				return startedTurn(p)
			}
			run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "deploy"})
			eventually(t, "waiting", func() bool { return runStatus(t, env, run.ID) == store.RunWaitingApproval })
			var ap *store.Approval
			eventually(t, "registered", func() bool {
				p, _ := store.ListPendingApprovals(ctx, env.pool, run.ID)
				if len(p) == 1 && p[0].ExternalID != nil {
					ap = &p[0]
				}
				return ap != nil
			})
			tc.gateway(env, *ap.ExternalID)
			if tc.webDecides {
				time.Sleep(150 * time.Millisecond)
				if got, _ := store.GetApproval(ctx, env.pool, ap.ID); got.Status != store.ApprovalPending {
					t.Fatalf("the gateway's timeout decided the approval: %s", got.Status)
				}
				if _, err := env.e.DecideApproval(ctx, ap.ID, DecisionApprove, "andre", ""); err != nil {
					t.Fatalf("web decision after the gateway window: %v", err)
				}
			}
			g := recv(t, gates, "the gated call")
			got := recv(t, g.decided, "the decision")
			if got.Status != tc.status || deref(got.DecidedBy) != tc.decidedBy {
				t.Fatalf("decision = %s by %s", got.Status, deref(got.DecidedBy))
			}
			if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
				t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
			}
			// Our own resolve echoes back as an event; nothing changes.
			time.Sleep(50 * time.Millisecond)
			if final, _ := store.GetApproval(ctx, env.pool, ap.ID); final.Status != tc.status || deref(final.DecidedBy) != tc.decidedBy {
				t.Fatalf("approval changed after the echo: %+v", final)
			}
		})
	}
}

// Exec approvals decided on the platform: allow-always resolves on the gateway, adds the
// command to the agent's allowlist, and the turn resumes when the terminal event comes.
func TestExecApprovalDecidedOnThePlatform(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	var mu sync.Mutex
	var sent openclaw.ChatSendParams
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		mu.Lock()
		sent = p
		mu.Unlock()
		go env.fc.push("session.approval", map[string]any{"sessionKey": p.SessionKey, "phase": "pending", "approval": map[string]any{
			"id": "exec-1", "status": "pending", "presentation": map[string]any{"kind": "exec", "commandText": "make deploy"}}})
		return startedTurn(p)
	}
	env.fc.onExecResolve = func(id, decision string) {
		mu.Lock()
		p := sent
		mu.Unlock()
		env.fc.push("session.approval", map[string]any{"sessionKey": p.SessionKey, "phase": "terminal", "approval": map[string]any{
			"id": id, "status": "allowed", "decision": decision}})
		finishThenFinal(t, env, p, `{"summary":"deployed"}`)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "deploy"})
	eventually(t, "waiting", func() bool { return runStatus(t, env, run.ID) == store.RunWaitingApproval })
	ap := pendingApproval(t, env, run.ID)
	decided, err := env.e.DecideApproval(ctx, ap.ID, DecisionApproveAlways, "andre", "")
	if err != nil || decided.Status != store.ApprovalApproved || deref(decided.DecidedBy) != "andre" {
		t.Fatalf("decide = %+v %v", decided, err)
	}
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	res := env.fc.callsFor("exec.approval.resolve")
	if len(res) != 1 || !strings.Contains(string(res[0]), `"decision":"allow-always"`) {
		t.Fatalf("exec.approval.resolve = %s", res)
	}
	got, _ := store.GetAgent(ctx, env.pool, a.ID)
	if !slices.Equal(got.ExecPolicy.Allowlist, []string{"make deploy"}) {
		t.Fatalf("allowlist = %v", got.ExecPolicy.Allowlist)
	}
	if final := pendingOrDecided(t, env, run.ID); deref(final.DecidedBy) != "andre" {
		t.Fatalf("the terminal event overwrote the platform decision: %+v", final)
	}
}

// T5.2.9 for a tool approval: the waiting MCP call gets the expired row (the handler
// answers the model "rejected") and the run fails with reason "approval expired".
func TestToolApprovalExpiry(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	eff := &countingEffects{}
	env.e.deps.Effects = eff
	a := env.agent(func(a *store.Agent) { a.Limits.ApprovalTimeoutSeconds = 60 })
	gates := make(chan *gate, 1)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			gates <- env.gatedCall(p, wctx)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "deploy"})
	eventually(t, "waiting", func() bool { return runStatus(t, env, run.ID) == store.RunWaitingApproval })
	env.clock.Advance(time.Minute)
	env.e.sweepApprovals(ctx)
	r := env.wait(done, run.ID)
	if r.Status != store.RunFailed || deref(r.FailureReason) != "approval expired" {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	g := recv(t, gates, "the gated call")
	got := recv(t, g.decided, "the decision")
	if got.Status != store.ApprovalExpired || deref(got.Reason) != "approval expired" {
		t.Fatalf("waiter got %s %q", got.Status, deref(got.Reason))
	}
	if len(eff.calls()) != 0 {
		t.Fatal("an expired approval executed the effect")
	}
}

// Platform tool items (yuno__*) are recorded by the MCP handler: the item recorder
// keeps their span but writes no row and counts nothing (common rules, wave 2).
func TestPlatformToolItemsAreNotRecorded(t *testing.T) {
	env := newEnv(t)
	a := env.agent(nil)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			k, r := p.SessionKey, p.IdempotencyKey
			env.fc.push("session.message", assistantMsg(k, r, []any{
				toolCallPart("call_dep", "yuno__deploy_release", map[string]any{"version": "1.0.0"}),
				toolCallPart("call_ls", "ls", map[string]any{"path": "."}),
			}, "toolUse", 10, 5, 0))
			env.fc.push("agent", itemEvent(k, r, "call_dep", "yuno__deploy_release", "start", "running", ""))
			env.fc.push("agent", itemEvent(k, r, "call_ls", "ls", "start", "running", ""))
			env.fc.push("agent", itemEvent(k, r, "call_dep", "yuno__deploy_release", "end", "completed", ""))
			env.fc.push("agent", itemEvent(k, r, "call_ls", "ls", "end", "completed", ""))
			env.fc.addHistory(k, toolResultEntry(r, "call_dep", "yuno__deploy_release", `{"release_id":"r1"}`, false))
			finishThenFinal(t, env, p, `{"summary":"done"}`)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "deploy"})
	r := env.wait(done, run.ID)
	if r.Status != store.RunSucceeded || r.ToolCallCount != 1 {
		t.Fatalf("run = %s tool calls %d", r.Status, r.ToolCallCount)
	}
	if n := env.count(`SELECT count(*) FROM tool_calls WHERE run_id=$1 AND tool_name LIKE 'yuno__%'`, run.ID); n != 0 {
		t.Fatalf("%d platform tool rows written by the engine", n)
	}
	if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND kind='tool_call' AND name='yuno__deploy_release' AND ended_at IS NOT NULL`, run.ID); n != 1 {
		t.Fatalf("platform tool spans = %d", n)
	}
}

// A node that delivers its result while its MCP call still waits leaves the approval
// pending; a later grant is executed by the engine, not handed to the dead call.
func TestToolApprovalOutlivesItsVisit(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	eff := &countingEffects{}
	env.e.deps.Effects = eff
	a := env.agent(nil)
	waitCtx, stopWaiting := context.WithCancel(ctx)
	defer stopWaiting()
	gates := make(chan *gate, 1)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() { gates <- env.gatedCall(p, waitCtx) }()
		go func() {
			eventually(t, "approval requested", func() bool {
				return env.count(`SELECT count(*) FROM approvals WHERE status='pending'`) == 1
			})
			finishThenFinal(t, env, p, `{"summary":"release requested"}`)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "deploy"})
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded || r.PausedAt != nil {
		t.Fatalf("run = %s %q paused_at=%v", r.Status, deref(r.FailureReason), r.PausedAt)
	}
	ap := pendingApproval(t, env, run.ID)
	if _, err := env.e.DecideApproval(ctx, ap.ID, DecisionApprove, "andre", ""); err != nil {
		t.Fatal(err)
	}
	env.e.bg.Wait()
	if got := eff.calls(); len(got) != 1 {
		t.Fatalf("executor calls = %v", got)
	}
	stopWaiting()
	g := recv(t, gates, "the gated call")
	if err := recv(t, g.waitErr, "the wait"); !errors.Is(err, context.Canceled) {
		t.Fatalf("the ended call got the decision: %v", err)
	}
}

// The api restarts while a turn waits for a tool approval: the decision, made on the new
// process with nobody waiting, executes the effect and re-queues the run, whose next
// worker stops the lost turn and visits the node again.
func TestDecisionAfterRestartRequeues(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	eff := &countingEffects{}
	env.e.deps.Effects = eff
	a := env.agent(nil)
	waitCtx, stopWaiting := context.WithCancel(ctx)
	defer stopWaiting()
	callDone := make(chan struct{})
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			defer close(callDone)
			env.gatedCall(p, waitCtx)
		}()
		return startedTurn(p)
	}
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, _ := store.ClaimQueuedRun(ctx, env.pool)
	wctx, shutdown := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.e.runRun(wctx, claimed)
	}()
	eventually(t, "waiting", func() bool { return runStatus(t, env, run.ID) == store.RunWaitingApproval })
	ap := pendingApproval(t, env, run.ID)
	shutdown()
	recv(t, done, "the worker to stop")
	// The MCP call the old process served dies with it: end its wait and join it before
	// the new engine replaces the one it used.
	stopWaiting()
	recv(t, callDone, "the gated call to end")

	e2 := New(env.e.deps)
	e2.tickInterval = env.e.tickInterval
	env.e = e2
	if _, err := e2.DecideApproval(ctx, ap.ID, DecisionApprove, "andre", ""); err != nil {
		t.Fatal(err)
	}
	e2.bg.Wait()
	if got := eff.calls(); len(got) != 1 {
		t.Fatalf("executor calls = %v", got)
	}
	if s := runStatus(t, env, run.ID); s != store.RunQueued {
		t.Fatalf("status after the decision = %s", s)
	}
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go finishThenFinal(t, env, p, `{"summary":"deployed earlier"}`)
		return startedTurn(p)
	}
	r := env.resume(e2, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if got := visitsOf(runNodes(t, env, run.ID)); !slices.Equal(got, []string{"agent#1:failed", "agent#2:succeeded"}) {
		t.Fatalf("visits = %v", got)
	}
}
