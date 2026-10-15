package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"yuno/internal/obs"
	"yuno/internal/openclaw"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// outcome is how a node visit, and with it the run, ended.
type outcome struct {
	runStatus  string // succeeded | failed | cancelled | timed_out
	nodeStatus string // succeeded | failed
	reason     string
	output     json.RawMessage
	// interrupted means the engine is shutting down: the run is left for the stale-run
	// sweeper to re-queue and nothing terminal is written.
	interrupted bool
}

func succeeded(output json.RawMessage) outcome {
	return outcome{runStatus: store.RunSucceeded, nodeStatus: store.NodeSucceeded, output: output}
}

// failed is a failure outcome; the reason is stored and published, so credentials in
// an error text are redacted here.
func failed(status, reason string) outcome {
	return outcome{runStatus: status, nodeStatus: store.NodeFailed, reason: obs.Redact(reason)}
}

// runRun executes one claimed run to a terminal state (DESIGN 5.1).
func (e *Engine) runRun(ctx context.Context, run *store.Run) {
	wctx := context.WithoutCancel(ctx)
	now := e.clock.Now()
	if run.Attempt <= 1 || run.StartedAt == nil {
		if err := store.SetRunStartedAt(wctx, e.deps.Pool, run.ID, now); err != nil {
			e.log.Warn("set started_at failed", "run_id", run.ID, "error", err)
		}
		run.StartedAt = &now
	}
	var stale []store.RunNode
	if run.Attempt > 1 {
		// The lost worker's turns may still run in OpenClaw: the replacement visit aborts
		// them by their recorded run ids before it starts its own turn.
		stale = unfinishedVisits(wctx, e, run.ID)
		if n, err := store.FailRunningNodes(wctx, e.deps.Pool, run.ID, "worker lost", now); err == nil && n > 0 {
			e.logEvent(ctx, run.ID, "warn", fmt.Sprintf("attempt %d: %d unfinished node visit(s) marked failed", run.Attempt, n))
		}
	}
	rootSpan := e.startSpan(ctx, run.ID, nil, "run", "run", now, nil, map[string]any{"attempt": run.Attempt, "trigger": run.Trigger})
	if fresh, err := store.GetRun(wctx, e.deps.Pool, run.ID); err == nil {
		run = fresh
	}
	e.emit(ctx, run.ID, "run.status", map[string]any{"run": run})

	var snap runSnapshot
	var node *workflow.Node
	err := json.Unmarshal(run.WorkflowSnapshot, &snap)
	if err == nil {
		node, err = singleAgentNode(&snap.Definition)
	}
	var res outcome
	sessionKey := ""
	if err != nil {
		res = failed(store.RunFailed, err.Error())
	} else {
		over := runOverrides{}
		if snap.RunOverrides != nil {
			over = *snap.RunOverrides
		}
		res, sessionKey = e.runAgentNode(ctx, run, node, over, rootSpan, stale)
	}
	if res.interrupted {
		e.endSpan(ctx, rootSpan, e.clock.Now(), map[string]any{"interrupted": true})
		return
	}
	e.finishRun(wctx, run.ID, node, res, rootSpan)
	e.deleteSession(wctx, sessionKey)
}

// unfinishedVisits lists a run's node visits that never reached a terminal status.
func unfinishedVisits(ctx context.Context, e *Engine, runID string) []store.RunNode {
	nodes, err := store.ListRunNodes(ctx, e.deps.Pool, runID)
	if err != nil {
		e.log.Warn("list unfinished node visits failed", "run_id", runID, "error", err)
		return nil
	}
	var out []store.RunNode
	for _, n := range nodes {
		switch n.Status {
		case store.NodePending, store.NodeRunning, store.NodeWaitingApproval:
			out = append(out, n)
		}
	}
	return out
}

// singleAgentNode returns the one agent node of a definition. Multi-node workflows are
// not executed by this engine version.
func singleAgentNode(d *workflow.Definition) (*workflow.Node, error) {
	var found *workflow.Node
	for i := range d.Nodes {
		if d.Nodes[i].Kind == workflow.KindAgent {
			if found != nil {
				return nil, errors.New("workflow has more than one agent node")
			}
			found = &d.Nodes[i]
		}
	}
	if found == nil {
		return nil, errors.New("workflow has no agent node")
	}
	return found, nil
}

