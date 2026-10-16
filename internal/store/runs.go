package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrDuplicateTrigger is CreateRun's answer when a run with the same schedule
// occurrence already exists (runs_schedule_occurrence_idx, DESIGN 5.9 rule 2).
var ErrDuplicateTrigger = errors.New("a run for this trigger already exists")

const runColumns = `id, workflow_id, workflow_version, workflow_snapshot, trigger, trigger_ref, status, input, state,
	limits, current_node, resume_node_visit_id, step_count, tool_call_count, attempt, cancel_requested, cancel_by,
	failure_reason, cost_usd_micros, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, paused_at,
	paused_total_ms, created_at, started_at, finished_at, updated_at`

func CreateRun(ctx context.Context, q Querier, r *Run) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	if r.Status == "" {
		r.Status = RunQueued
	}
	if len(r.Input) == 0 {
		r.Input = json.RawMessage(`{}`)
	}
	if len(r.State) == 0 {
		r.State = json.RawMessage(`{}`)
	}
	if r.Attempt == 0 {
		r.Attempt = 1
	}
	now := time.Now().UTC()
	r.CreatedAt, r.UpdatedAt = now, now
	_, err := q.Exec(ctx, `INSERT INTO runs (id, workflow_id, workflow_version, workflow_snapshot, trigger, trigger_ref,
		status, input, state, limits, attempt, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		r.ID, r.WorkflowID, r.WorkflowVersion, r.WorkflowSnapshot, r.Trigger, r.TriggerRef,
		r.Status, r.Input, r.State, r.Limits, r.Attempt, r.CreatedAt, r.UpdatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "runs_schedule_occurrence_idx" {
		return ErrDuplicateTrigger
	}
	return err
}

// GetRunByTrigger returns the run created by a trigger reference, e.g. a schedule
// occurrence; ErrNotFound when there is none.
func GetRunByTrigger(ctx context.Context, q Querier, trigger, ref string) (*Run, error) {
	rows, err := q.Query(ctx, `SELECT `+runColumns+` FROM runs WHERE trigger=$1 AND trigger_ref=$2
		ORDER BY created_at LIMIT 1`, trigger, ref)
	if err != nil {
		return nil, err
	}
	r, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Run])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

func GetRun(ctx context.Context, q Querier, id string) (*Run, error) {
	rows, err := q.Query(ctx, `SELECT `+runColumns+` FROM runs WHERE id=$1`, id)
	if err != nil {
		return nil, err
	}
	r, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Run])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// ListRuns returns the newest runs first, optionally filtered by status.
func ListRuns(ctx context.Context, q Querier, status string, limit int) ([]Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.Query(ctx, `SELECT `+runColumns+` FROM runs WHERE ($1 = '' OR status = $1) ORDER BY created_at DESC LIMIT $2`, status, limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Run])
	if out == nil {
		out = []Run{}
	}
	return out, err
}

// ClaimQueuedRun atomically moves the oldest queued run to running for this worker
// (DESIGN 5.1 rule 4, T5.1.8). Returns ErrNotFound when nothing is queued.
func ClaimQueuedRun(ctx context.Context, pool *pgxpool.Pool) (*Run, error) {
	rows, err := pool.Query(ctx, `UPDATE runs SET status=$1, started_at=COALESCE(started_at, now()), updated_at=now()
		WHERE id = (SELECT id FROM runs WHERE status=$2 ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING `+runColumns, RunRunning, RunQueued)
	if err != nil {
		return nil, err
	}
	r, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Run])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// SetRunStatus writes a status change; the engine validates the transition first.
// finished_at is set for terminal statuses.
func SetRunStatus(ctx context.Context, q Querier, id, status string, failureReason *string) error {
	var finished any
	if IsTerminal(status) {
		finished = time.Now().UTC()
	}
	_, err := q.Exec(ctx, `UPDATE runs SET status=$2, failure_reason=COALESCE($3, failure_reason),
		finished_at=COALESCE($4, finished_at), updated_at=now() WHERE id=$1`, id, status, failureReason, finished)
	return err
}

// RequestCancel flags a run; the engine observes the flag (DESIGN 5.2 rule 2).
func RequestCancel(ctx context.Context, q Querier, id, by string) error {
	tag, err := q.Exec(ctx, `UPDATE runs SET cancel_requested=true, cancel_by=$2, updated_at=now() WHERE id=$1`, id, by)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// AddRunUsage increments counters and tokens in one statement (DESIGN 5.2 consistency).
func AddRunUsage(ctx context.Context, q Querier, id string, steps, toolCalls int, in, out, cacheRead, cacheWrite, costMicros int64) error {
	_, err := q.Exec(ctx, `UPDATE runs SET step_count=step_count+$2, tool_call_count=tool_call_count+$3,
		input_tokens=input_tokens+$4, output_tokens=output_tokens+$5, cache_read_tokens=cache_read_tokens+$6,
		cache_write_tokens=cache_write_tokens+$7, cost_usd_micros=cost_usd_micros+$8, updated_at=now() WHERE id=$1`,
		id, steps, toolCalls, in, out, cacheRead, cacheWrite, costMicros)
	return err
}

func SetRunState(ctx context.Context, q Querier, id string, state json.RawMessage, currentNode *string) error {
	_, err := q.Exec(ctx, `UPDATE runs SET state=$2, current_node=$3, updated_at=now() WHERE id=$1`, id, state, currentNode)
	return err
}

// PauseRun records entering waiting_approval (DESIGN 5.2 rule 4).
func PauseRun(ctx context.Context, q Querier, id string, resumeNodeVisitID string) error {
	_, err := q.Exec(ctx, `UPDATE runs SET status=$2, paused_at=now(), resume_node_visit_id=$3, updated_at=now() WHERE id=$1`,
		id, RunWaitingApproval, resumeNodeVisitID)
	return err
}

// ResumeRun re-queues a paused run and accumulates the paused time.
func ResumeRun(ctx context.Context, q Querier, id string) error {
	_, err := q.Exec(ctx, `UPDATE runs SET status=$2,
		paused_total_ms = paused_total_ms + COALESCE(EXTRACT(EPOCH FROM (now() - paused_at)) * 1000, 0)::bigint,
		paused_at=NULL, updated_at=now() WHERE id=$1 AND status=$3`, id, RunQueued, RunWaitingApproval)
	return err
}

// RequeueStaleRuns implements DESIGN 5.1 rule 8: running runs untouched for longer than
// staleAfter are re-queued once (attempt 2) and failed on the second detection.
func RequeueStaleRuns(ctx context.Context, q Querier, staleAfter time.Duration) (requeued, failed int64, err error) {
	cutoff := time.Now().UTC().Add(-staleAfter)
	tag, err := q.Exec(ctx, `UPDATE runs SET status=$1, attempt=attempt+1, updated_at=now()
		WHERE status=$2 AND updated_at < $3 AND attempt < 2`, RunQueued, RunRunning, cutoff)
	if err != nil {
		return 0, 0, err
	}
	requeued = tag.RowsAffected()
	tag, err = q.Exec(ctx, `UPDATE runs SET status=$1, failure_reason='worker lost', finished_at=now(), updated_at=now()
		WHERE status=$2 AND updated_at < $3 AND attempt >= 2`, RunFailed, RunRunning, cutoff)
	if err != nil {
		return requeued, 0, err
	}
	return requeued, tag.RowsAffected(), nil
}

// ---- run nodes --------------------------------------------------------------------

const runNodeColumns = `id, run_id, node_id, agent_id, visit, iteration, status, messages, pending_tool_results, output,
	failure_reason, session_key, openclaw_run_id, started_at, finished_at`

func CreateRunNode(ctx context.Context, q Querier, n *RunNode) error {
	if n.ID == "" {
		n.ID = NewID()
	}
	if n.Visit == 0 {
		n.Visit = 1
	}
	if n.Status == "" {
		n.Status = NodePending
	}
	if len(n.Messages) == 0 {
		n.Messages = json.RawMessage(`[]`)
	}
	if len(n.PendingToolResults) == 0 {
		n.PendingToolResults = json.RawMessage(`null`)
	}
	if len(n.Output) == 0 {
		n.Output = json.RawMessage(`null`)
	}
	_, err := q.Exec(ctx, `INSERT INTO run_nodes (`+runNodeColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		n.ID, n.RunID, n.NodeID, n.AgentID, n.Visit, n.Iteration, n.Status, n.Messages, n.PendingToolResults, n.Output,
		n.FailureReason, n.SessionKey, n.OpenClawRunID, n.StartedAt, n.FinishedAt)
	return err
}

func GetRunNode(ctx context.Context, q Querier, id string) (*RunNode, error) {
	rows, err := q.Query(ctx, `SELECT `+runNodeColumns+` FROM run_nodes WHERE id=$1`, id)
	if err != nil {
		return nil, err
	}
	n, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[RunNode])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

func ListRunNodes(ctx context.Context, q Querier, runID string) ([]RunNode, error) {
	rows, err := q.Query(ctx, `SELECT `+runNodeColumns+` FROM run_nodes WHERE run_id=$1 ORDER BY started_at NULLS LAST, id`, runID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[RunNode])
	if out == nil {
		out = []RunNode{}
	}
	return out, err
}

// CountNodeVisits returns how many times a node has been visited in a run (DESIGN 5.2 rule 8).
func CountNodeVisits(ctx context.Context, q Querier, runID, nodeID string) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM run_nodes WHERE run_id=$1 AND node_id=$2`, runID, nodeID).Scan(&n)
	return n, err
}

func UpdateRunNode(ctx context.Context, q Querier, n *RunNode) error {
	_, err := q.Exec(ctx, `UPDATE run_nodes SET iteration=$2, status=$3, messages=$4, pending_tool_results=$5, output=$6,
		failure_reason=$7, session_key=$8, openclaw_run_id=$9, started_at=$10, finished_at=$11 WHERE id=$1`,
		n.ID, n.Iteration, n.Status, n.Messages, n.PendingToolResults, n.Output, n.FailureReason, n.SessionKey,
		n.OpenClawRunID, n.StartedAt, n.FinishedAt)
	return err
}

// ---- model calls, tool calls, spans, agent messages, approvals ----------------------

const modelCallColumns = `id, run_id, run_node_id, provider, model, request_id, stop_reason, input_tokens, output_tokens,
	cache_read_tokens, cache_write_tokens, cost_usd_micros, duration_ms, attempt, error, started_at, finished_at`

func InsertModelCall(ctx context.Context, q Querier, m *ModelCall) error {
	if m.ID == "" {
		m.ID = NewID()
	}
	if m.Attempt == 0 {
		m.Attempt = 1
	}
	_, err := q.Exec(ctx, `INSERT INTO model_calls (`+modelCallColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		m.ID, m.RunID, m.RunNodeID, m.Provider, m.Model, m.RequestID, m.StopReason, m.InputTokens, m.OutputTokens,
		m.CacheReadTokens, m.CacheWriteTokens, m.CostUSDMicros, m.DurationMs, m.Attempt, m.Error, m.StartedAt, m.FinishedAt)
	return err
}

func ListModelCalls(ctx context.Context, q Querier, runID string) ([]ModelCall, error) {
	rows, err := q.Query(ctx, `SELECT `+modelCallColumns+` FROM model_calls WHERE run_id=$1 ORDER BY started_at, id`, runID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[ModelCall])
	if out == nil {
		out = []ModelCall{}
	}
	return out, err
}

const toolCallColumns = `id, run_id, run_node_id, model_call_id, tool_use_id, tool_name, input, output, is_error, status,
	idempotency_key, attempt, duration_ms, started_at, finished_at`

func InsertToolCall(ctx context.Context, q Querier, t *ToolCall) error {
	if t.ID == "" {
		t.ID = NewID()
	}
	if t.Attempt == 0 {
		t.Attempt = 1
	}
	if len(t.Input) == 0 {
		t.Input = json.RawMessage(`{}`)
	}
	if len(t.Output) == 0 {
		t.Output = json.RawMessage(`null`)
	}
	_, err := q.Exec(ctx, `INSERT INTO tool_calls (`+toolCallColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		t.ID, t.RunID, t.RunNodeID, t.ModelCallID, t.ToolUseID, t.ToolName, t.Input, t.Output, t.IsError, t.Status,
		t.IdempotencyKey, t.Attempt, t.DurationMs, t.StartedAt, t.FinishedAt)
	return err
}

func UpdateToolCall(ctx context.Context, q Querier, t *ToolCall) error {
	if len(t.Output) == 0 {
		t.Output = json.RawMessage(`null`)
	}
	_, err := q.Exec(ctx, `UPDATE tool_calls SET output=$2, is_error=$3, status=$4, idempotency_key=$5, attempt=$6,
		duration_ms=$7, started_at=$8, finished_at=$9 WHERE id=$1`,
		t.ID, t.Output, t.IsError, t.Status, t.IdempotencyKey, t.Attempt, t.DurationMs, t.StartedAt, t.FinishedAt)
	return err
}

// GetToolCallByUseID finds the row for an OpenClaw toolCallId within a run.
func GetToolCallByUseID(ctx context.Context, q Querier, runID, toolUseID string) (*ToolCall, error) {
	rows, err := q.Query(ctx, `SELECT `+toolCallColumns+` FROM tool_calls WHERE run_id=$1 AND tool_use_id=$2 ORDER BY id DESC LIMIT 1`, runID, toolUseID)
	if err != nil {
		return nil, err
	}
	t, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[ToolCall])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

func ListToolCalls(ctx context.Context, q Querier, runID string) ([]ToolCall, error) {
	rows, err := q.Query(ctx, `SELECT `+toolCallColumns+` FROM tool_calls WHERE run_id=$1 ORDER BY started_at NULLS LAST, id`, runID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[ToolCall])
	if out == nil {
		out = []ToolCall{}
	}
	return out, err
}

const spanColumns = `id, run_id, parent_id, kind, name, started_at, ended_at, attrs`

func InsertSpan(ctx context.Context, q Querier, s *Span) error {
	if s.ID == "" {
		s.ID = NewID()
	}
	if len(s.Attrs) == 0 {
		s.Attrs = json.RawMessage(`{}`)
	}
	if s.StartedAt.IsZero() {
		s.StartedAt = time.Now().UTC()
	}
	_, err := q.Exec(ctx, `INSERT INTO spans (`+spanColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		s.ID, s.RunID, s.ParentID, s.Kind, s.Name, s.StartedAt, s.EndedAt, s.Attrs)
	return err
}

// EndSpan closes a span and merges attrs into the existing ones.
func EndSpan(ctx context.Context, q Querier, id string, attrs json.RawMessage) error {
	if len(attrs) == 0 {
		attrs = json.RawMessage(`{}`)
	}
	_, err := q.Exec(ctx, `UPDATE spans SET ended_at=now(), attrs = attrs || $2::jsonb WHERE id=$1`, id, attrs)
	return err
}

func ListSpans(ctx context.Context, q Querier, runID string) ([]Span, error) {
	rows, err := q.Query(ctx, `SELECT `+spanColumns+` FROM spans WHERE run_id=$1 ORDER BY started_at, id`, runID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Span])
	if out == nil {
		out = []Span{}
	}
	return out, err
}

