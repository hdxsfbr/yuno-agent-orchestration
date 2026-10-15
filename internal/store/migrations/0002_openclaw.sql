-- OpenClaw-shaped additions (DESIGN revision 2, section 4).

ALTER TABLE agents
    ADD COLUMN openclaw_agent_id TEXT,
    ADD COLUMN skills            JSONB NOT NULL DEFAULT '[]',
    ADD COLUMN thinking          TEXT  NOT NULL DEFAULT 'medium',
    ADD COLUMN exec_policy       JSONB NOT NULL DEFAULT '{}',
    ADD COLUMN materialized_hash TEXT  NOT NULL DEFAULT '';
CREATE UNIQUE INDEX agents_openclaw_id_idx ON agents (openclaw_agent_id) WHERE openclaw_agent_id IS NOT NULL AND deleted_at IS NULL;

CREATE TABLE skills (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    body        TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX skills_name_idx ON skills (name);

ALTER TABLE run_nodes
    ADD COLUMN session_key     TEXT,
    ADD COLUMN openclaw_run_id TEXT;

-- Nullable JSONB columns become JSON null so they scan into json.RawMessage uniformly.
UPDATE run_nodes SET output = 'null'::jsonb WHERE output IS NULL;
UPDATE run_nodes SET pending_tool_results = 'null'::jsonb WHERE pending_tool_results IS NULL;
ALTER TABLE run_nodes
    ALTER COLUMN output SET DEFAULT 'null'::jsonb, ALTER COLUMN output SET NOT NULL,
    ALTER COLUMN pending_tool_results SET DEFAULT 'null'::jsonb, ALTER COLUMN pending_tool_results SET NOT NULL;
UPDATE tool_calls SET output = 'null'::jsonb WHERE output IS NULL;
ALTER TABLE tool_calls ALTER COLUMN output SET DEFAULT 'null'::jsonb, ALTER COLUMN output SET NOT NULL;

ALTER TABLE approvals ADD COLUMN external_id TEXT;
CREATE INDEX approvals_external_idx ON approvals (external_id) WHERE external_id IS NOT NULL;

-- The agent workspace is OpenClaw's; run artifacts live under <workspace>/runs/<run_id>/.
DROP TABLE workspace_files;
