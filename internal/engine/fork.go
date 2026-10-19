package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// Parallel steps (DESIGN 5.26, decisions D-048, D-060 to D-064). A fork node starts one
// branch per true edge; every branch runs at once as its own walk over the fork's region
// (walk.go: a walk with a branch), recorded as run_branches rows under the fork's visit
// row; the fork is the run's re-entry point until its join passes. The first branch to
// fail stops the siblings and fails the run (rule 6); a branch parked on a question leaves
// the run running until every branch has ended or parked, then the fork releases the
// worker (rule 8); a claim after a park, a crash or a retry re-enters the fork and runs
// only the branches that are not done (rule 9).

// branchEnd is how one branch's walk ended: done (the join was reached), paused (parked on
// a question) or, otherwise, with an outcome (a failure, a limit, an interruption).
type branchEnd struct {
	w      *walk
	done   bool
	paused bool
	res    outcome
}

// forkNode runs a fork node (DESIGN 5.26 rules 3 to 9) and returns the node after its join,
// or the outcome that ends or pauses the run.
func (w *walk) forkNode(ctx context.Context, node *workflow.Node) (string, *outcome) {
	e := w.e
	region, err := w.def.Region(node.ID)
	if err != nil {
		o := failed(store.RunFailed, "fork "+node.ID+": "+err.Error())
		return "", &o
	}
	join, _ := w.def.Node(region.Join)
	// A fork visit that did not pass its join is re-entered (rule 9); a succeeded one, or
	// none, is a fresh entry (rule 3), counted toward max_loop_visits.
	latest, err := store.LatestNodeVisit(ctx, e.deps.Pool, w.run.ID, node.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		o := failed(store.RunFailed, "fork "+node.ID+": "+err.Error())
		return "", &o
	}
	reentry := err == nil && latest.Status != store.NodeSucceeded
	var fv *store.RunNode
	var heads []string
	now := e.clock.Now()
	if reentry {
		w.refresh(ctx)
		if viol := checkLimits(limitState{run: w.run, limits: w.run.Limits, now: now}); viol != nil {
			o := violated(viol)
			return "", &o
		}
		w.setCurrent(ctx, node.ID)
		if fv, err = store.ReopenRunNode(ctx, e.deps.Pool, latest.ID); err != nil {
			o := failed(store.RunFailed, "fork "+node.ID+": reopen: "+err.Error())
			return "", &o
		}
		e.emit(ctx, w.run.ID, "node.status", map[string]any{"node": fv})
		e.logEvent(ctx, w.run.ID, "info", "fork "+node.ID+": resumed", "node_id", node.ID)
	} else {
		if res := w.enter(ctx, node); res != nil {
			return "", res
		}
		var state any
		_ = json.Unmarshal(w.run.State, &state)
		for _, k := range region.EdgeIndexes {
			if edge := w.def.Edges[k]; workflow.Evaluate(edge.When, state) {
				heads = append(heads, edge.To)
			}
		}
		if len(heads) == 0 {
			o := failed(store.RunFailed, "no matching edge from "+node.ID)
			o.limit = true
			return "", &o
		}
		prior, err := store.CountNodeVisits(ctx, e.deps.Pool, w.run.ID, node.ID)
		if err != nil {
			o := failed(store.RunFailed, "count node visits: "+err.Error())
			return "", &o
		}
		out, _ := json.Marshal(map[string]any{"branches": heads})
		fv = &store.RunNode{RunID: w.run.ID, NodeID: node.ID, Visit: prior + 1, Status: store.NodeRunning, StartedAt: &now, Output: out}
		rows := make([]*store.RunBranch, 0, len(heads))
		for i, h := range heads {
			// Started a microsecond apart so the rows list in edge order.
			rows = append(rows, &store.RunBranch{RunID: w.run.ID, ForkID: node.ID, Head: h, Status: store.BranchRunning,
				CurrentNode: h, StartedAt: now.Add(time.Duration(i) * time.Microsecond)})
		}
		if err := store.CreateForkVisit(ctx, e.deps.Pool, fv, rows); err != nil {
			o := failed(store.RunFailed, "fork "+node.ID+": "+err.Error())
			return "", &o
		}
		e.emit(ctx, w.run.ID, "node.status", map[string]any{"node": fv})
		for _, b := range rows {
			e.emit(ctx, w.run.ID, "branch.status", map[string]any{"branch": b})
		}
		e.logEvent(ctx, w.run.ID, "info", fmt.Sprintf("fork %s: %d branches started: %s", node.ID, len(heads), strings.Join(heads, ", ")), "node_id", node.ID)
	}
	span := e.startSpan(ctx, w.run.ID, &w.rootSpan, "node", node.ID, now, nil,
		map[string]any{"run_node_id": fv.ID, "visit": fv.Visit, "branches": heads, "reentry": reentry})
	stale := w.stale
	w.stale = nil
	return w.runBranches(ctx, node, join, region, fv, span, stale)
}

