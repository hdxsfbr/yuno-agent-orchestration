package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"yuno/internal/obs"
	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// Approvals (DESIGN 5.3): one table, three kinds. Exec approvals come from the gateway
// and are mirrored by the visit (node.go); tool approvals gate platform side effects
// for the MCP handler; node approvals are workflow approval nodes (walk.go). This file
// holds what the HTTP API, the MCP handler, the sweeper and the gateway event listener
// call.

const (
	// gatewayRPCTimeout bounds one approval RPC to the gateway.
	gatewayRPCTimeout = 5 * time.Second
	// effectTimeout bounds the engine's own execution of an approved side effect.
	effectTimeout = 2 * time.Minute
	// maxApprovalDetail caps the tool input shown by OpenClaw's approval surfaces.
	maxApprovalDetail = 4000
	// defaultRejectReason is what the model is told when a rejection gives no reason;
	// the MCP handler uses the same text.
	defaultRejectReason = "rejected by the operator"
)

func secondsDuration(n int) time.Duration { return time.Duration(n) * time.Second }

// approvalWaiter is an MCP call waiting for a tool approval. Its presence in
// Engine.waiters means a decision goes to it, and only to it: the engine executes the
// effect itself only once nobody waits (DESIGN 5.3 rule 5).
type approvalWaiter struct {
	ch chan *store.Approval // buffered 1; written once, under waitMu, as the entry is removed
	v  *visit               // the visit whose turn the call blocks; nil after a restart
}

// deliver hands a decided approval to its waiter, if one is attached.
func (e *Engine) deliver(a *store.Approval) bool {
	e.waitMu.Lock()
	defer e.waitMu.Unlock()
	w := e.waiters[a.ID]
	if w == nil {
		return false
	}
	delete(e.waiters, a.ID)
	w.ch <- a
	return true
}

// detachWaiters stops delivering decisions to the MCP calls of a visit that ended.
func (e *Engine) detachWaiters(v *visit) {
	e.waitMu.Lock()
	defer e.waitMu.Unlock()
	for id, w := range e.waiters {
		if w.v == v {
			delete(e.waiters, id)
		}
	}
}

// unblock tells the waiter's visit that its MCP call no longer waits.
func (w *approvalWaiter) unblock(a *store.Approval, id string) {
	if w.v == nil {
		return
	}
	w.v.post(func(ctx context.Context) {
		delete(w.v.toolBlocking, id)
		if a != nil {
			w.v.approvalDecided(ctx, a)
			return
		}
		w.v.resumeIfClear(ctx)
	})
}

