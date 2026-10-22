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
// occurrence already exists (runs_schedule_occurrence_idx, DESIGN 5.9 rule 2) or the same
// ship run of a plan's wave (runs_plan_wave_idx, DESIGN 5.29 rule 12).
var ErrDuplicateTrigger = errors.New("a run for this trigger already exists")

const runColumns = `id, workflow_id, workflow_version, workflow_snapshot, trigger, trigger_ref, status, input, state,
	limits, current_node, resume_node_visit_id, step_count, tool_call_count, attempt, cancel_requested, cancel_by,
	failure_reason, cost_usd_micros, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, paused_at,
	paused_total_ms, created_at, started_at, finished_at, updated_at, reported_at, toolchain, project_id, project_snapshot, retries,
	merge_request_id, checkout_sha, merged_sha, lease_epoch, cancel_state, park_reason, park_detail, resume_note`

// Constraint names of the partial unique indexes that refuse a second live run for the
// same merge request (DESIGN 5.22 rule 1) or the same issue (5.23 rule 3).
const (
	mergeRequestLiveIndex = "runs_merge_request_live_idx"
	issueLiveIndex        = "runs_issue_live_idx"
)

// SetRunToolchain records the toolchain of the run's project checkout (DESIGN 5.16
// rule 3).
func SetRunToolchain(ctx context.Context, q Querier, id, toolchain string) error {
	_, err := q.Exec(ctx, `UPDATE runs SET toolchain=$2, updated_at=now() WHERE id=$1`, id, toolchain)
	return err
}

// SetRunCheckoutSHA records the commit the run's project checkout is at (DESIGN 5.22
// rules 2 and 4): written after the clone and after each push of open_merge_request. Both
// end a merge the run had pending (merged_sha, 5.22 rule 13): a fresh clone has none, and
// a push carried it.
func SetRunCheckoutSHA(ctx context.Context, q Querier, id, sha string) error {
	_, err := q.Exec(ctx, `UPDATE runs SET checkout_sha=$2, merged_sha=NULL, updated_at=now() WHERE id=$1`, id, sha)
	return err
}

// SetRunMergedSHA records the default branch commit the platform merged into the run's
// checkout before its first turn (DESIGN 5.22 rule 13), the second parent of its next push.
func SetRunMergedSHA(ctx context.Context, q Querier, id, sha string) error {
	_, err := q.Exec(ctx, `UPDATE runs SET merged_sha=$2, updated_at=now() WHERE id=$1`, id, sha)
	return err
}

// liveIndexError maps a unique violation of one of the two live-run indexes to its
// sentinel and returns every other error as it came (DESIGN 5.22 rule 1, 5.23 rule 3).
func liveIndexError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case mergeRequestLiveIndex:
			return ErrMergeRequestBusy
		case issueLiveIndex:
			return ErrIssueBusy
		}
	}
	return err
}

// CreateRun inserts a run, queued unless r says otherwise: a run on a merge request its
// dependencies hold is created paused with ParkReason and ParkDetail (Chief platform
// control, DESIGN 5.34 rule 7).
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
		status, input, state, limits, attempt, created_at, updated_at, project_id, project_snapshot,
		merge_request_id, checkout_sha, park_reason, park_detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		r.ID, r.WorkflowID, r.WorkflowVersion, r.WorkflowSnapshot, r.Trigger, r.TriggerRef,
		r.Status, r.Input, r.State, r.Limits, r.Attempt, r.CreatedAt, r.UpdatedAt, r.ProjectID, r.Project,
		r.MergeRequestID, r.CheckoutSHA, r.ParkReason, r.ParkDetail)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && (pgErr.ConstraintName == "runs_schedule_occurrence_idx" || pgErr.ConstraintName == "runs_plan_wave_idx") {
		return ErrDuplicateTrigger
	}
	return liveIndexError(err)
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

// GetRunForUpdate reads a run and locks its row until the transaction ends (SELECT ...
// FOR UPDATE), so a check on the run and the update that follows it are one step
// (DESIGN 5.21, Consistency). q must be a transaction; ErrNotFound when there is no run.
func GetRunForUpdate(ctx context.Context, q Querier, id string) (*Run, error) {
	rows, err := q.Query(ctx, `SELECT `+runColumns+` FROM runs WHERE id=$1 FOR UPDATE`, id)
	if err != nil {
		return nil, err
	}
	r, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Run])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// RetryRun re-queues a failed, timed-out or cancelled run for a retry from the step it