// runBranches runs the branches of a fork visit that are not done, waits for them, and
// ends the fork visit: the join (rule 5), a failure (rule 6), a release on parked branches
// (rule 8) or an interruption. It is the fork's event loop: a branch ending while others
// run is handled at once, and the branches that can run are re-read whenever none runs.
func (w *walk) runBranches(ctx context.Context, fork, join *workflow.Node, region *workflow.ForkRegion, fv *store.RunNode, span string, stale []store.RunNode) (string, *outcome) {
	e := w.e
	type liveBranch struct {
		bw     *walk
		cancel context.CancelFunc
	}
	results := make(chan branchEnd)
	running := map[string]*liveBranch{}
	var failure *branchEnd
	stopReason := ""
	start := func(b *store.RunBranch) {
		bctx, cancel := context.WithCancel(ctx)
		bw := &walk{e: e, run: w.run, def: w.def, over: w.over, rootSpan: w.rootSpan, branch: b, region: region, parentSpan: span, stale: stale}
		stale = nil
		running[b.ID] = &liveBranch{bw: bw, cancel: cancel}
		go func() { results <- bw.runBranch(bctx) }()
	}
	drain := func() {
		for len(running) > 0 {
			r := <-results
			delete(running, r.w.branch.ID)
			w.sessions = append(w.sessions, r.w.sessions...)
		}
	}
	for {
		if len(running) == 0 && failure == nil {
			runnable, done, parked, err := w.runnableBranches(ctx, fv.ID)
			if err != nil {
				o := failed(store.RunFailed, "fork "+fork.ID+": "+err.Error())
				w.closeFork(ctx, fv, span, store.NodeFailed, &o.reason)
				return "", &o
			}
			if len(runnable) == 0 {
				if parked > 0 {
					return w.releaseFork(ctx, fork, fv, span, parked, done)
				}
				return w.joinBranches(ctx, fork, join, fv, span)
			}
			for _, b := range runnable {
				start(b)
			}
		}
		if len(running) == 0 {
			break // the failure path drained every branch
		}
		select {
		case <-ctx.Done():
			// Shutting down: the branch contexts end with ctx; their visits leave the run for
			// recovery (walk.agentNode). The fork row stays running for the re-entry.
			drain()
			e.endSpan(ctx, span, e.clock.Now(), map[string]any{"interrupted": true})
			return "", &outcome{interrupted: true}
		case r := <-results:
			delete(running, r.w.branch.ID)
			w.sessions = append(w.sessions, r.w.sessions...)
			b := r.w.branch
			switch {
			case r.res.interrupted && ctx.Err() != nil:
				drain()
				e.endSpan(ctx, span, e.clock.Now(), map[string]any{"interrupted": true})
				return "", &outcome{interrupted: true}
			case r.done:
				w.finishBranch(ctx, b, store.BranchSucceeded, nil)
				e.logEvent(ctx, w.run.ID, "info", "branch "+b.Head+" done", "node_id", fork.ID)
			case r.paused:
				// The park wrote the branch row waiting_approval and published it
				// (questionTurnEnd); the fork decides below whether the run waits.
			case failure != nil:
				// A sibling stopped by the fork (or ended on its own meanwhile): recorded as
				// the stop, cancelled, as rule 6 says.
				reason := stopReason
				if !r.res.interrupted && r.res.reason != "" && !strings.HasPrefix(r.res.reason, "stopped: ") {
					reason = r.res.reason
				}
				w.finishBranch(ctx, b, store.BranchCancelled, &reason)
			default:
				// The first branch to fail ends the fork visit (rule 6).
				failure = &r
				reason := r.res.reason
				status := store.BranchFailed
				if r.res.runStatus == store.RunCancelled {
					status = store.BranchCancelled
				}
				w.finishBranch(ctx, b, status, &reason)
				stopReason = "stopped: branch " + b.Head + " failed"
				if r.res.limit {
					stopReason = "stopped: " + reason
				}
				e.logEvent(ctx, w.run.ID, "warn", fmt.Sprintf("branch %s failed: %s; stopping %d branch(es)", b.Head, reason, len(running)), "node_id", fork.ID)
				for _, l := range running {
					l.bw.stopMu.Lock()
					l.bw.stopReason = stopReason
					l.bw.stopMu.Unlock()
					l.cancel()
				}
			}
		}
	}
	return w.failFork(ctx, fork, fv, span, *failure, stopReason)
}

