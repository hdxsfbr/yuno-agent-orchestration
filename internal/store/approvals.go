package store

import "context"

// ApprovalItem is an approval as the inbox lists it: with the gated tool call joined
// when there is one, so the operator sees the tool's name and full input (DESIGN 5.3).
type ApprovalItem struct {
	Approval
	ToolCall *ToolCall `json:"tool_call,omitempty"`
}

// ListApprovalItems lists approvals newest first, filtered by status and run ("" for
// any), each with its tool call.
func ListApprovalItems(ctx context.Context, q Querier, status, runID string, limit int) ([]ApprovalItem, error) {
	approvals, err := ListApprovals(ctx, q, status, runID, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, a := range approvals {
		if a.ToolCallID != nil {
			ids = append(ids, *a.ToolCallID)
		}
	}
	calls, err := ListToolCallsByIDs(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	out := make([]ApprovalItem, len(approvals))
	for i, a := range approvals {
		out[i].Approval = a
		if a.ToolCallID != nil {
			if tc, ok := calls[*a.ToolCallID]; ok {
				out[i].ToolCall = &tc
			}
		}
	}
	return out, nil
}
