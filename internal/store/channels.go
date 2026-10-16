package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// Channels the platform binds agents to (DESIGN 5.8).
const ChannelTelegram = "telegram"

// Conversation message directions.
const (
	DirectionIn  = "in"
	DirectionOut = "out"
)

// ErrChannelBound is returned when a channel is already bound to another agent (409).
var ErrChannelBound = errors.New("channel is bound to another agent")

// ChannelBinding is the agent that answers a channel (DESIGN 5.8 rule 3), with the
// agent's name and OpenClaw id joined.
type ChannelBinding struct {
	ID              string          `db:"id" json:"id"`
	Channel         string          `db:"channel" json:"channel"`
	AgentID         string          `db:"agent_id" json:"agent_id"`
	AgentName       string          `db:"agent_name" json:"agent_name"`
	OpenClawAgentID *string         `db:"openclaw_agent_id" json:"openclaw_agent_id,omitempty"`
	ExternalID      string          `db:"external_id" json:"external_id,omitempty"`
	Config          json.RawMessage `db:"config" json:"config,omitempty"`
	CreatedAt       time.Time       `db:"created_at" json:"created_at"`
}

const bindingSelect = `SELECT b.id, b.channel, b.agent_id, a.name AS agent_name, a.openclaw_agent_id, b.external_id,
	b.config, b.created_at FROM channel_bindings b JOIN agents a ON a.id = b.agent_id`

// GetChannelBinding returns the channel's binding. A binding whose agent was deleted
// counts as none: ErrNotFound.
func GetChannelBinding(ctx context.Context, q Querier, channel string) (*ChannelBinding, error) {
	rows, err := q.Query(ctx, bindingSelect+` WHERE b.channel = $1 AND a.deleted_at IS NULL`, channel)
	if err != nil {
		return nil, err
	}
	b, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[ChannelBinding])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// BindChannel binds agentID to the channel. Binding the agent already bound is a no-op;
// another live agent's binding is ErrChannelBound; a deleted agent's binding is
// replaced. The channel is claimed by the insert, so of two concurrent binds the unique
// index lets one win and the other gets ErrChannelBound; nothing deletes a live binding.
func BindChannel(ctx context.Context, q Querier, channel, agentID string) (*ChannelBinding, error) {
	if _, err := q.Exec(ctx, `DELETE FROM channel_bindings b USING agents a
		WHERE b.channel = $1 AND a.id = b.agent_id AND a.deleted_at IS NOT NULL`, channel); err != nil {
		return nil, err
	}
	// DO NOTHING rather than a unique violation keeps a caller's transaction usable.
	tag, err := q.Exec(ctx, `INSERT INTO channel_bindings (id, channel, agent_id) VALUES ($1, $2, $3)
		ON CONFLICT (channel) DO NOTHING`, NewID(), channel, agentID)
	if err != nil {
		return nil, err
	}
	b, err := GetChannelBinding(ctx, q, channel)
	switch {
	case err == nil && (tag.RowsAffected() == 1 || b.AgentID == agentID):
		return b, nil
	case err == nil, errors.Is(err, ErrNotFound):
		return nil, ErrChannelBound
	}
	return nil, err
}

