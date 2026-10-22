-- Chief platform control, DESIGN 5.34 rule 7: a run that would work on a merge request
-- its dependencies hold is created paused, and no worker takes it until the dependency
-- watcher lets the request go and re-queues it (rule 8a). Such a run has no visit yet, so
-- the run row carries why it waits:
--   park_reason  'dependency' for a run parked that way, '' otherwise (a run the operator
--                paused carries its reason on the paused visit, run_nodes.park_reason);
--   park_detail  the reason it waits as it read when the run was parked ("!17 is not
--                merged"); kept on a run cancelled while it waited;
--   resume_note  the note the run's first step is told once the watcher re-queued it ("Its
--                prerequisite !17 is merged; the platform merged main into this branch
--                (<sha7>) and CI passed.").
ALTER TABLE runs
    ADD COLUMN park_reason TEXT NOT NULL DEFAULT ''
        CONSTRAINT runs_park_reason_check CHECK (park_reason IN ('', 'dependency')),
    ADD COLUMN park_detail TEXT,
    ADD COLUMN resume_note TEXT;
