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
		if rows, err := store.ListForkBranches(ctx, e.deps.Pool, fv.ID); err == nil {
			for _, b := range rows {
				heads = append(heads, b.Head)
			}
		}
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
	wrapped := false // the run's reason is `branch <head> failed: <reason>` (rule 6)
	finished := func(id string) {
		if l := running[id]; l != nil {
			l.cancel()
		}
		delete(running, id)
	}
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
			finished(r.w.branch.ID)
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
			finished(r.w.branch.ID)
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
				// A sibling stopped by the fork is recorded cancelled with the stop reason;
				// one that ended on its own meanwhile is recorded as it ended (rule 6).
				if !r.res.interrupted && r.res.reason != "" && !strings.HasPrefix(r.res.reason, "stopped: ") {
					reason := r.res.reason
					w.finishBranch(ctx, b, branchStatusOf(r.res), &reason)
					break
				}
				reason := stopReason
				w.finishBranch(ctx, b, store.BranchCancelled, &reason)
			default:
				// The first branch to fail ends the fork visit (rule 6).
				failure = &r
				reason := r.res.reason
				w.finishBranch(ctx, b, branchStatusOf(r.res), &reason)
				// A limit the run shares (steps, cost, tool calls, time, a cancel, the loop
				// limit) is the run's own reason; a node's failure, a node with no true edge
				// and the per-visit iteration limit are the branch's (rules 4 and 6).
				wrapped = !r.res.limit || strings.HasPrefix(reason, "no matching edge from ") || strings.HasPrefix(reason, "node iteration limit ")
				stopReason = "stopped: " + reason
				if wrapped {
					stopReason = "stopped: branch " + b.Head + " failed"
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
	return w.failFork(ctx, fork, fv, span, *failure, stopReason, wrapped)
}

// branchStatusOf is the branch row's status for an outcome that ended it: cancelled for a
// cancelled run, failed otherwise.
func branchStatusOf(res outcome) string {
	if res.runStatus == store.RunCancelled {
		return store.BranchCancelled
	}
	return store.BranchFailed
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
				// The decision landed while a sibling ran (rule 8): the parked visit is closed
				// here, as RequeueParkedRun closes it on the other path.
				if err := w.closeParkedVisit(ctx, b); err != nil {
					return nil, 0, 0, err
				}
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

// closeParkedVisit closes a parked branch visit succeeded without output once its
// question was decided and the branch continues in this walk (DESIGN 5.24 rule 5 (c)).
func (w *walk) closeParkedVisit(ctx context.Context, b *store.RunBranch) error {
	e := w.e
	parked, err := store.ParkedVisitOfBranch(ctx, e.deps.Pool, b.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	row, err := store.FinishRunNode(ctx, e.deps.Pool, parked.ID, store.NodeSucceeded, nil, e.clock.Now())
	if err != nil {
		return err
	}
	e.emit(ctx, w.run.ID, "node.status", map[string]any{"node": row})
	return nil
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
	} else {
		e.log.Warn("close fork visit failed", "run_id", w.run.ID, "node_id", fv.NodeID, "error", err)
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
	// The run's position moves before the fork row closes: a crash between the two leaves a
	// fork row the next claim marks lost, never a fork a claim would enter afresh (review L1).
	next, res := w.follow(ctx, join)
	if res != nil {
		w.closeFork(ctx, fv, span, store.NodeFailed, &res.reason)
		return "", res
	}
	w.closeFork(ctx, fv, span, store.NodeSucceeded, nil)
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
			q, qerr := store.ContinuationQuestion(ctx, e.deps.Pool, run.ID, b.CurrentNode)
			if qerr != nil {
				continue
			}
			// The same re-queue a decision makes (rule 8): it closes the parked visit, sets the
			// branch back to running and queues the run, all in one statement.
			if e.requeueParked(ctx, q) {
				if fresh, err := e.readRun(ctx, run.ID); err == nil {
					w.run = fresh
				}
			}
			break
		}
	}
	o := w.applyPendingCancel(ctx, w.run)
	return "", &o
}

// failFork ends a fork visit whose branch failed (rule 6): the branch rows still open are
// cancelled with the stop reason and their open visits closed, the fork row fails, and the
// run's outcome is the failing branch's own for a limit or a cancellation, else
// `branch <head> failed: <reason>`.
func (w *walk) failFork(ctx context.Context, fork *workflow.Node, fv *store.RunNode, span string, failure branchEnd, stopReason string, wrapped bool) (string, *outcome) {
	e := w.e
	now := e.clock.Now()
	reason := stopReason
	if _, err := store.CloseOpenBranches(ctx, e.deps.Pool, w.run.ID, store.BranchCancelled, &reason, now); err != nil {
		e.log.Warn("close branches failed", "run_id", w.run.ID, "error", err)
	}
	if rows, err := store.ListForkBranches(ctx, e.deps.Pool, fv.ID); err == nil {
		closed := map[string]bool{}
		for i := range rows {
			b := &rows[i]
			if b.Status == store.BranchCancelled {
				if n, err := store.CloseBranchVisits(ctx, e.deps.Pool, b.ID, reason, now); err != nil {
					e.log.Warn("close branch visits failed", "run_id", w.run.ID, "branch", b.Head, "error", err)
				} else if n > 0 {
					closed[b.ID] = true
				}
				e.emit(ctx, w.run.ID, "branch.status", map[string]any{"branch": b})
			}
		}
		if len(closed) > 0 {
			// The visits a stop closed (a parked one among them) are published too.
			if nodes, err := store.ListRunNodes(ctx, e.deps.Pool, w.run.ID); err == nil {
				for i := range nodes {
					n := &nodes[i]
					if n.BranchID != nil && closed[*n.BranchID] && n.FinishedAt != nil && n.FinishedAt.Equal(now) {
						e.emit(ctx, w.run.ID, "node.status", map[string]any{"node": n})
					}
				}
			}
		}
	}
	o := failure.res
	if wrapped {
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
