package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Audit outcomes (DESIGN 5.31 rule 1). AuditStarted is a row whose request has not been
// completed (yet, or ever).
const (
	AuditStarted         = "started"
	AuditOK              = "ok"
	AuditDenied          = "denied"
	AuditFailed          = "failed"
	AuditPendingApproval = "pending_approval"
	AuditReplayed        = "replayed"
)

// AuditOutcomes are the outcomes a completed row can have.
var AuditOutcomes = []string{AuditStarted, AuditOK, AuditDenied, AuditFailed, AuditPendingApproval, AuditReplayed}

// AuditEvent is one audited request (DESIGN 5.31 rule 1). Request, Before and After are
// redacted JSON, nil when there is nothing to record; Status and DurationMs are nil while
// the row is started.
type AuditEvent struct {
	ID             string          `json:"id"`
	At             time.Time       `json:"at"`
	RequestID      string          `json:"request_id"`
	ActorKind      string          `json:"actor_kind"`
	ActorID        string          `json:"actor_id"`
	ActorName      string          `json:"actor_name"`
	Via            string          `json:"via"`
	OnBehalfOf     string          `json:"on_behalf_of"`
	Method         string          `json:"method"`
	Route          string          `json:"route"`
	Path           string          `json:"path"`
	Target         string          `json:"target"`
	IdempotencyKey string          `json:"idempotency_key"`
	Status         *int            `json:"status"`
	Outcome        string          `json:"outcome"`
	ErrorCode      string          `json:"error_code"`
	Request        json.RawMessage `json:"request"`
	Before         json.RawMessage `json:"before"`
	After          json.RawMessage `json:"after"`
	ApprovalID     string          `json:"approval_id"`
	Evidence       json.RawMessage `json:"evidence"`
	DurationMs     *int64          `json:"duration_ms"`
}

const auditSelect = `id, at, request_id, actor_kind, actor_id, actor_name, via, on_behalf_of, method, route, path,
	target, idempotency_key, status, outcome, error_code, request, before, after, approval_id, evidence, duration_ms`

