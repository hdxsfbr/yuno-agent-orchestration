package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Queries behind the platform tool calls the MCP server records (DESIGN 5.3, 5.4, 5.7).

// GetToolCall returns one tool call by id, or ErrNotFound.
func GetToolCall(ctx context.Context, q Querier, id string) (*ToolCall, error) {
	rows, err := q.Query(ctx, `SELECT `+toolCallSelect+` FROM tool_calls WHERE id=$1`, id)
	if err != nil {
		return nil, err
	}
	t, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[ToolCall])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// ListToolCallsByIDs returns the tool calls with the given ids, keyed by id.
func ListToolCallsByIDs(ctx context.Context, q Querier, ids []string) (map[string]ToolCall, error) {
	out := map[string]ToolCall{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT `+toolCallSelect+` FROM tool_calls WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByName[ToolCall])
	if err != nil {
		return nil, err
	}
	for _, t := range list {
		out[t.ID] = t
	}
	return out, nil
}

// CompleteGatedToolCall writes the outcome of a call that waited for an approval, only
// while it is still awaiting_approval: the MCP handler and the engine's fallback both
// finish gated calls, and the first writer wins (DESIGN 5.3 rule 5). It reports whether
// this caller wrote the row.
func CompleteGatedToolCall(ctx context.Context, q Querier, t *ToolCall) (bool, error) {
	if len(t.Output) == 0 {
		t.Output = json.RawMessage(`null`)
	}
	tag, err := q.Exec(ctx, `UPDATE tool_calls SET output=$2, is_error=$3, status=$4, attempt=$5, duration_ms=$6,
		finished_at=$7 WHERE id=$1 AND status=$8`,
		t.ID, t.Output, t.IsError, t.Status, t.Attempt, t.DurationMs, t.FinishedAt, ToolAwaitingApproval)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// VisitAgent is the agent behind a node visit, read without the soft-delete filter: a
// run continues with an agent deleted after it started (DESIGN 5.6 edge cases).
type VisitAgent struct {
	NodeID          string
	AgentID         string
	OpenClawAgentID string
}

// GetVisitAgent resolves a run_nodes id to its node and agent.
func GetVisitAgent(ctx context.Context, q Querier, runNodeID string) (*VisitAgent, error) {
	var v VisitAgent
	var agentID, ocID *string
	err := q.QueryRow(ctx, `SELECT rn.node_id, rn.agent_id, a.openclaw_agent_id
		FROM run_nodes rn LEFT JOIN agents a ON a.id = rn.agent_id WHERE rn.id = $1`, runNodeID).Scan(&v.NodeID, &agentID, &ocID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if agentID != nil {
		v.AgentID = *agentID
	}
	if ocID != nil {
		v.OpenClawAgentID = *ocID
	}
	return &v, nil
}

// MergeRequestRef is the request an executed open_merge_request call opened or found
// (DESIGN 5.16 rule 12, 5.17 rules 8 and 10).
type MergeRequestRef struct {
	URL      string
	Provider string
}

// MergeRequestOfRun returns the newest executed open_merge_request call's request for a
// run, or nil when it has none.
func MergeRequestOfRun(ctx context.Context, q Querier, runID string) (*MergeRequestRef, error) {
	var ref MergeRequestRef
	err := q.QueryRow(ctx, `SELECT output->>'url', COALESCE(output->>'provider', '') FROM tool_calls
		WHERE run_id=$1 AND tool_name IN ('open_merge_request', 'yuno__open_merge_request') AND status='executed'
		  AND NOT is_error AND jsonb_typeof(output) = 'object' AND output ? 'url'
		ORDER BY finished_at DESC NULLS LAST, started_at DESC LIMIT 1`, runID).Scan(&ref.URL, &ref.Provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil || ref.URL == "" {
		return nil, err
	}
	return &ref, nil
}
