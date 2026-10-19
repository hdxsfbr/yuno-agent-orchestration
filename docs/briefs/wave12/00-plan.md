# Wave 12: agent files, skill bundles, skills library (DESIGN 5.27)

Agents: Sonnet 5.5, one per brief, disjoint files. Each runs its package tests in a
throwaway container on its own test database (yuno_test_w12p, _a, _b, _c), never `make`.

P. Prerequisites (first, alone): migration `0016_wave12.sql` (`agent_file_edits(agent_id,
name, addition, override, updated_at)`; `skills` gains `source`, `source_url`,
`source_ref`, `license`; `skill_files(skill_id, path, content bytea, mode, size)` with
the unique (skill_id, path)); store types and repositories; `api/openapi.yaml` 0.9.0 with
the new paths marked `x-planned: wave 12`.

A. Agent files (backend): `internal/openclaw` materialize composes additions and
overrides (rules 2 to 4), the list reader (rule 1, `os.Root`, redaction, truncation),
`internal/httpapi` routes for rule 1 to 4, tests T5.27.1 to T5.27.4.

B. Skill bundles (backend): zip import, Git import, caps and path checks, materialize of
bundle files with modes and the stale sweep (rules 5 to 9, 11), the library importer
(rule 10), `SKILL_IMPORT_HOSTS` in config and `.env.example`, tests T5.27.5 to T5.27.11.

C. Frontend: an agent page "Files" tab (list, viewer, editor for the addition or the
override, reset); the Skills page gains source and licence badges, a library filter,
"Upload zip or folder", "Import from Git", and a bundle file tree with a text editor,
executable marks, delete and export.

D. Library (coordinator, after Andre's permission to download): vendor the skills named in
D-069 into `skills-library/`, read each in full, write `skills-library/SOURCES.md` with
source, commit and licence per skill, and copy the directory into the api image.

Then: full suite and lint under the CI runner's conditions, one independent review, one
fix pass, one commit, push, verify live.
