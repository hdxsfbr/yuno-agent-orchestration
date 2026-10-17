-- Project repository runs (DESIGN 5.16 rule 3): the toolchain detected when the engine
-- cloned the repository into the first agent's run directory; NULL for runs that had no
-- checkout. Its presence also marks the clone as done for a re-queued attempt.
ALTER TABLE runs ADD COLUMN toolchain TEXT;
