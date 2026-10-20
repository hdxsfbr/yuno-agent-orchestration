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
//
// A parked visit (DESIGN 5.24 rule 5) that its decision already closed keeps its closed
// state: the visit goroutine that parked it may still save what it holds (a late
// transcript entry) after the decision re-queued the run, and must not reopen the row.
func UpdateRunNodeProgress(ctx context.Context, q Querier, n *RunNode) error {
	_, err := q.Exec(ctx, `UPDATE run_nodes SET iteration=$2, messages=$4, failure_reason=$5, session_key=$6,
		openclaw_run_id=$7, started_at=$8,
		status = CASE WHEN parked_at IS NOT NULL AND status = $10 THEN status ELSE $3 END,
		finished_at = CASE WHEN parked_at IS NOT NULL AND status = $10 THEN finished_at ELSE $9 END
		WHERE id=$1`,
		n.ID, n.Iteration, n.Status, n.Messages, n.FailureReason, n.SessionKey, n.OpenClawRunID, n.StartedAt, n.FinishedAt,
		NodeSucceeded)
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
	rows, err := q.Query(ctx, `SELECT `+approvalSelect+` FROM approvals WHERE external_id=$1 ORDER BY requested_at DESC LIMIT 1`, externalID)
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
		WHERE id=$1 AND status='pending' RETURNING `+approvalSelect, id, status, by, reason, at.UTC())
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Approval])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// AnswerApprovalAt records the operator's answer to a pending question (DESIGN 5.24 rule
// 3): the row becomes approved with answer, decided_at and decided_by. It returns
// ErrNotFound when the approval is not a pending question, as DecideApprovalAt does for
// one that is not pending.
func AnswerApprovalAt(ctx context.Context, q Querier, id, answer, by string, at time.Time) (*Approval, error) {
	rows, err := q.Query(ctx, `UPDATE approvals SET status=$2, answer=$3, decided_by=$4, decided_at=$5
		WHERE id=$1 AND status='pending' AND kind=$6 RETURNING `+approvalSelect,
		id, ApprovalApproved, answer, by, at.UTC(), ApprovalKindQuestion)
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
	rows, err := q.Query(ctx, `SELECT `+approvalSelect+` FROM approvals WHERE run_id=$1 AND status='pending' ORDER BY requested_at`, runID)
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

// ---- wave 2: graph walk, approvals, pending effects -----------------------------------

func collectRun(rows pgx.Rows, err error) (*Run, error) {
	if err != nil {
		return nil, err
	}
	r, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Run])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// SetRunCurrentNode records the node the walk is at without touching the state. It also
// drops resume_node_visit_id: the walk moves off a decided approval node only through
// here, so once it has, that visit's decision is spent and the node, reached again, is
// a new entry.
func SetRunCurrentNode(ctx context.Context, q Querier, id, node string) error {
	_, err := q.Exec(ctx, `UPDATE runs SET current_node=$2, resume_node_visit_id=NULL, updated_at=now() WHERE id=$1`, id, node)
	return err
}

// SetRunStateNode replaces state[<nodeID>] with output and moves current_node in one
// statement (DESIGN 5.6 rule 6). It changes only that key, so a concurrent append to
// state.pending_effects is never lost.
func SetRunStateNode(ctx context.Context, q Querier, id, nodeID string, output json.RawMessage, currentNode string) (*Run, error) {
	if len(output) == 0 {
		output = json.RawMessage(`null`)
	}
	return collectRun(q.Query(ctx, `UPDATE runs SET state = jsonb_set(CASE WHEN jsonb_typeof(state) = 'object' THEN state ELSE '{}'::jsonb END,
		ARRAY[$2::text], $3::jsonb, true), current_node=$4, updated_at=now() WHERE id=$1 RETURNING `+runColumns,
		id, nodeID, output, currentNode))
}

// InsertApprovalAndPause stores a pending approval and moves its running run to
// waiting_approval at the approval's node visit in one transaction (DESIGN 5.6 rule 8):
// a decision, which re-queues only a paused run, can never see the approval while the
// run is still running. ErrNotFound means the run was not running; nothing is stored.
func InsertApprovalAndPause(ctx context.Context, db interface {
	Begin(context.Context) (pgx.Tx, error)
}, a *Approval, at time.Time) (*Run, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit
	if err := InsertApproval(ctx, tx, a); err != nil {
		return nil, err
	}
	run, err := PauseRunAt(ctx, tx, a.RunID, a.RunNodeID, at)
	if err != nil {
		return nil, err
	}
	return run, tx.Commit(ctx)
}

// AppendPendingEffect appends one entry to state.pending_effects (DESIGN 5.3 rule 5).
func AppendPendingEffect(ctx context.Context, q Querier, id string, entry json.RawMessage) (*Run, error) {
	return collectRun(q.Query(ctx, `UPDATE runs SET state = jsonb_set(
		CASE WHEN jsonb_typeof(state) = 'object' THEN state ELSE '{}'::jsonb END, '{pending_effects}',
		COALESCE(state->'pending_effects', '[]'::jsonb) || jsonb_build_array($2::jsonb), true), updated_at=now()
		WHERE id=$1 RETURNING `+runColumns, id, entry))
}

// RequeuePausedRunAt moves a waiting_approval run back to queued (an approval node was
// decided) and adds the paused interval to paused_total_ms.
func RequeuePausedRunAt(ctx context.Context, q Querier, id string, at time.Time) (*Run, error) {
	return collectRun(q.Query(ctx, `UPDATE runs SET status=$2,
		paused_total_ms = paused_total_ms + GREATEST(0, COALESCE(EXTRACT(EPOCH FROM ($3::timestamptz - paused_at)) * 1000, 0))::bigint,
		paused_at=NULL, updated_at=now() WHERE id=$1 AND status=$4 RETURNING `+runColumns,
		id, RunQueued, at.UTC(), RunWaitingApproval))
}

// ParkVisit parks a visit on its pending question (DESIGN 5.24 rule 5) in one
// transaction: the visit's row becomes waiting_approval with parked_at, and the run moves
// to waiting_approval with resume_node_visit_id the visit and paused_at the park time (a
// run that is already waiting keeps its paused_at). It is committed before the worker is
// released. ErrNotFound means the visit or the run was not in a state that can park;
// nothing is stored.
func ParkVisit(ctx context.Context, db interface {
	Begin(context.Context) (pgx.Tx, error)
}, runID, runNodeID string, at time.Time) (*Run, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit
	tag, err := tx.Exec(ctx, `UPDATE run_nodes SET parked_at=$3, status=$4 WHERE id=$1 AND run_id=$2 AND status IN ($5, $4)`,
		runNodeID, runID, at.UTC(), NodeWaitingApproval, NodeRunning)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	run, err := collectRun(tx.Query(ctx, `UPDATE runs SET status=$2, paused_at=COALESCE(paused_at, $3), resume_node_visit_id=$4,
		updated_at=now() WHERE id=$1 AND status IN ($5, $2) RETURNING `+runColumns,
		runID, RunWaitingApproval, at.UTC(), runNodeID, RunRunning))
	if err != nil {
		return nil, err
	}
	return run, tx.Commit(ctx)
}

// IsParkedOn reports whether the run waits on a parked visit: the predicate of
// RequeueParkedRun, read without changing anything (DESIGN 5.24 rule 8), less its check
// that the visit waits on nothing any more.
func IsParkedOn(ctx context.Context, q Querier, runID, runNodeID string) (bool, error) {
	var parked bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs WHERE runs.id=$1 AND runs.status=$3 AND `+parkedResumeSQL+`
		AND EXISTS (SELECT 1 FROM run_nodes n WHERE n.id=$2 AND n.parked_at IS NOT NULL AND n.status=$4))`,
		runID, runNodeID, RunWaitingApproval, NodeWaitingApproval).Scan(&parked)
	return parked, err
}