// runBranch runs one branch of a fork visit from its position to the join (DESIGN 5.26
// rule 4) on a branch walk.
func (bw *walk) runBranch(ctx context.Context) branchEnd {
	cur := bw.branch.CurrentNode
	for {
		if ctx.Err() != nil {
			return branchEnd{w: bw, res: outcome{interrupted: true}}
		}
		if cur == bw.region.Join {
			return branchEnd{w: bw, done: true}
		}
		node, ok := bw.def.Node(cur)
		if !ok {
			return branchEnd{w: bw, res: failed(store.RunFailed, fmt.Sprintf("unknown node '%s'", cur))}
		}
		if node.Kind != workflow.KindAgent {
			return branchEnd{w: bw, res: failed(store.RunFailed, fmt.Sprintf("node %s: a %s node cannot run inside a branch", cur, node.Kind))}
		}
		next, res := bw.agentNode(ctx, node)
		if res != nil {
			if res.paused {
				return branchEnd{w: bw, paused: true}
			}
			return branchEnd{w: bw, res: *res}
		}
		cur = next
	}
}

// runnableBranches re-reads the fork visit's branch rows and returns the branches to run
// now (rule 9): the running ones, the parked ones whose question was decided (set back to
// running), and the failed or cancelled ones of an earlier attempt (a retry's re-entry);
// with how many are done and how many stay parked.
func (w *walk) runnableBranches(ctx context.Context, forkVisitID string) (runnable []*store.RunBranch, done, parked int, err error) {
	e := w.e
	rows, err := store.ListForkBranches(ctx, e.deps.Pool, forkVisitID)
	if err != nil {
		return nil, 0, 0, err
	}
	for i := range rows {
		b := &rows[i]
		switch b.Status {
		case store.BranchSucceeded:
			done++
		case store.BranchRunning:
			runnable = append(runnable, b)
		case store.BranchWaitingApproval:
			_, qerr := store.ContinuationQuestion(ctx, e.deps.Pool, w.run.ID, b.CurrentNode)
			switch {
			case qerr == nil:
				if err := w.reopenBranch(ctx, b); err != nil {
					return nil, 0, 0, err
				}
				runnable = append(runnable, b)
			case errors.Is(qerr, store.ErrNotFound):
				parked++
			default:
				return nil, 0, 0, qerr
			}
		default: // failed or cancelled: a retry re-enters them (rule 9)
			if err := w.reopenBranch(ctx, b); err != nil {
				return nil, 0, 0, err
			}
			runnable = append(runnable, b)
		}
	}
	return runnable, done, parked, nil
}

