package store

import "context"

// ReturnTicketToReview sets the ticket the run iterates on back to in_review once the
// iteration pushed onto the ticket's existing merge request (DESIGN 5.29, changes
// requested). LinkTicketIteration set it building; the request is already linked, so
// LinkTicketMergeRequest never runs again. It reports whether the ticket moved.
func ReturnTicketToReview(ctx context.Context, q Querier, runID string) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE plan_tickets SET status='in_review', updated_at=now()
		WHERE run_id=$1 AND status='building' AND merge_request_id IS NOT NULL`, runID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
