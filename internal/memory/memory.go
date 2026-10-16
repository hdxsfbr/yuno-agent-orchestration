// Package memory is agent memory and its retention policy (DESIGN 5.10): saving and
// recalling facts, the expiry sweep, and the MEMORY.md the materialiser writes into
// the agent's workspace. Every function takes the time it should treat as now, so
// expiry and eviction are testable with a pinned clock.
package memory

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"yuno/internal/store"
)

// Policy defaults and bounds (DESIGN 5.10 inputs and rules 3 to 5).
const (
	DefaultMaxFacts    = 200
	DefaultFactTTLDays = 30
	RecallLimit        = 20
	PromptFacts        = 20
	MaxValueBytes      = 2048
	listLimit          = 1000
)

// ErrValueTooLong is the model-facing refusal of a value over MaxValueBytes.
var ErrValueTooLong = errors.New("value must be at most 2048 bytes")

// MaxFacts is the policy's cap, else the default.
func MaxFacts(p store.MemoryPolicy) int {
	if p.MaxFacts > 0 {
		return p.MaxFacts
	}
	return DefaultMaxFacts
}

// ExpiresAt is when a fact saved at now expires (rule 3): after ttlDays when the call
// names it (0 expires at once and is never returned), else after the policy's
// fact_ttl_days, else after the 30-day default.
func ExpiresAt(p store.MemoryPolicy, ttlDays *int, now time.Time) time.Time {
	days := DefaultFactTTLDays
	switch {
	case ttlDays != nil:
		days = max(*ttlDays, 0)
	case p.FactTTLDays > 0:
		days = p.FactTTLDays
	}
	return now.Add(time.Duration(days) * 24 * time.Hour)
}

// Beginner opens a transaction (*pgxpool.Pool, pgx.Tx).
type Beginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

const factColumns = `id, scope, agent_id, key, value, source_run_id, created_at, updated_at, expires_at`

// live is the read filter: expired facts are invisible before the sweep removes them
// (rule 6).
const live = `(expires_at IS NULL OR expires_at > $NOW)`

// Save upserts f by (agent, scope, key) at now (rule 3) and enforces the cap in the same
// transaction: the agent's expired facts go first, then, when more than maxFacts live
// facts remain, the oldest by created_at. Replacing a key keeps its created_at, so a
// fact ages from when it was first saved.
func Save(ctx context.Context, db Beginner, f *store.MemoryFact, maxFacts int, now time.Time) error {
	if len(f.Value) > MaxValueBytes {
		return ErrValueTooLong
	}
	if f.Scope == "" {
		f.Scope = store.MemoryScopeAgent
	}
	if maxFacts <= 0 {
		maxFacts = DefaultMaxFacts
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `DELETE FROM memory_facts WHERE agent_id=$1 AND scope=$2 AND expires_at <= $3`,
		f.AgentID, f.Scope, now); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `INSERT INTO memory_facts (`+factColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7,$8)
		ON CONFLICT (agent_id, scope, key) DO UPDATE SET value=EXCLUDED.value, source_run_id=EXCLUDED.source_run_id,
			updated_at=EXCLUDED.updated_at, expires_at=EXCLUDED.expires_at
		RETURNING `+factColumns, store.NewID(), f.Scope, f.AgentID, f.Key, f.Value, f.SourceRunID, now, f.ExpiresAt)
	if err != nil {
		return err
	}
	saved, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[store.MemoryFact])
	if err != nil {
		return err
	}
	*f = saved
	if _, err := tx.Exec(ctx, `DELETE FROM memory_facts WHERE id IN (
		SELECT id FROM memory_facts WHERE agent_id=$1 AND scope=$2 AND `+bind(live, "$4")+`
		ORDER BY created_at DESC, id DESC OFFSET $3)`, f.AgentID, f.Scope, maxFacts, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Recall returns up to limit live facts whose key or value contains every word of the
// query, case-insensitively, newest first (rule 4); an empty query returns the newest.
func Recall(ctx context.Context, q store.Querier, agentID, query string, limit int, now time.Time) ([]store.MemoryFact, error) {
	if limit <= 0 {
		limit = RecallLimit
	}
	terms := strings.Fields(strings.ToLower(query))
	if terms == nil {
		terms = []string{}
	}
	// Key and value are joined by a newline, which no term contains, so a term never
	// matches across the two.
	return collect(q.Query(ctx, `SELECT `+factColumns+` FROM memory_facts
		WHERE agent_id=$1 AND scope=$2 AND `+bind(live, "$3")+`
		AND NOT EXISTS (SELECT 1 FROM unnest($4::text[]) AS t(term)
			WHERE strpos(lower(key) || E'\n' || lower(value), t.term) = 0)
		ORDER BY updated_at DESC, id DESC LIMIT $5`, agentID, store.MemoryScopeAgent, now, terms, limit))
}

// Newest returns the agent's n newest live facts (rule 5: what MEMORY.md shows).
func Newest(ctx context.Context, q store.Querier, agentID string, n int, now time.Time) ([]store.MemoryFact, error) {
	return Recall(ctx, q, agentID, "", n, now)
}

// List returns the agent's live facts, newest first, for the UI (rule 7).
func List(ctx context.Context, q store.Querier, agentID string, now time.Time) ([]store.MemoryFact, error) {
	return Recall(ctx, q, agentID, "", listLimit, now)
}

// Delete removes one of the agent's facts; store.ErrNotFound when it has no such fact.
func Delete(ctx context.Context, q store.Querier, agentID, factID string) error {
	tag, err := q.Exec(ctx, `DELETE FROM memory_facts WHERE id=$1 AND agent_id=$2`, factID, agentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// SweepExpired deletes every fact expired at now and returns how many (rule 6).
func SweepExpired(ctx context.Context, q store.Querier, now time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM memory_facts WHERE expires_at <= $1`, now)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func collect(rows pgx.Rows, err error) ([]store.MemoryFact, error) {
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[store.MemoryFact])
	if out == nil {
		out = []store.MemoryFact{}
	}
	return out, err
}

func bind(clause, param string) string {
	return strings.ReplaceAll(clause, "$NOW", param)
}
