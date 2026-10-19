package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// PlanEventChangesRequested records a change request on a ticket's merge request (DESIGN
// 5.29, changes requested): the detail carries the notes and the run that iterates.
const PlanEventChangesRequested = "changes_requested"

// LinkTicketIteration links the run that iterates on the ticket's merge request and sets
// the ticket building again, from in_review only. It reports whether the ticket moved.
func LinkTicketIteration(ctx context.Context, q Querier, planID, ticketID, runID string) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE plan_tickets SET run_id=$3, status='building', updated_at=now()
		WHERE plan_id=$1 AND ticket_id=$2 AND status='in_review'`, planID, ticketID, runID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// LiveRunOnMergeRequest returns the id of a run that is not finished and works on the merge
// request (the run that opened it, or an iteration), "" when none does.
func LiveRunOnMergeRequest(ctx context.Context, q Querier, mergeRequestID, openerRunID string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT id FROM runs WHERE (merge_request_id=$1 OR id=$2)
		AND status NOT IN ('succeeded','failed','cancelled','timed_out') LIMIT 1`, mergeRequestID, openerRunID).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return id, nil
}

// SetTicketContentHash records the hash of the text the ticket's issue was written from.
func SetTicketContentHash(ctx context.Context, q Querier, planID, ticketID, hash string) error {
	return execPlanUpdate(ctx, q, `UPDATE plan_tickets SET content_hash=$3 WHERE plan_id=$1 AND ticket_id=$2`, planID, ticketID, hash)
}

// TicketContentHashes returns the recorded content hash per ticket id of the plan; a ticket
// without one is absent.
func TicketContentHashes(ctx context.Context, q Querier, planID string) (map[string]string, error) {
	rows, err := q.Query(ctx, `SELECT ticket_id, content_hash FROM plan_tickets WHERE plan_id=$1 AND content_hash IS NOT NULL`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, h string
		if err := rows.Scan(&id, &h); err != nil {
			return nil, err
		}
		out[id] = h
	}
	return out, rows.Err()
}
