package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---- platform approvals (Chief platform control, wave 1 A6) ---------------------------

// PlatformBinding is the one call a platform approval allows: the agent that asked, the
// contract operation, the request path (with its query), the SHA-256 of the exact body and
// the caller's idempotency key.
type PlatformBinding struct {
	AgentID        string `json:"agent_id"`
	OperationID    string `json:"operation_id"`
	Path           string `json:"path"`
	BodySHA256     string `json:"body_sha256"`
	IdempotencyKey string `json:"idempotency_key"`
}

// platformBindingMatch selects the platform approvals of binding $1 (as jsonb), the
// expression index serving the agent and the key.
const platformBindingMatch = `kind = 'platform' AND binding->>'agent_id' = $1::jsonb->>'agent_id'
	AND binding->>'idempotency_key' = $1::jsonb->>'idempotency_key' AND binding @> $1::jsonb`

// FindPlatformApproval returns the newest platform approval of the binding that still
// speaks for it: one whose expiry a repeat was already told of (expired and used) is
// spent, and so is one used before usedSince (the replay window: rule A6 lets a used
// approval through only for idempotency to replay). ErrNotFound when there is none.
func FindPlatformApproval(ctx context.Context, q Querier, b PlatformBinding, usedSince time.Time) (*Approval, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT `+approvalSelect+` FROM approvals WHERE `+platformBindingMatch+`
		AND NOT (status = 'expired' AND used_at IS NOT NULL) AND (used_at IS NULL OR used_at >= $2)
		ORDER BY requested_at DESC, id DESC LIMIT 1`, raw, usedSince.UTC())
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Approval])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// CreatePlatformApproval stores a, a pending platform approval, unless one of its binding
// was stored meanwhile (FindPlatformApproval with usedSince): then it returns that one and
// created false. A transaction-scoped advisory lock on the binding serialises two first
// calls of the same action, so they share one approval.
func CreatePlatformApproval(ctx context.Context, pool *pgxpool.Pool, a *Approval, b PlatformBinding, usedSince time.Time) (*Approval, bool, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, false, err
	}
	a.Kind, a.Binding, a.RunID, a.RunNodeID = ApprovalKindPlatform, raw, "", ""
	var out *Approval
	created := false
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('platform_approval:' || $1::text, 0))`, string(raw)); err != nil {
			return err
		}
		found, err := FindPlatformApproval(ctx, tx, b, usedSince)
		if err == nil {
			out = found
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if err := InsertApproval(ctx, tx, a); err != nil {
			return err
		}
		out, created = a, true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

// UsePlatformApproval spends an approved platform approval for the call it allows: it
// sets used_at when nothing has spent it yet, and reports whether this call did.
func UsePlatformApproval(ctx context.Context, q Querier, id string, at time.Time) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE approvals SET used_at = $2 WHERE id = $1 AND kind = 'platform' AND status = 'approved'
		AND used_at IS NULL`, id, at.UTC())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ExpirePlatformApproval records that a repeat found the approval past its expiry (rule
// A6, checked at use): a pending one is decided expired by "platform", an approved one
// that was never used lapses to expired with its decision kept, and either way used_at
// marks the expiry as told, so the next repeat asks again. It returns the row as it is
// now; ErrNotFound when it was used, rejected or told already.
func ExpirePlatformApproval(ctx context.Context, q Querier, id string, at time.Time) (*Approval, error) {
	rows, err := q.Query(ctx, `UPDATE approvals SET used_at = $2,
		decided_at = CASE WHEN status = 'pending' THEN $2 ELSE decided_at END,
		decided_by = CASE WHEN status = 'pending' THEN 'platform' ELSE decided_by END,
		reason = CASE WHEN status = 'pending' THEN 'approval expired' ELSE reason END,
		status = 'expired'
		WHERE id = $1 AND kind = 'platform' AND used_at IS NULL AND status IN ('pending', 'approved', 'expired')
		RETURNING `+approvalSelect, id, at.UTC())
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Approval])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}