// jsonArg is a JSON column's argument: SQL NULL for an empty document.
func jsonArg(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// InsertAuditEvent writes e as started, with a fresh id, and sets e.ID and e.At.
func InsertAuditEvent(ctx context.Context, q Querier, e *AuditEvent) error {
	e.ID = NewID()
	e.Outcome = AuditStarted
	return q.QueryRow(ctx, `INSERT INTO audit_events (id, request_id, actor_kind, actor_id, actor_name, via, on_behalf_of,
		method, route, path, target, idempotency_key, request, before)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb, $14::jsonb) RETURNING at`,
		e.ID, e.RequestID, e.ActorKind, e.ActorID, e.ActorName, e.Via, e.OnBehalfOf, e.Method, e.Route, e.Path,
		e.Target, e.IdempotencyKey, jsonArg(e.Request), jsonArg(e.Before)).Scan(&e.At)
}

// CompleteAuditEvent records how e's request ended: status, outcome, error code, after,
// the gate's approval and evidence, and the duration.
func CompleteAuditEvent(ctx context.Context, q Querier, e *AuditEvent) error {
	tag, err := q.Exec(ctx, `UPDATE audit_events SET status = $2, outcome = $3, error_code = $4, after = $5::jsonb,
		approval_id = $6, evidence = $7::jsonb, duration_ms = $8 WHERE id = $1`,
		e.ID, e.Status, e.Outcome, e.ErrorCode, jsonArg(e.After), e.ApprovalID, jsonArg(e.Evidence), e.DurationMs)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// AuditFilter narrows ListAuditEvents (DESIGN 5.31 rule 5). Actor matches the actor's kind
// or id; Target the target exactly; Since is inclusive; After, when set, is the position
// (at, id) the previous page ended at.
type AuditFilter struct {
	Actor   string
	Target  string
	Outcome string
	Since   *time.Time
	AfterAt *time.Time
	AfterID string
	Limit   int
}

// ListAuditEvents lists audit rows newest first.
func ListAuditEvents(ctx context.Context, q Querier, f AuditFilter) ([]AuditEvent, error) {
	rows, err := q.Query(ctx, `SELECT `+auditSelect+` FROM audit_events
		WHERE ($1 = '' OR actor_kind = $1 OR actor_id = $1)
		  AND ($2 = '' OR target = $2)
		  AND ($3 = '' OR outcome = $3)
		  AND ($4::timestamptz IS NULL OR at >= $4)
		  AND ($5::timestamptz IS NULL OR (at, id) < ($5, $6))
		ORDER BY at DESC, id DESC LIMIT $7`,
		f.Actor, f.Target, f.Outcome, f.Since, f.AfterAt, f.AfterID, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEvent{}
	for rows.Next() {
		e, err := scanAuditEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetAuditEvent reads one audit row.
func GetAuditEvent(ctx context.Context, q Querier, id string) (*AuditEvent, error) {
	e, err := scanAuditEvent(q.QueryRow(ctx, `SELECT `+auditSelect+` FROM audit_events WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func scanAuditEvent(row pgx.Row) (AuditEvent, error) {
	var e AuditEvent
	var request, before, after, evidence []byte
	err := row.Scan(&e.ID, &e.At, &e.RequestID, &e.ActorKind, &e.ActorID, &e.ActorName, &e.Via, &e.OnBehalfOf,
		&e.Method, &e.Route, &e.Path, &e.Target, &e.IdempotencyKey, &e.Status, &e.Outcome, &e.ErrorCode,
		&request, &before, &after, &e.ApprovalID, &evidence, &e.DurationMs)
	e.Request, e.Before, e.After, e.Evidence = rawOrNil(request), rawOrNil(before), rawOrNil(after), rawOrNil(evidence)
	return e, err
}

func rawOrNil(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

// ---- idempotency keys (DESIGN 5.31 rule 3) ------------------------------------------

// IdempotencyTTL is how long a key is remembered after its first use.
const IdempotencyTTL = 24 * time.Hour

// Idempotency key states.
const (
	IdempotencyRunning = "running"
	IdempotencyDone    = "done"
)

// IdempotencyKey is one remembered key: who sent it for which method and path, the
// SHA-256 of the body it came with, and once done the response it got. Body is nil when
// the response was not kept (over 1 MiB).
type IdempotencyKey struct {
	Actor       string
	Method      string
	Path        string
	Key         string
	BodySHA256  string
	State       string
	Status      int
	Body        []byte
	ContentType string
	ETag        string
	Location    string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// ClaimIdempotencyKey claims k for a first use at now: it reports true when this call
// claimed it (a key that expired counts as unused), and otherwise returns the row that
// holds it.
func ClaimIdempotencyKey(ctx context.Context, q Querier, k IdempotencyKey, now time.Time) (bool, *IdempotencyKey, error) {
	now = now.UTC()
	// A row released between the insert and the read is gone: claim again.
	for range 3 {
		if _, err := q.Exec(ctx, `DELETE FROM idempotency_keys WHERE actor = $1 AND method = $2 AND path = $3 AND key = $4
			AND expires_at <= $5`, k.Actor, k.Method, k.Path, k.Key, now); err != nil {
			return false, nil, err
		}
		tag, err := q.Exec(ctx, `INSERT INTO idempotency_keys (actor, method, path, key, body_sha256, state, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, 'running', $6, $7) ON CONFLICT DO NOTHING`,
			k.Actor, k.Method, k.Path, k.Key, k.BodySHA256, now, now.Add(IdempotencyTTL))
		if err != nil {
			return false, nil, err
		}
		if tag.RowsAffected() == 1 {
			return true, nil, nil
		}
		got, err := GetIdempotencyKey(ctx, q, k.Actor, k.Method, k.Path, k.Key)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return false, nil, err
		}
		return false, got, nil
	}
	return false, nil, errors.New("idempotency key: claim kept losing to a release")
}

// GetIdempotencyKey reads one key.
func GetIdempotencyKey(ctx context.Context, q Querier, actor, method, path, key string) (*IdempotencyKey, error) {
	var k IdempotencyKey
	var status *int
	err := q.QueryRow(ctx, `SELECT actor, method, path, key, body_sha256, state, status, body, content_type, etag, location,
		created_at, expires_at FROM idempotency_keys WHERE actor = $1 AND method = $2 AND path = $3 AND key = $4`,
		actor, method, path, key).Scan(&k.Actor, &k.Method, &k.Path, &k.Key, &k.BodySHA256, &k.State, &status, &k.Body,
		&k.ContentType, &k.ETag, &k.Location, &k.CreatedAt, &k.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if status != nil {
		k.Status = *status
	}
	return &k, nil
}

// CompleteIdempotencyKey stores the response of k's first use (Body nil when not kept).
func CompleteIdempotencyKey(ctx context.Context, q Querier, k IdempotencyKey) error {
	_, err := q.Exec(ctx, `UPDATE idempotency_keys SET state = 'done', status = $5, body = $6, content_type = $7, etag = $8,
		location = $9 WHERE actor = $1 AND method = $2 AND path = $3 AND key = $4`,
		k.Actor, k.Method, k.Path, k.Key, k.Status, k.Body, k.ContentType, k.ETag, k.Location)
	return err
}

// ReleaseIdempotencyKey forgets a claim whose request failed with a 5xx (or panicked), so
// the caller can retry with the same key.
func ReleaseIdempotencyKey(ctx context.Context, q Querier, actor, method, path, key string) error {
	_, err := q.Exec(ctx, `DELETE FROM idempotency_keys WHERE actor = $1 AND method = $2 AND path = $3 AND key = $4`,
		actor, method, path, key)
	return err
}

// IdempotencyAbandonedAfter is how long a claim may stay running before the sweep takes it
// for one whose process died: far longer than any handler runs.
const IdempotencyAbandonedAfter = time.Hour

// SweepIdempotencyKeys deletes the keys expired at now, and the claims still running an
// hour after they were made, and returns how many it deleted.
func SweepIdempotencyKeys(ctx context.Context, q Querier, now time.Time) (int64, error) {
	now = now.UTC()
	tag, err := q.Exec(ctx, `DELETE FROM idempotency_keys WHERE expires_at <= $1 OR (state = 'running' AND created_at <= $2)`,
		now, now.Add(-IdempotencyAbandonedAfter))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
