-- DESIGN 5.31 (D-098): audit, idempotency and versions, for the UI and the Chief alike.

-- Rules 1 and 2: one row per /api request that changes something (POST, PUT, PATCH or
-- DELETE) and per /api request the bearer check refused. The row is written with outcome
-- 'started' before the handler runs and completed after it; a row left 'started' is a
-- request whose completion was not recorded. route is the mux pattern without its method;
-- target is the resource the request reads before and after ("" when it has none).
-- request, before and after are redacted JSON (at most 16 KiB, 64 KiB and 64 KiB); null
-- when there is nothing to record, and before or after is null when the target did not
-- exist. approval_id and evidence are what the platform gate noted.
CREATE TABLE audit_events (
    id              TEXT PRIMARY KEY,
    at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    request_id      TEXT NOT NULL DEFAULT '',
    actor_kind      TEXT NOT NULL CHECK (actor_kind IN ('operator', 'agent', 'anonymous')),
    actor_id        TEXT NOT NULL DEFAULT '',
    actor_name      TEXT NOT NULL DEFAULT '',
    via             TEXT NOT NULL DEFAULT '',
    on_behalf_of    TEXT NOT NULL DEFAULT '',
    method          TEXT NOT NULL,
    route           TEXT NOT NULL DEFAULT '',
    path            TEXT NOT NULL,
    target          TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL DEFAULT '',
    status          INT,
    outcome         TEXT NOT NULL DEFAULT 'started' CHECK (outcome IN
                    ('started', 'ok', 'denied', 'failed', 'pending_approval', 'replayed')),
    error_code      TEXT NOT NULL DEFAULT '',
    request         JSONB,
    before          JSONB,
    after           JSONB,
    approval_id     TEXT NOT NULL DEFAULT '',
    evidence        JSONB,
    duration_ms     BIGINT
);
-- Rule 5: newest first, filtered by actor, target or outcome.
CREATE INDEX audit_events_at_idx ON audit_events (at DESC, id DESC);
CREATE INDEX audit_events_actor_idx ON audit_events (actor_id, at DESC, id DESC) WHERE actor_id <> '';
CREATE INDEX audit_events_target_idx ON audit_events (target, at DESC, id DESC) WHERE target <> '';

-- Rule 3: an Idempotency-Key, per actor ("operator" or "agent:<id>"), method and path, with
-- the SHA-256 of the body it was first sent with. state is 'running' while the first
-- request runs, then 'done' with the response it got (a 5xx is not kept: the row is
-- deleted so the caller can retry). body is null when the response was over 1 MiB.
-- Keys expire 24 hours after first use.
CREATE TABLE idempotency_keys (
    actor        TEXT NOT NULL,
    method       TEXT NOT NULL,
    path         TEXT NOT NULL,
    key          TEXT NOT NULL,
    body_sha256  TEXT NOT NULL,
    state        TEXT NOT NULL DEFAULT 'running' CHECK (state IN ('running', 'done')),
    status       INT,
    body         BYTEA,
    content_type TEXT NOT NULL DEFAULT '',
    etag         TEXT NOT NULL DEFAULT '',
    location     TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (actor, method, path, key)
);
CREATE INDEX idempotency_keys_expires_idx ON idempotency_keys (expires_at);