func (w *walk) reopenBranch(ctx context.Context, b *store.RunBranch) error {
	row, err := store.SetBranchStatus(ctx, w.e.deps.Pool, b.ID, store.BranchRunning, nil, w.e.clock.Now())
	if err != nil {
		return err
	}
	*b = *row
	w.e.emit(ctx, w.run.ID, "branch.status", map[string]any{"branch": b})
	return nil
}

// finishBranch writes a branch's terminal status and publishes it.
func (w *walk) finishBranch(ctx context.Context, b *store.RunBranch, status string, reason *string) {
	row, err := store.SetBranchStatus(ctx, w.e.deps.Pool, b.ID, status, reason, w.e.clock.Now())
	if err != nil {
		w.e.log.Warn("finish branch failed", "run_id", w.run.ID, "branch", b.Head, "error", err)
		return
	}
	*b = *row
	w.e.emit(ctx, w.run.ID, "branch.status", map[string]any{"branch": b})
}

// closeFork writes the fork visit's terminal status and ends its span.
func (w *walk) closeFork(ctx context.Context, fv *store.RunNode, span, status string, reason *string) {
	e := w.e
	now := e.clock.Now()
	if row, err := store.FinishRunNode(ctx, e.deps.Pool, fv.ID, status, reason, now); err == nil {
		*fv = *row
		e.emit(ctx, w.run.ID, "node.status", map[string]any{"node": fv})
	}
	attrs := map[string]any{"status": status}
	if reason != nil {
		attrs["reason"] = *reason
	}
	e.endSpan(ctx, span, now, attrs)
}

// joinBranches passes the join once every branch succeeded (rule 5): the join's visit row,
// the fork row closed, the first true edge of the join.
func (w *walk) joinBranches(ctx context.Context, fork, join *workflow.Node, fv *store.RunNode, span string) (string, *outcome) {
	e := w.e
	rows, err := store.ListForkBranches(ctx, e.deps.Pool, fv.ID)
	if err != nil {
		o := failed(store.RunFailed, "fork "+fork.ID+": "+err.Error())
		w.closeFork(ctx, fv, span, store.NodeFailed, &o.reason)
		return "", &o
	}
	heads := make([]string, 0, len(rows))
	for _, b := range rows {
		heads = append(heads, b.Head)
	}
	now := e.clock.Now()
	prior, err := store.CountNodeVisits(ctx, e.deps.Pool, w.run.ID, join.ID)
	if err != nil {
		o := failed(store.RunFailed, "count node visits: "+err.Error())
		w.closeFork(ctx, fv, span, store.NodeFailed, &o.reason)
		return "", &o
	}
	out, _ := json.Marshal(map[string]any{"branches": heads})
	jrow := &store.RunNode{RunID: w.run.ID, NodeID: join.ID, Visit: prior + 1, Status: store.NodeSucceeded, StartedAt: &now, FinishedAt: &now, Output: out}
	if err := store.CreateRunNode(ctx, e.deps.Pool, jrow); err != nil {
		o := failed(store.RunFailed, "join "+join.ID+": "+err.Error())
		w.closeFork(ctx, fv, span, store.NodeFailed, &o.reason)
		return "", &o
	}
	e.emit(ctx, w.run.ID, "node.status", map[string]any{"node": jrow})
	w.closeFork(ctx, fv, span, store.NodeSucceeded, nil)
	next, res := w.follow(ctx, join)
	if res != nil {
		return "", res
	}
	e.logEvent(ctx, w.run.ID, "info", fmt.Sprintf("fork %s: joined; continuing %s", fork.ID, next), "node_id", join.ID)
	return next, nil
}

