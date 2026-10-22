-- Chief platform control, wave 3 (DESIGN rules 11 and 13): a merge request's check is the
-- check of exactly one commit. ci_sha is the commit ci_status was read for, and the merge
-- guard (rule 15) needs it equal to head_sha; ci_pipeline_id and ci_url name the pipeline
-- or check behind it. mergeable is the provider's account of whether the branch merges
-- cleanly (null while it does not know), merge_status the provider's own word for it.
-- conflicts is what the last update from the default branch met ({files, at, base_sha}),
-- and updated_from_default_at the time of the last such update the platform pushed.
-- Existing records keep ci_sha null until their next sync reads the head's own pipeline.
ALTER TABLE merge_requests
    ADD COLUMN ci_sha                  TEXT,
    ADD COLUMN ci_pipeline_id          BIGINT,
    ADD COLUMN ci_url                  TEXT,
    ADD COLUMN mergeable               BOOLEAN,
    ADD COLUMN merge_status            TEXT,
    ADD COLUMN conflicts               JSONB,
    ADD COLUMN updated_from_default_at TIMESTAMPTZ;
