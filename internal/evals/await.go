package evals

import (
	"context"
	"fmt"
	"slices"
	"time"

	"yuno/internal/store"
)

// awaitOutcome is how the wait for a run ended.
type awaitOutcome struct {
	// last is the last run the runner saw.
	last *store.Run
	// timedOut is true when the case deadline passed before the run was terminal.
	timedOut bool
	// stalled describes an approval that is pending with no action left to apply; the
	// run would wait for a human forever, so the runner stops waiting.
	stalled string
}

// approvalState is the runner's progress through a case's approval actions.
type approvalState struct {
	next      int             // index of the next action to apply
	handled   map[string]bool // approvals already acted on
	holdUntil time.Time       // actions are not applied before this moment
}

// await polls the run until it is terminal, the case times out or an approval stalls.
// With actions.cancel_after_steps it requests cancellation once step_count reaches that
// value. While the run waits for approval it applies the case's approval actions to the
// pending approvals, in order. It returns an error only when polling keeps failing or
// ctx ends; out.last is always the last run seen.
func (r *Runner) await(ctx context.Context, c Case, run *store.Run) (out awaitOutcome, err error) {
	out.last = run
	deadline := time.NewTimer(r.caseTimeout)
	defer deadline.Stop()

	cancelled := false
	failures := 0
	approvals := approvalState{handled: map[string]bool{}}
	for {
		got, err := r.api.GetRun(ctx, run.ID)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			failures++
			r.log.Warn("eval poll failed", "case", c.ID, "run", run.ID, "error", err, "consecutive", failures)
			if failures >= maxPollFailures {
				return out, fmt.Errorf("poll run: %w", err)
			}
		default:
			failures = 0
			out.last = got
			if store.IsTerminal(got.Status) {
				return out, nil
			}
			if n := c.Actions.CancelAfterSteps; n > 0 && !cancelled && got.StepCount >= n {
				if err := r.api.CancelRun(ctx, run.ID); err != nil {
					r.log.Warn("eval cancel failed", "case", c.ID, "run", run.ID, "error", err)
				} else {
					cancelled = true
					r.log.Info("eval cancel requested", "case", c.ID, "run", run.ID, "step_count", got.StepCount)
				}
			}
			if got.Status == store.RunWaitingApproval {
				if c.Actions.CancelWhenWaiting {
					// The case is about the cancel, not the decision: the approval is left
					// for the engine to expire with the run.
					if !cancelled {
						if err := r.api.CancelRun(ctx, run.ID); err != nil {
							r.log.Warn("eval cancel failed", "case", c.ID, "run", run.ID, "error", err)
						} else {
							cancelled = true
							r.log.Info("eval cancel requested while waiting for approval", "case", c.ID, "run", run.ID)
						}
					}
				} else if msg := r.applyApprovals(ctx, c, run.ID, &approvals); msg != "" {
					out.stalled = msg
					return out, nil
				}
			}
		}

		wait := time.NewTimer(r.poll)
		select {
		case <-ctx.Done():
			wait.Stop()
			return out, ctx.Err()
		case <-deadline.C:
			wait.Stop()
			out.timedOut = true
			return out, nil
		case <-wait.C:
		}
	}
}

// applyApprovals applies the case's next approval action to each pending approval of the
// run that it has not acted on yet. The first pending approval starts the
// actions.approval_wait_seconds hold, during which nothing is decided. It returns a
// description of a pending approval that has no action left, "" otherwise. A failed
// decision is retried on the next poll; a 409 means somebody else (an expiry) decided
// first, which uses up the action.
func (r *Runner) applyApprovals(ctx context.Context, c Case, runID string, st *approvalState) string {
	pending, err := r.api.ListApprovals(ctx, runID, store.ApprovalPending)
	if err != nil {
		r.log.Warn("eval approvals list failed", "case", c.ID, "run", runID, "error", err)
		return ""
	}
	slices.SortStableFunc(pending, func(a, b ApprovalItem) int { return a.RequestedAt.Compare(b.RequestedAt) })
	for _, ap := range pending {
		if st.handled[ap.ID] {
			continue
		}
		now := r.now()
		if st.holdUntil.IsZero() && c.Actions.ApprovalWaitSeconds > 0 {
			st.holdUntil = now.Add(time.Duration(c.Actions.ApprovalWaitSeconds) * time.Second)
			r.log.Info("eval holding approval", "case", c.ID, "run", runID, "approval", ap.ID, "seconds", c.Actions.ApprovalWaitSeconds)
		}
		if now.Before(st.holdUntil) {
			return ""
		}
		if st.next >= len(c.Actions.Approvals) {
			return fmt.Sprintf("approval %s (%s) is pending and the case has no action left for it", ap.ID, describeApproval(ap))
		}
		act := c.Actions.Approvals[st.next]
		_, err := r.api.DecideApproval(ctx, ap.ID, act.Decision, act.Reason)
		switch {
		case err == nil:
			r.log.Info("eval approval decided", "case", c.ID, "run", runID, "approval", ap.ID, "decision", act.Decision)
		case statusOf(err) == 409:
			r.log.Warn("eval approval was no longer pending", "case", c.ID, "run", runID, "approval", ap.ID)
		default:
			r.log.Warn("eval approval decision failed", "case", c.ID, "run", runID, "approval", ap.ID, "error", err)
			return ""
		}
		st.handled[ap.ID] = true
		st.next++
	}
	return ""
}

// describeApproval names what an approval gates, for messages.
func describeApproval(ap ApprovalItem) string {
	if ap.ToolCall != nil {
		return "tool " + toolBase(ap.ToolCall.ToolName) + " at node " + ap.NodeID
	}
	return "node " + ap.NodeID
}
