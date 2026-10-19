package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ApprovalItem is an approval as the inbox lists it: with the gated tool call joined
// when there is one, so the operator sees the tool's name and full input (DESIGN 5.3),
// and the status of its run, so a tool approval on a run that has already ended can
// say that approving still executes the call (DESIGN 5.17 rule 12). RunStatus is empty
// for an approval raised in a conversation.
type ApprovalItem struct {
	Approval
	ToolCall  *ToolCall `json:"tool_call,omitempty"`
	RunStatus string    `json:"run_status,omitempty"`
}

// ListApprovalItems lists approvals newest first, filtered by status and run ("" for
// any), each with its tool call and its run's status.
func ListApprovalItems(ctx context.Context, q Querier, status, runID string, limit int) ([]ApprovalItem, error) {
	approvals, err := ListApprovals(ctx, q, status, runID, limit)
	if err != nil {
		return nil, err
	}
	var ids, runIDs []string
	for _, a := range approvals {
		if a.ToolCallID != nil {
			ids = append(ids, *a.ToolCallID)
		}
		if a.RunID != "" {
			runIDs = append(runIDs, a.RunID)
		}
	}
	calls, err := ListToolCallsByIDs(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	statuses, err := runStatuses(ctx, q, runIDs)
	if err != nil {
		return nil, err
	}
	out := make([]ApprovalItem, len(approvals))
	for i, a := range approvals {
		out[i].Approval = a
		out[i].RunStatus = statuses[a.RunID]
		if a.ToolCallID != nil {
			if tc, ok := calls[*a.ToolCallID]; ok {
				out[i].ToolCall = &tc
			}
		}
	}
	return out, nil
}

// runStatuses returns the status of each listed run, in one query.
func runStatuses(ctx context.Context, q Querier, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT id, status FROM runs WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			return nil, err
		}
		out[id] = status
	}
	return out, rows.Err()
}

// ---- questions to the operator (DESIGN 5.24) ------------------------------------------

// The step queries write the kind as the literal 'question' (the value of
// ApprovalKindQuestion; a test keeps them in step): bound as a parameter it would keep the
// planner from using the partial index approvals_question_step_idx once it settles on a
// generic plan (review L6).
const (
	countStepQuestionsSQL = `SELECT count(*) FROM approvals WHERE kind='question' AND run_id=$1 AND node_id=$2`

	listStepQuestionsSQL = `SELECT ` + approvalSelect + ` FROM approvals
		WHERE kind='question' AND run_id=$1 AND node_id=$2 AND status <> $3 ORDER BY requested_at, id`

	continuationQuestionSQL = `SELECT ` + approvalSelect + ` FROM approvals
		WHERE kind='question' AND run_id=$1 AND node_id=$2 AND status IN ($3, $4)
		  AND EXISTS (SELECT 1 FROM run_nodes p WHERE p.id = approvals.run_node_id AND p.parked_at IS NOT NULL)
		  AND NOT EXISTS (SELECT 1 FROM run_nodes n WHERE n.question_id = approvals.id)
		  AND NOT EXISTS (SELECT 1 FROM approvals later WHERE later.kind = 'question' AND later.run_node_id = approvals.run_node_id
		                  AND (later.requested_at, later.id) > (approvals.requested_at, approvals.id))
		ORDER BY requested_at DESC, id DESC LIMIT 1`
)

// CountStepQuestions counts the questions a step has asked in a run: the rows of kind
// question with the run and node id, whatever their status (rule 2: the fourth is
// refused). The partial index approvals_question_step_idx serves it.
func CountStepQuestions(ctx context.Context, q Querier, runID, nodeID string) (int, error) {
	var n int
	err := q.QueryRow(ctx, countStepQuestionsSQL, runID, nodeID).Scan(&n)
	return n, err
}

// ListStepQuestions lists the step's decided questions, oldest first: the rows of kind
// question with the run and node id that are no longer pending, whichever visit asked
// them (rule 7's answer paragraph has one block for each).
func ListStepQuestions(ctx context.Context, q Querier, runID, nodeID string) ([]Approval, error) {
	rows, err := q.Query(ctx, listStepQuestionsSQL, runID, nodeID, ApprovalPending)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Approval])
	if out == nil {
		out = []Approval{}
	}
	return out, err
}

// ContinuationQuestion is the question a new visit of the step continues (DESIGN 5.24
// rule 5): the newest question of the step that was answered or declined, that the visit
// which asked it parked on (the last question of a visit whose parked_at is set), and
// that no visit names yet. ErrNotFound for a loop revisit or a retry, which continue
// nothing: an expired question is not one (a retry after rule 8 is not a continuation),
// and a question the visit went on past (answered within the wait) is not the one it
// parked on.
func ContinuationQuestion(ctx context.Context, q Querier, runID, nodeID string) (*Approval, error) {
	rows, err := q.Query(ctx, continuationQuestionSQL, runID, nodeID, ApprovalApproved, ApprovalRejected)
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Approval])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// ListParkedQuestions returns, for every run that waits on a parked visit, that visit's
// newest question when it is no longer pending: the decisions whose re-queue (or, for an
// expiry, whose failure) the sweep repeats (DESIGN 5.24 Failure handling).
func ListParkedQuestions(ctx context.Context, q Querier) ([]Approval, error) {
	// A run waits on the parked visit itself, or on the fork visit whose branch holds a
	// parked visit (DESIGN 5.26 rule 8): both kinds of parked visit are listed.
	rows, err := q.Query(ctx, `SELECT `+approvalSelect+` FROM approvals WHERE status <> $1 AND id IN (
		SELECT (SELECT a.id FROM approvals a WHERE a.run_node_id = n.id AND a.kind = $2
		        ORDER BY a.requested_at DESC, a.id DESC LIMIT 1)
		FROM runs r JOIN run_nodes n ON n.run_id = r.id AND n.parked_at IS NOT NULL AND n.status = $4
		  AND (n.id = r.resume_node_visit_id
		       OR r.resume_node_visit_id = (SELECT b.fork_visit_id FROM run_branches b WHERE b.id = n.branch_id))
		WHERE r.status = $3)
		ORDER BY decided_at`, ApprovalPending, ApprovalKindQuestion, RunWaitingApproval, NodeWaitingApproval)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Approval])
	if out == nil {
		out = []Approval{}
	}
	return out, err
}