// RequeueParkedRun re-queues a run that waits on a parked visit (DESIGN 5.24 rule 5, the
// Consistency paragraph): status waiting_approval, resume_node_visit_id the visit, and the
// visit's parked_at set. The paused interval is added to paused_total_ms as
// RequeuePausedRunAt does, and in the same statement the parked visit's row is closed
// succeeded without output, so a worker that claims the run at once never finds it open.
// It returns ErrNotFound when the predicate does not hold (nothing parked, or already
// re-queued), which makes every call after the first idempotent.
//
// A visit parks on its tool calls' approvals too (DESIGN 5.3 rule 5), and on a question and
// such approvals at once: the predicate also requires that it waits on nothing any more
// (visitSettled), so the last of its decisions re-queues the run.
//
// Since DESIGN 5.26 rule 8 the predicate also holds when the run waits on the fork visit
// whose branch the parked visit belongs to (parkedResumeSQL); the branch row goes back to
// running in the same statement, so the fork's re-entry runs it.
func RequeueParkedRun(ctx context.Context, q Querier, runID, runNodeID string, at time.Time) (*Run, error) {
	return collectRun(q.Query(ctx, `WITH parked AS (
			UPDATE run_nodes SET status=$5, finished_at=$3
			WHERE id=$2 AND run_id=$1 AND parked_at IS NOT NULL AND status IN ($6, $7, $8)
			  AND EXISTS (SELECT 1 FROM runs WHERE runs.id=$1 AND runs.status=$4 AND `+parkedResumeSQL+`)
			  AND `+visitSettled("$2")+`
			RETURNING id, branch_id),
		branch AS (
			UPDATE run_branches SET status=$7 WHERE id IN (SELECT branch_id FROM parked WHERE branch_id IS NOT NULL) AND status=$8
			RETURNING id)
		UPDATE runs SET status=$9,
			paused_total_ms = paused_total_ms + GREATEST(0, COALESCE(EXTRACT(EPOCH FROM ($3::timestamptz - paused_at)) * 1000, 0))::bigint,
			paused_at=NULL, updated_at=now()
		WHERE runs.id=$1 AND runs.status=$4 AND `+parkedResumeSQL+`
		  AND EXISTS (SELECT 1 FROM run_nodes WHERE id=$2 AND parked_at IS NOT NULL AND status=$8)
		  AND `+visitSettled("$2")+`
		RETURNING `+runColumns,
		runID, runNodeID, at.UTC(), RunWaitingApproval, NodeSucceeded, NodePending, NodeRunning, NodeWaitingApproval, RunQueued))
}

