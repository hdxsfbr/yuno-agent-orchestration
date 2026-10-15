package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/events"
)

// Querier is satisfied by *pgxpool.Pool, pgx.Tx and *pgxpool.Conn so repository
// functions work inside or outside a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconnCommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// AppendRunEvent writes one durable SSE event and returns its sequence number.
// runID may be empty for platform-wide events.
func AppendRunEvent(ctx context.Context, q Querier, runID, typ string, data json.RawMessage) (int64, error) {
	var runIDArg any
	if runID != "" {
		runIDArg = runID
	}
	var seq int64
	err := q.QueryRow(ctx,
		`INSERT INTO run_events (run_id, event, data) VALUES ($1, $2, $3) RETURNING seq`,
		runIDArg, typ, data).Scan(&seq)
	return seq, err
}

// RunEventsAfter returns events with seq > after for runID ("" = all), oldest first.
func RunEventsAfter(ctx context.Context, pool *pgxpool.Pool, runID string, after int64, limit int) ([]events.Event, error) {
	rows, err := pool.Query(ctx,
		`SELECT seq, COALESCE(run_id, ''), event, data, created_at FROM run_events
		 WHERE seq > $1 AND ($2 = '' OR run_id = $2) ORDER BY seq LIMIT $3`,
		after, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []events.Event
	for rows.Next() {
		var e events.Event
		var at time.Time
		if err := rows.Scan(&e.Seq, &e.RunID, &e.Type, &e.Data, &at); err != nil {
			return nil, err
		}
		e.At = at
		out = append(out, e)
	}
	return out, rows.Err()
}
