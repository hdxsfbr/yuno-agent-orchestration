package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"yuno/internal/obs"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// walk executes a run's workflow graph from start, or from where a paused or re-queued
// run stopped (DESIGN 5.6). Only the walk moves runs.current_node; node outputs reach
// runs.state one key at a time, so the fallback execution of an approved side effect
// can append to state.pending_effects concurrently.
type walk struct {
	e        *Engine
	run      *store.Run
	def      *workflow.Definition
	over     runOverrides
	rootSpan string
	stale    []store.RunNode // a lost worker's unfinished visits, for the next agent visit
	sessions []string        // node sessions to delete when the worker lets the run go
}

func (w *walk) execute(ctx context.Context) outcome {
	cur, res := w.resume(ctx)
	if res != nil {
		return *res
	}
	for {
		if ctx.Err() != nil {
			return outcome{interrupted: true}
		}
		node, ok := w.def.Node(cur)
		if !ok {
			return failed(store.RunFailed, fmt.Sprintf("unknown node '%s'", cur))
		}
		var next string
		switch node.Kind {
		case workflow.KindStart:
			next, res = w.follow(ctx, node)
		case workflow.KindEnd:
			w.setCurrent(ctx, node.ID)
			return succeeded(nil)
		case workflow.KindAgent:
			next, res = w.agentNode(ctx, node)
		case workflow.KindApproval:
			next, res = w.approvalNode(ctx, node)
		default:
			return failed(store.RunFailed, fmt.Sprintf("node %s: unknown kind '%s'", node.ID, node.Kind))
		}
		if res != nil {
			return *res
		}
		cur = next
	}
}

// resume returns the node to process first. A run paused at an approval node continues
// by that visit's decision; any other node is (re-)entered.
func (w *walk) resume(ctx context.Context) (string, *outcome) {
	if w.run.CurrentNode == nil || *w.run.CurrentNode == "" {
		start, ok := w.def.Start()
		if !ok {
			o := failed(store.RunFailed, "workflow has no start node")
			return "", &o
		}
		return start, nil
	}
	cur := *w.run.CurrentNode
	node, ok := w.def.Node(cur)
	if !ok || node.Kind != workflow.KindApproval {
		return cur, nil
	}
	// Only the visit the run paused at holds a decision for this entry. Moving off the
	// gate drops that pointer (setCurrent), so a gate the walk reached again, with the
	// run stopped before the new visit asked, is entered afresh rather than passed on an
	// old approval.
	if w.run.ResumeNodeVisitID == nil {
		return cur, nil
	}
	rn, err := store.GetRunNode(ctx, w.e.deps.Pool, *w.run.ResumeNodeVisitID)
	if err != nil || rn.NodeID != cur {
		return cur, nil
	}
	a, err := store.GetApprovalForRunNode(ctx, w.e.deps.Pool, rn.ID, store.ApprovalKindNode)
	if err != nil {
		return cur, nil // the approval was never raised: enter the node
	}
	switch a.Status {
	case store.ApprovalApproved:
		w.e.logEvent(ctx, w.run.ID, "info", fmt.Sprintf("%s approved; continuing", cur), "node_id", cur)
		next, res := w.follow(ctx, node)
		return next, res
	case store.ApprovalRejected:
		if node.OnReject != "" {
			w.e.logEvent(ctx, w.run.ID, "info", fmt.Sprintf("%s rejected; following on_reject to %s", cur, node.OnReject), "node_id", cur)
			w.setCurrent(ctx, node.OnReject)
			return node.OnReject, nil
		}
		o := failed(store.RunFailed, "rejected at "+cur)
		o.limit = true
		return "", &o
	case store.ApprovalExpired:
		o := failed(store.RunFailed, "approval expired")
		o.limit = true
		return "", &o
	}
	// Still pending: whoever queued the run did so early. Wait again.
	o := w.pauseAt(ctx, rn.ID)
	return "", &o
}

// enter checks the limits before a node runs: the run's own limits and the loop limit
// on how often one node may run (DESIGN 5.2).
func (w *walk) enter(ctx context.Context, node *workflow.Node) *outcome {
	e := w.e
	w.refresh(ctx)
	if viol := checkLimits(limitState{run: w.run, limits: w.run.Limits, now: e.clock.Now()}); viol != nil {
		o := violated(viol)
		return &o
	}
	if max := w.run.Limits.MaxLoopVisits; max > 0 {
		n, err := store.CountNodeVisits(ctx, e.deps.Pool, w.run.ID, node.ID)
		if err != nil {
			o := failed(store.RunFailed, "count node visits: "+err.Error())
			return &o
		}
		if n >= max {
			o := violated(&violation{store.RunFailed, fmt.Sprintf("loop limit %d exceeded for %s", max, node.ID)})
			return &o
		}
	}
	w.setCurrent(ctx, node.ID)
	return nil
}

