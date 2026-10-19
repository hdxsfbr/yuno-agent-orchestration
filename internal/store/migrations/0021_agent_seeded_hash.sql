-- The sha256 of the role, system prompt and tool allowlist the seed last wrote to a shipped
-- agent, so a later seed can tell an untouched agent (upgrade it to the shipped values)
-- from an edited one (keep it). NULL for agents made before this column.
ALTER TABLE agents ADD COLUMN seeded_hash TEXT NULL;
