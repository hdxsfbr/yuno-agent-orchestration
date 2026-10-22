# Notes from builder W4a (branch w4-versions): workflow versions, restores, schedules

Scope: DESIGN wave 4 rules 1 to 4 and rule 6. Rule 5 (audit revert), rule 7 (chat
acceptance) and rule 8 (parity re-check) are other builders' work. Code comments cite these
rules as "DESIGN W4 rule N"; renumber them when wave 4 becomes a DESIGN.md section.

## Doc text to fold in

### DESIGN.md (wave 4 section)

- **Workflow versions (rule 1).** Migration 0031 adds `workflow_versions(workflow_id,
  version, name, description, definition, created_at, created_by)`, primary key
  `(workflow_id, version)`, cascading from `workflows`. `store.CreateWorkflow`,
  `store.UpdateWorkflow` and `store.UpdateWorkflowIfVersion` write the workflow row and
  its version in one statement (a data-modifying CTE), so every caller writes a version:
  the API's create, update and restore; the template loader; template instantiation; the
  seed's upgrade and reset; and the engine's quick-run workflow. `created_by` is the
  request's actor (`internal/actor`), `{"kind":"platform"}` when the context carries none
  (the seed, the template loader, quick-run workflows), and `{"kind":"backfill"}` for the
  backfill.
- **Backfill.** Each workflow's current version (live or soft-deleted), from the row, with
  `created_at = updated_at`. Then, for every `(workflow_id, workflow_version)` a run holds
  and the table does not have, one snapshot: `workflow_snapshot` without
  `run_overrides`; when the runs of that version hold different snapshots, the one most
  runs hold, the earliest on a tie; `created_at` is the first run that held it. Name and
  description are the workflow's current ones (runs keep neither). Versions no run held
  and that are not current are lost: the list can have gaps.
- **Reads (rule 2).** `GET /api/workflows/{id}/versions` (newest first, no definitions)
  and `GET /api/workflows/{id}/versions/{version}`. 404 for an unknown or deleted
  workflow, and for a version that is not a positive integer or that the workflow lacks.
  Template rows and quick-run workflows are readable.
- **Restore (rule 3).** `POST /api/workflows/{id}/versions/{version}/restore
  {expected_version}`: 404 workflow or version; 409 `read_only` for platform-managed rows
  (as PUT); 422 when `expected_version` is missing; 412 `version_conflict`
  `{current_version}` when it is not the current version, checked again in the writing
  statement (`version = expected` in its WHERE); 422 with the PUT's details when the
  version is not valid now; otherwise the version's name, description and definition are
  written as version current + 1 and the workflow is returned (200). Runs keep their
  snapshot.
- **Agent restore (rule 4).** `POST /api/agents/{id}/restore`: clears `deleted_at` in one
  statement guarded by "no live agent has this name" (trimmed, case-insensitive), then
  materialises; 404 unknown; 409 `not_archived` for a live agent; 409 `name_taken` with
  `details {agent_id, agent_name}` of the live holder; 502 `materialize_failed` leaves
  the agent restored (create's rule). A Telegram binding released by the delete is not
  restored. `GET /api/agents?include_archived=true` lists archived agents too, each with
  `archived_at`; a value other than true/false is 422 `include_archived`.
- **Schedules (rule 6).** Unchanged code; `TestScheduleEnabledTimezoneNextRun` confirms
  pause and resume through `enabled`, the zone (`tz`) and `next_run_at` through the HTTP
  API, each change read back with GET and the list.

### CHANGELOG

- Workflows keep every version: list them, read one, and restore one as a new version
  (`expected_version` guards against a concurrent change). Versions from before this
  release are recovered from the current workflows and from the runs' snapshots.
- An archived agent can be restored (`POST /api/agents/{id}/restore`); the agents list
  shows archived agents with `include_archived=true`.

### Threat model

- Agent restore grants the agent's tools again; the operation's `x-yuno-risk` is
  `privileged`, so the Chief needs Andre's approval code for it.

## Rules I had to interpret

1. **Snapshots that disagree (rule 1 backfill).** The primary key allows one row per
   `(workflow_id, version)`, but the runs of one version can hold different snapshots (a
   snapshot carries that run's resolved limits and overrides). I keep one: the current
   row's definition for the current version; otherwise the snapshot most runs hold,
   earliest on a tie, with `run_overrides` removed. A recovered older version has
   resolved limits and lacks keys the engine does not read (the builder's layout), and
   carries today's name and description. Disclosed in the contract.
2. **Soft-deleted workflows** are backfilled too (their runs still point at them); their
   version reads answer 404 like the workflow.
3. **`created_by` without an actor.** The seed, the template loader and the engine's
   quick-run workflow have no request actor; they record `{"kind":"platform"}`. On this
   branch the HTTP handlers also record `platform`, because the actor middleware (wave 1
   B) is not here yet; once it merges, API writes record the operator or agent actor.
4. **What a restore restores (rule 3).** The rule says "writes the definition"; I restore
   the version's name and description with it, so the workflow matches the version read.
5. **`expected_version` is required**, 422 when absent; 412 is checked before validation
   (a stale caller learns that first). Restoring the current version is allowed and
   writes an identical new version.
6. **Path parameter name.** `{version}` in the routes and the contract, not `{v}`, so a
   `platform_call` names it plainly (`path_params: {id, version}`).
7. **Platform-managed workflows** (templates, quick-run wrappers) refuse a restore with
   409 `read_only`, as PUT does.
8. **Name uniqueness (rule 4).** Agents have no unique name today; restore compares names
   trimmed and ignoring case (as the seed and templates resolve agents). Create and update
   still allow duplicates, and two concurrent restores of same-named archived agents can
   both succeed (no lock); disclosed.
9. **A restore of a live agent** answers 409 `not_archived` rather than a silent 200.
10. **Risk of agent restore.** Per the brief, the operation's `x-yuno-risk` is
    `privileged` only. Rule 4's "routine when the allowlist is empty" is a per-call
    refinement for the rule A5 gate (another builder); until it exists, every restore
    through the Chief needs approval, the safe direction.
11. **Schedule "timezone"** is the existing `tz` field; I did not rename it. Rule 6 says
    "through `platform_call`", which is not on this branch: the test goes through the HTTP
    API, and rule 7's chat acceptance covers the tool path.

## For the other builders and the coordinator

- PARITY.md row 84 names `TestWorkflowRollback`; DESIGN wave 4 names
  `TestWorkflowRestore`, which is what exists. Rule 8's re-check should update the row,
  and add rows for `GET /api/workflows/{id}/versions/{version}` and
  `POST /api/agents/{id}/restore`.
- Audit target (wave 1 B rule 2): for `POST /api/workflows/{id}/versions/{version}/restore`
  the "last segment removed" rule resolves to the version, which a restore never changes,
  so before and after would be equal. The workflow (`/api/workflows/{id}`) is the useful
  target; the response body already is the workflow.
- The UI for versions, restore and archived agents is out of scope for this wave.
