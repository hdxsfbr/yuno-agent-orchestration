package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrEffectNotRecorded wraps a failure to store the result of an external call that
// succeeded: the effect happened, the key was not kept, and a later retry relies on the
// external service's own idempotency key (DESIGN 5.4 rule 5).
var ErrEffectNotRecorded = errors.New("side effect succeeded but its result was not recorded")

// recordTimeout bounds the writes after the external call, which run detached from the
// caller's context so a cancelled request still records an effect that happened.
const recordTimeout = 5 * time.Second

// RunSideEffect executes fn at most once per idempotency key (DESIGN 5.4 rules 2, 5
// and 6). In one transaction it inserts the key, locks it with SELECT ... FOR UPDATE and
// either returns the stored result (replayed=true) or calls fn while holding the lock,
// so a concurrent caller with the same key waits and then replays. fn's result is
// written in the same transaction after it succeeds; when fn fails the key row is
// deleted and fn's error is returned, so a retry can try again.
func RunSideEffect(ctx context.Context, pool *pgxpool.Pool, key, toolName, runID, toolCallID string,
	fn func(ctx context.Context) (json.RawMessage, error)) (result json.RawMessage, replayed bool, err error) {
	return RunSideEffectTx(ctx, pool, key, toolName, runID, toolCallID,
		func(ctx context.Context, _ pgx.Tx) (json.RawMessage, error) { return fn(ctx) })
}

// RunSideEffectTx is RunSideEffect with fn handed the transaction it runs in, so a tool
// can write its own rows (the merge request record of DESIGN 5.20 rule 2) in the same
// transaction as the result: they commit together with it and disappear together with
// the key when fn fails. fn must not commit or roll the transaction back.
func RunSideEffectTx(ctx context.Context, pool *pgxpool.Pool, key, toolName, runID, toolCallID string,
	fn func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error)) (result json.RawMessage, replayed bool, err error) {
	if key == "" {
		return nil, false, errors.New("side effect: empty idempotency key")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("side effect: begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx, `INSERT INTO side_effects (idempotency_key, tool_name, run_id, tool_call_id)
		VALUES ($1, $2, $3, $4) ON CONFLICT (idempotency_key) DO NOTHING`, key, toolName, runID, toolCallID); err != nil {
		return nil, false, fmt.Errorf("side effect: insert key: %w", err)
	}
	var stored json.RawMessage
	if err := tx.QueryRow(ctx, `SELECT COALESCE(result, 'null'::jsonb) FROM side_effects
		WHERE idempotency_key = $1 FOR UPDATE`, key).Scan(&stored); err != nil {
		return nil, false, fmt.Errorf("side effect: lock key: %w", err)
	}
	if !isJSONNull(stored) {
		if err := tx.Commit(ctx); err != nil {
			return nil, false, fmt.Errorf("side effect: commit replay: %w", err)
		}
		return stored, true, nil
	}

	res, fnErr := fn(ctx, tx)
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if fnErr != nil {
		// The transaction ends in a rollback, not a commit: the key row it inserted
		// goes away with whatever fn wrote beside it (a failed call leaves no row, DESIGN
		// 5.20 rule 2).
		_ = tx.Rollback(wctx)
		return nil, false, fnErr
	}
	if len(res) == 0 {
		res = json.RawMessage(`{}`)
	}
	if _, err := tx.Exec(wctx, `UPDATE side_effects SET result = $2, tool_call_id = $3 WHERE idempotency_key = $1`,
		key, res, toolCallID); err != nil {
		return res, false, fmt.Errorf("%w: %v", ErrEffectNotRecorded, err)
	}
	if err := tx.Commit(wctx); err != nil {
		return res, false, fmt.Errorf("%w: %v", ErrEffectNotRecorded, err)
	}
	return res, false, nil
}

const sideEffectColumns = `idempotency_key, tool_name, run_id, tool_call_id, COALESCE(result, 'null'::jsonb) AS result, created_at`

// GetSideEffect returns the record for a key, or ErrNotFound.
func GetSideEffect(ctx context.Context, q Querier, key string) (*SideEffect, error) {
	rows, err := q.Query(ctx, `SELECT `+sideEffectColumns+` FROM side_effects WHERE idempotency_key = $1`, key)
	if err != nil {
		return nil, err
	}
	se, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[SideEffect])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return se, err
}

// ListSideEffects returns a run's recorded side effects, oldest first (the trace's
// side_effects[], DESIGN 5.4 outputs).
func ListSideEffects(ctx context.Context, q Querier, runID string) ([]SideEffect, error) {
	rows, err := q.Query(ctx, `SELECT `+sideEffectColumns+` FROM side_effects WHERE run_id = $1 AND result IS NOT NULL
		ORDER BY created_at, idempotency_key`, runID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[SideEffect])
	if out == nil {
		out = []SideEffect{}
	}
	return out, err
}

func isJSONNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}