// stopped at (DESIGN 5.21 rule 3): one conditional update, WHERE the status is still one
// of the three, that returns the row. ErrNotFound means the run is gone or no longer in
// one of them (a concurrent retry won). The run keeps its state, its current_node and
// its counters; what ended it is cleared: failure_reason, finished_at, the cancel flag
// and who set it, resume_node_visit_id (an approval node reached again asks again, 5.6
// rule 8) and reported_at (the report of the new outcome is sent once more, rule 6).
// retries goes up by one and attempt back to 1, so the crash re-queue of 5.1 counts per
// retry. started_at and the pause bookkeeping are cleared too: the time limit restarts
// when the run is next claimed (rule 2, ClaimQueuedRun). limits, when not nil, replace
// the run's limits. now stamps updated_at. A retry re-enters the two live-run indexes
// (DESIGN 5.22 rule 1, 5.23 rule 3): ErrMergeRequestBusy when another live run names the
// same merge request, ErrIssueBusy when another live run exists for the same issue; the
// run stays as it was. The move to queued increments the lease epoch (DESIGN 5.33 rule 7).
func RetryRun(ctx context.Context, q Querier, id string, limits *Limits, now time.Time) (*Run, error) {
	rows, err := q.Query(ctx, `UPDATE runs SET status=$2, retries=retries+1, attempt=1, failure_reason=NULL,
		finished_at=NULL, cancel_requested=false, cancel_by=NULL, resume_node_visit_id=NULL, reported_at=NULL,
		started_at=NULL, paused_at=NULL, paused_total_ms=0, limits=COALESCE($3::jsonb, limits), updated_at=$4,
		lease_epoch=lease_epoch+1
		WHERE id=$1 AND status IN ($5, $6, $7) RETURNING `+runColumns,
		id, RunQueued, limits, now.UTC(), RunFailed, RunTimedOut, RunCancelled)
	if err != nil {
		return nil, err
	}
	r, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Run])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, liveIndexError(err)
	}
	return r, nil
}

// RunFilter narrows ListRuns; an empty member does not filter. Trigger matches runs.trigger
// (manual, schedule, channel, eval, issue) and MergeRequestID the merge request record a
// run names (runs.merge_request_id, DESIGN 5.22 rule 1), not the record a run opened.
// A Limit outside 1..500 is 100.
type RunFilter struct {
	Status         string
	ProjectID      string
	Trigger        string
	MergeRequestID string
	Limit          int
}