// UnbindChannel removes the channel's binding and reports whether there was one.
func UnbindChannel(ctx context.Context, q Querier, channel string) (bool, error) {
	tag, err := q.Exec(ctx, `DELETE FROM channel_bindings WHERE channel = $1`, channel)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SetChannelBindingExternalID records the bot username the gateway reported.
func SetChannelBindingExternalID(ctx context.Context, q Querier, channel, externalID string) error {
	_, err := q.Exec(ctx, `UPDATE channel_bindings SET external_id = $2 WHERE channel = $1 AND external_id <> $2`, channel, externalID)
	return err
}

// ---- conversations (DESIGN 5.8 rule 4) ------------------------------------------------

// Conversation is one OpenClaw channel session as the platform mirrors it. AgentName
// and MessageCount are joined for the list.
type Conversation struct {
	ID             string     `db:"id" json:"id"`
	Channel        string     `db:"channel" json:"channel"`
	ExternalChatID string     `db:"external_chat_id" json:"external_chat_id"`
	AgentID        string     `db:"agent_id" json:"agent_id"`
	AgentName      string     `db:"agent_name" json:"agent_name"`
	SessionKey     string     `db:"session_key" json:"session_key"`
	DisplayName    string     `db:"display_name" json:"display_name,omitempty"`
	CreatedAt      time.Time  `db:"created_at" json:"created_at"`
	LastMessageAt  *time.Time `db:"last_message_at" json:"last_message_at,omitempty"`
	MessageCount   int        `db:"message_count" json:"message_count"`
}

const conversationSelect = `SELECT c.id, c.channel, c.external_chat_id, COALESCE(c.agent_id, '') AS agent_id,
	COALESCE(a.name, '') AS agent_name, c.session_key, c.display_name, c.created_at, c.last_message_at,
	(SELECT count(*) FROM conversation_messages m WHERE m.conversation_id = c.id)::int AS message_count
	FROM conversations c LEFT JOIN agents a ON a.id = c.agent_id`

// UpsertConversation returns the conversation of c.SessionKey, creating it from c when
// it does not exist. A non-empty display name or agent replaces the stored one.
func UpsertConversation(ctx context.Context, q Querier, c *Conversation) (*Conversation, error) {
	var agentID any
	if c.AgentID != "" {
		agentID = c.AgentID
	}
	_, err := q.Exec(ctx, `INSERT INTO conversations (id, channel, external_chat_id, agent_id, session_key, display_name)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (session_key) DO UPDATE SET
			display_name = CASE WHEN EXCLUDED.display_name <> '' THEN EXCLUDED.display_name ELSE conversations.display_name END,
			agent_id = COALESCE(EXCLUDED.agent_id, conversations.agent_id)`,
		NewID(), c.Channel, c.ExternalChatID, agentID, c.SessionKey, c.DisplayName)
	if err != nil {
		return nil, err
	}
	return getConversation(ctx, q, `c.session_key = $1`, c.SessionKey)
}

// GetConversation returns one conversation by id.
func GetConversation(ctx context.Context, q Querier, id string) (*Conversation, error) {
	return getConversation(ctx, q, `c.id = $1`, id)
}

func getConversation(ctx context.Context, q Querier, where string, arg any) (*Conversation, error) {
	rows, err := q.Query(ctx, conversationSelect+` WHERE `+where, arg)
	if err != nil {
		return nil, err
	}
	c, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Conversation])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// ListConversations lists conversations, most recently active first.
func ListConversations(ctx context.Context, q Querier, limit int) ([]Conversation, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.Query(ctx, conversationSelect+` ORDER BY COALESCE(c.last_message_at, c.created_at) DESC, c.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Conversation])
	if out == nil {
		out = []Conversation{}
	}
	return out, err
}

// ConversationMessage is one mirrored transcript entry: what the person sent (in) or
// what the agent answered (out).
type ConversationMessage struct {
	ID                string          `db:"id" json:"id"`
	ConversationID    string          `db:"conversation_id" json:"conversation_id"`
	Direction         string          `db:"direction" json:"direction"`
	ExternalMessageID string          `db:"external_message_id" json:"external_message_id"`
	Text              string          `db:"text" json:"text"`
	Usage             json.RawMessage `db:"usage" json:"usage,omitempty"`
	RunID             *string         `db:"run_id" json:"run_id"`
	CreatedAt         time.Time       `db:"created_at" json:"created_at"`
}

const conversationMessageColumns = `id, conversation_id, direction, external_message_id, text, usage, run_id, created_at`

// InsertConversationMessage stores m unless the conversation already has a message with
// its external id, and moves the conversation's last_message_at forward. It reports
// whether m was new.
func InsertConversationMessage(ctx context.Context, q Querier, m *ConversationMessage) (bool, error) {
	if m.ID == "" {
		m.ID = NewID()
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	usage := m.Usage
	if len(usage) == 0 {
		usage = json.RawMessage(`null`)
	}
	tag, err := q.Exec(ctx, `INSERT INTO conversation_messages (`+conversationMessageColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (conversation_id, external_message_id) DO NOTHING`,
		m.ID, m.ConversationID, m.Direction, m.ExternalMessageID, m.Text, usage, m.RunID, m.CreatedAt)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	_, err = q.Exec(ctx, `UPDATE conversations SET last_message_at = GREATEST(COALESCE(last_message_at, $2), $2) WHERE id = $1`,
		m.ConversationID, m.CreatedAt)
	return true, err
}

// ListConversationMessages returns the newest limit messages of a conversation, oldest
// first, leaving out those its agent's conversation TTL has expired at now (DESIGN 5.10
// rule 6: the sweep timing is invisible).
func ListConversationMessages(ctx context.Context, q Querier, conversationID string, limit int, now time.Time) ([]ConversationMessage, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := q.Query(ctx, `SELECT `+conversationMessageColumns+` FROM (
		SELECT m.id, m.conversation_id, m.direction, m.external_message_id, m.text, m.usage, m.run_id, m.created_at
		FROM conversation_messages m JOIN conversations c ON c.id = m.conversation_id LEFT JOIN agents a ON a.id = c.agent_id
		WHERE m.conversation_id = $1 AND m.created_at >= $3::timestamptz - make_interval(days => `+conversationTTLDays+`)
		ORDER BY m.created_at DESC, m.id DESC LIMIT $2) m ORDER BY created_at, id`, conversationID, limit, now.UTC())
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[ConversationMessage])
	if out == nil {
		out = []ConversationMessage{}
	}
	return out, err
}

