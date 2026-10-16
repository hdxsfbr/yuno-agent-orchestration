// Package guardrails enforces an agent's guardrails (DESIGN 5.11): the per-run cost
// cap and the hourly run rate at trigger time, blocked tools and blocked input
// patterns in the MCP gate, and the tool lists and exec policy the materialiser
// pushes to OpenClaw. It holds no state; every function reads what it is given.
package guardrails

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// RateWindow is the rolling window max_runs_per_hour counts over (DESIGN 5.11 units).
const RateWindow = time.Hour

// EffectiveLimits applies rule 1 to limits already layered and clamped at trigger time
// (request, workflow, defaults, platform maximum): max_cost_usd becomes the smallest
// of that value and every participating agent's max_cost_usd_per_run, and never more
// than platformMaxCostUSD (0 means no platform maximum). Agents without a per-run cap
// do not lower it.
func EffectiveLimits(l store.Limits, agents []*store.Agent, platformMaxCostUSD float64) store.Limits {
	cost := l.MaxCostUSD
	for _, a := range agents {
		if a == nil {
			continue
		}
		if c := a.Guardrails.MaxCostUSDPerRun; c > 0 && (cost <= 0 || c < cost) {
			cost = c
		}
	}
	if platformMaxCostUSD > 0 && cost > platformMaxCostUSD {
		cost = platformMaxCostUSD
	}
	l.MaxCostUSD = cost
	return l
}

// AgentIDs returns the distinct agent ids a definition's agent nodes name, in node
// order.
func AgentIDs(def *workflow.Definition) []string {
	var ids []string
	for _, n := range def.Nodes {
		if n.Kind == workflow.KindAgent && n.AgentID != "" && !slices.Contains(ids, n.AgentID) {
			ids = append(ids, n.AgentID)
		}
	}
	return ids
}

// ParticipatingAgents loads the agents a definition's agent nodes name, in node order.
// An id with no live agent is skipped: trigger validation reports it.
func ParticipatingAgents(ctx context.Context, q store.Querier, def *workflow.Definition) ([]*store.Agent, error) {
	var out []*store.Agent
	for _, id := range AgentIDs(def) {
		a, err := store.GetAgent(ctx, q, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", id, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// RateLimitError is rule 2's refusal; the HTTP layer answers 429 with Error().
type RateLimitError struct {
	AgentID   string
	AgentName string
	Limit     int
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limit: %d runs/hour for agent %s", e.Limit, e.AgentName)
}

// CheckRateLimit applies rule 2: a trigger is refused with *RateLimitError when any
// participating agent with max_runs_per_hour = n already has n runs created in the
// rolling hour before now. Call it with the transaction that then inserts the run
// (rule 2 consistency): it takes a transaction-scoped advisory lock per limited
// agent, in id order, so concurrent triggers for one agent queue behind each other and
// cannot both take the last slot. Outside a transaction the locks are released at once
// and the check is only as good as its timing.
func CheckRateLimit(ctx context.Context, q store.Querier, agents []*store.Agent, now time.Time) error {
	var limited []*store.Agent
	for _, a := range agents {
		if a != nil && a.Guardrails.MaxRunsPerHour > 0 &&
			!slices.ContainsFunc(limited, func(b *store.Agent) bool { return b.ID == a.ID }) {
			limited = append(limited, a)
		}
	}
	slices.SortFunc(limited, func(a, b *store.Agent) int { return strings.Compare(a.ID, b.ID) })
	for _, a := range limited {
		if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "yuno:rate:"+a.ID); err != nil {
			return fmt.Errorf("rate limit lock: %w", err)
		}
		n, err := RunsInWindow(ctx, q, a.ID, now)
		if err != nil {
			return err
		}
		if n >= a.Guardrails.MaxRunsPerHour {
			return &RateLimitError{AgentID: a.ID, AgentName: a.Name, Limit: a.Guardrails.MaxRunsPerHour}
		}
	}
	return nil
}

// RunsInWindow counts the runs created in the rolling hour before now whose workflow
// includes the agent, whatever triggered them.
func RunsInWindow(ctx context.Context, q store.Querier, agentID string, now time.Time) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM runs WHERE created_at > $2
		AND workflow_snapshot->'nodes' @> jsonb_build_array(jsonb_build_object('agent_id', $1::text))`,
		agentID, now.Add(-RateWindow)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count runs for agent %s: %w", agentID, err)
	}
	return n, nil
}
