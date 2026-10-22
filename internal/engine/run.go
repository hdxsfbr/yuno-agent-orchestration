package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"yuno/internal/obs"
	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// outcome is how a node visit, and with it the run, ended.
type outcome struct {
	runStatus  string // succeeded | failed | cancelled | timed_out
	nodeStatus string // succeeded | failed
	reason     string
	output     json.RawMessage
	// limit marks a limit violation or a cancellation: a node's on_failure edge does not
	// apply to those (DESIGN 5.6 rule 12).
	limit bool
	// interrupted means the engine is shutting down: the run is left for the stale-run
	// sweeper to re-queue and nothing terminal is written.
	interrupted bool
	// paused means the run waits at an approval node: the worker lets it go and a
	// decision re-queues it (DESIGN 5.6 rule 8).
	paused bool
	// operatorPause is a visit stopped for the operator's pause (DESIGN W2 rule 14): its
	// turn is confirmed stopped, and the walk saves its checkout and parks it.
	operatorPause bool
	// leaseLost means the worker found its lease gone (DESIGN W2 rule 9): it writes nothing
	// more to the run, which is another worker's now.
	leaseLost bool
}

func succeeded(output json.RawMessage) outcome {
	return outcome{runStatus: store.RunSucceeded, nodeStatus: store.NodeSucceeded, output: output}
}

// failed is a failure outcome; the reason is stored and published, so credentials in
// an error text are redacted here. A timeout or a cancellation is never a node error.
func failed(status, reason string) outcome {
	return outcome{runStatus: status, nodeStatus: store.NodeFailed, reason: obs.Redact(reason), limit: status != store.RunFailed}
}

// violated is the outcome of a limit violation (DESIGN 5.2).
func violated(v *violation) outcome {
	o := failed(v.status, v.reason)
	o.limit = true
	return o
}

// runRun executes one claimed run until it is terminal or waits at an approval node
// (DESIGN 5.1, 5.6).
func (e *Engine) runRun(ctx context.Context, run *store.Run) {
	// The worker keeps the lease epoch it claimed the run with; every fenced write of the
	// run carries it (DESIGN W2 rules 7 and 8).
	lease := store.NewLease(run)
	ctx = store.WithLease(ctx, lease)
	wctx := context.WithoutCancel(ctx)
	now := e.clock.Now()
	resumed := run.CurrentNode != nil && *run.CurrentNode != ""
	// The time limit counts from started_at. A run claimed the first time is stamped here
	// from the injected clock; a retried run (DESIGN 5.21 rule 2) arrives with started_at
	// cleared, because its claim leaves it empty, and is stamped by the same line, with
	// paused_total_ms already back at 0. A resumed claim later on (an approval, a crash
	// re-queue) finds it set and keeps it.
	if run.StartedAt == nil || (run.Attempt <= 1 && !resumed) {
		if err := store.SetRunStartedAt(wctx, e.deps.Pool, run.ID, now); err != nil {
			e.log.Warn("set started_at failed", "run_id", run.ID, "error", err)
			// The row keeps no start (a retried run: null), and every re-read of it would
			// show a run with no time limit. Keep the stamp in memory for this attempt
			// (DESIGN 5.21 rule 2: the time limit restarts at the claim).
			e.unsavedStarts.Store(run.ID, now)
			defer e.unsavedStarts.Delete(run.ID)
		}
		run.StartedAt = &now
	}
	// Visits left unfinished by a lost worker (a re-queue, or a decision on a run whose
	// worker is gone) may still have turns in OpenClaw: the next agent visit aborts them
	// by their recorded run ids before it starts its own turn.
	// The fork visit a run resumes at after its branches parked is open on purpose
	// (DESIGN 5.26 rule 8): it is not a lost worker's visit.
	stale := unfinishedVisits(wctx, e, run.ID)
	keep := ""
	if run.ResumeNodeVisitID != nil {
		for _, n := range stale {
			if n.ID == *run.ResumeNodeVisitID && n.AgentID == nil { // a fork row: no agent, no session
				keep = n.ID
			}
		}
	}
	// A visit still parked on its question (a branch whose sibling was answered first,
	// DESIGN 5.26 rule 9) waits for its own decision: it is not lost either.
	stale = slices.DeleteFunc(stale, func(n store.RunNode) bool {
		return n.ID == keep || (n.ParkedAt != nil && n.Status == store.NodeWaitingApproval)
	})
	if len(stale) > 0 {
		if n, err := store.FailRunningNodesExcept(wctx, e.deps.Pool, run.ID, keep, "worker lost", now); err == nil && n > 0 {
			e.logEvent(ctx, run.ID, "warn", fmt.Sprintf("attempt %d: %d unfinished node visit(s) marked failed", run.Attempt, n))
		}
	}
	rootSpan := ""
	if resumed {
		rootSpan, _ = store.OpenRootSpan(wctx, e.deps.Pool, run.ID)
	}
	if rootSpan == "" {
		attrs := map[string]any{"attempt": run.Attempt, "trigger": run.Trigger}
		if run.Retries > 0 {
			// A retried claim opens a root span of its own (the earlier attempts' spans
			// stay in the trace with their own starts), marked with the retry's number
			// (DESIGN 5.21 rule 4).
			attrs["retry"] = run.Retries
		}
		rootSpan = e.startSpan(ctx, run.ID, nil, "run", "run", now, nil, attrs)
	}
	if fresh, err := e.readRun(wctx, run.ID); err == nil {
		run = fresh
	}
	e.emit(ctx, run.ID, "run.status", map[string]any{"run": run})

	var snap runSnapshot
	var res outcome
	w := &walk{e: e, run: run, rootSpan: rootSpan, stale: stale}
	if err := json.Unmarshal(run.WorkflowSnapshot, &snap); err != nil {
		res = failed(store.RunFailed, "workflow snapshot: "+err.Error())
	} else {
		w.def = &snap.Definition
		if snap.RunOverrides != nil {
			w.over = *snap.RunOverrides
		}
		res = w.execute(ctx)
	}
	if lease.Lost() {
		res = outcome{leaseLost: true}
	}
	switch {
	case res.leaseLost:
		// DESIGN W2 rule 9: the run is another worker's; this one stops and writes nothing.
		e.leaseLost(wctx, run.ID)
		return
	case res.interrupted:
		e.endSpan(ctx, rootSpan, e.clock.Now(), map[string]any{"interrupted": true})
		return
	case res.paused:
		// The root span stays open: the worker that resumes the run continues it.
	default:
		e.finishRun(wctx, run.ID, res, rootSpan)
	}
	for _, key := range w.sessions {
		e.deleteSession(wctx, key)
	}
	if res.paused {
		// The worker let the run go: the commands still pending for it are decided as the api
		// decides those of a run no worker holds (DESIGN W2 rule 5).
		e.settleUnheld(wctx, run.ID)
	}
}

