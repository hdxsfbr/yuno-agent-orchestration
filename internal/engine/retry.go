package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"yuno/internal/guardrails"
	"yuno/internal/obs"
	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// Retry from the failed step (DESIGN 5.21, decision D-043). An operator re-queues a run
// that ended failed, timed_out or cancelled; it continues at the node it stopped at on
// the engine's resumed path (run.go: current_node set, unfinished visits failed, a new
// root span), keeping its state, its counters and the work its agents did.

// maxRetries is DESIGN 5.21 rule 1's cap: the sixth retry is refused.
const maxRetries = 5

// Codes of a RetryError, the API's 409 error codes (DESIGN 5.21 rules 1 and 2).
const (
	RetryNotRetryable    = "run_not_retryable"
	RetryLimitReached    = "retry_limit"
	RetryLimitsExhausted = "limits_exhausted"
)

// retryableStatuses are the statuses a run is retried from. DESIGN 5.1 rule 2 gains the
// transition `failed | timed_out | cancelled → queued` "for an operator retry only":
// the table behind ValidTransition (limits.go) stays as it was, so no other path can
// take it, and this list is the retry path's own.
var retryableStatuses = []string{store.RunFailed, store.RunTimedOut, store.RunCancelled}

// RetryError is a refusal of Retry (DESIGN 5.21 rules 1 and 2): Code is the API's
// error code (run_not_retryable, retry_limit, limits_exhausted), Message the sentence
// for the operator, Details one "<limit>: <used> of <cap> used" line per exhausted
// limit for limits_exhausted.
type RetryError struct {
	Code    string
	Message string
	Details []string
}

func (e *RetryError) Error() string { return e.Message }