// releaseFork lets the worker go while parked branches wait for their answers (rule 8):
// the run waits on the fork's visit, and a decision that landed meanwhile re-queues it at
// once; the approval sweep repeats that (requeueParkedDecided).
func (w *walk) releaseFork(ctx context.Context, fork *workflow.Node, fv *store.RunNode, span string, parked, done int) (string, *outcome) {
	e := w.e
	now := e.clock.Now()
	run, err := store.PauseRunAt(ctx, e.deps.Pool, w.run.ID, fv.ID, now)
	if err != nil {
		o := failed(store.RunFailed, "fork "+fork.ID+": pause: "+err.Error())
		w.closeFork(ctx, fv, span, store.NodeFailed, &o.reason)
		return "", &o
	}
	w.run = run
	e.emit(ctx, run.ID, "run.status", map[string]any{"run": run})
	e.logEvent(ctx, run.ID, "info", fmt.Sprintf("fork %s: waiting for the answer to %d question(s); %d branch(es) done", fork.ID, parked, done), "node_id", fork.ID)
	e.endSpan(ctx, span, now, map[string]any{"status": store.RunWaitingApproval, "parked": parked})
	// A decision that landed between the last branch's end and the pause (its delivery found
	// the run still running) re-queues the run now.
	rows, err := store.ListForkBranches(ctx, e.deps.Pool, fv.ID)
	if err == nil {
		for _, b := range rows {
			if b.Status != store.BranchWaitingApproval {
				continue
			}
			if _, qerr := store.ContinuationQuestion(ctx, e.deps.Pool, run.ID, b.CurrentNode); qerr == nil {
				if requeued, rerr := store.RequeuePausedRunAt(ctx, e.deps.Pool, run.ID, e.clock.Now()); rerr == nil {
					w.run = requeued
					e.emit(ctx, run.ID, "run.status", map[string]any{"run": requeued})
					e.logEvent(ctx, run.ID, "info", "question answered; continuing branch "+b.Head, "node_id", fork.ID)
				}
				break
			}
		}
	}
	o := w.applyPendingCancel(ctx, w.run)
	return "", &o
}

// failFork ends a fork visit whose branch failed (rule 6): the branch rows still open are
// cancelled with the stop reason and their open visits closed, the fork row fails, and the
// run's outcome is the failing branch's own for a limit or a cancellation, else
// `branch <head> failed: <reason>`.
func (w *walk) failFork(ctx context.Context, fork *workflow.Node, fv *store.RunNode, span string, failure branchEnd, stopReason string) (string, *outcome) {
	e := w.e
	now := e.clock.Now()
	reason := stopReason
	if _, err := store.CloseOpenBranches(ctx, e.deps.Pool, w.run.ID, store.BranchCancelled, &reason, now); err != nil {
		e.log.Warn("close branches failed", "run_id", w.run.ID, "error", err)
	}
	if rows, err := store.ListForkBranches(ctx, e.deps.Pool, fv.ID); err == nil {
		for i := range rows {
			b := &rows[i]
			if b.Status == store.BranchCancelled {
				if _, err := store.CloseBranchVisits(ctx, e.deps.Pool, b.ID, reason, now); err != nil {
					e.log.Warn("close branch visits failed", "run_id", w.run.ID, "branch", b.Head, "error", err)
				}
				e.emit(ctx, w.run.ID, "branch.status", map[string]any{"branch": b})
			}
		}
	}
	o := failure.res
	if !o.limit {
		o = failed(store.RunFailed, "branch "+failure.w.branch.Head+" failed: "+failure.res.reason)
	}
	w.closeFork(ctx, fv, span, store.NodeFailed, &o.reason)
	return "", &o
}

// closeBranches closes the open branch rows of a run that ended (DESIGN 5.26 rule 12),
// cancelled for a cancelled run, failed otherwise, with the run's reason.
func (e *Engine) closeBranches(ctx context.Context, run *store.Run, reason string) {
	status := store.BranchFailed
	if run.Status == store.RunCancelled {
		status = store.BranchCancelled
	}
	var r *string
	if reason != "" {
		r = &reason
	}
	n, err := store.CloseOpenBranches(ctx, e.deps.Pool, run.ID, status, r, e.clock.Now())
	if err != nil {
		e.log.Warn("close branches failed", "run_id", run.ID, "error", err)
		return
	}
	if n == 0 {
		return
	}
	if rows, err := store.ListRunBranches(ctx, e.deps.Pool, run.ID); err == nil {
		for i := range rows {
			if rows[i].Status == status {
				e.emit(ctx, run.ID, "branch.status", map[string]any{"branch": &rows[i]})
			}
		}
	}
}
