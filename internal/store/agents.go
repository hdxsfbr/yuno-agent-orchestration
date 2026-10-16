package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"yuno/internal/runtoken"
)

// ErrNotFound is returned by every Get when the row does not exist (or is soft-deleted).
var ErrNotFound = errors.New("not found")

const agentColumns = `id, name, role, system_prompt, model, effort, max_tokens, tool_allowlist,
	interaction_rules, limits, guardrails, memory_policy, channels, openclaw_agent_id, skills,
	thinking, exec_policy, materialized_hash, created_at, updated_at, deleted_at`

// agentSelect reads an agent for scanning: agentColumns plus the chat token, "" until
// the backfill has filled a row that predates it.
const agentSelect = agentColumns + `, COALESCE(chat_token, '') AS chat_token`

func normalizeAgent(a *Agent) {
	if a.ToolAllowlist == nil {
		a.ToolAllowlist = []string{}
	}
	if a.InteractionRules == nil {
		a.InteractionRules = map[string]string{}
	}
	if a.Channels == nil {
		a.Channels = []string{}
	}
	if a.Skills == nil {
		a.Skills = []string{}
	}
	if a.Thinking == "" {
		a.Thinking = "medium"
	}
	if a.Effort == "" {
		a.Effort = "medium"
	}
}

// CreateAgent stores a new agent with a fresh chat token (DESIGN 5.15 rule 1) unless
// the caller set one.
func CreateAgent(ctx context.Context, q Querier, a *Agent) error {
	if a.ID == "" {
		a.ID = NewID()
	}
	if a.ChatToken == "" {
		a.ChatToken = runtoken.NewChat()
	}
	normalizeAgent(a)
	now := time.Now().UTC()
	a.CreatedAt, a.UpdatedAt = now, now
	_, err := q.Exec(ctx, `INSERT INTO agents (`+agentColumns+`, chat_token)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,NULL,$21)`,
		a.ID, a.Name, a.Role, a.SystemPrompt, a.Model, a.Effort, a.MaxTokens, a.ToolAllowlist,
		a.InteractionRules, a.Limits, a.Guardrails, a.MemoryPolicy, a.Channels, a.OpenClawAgentID, a.Skills,
		a.Thinking, a.ExecPolicy, a.MaterializedHash, a.CreatedAt, a.UpdatedAt, a.ChatToken)
	return err
}

func GetAgent(ctx context.Context, q Querier, id string) (*Agent, error) {
	rows, err := q.Query(ctx, `SELECT `+agentSelect+` FROM agents WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Agent])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// GetAgentByOpenClawID finds the agent behind an OpenClaw agent id (events carry those).
func GetAgentByOpenClawID(ctx context.Context, q Querier, openclawID string) (*Agent, error) {
	rows, err := q.Query(ctx, `SELECT `+agentSelect+` FROM agents WHERE openclaw_agent_id = $1 AND deleted_at IS NULL`, openclawID)
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Agent])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

func ListAgents(ctx context.Context, q Querier) ([]Agent, error) {
	rows, err := q.Query(ctx, `SELECT `+agentSelect+` FROM agents WHERE deleted_at IS NULL ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Agent])
	if out == nil {
		out = []Agent{}
	}
	return out, err
}

