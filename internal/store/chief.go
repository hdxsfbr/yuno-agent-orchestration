package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Queries behind the Chief of Staff (DESIGN 5.15): chat-time tool calls, the runs it
// delegates and the reports sent back into the conversation they came from.

// ConversationRefPrefix starts the trigger_ref of a run delegated from a conversation:
// "conversation:<conversation id>" (DESIGN 5.15 rule 6).
const ConversationRefPrefix = "conversation:"

// ConversationRef is the trigger_ref of a run delegated from conversation id, "" when
// there is no conversation.
func ConversationRef(conversationID string) string {
	if conversationID == "" {
		return ""
	}
	return ConversationRefPrefix + conversationID
}

// ConversationOfRun returns the conversation a channel-triggered run reports back to:
// the id in its "conversation:<id>" trigger_ref, or false when it has none.
func ConversationOfRun(r *Run) (string, bool) {
	if r == nil || r.Trigger != TriggerChannel || r.TriggerRef == nil {
		return "", false
	}
	id, ok := strings.CutPrefix(*r.TriggerRef, ConversationRefPrefix)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// LatestConversationForAgent returns the agent's conversation with the latest message
// (DESIGN 5.15 rule 3); ErrNotFound when the agent has none.
func LatestConversationForAgent(ctx context.Context, q Querier, agentID string) (*Conversation, error) {
	rows, err := q.Query(ctx, conversationSelect+` WHERE c.agent_id = $1
		ORDER BY c.last_message_at DESC NULLS LAST, c.created_at DESC, c.id DESC LIMIT 1`, agentID)
	if err != nil {
		return nil, err
	}
	c, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Conversation])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// ListToolCallsByConversation returns the chat-time tool calls recorded against a
// conversation, oldest first, at most limit (the newest ones when there are more).
func ListToolCallsByConversation(ctx context.Context, q Querier, conversationID string, limit int) ([]ToolCall, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := q.Query(ctx, `SELECT * FROM (SELECT `+toolCallSelect+` FROM tool_calls WHERE conversation_id = $1
		ORDER BY started_at DESC NULLS LAST, id DESC LIMIT $2) t ORDER BY started_at NULLS FIRST, id`, conversationID, limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[ToolCall])
	if out == nil {
		out = []ToolCall{}
	}
	return out, err
}

// ClaimRunReport marks a terminal channel run as reported and returns it; ErrNotFound
// when it is not terminal, not delegated from a conversation, or was reported already.
// The claim comes before the send, so a run is reported at most once (DESIGN 5.15 rule
// 9: one attempt, never retried).
func ClaimRunReport(ctx context.Context, q Querier, runID string, at time.Time) (*Run, error) {
	return collectRun(q.Query(ctx, `UPDATE runs SET reported_at = $2
		WHERE id = $1 AND reported_at IS NULL AND trigger = $3 AND trigger_ref LIKE $4
		  AND status IN ($5, $6, $7, $8)
		RETURNING `+runColumns, runID, at.UTC(), TriggerChannel, ConversationRefPrefix+"%",
		RunSucceeded, RunFailed, RunCancelled, RunTimedOut))
}

// UnclaimRunReport gives back a run's report claim when the report was never sent (the
// gateway was not connected), so the next sweep sends it.
func UnclaimRunReport(ctx context.Context, q Querier, runID string) error {
	_, err := q.Exec(ctx, `UPDATE runs SET reported_at = NULL WHERE id = $1`, runID)
	return err
}

// UnclaimApprovalReport gives back an approval notice's claim when it was never sent.
func UnclaimApprovalReport(ctx context.Context, q Querier, approvalID string) error {
	_, err := q.Exec(ctx, `UPDATE approvals SET reported_at = NULL WHERE id = $1`, approvalID)
	return err
}

// ListUnreportedChannelRuns returns the ids of terminal runs delegated from a
// conversation whose report was never claimed, oldest first: the safety net for
// terminal transitions that happen outside the engine's report hooks (the stale-run
// sweep) or that a crash interrupted.
func ListUnreportedChannelRuns(ctx context.Context, q Querier, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := q.Query(ctx, `SELECT id FROM runs WHERE trigger = $1 AND reported_at IS NULL AND trigger_ref LIKE $2
		AND status IN ($3, $4, $5, $6) ORDER BY created_at LIMIT $7`,
		TriggerChannel, ConversationRefPrefix+"%", RunSucceeded, RunFailed, RunCancelled, RunTimedOut, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ClaimApprovalReports marks every pending, unreported approval of a channel run that
// was delegated from a conversation as reported and returns them, oldest first
// (DESIGN 5.15 rule 10). Nothing is claimed for any other run.
func ClaimApprovalReports(ctx context.Context, q Querier, runID string, at time.Time) ([]Approval, error) {
	rows, err := q.Query(ctx, `UPDATE approvals a SET reported_at = $2 FROM runs r
		WHERE a.run_id = $1 AND r.id = a.run_id AND a.status = $3 AND a.reported_at IS NULL
		  AND r.trigger = $4 AND r.trigger_ref LIKE $5
		RETURNING a.id, COALESCE(a.run_id, '') AS run_id, COALESCE(a.run_node_id, '') AS run_node_id, a.tool_call_id,
		  a.kind, a.node_id, a.message, a.status, a.external_id, a.requested_at, a.decided_at, a.decided_by, a.reason,
		  a.expires_at, a.conversation_id, a.reported_at`,
		runID, at.UTC(), ApprovalPending, TriggerChannel, ConversationRefPrefix+"%")
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Approval])
	if err != nil {
		return nil, err
	}
	sortApprovals(out)
	return out, nil
}

// ListRunsWithUnreportedApprovals returns the ids of channel runs delegated from a
// conversation that hold a pending approval nobody announced yet.
func ListRunsWithUnreportedApprovals(ctx context.Context, q Querier) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT a.run_id FROM approvals a JOIN runs r ON r.id = a.run_id
		WHERE a.status = $1 AND a.reported_at IS NULL AND r.trigger = $2 AND r.trigger_ref LIKE $3`,
		ApprovalPending, TriggerChannel, ConversationRefPrefix+"%")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// sortApprovals orders approvals by request time, then id.
func sortApprovals(a []Approval) {
	slices.SortFunc(a, func(x, y Approval) int {
		if c := x.RequestedAt.Compare(y.RequestedAt); c != 0 {
			return c
		}
		return strings.Compare(x.ID, y.ID)
	})
}