// CountNodeLoopVisits returns the visits of a node in a run that count toward
// max_loop_visits (DESIGN 5.24 rule 5): those that continue no question. CountNodeVisits
// stays the visit number.
func CountNodeLoopVisits(ctx context.Context, q Querier, runID, nodeID string) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM run_nodes WHERE run_id=$1 AND node_id=$2 AND question_id IS NULL`, runID, nodeID).Scan(&n)
	return n, err
}

// SetApprovalExternalID stores the gateway's id for an approval registered with it.
func SetApprovalExternalID(ctx context.Context, q Querier, id, externalID string) error {
	_, err := q.Exec(ctx, `UPDATE approvals SET external_id=$2 WHERE id=$1`, id, externalID)
	return err
}

// ListExpiredApprovals returns pending approvals whose expires_at is not after `at`.
func ListExpiredApprovals(ctx context.Context, q Querier, at time.Time) ([]Approval, error) {
	rows, err := q.Query(ctx, `SELECT `+approvalSelect+` FROM approvals WHERE status='pending' AND expires_at <= $1
		ORDER BY expires_at`, at.UTC())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Approval])
}

// GetApprovalForRunNode returns the newest approval of a kind raised by a node visit.
func GetApprovalForRunNode(ctx context.Context, q Querier, runNodeID, kind string) (*Approval, error) {
	rows, err := q.Query(ctx, `SELECT `+approvalSelect+` FROM approvals WHERE run_node_id=$1 AND kind=$2
		ORDER BY requested_at DESC, id DESC LIMIT 1`, runNodeID, kind)
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Approval])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// LatestNodeVisit returns the newest visit of a node in a run.
func LatestNodeVisit(ctx context.Context, q Querier, runID, nodeID string) (*RunNode, error) {
	rows, err := q.Query(ctx, `SELECT `+runNodeColumns+` FROM run_nodes WHERE run_id=$1 AND node_id=$2
		ORDER BY visit DESC LIMIT 1`, runID, nodeID)
	if err != nil {
		return nil, err
	}
	n, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[RunNode])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

// FinishRunNode sets a node visit's terminal status (approval node visits, which no
// visit goroutine owns) unless it is already terminal, and returns the row.
func FinishRunNode(ctx context.Context, q Querier, id, status string, reason *string, at time.Time) (*RunNode, error) {
	rows, err := q.Query(ctx, `UPDATE run_nodes SET status=$2, failure_reason=COALESCE($3, failure_reason), finished_at=$4
		WHERE id=$1 AND status IN ($5, $6, $7) RETURNING `+runNodeColumns,
		id, status, reason, at.UTC(), NodePending, NodeRunning, NodeWaitingApproval)
	if err != nil {
		return nil, err
	}
	n, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[RunNode])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

// SetToolCallOutcome records the outcome of a gated tool call the engine settled after
// its MCP request had stopped waiting (rejected or expired approval).
func SetToolCallOutcome(ctx context.Context, q Querier, id, status string, output json.RawMessage, at time.Time) error {
	if len(output) == 0 {
		output = json.RawMessage(`null`)
	}
	_, err := q.Exec(ctx, `UPDATE tool_calls SET status=$2, output=$3, finished_at=COALESCE(finished_at, $4)
		WHERE id=$1 AND status=$5`, id, status, output, at.UTC(), ToolAwaitingApproval)
	return err
}

// EndApprovalSpan closes the approval_wait span of an approval, wherever it was opened.
func EndApprovalSpan(ctx context.Context, q Querier, runID, approvalID string, at time.Time, attrs json.RawMessage) error {
	if len(attrs) == 0 {
		attrs = json.RawMessage(`{}`)
	}
	_, err := q.Exec(ctx, `UPDATE spans SET ended_at=$3, attrs = attrs || $4::jsonb
		WHERE run_id=$1 AND kind='approval_wait' AND attrs->>'approval_id'=$2 AND ended_at IS NULL`,
		runID, approvalID, at.UTC(), attrs)
	return err
}

// OpenRootSpan returns the run's open root span, which a run paused at an approval node
// keeps open across workers; ErrNotFound when there is none.
func OpenRootSpan(ctx context.Context, q Querier, runID string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT id FROM spans WHERE run_id=$1 AND kind='run' AND parent_id IS NULL AND ended_at IS NULL
		ORDER BY started_at DESC, id DESC LIMIT 1`, runID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// GetAgentForRun loads an agent even when it was deleted after the run started: a
