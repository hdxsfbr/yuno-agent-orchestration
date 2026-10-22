-- DESIGN 5.29 rule 4: a run whose merge request was merged before its last push opens a
-- follow-up request with what was left. The follow-up is a record of the same run that names
-- the record it follows; the run's own record is the one that follows none, so the
-- one-record-per-run index now covers only those, and a record has at most one follow-up.
ALTER TABLE merge_requests ADD COLUMN follow_up_of TEXT REFERENCES merge_requests (id) ON DELETE CASCADE;
DROP INDEX merge_requests_run_idx;
CREATE UNIQUE INDEX merge_requests_run_idx ON merge_requests (run_id) WHERE follow_up_of IS NULL;
CREATE UNIQUE INDEX merge_requests_follow_up_idx ON merge_requests (follow_up_of);
