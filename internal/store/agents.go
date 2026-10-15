package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned by every Get when the row does not exist (or is soft-deleted).
var ErrNotFound = errors.New("not found")

const agentColumns = `id, name, role, system_prompt, model, effort, max_tokens, tool_allowlist,
	interaction_rules, limits, guardrails, memory_policy, channels, openclaw_agent_id, skills,
	thinking, exec_policy, materialized_hash, created_at, updated_at, deleted_at`

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

func CreateAgent(ctx context.Context, q Querier, a *Agent) error {
	if a.ID == "" {
		a.ID = NewID()
	}
	normalizeAgent(a)
	now := time.Now().UTC()
	a.CreatedAt, a.UpdatedAt = now, now
	_, err := q.Exec(ctx, `INSERT INTO agents (`+agentColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,NULL)`,
		a.ID, a.Name, a.Role, a.SystemPrompt, a.Model, a.Effort, a.MaxTokens, a.ToolAllowlist,
		a.InteractionRules, a.Limits, a.Guardrails, a.MemoryPolicy, a.Channels, a.OpenClawAgentID, a.Skills,
		a.Thinking, a.ExecPolicy, a.MaterializedHash, a.CreatedAt, a.UpdatedAt)
	return err
}

func GetAgent(ctx context.Context, q Querier, id string) (*Agent, error) {
	rows, err := q.Query(ctx, `SELECT `+agentColumns+` FROM agents WHERE id = $1 AND deleted_at IS NULL`, id)
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
	rows, err := q.Query(ctx, `SELECT `+agentColumns+` FROM agents WHERE openclaw_agent_id = $1 AND deleted_at IS NULL`, openclawID)
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
	rows, err := q.Query(ctx, `SELECT `+agentColumns+` FROM agents WHERE deleted_at IS NULL ORDER BY created_at`)
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
