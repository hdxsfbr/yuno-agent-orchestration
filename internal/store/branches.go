package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Branches of a fork visit (DESIGN 5.26 rule 3): one row per branch, written by the
// engine's fork and read by the trace, the Chief's run_status and the engine's re-entry.

// branchColumns are the columns of run_branches in the order RunBranch lists them.
const branchColumns = `id, run_id, fork_visit_id, fork_id, head, status, current_node, failure_reason, started_at, finished_at`

// ListRunBranches returns every branch row of the run by started_at then id; an empty
// slice, not nil, for a run without forks.
func ListRunBranches(ctx context.Context, q Querier, runID string) ([]RunBranch, error) {
	return listBranches(ctx, q, `SELECT `+branchColumns+` FROM run_branches WHERE run_id=$1 ORDER BY started_at, id`, runID)
}

// ListForkBranches returns the branch rows of one fork visit in the same order.
func ListForkBranches(ctx context.Context, q Querier, forkVisitID string) ([]RunBranch, error) {
	return listBranches(ctx, q, `SELECT `+branchColumns+` FROM run_branches WHERE fork_visit_id=$1 ORDER BY started_at, id`, forkVisitID)
}

func listBranches(ctx context.Context, q Querier, sql string, args ...any) ([]RunBranch, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[RunBranch])
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []RunBranch{}
	}
	return out, nil
}

// InsertRunBranch stores one branch row; an empty id and status take a new id and
// running, an empty current_node the head, a zero started_at now.
func InsertRunBranch(ctx context.Context, q Querier, b *RunBranch) error {
	if b.ID == "" {
		b.ID = NewID()
	}
	if b.Status == "" {
		b.Status = BranchRunning
	}
	if b.CurrentNode == "" {
		b.CurrentNode = b.Head
	}
	if b.StartedAt.IsZero() {
		b.StartedAt = time.Now().UTC()
	}
	_, err := q.Exec(ctx, `INSERT INTO run_branches (`+branchColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		b.ID, b.RunID, b.ForkVisitID, b.ForkID, b.Head, b.Status, b.CurrentNode, b.FailureReason, b.StartedAt, b.FinishedAt)
	return err
}

// beginner is what the one-transaction writes of this file need from a pool or tx.
type beginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// CreateForkVisit stores a fork's visit row and its branch rows in one transaction
// (DESIGN 5.26 rule 3, Consistency), so no branch exists without its fork visit.
func CreateForkVisit(ctx context.Context, db beginner, fork *RunNode, branches []*RunBranch) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit
	if err := CreateRunNode(ctx, tx, fork); err != nil {
		return err
	}
	for _, b := range branches {
		b.ForkVisitID = fork.ID
		if err := InsertRunBranch(ctx, tx, b); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ReopenRunNode reopens a fork's visit row on a re-entry (DESIGN 5.26 rule 9): running,
// with finished_at and failure_reason cleared.
func ReopenRunNode(ctx context.Context, q Querier, id string) (*RunNode, error) {
	rows, err := q.Query(ctx, `UPDATE run_nodes SET status=$2, finished_at=NULL, failure_reason=NULL WHERE id=$1 RETURNING `+runNodeColumns, id, NodeRunning)
	if err != nil {
		return nil, err
	}
	n, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[RunNode])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

// SetBranchCurrent writes where a branch is (DESIGN 5.26 rule 4); fenced by the worker's
// lease through the branch's run (DESIGN W2 rule 8).
func SetBranchCurrent(ctx context.Context, q Querier, id, node string) error {
	l := LeaseOf(ctx)
	r, e := fenceArgs(l)
	tag, err := q.Exec(ctx, `UPDATE run_branches SET current_node=$2 WHERE id=$1 AND `+childFence("run_id", "$3", "$4"), id, node, r, e)
	if err == nil && tag.RowsAffected() == 0 {
		return lostOr(ctx, q, l, nil)
	}
	return err
}

// SetBranchStatus writes a branch's status: a terminal one (succeeded, failed, cancelled)
// with its finished_at and reason, running or waiting_approval with neither. Fenced by the
// worker's lease through the branch's run (DESIGN W2 rule 8).
func SetBranchStatus(ctx context.Context, q Querier, id, status string, reason *string, at time.Time) (*RunBranch, error) {
	var finished any
	if status == BranchSucceeded || status == BranchFailed || status == BranchCancelled {
		finished = at.UTC()
	}
	l := LeaseOf(ctx)
	r, e := fenceArgs(l)
	rows, err := q.Query(ctx, `UPDATE run_branches SET status=$2, failure_reason=$3, finished_at=$4 WHERE id=$1 AND `+
		childFence("run_id", "$5", "$6")+` RETURNING `+branchColumns, id, status, reason, finished, r, e)
	if err != nil {
		return nil, err
	}
	b, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[RunBranch])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, lostOr(ctx, q, l, ErrNotFound)
	}
	return b, err
}

// CloseOpenBranches closes every branch row of the run that is still running or waiting
// (DESIGN 5.26 rule 12), with the run's terminal reason, and returns how many it closed.
func CloseOpenBranches(ctx context.Context, q Querier, runID, status string, reason *string, at time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `UPDATE run_branches SET status=$2, failure_reason=COALESCE($3, failure_reason), finished_at=$4
		WHERE run_id=$1 AND status IN ($5, $6)`, runID, status, reason, at.UTC(), BranchRunning, BranchWaitingApproval)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// SetRunStateNodeInBranch is SetRunStateNode for a visit inside a branch (DESIGN 5.26