func UpdateAgent(ctx context.Context, q Querier, a *Agent) error {
	normalizeAgent(a)
	a.UpdatedAt = time.Now().UTC()
	tag, err := q.Exec(ctx, `UPDATE agents SET name=$2, role=$3, system_prompt=$4, model=$5, effort=$6,
		max_tokens=$7, tool_allowlist=$8, interaction_rules=$9, limits=$10, guardrails=$11,
		memory_policy=$12, channels=$13, openclaw_agent_id=$14, skills=$15, thinking=$16,
		exec_policy=$17, materialized_hash=$18, updated_at=$19 WHERE id=$1 AND deleted_at IS NULL`,
		a.ID, a.Name, a.Role, a.SystemPrompt, a.Model, a.Effort, a.MaxTokens, a.ToolAllowlist,
		a.InteractionRules, a.Limits, a.Guardrails, a.MemoryPolicy, a.Channels, a.OpenClawAgentID, a.Skills,
		a.Thinking, a.ExecPolicy, a.MaterializedHash, a.UpdatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAgentMaterialized records the hash of the config last pushed to OpenClaw.
func SetAgentMaterialized(ctx context.Context, q Querier, id, openclawID, hash string) error {
	_, err := q.Exec(ctx, `UPDATE agents SET openclaw_agent_id=$2, materialized_hash=$3, updated_at=now() WHERE id=$1`, id, openclawID, hash)
	return err
}

// DeleteAgent soft-deletes so existing runs keep their agent snapshot.
func DeleteAgent(ctx context.Context, q Querier, id string) error {
	tag, err := q.Exec(ctx, `UPDATE agents SET deleted_at = now() WHERE id=$1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListAgentsWithDeleted is ListAgents plus the soft-deleted rows, for reconciling the
// runtime with the platform: a deleted agent still owns its OpenClaw id.
func ListAgentsWithDeleted(ctx context.Context, q Querier) ([]Agent, error) {
	rows, err := q.Query(ctx, `SELECT `+agentSelect+` FROM agents ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Agent])
	if out == nil {
		out = []Agent{}
	}
	return out, err
}

// AgentActivity counts an agent's node visits in flight: running, or waiting for an
// approval, in a run that has not finished (a visit a lost worker left behind in a
// finished run does not count).
type AgentActivity struct {
	Running         int `json:"running"`
	WaitingApproval int `json:"waiting_approval"`
}

// Busy reports whether any visit is in flight.
func (a AgentActivity) Busy() bool { return a.Running+a.WaitingApproval > 0 }

// ListAgentActivity returns the activity of every agent with a visit in flight, by
// agent id; agents without one are absent.
func ListAgentActivity(ctx context.Context, q Querier) (map[string]AgentActivity, error) {
	rows, err := q.Query(ctx, `SELECT rn.agent_id,
		count(*) FILTER (WHERE rn.status = $1), count(*) FILTER (WHERE rn.status = $2)
		FROM run_nodes rn JOIN runs r ON r.id = rn.run_id
		WHERE rn.agent_id IS NOT NULL AND rn.status IN ($1, $2) AND r.status IN ($3, $4, $5)
		GROUP BY rn.agent_id`,
		NodeRunning, NodeWaitingApproval, RunQueued, RunRunning, RunWaitingApproval)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]AgentActivity{}
	for rows.Next() {
		var id string
		var a AgentActivity
		if err := rows.Scan(&id, &a.Running, &a.WaitingApproval); err != nil {
			return nil, err
		}
		out[id] = a
	}
	return out, rows.Err()
}

// ListAgentChannels returns the ids of the agents bound to a channel, mapped to the
// channels they answer (DESIGN 5.8 rule 3: at most one agent per channel).
func ListAgentChannels(ctx context.Context, q Querier) (map[string][]string, error) {
	rows, err := q.Query(ctx, `SELECT agent_id, channel FROM channel_bindings ORDER BY channel`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var id, ch string
		if err := rows.Scan(&id, &ch); err != nil {
			return nil, err
		}
		out[id] = append(out[id], ch)
	}
	return out, rows.Err()
}

// GetAgentByChatToken returns the live agent that owns a chat token (DESIGN 5.15 rule
// 1); ErrNotFound for an unknown token, a deleted agent's or "".
func GetAgentByChatToken(ctx context.Context, q Querier, token string) (*Agent, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrNotFound
	}
	rows, err := q.Query(ctx, `SELECT `+agentSelect+` FROM agents WHERE chat_token = $1 AND deleted_at IS NULL`, token)
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Agent])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// SetAgentChatToken replaces an agent's chat token (a rotation); the next
// materialisation writes the new one into AGENTS.md, since it is part of the desired
// state.
func SetAgentChatToken(ctx context.Context, q Querier, id, token string) error {
	tag, err := q.Exec(ctx, `UPDATE agents SET chat_token = $2, updated_at = now() WHERE id = $1`, id, token)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// BackfillChatTokens gives every agent without a chat token a fresh one and returns how
// many it filled: the rows created before migration 0008. It runs after every Migrate,
// so a process that starts on an older database fills them once.
func BackfillChatTokens(ctx context.Context, q Querier) (int, error) {
	rows, err := q.Query(ctx, `SELECT id FROM agents WHERE chat_token IS NULL`)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		tag, err := q.Exec(ctx, `UPDATE agents SET chat_token = $2 WHERE id = $1 AND chat_token IS NULL`, id, runtoken.NewChat())
		if err != nil {
			return n, err
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}
