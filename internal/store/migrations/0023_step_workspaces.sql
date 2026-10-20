-- DESIGN 5.30 (D-083): every run step works in its own private workspace, and the checkout
-- moves between steps as commits of a per-run bare repository. workspace_dir is the visit's
-- private workspace under the gateway's sandboxes/ folder (null for a visit that never got
-- one, and for visits made before this column); repo_sha is the run repository commit the
-- visit's checkout ended at (the commit it started from when nothing changed or it never
-- had a checkout).
ALTER TABLE run_nodes ADD COLUMN workspace_dir TEXT;
ALTER TABLE run_nodes ADD COLUMN repo_sha TEXT;