// agentNode runs one visit of an agent node and returns the next node.
func (w *walk) agentNode(ctx context.Context, node *workflow.Node) (string, *outcome) {
	e := w.e
	if res := w.enter(ctx, node); res != nil {
		return "", res
	}
	v, early := e.newVisit(ctx, w.run, node, w.over, w.rootSpan, w.stale)
	if v == nil {
		return "", &early
	}
	w.stale = nil
	if v.early == nil {
		task, err := w.renderTask(node)
		if err != nil {
			o := failed(store.RunFailed, "task template: "+err.Error())
			v.early = &o
		}
		v.task = task
	}
	if v.early == nil {
		from, err := w.carrySources(ctx, v)
		if err != nil {
			o := failed(store.RunFailed, "run directory: "+err.Error())
			v.early = &o
		}
		v.carryFrom = from
	}
	e.mu.Lock()
	e.active[w.run.ID] = v
	e.mu.Unlock()
	res := v.execute(ctx)
	e.mu.Lock()
	if e.active[w.run.ID] == v {
		delete(e.active, w.run.ID)
	}
	e.mu.Unlock()
	wctx := context.WithoutCancel(ctx)
	if res.interrupted {
		// Shutting down: OpenClaw must not keep executing a turn nobody mirrors. The run
		// stays for recovery, whose replacement visit starts a session of its own.
		v.abortLiveTurn(wctx)
		v.release()
		return "", &res
	}
	v.abortLiveTurn(wctx) // a turn whose chat.send answer was lost, or an unconfirmed abort
	v.finish(wctx, res)
	if v.sessionCreated {
		w.sessions = append(w.sessions, v.key)
	}
	if res.nodeStatus != store.NodeSucceeded {
		return w.nodeFailed(ctx, node, res)
	}
	return w.advance(ctx, node, res.output)
}

// carrySources lists the workspaces whose runs/<run_id>/ flows into visit v's agent
// before its turn (DESIGN 5.6 rule 13): the agents of the run's earlier visits, ordered
// by each agent's last visit so a later visit's file wins. An agent whose last visit
// came before v's own agent last ran is left out: its files reached this workspace
// then, and this agent's own later changes must win over them.
func (w *walk) carrySources(ctx context.Context, v *visit) ([]string, error) {
	visits, err := store.ListRunNodes(ctx, w.e.deps.Pool, w.run.ID)
	if err != nil {
		return nil, err
	}
	last := map[string]int{} // OpenClaw agent id → index of its last earlier visit
	var order []string
	for i, n := range visits {
		if n.ID == v.rn.ID || n.SessionKey == nil {
			continue
		}
		id := sessionAgent(*n.SessionKey)
		if id == "" {
			continue
		}
		if _, ok := last[id]; !ok {
			order = append(order, id)
		}
		last[id] = i
	}
	own, ranBefore := last[v.ocID]
	var out []string
	for _, id := range order {
		if id != v.ocID && (!ranBefore || last[id] > own) {
			out = append(out, id)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return last[a] - last[b] })
	return out, nil
}

// sessionAgent is the OpenClaw agent id in a node session key (SessionKey): the
// workspace the visit worked in.
func sessionAgent(key string) string {
	rest, ok := strings.CutPrefix(key, "agent:")
	if !ok {
		return ""
	}
	id, _, ok := strings.Cut(rest, ":")
	if !ok {
		return ""
	}
	return id
}

// renderTask is the node's task: the template over {input, state}, then $RUN_ID, then
// the outputs of its input_from nodes (DESIGN 5.6 rule 5). The protocol block is the
// visit's.
func (w *walk) renderTask(node *workflow.Node) (string, error) {
	task, err := workflow.Render(node.Task, w.run.Input, w.run.State)
	if err != nil {
		return "", err
	}
	task = strings.ReplaceAll(task, "$RUN_ID", w.run.ID)
	start, _ := w.def.Start()
	if block := inputContext(node.InputFrom, start, w.run.Input, w.run.State); block != "" {
		task += "\n\n" + block
	}
	return task, nil
}

