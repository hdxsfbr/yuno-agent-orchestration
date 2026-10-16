-- One run per schedule occurrence (DESIGN 5.9 rule 2). The tick triggers with
-- trigger_ref "<schedule id>@<occurrence>"; the run is inserted outside the schedule's
-- transaction, so when the schedule update is lost after the run was created the next
-- tick fires the same occurrence again and this index refuses the second run.

-- Runs fired before this migration carry the bare schedule id once per occurrence. They
-- keep it as the prefix and gain their own run id, which makes them unique.
UPDATE runs SET trigger_ref = trigger_ref || '@' || id
WHERE trigger = 'schedule' AND trigger_ref IS NOT NULL AND position('@' IN trigger_ref) = 0;

CREATE UNIQUE INDEX runs_schedule_occurrence_idx ON runs (trigger, trigger_ref) WHERE trigger = 'schedule';