// ListRuns returns the newest runs first, optionally filtered by status, by the project
// the run works on (runs.project_id, which a deleted project's runs keep), by trigger
// and by the merge request the run names; an empty filter matches every run.
func ListRuns(ctx context.Context, q Querier, f RunFilter) ([]Run, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.Query(ctx, `SELECT `+runColumns+` FROM runs WHERE ($1 = '' OR status = $1) AND ($2 = '' OR project_id = $2)
		AND ($3 = '' OR trigger = $3) AND ($4 = '' OR merge_request_id = $4)
		ORDER BY created_at DESC LIMIT $5`, f.Status, f.ProjectID, f.Trigger, f.MergeRequestID, limit)
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
//
// A first claim stamps started_at when it is empty. A retried run (retries > 0, DESIGN
// 5.21 rule 2) comes back with started_at cleared and keeps it empty here: the engine
// stamps it from its injected clock as the run starts (runRun), so the time limit
// restarts from that instant exactly as it does for a run claimed the first time.
//
// The claim increments the run's lease epoch (DESIGN 5.33 rule 7): the returned row carries
// the epoch the worker's writes are fenced with.
func ClaimQueuedRun(ctx context.Context, pool *pgxpool.Pool) (*Run, error) {
	rows, err := pool.Query(ctx, `UPDATE runs SET status=$1, lease_epoch=lease_epoch+1,
		started_at=CASE WHEN retries = 0 THEN COALESCE(started_at, now()) ELSE started_at END, updated_at=now()
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

// AddRunUsage increments counters and tokens in one statement (DESIGN 5.2 consistency),
// fenced by the worker's lease (DESIGN 5.33 rule 8).
func AddRunUsage(ctx context.Context, q Querier, id string, steps, toolCalls int, in, out, cacheRead, cacheWrite, costMicros int64) error {
	l := fence(ctx, id)
	_, epoch := fenceArgs(l)
	tag, err := q.Exec(ctx, `UPDATE runs SET step_count=step_count+$2, tool_call_count=tool_call_count+$3,
		input_tokens=input_tokens+$4, output_tokens=output_tokens+$5, cache_read_tokens=cache_read_tokens+$6,
		cache_write_tokens=cache_write_tokens+$7, cost_usd_micros=cost_usd_micros+$8, updated_at=now() WHERE id=$1 AND `+runFence("$9"),
		id, steps, toolCalls, in, out, cacheRead, cacheWrite, costMicros, epoch)
	if err == nil && tag.RowsAffected() == 0 {
		return lostOr(ctx, q, l, nil)
	}
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

// ResumeRun re-queues a paused run and accumulates the paused time; the move to queued
// increments the lease epoch (DESIGN 5.33 rule 7).
func ResumeRun(ctx context.Context, q Querier, id string) error {
	_, err := q.Exec(ctx, `UPDATE runs SET status=$2, lease_epoch=lease_epoch+1,
		paused_total_ms = paused_total_ms + COALESCE(EXTRACT(EPOCH FROM (now() - paused_at)) * 1000, 0)::bigint,
		paused_at=NULL, updated_at=now() WHERE id=$1 AND status=$3`, id, RunQueued, RunWaitingApproval)
	return err
}

// staleTurnsSQL marks the turns a lost worker may have left running (DESIGN 5.33 rule 12): the
// unfinished agent visits of the swept runs (the CTE stale) that recorded a turn and are not
// parked. The runs with one get cancel_state unconfirmed in the same statement.
const staleTurnsSQL = `turns AS (
		UPDATE run_nodes SET turn_unconfirmed = true
		WHERE run_id IN (SELECT id FROM stale) AND agent_id IS NOT NULL AND openclaw_run_id IS NOT NULL
		  AND finished_at IS NULL AND parked_at IS NULL AND status IN ('pending', 'running', 'waiting_approval')
		RETURNING run_id)`

// RequeueStaleRuns implements DESIGN 5.1 rule 8: running runs untouched for longer than
// staleAfter are re-queued once (attempt 2) and failed on the second detection. The sweep is
// the leases' expiry (DESIGN 5.33 rule 7): both moves increment the epoch, so the lost
// worker's late writes are refused, and a run whose last turn may still run gets
// cancel_state unconfirmed for it (rule 12).
func RequeueStaleRuns(ctx context.Context, q Querier, staleAfter time.Duration) (requeued, failed int64, err error) {
	cutoff := time.Now().UTC().Add(-staleAfter)
	tag, err := q.Exec(ctx, `WITH stale AS (SELECT id FROM runs WHERE status=$2 AND updated_at < $3 AND attempt < 2 FOR UPDATE),
		`+staleTurnsSQL+`
		UPDATE runs SET status=$1, attempt=attempt+1, lease_epoch=lease_epoch+1, updated_at=now(),
			cancel_state = CASE WHEN id IN (SELECT run_id FROM turns) THEN $4 ELSE cancel_state END
		WHERE id IN (SELECT id FROM stale)`, RunQueued, RunRunning, cutoff, CancelUnconfirmed)
	if err != nil {
		return 0, 0, err
	}
	requeued = tag.RowsAffected()
	tag, err = q.Exec(ctx, `WITH stale AS (SELECT id FROM runs WHERE status=$2 AND updated_at < $3 AND attempt >= 2 FOR UPDATE),
		`+staleTurnsSQL+`
		UPDATE runs SET status=$1, failure_reason='worker lost', finished_at=now(), updated_at=now(), lease_epoch=lease_epoch+1,
			cancel_state = CASE WHEN id IN (SELECT run_id FROM turns) THEN $4 ELSE cancel_state END
		WHERE id IN (SELECT id FROM stale)`, RunFailed, RunRunning, cutoff, CancelUnconfirmed)
	if err != nil {
		return requeued, 0, err
	}
	return requeued, tag.RowsAffected(), nil
}

// ---- run nodes --------------------------------------------------------------------

// runNodeInsertColumns are the columns CreateRunNode writes; runNodeColumns adds the
// recovery columns (DESIGN 5.33) a visit row reads back.
const runNodeInsertColumns = `id, run_id, node_id, agent_id, visit, iteration, status, messages, pending_tool_results, output,
	failure_reason, session_key, openclaw_run_id, started_at, finished_at, question_id, parked_at, branch_id,
	workspace_dir, repo_sha`

const runNodeColumns = runNodeInsertColumns + `, park_reason, resumed_at, resume_note, start_sha, retry_from_at, turn_unconfirmed`

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
	_, err := q.Exec(ctx, `INSERT INTO run_nodes (`+runNodeInsertColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		n.ID, n.RunID, n.NodeID, n.AgentID, n.Visit, n.Iteration, n.Status, n.Messages, n.PendingToolResults, n.Output,
		n.FailureReason, n.SessionKey, n.OpenClawRunID, n.StartedAt, n.FinishedAt, n.QuestionID, n.ParkedAt, n.BranchID,
		n.WorkspaceDir, n.RepoSHA)
	return err
}

// SetRunNodeWorkspace records a visit's private workspace (DESIGN 5.30 rule 2), before
// anything is written into it, so cleanup finds every folder a visit may have made.
func SetRunNodeWorkspace(ctx context.Context, q Querier, id, dir string) error {
	_, err := q.Exec(ctx, `UPDATE run_nodes SET workspace_dir=$2 WHERE id=$1`, id, dir)
	return err
}

// SetRunNodeRepoSHA records the run repository commit a visit's checkout ended at (DESIGN
// 5.30 rule 4), fenced by the worker's lease (DESIGN 5.33 rule 8).
func SetRunNodeRepoSHA(ctx context.Context, q Querier, id, sha string) error {
	l := LeaseOf(ctx)
	r, e := fenceArgs(l)
	tag, err := q.Exec(ctx, `UPDATE run_nodes SET repo_sha=$2 WHERE id=$1 AND `+childFence("run_id", "$3", "$4"), id, sha, r, e)
	if err == nil && tag.RowsAffected() == 0 {
		return lostOr(ctx, q, l, nil)
	}
	return err
}

// SetRunNodeStartSHA records the commit a visit's checkout starts from (DESIGN 5.30 rule 4):
// start_sha, kept for a retry from the step (DESIGN 5.33 rule 17), and repo_sha, which the
// visit's end replaces. Fenced like SetRunNodeRepoSHA.
func SetRunNodeStartSHA(ctx context.Context, q Querier, id, sha string) error {
	l := LeaseOf(ctx)
	r, e := fenceArgs(l)
	tag, err := q.Exec(ctx, `UPDATE run_nodes SET start_sha=$2, repo_sha=$2 WHERE id=$1 AND `+childFence("run_id", "$3", "$4"), id, sha, r, e)
	if err == nil && tag.RowsAffected() == 0 {
		return lostOr(ctx, q, l, nil)
	}
	return err
}

// MarkTurnUnconfirmed records that a visit's turn was not confirmed stopped (DESIGN 5.33 rule
// 11): the reconcile pass checks it until the gateway shows no active run for it. Not fenced:
// it is a record of what happened, written also after the lease is gone.
func MarkTurnUnconfirmed(ctx context.Context, q Querier, id string) error {
	_, err := q.Exec(ctx, `UPDATE run_nodes SET turn_unconfirmed=true WHERE id=$1`, id)
	return err
}

// RunWorkspaceDirs lists the private workspaces the visits of a run recorded (DESIGN 5.30
// rule 8).
func RunWorkspaceDirs(ctx context.Context, q Querier, runID string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT workspace_dir FROM run_nodes WHERE run_id=$1 AND workspace_dir IS NOT NULL ORDER BY started_at NULLS LAST, id`, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// WorkspaceOwners maps each of dirs that a visit recorded as its private workspace to the
// visit's run (DESIGN 5.30 rule 8); a folder no visit recorded is not in the map.
func WorkspaceOwners(ctx context.Context, q Querier, dirs []string) (map[string]string, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT workspace_dir, run_id FROM run_nodes WHERE workspace_dir = ANY($1::text[])`, dirs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var dir, runID string
		if err := rows.Scan(&dir, &runID); err != nil {
			return nil, err
		}
		out[dir] = runID
	}
	return out, rows.Err()
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
	idempotency_key, attempt, duration_ms, started_at, finished_at, conversation_id`

// toolCallSelect reads a tool call for scanning. A chat-time call has no run or node
// visit (DESIGN 5.15 rule 3): they read as "".
const toolCallSelect = `id, COALESCE(run_id, '') AS run_id, COALESCE(run_node_id, '') AS run_node_id, model_call_id,
	tool_use_id, tool_name, input, output, is_error, status, idempotency_key, attempt, duration_ms, started_at, finished_at,
	conversation_id`

// InsertToolCall stores a tool call; an empty RunID and RunNodeID are stored as NULL (a
// chat-time call).
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
		VALUES ($1,NULLIF($2,''),NULLIF($3,''),$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		t.ID, t.RunID, t.RunNodeID, t.ModelCallID, t.ToolUseID, t.ToolName, t.Input, t.Output, t.IsError, t.Status,
		t.IdempotencyKey, t.Attempt, t.DurationMs, t.StartedAt, t.FinishedAt, t.ConversationID)
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
	rows, err := q.Query(ctx, `SELECT `+toolCallSelect+` FROM tool_calls WHERE run_id=$1 AND tool_use_id=$2 ORDER BY id DESC LIMIT 1`, runID, toolUseID)
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
	rows, err := q.Query(ctx, `SELECT `+toolCallSelect+` FROM tool_calls WHERE run_id=$1 ORDER BY started_at NULLS LAST, id`, runID)
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
	decided_at, decided_by, reason, expires_at, options, answer, binding`

// approvalSelect reads an approval for scanning. One raised in a conversation, and a
// platform approval (wave 1 A6), has no run or run node (DESIGN 5.8 rule 5): they read as "".
const approvalSelect = `id, COALESCE(run_id, '') AS run_id, COALESCE(run_node_id, '') AS run_node_id, tool_call_id, kind,
	node_id, message, options, status, external_id, requested_at, decided_at, decided_by, reason, answer, expires_at,
	conversation_id, reported_at, binding, used_at`

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
	_, err := q.Exec(ctx, `INSERT INTO approvals (`+approvalColumns+`) VALUES ($1,NULLIF($2,''),NULLIF($3,''),$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15::jsonb,$16,$17::jsonb)`,
		a.ID, a.RunID, a.RunNodeID, a.ToolCallID, a.Kind, a.NodeID, a.Message, a.Status, a.ExternalID, a.RequestedAt,
		a.DecidedAt, a.DecidedBy, a.Reason, a.ExpiresAt, optionsParam(a.Options), a.Answer, optionsParam(a.Binding))
	return err
}

// optionsParam is the value of a nullable jsonb column of approvals: options, the JSON
// array of a question's suggested answers, or binding, a platform approval's call; NULL
// (not an empty document) when it has none.
func optionsParam(options json.RawMessage) any {
	if len(options) == 0 {
		return nil
	}
	return []byte(options)
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
	if t.Branches, err = ListRunBranches(ctx, q, runID); err != nil {
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
	// An OpenAI call is named as the agent's model is, openai/<id> (D-091); the proxy's
	// Anthropic calls keep the bare id the agents use.
	models, err := collectAggregates(ctx, q, `SELECT x.run_id, array_agg(x.model ORDER BY x.first) FROM (
		SELECT run_id, CASE WHEN provider = 'openai' THEN 'openai/' || model ELSE model END AS model,
		min(started_at) AS first FROM model_calls WHERE run_id = ANY($1)
		GROUP BY 1, 2) x GROUP BY x.run_id`, ids)
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
