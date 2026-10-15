package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Queries the run engine needs beyond the shared repositories. Every timestamp comes
// from the engine's clock so the limits of DESIGN 5.2 are testable from both sides.

// SetRunStartedAt records when the engine started executing a run.
func SetRunStartedAt(ctx context.Context, q Querier, id string, at time.Time) error {
	_, err := q.Exec(ctx, `UPDATE runs SET started_at=$2, updated_at=now() WHERE id=$1`, id, at.UTC())
	return err
}

// TransitionRun moves a run to status `to` only if its current status is one of
// `from`, and returns the updated row. It returns ErrNotFound when the run does not
// exist or is in none of the expected statuses. finished_at is set for terminal
// statuses; reason, when non-nil, replaces failure_reason.
func TransitionRun(ctx context.Context, q Querier, id string, from []string, to string, reason *string, at time.Time) (*Run, error) {
	var finished any
	if IsTerminal(to) {
		finished = at.UTC()
	}
	rows, err := q.Query(ctx, `UPDATE runs SET status=$3, failure_reason=COALESCE($4, failure_reason),
		finished_at=COALESCE($5, finished_at), updated_at=now()
		WHERE id=$1 AND status = ANY($2) RETURNING `+runColumns, id, from, to, reason, finished)
	if err != nil {
		return nil, err
	}
	r, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Run])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// AddRunUsageReturning is AddRunUsage that returns the updated row, so the engine can
// check limits and the cancel flag on the same read (DESIGN 5.2).
func AddRunUsageReturning(ctx context.Context, q Querier, id string, steps, toolCalls int, in, out, cacheRead, cacheWrite, costMicros int64) (*Run, error) {
	rows, err := q.Query(ctx, `UPDATE runs SET step_count=step_count+$2, tool_call_count=tool_call_count+$3,
		input_tokens=input_tokens+$4, output_tokens=output_tokens+$5, cache_read_tokens=cache_read_tokens+$6,
		cache_write_tokens=cache_write_tokens+$7, cost_usd_micros=cost_usd_micros+$8, updated_at=now()
		WHERE id=$1 RETURNING `+runColumns,
		id, steps, toolCalls, in, out, cacheRead, cacheWrite, costMicros)
	if err != nil {
		return nil, err
	}
	r, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Run])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// TouchRun refreshes updated_at so the stale-run sweeper (DESIGN 5.1 rule 8) leaves a
// long, quiet turn alone.
func TouchRun(ctx context.Context, q Querier, id string) error {
	_, err := q.Exec(ctx, `UPDATE runs SET updated_at=now() WHERE id=$1`, id)
	return err
}

// PauseRunAt moves a running run to waiting_approval at the given time.
func PauseRunAt(ctx context.Context, q Querier, id, resumeNodeVisitID string, at time.Time) (*Run, error) {
	rows, err := q.Query(ctx, `UPDATE runs SET status=$2, paused_at=$3, resume_node_visit_id=$4, updated_at=now()
		WHERE id=$1 AND status=$5 RETURNING `+runColumns, id, RunWaitingApproval, at.UTC(), resumeNodeVisitID, RunRunning)
	if err != nil {
		return nil, err
	}
	r, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Run])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// ResumeRunAt moves a waiting_approval run back to running in place (the OpenClaw turn
// never stopped) and adds the paused interval to paused_total_ms.
func ResumeRunAt(ctx context.Context, q Querier, id string, at time.Time) (*Run, error) {
	rows, err := q.Query(ctx, `UPDATE runs SET status=$2,
		paused_total_ms = paused_total_ms + GREATEST(0, COALESCE(EXTRACT(EPOCH FROM ($3::timestamptz - paused_at)) * 1000, 0))::bigint,
		paused_at=NULL, updated_at=now() WHERE id=$1 AND status=$4 RETURNING `+runColumns,
		id, RunRunning, at.UTC(), RunWaitingApproval)
	if err != nil {
		return nil, err
	}
	r, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Run])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// UpdateRunNodeProgress writes everything about a node visit except its output, which
// only SetRunNodeOutput writes (the finish tool and the engine race otherwise).
func UpdateRunNodeProgress(ctx context.Context, q Querier, n *RunNode) error {
	_, err := q.Exec(ctx, `UPDATE run_nodes SET iteration=$2, status=$3, messages=$4, failure_reason=$5, session_key=$6,
		openclaw_run_id=$7, started_at=$8, finished_at=$9 WHERE id=$1`,
		n.ID, n.Iteration, n.Status, n.Messages, n.FailureReason, n.SessionKey, n.OpenClawRunID, n.StartedAt, n.FinishedAt)
	return err
}

// SetRunNodeOutput stores a node visit's typed output.
func SetRunNodeOutput(ctx context.Context, q Querier, id string, output json.RawMessage) error {
	if len(output) == 0 {
		output = json.RawMessage(`null`)
	}
	_, err := q.Exec(ctx, `UPDATE run_nodes SET output=$2 WHERE id=$1`, id, output)
	return err
}

// FailRunningNodes marks a run's unfinished node visits failed (a re-queued run after
// a lost worker, DESIGN 5.1 rule 8).
func FailRunningNodes(ctx context.Context, q Querier, runID, reason string, at time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `UPDATE run_nodes SET status=$2, failure_reason=$3, finished_at=$4
		WHERE run_id=$1 AND status IN ($5, $6, $7)`, runID, NodeFailed, reason, at.UTC(), NodePending, NodeRunning, NodeWaitingApproval)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// SetToolCallInput fills in a tool call's input (and the model call that requested it)
// once the transcript entry carrying the arguments arrives.
func SetToolCallInput(ctx context.Context, q Querier, id string, input json.RawMessage, modelCallID *string) error {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	_, err := q.Exec(ctx, `UPDATE tool_calls SET input=$2, model_call_id=COALESCE($3, model_call_id) WHERE id=$1`, id, input, modelCallID)
	return err
}

// GetApprovalByExternalID finds the approval recorded for an OpenClaw approval id.
func GetApprovalByExternalID(ctx context.Context, q Querier, externalID string) (*Approval, error) {
	rows, err := q.Query(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE external_id=$1 ORDER BY requested_at DESC LIMIT 1`, externalID)
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Approval])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// DecideApprovalAt is DecideApproval with the decision time supplied by the caller. It
// returns ErrNotFound when the approval is not pending.
func DecideApprovalAt(ctx context.Context, q Querier, id, status, by, reason string, at time.Time) (*Approval, error) {
	rows, err := q.Query(ctx, `UPDATE approvals SET status=$2, decided_at=$5, decided_by=$3, reason=$4
		WHERE id=$1 AND status='pending' RETURNING `+approvalColumns, id, status, by, reason, at.UTC())
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Approval])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// ListPendingApprovals returns a run's pending approvals, oldest first.
func ListPendingApprovals(ctx context.Context, q Querier, runID string) ([]Approval, error) {
	rows, err := q.Query(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE run_id=$1 AND status='pending' ORDER BY requested_at`, runID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Approval])
	if out == nil {
		out = []Approval{}
	}
	return out, err
}

// EndSpanAt closes a span at the given time and merges attrs into the existing ones.
func EndSpanAt(ctx context.Context, q Querier, id string, at time.Time, attrs json.RawMessage) error {
	if len(attrs) == 0 {
		attrs = json.RawMessage(`{}`)
	}
	_, err := q.Exec(ctx, `UPDATE spans SET ended_at=$2, attrs = attrs || $3::jsonb WHERE id=$1`, id, at.UTC(), attrs)
	return err
}