// inputContext lists the JSON output of each input_from node that has one; the start
// node stands for the run input.
func inputContext(from []string, startID string, input, state json.RawMessage) string {
	var st map[string]json.RawMessage
	_ = json.Unmarshal(state, &st)
	var sb strings.Builder
	for _, n := range from {
		raw, label := st[n], n
		if n == startID || n == workflow.KindStart {
			raw, label = input, n+" (run input)"
		}
		if isNullJSON(raw) {
			continue
		}
		var buf bytes.Buffer
		if json.Compact(&buf, raw) == nil {
			raw = buf.Bytes()
		}
		sb.WriteString(label + ": " + string(raw) + "\n")
	}
	if sb.Len() == 0 {
		return ""
	}
	return "Context from earlier nodes:\n" + strings.TrimSuffix(sb.String(), "\n")
}

// nodeFailed follows the node's on_failure edge when the failure is the node's own,
// not a limit or a cancellation; otherwise the run ends with the node's reason (DESIGN
// 5.6 rule 12).
func (w *walk) nodeFailed(ctx context.Context, node *workflow.Node, res outcome) (string, *outcome) {
	if res.limit || node.OnFailure == "" {
		return "", &res
	}
	w.e.logEvent(ctx, w.run.ID, "warn", fmt.Sprintf("%s failed (%s); following on_failure to %s", node.ID, res.reason, node.OnFailure),
		"node_id", node.ID)
	w.setCurrent(ctx, node.OnFailure)
	return node.OnFailure, nil
}

// advance stores an agent node's output as state[<node>] (replacing a previous visit's),
// chooses the next node and records the hand-off as an agent message (DESIGN 5.6 rule 6).
func (w *walk) advance(ctx context.Context, node *workflow.Node, output json.RawMessage) (string, *outcome) {
	e := w.e
	w.refresh(ctx)
	var state map[string]any
	_ = json.Unmarshal(w.run.State, &state)
	if state == nil {
		state = map[string]any{}
	}
	var out any
	_ = json.Unmarshal(output, &out)
	state[node.ID] = out
	next, ok := w.def.Next(node.ID, state)
	cur := next
	if !ok {
		cur = node.ID
	}
	run, err := store.SetRunStateNode(ctx, e.deps.Pool, w.run.ID, node.ID, output, cur)
	if err != nil {
		o := failed(store.RunFailed, "store node output in run state: "+err.Error())
		return "", &o
	}
	w.run = run
	if !ok {
		e.emit(ctx, w.run.ID, "run.status", map[string]any{"run": run})
		o := failed(store.RunFailed, "no matching edge from "+node.ID)
		o.limit = true // the run's outcome, not the node's: on_failure does not apply
		return "", &o
	}
	msg := &store.AgentMessage{RunID: w.run.ID, FromNode: node.ID, ToNode: next, Payload: output, CreatedAt: e.clock.Now()}
	if err := store.InsertAgentMessage(ctx, e.deps.Pool, msg); err != nil {
		e.log.Warn("insert agent message failed", "run_id", w.run.ID, "node_id", node.ID, "error", err)
	} else {
		e.emit(ctx, w.run.ID, "agent_message", map[string]any{"agent_message": msg})
	}
	e.emit(ctx, w.run.ID, "run.status", map[string]any{"run": run})
	return next, nil
}

// follow takes the first true edge from a node that produces no output (start, an
// approved approval node).
func (w *walk) follow(ctx context.Context, node *workflow.Node) (string, *outcome) {
	w.refresh(ctx)
	var state any
	_ = json.Unmarshal(w.run.State, &state)
	next, ok := w.def.Next(node.ID, state)
	if !ok {
		o := failed(store.RunFailed, "no matching edge from "+node.ID)
		o.limit = true
		return "", &o
	}
	w.setCurrent(ctx, next)
	return next, nil
}

