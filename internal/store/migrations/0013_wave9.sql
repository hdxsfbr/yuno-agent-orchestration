-- Wave 9 (DESIGN 5.22, 5.23): a run that updates an open merge request from its own
-- branch, and issue intake. Columns, two partial unique indexes, a partial index for the
-- issue closer and the intake settings of a project; no data is rewritten.

-- 5.22 rule 1: the merge request record a run names and updates (set null: the run
-- outlives the record's deletion). The record the run opened stays merge_requests.run_id.
ALTER TABLE runs ADD COLUMN merge_request_id TEXT REFERENCES merge_requests (id) ON DELETE SET NULL;
-- 5.22 rule 2: the commit the run's project checkout was at (git rev-parse HEAD after the
-- clone, and after each push), the base the branch guard of rule 4 compares against.
ALTER TABLE runs ADD COLUMN checkout_sha TEXT;

-- 5.22 rule 1: one live run at a time pushes onto a request's branch. A run that ends
-- leaves the index and frees the request; a retry re-enters it.
CREATE UNIQUE INDEX runs_merge_request_live_idx ON runs (merge_request_id)
    WHERE merge_request_id IS NOT NULL AND status IN ('queued', 'running', 'waiting_approval');
-- 5.23 rule 3: one live run per issue (trigger_ref is "<project id>#<n>"); a second take
-- by another api process is a 23505 that counts as taken.
CREATE UNIQUE INDEX runs_issue_live_idx ON runs (trigger_ref)
    WHERE trigger = 'issue' AND status IN ('queued', 'running', 'waiting_approval');
-- 5.23 rule 5: the closer's list, the issue runs whose outcome is not posted yet, newest
-- finished first. The queries write the trigger as a literal so the planner can prove the
-- predicate (a bound parameter cannot); the live-run lookup does the same for the index above.
CREATE INDEX runs_issue_unreported_idx ON runs (finished_at DESC NULLS LAST, id)
    WHERE trigger = 'issue' AND reported_at IS NULL;

-- 5.22 rule 7: the request a schedule's runs update; the row survives the record.
ALTER TABLE schedules ADD COLUMN merge_request_id TEXT REFERENCES merge_requests (id) ON DELETE SET NULL;

-- 5.23 rule 1: the intake settings of a project and the poll's bookkeeping (the loop
-- writes intake_polled_at and intake_last_error; the operator writes the first three).
ALTER TABLE projects ADD COLUMN intake_enabled BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE projects ADD COLUMN intake_label TEXT NOT NULL DEFAULT 'yuno';
ALTER TABLE projects ADD COLUMN intake_workflow_id TEXT REFERENCES workflows (id) ON DELETE SET NULL;
ALTER TABLE projects ADD COLUMN intake_polled_at TIMESTAMPTZ;
ALTER TABLE projects ADD COLUMN intake_last_error TEXT;
