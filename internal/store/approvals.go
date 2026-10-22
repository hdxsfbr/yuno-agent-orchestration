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
var (
	countStepQuestionsSQL = `SELECT count(*) FROM approvals WHERE kind='question' AND run_id=$1 AND node_id=$2
		AND ` + sinceRetry("approvals")

	listStepQuestionsSQL = `SELECT ` + approvalSelect + ` FROM approvals
		WHERE kind='question' AND run_id=$1 AND node_id=$2 AND status <> $3
		  AND ` + sinceRetry("approvals") + `
		ORDER BY requested_at, id`

	continuationQuestionSQL = `SELECT ` + approvalSelect + ` FROM approvals
		WHERE kind='question' AND run_id=$1 AND node_id=$2 AND status IN ($3, $4)
		  AND EXISTS (SELECT 1 FROM run_nodes p WHERE p.id = approvals.run_node_id AND p.parked_at IS NOT NULL)
		  AND NOT EXISTS (SELECT 1 FROM run_nodes n WHERE n.question_id = approvals.id)
		  AND NOT EXISTS (SELECT 1 FROM approvals later WHERE later.kind = 'question' AND later.run_node_id = approvals.run_node_id
		                  AND (later.requested_at, later.id) > (approvals.requested_at, approvals.id))
		  AND ` + visitSettled("approvals.run_node_id") + `
		  AND ` + sinceRetry("approvals") + `
		ORDER BY requested_at DESC, id DESC LIMIT 1`

	// continuationToolSQL is the continuation of a visit that parked on its tool calls'
	// approvals (DESIGN 5.3 rule 5): its newest decided tool approval, once nothing of the
	// visit waits any more, while no visit continues the parked one yet (a continuation may
	// name its question instead). The first EXISTS reads no approval for a step that never
	// parked, the usual case; a parked visit that failed (its run was cancelled or failed)
	// is retried, not continued.
	continuationToolSQL = `SELECT ` + approvalSelect + ` FROM approvals
		WHERE kind='tool' AND run_id=$1 AND node_id=$2 AND status <> 'pending'
		  AND EXISTS (SELECT 1 FROM run_nodes g WHERE g.run_id = $1 AND g.node_id = $2 AND g.parked_at IS NOT NULL)
		  AND EXISTS (SELECT 1 FROM run_nodes p WHERE p.id = approvals.run_node_id AND p.parked_at IS NOT NULL
		              AND p.status IN ('waiting_approval', 'succeeded'))
		  AND NOT EXISTS (SELECT 1 FROM run_nodes n JOIN approvals x ON x.id = n.question_id WHERE x.run_node_id = approvals.run_node_id)
		  AND ` + visitSettled("approvals.run_node_id") + `
		  AND ` + sinceRetry("approvals") + `
		ORDER BY requested_at DESC, id DESC LIMIT 1`
)

// sinceRetry is the predicate that the approval whose table alias is a was raised after the
// operator last retried its step (DESIGN 5.21 rule 4, D-093): no visit of the step at or
// after the one that raised it is marked retried_at. A retry starts the step afresh, so what
// the attempt it replaced asked and was told is carried, continued and counted no more.
func sinceRetry(a string) string {
	return `NOT EXISTS (SELECT 1 FROM run_nodes asker JOIN run_nodes rv ON rv.run_id = asker.run_id AND rv.node_id = asker.node_id
		              AND rv.retried_at IS NOT NULL AND rv.visit >= asker.visit
		            WHERE asker.id = ` + a + `.run_node_id)`
}

// visitSettled is the predicate that the visit whose id the SQL expression visit gives
// waits on nothing (DESIGN 5.3 rule 5, 5.24 rule 5): none of its questions or tool
// approvals is pending, and no approved tool call of it is still executing (its row still
// awaiting_approval while the engine runs the effect).
func visitSettled(visit string) string {
	return `NOT EXISTS (SELECT 1 FROM approvals u LEFT JOIN tool_calls c ON c.id = u.tool_call_id
		WHERE u.run_node_id = ` + visit + ` AND u.kind IN ('question', 'tool')
		  AND (u.status = 'pending' OR (u.kind = 'tool' AND u.status = 'approved' AND c.status = 'awaiting_approval')))`
}

// CountStepQuestions counts the questions a step has asked in a run since the operator last
// retried it: the rows of kind question with the run and node id, whatever their status
// (rule 2: the fourth is refused; a retry starts the count afresh, D-093). The partial index
// approvals_question_step_idx serves it.
func CountStepQuestions(ctx context.Context, q Querier, runID, nodeID string) (int, error) {
	var n int
	err := q.QueryRow(ctx, countStepQuestionsSQL, runID, nodeID).Scan(&n)
	return n, err
}

// ListStepQuestions lists the step's decided questions, oldest first: the rows of kind
// question with the run and node id that are no longer pending, whichever visit asked
// them since the operator last retried the step (rule 7's answer paragraph has one block
// for each; D-093).
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
// a question the visit went on past (answered within the wait) is not the one it parked
// on, and nothing raised before the operator last retried the step is (D-093: a decision
// that landed as the run ended, before its continuation started, does not make the retried
// visit one).
//
// A visit also parks on the approvals of tool calls it was told are pending (DESIGN 5.3
// rule 5): it is continued only once nothing of it waits any more, and when it has no
// question to name, the continuation names its newest decided tool approval. Either way
// the approval's run_node_id is the parked visit.
func ContinuationQuestion(ctx context.Context, q Querier, runID, nodeID string) (*Approval, error) {
	a, err := continuation(ctx, q, continuationQuestionSQL, runID, nodeID, ApprovalApproved, ApprovalRejected)
	if !errors.Is(err, ErrNotFound) {
		return a, err
	}
	return continuation(ctx, q, continuationToolSQL, runID, nodeID)
}

func continuation(ctx context.Context, q Querier, sql string, args ...any) (*Approval, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Approval])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// ListVisitToolApprovals lists a visit's tool approvals, oldest first: what a continuation
// of the visit is told about its gated calls (DESIGN 5.3 rule 5).
func ListVisitToolApprovals(ctx context.Context, q Querier, runNodeID string) ([]Approval, error) {
	rows, err := q.Query(ctx, `SELECT `+approvalSelect+` FROM approvals WHERE run_node_id = $1 AND kind = $2
		ORDER BY requested_at, id`, runNodeID, ApprovalKindTool)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Approval])
}

// ListParkedToolApprovals returns the decided tool approvals of every visit a run waits on
// parked (DESIGN 5.3 rule 5, the parked visits of ListParkedQuestions): the decisions whose
// re-queue the sweep repeats, and the approved calls whose effect it runs again when the
// process that ran it is gone (the row still awaiting_approval).
func ListParkedToolApprovals(ctx context.Context, q Querier) ([]Approval, error) {
	rows, err := q.Query(ctx, `SELECT `+approvalSelect+` FROM approvals WHERE kind = $2 AND status <> $1 AND run_node_id IN (
		SELECT n.id FROM runs r JOIN run_nodes n ON n.run_id = r.id AND n.parked_at IS NOT NULL AND n.status = $4
		  AND (n.id = r.resume_node_visit_id
		       OR r.resume_node_visit_id = (SELECT b.fork_visit_id FROM run_branches b WHERE b.id = n.branch_id))
		WHERE r.status = $3)
		ORDER BY decided_at`, ApprovalPending, ApprovalKindTool, RunWaitingApproval, NodeWaitingApproval)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Approval])
	if out == nil {
		out = []Approval{}
	}
	return out, err
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
