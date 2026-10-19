-- Delivery process wave 3 (DESIGN 5.29 rules 11 to 14): a wave's ship run is unique per plan
-- and wave, and the plan keeps the watcher's last read of the default branch.

-- A ship run's trigger_ref is "plan:<plan id>:wave:<n>" (rule 12); a second run for the same
-- plan and wave is refused whatever its status, so a retry is the run's own retry.
CREATE UNIQUE INDEX runs_plan_wave_idx ON runs (trigger, trigger_ref) WHERE trigger = 'plan';

-- The default branch's status as the watcher last read it (rule 11); null until the first read.
ALTER TABLE plans ADD COLUMN main_state TEXT;
ALTER TABLE plans ADD COLUMN main_sha   TEXT;
ALTER TABLE plans ADD COLUMN main_at    TIMESTAMPTZ;
ALTER TABLE plans ADD COLUMN main_url   TEXT;
