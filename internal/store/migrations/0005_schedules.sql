-- Durable schedules (DESIGN 5.9) and the indexes the guardrail rate window and the
-- memory sweeper read (DESIGN 5.11 rule 2, 5.10 rule 6).

CREATE TABLE schedules (
    id          TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL REFERENCES workflows (id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('cron', 'delay')),
    cron_expr   TEXT,                       -- kind cron: 5-field standard expression
    tz          TEXT NOT NULL DEFAULT 'UTC', -- IANA zone the expression is read in
    run_at      TIMESTAMPTZ,                -- kind delay: the one instant it fires
    enabled     BOOLEAN NOT NULL DEFAULT true,
    next_run_at TIMESTAMPTZ,                -- NULL while disabled
    last_run_at TIMESTAMPTZ,
    last_run_id TEXT,
    last_error  TEXT,
    input       JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX schedules_due_idx ON schedules (next_run_at) WHERE enabled;
CREATE INDEX schedules_workflow_idx ON schedules (workflow_id);

-- Runs created in the last hour, per agent, at every trigger.
CREATE INDEX runs_created_idx ON runs (created_at);

-- Eviction by age and the expiry sweep.
CREATE INDEX memory_facts_created_idx ON memory_facts (agent_id, scope, created_at);
CREATE INDEX memory_facts_expires_idx ON memory_facts (expires_at) WHERE expires_at IS NOT NULL;
