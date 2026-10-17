-- A schedule names the project its runs work on (DESIGN 5.9 rule 6): required for a
-- workflow whose definition has project: true, refused otherwise. The tick passes it
-- to the engine, which snapshots the project onto the run like a manual request.
ALTER TABLE schedules ADD COLUMN project_id TEXT REFERENCES projects (id);
CREATE INDEX schedules_project_idx ON schedules (project_id) WHERE project_id IS NOT NULL;