// RequestApproval records a pending tool approval for the token's visit, registers it
// with the gateway as a two-phase plugin approval, pauses the run and the node and
// publishes approval.requested (DESIGN 5.3 rules 5 and 5a). The caller is expected to
// WaitApproval next: decisions go to it from this moment on.
func (e *Engine) RequestApproval(ctx context.Context, req ApprovalRequest) (*store.Approval, error) {
	t, ok := e.tokens.get(req.Token)
	if !ok {
		return nil, ErrInvalidRunToken
	}
	v := t.v
	now := e.clock.Now()
	timeout := v.limits.ApprovalTimeoutSeconds
	if timeout <= 0 {
		timeout = store.DefaultLimits.ApprovalTimeoutSeconds
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		msg = req.ToolName + " requested by " + v.agent.Name
	}
	a := &store.Approval{ID: store.NewID(), RunID: v.run.ID, RunNodeID: v.rn.ID, Kind: store.ApprovalKindTool,
		NodeID: v.node.ID, Message: obs.Redact(msg), Status: store.ApprovalPending, RequestedAt: now,
		ExpiresAt: now.Add(secondsDuration(timeout))}
	if req.ToolCallID != "" {
		id := req.ToolCallID
		a.ToolCallID = &id
	}
	e.waitMu.Lock()
	e.waiters[a.ID] = &approvalWaiter{ch: make(chan *store.Approval, 1), v: v}
	e.waitMu.Unlock()
	drop := func() {
		e.waitMu.Lock()
		delete(e.waiters, a.ID)
		e.waitMu.Unlock()
	}
	if err := store.InsertApproval(ctx, e.deps.Pool, a); err != nil {
		drop()
		return nil, fmt.Errorf("record approval: %w", err)
	}
	if ext := e.registerPluginApproval(ctx, v, a, req); ext != "" {
		a.ExternalID = &ext
		if err := store.SetApprovalExternalID(ctx, e.deps.Pool, a.ID, ext); err != nil {
			v.log.Warn("store gateway approval id failed", "approval_id", a.ID, "error", err)
		}
	}
	err := v.do(ctx, func() {
		wctx := context.WithoutCancel(ctx)
		span := e.startSpan(wctx, a.RunID, &v.nodeSpan, "approval_wait", "tool approval "+req.ToolName, now, nil,
			map[string]any{"approval_id": a.ID, "kind": store.ApprovalKindTool, "tool": req.ToolName, "tool_call_id": req.ToolCallID})
		e.waitMu.Lock()
		_, waiting := e.waiters[a.ID] // not decided in the moments since the insert
		e.waitMu.Unlock()
		if waiting {
			v.toolBlocking[a.ID] = true
			v.pause(wctx)
		} else {
			e.endSpan(wctx, span, e.clock.Now(), map[string]any{"status": "decided"})
		}
		e.emit(wctx, a.RunID, "approval.requested", map[string]any{"approval": a})
	})
	if err != nil {
		// The visit ended between the token check and now: nothing can use the approval.
		drop()
		e.expireApproval(context.WithoutCancel(ctx), a, "run ended")
		return nil, err
	}
	e.logEvent(ctx, a.RunID, "info", fmt.Sprintf("%s waits for approval", req.ToolName), "node_id", a.NodeID, "approval_id", a.ID)
	return a, nil
}

// registerPluginApproval announces a tool approval to the gateway's approval clients
// (DESIGN 5.3 rule 5a) and returns its gateway id, or "" when the gateway could not
// take it: the approval is still decidable on the platform.
func (e *Engine) registerPluginApproval(ctx context.Context, v *visit, a *store.Approval, req ApprovalRequest) string {
	gw := e.deps.Gateway
	if gw == nil {
		return ""
	}
	timeoutMs := e.deps.Cfg.MCPToolWait.Milliseconds()
	if timeoutMs <= 0 || timeoutMs > openclaw.MaxPluginApprovalTimeoutMs {
		timeoutMs = openclaw.MaxPluginApprovalTimeoutMs
	}
	detail := ""
	if len(req.Input) > 0 {
		detail = string(obs.RedactJSON(req.Input))
		if len(detail) > maxApprovalDetail {
			detail = strings.ToValidUTF8(detail[:maxApprovalDetail], "") + "…"
		}
	}
	cctx, cancel := context.WithTimeout(ctx, gatewayRPCTimeout)
	defer cancel()
	res, err := openclaw.PluginApprovalRequest(cctx, gw, openclaw.PluginApprovalRequestParams{
		Title:            fmt.Sprintf("Approve %s for %s?", req.ToolName, v.agent.Name),
		Description:      a.Message,
		Detail:           detail,
		Severity:         "warning",
		ToolName:         req.ToolName,
		ToolCallID:       req.ToolCallID,
		MCPTool:          openclaw.PluginMCPTool{Server: "yuno", Tool: req.ToolName},
		AllowedDecisions: []string{"allow-once", "deny"},
		AgentID:          v.ocID,
		SessionKey:       v.key,
		TimeoutMs:        timeoutMs,
		TwoPhase:         true,
	})
	if err != nil || res.ID == "" {
		v.log.Warn("plugin approval not registered with the gateway; it can still be decided on the platform",
			"approval_id", a.ID, "error", err)
		return ""
	}
	return res.ID
}