// Retry re-queues a failed, timed-out or cancelled run from the node it stopped at
// (DESIGN 5.21). The checks and the update are one transaction that holds the run row
// (rule 3, Consistency), so of two concurrent retries the second finds the run queued
// and is refused with run_not_retryable. limits, when given, are resolved as at trigger
// time (5.2) and replace the run's limits before the exhausted-limit check (rule 2).
// A refusal is a *RetryError, a bad limit a *ValidationError, an unknown run wraps
// store.ErrNotFound; nothing changes in any of them.
func (e *Engine) Retry(ctx context.Context, runID, by string, limits *store.Limits) (*store.Run, error) {
	if by == "" {
		by = "operator"
	}
	now := e.clock.Now()
	var retried *store.Run
	var from string
	var applied *store.Limits
	var ticketPlan, ticketProject string
	err := pgx.BeginFunc(ctx, e.deps.Pool, func(tx pgx.Tx) error {
		run, err := store.GetRunForUpdate(ctx, tx, runID)
		if err != nil {
			return fmt.Errorf("run %s: %w", runID, err)
		}
		// Rule 1: where a retry can start, and how often.
		if !slices.Contains(retryableStatuses, run.Status) {
			return &RetryError{Code: RetryNotRetryable, Message: "run is " + run.Status}
		}
		if run.CurrentNode == nil || *run.CurrentNode == "" {
			return &RetryError{Code: RetryNotRetryable, Message: "run has no step to retry from"}
		}
		if run.Retries >= maxRetries {
			return &RetryError{Code: RetryLimitReached, Message: fmt.Sprintf("run was retried %d times", maxRetries)}
		}
		// Rule 2: the limits that will apply, and the counters against them.
		under := run.Limits
		if limits != nil {
			resolved, err := e.retryLimits(ctx, tx, run, *limits)
			if err != nil {
				return err
			}
			applied, under = &resolved, resolved
		}
		// The visits of the node the walk resumes at count against max_loop_visits as its
		// entry check counts them (walk.enter, >=), or the run would fail again at once; a
		// continuation visit does not count (DESIGN 5.24 rule 5). The entry check lets a
		// visit in that would continue an answered question whatever the count, so a
		// retry of a run with such a question counts nothing either (review L13).
		visits := 0
		if under.MaxLoopVisits > 0 && resumesAtFork(run) {
			// The fork is re-entered, not counted (DESIGN 5.26 rule 9); its unfinished
			// branches run again from their positions, and those visits are counted at
			// their nodes' entry, so the most visited of them is what the check compares.
			if visits, err = forkRetryVisits(ctx, tx, run); err != nil {
				return fmt.Errorf("run %s: count branch visits: %w", runID, err)
			}
		} else if under.MaxLoopVisits > 0 {
			switch _, cerr := store.ContinuationQuestion(ctx, tx, runID, *run.CurrentNode); {
			case cerr == nil:
				// The next visit is a continuation: free of the limit.
			case errors.Is(cerr, store.ErrNotFound):
				if visits, err = store.CountNodeLoopVisits(ctx, tx, runID, *run.CurrentNode); err != nil {
					return fmt.Errorf("run %s: count visits of %s: %w", runID, *run.CurrentNode, err)
				}
			default:
				return fmt.Errorf("run %s: continuation question of %s: %w", runID, *run.CurrentNode, cerr)
			}
		}
		if details := exhaustedLimits(run, under, visits); len(details) > 0 {
			names := make([]string, len(details))
			for i, d := range details {
				names[i], _, _ = strings.Cut(d, ":")
			}
			return &RetryError{Code: RetryLimitsExhausted, Message: "limits exhausted: " + strings.Join(names, ", "), Details: details}
		}
		// DESIGN 5.19 rule 3: a retry is a trigger for the project's budget. A deleted
		// project cannot be over budget: the run still serves its snapshot (5.21 edge cases).
		if run.ProjectID != nil {
			proj, err := store.GetProject(ctx, tx, *run.ProjectID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if err == nil {
				if err := project.CheckBudget(ctx, tx, proj, now); err != nil {
					return err
				}
			}
		}
		// Rule 3: the one conditional update. The two live-run indexes can refuse it
		// (DESIGN 5.22 rule 1, 5.23 rule 3); the refusal names the run that holds the
		// request or the issue.
		if err := e.checkOpenedRequestIdle(ctx, tx, run); err != nil {
			return err
		}
		if err := e.checkNamedRequestOpenerIdle(ctx, tx, run); err != nil {
			return err
		}
		retried, err = e.retryRow(ctx, tx, run, applied, now)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// The row is locked, so this is not expected; answer as a lost race.
				return &RetryError{Code: RetryNotRetryable, Message: "run is " + run.Status}
			}
			var re *RetryError
			if errors.As(err, &re) {
				return err
			}
			return fmt.Errorf("run %s: %w", runID, err)
		}
		from = *run.CurrentNode
		// DESIGN 5.29 rule 7 (review H2): a plan ticket the run's failure marked failed is
		// building again (in review with a merge request), in this transaction so the run's
		// next failure finds it so and escalates again.
		if e.deps.Plans != nil {
			if ticketPlan, ticketProject, err = store.RetryTicketOfRun(ctx, tx, runID); err != nil {
				return fmt.Errorf("run %s: move its plan ticket: %w", runID, err)
			}
		}
		// Rule 4: the retried claim opens a root span of its own. A run the stale-run
		// sweeper failed ("worker lost") never ended its root span, and the resumed path
		// would continue it without the retry attribute: end it here, in the same
		// transaction, so no claim can find it open.
		return endOpenRootSpan(ctx, tx, runID, run.Status, now)
	})
	if err != nil {
		return nil, err
	}
	// After the commit; a failed event insert is logged by emit and the run stays queued
	// (Failure handling). The payload carries `at` because the SSE data does not.
	if ticketPlan != "" {
		e.deps.Plans.OnRunRetried(ticketPlan, ticketProject)
	}
	e.emit(ctx, runID, "run.retry", map[string]any{"retries": retried.Retries, "from_node": from, "by": by,
		"limits": applied, "at": now})
	e.emit(ctx, runID, "run.status", map[string]any{"run": retried})
	e.logEvent(ctx, runID, "info", fmt.Sprintf("retry %d: continuing from %s", retried.Retries, from),
		"node_id", from, "by", by)
	return retried, nil
}

// resumesAtFork reports whether the run's current node is a fork of its snapshot: the
// retry re-enters the fork visit (DESIGN 5.26 rule 9), which the loop count does not see.
func resumesAtFork(run *store.Run) bool {
	if run.CurrentNode == nil {
		return false
	}
	var snap runSnapshot
	if json.Unmarshal(run.WorkflowSnapshot, &snap) != nil {
		return false
	}
	n, ok := snap.Definition.Node(*run.CurrentNode)
	return ok && n.Kind == workflow.KindFork
}

