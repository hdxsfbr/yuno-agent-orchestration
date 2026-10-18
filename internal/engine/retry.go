package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"yuno/internal/guardrails"
	"yuno/internal/project"
	"yuno/internal/store"
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
		if details := exhaustedLimits(run, under); len(details) > 0 {
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
		// Rule 3: the one conditional update.
		retried, err = store.RetryRun(ctx, tx, runID, applied, now)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// The row is locked, so this is not expected; answer as a lost race.
				return &RetryError{Code: RetryNotRetryable, Message: "run is " + run.Status}
			}
			return fmt.Errorf("run %s: %w", runID, err)
		}
		from = *run.CurrentNode
		return nil
	})
	if err != nil {
		return nil, err
	}
	// After the commit; a failed event insert is logged by emit and the run stays queued
	// (Failure handling). The payload carries `at` because the SSE data does not.
	e.emit(ctx, runID, "run.retry", map[string]any{"retries": retried.Retries, "from_node": from, "by": by,
		"limits": applied, "at": now})
	e.emit(ctx, runID, "run.status", map[string]any{"run": retried})
	e.logEvent(ctx, runID, "info", fmt.Sprintf("retry %d: continuing from %s", retried.Retries, from),
		"node_id", from, "by", by)
	return retried, nil
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

// exhaustedLimits lists, in the order max_steps, max_tool_calls, max_cost_usd, every
// counted limit whose counter has reached its cap (>=, as checkLimits stops the run),
// as "<limit>: <used> of <cap> used" (DESIGN 5.21 rule 2). A cap of 0 is no cap.
func exhaustedLimits(run *store.Run, l store.Limits) []string {
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
	return out
}
