-- Wave 11 (DESIGN 5.26): parallel steps. One table and one column; no data is rewritten,
-- and every existing row reads with the defaults (no branches, a visit in no branch).

-- 5.26 rule 3: one row per branch of a fork visit. The fork's own run_nodes row is the
-- visit the branches belong to; head is the branch's first node (the fork edge's target)
-- and names the branch; current_node is where the branch is (the node its next or current
-- visit runs); status is running | waiting_approval | succeeded | failed | cancelled
-- (waiting_approval: the branch's newest visit parked on a question, 5.26 rule 8).
CREATE TABLE run_branches (
    id             TEXT PRIMARY KEY,
    run_id         TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    fork_visit_id  TEXT NOT NULL REFERENCES run_nodes (id) ON DELETE CASCADE,
    fork_id        TEXT NOT NULL,
    head           TEXT NOT NULL,
    status         TEXT NOT NULL,
    current_node   TEXT NOT NULL,
    failure_reason TEXT,
    started_at     TIMESTAMPTZ NOT NULL,
    finished_at    TIMESTAMPTZ,
    UNIQUE (fork_visit_id, head)
);
CREATE INDEX run_branches_run_idx ON run_branches (run_id, started_at);

-- 5.26 rule 4: the branch a visit was made in (set null: the visit outlives the branch
-- row only if the row is deleted by hand; the cascade from the run removes both).
ALTER TABLE run_nodes ADD COLUMN branch_id TEXT REFERENCES run_branches (id) ON DELETE SET NULL;
CREATE INDEX run_nodes_branch_idx ON run_nodes (branch_id) WHERE branch_id IS NOT NULL;