// forkRetryVisits is the largest loop-visit count among the positions of the branches the
// fork's latest visit will run again (every branch not succeeded).
func forkRetryVisits(ctx context.Context, q store.Querier, run *store.Run) (int, error) {
	latest, err := store.LatestNodeVisit(ctx, q, run.ID, *run.CurrentNode)
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	rows, err := store.ListForkBranches(ctx, q, latest.ID)
	if err != nil {
		return 0, err
	}
	most := 0
	for _, b := range rows {
		if b.Status == store.BranchSucceeded {
			continue
		}
		if _, cerr := store.ContinuationQuestion(ctx, q, run.ID, b.CurrentNode); cerr == nil {
			continue // a continuation visit is free of the limit (5.24 rule 5)
		}
		n, err := store.CountNodeLoopVisits(ctx, q, run.ID, b.CurrentNode)
		if err != nil {
			return 0, err
		}
		most = max(most, n)
	}
	return most, nil
}

// retryRow is store.RetryRun with the answers of the two live-run indexes turned into
// refusals (DESIGN 5.21 rule 3 amended): another live run names the same merge request
// (store.ErrMergeRequestBusy, "<merge request !n|pull request #n> is being updated by run
// <id>", 5.22 rule 1), or has the same issue (store.ErrIssueBusy, "issue #<n> already
// has a running run <id>", 5.23 rule 6), both 409 run_not_retryable. The update runs under
// a savepoint, because a violated index aborts the statement's transaction and the run
// that holds the request has to be read in it afterwards; a run that ended meanwhile
// leaves nothing to name, and the update goes again.
func (e *Engine) retryRow(ctx context.Context, tx pgx.Tx, run *store.Run, limits *store.Limits, now time.Time) (*store.Run, error) {
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return nil, err
		}
		retried, err := store.RetryRun(ctx, sp, run.ID, limits, now)
		if err == nil {
			return retried, sp.Commit(ctx)
		}
		if rbErr := sp.Rollback(ctx); rbErr != nil {
			return nil, rbErr
		}
		last = err
		var msg string
		switch {
		case errors.Is(err, store.ErrMergeRequestBusy) && run.MergeRequestID != nil:
			live, lerr := store.LiveRunForMergeRequest(ctx, tx, *run.MergeRequestID)
			if lerr == nil {
				mr, merr := store.GetMergeRequest(ctx, tx, *run.MergeRequestID)
				if merr != nil {
					return nil, merr
				}
				msg = mr.Label() + " is being updated by run " + live.ID
			} else if !errors.Is(lerr, store.ErrNotFound) {
				return nil, lerr
			}
		case errors.Is(err, store.ErrIssueBusy) && run.TriggerRef != nil:
			live, lerr := store.LiveIssueRun(ctx, tx, *run.TriggerRef)
			if lerr == nil {
				msg = "issue #" + (*run.TriggerRef)[strings.LastIndex(*run.TriggerRef, "#")+1:] + " already has a running run " + live.ID
			} else if !errors.Is(lerr, store.ErrNotFound) {
				return nil, lerr
			}
		default:
			return nil, err
		}
		if msg != "" {
			return nil, &RetryError{Code: RetryNotRetryable, Message: msg}
		}
	}
	return nil, last
}

// checkOpenedRequestIdle refuses the retry of a run that opened a merge request while
// another live run updates it (DESIGN 5.22 edge cases, rule 1): the opening run names no
// request, so the live-run index does not see its retry, yet its next open_merge_request
// would push onto the branch the other run works on.
func (e *Engine) checkOpenedRequestIdle(ctx context.Context, tx pgx.Tx, run *store.Run) error {
	if run.MergeRequestID != nil {
		return nil // the index covers a run that names one
	}
	opened, err := store.MergeRequestByRun(ctx, tx, run.ID)
	if err != nil {
		return fmt.Errorf("run %s: read its merge request: %w", run.ID, err)
	}
	if opened == nil {
		return nil
	}
	live, err := store.LiveRunForMergeRequest(ctx, tx, opened.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("run %s: %w", run.ID, err)
	}
	return &RetryError{Code: RetryNotRetryable, Message: opened.Label() + " is being updated by run " + live.ID}
}