// run continues with the agents its snapshot names (DESIGN 5.6 edge cases).
func GetAgentForRun(ctx context.Context, q Querier, id string) (*Agent, error) {
	rows, err := q.Query(ctx, `SELECT `+agentSelect+` FROM agents WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Agent])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// AddExecAllowlistPattern appends a command pattern to an agent's exec allowlist
// unless it is there already (DESIGN 5.3 rule 3, allow-always).
func AddExecAllowlistPattern(ctx context.Context, q Querier, agentID, pattern string) error {
	_, err := q.Exec(ctx, `UPDATE agents SET exec_policy = jsonb_set(
		CASE WHEN jsonb_typeof(exec_policy) = 'object' THEN exec_policy ELSE '{}'::jsonb END, '{allowlist}',
		COALESCE(exec_policy->'allowlist', '[]'::jsonb) || to_jsonb($2::text), true), updated_at=now()
		WHERE id=$1 AND NOT (COALESCE(exec_policy->'allowlist', '[]'::jsonb) ? $2)`, agentID, pattern)
	return err
}

// ReopenApproval undoes a decision the gateway could not be told about, so the
// approval can be decided again.
func ReopenApproval(ctx context.Context, q Querier, id, decidedStatus string) error {
	_, err := q.Exec(ctx, `UPDATE approvals SET status='pending', decided_at=NULL, decided_by=NULL, reason=NULL
		WHERE id=$1 AND status=$2`, id, decidedStatus)
	return err
}
