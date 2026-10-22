-- Chief platform control, wave 3 (DESIGN rules 1 to 8): dependent work waits for its
-- prerequisites. One row per edge: the dependent (a plan ticket or a merge request record)
-- waits for the prerequisite. A ticket is named <plan id>/<ticket id>, since ticket ids are
-- unique only within their plan. source says who set the edge: the Planner (publish_tickets
-- depends_on), the operator or the Chief (PUT /api/merge-requests/{id}/dependencies).
--
-- The watcher's account of each edge is kept with it: satisfied_at once the prerequisite
-- is satisfied (rule 4; a satisfied prerequisite stays satisfied), reason the latest rule-5
-- reason while it is not (or the note "no CI configured" once it is), evaluated_at the time
-- of that evaluation, and cleared_at once the dependent was let go with this edge satisfied:
-- a ticket labelled for intake, a merge request updated from the default branch with CI
-- passed on its new head. An edge without cleared_at holds its dependent.
CREATE TABLE dependencies (
    id                TEXT PRIMARY KEY,
    project_id        TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    dependent_kind    TEXT NOT NULL CHECK (dependent_kind IN ('ticket', 'merge_request')),
    dependent_id      TEXT NOT NULL,
    prerequisite_kind TEXT NOT NULL CHECK (prerequisite_kind IN ('ticket', 'merge_request')),
    prerequisite_id   TEXT NOT NULL,
    source            TEXT NOT NULL CHECK (source IN ('planner', 'operator', 'chief')),
    created_by        JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    satisfied_at      TIMESTAMPTZ,
    reason            TEXT,
    evaluated_at      TIMESTAMPTZ,
    cleared_at        TIMESTAMPTZ,
    UNIQUE (dependent_kind, dependent_id, prerequisite_kind, prerequisite_id),
    CHECK (dependent_kind <> prerequisite_kind OR dependent_id <> prerequisite_id)
);
CREATE INDEX dependencies_prerequisite_idx ON dependencies (prerequisite_kind, prerequisite_id);
CREATE INDEX dependencies_project_idx ON dependencies (project_id);
CREATE INDEX dependencies_held_idx ON dependencies (dependent_kind, dependent_id) WHERE cleared_at IS NULL;

-- The watcher's progress on a dependent merge request once its prerequisites are all
-- satisfied (rules 8a and 12): head_sha is the head it found up to date with the default
-- branch, or pushed there (outcome up_to_date or updated); reason why the request still
-- waits (conflicts, CI on that head); notice the last thing the operator was told, so each
-- failed commit and each conflict is told once. The row goes when the request is let go or
-- its edges change.
CREATE TABLE dependency_gates (
    merge_request_id TEXT PRIMARY KEY REFERENCES merge_requests (id) ON DELETE CASCADE,
    head_sha         TEXT,
    outcome          TEXT,
    reason           TEXT,
    notice           TEXT,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
