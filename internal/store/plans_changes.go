package store

import (
	"context"
	"encoding/json"
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

// PlanEventFollowUp records a ticket that carries a revision's change to a merged or closed
// ticket (DESIGN 5.29 rule 16): ticket_id is the follow-up, the detail names the ticket it
// follows and the content hash of the revised text it carries.
const PlanEventFollowUp = "follow_up"

// FollowUp is one recorded follow-up: the ticket, the ticket it follows and the hash of the
// revised text of that ticket it carries.
type FollowUp struct {
	TicketID string `json:"-"`
	Follows  string `json:"follows"`
	Hash     string `json:"hash"`
}

// ListFollowUps returns the plan's recorded follow-ups, oldest first. A follow-up that took
// a further change in a later revision is recorded again; its last record is the one that
// counts.
func ListFollowUps(ctx context.Context, q Querier, planID string) ([]FollowUp, error) {
	rows, err := q.Query(ctx, `SELECT ticket_id, detail FROM plan_events
		WHERE plan_id=$1 AND kind=$2 AND ticket_id IS NOT NULL ORDER BY at, id`, planID, PlanEventFollowUp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FollowUp
	for rows.Next() {
		var f FollowUp
		var detail []byte
		if err := rows.Scan(&f.TicketID, &detail); err != nil {
			return nil, err
		}
		if json.Unmarshal(detail, &f) != nil || f.Follows == "" {
			continue
		}
		out = append(out, f)
	}
	return out, rows.Err()
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
