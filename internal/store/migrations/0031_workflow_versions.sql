-- DESIGN W4 rule 1: every version of a workflow, so it can be read back and restored.
-- Workflow create and every update write the new version in the same statement as the
-- workflow row. created_by is the actor of the request that wrote it, {"kind":"platform"}
-- for a write no request made (the seed, the template loader, a quick-run workflow), and
-- {"kind":"backfill"} for the versions recovered below.
CREATE TABLE workflow_versions (
    workflow_id  TEXT NOT NULL REFERENCES workflows (id) ON DELETE CASCADE,
    version      INT  NOT NULL,
    name         TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    definition   JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by   JSONB NOT NULL,
    PRIMARY KEY (workflow_id, version)
);

-- The backfill. Each workflow's current version, as it is stored now.
INSERT INTO workflow_versions (workflow_id, version, name, description, definition, created_at, created_by)
SELECT id, version, name, description, definition, updated_at, '{"kind":"backfill"}'
FROM workflows;

-- Every older version a run still holds: runs.workflow_snapshot was the only copy of it.
-- A snapshot is the definition as the run ran it, without the run's overrides. When the
-- runs of one version hold different snapshots (their limits were set per run), the one
-- most runs hold is kept, the earliest on a tie; the current version above always wins.
-- Runs keep no name or description, so those are the workflow's current ones.
WITH held AS (
    SELECT workflow_id, workflow_version,
           CASE WHEN jsonb_typeof(workflow_snapshot) = 'object' THEN workflow_snapshot - 'run_overrides'
                ELSE workflow_snapshot END AS definition,
           count(*) AS runs, min(created_at) AS first_at
    FROM runs
    GROUP BY 1, 2, 3
)
INSERT INTO workflow_versions (workflow_id, version, name, description, definition, created_at, created_by)
SELECT DISTINCT ON (h.workflow_id, h.workflow_version)
       h.workflow_id, h.workflow_version, w.name, w.description, h.definition, h.first_at, '{"kind":"backfill"}'
FROM held h JOIN workflows w ON w.id = h.workflow_id
ORDER BY h.workflow_id, h.workflow_version, h.runs DESC, h.first_at
ON CONFLICT (workflow_id, version) DO NOTHING;