// checkNamedRequestOpenerIdle is the other side of checkOpenedRequestIdle (DESIGN 5.22
// rule 1, review L3): the retry of a run that names a merge request refuses while the run
// that opened the request is live, since the opening run may still push to the branch the
// retried run would push onto. The live-run index sees only runs that name the record, so
// the opener is read through the record. A record that is gone, or opened by the run
// itself, leaves nothing to refuse.
func (e *Engine) checkNamedRequestOpenerIdle(ctx context.Context, tx pgx.Tx, run *store.Run) error {
	if run.MergeRequestID == nil {
		return nil
	}
	mr, err := store.GetMergeRequest(ctx, tx, *run.MergeRequestID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("run %s: read its merge request: %w", run.ID, err)
	}
	if mr.RunID == run.ID {
		return nil
	}
	opener, err := store.GetRun(ctx, tx, mr.RunID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("run %s: run %s: %w", run.ID, mr.RunID, err)
	}
	if store.IsTerminal(opener.Status) {
		return nil
	}
	return &RetryError{Code: RetryNotRetryable, Message: mr.Label() + " is being updated by run " + opener.ID}
}

// endOpenRootSpan ends the run's open root span, if it has one, marked as retried with
// the status the run ended in (DESIGN 5.21 rule 4).
func endOpenRootSpan(ctx context.Context, q store.Querier, runID, status string, at time.Time) error {
	span, err := store.OpenRootSpan(ctx, q, runID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("run %s: open root span: %w", runID, err)
	}
	attrs, _ := json.Marshal(map[string]any{"status": status, "retried": true})
	if err := store.EndSpanAt(ctx, q, span, at, obs.RedactJSON(attrs)); err != nil {
		return fmt.Errorf("run %s: end root span: %w", runID, err)
	}
	return nil
}

// retryLimits resolves the operator's limits as a trigger does (DESIGN 5.2: negative
// values refused, platform clamp) over the run's own limits, so a limit the body leaves
// out keeps the run's value, including one an earlier retry raised; then the
// participating agents' per-run cost caps lower the cost limit again (5.11 rule 1), so
// a retry cannot lift a guardrail.
func (e *Engine) retryLimits(ctx context.Context, q store.Querier, run *store.Run, req store.Limits) (store.Limits, error) {
	l, err := resolveLayeredLimits("run limits.", req, run.Limits, e.deps.Cfg)
	if err != nil {
		return store.Limits{}, err
	}
	var snap runSnapshot
	if json.Unmarshal(run.WorkflowSnapshot, &snap) != nil {
		return l, nil // a run with a snapshot nobody can read has no current_node either
	}
	agents, err := guardrails.ParticipatingAgents(ctx, q, &snap.Definition)
	if err != nil {
		return store.Limits{}, err
	}
	return guardrails.EffectiveLimits(l, agents, e.deps.Cfg.MaxCostUSD), nil
}

// exhaustedLimits lists, in the order max_steps, max_tool_calls, max_cost_usd,
// max_loop_visits, every counted limit whose counter has reached its cap (>=, as
// checkLimits stops the run and walk.enter refuses a visit), as "<limit>: <used> of <cap>
// used" (DESIGN 5.21 rule 2). visits is the number of visits the run's current node has
// had. A cap of 0 is no cap.
func exhaustedLimits(run *store.Run, l store.Limits, visits int) []string {
	var out []string
	if l.MaxSteps > 0 && run.StepCount >= l.MaxSteps {
		out = append(out, fmt.Sprintf("max_steps: %d of %d used", run.StepCount, l.MaxSteps))
	}
	if l.MaxToolCalls > 0 && run.ToolCallCount >= l.MaxToolCalls {
		out = append(out, fmt.Sprintf("max_tool_calls: %d of %d used", run.ToolCallCount, l.MaxToolCalls))
	}
	if l.MaxCostUSD > 0 && run.CostUSDMicros >= int64(l.MaxCostUSD*1e6+0.5) {
		out = append(out, fmt.Sprintf("max_cost_usd: $%s of $%s used", formatUSD(float64(run.CostUSDMicros)/1e6), formatUSD(l.MaxCostUSD)))
	}
	if l.MaxLoopVisits > 0 && visits >= l.MaxLoopVisits {
		out = append(out, fmt.Sprintf("max_loop_visits: %d of %d used", visits, l.MaxLoopVisits))
	}
	return out
}
