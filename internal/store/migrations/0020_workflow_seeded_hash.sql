-- The sha256 of the definition the seed last wrote to a shipped workflow copy, so a later
-- seed can tell an untouched copy (upgrade it to the template) from an edited one (keep it).
ALTER TABLE workflows ADD COLUMN seeded_hash TEXT NULL;
