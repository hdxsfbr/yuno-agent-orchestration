-- Delivery process wave 3 (DESIGN 5.29 rule 11): the commit a merge made, as the provider
-- reports it, so a wave ships on the pipeline of that commit whatever the clocks say.
ALTER TABLE merge_requests ADD COLUMN merge_sha TEXT;
