package memory

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"yuno/internal/store"
)

// Project notes (DESIGN 5.18): facts keyed by (project, key) that every agent of a run
// on the project reads and writes. They follow agent memory's rules (5.10) with their
// own table, a 90-day default TTL and a cap of 100 per project.
const (
	DefaultProjectFactTTLDays = 90
	MaxProjectFacts           = 100
	MaxProjectKeyBytes        = 200
)

// Model-facing refusals of a note the table would not hold (DESIGN 5.18 rule 2). A value
// over MaxValueBytes is ErrValueTooLong.
var (
	ErrProjectKeyLength = errors.New("key must be 1 to 200 bytes")
	ErrEmptyValue       = errors.New("value must not be empty")
)

// ProjectFactExpiresAt is when a note saved at now expires: after ttlDays when the call
// names it (0 expires at once and is never returned), else after the 90-day default.
// The agent's own fact_ttl_days does not apply to a note it shares.
func ProjectFactExpiresAt(ttlDays *int, now time.Time) time.Time {
	days := DefaultProjectFactTTLDays
	if ttlDays != nil {
		days = max(*ttlDays, 0)
	}
	return now.Add(time.Duration(days) * 24 * time.Hour)
}

// projectFactColumns reads a note without the agent's name; the join queries select it.
const projectFactColumns = `id, project_id, key, value, agent_id, NULL::text AS agent_name, source_run_id,
	created_at, updated_at, expires_at`

// SaveProjectFact upserts f by (project, key) at now (rule 3: the later write wins, with
// the caller as the note's agent and run) and enforces the cap in the same transaction:
// the project's expired notes go first, then, when more than MaxProjectFacts live notes
// remain, the oldest by created_at. Replacing a key keeps its created_at, so a note ages
// from when it was first saved. f.ExpiresAt must be set (ProjectFactExpiresAt).
func SaveProjectFact(ctx context.Context, db Beginner, f *store.ProjectFact, now time.Time) error {
	if n := len(f.Key); n < 1 || n > MaxProjectKeyBytes {
		return ErrProjectKeyLength
	}
	if f.Value == "" {
		return ErrEmptyValue
	}
	if len(f.Value) > MaxValueBytes {
		return ErrValueTooLong
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `DELETE FROM project_facts WHERE project_id=$1 AND expires_at <= $2`, f.ProjectID, now); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `INSERT INTO project_facts (id, project_id, key, value, agent_id, source_run_id, created_at, updated_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7,$8)
		ON CONFLICT (project_id, key) DO UPDATE SET value=EXCLUDED.value, agent_id=EXCLUDED.agent_id,
			source_run_id=EXCLUDED.source_run_id, updated_at=EXCLUDED.updated_at, expires_at=EXCLUDED.expires_at
		RETURNING `+projectFactColumns, store.NewID(), f.ProjectID, f.Key, f.Value, f.AgentID, f.SourceRunID, now, f.ExpiresAt)
	if err != nil {
		return err
	}
	saved, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[store.ProjectFact])
	if err != nil {
		return err
	}
	*f = saved
	if _, err := tx.Exec(ctx, `DELETE FROM project_facts WHERE id IN (
		SELECT id FROM project_facts WHERE project_id=$1 AND `+bind(live, "$3")+`
		ORDER BY created_at DESC, id DESC OFFSET $2)`, f.ProjectID, MaxProjectFacts, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RecallProjectFacts returns up to limit live notes of the project whose key or value
// contains every word of the query, case-insensitively, newest by updated_at first (rule
// 3, with 5.10 rule 4's match); an empty query returns the newest.
func RecallProjectFacts(ctx context.Context, q store.Querier, projectID, query string, limit int, now time.Time) ([]store.ProjectFact, error) {
	if limit <= 0 {
		limit = RecallLimit
	}
	terms := strings.Fields(strings.ToLower(query))
	if terms == nil {
		terms = []string{}
	}
	// Key and value are joined by a newline, which no term contains, so a term never
	// matches across the two.
	return collectProject(q.Query(ctx, `SELECT `+projectFactColumns+` FROM project_facts
		WHERE project_id=$1 AND `+bind(live, "$2")+`
		AND NOT EXISTS (SELECT 1 FROM unnest($3::text[]) AS t(term)
			WHERE strpos(lower(key) || E'\n' || lower(value), t.term) = 0)
		ORDER BY updated_at DESC, id DESC LIMIT $4`, projectID, now, terms, limit))
}

// NewestProjectFacts returns the project's n newest live notes (rule 4: what a task
// carries).
func NewestProjectFacts(ctx context.Context, q store.Querier, projectID string, n int, now time.Time) ([]store.ProjectFact, error) {
	return RecallProjectFacts(ctx, q, projectID, "", n, now)
}

// ListProjectFacts returns the project's live notes, newest first, each with the saving
// agent's name (nil once the agent row is gone) for the API (rule 6).
func ListProjectFacts(ctx context.Context, q store.Querier, projectID string, now time.Time) ([]store.ProjectFact, error) {
	return collectProject(q.Query(ctx, `SELECT pf.id, pf.project_id, pf.key, pf.value, pf.agent_id, a.name AS agent_name,
		pf.source_run_id, pf.created_at, pf.updated_at, pf.expires_at
		FROM project_facts pf LEFT JOIN agents a ON a.id = pf.agent_id
		WHERE pf.project_id=$1 AND (pf.expires_at IS NULL OR pf.expires_at > $2)
		ORDER BY pf.updated_at DESC, pf.id DESC LIMIT $3`, projectID, now, listLimit))
}

// DeleteProjectFact removes one of the project's notes; store.ErrNotFound when it has no
// such note.
func DeleteProjectFact(ctx context.Context, q store.Querier, projectID, factID string) error {
	tag, err := q.Exec(ctx, `DELETE FROM project_facts WHERE id=$1 AND project_id=$2`, factID, projectID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// SweepExpiredProjectFacts deletes every note expired at now and returns how many (rule
// 5: the same pass as agent memory's sweep).
func SweepExpiredProjectFacts(ctx context.Context, q store.Querier, now time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM project_facts WHERE expires_at <= $1`, now)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func collectProject(rows pgx.Rows, err error) ([]store.ProjectFact, error) {
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[store.ProjectFact])
	if out == nil {
		out = []store.ProjectFact{}
	}
	return out, err
}
