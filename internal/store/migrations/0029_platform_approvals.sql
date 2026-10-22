-- Chief platform control, wave 1 A6 (D-097): a platform approval gates one agent's
-- platform_call of a destructive or privileged operation. It belongs to no run: run_id and
-- run_node_id stay NULL (nullable since 0006, restated here for this kind) and node_id is ''.
-- binding is the exact call it allows: {agent_id, operation_id, path, body_sha256,
-- idempotency_key}; NULL for every other kind. used_at is when a repeat of that call spent
-- it: approved and used means the call ran; expired and used means a repeat was told it
-- expired, so the next one asks again.
ALTER TABLE approvals
    ALTER COLUMN run_id DROP NOT NULL,
    ALTER COLUMN run_node_id DROP NOT NULL,
    ADD COLUMN binding JSONB,
    ADD COLUMN used_at TIMESTAMPTZ;

-- A repeat finds its approval by the calling agent and the key, then the rest of the binding.
CREATE INDEX approvals_platform_binding_idx ON approvals ((binding->>'agent_id'), (binding->>'idempotency_key'))
    WHERE kind = 'platform';