// readRun re-reads a run the engine is executing. A started_at stamp whose write failed
// (runRun) is put back on the row it returns, so the time limit does not vanish with the
// next re-read.
func (e *Engine) readRun(ctx context.Context, id string) (*store.Run, error) {
	run, err := store.GetRun(ctx, e.deps.Pool, id)
	if err == nil && run.StartedAt == nil {
		if at, ok := e.unsavedStarts.Load(id); ok {
			t := at.(time.Time)
			run.StartedAt = &t
		}
	}
	return run, err
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

// finishRun writes the run's terminal state: status, approvals it leaves behind, SSE,
// root span.
func (e *Engine) finishRun(ctx context.Context, runID string, res outcome, rootSpan string) {
	now := e.clock.Now()
	var reason *string
	if res.reason != "" {
		r := res.reason
		reason = &r
	}
	run, err := e.transition(ctx, runID, res.runStatus, reason, now)
	if errors.Is(err, store.ErrLeaseLost) {
		e.leaseLost(ctx, runID) // DESIGN W2 rule 9
		return
	}
	if err != nil {
		e.log.Error("terminal transition failed", "run_id", runID, "to", res.runStatus, "error", err)
		if run, err = store.GetRun(ctx, e.deps.Pool, runID); err != nil {
			return
		}
	}
	// DESIGN W2 rules 11 and 12: whether the run's last turn is confirmed stopped, then the
	// commands it leaves pending (a cancel applied, or acknowledged until the reconcile pass
	// confirms the stop; the rest rejected, the run having ended first).
	if state := e.stopState(ctx, e.deps.Pool, run); state != "" && state != run.CancelState {
		if done, err := store.SetRunCancelState(ctx, e.deps.Pool, runID, state); err == nil {
			run = done
		} else {
			e.log.Warn("record the stop's state failed", "run_id", runID, "error", err)
		}
	}
	e.settleAtEnd(ctx, run)
	if run.Status != store.RunSucceeded {
		why := "run ended"
		if run.Status == store.RunCancelled {
			why = "run cancelled"
		}
		e.expireRunApprovals(ctx, runID, why)
		e.closeBranches(ctx, run, res.reason)
		// A visit the ending left open (a fork row a limit found at re-entry, DESIGN 5.26
		// rule 12) closes with the run's reason.
		if n, err := store.FailRunningNodes(ctx, e.deps.Pool, runID, res.reason, now); err != nil {
			e.log.Warn("close open visits failed", "run_id", runID, "error", err)
		} else if n > 0 {
			e.log.Info("open visits closed with the run", "run_id", runID, "visits", n)
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
	// DESIGN 5.15 rule 9: a run delegated from a conversation reports how it ended.
	e.afterTerminal(ctx, run)
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
