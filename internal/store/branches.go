package store

import (
	"context"
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