const agentMessageColumns = `id, run_id, from_node, to_node, payload, created_at`

func InsertAgentMessage(ctx context.Context, q Querier, m *AgentMessage) error {
	if m.ID == "" {
		m.ID = NewID()
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	_, err := q.Exec(ctx, `INSERT INTO agent_messages (`+agentMessageColumns+`) VALUES ($1,$2,$3,$4,$5,$6)`,
		m.ID, m.RunID, m.FromNode, m.ToNode, m.Payload, m.CreatedAt)
	return err
}

func ListAgentMessages(ctx context.Context, q Querier, runID string) ([]AgentMessage, error) {
	rows, err := q.Query(ctx, `SELECT `+agentMessageColumns+` FROM agent_messages WHERE run_id=$1 ORDER BY created_at, id`, runID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[AgentMessage])
	if out == nil {
		out = []AgentMessage{}
	}
	return out, err
}

const approvalColumns = `id, run_id, run_node_id, tool_call_id, kind, node_id, message, status, external_id, requested_at,
	decided_at, decided_by, reason, expires_at`

// approvalSelect reads an approval for scanning. One raised in a conversation has no
// run or run node (DESIGN 5.8 rule 5): they read as "", with conversation_id set.
const approvalSelect = `id, COALESCE(run_id, '') AS run_id, COALESCE(run_node_id, '') AS run_node_id, tool_call_id, kind,
	node_id, message, status, external_id, requested_at, decided_at, decided_by, reason, expires_at, conversation_id`

func InsertApproval(ctx context.Context, q Querier, a *Approval) error {
	if a.ID == "" {
		a.ID = NewID()
	}
	if a.Status == "" {
		a.Status = "pending"
	}
	if a.RequestedAt.IsZero() {
		a.RequestedAt = time.Now().UTC()
	}
	_, err := q.Exec(ctx, `INSERT INTO approvals (`+approvalColumns+`) VALUES ($1,NULLIF($2,''),NULLIF($3,''),$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		a.ID, a.RunID, a.RunNodeID, a.ToolCallID, a.Kind, a.NodeID, a.Message, a.Status, a.ExternalID, a.RequestedAt,
		a.DecidedAt, a.DecidedBy, a.Reason, a.ExpiresAt)
	return err
}

func GetApproval(ctx context.Context, q Querier, id string) (*Approval, error) {
	rows, err := q.Query(ctx, `SELECT `+approvalSelect+` FROM approvals WHERE id=$1`, id)
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Approval])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

func ListApprovals(ctx context.Context, q Querier, status, runID string, limit int) ([]Approval, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.Query(ctx, `SELECT `+approvalSelect+` FROM approvals
		WHERE ($1 = '' OR status=$1) AND ($2 = '' OR run_id=$2) ORDER BY requested_at DESC LIMIT $3`, status, runID, limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Approval])
	if out == nil {
		out = []Approval{}
	}
	return out, err
}

// DecideApproval moves a pending approval to approved/rejected/expired. It returns
// ErrNotFound when the approval is not pending (callers answer 409, DESIGN 5.3 rule 8).
func DecideApproval(ctx context.Context, q Querier, id, status, by, reason string) error {
	tag, err := q.Exec(ctx, `UPDATE approvals SET status=$2, decided_at=now(), decided_by=$3, reason=$4
		WHERE id=$1 AND status='pending'`, id, status, by, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetTrace assembles the run detail payload.
func GetTrace(ctx context.Context, q Querier, runID string) (*Trace, error) {
	run, err := GetRun(ctx, q, runID)
	if err != nil {
		return nil, err
	}
	t := &Trace{Run: run}
	if t.Nodes, err = ListRunNodes(ctx, q, runID); err != nil {
		return nil, err
	}
	if t.ModelCalls, err = ListModelCalls(ctx, q, runID); err != nil {
		return nil, err
	}
	if t.ToolCalls, err = ListToolCalls(ctx, q, runID); err != nil {
		return nil, err
	}
	if t.Spans, err = ListSpans(ctx, q, runID); err != nil {
		return nil, err
	}
	if t.AgentMessages, err = ListAgentMessages(ctx, q, runID); err != nil {
		return nil, err
	}
	if t.Approvals, err = ListApprovals(ctx, q, "", runID, 500); err != nil {
		return nil, err
	}
	return t, nil
}

// FillRunParticipants sets AgentNames and Models on each run for list views: the agents
// its nodes ran and the models its model calls used, first appearance first. A run that
// has no nodes or no model calls yet falls back to the agent nodes of its workflow
// snapshot and their configured models, or the run's model override when it has one.
func FillRunParticipants(ctx context.Context, q Querier, runs []Run) error {
	if len(runs) == 0 {
		return nil
	}
	ids := make([]string, len(runs))
	for i := range runs {
		ids[i] = runs[i].ID
	}
	names, err := collectAggregates(ctx, q, `SELECT x.run_id, array_agg(x.name ORDER BY x.first) FROM (
		SELECT rn.run_id, a.name, min(rn.id) AS first FROM run_nodes rn JOIN agents a ON a.id = rn.agent_id
		WHERE rn.run_id = ANY($1) GROUP BY rn.run_id, a.name) x GROUP BY x.run_id`, ids)
	if err != nil {
		return err
	}
	models, err := collectAggregates(ctx, q, `SELECT x.run_id, array_agg(x.model ORDER BY x.first) FROM (
		SELECT run_id, model, min(started_at) AS first FROM model_calls WHERE run_id = ANY($1)
		GROUP BY run_id, model) x GROUP BY x.run_id`, ids)
	if err != nil {
		return err
	}

	// Fallback for runs that have not produced either yet: the snapshot's agent nodes.
	type snapshot struct {
		Nodes []struct {
			Kind    string `json:"kind"`
			AgentID string `json:"agent_id"`
		} `json:"nodes"`
		RunOverrides *struct {
			Model string `json:"model"`
		} `json:"run_overrides"`
	}
	snaps := make([]snapshot, len(runs))
	var need []string
	for i := range runs {
		if len(names[runs[i].ID]) > 0 && len(models[runs[i].ID]) > 0 {
			continue
		}
		_ = json.Unmarshal(runs[i].WorkflowSnapshot, &snaps[i])
		for _, n := range snaps[i].Nodes {
			if n.Kind == "agent" && n.AgentID != "" {
				need = append(need, n.AgentID)
			}
		}
	}
	type nameModel struct{ name, model string }
	agents := map[string]nameModel{}
	if len(need) > 0 {
		rows, err := q.Query(ctx, `SELECT id, name, model FROM agents WHERE id = ANY($1)`, need)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, name, model string
			if err := rows.Scan(&id, &name, &model); err != nil {
				return err
			}
			agents[id] = nameModel{name, model}
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for i := range runs {
		r := &runs[i]
		r.AgentNames, r.Models = names[r.ID], models[r.ID]
		if len(r.AgentNames) > 0 && len(r.Models) > 0 {
			continue
		}
		var fallbackNames, fallbackModels []string
		for _, n := range snaps[i].Nodes {
			a, ok := agents[n.AgentID]
			if n.Kind != "agent" || !ok {
				continue
			}
			fallbackNames = appendDistinct(fallbackNames, a.name)
			model := a.model
			if o := snaps[i].RunOverrides; o != nil && o.Model != "" {
				model = o.Model
			}
			fallbackModels = appendDistinct(fallbackModels, model)
		}
		if len(r.AgentNames) == 0 {
			r.AgentNames = fallbackNames
		}
		if len(r.Models) == 0 {
			r.Models = fallbackModels
		}
	}
	return nil
}

// collectAggregates runs a query returning (run_id text, values text[]) rows into a map.
func collectAggregates(ctx context.Context, q Querier, sql string, ids []string) (map[string][]string, error) {
	rows, err := q.Query(ctx, sql, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var id string
		var values []string
		if err := rows.Scan(&id, &values); err != nil {
			return nil, err
		}
		out[id] = values
	}
	return out, rows.Err()
}

func appendDistinct(list []string, v string) []string {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}
