package engine

import (
	"context"
	"errors"
	"fmt"

	"yuno/internal/store"
)

// Escalation kinds the escalate tool takes (DESIGN 5.29 rule 6).
var escalationKinds = map[string]bool{"prd_gap": true, "design_wrong": true, "loop_limit": true, "other": true}

// ErrRunEnded is EscalateRun's answer for a run that is terminal already.
var ErrRunEnded = errors.New("the run has ended already")

// EscalateReason is the reason an escalated run fails with.
func EscalateReason(kind, reason string) string {
	return fmt.Sprintf("escalated (%s): %s", kind, reason)
}

// EscalateRun ends a run failed with "escalated (<kind>): <reason>" at once (DESIGN 5.29
// rule 6): a live visit aborts its turn as a cancel does, so the agent needs no finish; a
// run with no live visit (paused at an approval, queued, or running with no visit left) is
// failed in place. The plan side (the ticket failed, the waves paused, the event and the
// notice) follows from the run's end, which reads the kind from the reason
// (plans.Service.OnRunTerminal): it is applied only once the run is failed, never for a run
// that could not be ended (review M4). ErrRunEnded when nothing could be ended.
func (e *Engine) EscalateRun(ctx context.Context, runID, kind, reason string) error {
	if !escalationKinds[kind] {
		return fmt.Errorf("unknown escalation kind %q", kind)
	}
	run, err := store.GetRun(ctx, e.deps.Pool, runID)
	if err != nil {
		return fmt.Errorf("run %s: %w", runID, err)
	}
	if store.IsTerminal(run.Status) {
		return ErrRunEnded
	}
	e.logEvent(ctx, runID, "warn", "escalated ("+kind+")")
	if !e.failRunFrom(context.WithoutCancel(ctx), runID, EscalateReason(kind, reason),
		store.RunWaitingApproval, store.RunQueued, store.RunRunning) {
		return ErrRunEnded
	}
	return nil
}