// WaitApproval blocks until the approval is decided or ctx ends. A decision made
// while it waits is the caller's to act on; once it returns ctx.Err() the engine acts
// on a later grant itself (DESIGN 5.3 rule 5).
func (e *Engine) WaitApproval(ctx context.Context, approvalID string) (*store.Approval, error) {
	e.waitMu.Lock()
	w := e.waiters[approvalID]
	e.waitMu.Unlock()
	if w == nil {
		// Not requested by this process (or already delivered): the row decides.
		a, err := store.GetApproval(ctx, e.deps.Pool, approvalID)
		if err != nil {
			return nil, fmt.Errorf("approval %s: %w", approvalID, err)
		}
		if a.Status != store.ApprovalPending {
			// Decided between RequestApproval and this wait: the decision went to a waiter
			// entry nobody reads, so the visit the call blocked is released here, as the
			// waiter path does; otherwise the run stays waiting while the call proceeds.
			if v := e.visitOf(a); v != nil {
				(&approvalWaiter{v: v}).unblock(a, approvalID)
			}
			return a, nil
		}
		e.waitMu.Lock()
		if w = e.waiters[approvalID]; w == nil {
			w = &approvalWaiter{ch: make(chan *store.Approval, 1)}
			e.waiters[approvalID] = w
		}
		e.waitMu.Unlock()
	}
	select {
	case a := <-w.ch:
		w.unblock(a, approvalID)
		return a, nil
	case <-ctx.Done():
	}
	e.waitMu.Lock()
	select {
	case a := <-w.ch: // decided while the wait was ending: still ours
		e.waitMu.Unlock()
		w.unblock(a, approvalID)
		return a, nil
	default:
	}
	if e.waiters[approvalID] == w {
		delete(e.waiters, approvalID)
	}
	e.waitMu.Unlock()
	w.unblock(nil, approvalID)
	return nil, ctx.Err()
}

// DecideApproval applies a decision by `by` (DESIGN 5.3 rules 3, 5, 8, 9).
func (e *Engine) DecideApproval(ctx context.Context, approvalID, decision, by, reason string) (*store.Approval, error) {
	var status string
	switch decision {
	case DecisionApprove, DecisionApproveAlways:
		status = store.ApprovalApproved
	case DecisionReject:
		status = store.ApprovalRejected
	default:
		return nil, &ValidationError{Messages: []string{"decision: must be approve, approve_always or reject"}}
	}
	if strings.TrimSpace(by) == "" {
		by = "user"
	}
	reason = obs.Redact(reason)
	a, err := store.GetApproval(ctx, e.deps.Pool, approvalID)
	if err != nil {
		return nil, fmt.Errorf("approval %s: %w", approvalID, err)
	}
	if a.Status != store.ApprovalPending {
		return nil, ErrNotPending
	}
	switch a.Kind {
	case store.ApprovalKindExec:
		return e.decideExec(ctx, a, decision, status, by, reason)
	case store.ApprovalKindTool:
		return e.decideTool(ctx, a, status, by, reason, true)
	case store.ApprovalKindNode:
		return e.decideNode(ctx, a, status, by, reason)
	}
	return nil, fmt.Errorf("approval %s: unknown kind %q", approvalID, a.Kind)
}

// decideExec records an exec approval decision and resolves it on the gateway, where
// the turn waits for it; the visit also sees the gateway's terminal event, which finds
// the row decided. The row is decided first so that event cannot overtake the human's
// decision; a gateway that cannot be reached reopens it.
func (e *Engine) decideExec(ctx context.Context, a *store.Approval, decision, status, by, reason string) (*store.Approval, error) {
	ocDecision := map[string]string{DecisionApprove: "allow-once", DecisionApproveAlways: "allow-always", DecisionReject: "deny"}[decision]
	decided, err := e.decideRow(ctx, a, status, by, reason)
	if err != nil {
		return nil, err
	}
	if gw := e.deps.Gateway; gw != nil && a.ExternalID != nil {
		cctx, cancel := context.WithTimeout(ctx, gatewayRPCTimeout)
		err := openclaw.ExecApprovalResolve(cctx, gw, *a.ExternalID, ocDecision)
		cancel()
		var rpcErr *openclaw.RPCError
		switch {
		case err == nil:
		case errors.As(err, &rpcErr):
			// The gateway answered but no longer holds the approval (decided in chat,
			// expired, or restarted): its side is settled, so the record still is.
			e.log.Warn("exec approval not resolvable on the gateway", "run_id", a.RunID, "approval_id", a.ID, "error", err)
		default:
			if rerr := store.ReopenApproval(context.WithoutCancel(ctx), e.deps.Pool, a.ID, status); rerr != nil {
				e.log.Warn("reopen approval failed", "approval_id", a.ID, "error", rerr)
			}
			return nil, fmt.Errorf("resolve exec approval on the gateway: %w", err)
		}
	}
	e.announceDecision(ctx, decided)
	if decision == DecisionApproveAlways {
		e.extendExecAllowlist(ctx, decided)
	}
	if v := e.visitOf(decided); v != nil {
		v.post(func(ctx context.Context) { v.approvalDecided(ctx, decided) })
	} else {
		e.requeueIfUnattended(ctx, decided.RunID)
	}
	return decided, nil
}

