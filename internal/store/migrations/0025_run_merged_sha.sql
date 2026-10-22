-- DESIGN 5.22 rule 13 (D-088): before the first turn of a run on a merge request the
-- platform merges the project's default branch into the request's branch in the checkout.
-- merged_sha is the default branch commit it merged, which the run's next push makes the
-- second parent of its commit, so the request's branch gets a real merge; null when nothing
-- was merged, and cleared by that push (with checkout_sha).
ALTER TABLE runs ADD COLUMN merged_sha TEXT;
