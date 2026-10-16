-- Chief of Staff (DESIGN 5.15): chat-time platform tool calls, delegated runs that
-- report back into the conversation they came from, and seeded workflows.

-- The identity a chat-time tool call carries (rule 1). Minted at agent creation; rows
-- that predate this migration are filled by store.Migrate's backfill, so the column
-- stays nullable here and UNIQUE ignores the NULLs until then.
ALTER TABLE agents ADD COLUMN chat_token TEXT UNIQUE;

-- Exactly one report per channel run (rule 9) and per approval it waits for (rule 10).
ALTER TABLE runs ADD COLUMN reported_at TIMESTAMPTZ;
ALTER TABLE approvals ADD COLUMN reported_at TIMESTAMPTZ;
CREATE INDEX runs_unreported_channel_idx ON runs (created_at) WHERE trigger = 'channel' AND reported_at IS NULL;

-- A chat-time tool call has no run or node visit; it belongs to the calling agent's most
-- recent conversation, or to nothing when the agent has none (rule 3).
ALTER TABLE tool_calls
    ALTER COLUMN run_id DROP NOT NULL,
    ALTER COLUMN run_node_id DROP NOT NULL,
    ADD COLUMN conversation_id TEXT REFERENCES conversations (id) ON DELETE SET NULL;
CREATE INDEX tool_calls_conversation_idx ON tool_calls (conversation_id, started_at) WHERE conversation_id IS NOT NULL;

-- The template a workflow was instantiated from (rule 13): the seed instantiates each
-- template once, when no workflow carries its key here. The template rows keep owning
-- template_key, which is unique and marks a workflow as platform-managed.
ALTER TABLE workflows ADD COLUMN from_template TEXT;
CREATE INDEX workflows_from_template_idx ON workflows (from_template) WHERE from_template IS NOT NULL AND deleted_at IS NULL;

-- Copies made with "Create copy" before this migration keep the template's name: they
-- count as instantiated, so the first seed on an upgraded database makes no second copy.
UPDATE workflows w SET from_template = t.template_key
FROM workflows t
WHERE t.template_key IN ('dev_pipeline', 'research_pipeline') AND t.deleted_at IS NULL
  AND w.template_key IS NULL AND w.deleted_at IS NULL AND w.from_template IS NULL
  AND w.name = t.name;
