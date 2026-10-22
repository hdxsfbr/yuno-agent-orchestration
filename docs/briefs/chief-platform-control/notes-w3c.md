# Notes: wave 3, part C (rule 7: runs on a dependent merge request park and resume)

Branch `w3-park`, from `feat/chief-platform-control` at `692645d` (Merge branch 'w3-gate').
Built: DESIGN wave 3 rule 7, the re-queue of rule 8(a) through the hook w3b left
(`mergereq.Sync.RequeueDependencyRuns`), the diagnosis's `blocked` for such a run, and the
tests rule 7 names. Migration `0034_dependency_park.sql`.

## DESIGN (text to add, dependencies section)

A run that would work on an open merge request its dependencies hold is created `paused`
with `park_reason = dependency` and `park_detail` the reason, and no worker takes it. "Held"
is the merge request read's `blocked_by`: any of the request's edges (its own and its plan
ticket's) not cleared yet; the reason is `blocked_by[0].reason` as the watcher last recorded
it ("!17 is not merged", "!17 merged; CI on <sha7> is pending", "waiting for CI on <sha7>",
"conflicts with main in <files>"). The check is in the engine's one choke point,
`Trigger` (`queueRun`), so every path that names a merge request parks: `POST /api/runs`
with `merge_request_id`, the Chief's `delegate_task` with `merge_request`, `POST
/api/plans/{id}/tickets/{ticket}/changes`, and a schedule's occurrence on a request. The
insert locks the record's row (`FOR KEY SHARE`) before it reads the edges; the watcher's
release holds it `FOR UPDATE`, so either the release comes first and the run is queued, or
the release waits for the insert and then re-queues the run.

While the request is held, a resume (operator or Chief) of a paused run on it and a retry of
an ended run on it are refused, 409 `dependency_blocked`, "blocked by <reason>; remove the
dependency to override", the command recorded `rejected` with that detail. The run's request
is the one it names or, for a run that names none, the one it opened.

When the watcher lets the request go (edges cleared, its gate removed), the server's hook
re-queues every run parked on it for a dependency in one statement: status `queued`, lease
epoch + 1, the park reason and detail cleared, and `runs.resume_note` set to the watcher's
note ("Its prerequisite !17 is merged; the platform merged main into this branch (<sha7>)
and CI passed.", or "this branch already contains main (<sha7>)", or "; the project has no
CI configured."). A `run.status` event and a run log line ("<note> The run is queued.")
follow. The run's steps are told the note after their task until one of them succeeds, as a
resumed step is told its resume.

The diagnosis reports a parked run `blocked`, with what holds the request now as the reason
(read from the record, no provider asked). `delegate_task` and `run_status` answer
`blocked_by` (the reason as it read when the run was parked) for such a run.

## API (to fold in)

- `Run` gains `park_reason` (`dependency`, absent otherwise) and `park_detail` (the reason
  when parked; kept on a run cancelled while it waited).
- `POST /api/runs/{id}/resume` and `POST /api/runs/{id}/retry`: 409 `dependency_blocked`.
- `RunDiagnosis`: a run parked for its dependencies is `blocked`.

## CHANGELOG

- A run on a merge request whose prerequisites are not in yet (requested changes, a
  delegated run, a schedule, a manual run) starts paused and is queued by itself, with a
  note for its first step, once the platform brought the request up to date and CI passed;
  resume and retry are refused meanwhile, naming what holds it.

## DECISIONS (fold into the wave 3 entry)

Rule 7 is enforced at the engine's trigger, not per route; held means "an edge not cleared"
(the watcher's whole gate, update and CI on the new head included), not only "a
prerequisite unsatisfied". The run carries its own park reason, since a run parked before
its first step has no visit to carry one.

## Rules I had to interpret

1. **Where the park reason lives.** Wave 2's `park_reason` is on the visit; a run parked at
   creation has none. Migration 0034 adds `runs.park_reason` ('' | dependency),
   `runs.park_detail` and `runs.resume_note`. This is why a migration was needed: the hook
   must tell a dependency park from an operator pause, and the note needs a home on a run
   with no visit.
2. **The reason text** is the stored one (`blocked_by[0].reason`), never a provider call in
   the trigger. An edge the watcher has not judged yet reads "!17 has not been checked yet".
   `park_detail` is the text at park time; the diagnosis and the refusals read the current
   one.
3. **Retry is refused, not parked.** The brief says refused (409); rule 7's list puts "a
   retry of such a run" among the runs created paused. A retry re-queues an existing run, so
   it is refused like a resume; the operator retries once the request is let go or the
   dependency removed.
4. **Which resumes are refused.** Every resume of a paused run on a held request, not only of
   runs parked for a dependency: a run the operator paused, whose request gained a
   dependency since, does not go on with it either. Only dependency parks are re-queued by
   the watcher; an operator pause stays the operator's to resume.
5. **The opener's retry.** A run that names no request but opened one (its next push goes to
   that branch) is checked against the request it opened.
6. **"Queued with the note" for a run with no visit.** The note is kept on the run
   (`resume_note`) and appended after the task of each step's visit until a step of the run
   succeeds, so a visit re-run after a lost worker is told it too.
7. **Paused time.** A run parked before it started has no `paused_at`; its wait does not
   count in `paused_total_ms` (the time limit starts at its first claim anyway).
8. **Operator resume after the dependency was removed** clears the park reason and detail;
   the note of an operator resume of a run with no visit still goes nowhere (wave 2's
   behaviour, unchanged).
9. **A lost re-queue** (the process stopped between the release's commit and the hook, w3b
   interpretation 11): the run stays parked, the diagnosis says "nothing holds its merge
   request any more; a resume starts it", and the operator's resume is accepted since nothing
   holds the request. No sweep was added.
10. **Diagnosis reason** is the bare reason, the same words as `blocked_by` and
    `park_detail`.
11. **Chief resume** is the same route through `platform_call`; no separate path.

## Tests

- `internal/httpapi`: TestRunOnDependentParks (POST /api/runs, delegate_task's delegator,
  changes requested on a plan ticket, a schedule fired by the scheduler, through the real
  engine; a free request still queues; the diagnosis says blocked with the reason),
  TestResumeRefusedWhileBlocked (resume, retry, retry of the opener; command rows rejected;
  both go through once the dependency is removed), TestDependencyAcceptanceResumesRun (fake
  forge, real engine, the server's sync and hook: parks, !1 merges, main passes, !2 updated
  from main, CI passes on the new head, the run is queued once, epoch 1, with the note; the
  diagnosis follows each stage).
- `internal/engine`: TestRequeuedRunCarriesNote (the note follows the first step's task and
  not the second's).
- PARITY.md: the "(wave 3 gate)" marks dropped (TestDependencyEdges, TestRunOnDependentParks,
  TestDependencyAcceptance exist); TestDependencyAcceptanceResumesRun and
  TestResumeRefusedWhileBlocked added to their rows; the dependencies row now names its
  handlers (getMergeRequestDependencies, putMergeRequestDependencies), which
  TestParityMatrix asked for once the marks were gone.

## Left open

- The web UI shows a dependency park as "paused" with a Resume button that answers the
  refusal; it does not show `park_detail` or hide the button.
- No sweep re-queues a run whose release lost its hook (interpretation 9).
