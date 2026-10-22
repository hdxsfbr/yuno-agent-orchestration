# Notes: wave 3, part B (dependencies: edges, satisfied, ticket gate, watcher)

Branch `w3-gate`. Built: DESIGN wave 3 rules 1 to 6 and 8, and the dependency state on the
merge request read. Not built: rule 7 (runs parked on a dependent merge request), which waits
for the run status `paused` with park reasons.

## For rule 7

- `(*deps.Gate).PrerequisitesSatisfied(ctx, mrID) (bool, reason string, error)` judges every
  edge that holds the request (its own and its plan ticket's), records the judgement, and
  returns the first unsatisfied reason in rule 5's words. `(*mergereq.Sync).Gate()` builds
  the gate over the sync's resolver and client. It asks the provider only for prerequisites
  that are merged and not yet satisfied, so it is cheap for the common cases.
- A request is "held" while any of its edges has `cleared_at` null
  (`store.MergeRequestDependencies`, `deps.State.Open`). Prerequisites can all be satisfied
  while the request is still held (the update from the default branch and CI on the new head
  come first); `GET /api/merge-requests/{id}` `blocked_by` then carries the watcher's reason
  (`dependency_gates.reason`). Rule 7 should park while the request is held, not only while a
  prerequisite is unsatisfied; the reason to show is `blocked_by[0].reason`.
- The re-queue hook is `mergereq.Sync.RequeueDependencyRuns func(ctx, rec, note) error`,
  called once from `letGo` (internal/mergereq/dependencies.go, `requeueDependencyRuns`) after
  the edges were cleared in a transaction that holds the record's row. Nothing sets it in
  `internal/httpapi/server.go` yet; rule 7 sets it there. The note is `mergereq.ResumeNote`:
  "Its prerequisite !17 is merged; the platform merged main into this branch (abc1234) and CI
  passed."
- Event `merge_request.unblocked` `{merge_request, note}` is emitted under the request's run
  when it is let go.

## Doc text to fold in

**DESIGN (dependencies).** An edge `dependencies(dependent, prerequisite, source, created_by)`
makes its dependent, a plan ticket (`<plan id>/<ticket id>`) or a merge request record, wait
for its prerequisite. The Planner sets ticket edges through `depends_on` in the slicer's
tickets (same or an earlier wave, no cycle); the operator and the Chief set merge request
edges with `PUT /api/merge-requests/{id}/dependencies`. A merge request is held by its own
edges and by the Planner's edges of the ticket it was built for.

The dependency watcher runs at the end of every merge request sync pass, and right after a
manual sync that changed a prerequisite's state or CI. It judges each held open request:

- each prerequisite not yet satisfied is judged and the judgement kept on the edge
  (`satisfied_at`, `reason`, `evaluated_at`); a satisfied prerequisite stays satisfied;
- once all are, the request is updated from the default branch with its recorded head as the
  expected head; the head it found up to date or pushed is kept (`dependency_gates`), so the
  update is not repeated while the head stays;
- the request is let go when CI passed on exactly that head: its edges get `cleared_at`, the
  re-queue hook runs, and `merge_request.unblocked` is emitted;
- a failed CI ("CI failed on <sha7>: <url>"), conflicts ("conflicts with <default> in
  <files>") or a prerequisite closed without merging keep it held, and the operator is told
  once per commit, conflict or prerequisite, in the conversation the request's run reports
  to ("[platform] Run <id>: merge request !24 is blocked: <reason>.").

After the requests, the wave watcher releases the held tickets whose prerequisites are
satisfied (rule 8b). The wave watcher's own pass does the same.

A released wave's ticket with an unsatisfied prerequisite stays `pending` and unlabelled; a
ticket released with its prerequisites satisfied has its edges cleared, so they never hold
the merge request it opens later. A ticket whose prerequisite was closed without merging is
escalated (failed, waves paused, the operator told).

**API.** `GET /api/merge-requests/{id}/dependencies` (read) and `PUT` with `{prerequisites:
[merge request id]}` (routine), answering `{merge_request_id, prerequisites: [{id,
prerequisite, kind, label, source, editable, created_by, created_at, satisfied, satisfied_at,
reason, cleared_at}], blocked_by: [{prerequisite, kind, label, reason}]}`. 409
`dependency_cycle` (details `{path}`), 422 for a self-edge, a request of another project or no
list. `GET /api/merge-requests/{id}` gains `blocked_by` (absent when nothing holds the request).

**CHANGELOG.**
- The Planner can say a ticket depends on another (`depends_on`); a dependent ticket is
  released for intake only once its prerequisite merged and the default branch passed on the
  merge commit.
- Merge requests can depend on other merge requests (`/api/merge-requests/{id}/dependencies`).
  The platform updates a dependent from the default branch once its prerequisites are in and
  lets it go when CI passes on the updated head.
- A merge request read says what holds it (`blocked_by`).

**Threat model.** The watcher pushes to a dependent's branch with the project's token, never
forced, only with the record's head as the expected head (rules 9 and 10). The PUT accepts
only merge requests of the dependent's own project.

## Rules I had to interpret

1. **Ticket ids.** A ticket end is stored as `<plan id>/<ticket id>`: ticket ids are unique
   only within a plan. No foreign keys on the ends (they are of two kinds); the project's
   foreign key removes the edges with the project. A prerequisite record that no longer
   exists is unsatisfied, "prerequisite <id> no longer exists".
2. **Judgement kept on the edge.** Rule 1's table gains `satisfied_at`, `reason`,
   `evaluated_at` and `cleared_at`, and a table `dependency_gates(merge_request_id, head_sha,
   outcome, reason, notice)` keeps the watcher's progress on a request. The read routes show
   this record and ask no provider. An edge not judged yet reads "<label> has not been
   checked yet".
3. **Satisfied is sticky.** Once `satisfied_at` is set the prerequisite is not judged again;
   a later failure of the same merge commit does not hold anyone again.
4. **A merge request's prerequisites include its ticket's.** The Planner's edges of the
   ticket the request was built for (`plan_tickets.merge_request_id`) hold the request too, as
   rule 3 lists them on the request. Edges cleared when the ticket was released hold nothing.
5. **Default-branch CI of the merge commit** is the new `RequestProvider.CommitCI(ref, sha)`:
   GitLab `pipelines?sha=&ref=<default>` (the variant of `headPipeline` the w3a note asked
   for), GitHub the commit's check runs and combined status (a GitHub check has no ref).
   GitHub cannot tell running from pending, so its reasons say pending.
6. **The no-CI window** counts from the record's `closed_at` (the provider's `merged_at`);
   inside 15 minutes no provider is asked whether CI is configured; at 15 minutes or more,
   `CIConfigured(default)` false satisfies with the note "no CI configured" (kept as the edge's
   `reason`).
7. **CI none on the dependent's updated head** lets it go only when the project has no CI
   configured (the merge guard's test, live); the note then says "the project has no CI
   configured" instead of "CI passed".
8. **Up to date.** A dependent whose branch already contained the default branch waits for CI
   on its current head; the note says "this branch already contains main (<sha7>)".
9. **Re-updating.** The watcher updates again only when the record's head differs from the
   head it last brought up to date (a push from outside), or after conflicts (each pass tries
   again; the operator is told once per conflict: default commit and files). Head moved before
   the push: logged, the next pass starts from the head the sync read.
10. **Once per SHA.** The operator notice key is kept in `dependency_gates.notice`; a send the
    gateway deferred is tried again on the next pass; a run that reports to no conversation
    sends nothing (the reason is on the record read). Message keys are
    `report-<run id>-mr-dependency-<hash of the notice>`, so the conversation mirror links them
    to the run.
11. **Let go at most once.** The edges are cleared and the gate removed in one transaction that
    locks the record's row, before the hook runs. A process that stops between the commit and
    the hook loses that re-queue (rule 7 may want a sweep of `paused` runs whose request is no
    longer held).
12. **Provider failures** during a watcher pass are written to the dependent's
    `last_sync_error` as "dependency check: <text>" (redacted); its `synced_at` moves with it,
    as any recorded sync failure does.
13. **Ticket gate.** A held ticket stays `pending` (not `released`) and unlabelled. "Held" is
    judged live by the wave watcher before every release (wave 1 after the plan merge, a wave
    held for a revision, the next wave in `advance_wave`); a ticket whose judgement fails waits
    for the next pass. A revision's re-release, which runs inside the tool and asks no provider,
    holds a ticket with any edge not yet satisfied; the watcher releases it on its next pass.
14. **Rule 8b** is `waves.Service.ReleaseSatisfied`, called by the merge request sync after
    its dependency pass and by each wave watcher pass for the plan's current, released wave,
    with the plan lock, the pause and the revision read again as a wave release does.
15. **Closed ticket prerequisite.** For a ticket dependent the operator is told by escalating
    the dependent ticket ("prerequisite T9 was closed without merging"): it fails and the
    project's waves pause, which is how a plan ticket that cannot proceed is reported today.
    For a merge request dependent it is the one-time notice of rule 12's kind.
16. **Planner cycles** are refused in `publish_tickets` as "the dependencies would make a
    cycle: T1 -> T2 -> T1". Cycles that pass through a ticket and its merge request (a ticket
    edge one way, a merge request edge the other) are not detected; the two dependents then
    wait on each other, and their reasons say so.
17. **`depends_on` is not in the issue text** and not in the ticket's content hash, so adding
    it to an existing plan does not rewrite issues.
18. **PUT on a request that is not open** is accepted (the edges are history); nothing is
    judged or resumed for it, and `blocked_by` is empty.
19. **Chief vs operator source**: an agent actor (the Chief's `platform_call`) sets `chief`,
    anyone else `operator`; `created_by` is the actor.

## Tests

- `internal/tools`: TestDependencyEdges (Planner edges stored, later wave, self, unknown,
  cycle; merge request edges unique, self, cycle path, replace).
- `internal/deps`: TestPrerequisiteSatisfied (rule 4 clauses both ways, rule 5 reasons, the
  window at 14 and 16 minutes, ticket prerequisites, sticky satisfied, provider failure).
- `internal/waves`: TestTicketNotReleasedUntilSatisfied.
- `internal/mergereq` (fake forge): TestResumeAfterCIOnNewSHA (adapted to the hook),
  TestCIFailedKeepsBlocked, TestDependentConflictsKeepBlocked, TestDependencyAcceptance.
- `internal/httpapi`: TestMergeRequestDependencyRoutes (conformance-checked).
- TestStaleCheckInvalidated is w3a's and unchanged.

## Left open

- Rule 7 and its tests (TestRunOnDependentParks, TestResumeRefusedWhileBlocked).
- The web UI shows neither the prerequisites nor `blocked_by`.
- The waves partition check (`waves_partition`) does not check `depends_on`; publish_tickets
  does.