// extendExecAllowlist adds an always-allowed command to the agent's stored exec
// allowlist so the agent form shows the effective policy (DESIGN 5.3 rule 3).
func (e *Engine) extendExecAllowlist(ctx context.Context, a *store.Approval) {
	rn, err := store.GetRunNode(ctx, e.deps.Pool, a.RunNodeID)
	if err != nil || rn.AgentID == nil || strings.TrimSpace(a.Message) == "" {
		return
	}
	if err := store.AddExecAllowlistPattern(ctx, e.deps.Pool, *rn.AgentID, strings.TrimSpace(a.Message)); err != nil {
		e.log.Warn("extend exec allowlist failed", "agent_id", *rn.AgentID, "error", err)
	}
}

// decideTool records a tool approval decision, settles the gateway's copy and hands
// the decision to the waiting MCP call, or, when none waits, acts on it itself.
func (e *Engine) decideTool(ctx context.Context, a *store.Approval, status, by, reason string, resolveOnGateway bool) (*store.Approval, error) {
	decided, err := e.recordDecision(ctx, a, status, by, reason)
	if err != nil {
		return nil, err
	}
	if resolveOnGateway {
		decision := "deny"
		if status == store.ApprovalApproved {
			decision = "allow-once"
		}
		e.resolvePlugin(ctx, decided, decision)
	}
	if !e.deliver(decided) {
		e.settleUnwaited(ctx, decided)
		e.requeueIfUnattended(ctx, decided.RunID)
	}
	return decided, nil
}

// decideNode records an approval node decision and re-queues the run, which a worker
// continues along the node's first true edge or its on_reject edge (DESIGN 5.6 rule 8).
func (e *Engine) decideNode(ctx context.Context, a *store.Approval, status, by, reason string) (*store.Approval, error) {
	decided, err := e.recordDecision(ctx, a, status, by, reason)
	if err != nil {
		return nil, err
	}
	now := e.clock.Now()
	nodeStatus, nodeReason := store.NodeSucceeded, (*string)(nil)
	if status != store.ApprovalApproved {
		r := "rejected"
		if reason != "" {
			r += ": " + reason
		}
		nodeStatus, nodeReason = store.NodeFailed, &r
	}
	if rn, err := store.FinishRunNode(ctx, e.deps.Pool, a.RunNodeID, nodeStatus, nodeReason, now); err == nil {
		e.emit(ctx, a.RunID, "node.status", map[string]any{"node": rn})
	}
	if run, err := store.RequeuePausedRunAt(ctx, e.deps.Pool, a.RunID, now); err == nil {
		e.emit(ctx, a.RunID, "run.status", map[string]any{"run": run})
	} else if !errors.Is(err, store.ErrNotFound) {
		e.log.Warn("re-queue after approval failed", "run_id", a.RunID, "error", err)
	}
	return decided, nil
}

// recordDecision moves a pending approval to its decision, closes its span and
// publishes approval.decided. A row decided meanwhile is ErrNotPending.
func (e *Engine) recordDecision(ctx context.Context, a *store.Approval, status, by, reason string) (*store.Approval, error) {
	decided, err := e.decideRow(ctx, a, status, by, reason)
	if err != nil {
		return nil, err
	}
	e.announceDecision(ctx, decided)
	return decided, nil
}

