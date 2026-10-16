-- Telegram on OpenClaw's native channel (DESIGN 5.8): the platform's binding, and the
-- conversations it mirrors from the bound agent's channel sessions.

-- At most one platform agent answers a channel (5.8 rule 3, D-025).
CREATE TABLE channel_bindings (
    id          TEXT PRIMARY KEY,
    channel     TEXT NOT NULL,
    agent_id    TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    external_id TEXT NOT NULL DEFAULT '',   -- the bot's username, as the gateway reports it
    config      JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX channel_bindings_channel_idx ON channel_bindings (channel);

-- One conversation per OpenClaw channel session (5.8 rule 4).
CREATE TABLE conversations (
    id               TEXT PRIMARY KEY,
    channel          TEXT NOT NULL,
    external_chat_id TEXT NOT NULL,
    agent_id         TEXT REFERENCES agents (id) ON DELETE SET NULL,
    session_key      TEXT NOT NULL,
    display_name     TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_message_at  TIMESTAMPTZ
);
CREATE UNIQUE INDEX conversations_session_idx ON conversations (session_key);
CREATE INDEX conversations_recent_idx ON conversations (last_message_at DESC NULLS LAST, created_at DESC);

-- Transcript entries mirrored idempotently by the gateway's message id.
CREATE TABLE conversation_messages (
    id                  TEXT PRIMARY KEY,
    conversation_id     TEXT NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    direction           TEXT NOT NULL,           -- in | out
    external_message_id TEXT NOT NULL,
    text                TEXT NOT NULL DEFAULT '',
    usage               JSONB NOT NULL DEFAULT 'null',
    run_id              TEXT REFERENCES runs (id) ON DELETE SET NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX conversation_messages_external_idx ON conversation_messages (conversation_id, external_message_id);
CREATE INDEX conversation_messages_order_idx ON conversation_messages (conversation_id, created_at, id);

-- An exec approval raised by a conversation turn belongs to the conversation, not to a
-- run (5.8 rule 5).
ALTER TABLE approvals
    ALTER COLUMN run_id DROP NOT NULL,
    ALTER COLUMN run_node_id DROP NOT NULL,
    ADD COLUMN conversation_id TEXT REFERENCES conversations (id) ON DELETE CASCADE;
CREATE INDEX approvals_conversation_idx ON approvals (conversation_id) WHERE conversation_id IS NOT NULL;
