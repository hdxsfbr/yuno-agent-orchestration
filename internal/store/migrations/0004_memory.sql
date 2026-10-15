-- Agent memory facts (DESIGN 5.10) and the trace's side-effect lookup (DESIGN 5.4).

CREATE TABLE memory_facts (
    id            TEXT PRIMARY KEY,
    scope         TEXT NOT NULL DEFAULT 'agent',   -- agent (conversation scope arrives with the channel)
    agent_id      TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    key           TEXT NOT NULL,
    value         TEXT NOT NULL,
    source_run_id TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ
);
CREATE UNIQUE INDEX memory_facts_key_idx ON memory_facts (agent_id, scope, key);
CREATE INDEX memory_facts_recent_idx ON memory_facts (agent_id, scope, updated_at DESC);

CREATE INDEX side_effects_run_idx ON side_effects (run_id, created_at);