func (e *Engine) decideRow(ctx context.Context, a *store.Approval, status, by, reason string) (*store.Approval, error) {
	decided, err := store.DecideApprovalAt(ctx, e.deps.Pool, a.ID, status, by, reason, e.clock.Now())
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotPending
	}
	if err != nil {
		return nil, fmt.Errorf("decide approval %s: %w", a.ID, err)
	}
	return decided, nil
}

func (e *Engine) announceDecision(ctx context.Context, a *store.Approval) {
	at := e.clock.Now()
	if a.DecidedAt != nil {
		at = *a.DecidedAt
	}
	if err := store.EndApprovalSpan(ctx, e.deps.Pool, a.RunID, a.ID, at, mustMarshal(map[string]any{
		"status": a.Status, "decided_by": strValue(a.DecidedBy), "reason": strValue(a.Reason)})); err != nil {
		e.log.Warn("end approval span failed", "approval_id", a.ID, "error", err)
	}
	e.emit(ctx, a.RunID, "approval.decided", map[string]any{"approval": a})
	e.logEvent(ctx, a.RunID, "info", fmt.Sprintf("%s approval %s by %s", a.Kind, a.Status, strValue(a.DecidedBy)),
		"node_id", a.NodeID, "approval_id", a.ID)
}

// resolvePlugin settles the gateway's copy of a tool approval. "Already resolved" and
// "not found" mean the gateway's side is settled anyway (DESIGN 5.3 rule 5a).
func (e *Engine) resolvePlugin(ctx context.Context, a *store.Approval, decision string) {
	gw := e.deps.Gateway
	if gw == nil || a.ExternalID == nil || *a.ExternalID == "" {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gatewayRPCTimeout)
	defer cancel()
	if err := openclaw.PluginApprovalResolve(cctx, gw, *a.ExternalID, decision); err != nil && !openclaw.IsSettledApprovalError(err) {
		e.log.Warn("resolve plugin approval on the gateway failed", "run_id", a.RunID, "approval_id", a.ID, "error", err)
	}
}

// settleUnwaited acts on a tool approval decided after its MCP call stopped waiting: a
// grant executes the effect under the call's idempotency key, a rejection or an expiry
// is recorded, and either way the outcome is appended to state.pending_effects (DESIGN
// 5.3 rule 5). The effect runs in the background so the decision returns at once.
func (e *Engine) settleUnwaited(ctx context.Context, a *store.Approval) {
	if a.ToolCallID == nil || *a.ToolCallID == "" {
		return
	}
	if a.Status == store.ApprovalApproved && e.deps.Effects == nil {
		e.logEvent(ctx, a.RunID, "warn", "approved side effect not executed: no executor is configured", "approval_id", a.ID)
		return
	}
	e.bg.Add(1)
	go func() {
		defer e.bg.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), effectTimeout)
		defer cancel()
		toolCallID := *a.ToolCallID
		var status string
		var output json.RawMessage
		switch a.Status {
		case store.ApprovalApproved:
			out, isError, err := e.deps.Effects.ExecuteApproved(ctx, toolCallID)
			status, output = "executed", out
			switch {
			case err != nil:
				status, output = "failed", mustMarshal(map[string]string{"error": obs.Redact(err.Error())})
			case isError:
				status = "failed"
			}
		default:
			reason := strValue(a.Reason)
			if reason == "" {
				reason = defaultRejectReason
			}
			status = a.Status // rejected | expired
			output = mustMarshal(map[string]string{"status": store.ToolRejected, "reason": reason})
			rowStatus := store.ToolRejected
			if a.Status == store.ApprovalExpired {
				rowStatus = store.ToolExpired
			}
			if err := store.SetToolCallOutcome(ctx, e.deps.Pool, toolCallID, rowStatus, output, e.clock.Now()); err != nil {
				e.log.Warn("record tool call outcome failed", "tool_call_id", toolCallID, "error", err)
			}
		}
		if len(output) == 0 {
			output = json.RawMessage(`null`)
		}
		entry := mustMarshal(map[string]any{"approval_id": a.ID, "tool_call_id": toolCallID, "status": status, "output": output})
		run, err := store.AppendPendingEffect(ctx, e.deps.Pool, a.RunID, entry)
		if err != nil {
			e.log.Warn("record pending effect failed", "run_id", a.RunID, "approval_id", a.ID, "error", err)
			return
		}
		e.emit(ctx, a.RunID, "run.status", map[string]any{"run": run})
		e.logEvent(ctx, a.RunID, "info", fmt.Sprintf("side effect settled after approval: %s", status), "approval_id", a.ID, "tool_call_id", toolCallID)
	}()
}

