-- Delivery process wave 2 (DESIGN 5.29 rule 1): a feature's plan, its tickets and the events
-- of their advance; the project's pause flag for waves; the draft flag of a merge request.

-- One plan per feature of a project, while open. plan_run_id is the plan pipeline run that produced it;
-- plan_mr_id the merge request carrying the plan documents (null until opened).
CREATE TABLE plans (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    feature      TEXT NOT NULL,
    plan_run_id  TEXT NOT NULL REFERENCES runs (id),
    plan_mr_id   TEXT REFERENCES merge_requests (id) ON DELETE SET NULL,
    status       TEXT NOT NULL DEFAULT 'publishing' CHECK (status IN
                 ('publishing', 'waiting_plan_merge', 'building', 'shipping', 'accepting', 'done', 'cancelled')),
    current_wave INT NOT NULL DEFAULT 1 CHECK (current_wave >= 1),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A feature name is unique among a project's open plans only: a finished or cancelled
-- plan frees it (rule 1).
CREATE UNIQUE INDEX plans_open_feature_idx ON plans (project_id, feature) WHERE status NOT IN ('done', 'cancelled');

-- A project has at most one plan that is not finished (rule 1).
CREATE UNIQUE INDEX plans_one_open_idx ON plans (project_id) WHERE status NOT IN ('done', 'cancelled');
CREATE INDEX plans_project_idx ON plans (project_id, created_at DESC);

CREATE TABLE plan_tickets (
    plan_id          TEXT NOT NULL REFERENCES plans (id) ON DELETE CASCADE,
    ticket_id        TEXT NOT NULL,
    wave             INT NOT NULL CHECK (wave >= 1),
    title            TEXT NOT NULL,
    issue_number     BIGINT,
    issue_url        TEXT NOT NULL DEFAULT '',
    run_id           TEXT REFERENCES runs (id) ON DELETE SET NULL,
    merge_request_id TEXT REFERENCES merge_requests (id) ON DELETE SET NULL,
    status           TEXT NOT NULL DEFAULT 'pending' CHECK (status IN
                     ('pending', 'released', 'building', 'in_review', 'merged', 'failed', 'closed')),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (plan_id, ticket_id)
);

-- Intake's take and the sync find a ticket by its issue number (rule 10).
CREATE INDEX plan_tickets_issue_idx ON plan_tickets (issue_number) WHERE issue_number IS NOT NULL;
CREATE INDEX plan_tickets_run_idx ON plan_tickets (run_id) WHERE run_id IS NOT NULL;
CREATE INDEX plan_tickets_mr_idx ON plan_tickets (merge_request_id) WHERE merge_request_id IS NOT NULL;

CREATE TABLE plan_events (
    id        TEXT PRIMARY KEY,
    plan_id   TEXT NOT NULL REFERENCES plans (id) ON DELETE CASCADE,
    at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    kind      TEXT NOT NULL,
    wave      INT,
    ticket_id TEXT,
    detail    JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- set once the operator has been told of the event (rule 8); null for events that are not told
    notified_at TIMESTAMPTZ
);
CREATE INDEX plan_events_plan_idx ON plan_events (plan_id, at DESC, id DESC);

-- Rule 1: pausing stops the next release and the next ship run, nothing in flight.
ALTER TABLE projects ADD COLUMN waves_paused BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE projects ADD COLUMN waves_paused_reason TEXT;

-- Rule 4: the record of a request opened as a draft.
ALTER TABLE merge_requests ADD COLUMN draft BOOLEAN NOT NULL DEFAULT false;
