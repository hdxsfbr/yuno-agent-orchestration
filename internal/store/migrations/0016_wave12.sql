-- Wave 12 (DESIGN 5.27): agent files, skill bundles and the skills library. Two tables and
-- four columns; every existing skill reads as a user skill with no bundle files and every
-- agent has no stored edits.

-- 5.27 rules 2 and 3: the operator's text for one workspace file of an agent. addition is
-- written after a generated file's platform part; override replaces a runtime file. A row
-- with neither is deleted by the repository.
CREATE TABLE agent_file_edits (
    agent_id   TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    addition   TEXT,
    override   TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, name)
);

-- 5.27 rule 10: where a skill came from. user is today's behaviour; library is imported from
-- skills-library/; git and upload are imports (rules 6 and 7).
ALTER TABLE skills ADD COLUMN source TEXT NOT NULL DEFAULT 'user'
    CHECK (source IN ('user', 'library', 'git', 'upload'));
ALTER TABLE skills ADD COLUMN source_url TEXT NOT NULL DEFAULT '';
ALTER TABLE skills ADD COLUMN source_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE skills ADD COLUMN license TEXT NOT NULL DEFAULT '';

-- 5.27 rule 5: a skill's files besides SKILL.md (the body). mode is 0644 (420) or 0755 (493).
-- sha256 is the digest of content, computed on every write, so the materialise hash reads
-- path, size, mode and digest without loading the bytes.
CREATE TABLE skill_files (
    skill_id TEXT NOT NULL REFERENCES skills (id) ON DELETE CASCADE,
    path     TEXT NOT NULL,
    content  BYTEA NOT NULL,
    mode     INT NOT NULL CHECK (mode IN (420, 493)),
    size     INT NOT NULL,
    sha256   BYTEA NOT NULL,
    PRIMARY KEY (skill_id, path)
);
