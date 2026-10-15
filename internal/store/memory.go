package store

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// MemoryScopeAgent is the only memory scope this wave (DESIGN 5.10).
const MemoryScopeAgent = "agent"

// MemoryFact is one remembered key/value for an agent.
type MemoryFact struct {
	ID          string     `db:"id" json:"id"`
	Scope       string     `db:"scope" json:"scope"`
	AgentID     string     `db:"agent_id" json:"agent_id"`
	Key         string     `db:"key" json:"key"`
	Value       string     `db:"value" json:"value"`
	SourceRunID *string    `db:"source_run_id" json:"source_run_id,omitempty"`
	CreatedAt   time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at" json:"updated_at"`
	ExpiresAt   *time.Time `db:"expires_at" json:"expires_at,omitempty"`
}

const memoryFactColumns = `id, scope, agent_id, key, value, source_run_id, created_at, updated_at, expires_at`

// SaveMemoryFact upserts a fact by (agent, scope, key), drops the agent's expired facts
// and then the oldest ones beyond maxFacts, in one transaction.
func SaveMemoryFact(ctx context.Context, db interface {
	Begin(context.Context) (pgx.Tx, error)
}, f *MemoryFact, maxFacts int) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if f.Scope == "" {
		f.Scope = MemoryScopeAgent
	}
	now := time.Now().UTC()
	rows, err := tx.Query(ctx, `INSERT INTO memory_facts (`+memoryFactColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7,$8)
		ON CONFLICT (agent_id, scope, key) DO UPDATE SET value=EXCLUDED.value, source_run_id=EXCLUDED.source_run_id,
			updated_at=EXCLUDED.updated_at, expires_at=EXCLUDED.expires_at
		RETURNING `+memoryFactColumns, NewID(), f.Scope, f.AgentID, f.Key, f.Value, f.SourceRunID, now, f.ExpiresAt)
	if err != nil {
		return err
	}
	saved, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[MemoryFact])
	if err != nil {
		return err
	}
	*f = saved
	if _, err := tx.Exec(ctx, `DELETE FROM memory_facts WHERE agent_id=$1 AND scope=$2 AND expires_at <= now()`,
		f.AgentID, f.Scope); err != nil {
		return err
	}
	if maxFacts > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM memory_facts WHERE id IN (
			SELECT id FROM memory_facts WHERE agent_id=$1 AND scope=$2 ORDER BY updated_at DESC, id DESC OFFSET $3)`,
			f.AgentID, f.Scope, maxFacts); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// RecallMemoryFacts returns the agent's live facts whose key or value contains any of
// the query's words (case-insensitive), most matching words first, then newest. An
// empty query returns the newest facts.
func RecallMemoryFacts(ctx context.Context, q Querier, agentID, scope, query string, limit int) ([]MemoryFact, error) {
	if scope == "" {
		scope = MemoryScopeAgent
	}
	if limit <= 0 {
		limit = 20
	}
	terms := strings.Fields(strings.ToLower(query))
	if terms == nil {
		terms = []string{}
	}
	rows, err := q.Query(ctx, `SELECT `+memoryFactColumns+` FROM (
			SELECT f.*, (SELECT count(*) FROM unnest($3::text[]) AS t(term)
				WHERE strpos(lower(f.key), t.term) > 0 OR strpos(lower(f.value), t.term) > 0) AS hits
			FROM memory_facts f
			WHERE f.agent_id=$1 AND f.scope=$2 AND (f.expires_at IS NULL OR f.expires_at > now())
		) m
		WHERE cardinality($3::text[]) = 0 OR hits > 0
		ORDER BY hits DESC, updated_at DESC, id DESC LIMIT $4`, agentID, scope, terms, limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[MemoryFact])
	if out == nil {
		out = []MemoryFact{}
	}
	return out, err
}