// visitOf returns the live visit that raised an approval, if any.
func (e *Engine) visitOf(a *store.Approval) *visit {
	e.mu.Lock()
	defer e.mu.Unlock()
	if v := e.active[a.RunID]; v != nil && v.rn.ID == a.RunNodeID {
		return v
	}
	return nil
}

// requeueIfUnattended re-queues a run left waiting for approvals when nothing is
// pending any more and no worker of this process executes it (the api restarted while
// a turn waited): a worker then starts a fresh visit of its current node.
func (e *Engine) requeueIfUnattended(ctx context.Context, runID string) {
	e.mu.Lock()
	live := e.active[runID] != nil
	e.mu.Unlock()
	if live {
		return
	}
	run, err := store.GetRun(ctx, e.deps.Pool, runID)
	if err != nil || run.Status != store.RunWaitingApproval {
		return
	}
	if pending, err := store.ListPendingApprovals(ctx, e.deps.Pool, runID); err != nil || len(pending) > 0 {
		return
	}
	if run, err = store.RequeuePausedRunAt(ctx, e.deps.Pool, runID, e.clock.Now()); err == nil {
		e.emit(ctx, runID, "run.status", map[string]any{"run": run})
		e.logEvent(ctx, runID, "warn", "no worker was executing this run; re-queued")
	}
}

// expireApproval marks a pending approval expired, denies it on the gateway, hands it
// to a waiting MCP call (which answers the model "rejected") or records the outcome.
func (e *Engine) expireApproval(ctx context.Context, a *store.Approval, reason string) (*store.Approval, bool) {
	decided, err := e.recordDecision(ctx, a, store.ApprovalExpired, "platform", reason)
	if err != nil {
		return nil, false
	}
	switch a.Kind {
	case store.ApprovalKindExec:
		if gw := e.deps.Gateway; gw != nil && a.ExternalID != nil {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gatewayRPCTimeout)
			if err := openclaw.ExecApprovalResolve(cctx, gw, *a.ExternalID, "deny"); err != nil {
				e.log.Warn("deny exec approval on the gateway failed", "approval_id", a.ID, "error", err)
			}
			cancel()
		}
	case store.ApprovalKindTool:
		e.resolvePlugin(ctx, decided, "deny")
		if !e.deliver(decided) {
			e.settleUnwaited(ctx, decided)
		}
	case store.ApprovalKindNode:
		r := reason
		if rn, err := store.FinishRunNode(ctx, e.deps.Pool, a.RunNodeID, store.NodeFailed, &r, e.clock.Now()); err == nil {
			e.emit(ctx, a.RunID, "node.status", map[string]any{"node": rn})
		}
	}
	return decided, true
}

// expireRunApprovals expires every approval a finished run left pending.
func (e *Engine) expireRunApprovals(ctx context.Context, runID, reason string) {
	pending, err := store.ListPendingApprovals(ctx, e.deps.Pool, runID)
	if err != nil {
		e.log.Warn("list pending approvals failed", "run_id", runID, "error", err)
		return
	}
	for i := range pending {
		e.expireApproval(ctx, &pending[i], reason)
	}
}

// sweepApprovals expires approvals past their expires_at and fails their runs with
// reason "approval expired" (DESIGN 5.2 rule 10).
func (e *Engine) sweepApprovals(ctx context.Context) {
	due, err := store.ListExpiredApprovals(ctx, e.deps.Pool, e.clock.Now())
	if err != nil {
		e.log.Warn("approval sweep failed", "error", err)
		return
	}
	for i := range due {
		a := &due[i]
		if _, ok := e.expireApproval(ctx, a, "approval expired"); ok {
			e.failRun(ctx, a.RunID, "approval expired")
		}
	}
}