// ---- approvals raised in a conversation (DESIGN 5.8 rule 5) ---------------------------

// conversationApprovalColumns read an approval whose run columns may be NULL. Scans are
// lax so the struct may carry columns this list leaves out.
const conversationApprovalColumns = `id, COALESCE(run_id, '') AS run_id, COALESCE(run_node_id, '') AS run_node_id,
	tool_call_id, kind, node_id, message, status, external_id, requested_at, decided_at, decided_by, reason, expires_at`

// InsertConversationApproval stores an approval raised by a conversation turn: no run,
// no run node, the conversation instead.
func InsertConversationApproval(ctx context.Context, q Querier, a *Approval, conversationID string) error {
	if a.ID == "" {
		a.ID = NewID()
	}
	if a.Status == "" {
		a.Status = ApprovalPending
	}
	if a.RequestedAt.IsZero() {
		a.RequestedAt = time.Now().UTC()
	}
	a.RunID, a.RunNodeID = "", ""
	_, err := q.Exec(ctx, `INSERT INTO approvals (id, run_id, run_node_id, tool_call_id, kind, node_id, message, status,
		external_id, requested_at, decided_at, decided_by, reason, expires_at, conversation_id)
		VALUES ($1, NULL, NULL, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		a.ID, a.ToolCallID, a.Kind, a.NodeID, a.Message, a.Status, a.ExternalID, a.RequestedAt,
		a.DecidedAt, a.DecidedBy, a.Reason, a.ExpiresAt, conversationID)
	return err
}

// GetConversationApprovalByExternalID returns the newest conversation approval with the
// gateway's id.
func GetConversationApprovalByExternalID(ctx context.Context, q Querier, externalID string) (*Approval, error) {
	rows, err := q.Query(ctx, `SELECT `+conversationApprovalColumns+` FROM approvals
		WHERE external_id = $1 AND conversation_id IS NOT NULL ORDER BY requested_at DESC LIMIT 1`, externalID)
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByNameLax[Approval])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// DecideConversationApproval decides a pending conversation approval; ErrNotFound when
// it is not pending any more.
func DecideConversationApproval(ctx context.Context, q Querier, id, status, by, reason string, at time.Time) (*Approval, error) {
	rows, err := q.Query(ctx, `UPDATE approvals SET status = $2, decided_at = $5, decided_by = $3, reason = $4
		WHERE id = $1 AND status = 'pending' AND conversation_id IS NOT NULL RETURNING `+conversationApprovalColumns,
		id, status, by, reason, at.UTC())
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByNameLax[Approval])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// ApprovalConversation returns the conversation an approval was raised in, or
// ErrNotFound when it belongs to a run.
func ApprovalConversation(ctx context.Context, q Querier, approvalID string) (*Conversation, error) {
	var convID *string
	err := q.QueryRow(ctx, `SELECT conversation_id FROM approvals WHERE id = $1`, approvalID).Scan(&convID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && convID == nil) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return GetConversation(ctx, q, *convID)
}

// DefaultConversationTTLDays is memory_policy.conversation_ttl_days when unset (DESIGN
// 5.10 inputs).
const DefaultConversationTTLDays = 7

// conversationTTLDays is the conversation TTL in days of the agent joined as a: the sweep,
// the reads and the replay all use this one expression.
var conversationTTLDays = `COALESCE(NULLIF((a.memory_policy->>'conversation_ttl_days')::int, 0), ` +
	strconv.Itoa(DefaultConversationTTLDays) + `)`

// ConversationTTLDays is the conversation TTL of the conversation's agent: its
// conversation_ttl_days, or the default when unset or the agent is gone.
func ConversationTTLDays(ctx context.Context, q Querier, conversationID string) (int, error) {
	var days int
	err := q.QueryRow(ctx, `SELECT `+conversationTTLDays+` FROM conversations c LEFT JOIN agents a ON a.id = c.agent_id
		WHERE c.id = $1`, conversationID).Scan(&days)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return days, err
}

// DeleteExpiredConversationMessages deletes conversation messages older than their
// agent's conversation_ttl_days at now (DESIGN 5.10 rule 6) and returns how many.
func DeleteExpiredConversationMessages(ctx context.Context, q Querier, now time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM conversation_messages m
		USING conversations c LEFT JOIN agents a ON a.id = c.agent_id
		WHERE m.conversation_id = c.id
		  AND m.created_at < $1::timestamptz - make_interval(days => `+conversationTTLDays+`)`,
		now.UTC())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// UnbindAgent removes any binding of agentID and reports whether there was one.
func UnbindAgent(ctx context.Context, q Querier, agentID string) (bool, error) {
	tag, err := q.Exec(ctx, `DELETE FROM channel_bindings WHERE agent_id = $1`, agentID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
