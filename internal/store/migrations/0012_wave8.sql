-- Wave 8 (DESIGN 5.18 to 5.21): project instructions and shared project notes, project
-- budgets with their alert ledger, merge requests as records, and the retry counter of
-- a run re-queued from the step it stopped at.

-- 5.18 rule 1, 5.19 rule 1: the operator's instructions and the caps (micro-USD, null = none).
ALTER TABLE projects ADD COLUMN instructions TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN budget_day_usd_micros BIGINT;
ALTER TABLE projects ADD COLUMN budget_month_usd_micros BIGINT;

-- 5.18 rule 2: notes shared by every agent of a run on the project; the note outlives
-- the agent that saved it.
CREATE TABLE project_facts (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    key           TEXT NOT NULL,
    value         TEXT NOT NULL,
    agent_id      TEXT REFERENCES agents (id) ON DELETE SET NULL,
    source_run_id TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ
);
CREATE UNIQUE INDEX project_facts_key_idx ON project_facts (project_id, key);
CREATE INDEX project_facts_recent_idx ON project_facts (project_id, updated_at DESC);

-- 5.19 rule 5: one row per alert sent, so a restart never re-sends one.
CREATE TABLE project_budget_alerts (
    project_id   TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    window_kind  TEXT NOT NULL,            -- day | month
    window_start TIMESTAMPTZ NOT NULL,
    level        TEXT NOT NULL,            -- warning | reached
    sent_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, window_kind, window_start, level)
);

-- 5.20 rule 1: one merge request per run, refreshed from the provider.
CREATE TABLE merge_requests (
    id                 TEXT PRIMARY KEY,
    project_id         TEXT NOT NULL REFERENCES projects (id),
    run_id             TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    provider           TEXT NOT NULL,
    number             BIGINT NOT NULL,
    url                TEXT NOT NULL,
    source_branch      TEXT NOT NULL,
    target_branch      TEXT NOT NULL,
    title              TEXT NOT NULL DEFAULT '',
    state              TEXT NOT NULL DEFAULT 'open',   -- open | merged | closed
    ci_status          TEXT NOT NULL DEFAULT 'none',   -- none | pending | passed | failed
    head_sha           TEXT,
    unresolved_threads INT,
    last_sync_error    TEXT,
    synced_at          TIMESTAMPTZ,
    reported_state     TEXT,                           -- merged | closed, once reported
    merged_by          TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at          TIMESTAMPTZ
);
CREATE UNIQUE INDEX merge_requests_number_idx ON merge_requests (project_id, number);
CREATE UNIQUE INDEX merge_requests_run_idx ON merge_requests (run_id);
CREATE INDEX merge_requests_open_idx ON merge_requests (synced_at NULLS FIRST, created_at) WHERE state = 'open';

-- 5.21 rule 3: how many times the operator retried the run; 5.19 rule 2: spend by window.
ALTER TABLE runs ADD COLUMN retries INT NOT NULL DEFAULT 0;
CREATE INDEX runs_project_created_idx ON runs (project_id, created_at) WHERE project_id IS NOT NULL;
