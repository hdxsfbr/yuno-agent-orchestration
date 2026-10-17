package store

import "context"

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