// failRun ends a run that is not terminal with reason: a live visit aborts its turn,
// a run paused with no worker is failed in place.
func (e *Engine) failRun(ctx context.Context, runID, reason string) {
	e.mu.Lock()
	v := e.active[runID]
	e.mu.Unlock()
	if v != nil {
		o := failed(store.RunFailed, reason)
		o.limit = true
		v.post(func(ctx context.Context) {
			if v.aborting == nil && v.result == nil {
				v.startAbort(ctx, o)
			}
		})
		return
	}
	now := e.clock.Now()
	run, err := store.TransitionRun(ctx, e.deps.Pool, runID, []string{store.RunWaitingApproval}, store.RunFailed, &reason, now)
	if err != nil {
		return // terminal already, or a worker owns it
	}
	e.closeWaitingRun(ctx, run, reason, "run ended")
}

// cancelWaitingRun cancels a run paused with no worker (an approval node, or a turn
// whose worker is gone): its approvals expire with reason "run cancelled" (DESIGN 5.3
// rule 8).
func (e *Engine) cancelWaitingRun(ctx context.Context, runID, reason string) (*store.Run, bool) {
	run, err := store.TransitionRun(ctx, e.deps.Pool, runID, []string{store.RunWaitingApproval}, store.RunCancelled, &reason, e.clock.Now())
	if err != nil {
		return nil, false
	}
	e.closeWaitingRun(ctx, run, reason, "run cancelled")
	return run, true
}

// closeWaitingRun is the terminal housekeeping of a run that ended while paused.
func (e *Engine) closeWaitingRun(ctx context.Context, run *store.Run, reason, approvalReason string) {
	now := e.clock.Now()
	e.expireRunApprovals(ctx, run.ID, approvalReason)
	for _, n := range unfinishedVisits(ctx, e, run.ID) {
		r := reason
		if rn, err := store.FinishRunNode(ctx, e.deps.Pool, n.ID, store.NodeFailed, &r, now); err == nil {
			e.emit(ctx, run.ID, "node.status", map[string]any{"node": rn})
		}
	}
	if root, err := store.OpenRootSpan(ctx, e.deps.Pool, run.ID); err == nil {
		e.endSpan(ctx, root, now, map[string]any{"status": run.Status, "reason": reason})
	}
	e.emit(ctx, run.ID, "run.status", map[string]any{"run": run})
	e.logEvent(ctx, run.ID, "warn", "run "+run.Status+": "+reason)
}

// listenGateway takes decisions made on OpenClaw's own approval surfaces (DESIGN 5.3
// rule 5a) until ctx ends.
func (e *Engine) listenGateway(ctx context.Context) {
	if e.deps.Gateway == nil {
		return
	}
	events, unsub := e.deps.Gateway.Subscribe(1024)
	defer unsub()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			e.onGatewayEvent(ctx, ev)
		}
	}
}

// onGatewayEvent applies plugin.approval.resolved: a decision by someone is a decision
// by "openclaw"; the gateway's own timeout (resolvedBy null) is ignored, so the
// approval stays pending until the platform's expiry.
func (e *Engine) onGatewayEvent(ctx context.Context, ev openclaw.Event) {
	if ev.Name != openclaw.EventPluginApprovalResolved {
		return
	}
	var pe openclaw.PluginApprovalResolvedEvent
	if json.Unmarshal(ev.Payload, &pe) != nil || pe.ID == "" {
		return
	}
	a, err := store.GetApprovalByExternalID(ctx, e.deps.Pool, pe.ID)
	if err != nil || a.Kind != store.ApprovalKindTool || a.Status != store.ApprovalPending {
		return
	}
	if !pe.Decided() {
		e.logEvent(ctx, a.RunID, "info", "the gateway's approval window ended; the approval stays pending on the platform",
			"approval_id", a.ID)
		return
	}
	status := store.ApprovalRejected
	if strings.HasPrefix(pe.Decision, "allow") {
		status = store.ApprovalApproved
	}
	if _, err := e.decideTool(ctx, a, status, "openclaw", pe.Decision, false); err != nil && !errors.Is(err, ErrNotPending) {
		e.log.Warn("apply gateway approval decision failed", "approval_id", a.ID, "error", err)
	}
}

func strValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}
