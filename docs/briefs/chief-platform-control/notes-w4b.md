# Notes from builder W4b (branch w4-revert): audit revert, chat acceptance, parity re-check

Scope: DESIGN wave 4 rules 5, 7 and 8, and part A's open items (TestPlatformCallActor's audit
row; A's tests on the real chain). Code comments cite the rules as "DESIGN W4 rule N", as
W4a did. No migration.

## Doc text to fold in

### DESIGN.md (wave 4 section)

- **Audit revert (rule 5).** `POST /api/audit/{id}/revert` (`revertAudit`, no body, risk
  `routine` in the contract):
  - 404 `not_found` for an unknown audit id.
  - 422 `not_revertible` unless the row is a PUT with outcome `ok` on `/api/agents/{id}`,
    `/api/schedules/{id}`, `/api/skills/{id}` or `/api/projects/{id}`; a workflow PUT is
    pointed at the version restore.
  - Mappers take the input fields of the resource's PUT from the row's `before`: agent
    (name, role, system_prompt, model, effort, thinking, max_tokens, tool_allowlist,
    interaction_rules, limits, guardrails, memory_policy, exec_policy, skills); schedule
    (workflow_id, project_id, merge_request_id, kind, cron_expr, tz, run_at, enabled, input,
    with "" for an absent project or merge request so the PUT clears them); skill (name,
    description, body); project (name, repo_url, provider, default_branch, sandbox_network,
    description, instructions, both budget caps, intake), never a token.
  - 422 `nothing_to_revert` when `before` is missing (null, the audit's read error, or a
    document cut to the 64 KiB cap), or when the mapped `before` equals the mapped `after`
    (a change of the token alone, or of read-only fields).
  - 422 `not_revertible` when the mapped `before` holds the redaction marker: the value it
    stands for is not in the audit log, and writing the marker would destroy it.
  - 412 `version_conflict` "changed again since; revert the later change first" (details
    `current_etag`) when the resource as it is now, mapped the same way, differs from the
    mapped `after`, or is gone.
  - Otherwise the mapped `before` goes through the resource's own PUT handler behind the
    If-Match layer, with `If-Match` the current ETag just compared; a change in between
    answers the same 412. The answer is the PUT's (the resource, its 422, its 502).
  - The revert's own audit row targets the reverted resource and reads it before and after.
  - The platform gate holds an agent's revert to the change it writes back: rule A5 on the
    PUT of the target with the mapped `before` as its body, against the stored record, so it
    is privileged when it gives tools back, loosens interaction rules, guardrails or the exec
    policy, changes the calling agent itself, changes a project's repository or provider, or
    raises a budget cap. A revert the handler refuses raises nothing. The approval message
    adds "It writes back to <target>: <body>", and its "Now:" is the resource.
