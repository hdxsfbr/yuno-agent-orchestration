# Wave 12 review (DESIGN 5.27): agent files, skill bundles, skills library

One independent reviewer (Claude Opus 5.5), read-only, over the uncommitted wave against
DESIGN 5.27, 2026-10-19. Twelve findings, none high; every one is kept for the fix pass.

| # | Severity | Finding | Disposition |
|---|---|---|---|
| 1 | medium | A named pipe the agent creates in its workspace blocks materialize (read before Lstat) and the files list | fix: Lstat through the root, read regular files only |
| 2 | low | Redaction before truncation can expose the start of a secret straddling the read limit | fix: drop the tail of the raw buffer when the limit was hit |
| 3 | medium | A file replaced by a folder (or the reverse) in a bundle fails every materialize | fix: prune stale paths before writing; clear a conflicting path |
| 4 | medium | Hidden folders (`.github/`, `.git/`) refuse a whole import | fix: skip hidden folders and their zip entries |
| 5 | medium | Runtime overrides are not written again after OpenClaw rewrites the file | fix: write overrides on every visit and sync (identical content is skipped) |
| 6 | low | The stale sweep deletes files the agent created under an assigned skill's folder | fix: a manifest of the platform's paths; prune only those |
| 7 | low | A library skill is skipped when an unrelated skill has its folder's name | fix: match on the front-matter name |
| 8 | low | The library's added LICENSE bypasses the caps | fix: add before the caps |
| 9 | medium | Import 422s reach the UI as "invalid skill" | fix: the field reasons in the message |
| 10 | medium | The import dialogs treat a replace's 502 materialize_failed as a failure | fix: saved, as the files dialog does |
| 11 | medium | Saving a runtime file seeded from redacted content writes the marker | fix: `redacted: true` flag; the editor refuses to seed from it |
| 12 | medium | Every node visit and sync reads all bundle bytes to hash them | fix: a sha256 column; content loaded only to write |
| — | test | An engine test names its skill with a dot the API refuses | fix the name |
