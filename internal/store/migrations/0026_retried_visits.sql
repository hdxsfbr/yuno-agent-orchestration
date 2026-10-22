-- DESIGN 5.21 rule 4 and 5.24 rule 7 (D-093): a Retry starts the failed step afresh.
-- retried_at marks the step's latest visit when the operator retries the run from that step:
-- the questions and tool approvals of that visit and of the step's earlier visits are not
-- carried into, continued by, or counted for the step's visits after the retry. Null for
-- every visit no retry followed.
ALTER TABLE run_nodes ADD COLUMN retried_at TIMESTAMPTZ;