- **Chat acceptance (rule 7).** TestChiefAgentCRUD, TestChiefWorkflowCRUD and
  TestChiefScheduleCRUD drive `platform_call` through the MCP server, the tools, the
  dispatcher, the whole middleware chain and the handlers against the database, each change
  read back with a GET; each delete is the approval round trip (pending, approved by the
  person's code, run once, replayed). TestChiefAgentCRUD has the privileged grant refused
  (rejected) and audited `denied` for the agent on the person's behalf; TestChiefWorkflowCRUD
  the restore of version 1 while two runs are in flight on versions 1 and 2, which keep
  their snapshots; TestAuditRevertByChief the revert of an agent's prompt;
  TestUnauthorizedAudited the anonymous 401s and their rows.
- **Parity (rule 8).** PARITY.md names every operation's handler in its Backend column and
  only tests that exist; TestParityMatrix checks both, and that a test marked "(wave 2)" or
  "(wave 3 gate)" does not exist yet. TestPlatformParity names the routes waves 1, 3 and 4
  added.

### CHANGELOG

- A change to an agent, a schedule, a skill or a project can be undone from the audit log
  (`POST /api/audit/{id}/revert`); tokens are never reverted, and a resource changed again
  since asks for the later change to be reverted first. The Chief can revert too; a revert
  that gives tools back, loosens rules, changes the Chief itself or raises a budget waits for
  your approval.

### Threat model

- The revert writes back a recorded state through the same PUT and the same gate as a direct
  change, so it grants nothing the change itself would not; the gate judges it on what it
  writes back. Values the audit log redacts are never written back, and tokens are never
  reverted.

## Rules I had to interpret

1. **"If-Match = the ETag of the row's after."** The row's `after` is redacted and stored as
   jsonb (keys reordered, numbers normalised), so its bytes never hash to a live ETag. The
   revert compares the row's `after` with the resource as it is now, both read as the audit
   reads them and mapped by the resource's mapper, so the read-only fields that move on their
   own (a schedule's next and last run, a project's spend, an agent's materialised hash) do
   not refuse a revert; then it sends the PUT with `If-Match` = the current ETag it compared,
   so the If-Match layer refuses anything that changed in between.
2. **A resource deleted since** answers 412, like B4's If-Match on a missing target (notes-b
   rule 13); for an agent the later change to undo is the archive (restore it first).
3. **"Before is missing"** includes the audit's `{"error": ...}` and a `before` cut to the
   64 KiB cap; both answer 422 `nothing_to_revert`. A missing `after` answers 422
   `not_revertible` (a later change cannot be ruled out).
4. **"Nothing to revert"** is general: the mapped `before` equals the mapped `after`, which
   covers the token-only change and a PUT that changed only read-only fields.
5. **Redacted values.** A `before` holding the redaction marker (a credential-shaped string in
   a prompt, a secret-keyed value in a schedule's input) is refused 422 `not_revertible`
   rather than writing the marker over the real value.
6. **The revert's audit target** is the reverted resource (looked up from the audit row in
   `routeTarget`), so the revert's own row has before and after and the gate's approval shows
   the resource as it is now. A revert row is a POST and is not itself revertible; the undo
   of a revert is another PUT.
7. **Gate wiring.** `platformRisk` evaluates `revertAudit` as the PUT of the target with the
   mapped `before` (rule A5 including part A's exec-policy reading), against the stored
   record; when that is gated, its risk and reasons are the revert's. The approval binds the
   revert call (path, empty body, key); the body it writes back cannot change, because the
   audit row does not.
8. **Error codes.** `not_revertible` and `nothing_to_revert` are new 422 codes;
   `version_conflict` is B4's.
9. **Chat acceptance path.** The CRUD tests call `platform_call` through the MCP server
   (`chatFixture.call`), which dispatches with `platformDispatcher{s}` into `Handler()`: the
   real chain with nothing replaced. "Executes once" is shown by the audit (one `ok`, one
   `replayed` row) and, for the agent, by one removal from OpenClaw.
10. **"A privileged change without approval refused and audited denied."** The gate answers a
    first privileged call `pending_approval`; the refusal is the repeat after the person
    rejects it (403 `approval_rejected`, outcome `denied`).
11. **Rule 6 through platform_call.** TestChiefScheduleCRUD reads back the zone and
    `next_run_at` with GET; the list's `next_run_at` stays with TestScheduleEnabledTimezoneNextRun
    (over HTTP), because through `platform_call` a list in the shared test database passes
    the 32 KiB result cap and comes back as text.
12. **"Cyclic is refused" (PARITY row 25).** Workflows allow loops, so the row now reads
    "an invalid definition is refused" (an unknown agent), which is what validation refuses.
13. **PARITY marks.** A name followed by "(wave 2)" or "(wave 3 gate)" inside its own
    comma or semicolon item is skipped; such a name that already exists fails the test, so
    the mark is dropped when the wave lands. Wave 2 and wave 3 names are DESIGN's
    (TestCommandLifecycle, TestCancelLiveConfirmedByEvent, TestRetryFromStep,
    TestRetryRefusedUnconfirmedCancel, TestPauseResume, TestSteerLive; TestDependencyEdges,
    TestRunOnDependentParks, TestDependencyAcceptance), replacing PARITY's
    TestCancelAcknowledged, TestSteerDelivered and TestDependencyGate.
14. **Backend column.** Every row names its handlers by operationId (rows that abbreviated
    them, such as the skill reads, now list them), so the matrix is checked against the
    contract mechanically.
15. **Part A's fakes.** `fakeIdempotency`, `fakeVersions` and `fakeETag` are gone: the
    platform fixture dispatches into `withMiddleware` around a handler counter at the routes'
    place, so the real audit, gate, idempotency and If-Match run. TestDestructiveNeedsApproval
    also checks the audit outcomes (three pending, one ok naming the approval, one replayed).

## Left open

- Agent restore's per-call risk (DESIGN W4 rule 4: routine when the allowlist is empty) is
  still the contract's `privileged` for every restore (W4a rule 10); the safe direction.
- "Recreate" of a deleted workflow, schedule or skill from its audit `before` (DESIGN W4 edge
  cases) is not built; the revert refuses a DELETE row.
- No UI for the revert: there is no audit page in the web app yet.
- The Chief's prompt does not mention the revert; `platform_operations` finds it under the
  `ops` tag. A prompt change would need `make eval`.