// approvalNode raises the human gate of an approval node and lets the worker go
// (DESIGN 5.6 rule 8, 5.3 rule 9). Nothing in OpenClaw is involved.
func (w *walk) approvalNode(ctx context.Context, node *workflow.Node) (string, *outcome) {
	e := w.e
	if res := w.enter(ctx, node); res != nil {
		return "", res
	}
	prior, err := store.CountNodeVisits(ctx, e.deps.Pool, w.run.ID, node.ID)
	if err != nil {
		o := failed(store.RunFailed, "count node visits: "+err.Error())
		return "", &o
	}
	now := e.clock.Now()
	rn := &store.RunNode{RunID: w.run.ID, NodeID: node.ID, Visit: prior + 1, Status: store.NodeWaitingApproval, StartedAt: &now}
	if err := store.CreateRunNode(ctx, e.deps.Pool, rn); err != nil {
		o := failed(store.RunFailed, "create node visit: "+err.Error())
		return "", &o
	}
	message, err := workflow.Render(node.Message, w.run.Input, w.run.State)
	if err != nil {
		reason := obs.Redact("message template: " + err.Error())
		if done, ferr := store.FinishRunNode(ctx, e.deps.Pool, rn.ID, store.NodeFailed, &reason, now); ferr == nil {
			e.emit(ctx, w.run.ID, "node.status", map[string]any{"node": done})
		}
		return w.nodeFailed(ctx, node, failed(store.RunFailed, reason))
	}
	timeout := w.run.Limits.ApprovalTimeoutSeconds
	if timeout <= 0 {
		timeout = store.DefaultLimits.ApprovalTimeoutSeconds
	}
	a := &store.Approval{ID: store.NewID(), RunID: w.run.ID, RunNodeID: rn.ID, Kind: store.ApprovalKindNode, NodeID: node.ID,
		Message: obs.Redact(message), Status: store.ApprovalPending, RequestedAt: now,
		ExpiresAt: now.Add(secondsDuration(timeout))}
	// The span exists before the approval can be decided, so the decision closes it.
	span := e.startSpan(ctx, w.run.ID, &w.rootSpan, "approval_wait", "approval "+node.ID, now, nil,
		map[string]any{"approval_id": a.ID, "kind": store.ApprovalKindNode, "node_id": node.ID, "run_node_id": rn.ID})
	// The approval row and the pause are one step, taken before anything is published:
	// a decision always finds the run paused, so its re-queue applies, and a decision on
	// another approval of this run never finds it paused with nothing pending.
	run, err := store.InsertApprovalAndPause(ctx, e.deps.Pool, a, now)
	if err != nil {
		e.endSpan(ctx, span, now, map[string]any{"status": "failed", "error": err.Error()})
		o := failed(store.RunFailed, "pause for approval: "+err.Error())
		return "", &o
	}
	w.run = run
	e.emit(ctx, w.run.ID, "node.status", map[string]any{"node": rn})
	e.emit(ctx, w.run.ID, "run.status", map[string]any{"run": run})
	e.emit(ctx, w.run.ID, "approval.requested", map[string]any{"approval": a})
	e.logEvent(ctx, w.run.ID, "info", "waiting for approval at "+node.ID, "node_id", node.ID, "approval_id", a.ID)
	o := w.applyPendingCancel(ctx, run)
	return "", &o
}

// pauseAt moves the run to waiting_approval for a node visit and releases the worker.
func (w *walk) pauseAt(ctx context.Context, runNodeID string) outcome {
	e := w.e
	run, err := store.PauseRunAt(ctx, e.deps.Pool, w.run.ID, runNodeID, e.clock.Now())
	if err != nil {
		o := failed(store.RunFailed, "pause run: "+err.Error())
		return o
	}
	w.run = run
	e.emit(ctx, w.run.ID, "run.status", map[string]any{"run": run})
	return w.applyPendingCancel(ctx, run)
}

// applyPendingCancel applies, to a run that just paused, a cancellation that arrived
// while the run was between nodes, when Cancel found no visit to stop. The worker lets
// the run go either way.
func (w *walk) applyPendingCancel(ctx context.Context, run *store.Run) outcome {
	e := w.e
	if run.CancelRequested {
		by := "user"
		if run.CancelBy != nil && *run.CancelBy != "" {
			by = *run.CancelBy
		}
		e.cancelWaitingRun(context.WithoutCancel(ctx), run.ID, "cancelled by "+by)
	}
	return outcome{paused: true}
}

func (w *walk) setCurrent(ctx context.Context, node string) {
	if err := store.SetRunCurrentNode(ctx, w.e.deps.Pool, w.run.ID, node); err != nil {
		w.e.log.Warn("set current node failed", "run_id", w.run.ID, "node_id", node, "error", err)
		return
	}
	w.run.CurrentNode = &node
}

func (w *walk) refresh(ctx context.Context) {
	if run, err := store.GetRun(ctx, w.e.deps.Pool, w.run.ID); err == nil {
		w.run = run
	} else if !errors.Is(err, context.Canceled) {
		w.e.log.Warn("re-read run failed", "run_id", w.run.ID, "error", err)
	}
}
