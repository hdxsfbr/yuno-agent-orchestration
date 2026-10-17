-- Projects as records (DESIGN 5.17): the repositories runs work on, with the git token
-- encrypted at rest under PROJECT_TOKEN_KEY (rule 2), and the project a run names with
-- a snapshot of its settings (rule 4). The first row of an install that set PROJECT_*
-- is created by the api's bootstrap at start (rule 9).
CREATE TABLE projects (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    repo_url        TEXT NOT NULL,             -- https clone URL, no credentials
    provider        TEXT NOT NULL,             -- gitlab | github
    default_branch  TEXT NOT NULL DEFAULT 'main',
    sandbox_network BOOLEAN NOT NULL DEFAULT false,
    description     TEXT NOT NULL DEFAULT '',
    token_enc       BYTEA,                     -- AES-GCM blob; never served by the API
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ                -- soft delete: runs keep their snapshot, the row keeps the token
);
CREATE UNIQUE INDEX projects_name_idx ON projects (lower(name)) WHERE deleted_at IS NULL;

ALTER TABLE runs
    ADD COLUMN project_id TEXT REFERENCES projects (id),
    ADD COLUMN project_snapshot JSONB;        -- {name, repo_url, provider, default_branch, sandbox_network}
CREATE INDEX runs_project_idx ON runs (project_id, created_at DESC) WHERE project_id IS NOT NULL;
