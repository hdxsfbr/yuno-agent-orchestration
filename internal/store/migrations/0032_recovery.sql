-- DESIGN W2, recovery: the operator's and the Chief's commands to a run, the run's worker
-- lease, the confirmation of a stopped turn, and the run status paused.

-- Rules 1 to 6: one row per command, from receipt to its outcome. acked_at is when the
-- worker holding the run's lease took it (or the api, for a run no worker holds); applied_at
-- when its effect was confirmed. A cancel supersedes the pause, resume and steer commands
-- still pending before it.
CREATE TABLE run_commands (
    id          TEXT PRIMARY KEY,
    run_id      TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('cancel', 'pause', 'resume', 'retry', 'steer')),
    node_id     TEXT,
    payload     JSONB NOT NULL DEFAULT '{}',
    actor       JSONB NOT NULL,
    request_id  TEXT NOT NULL DEFAULT '',
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    acked_at    TIMESTAMPTZ,
    applied_at  TIMESTAMPTZ,
    outcome     TEXT NOT NULL DEFAULT 'pending'
        CHECK (outcome IN ('pending', 'applied', 'rejected', 'failed', 'superseded')),
    detail      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX run_commands_run_idx ON run_commands (run_id, received_at DESC, id DESC);
CREATE INDEX run_commands_pending_idx ON run_commands (run_id, received_at, id) WHERE outcome = 'pending';

-- Rules 7 to 9: the worker lease. Every claim and every move to queued increments
-- lease_epoch; a worker's writes to its run carry the epoch it claimed with.
ALTER TABLE runs ADD COLUMN lease_epoch BIGINT NOT NULL DEFAULT 0;
-- Rules 11 to 13: whether the last turn of a stopped run is confirmed stopped. '' when
-- nothing was stopped; unconfirmed until the gateway shows no active run for it.
ALTER TABLE runs ADD COLUMN cancel_state TEXT NOT NULL DEFAULT ''
    CONSTRAINT runs_cancel_state_check CHECK (cancel_state IN ('', 'confirmed', 'unconfirmed'));
CREATE INDEX runs_cancel_unconfirmed_idx ON runs (updated_at) WHERE cancel_state = 'unconfirmed';

-- Per visit:
--   park_reason       'paused' for a visit the operator paused (rule 14), '' otherwise;
--   resumed_at        when a paused visit was resumed, with the resume's note (rule 15);
--   start_sha         the run repository commit the visit's checkout started from;
--   retry_from_at     set on the visit a retry from that step rewinds to (rule 17);
--   turn_unconfirmed  the visit's turn was not confirmed stopped (rules 11 and 12).
ALTER TABLE run_nodes
    ADD COLUMN park_reason      TEXT NOT NULL DEFAULT '',
    ADD COLUMN resumed_at       TIMESTAMPTZ,
    ADD COLUMN resume_note      TEXT,
    ADD COLUMN start_sha        TEXT,
    ADD COLUMN retry_from_at    TIMESTAMPTZ,
    ADD COLUMN turn_unconfirmed BOOLEAN NOT NULL DEFAULT false;

-- A paused run is live: it keeps its merge request and its issue (DESIGN 5.22 rule 1, 5.23
-- rule 3) until it ends.
DROP INDEX runs_merge_request_live_idx;
CREATE UNIQUE INDEX runs_merge_request_live_idx ON runs (merge_request_id)
    WHERE merge_request_id IS NOT NULL AND status IN ('queued', 'running', 'waiting_approval', 'paused');
DROP INDEX runs_issue_live_idx;
CREATE UNIQUE INDEX runs_issue_live_idx ON runs (trigger_ref)
    WHERE trigger = 'issue' AND status IN ('queued', 'running', 'waiting_approval', 'paused');