// finishRun writes the run's terminal state: typed state, status, SSE, root span.
func (e *Engine) finishRun(ctx context.Context, runID string, node *workflow.Node, res outcome, rootSpan string) {
	now := e.clock.Now()
	if res.runStatus == store.RunSucceeded && node != nil {
		state := map[string]json.RawMessage{node.ID: res.output}
		b, _ := json.Marshal(state)
		end := "end"
		if err := store.SetRunState(ctx, e.deps.Pool, runID, b, &end); err != nil {
			e.log.Warn("set run state failed", "run_id", runID, "error", err)
		}
	}
	var reason *string
	if res.reason != "" {
		r := res.reason
		reason = &r
	}
	run, err := e.transition(ctx, runID, res.runStatus, reason, now)
	if err != nil {
		e.log.Error("terminal transition failed", "run_id", runID, "to", res.runStatus, "error", err)
		if run, err = store.GetRun(ctx, e.deps.Pool, runID); err != nil {
			return
		}
	}
	e.endSpan(ctx, rootSpan, now, map[string]any{"status": run.Status, "steps": run.StepCount,
		"tool_calls": run.ToolCallCount, "cost_usd_micros": run.CostUSDMicros, "reason": res.reason})
	e.emit(ctx, runID, "run.status", map[string]any{"run": run})
	level := "info"
	msg := "run " + run.Status
	if res.reason != "" {
		level = "warn"
		msg += ": " + res.reason
	}
	e.logEvent(ctx, runID, level, msg)
}

// transition moves a run from its current status to `to`, checking the state machine.
// A run waiting for an approval is resumed first when the target needs it.
func (e *Engine) transition(ctx context.Context, runID, to string, reason *string, at time.Time) (*store.Run, error) {
	for attempt := 0; attempt < 3; attempt++ {
		cur, err := store.GetRun(ctx, e.deps.Pool, runID)
		if err != nil {
			return nil, err
		}
		if cur.Status == to {
			return cur, nil
		}
		if err := ValidTransition(cur.Status, to); err != nil {
			if cur.Status == store.RunWaitingApproval && ValidTransition(store.RunRunning, to) == nil {
				if _, err := store.ResumeRunAt(ctx, e.deps.Pool, runID, at); err != nil && !errors.Is(err, store.ErrNotFound) {
					return nil, err
				}
				continue
			}
			return cur, err
		}
		run, err := store.TransitionRun(ctx, e.deps.Pool, runID, []string{cur.Status}, to, reason, at)
		if errors.Is(err, store.ErrNotFound) {
			continue // changed underneath us; re-read
		}
		return run, err
	}
	return nil, fmt.Errorf("run %s: status kept changing", runID)
}

// runAgentNode executes one visit of an agent node and returns its outcome and the
// session to delete once the run's terminal state is stored.
func (e *Engine) runAgentNode(ctx context.Context, run *store.Run, node *workflow.Node, over runOverrides, rootSpan string, stale []store.RunNode) (outcome, string) {
	v, res := e.newVisit(ctx, run, node, over, rootSpan, stale)
	if v == nil {
		return res, ""
	}
	e.mu.Lock()
	e.active[run.ID] = v
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.active, run.ID)
		e.mu.Unlock()
	}()
	res = v.execute(ctx)
	wctx := context.WithoutCancel(ctx)
	if res.interrupted {
		// Shutting down: OpenClaw must not keep executing a turn nobody mirrors. The run
		// stays for recovery, whose replacement visit starts a session of its own.
		v.abortLiveTurn(wctx)
		v.release()
		return res, ""
	}
	v.abortLiveTurn(wctx) // a turn whose chat.send answer was lost, or an unconfirmed abort
	v.finish(wctx, res)
	if !v.sessionCreated {
		return res, "" // nothing reached the gateway that could have created the session
	}
	return res, v.key
}

// deleteSession removes a finished node session from OpenClaw, best effort (DESIGN
// 5.14 rule 5).
func (e *Engine) deleteSession(ctx context.Context, key string) {
	if e.deps.Gateway == nil || key == "" {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := openclaw.SessionsDelete(cctx, e.deps.Gateway, key); err != nil {
		e.log.Warn("delete node session failed", "session_key", key, "error", err)
	}
}
