-- Wave 10 (DESIGN 5.24, 5.25): questions to the operator and run artifacts. Columns, one
-- partial index and one table; no data is rewritten, and every existing row reads with
-- the defaults (no options, no answer, no question, not parked, no artifacts).

-- 5.24 rule 2: the suggested answers of a question (a JSON array of 2 to 5 strings, null
-- for a free-text question), and rule 3: the operator's answer. Both stay null on the
-- other kinds of approval.
ALTER TABLE approvals ADD COLUMN options JSONB;
ALTER TABLE approvals ADD COLUMN answer TEXT;
-- 5.24 rule 2: the step's question count ("a fourth question of the step is refused")
-- reads the rows of kind question of one run and node, any status.
CREATE INDEX approvals_question_step_idx ON approvals (run_id, node_id) WHERE kind = 'question';

-- 5.24 rule 5: the question a continuation visit was created for (set null: the visit
-- outlives the approval row), and when the visit parked on its pending question and
-- released its worker (null for a visit that never parked).
ALTER TABLE run_nodes ADD COLUMN question_id TEXT REFERENCES approvals (id) ON DELETE SET NULL;
ALTER TABLE run_nodes ADD COLUMN parked_at TIMESTAMPTZ;

-- 5.25 rule 4: the files a run delivers, one row per path of the run (a later visit that
-- changes a file updates its row in place). The files themselves live under ARTIFACT_DIR
-- as <run id>/<artifact id>; the rows go with the run (cascade).
CREATE TABLE run_artifacts (
    id           TEXT PRIMARY KEY,
    run_id       TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    node_id      TEXT NOT NULL,
    visit        INT NOT NULL,
    path         TEXT NOT NULL,
    size         BIGINT NOT NULL,
    sha256       TEXT NOT NULL,
    content_type TEXT NOT NULL,
    generated    BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    UNIQUE (run_id, path)
);
