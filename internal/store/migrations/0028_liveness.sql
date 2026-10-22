-- DESIGN 5.12, liveness from evidence (D-099): what a live agent visit is doing, written by
-- its visit goroutine, so a diagnosis rests on timestamps rather than on the "running" status.
--   heartbeat_at      the worker's tick: at the visit's start, then every 30 s with the run's
--                     updated_at (TouchRunNode);
--   progress_at       the turn's last sign of life (a chat or agent event of the turn, a new
--                     transcript entry), written when it changes, at most once per 30 s;
--   waiting_on        model, tool, human, setup or '' (nothing the visit can name), written
--                     when it changes;
--   model_wait_since  when waiting_on became model; null otherwise;
--   open_calls        the turn's tool calls with no result yet, [{id, name, started_at,
--                     summary}], summary the redacted first 200 characters of the command or
--                     path.
-- Rows no visit goroutine owns (fork, join and approval node visits) and rows written before
-- this migration keep the defaults: no heartbeat, which never reads as executing.
ALTER TABLE run_nodes
    ADD COLUMN heartbeat_at     TIMESTAMPTZ,
    ADD COLUMN progress_at      TIMESTAMPTZ,
    ADD COLUMN waiting_on       TEXT NOT NULL DEFAULT ''
        CONSTRAINT run_nodes_waiting_on_check CHECK (waiting_on IN ('', 'model', 'tool', 'human', 'setup')),
    ADD COLUMN model_wait_since TIMESTAMPTZ,
    ADD COLUMN open_calls       JSONB NOT NULL DEFAULT '[]';