// rule 4): the node's output lands in runs.state under its id (key-wise, so a sibling's
// write is kept) and the branch's current_node moves, in one transaction; runs.current_node
// stays the fork's.
func SetRunStateNodeInBranch(ctx context.Context, db beginner, runID, nodeID string, output json.RawMessage, branchID, branchCurrent string) (*Run, error) {
	if len(output) == 0 {
		output = json.RawMessage(`null`)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit
	// A worker's write is fenced by its lease (DESIGN W2 rule 8).
	l := fence(ctx, runID)
	_, epoch := fenceArgs(l)
	run, err := collectRun(tx.Query(ctx, `UPDATE runs SET state = jsonb_set(CASE WHEN jsonb_typeof(state) = 'object' THEN state ELSE '{}'::jsonb END,
		ARRAY[$2::text], $3::jsonb, true), updated_at=now() WHERE id=$1 AND `+runFence("$4")+` RETURNING `+runColumns, runID, nodeID, output, epoch))
	if errors.Is(err, ErrNotFound) {
		return nil, lostOr(ctx, tx, l, err)
	}
	if err != nil {
		return nil, err
	}
	if err := SetBranchCurrent(ctx, tx, branchID, branchCurrent); err != nil {
		return nil, err
	}
	return run, tx.Commit(ctx)
}

// ParkBranchVisit parks a visit inside a branch on its pending question (DESIGN 5.26
// rule 8): the visit's row becomes waiting_approval with parked_at and its branch row
// waiting_approval, in one transaction; the run is left as it is (the fork releases it
// once every branch has ended or parked). ErrNotFound means the visit was not in a state
// that can park; nothing is stored.
func ParkBranchVisit(ctx context.Context, db beginner, runID, runNodeID, branchID string, at time.Time) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit
	// The park is the worker's write, fenced by its lease (DESIGN W2 rule 8).
	l := fence(ctx, runID)
	r, e := fenceArgs(l)
	tag, err := tx.Exec(ctx, `UPDATE run_nodes SET parked_at=$3, status=$4 WHERE id=$1 AND run_id=$2 AND status IN ($5, $4)
		AND `+childFence("run_id", "$6", "$7"), runNodeID, runID, at.UTC(), NodeWaitingApproval, NodeRunning, r, e)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return lostOr(ctx, tx, l, ErrNotFound)
	}
	if _, err := tx.Exec(ctx, `UPDATE run_branches SET status=$2 WHERE id=$1`, branchID, BranchWaitingApproval); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// parkedResumeSQL is the predicate shared by RequeueParkedRun, IsParkedOn and
// ListParkedQuestions (DESIGN 5.24 rule 5, 5.26 rule 8): the run waits with its
// resume_node_visit_id on the parked visit itself, or on the fork visit whose branch the
// parked visit belongs to. $1 is the run id, $2 the parked visit.
const parkedResumeSQL = `(runs.resume_node_visit_id = $2 OR runs.resume_node_visit_id = (
		SELECT b.fork_visit_id FROM run_nodes n JOIN run_branches b ON b.id = n.branch_id WHERE n.id = $2))`

// CloseBranchVisits fails the unfinished visits of a branch that was stopped (DESIGN
// 5.26 rule 6) with the stop reason, and returns how many it closed.
func CloseBranchVisits(ctx context.Context, q Querier, branchID, reason string, at time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `UPDATE run_nodes SET status=$2, failure_reason=$3, finished_at=$4
		WHERE branch_id=$1 AND status IN ($5, $6, $7)`, branchID, NodeFailed, reason, at.UTC(), NodePending, NodeRunning, NodeWaitingApproval)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// FailRunningNodesExcept is FailRunningNodes that leaves alone the one visit named and
// every visit still parked on its question (parked_at set, waiting_approval): the fork
// visit a run resumes at after its branches parked (DESIGN 5.26 rule 8) and the parked
// branch visits whose questions are still pending (rule 9) are not lost, they wait.
func FailRunningNodesExcept(ctx context.Context, q Querier, runID, exceptID, reason string, at time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `UPDATE run_nodes SET status=$2, failure_reason=$3, finished_at=$4
		WHERE run_id=$1 AND id <> $8 AND status IN ($5, $6, $7) AND NOT (parked_at IS NOT NULL AND status = $7)`,
		runID, NodeFailed, reason, at.UTC(), NodePending, NodeRunning, NodeWaitingApproval, exceptID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ParkedVisitOfBranch returns the branch's visit that is parked on its question (parked_at
// set and still waiting_approval), ErrNotFound when none is.
func ParkedVisitOfBranch(ctx context.Context, q Querier, branchID string) (*RunNode, error) {
	rows, err := q.Query(ctx, `SELECT `+runNodeColumns+` FROM run_nodes WHERE branch_id=$1 AND parked_at IS NOT NULL AND status=$2
		ORDER BY visit DESC LIMIT 1`, branchID, NodeWaitingApproval)
	if err != nil {
		return nil, err
	}
	n, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[RunNode])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}
