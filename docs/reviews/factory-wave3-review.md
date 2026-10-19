# Delivery process, wave 3 review (DESIGN 5.29 rules 10 to 15 and 17)

One independent reviewer (Claude Opus 5.5), read-only, 2026-10-19. Two high, three medium,
eight low. Evidence: `go test -count=1` of `internal/waves`, `plans`, `forgefake`, `tools`,
`store` and `project` passes in a throwaway container (database `yuno_test_w3r`); every
finding below was confirmed by reading the code. Eval 028 was not present and is not reviewed.

| # | Sev | Finding | Disposition |
|---|---|---|---|
| H1 | high | Rule 11 compares wall-clock times, and a merge made with the platform's Merge button is stamped with the platform's `now()`, so the pipeline of the merge commit can look older than the merge and the wave never ships | fix: decide on commits (D-074 says "on a commit at or after the last merge"), or stamp `MarkMerged` with the provider's `merged_at` |
| H2 | high | A ship run that succeeds without having advanced the wave (no `advance_wave` call, or `last` reported wrongly) leaves the plan `shipping` or `accepting` forever, with nothing recorded and no one told | fix: on success, require the wave's `wave_done` event (and the accept approval when `accepting`); otherwise escalate as for a failed ship run |
| M1 | medium | A retried ship run that ends badly again is silent: no pause, no event, no notice | fix: deduplicate the escalation on the run id plus its retry count, not the run id alone |
| M2 | medium | `GET /api/runs?trigger=plan` is a 422, so the approval card never names the plan, and the Runs page's new "plan" filter fails | fix: add `TriggerPlan` to `runTriggers` and to the openapi enum; the card reads the run by id |
| M3 | medium | D-075's small MR that appends the lessons to `CLAUDE.md` is not built: the ship pipeline saves project notes only | disclose (or fix: grant `open_merge_request` to the ship Integrator, gated as usual) |
| L1 | low | `Advance` and the first release write the tracker before the transaction, which does not check the pause again: a pause in between still releases the next wave | fix: re-read `waves_paused` under the plan lock and refuse; the labels already added are disclosed |
| L2 | low | The closed-issue rule judges `building` tickets as well as `released` ones, and GitLab's listing drops confidential issues, so a confidential ticket issue escalates as "closed" | fix: judge `released` tickets only (as DESIGN says), and count confidential issues as open |
| L3 | low | `make eval` recreates the dev api with `EVAL_FORGE_URL` set and leaves `forgefake` running until the next `make run` | fix: restore the api without the setting and stop `forgefake` at the end of the target, or disclose in the README |
| L4 | low | The smoke-test message lacks the wave's merged MRs and main's pipeline (rule 13) | fix: pass them in the ship run's input and template them |
| L5 | low | The wave timeline shows "Shipping" for a wave whose ship run failed or was rejected, and "Waiting for your smoke test" for any approval the ship run waits on | fix: a "Stopped" state for a terminal, unsucceeded ship run; read the pending approval's node |
| L6 | low | No test drives `advance_wave` through the gate (gated before the smoke-test approval, through after it); the ticket run's `manual_test` approval also admits `advance_wave`, and only `Advance`'s ship-run check refuses it | fix: add the gate test; tie `autoAfterTools` to its approval node (`smoke_test` for `advance_wave`) |
| L7 | low | The threat model is not updated for wave 3: `advance_wave` under `auto_after`, runs the watcher starts by itself, the `EVAL_FORGE_URL` http exception | fix: add the paragraphs |
| L8 | low | A ticket the watcher escalates (MR closed unmerged, issue closed) cannot be recovered except by cancelling the plan: its run succeeded (not retryable), or it has none | disclose until plan revision (rule 16, wave 4) |

## High

**H1. "Green after the newest merge" can be false for the pipeline the merge itself started.**
`internal/waves/waves.go:342` ships only when `main.At` (GitLab: the newest default-branch
pipeline's `created_at`, `internal/tools/providers.go` `DefaultBranchStatus`) is not before
the newest merge. The newest merge is the record's `closed_at`
(`internal/store/plans.go` `ListWaveTickets`). When the sync sees the merge, it stores the
provider's `merged_at`. When the operator merges with the platform's Merge button
(`internal/httpapi/mergerequests.go:137` → `mergereq.Sync.ConfirmMerged` →
`MarkMerged`, `internal/mergereq/sync.go:162`), it stores `s.now()`, the platform's clock,
taken after the merge call and a status read have returned.

Scenario: on GitLab the operator merges the last ticket of wave 1 from the project page at
10:00:00.0. GitLab creates the merge commit's pipeline at 10:00:00.4. The platform records
`closed_at` 10:00:00.9. The pipeline passes, but its `created_at` is before `closed_at`, so
`shippable` is false on every pass. Because the next wave is not released, nothing else
lands on main and no newer pipeline arrives. The plan sits in `building`. The page shows
Main "Passing" and every ticket merged, nothing escalates, and the wave never ships until
someone re-runs a pipeline by hand. Clock skew between the forge and the host has the same
effect in either direction.

D-074 itself states the rule by commit ("passed on a commit at or after the last merge").

Fix: compare commits instead of times. The pipeline's sha must be the default branch head,
and each ticket's merge commit must be an ancestor of it (or simply: the pipeline sha equals
the head read after the last merge). At minimum, `ConfirmMerged` should pass
`st.ClosedAt` (the provider's `merged_at`) to `MarkMerged`, so that both times come from
the forge's clock.

**H2. A ship run that succeeds without advancing strands the plan silently.**
The integrator's edges route on the model's own `last` output
(`templates/ship_pipeline.json:141,150`). `onShipRunTerminal`
(`internal/plans/plans.go:617-624`) returns on success unless `accept_feature` was approved,
on the assumption that "advance_wave moved the plan on already". Nothing checks that
assumption. There are two failure scenarios:

- The Integrator saves its lessons, skips `advance_wave` and finishes with `last: false`. The
  run succeeds. The plan stays `shipping` at the wave, and the watcher ignores `shipping`
  plans. The unique index refuses a second ship run, and a succeeded run cannot be retried.
  No event is written and no notice is sent.
- On the last wave the Integrator calls `advance_wave` (the plan becomes `accepting` and the
  operator is told "waiting for your acceptance", `waves.go:612`), then finishes with
  `last: false`. The run ends at `end` without the `accept_feature` approval. The plan stays
  `accepting` forever, and the operator was told to accept something that never opens.

Fix: in `onShipRunTerminal`, when a ship run succeeds, look up the wave's `wave_done` event.
If it is missing, or the plan is `accepting` with no approved `accept_feature`, take the
non-success path (pause, `escalated`, notice). Better still, route the edge on the recorded
advance result rather than the model's output. Record `accept_waiting` when the approval
opens, not inside `Advance`.

## Medium

**M1. The second ending of a retried ship run is silent.** `internal/plans/plans.go:670`
deduplicates the ship run's escalation on `(escalated, wave, run_id)`. `engine.Retry`
requeues the same run row (`internal/engine/retry.go`), which is the operator's only way
forward after a smoke-test rejection: resume, then retry. Scenario: the smoke test is
rejected, so the waves pause and the operator is told. The operator resumes and retries.
The smoke test is rejected again (or the approval expires, or `advance_wave` refuses). The
second ending finds the first event, so the project is not paused, no event is recorded and
nothing is sent. This is the same class as the wave 2 finding H2.

Fix: put the run's retry count in the event detail and deduplicate on run id plus retry
count.

**M2. The plan link on approvals never renders, and the new run filter errors.**
`web/src/components/approval/ApprovalCard.tsx:57` calls `listRuns({trigger: "plan"})`, and
`web/src/lib/api.ts:301` adds `"plan"` to `RUN_TRIGGERS`, which the Runs page offers as a
filter. However, `internal/httpapi/runs.go:34` (`runTriggers`) and the openapi enum at
`api/openapi.yaml:239` do not include `plan`, so the server answers 422. On the approvals
page, the ship approvals therefore never show "Feature x, wave n" (`ShipRunPlan` renders
nothing on the error), and choosing "plan" on the Runs page shows an error. The web test
mocks `/api/runs` and so does not catch the mismatch.

Fix: add `store.TriggerPlan` to `runTriggers` and to the enum. Have the card read the
approval's run directly (`getRun`) instead of a capped list of 200.

**M3. D-075's `CLAUDE.md` MR is missing.** Rule 13 and D-075 say lessons go to project notes
and to the repository's `CLAUDE.md` through a small MR. The wave 3 plan's contract (Integrator
tools `memory_save, advance_wave, escalate, finish`) and `templates/ship_pipeline.json:116`
cover project notes only, and the change does not record the deviation.

Fix: either disclose it in DESIGN 5.29 and the wave report, or grant `open_merge_request` in
the ship run. The `auto_after` rule keeps that call gated, because `CLAUDE.md` is not under
`docs/`.

## Low

**L1. The pause is not re-checked where the tracker is written.** `Advance` reads
`waves_paused` (`internal/waves/waves.go:518`), closes the wave's issues, and labels the next
wave with the intake label (`:549`) before its transaction. The transaction (`:557-616`)
re-checks the status and the wave but not the pause. `afterPlanMerge` does the same
(`:185`, `:207`). Scenario: an operator pauses while a ticket escalates between the check
and the labelling. The next wave's issues get the intake label and intake starts building
them, which contradicts "nothing is released while paused".

Fix: read the project row under the plan lock in the transaction and refuse when paused.
Because the label is already on the tracker, document that a pause inside that window
can let one release through.

**L2. The closed-issue heuristic can misfire.** `escalateEnded` collects `released`,
`building` and `in_review` tickets without a merge request record
(`internal/waves/waves.go:354`), while DESIGN's edge case limits the rule to released
tickets. GitLab's `ListLabelled` also drops confidential issues
(`internal/tools/issues.go:209`). Scenario: someone marks a ticket's issue confidential. It
disappears from the open list and escalates with the reason "its issue was closed without a
merged merge request", pausing the waves for a reason that is not true.

Fix: judge `released` tickets only, and for this check list issues without the
confidential filter.

**L3. `make eval` changes the dev stack.** The `eval` target (`Makefile:147-153`) recreates
`api` with `EVAL_FORGE_URL=http://forgefake:8090` and starts `forgefake`. Neither is undone
afterwards. Until the next `make run`, the dev api accepts `http://forgefake:8090/...` as a
repository URL, and that stack sends the project token in clear to a forge that is still
running. The recreate also restarts the api in the middle of the session, interrupting
runs that are in flight. The exception itself is tight (exact host and port, http only,
empty means none: `internal/project/project.go:49-69`).

Fix: at the end of the target, run `EVAL_FORGE_URL= $(COMPOSE) up -d --no-deps api` and
`$(COMPOSE) --profile eval stop forgefake`, or state in the README that `make run` restores
the stack.

**L4. The smoke-test message is thinner than rule 13.** Rule 13 asks for "the wave, its
merged MRs, main's pipeline". `templates/ship_pipeline.json:83` names only the wave and the
feature, because the watcher's input (`waves.go:443`) carries neither the merge requests nor
`main.url`.

Fix: add `merge_requests` and `main_url` to the input and the message.

**L5. The timeline can show the wrong state.** `planWaves` (`web/src/lib/plan.ts:142`)
labels a wave "Shipping" whenever it has a ship run that has not succeeded, including a
`failed` one after a smoke-test rejection. In that case the plan is paused and nothing is
shipping. `waiting` also reads "Waiting for your smoke test" for any `waiting_approval`, for
example a tool approval of the Integrator.

Fix: add a "Stopped" state for a terminal, unsucceeded ship run, and derive the waiting text
from the pending approval's `node_id`.

**L6. The gate for `advance_wave` has no test, and `auto_after` is not tied to the tool.**
`internal/httpapi/mcp_autoafter.go:17` lists `advance_wave` for every `auto_after` node.
`mcp_autoafter_test.go` exercises only `open_merge_request`, so no test shows that
`advance_wave` is gated before the smoke-test approval and goes through after it. In a ticket
run, the `integrator_ready` node's `auto_after: manual_test` also admits `advance_wave`
without an approval. Only `Advance`'s "not a ship run" refusal (`waves.go:495`) stops it.

Fix: add the gate test, and accept `advance_wave` only when the covering node is
`smoke_test`, or the open-MR variant only when it is `manual_test`.

**L7. The threat model lags the code.** `docs/threat-model.md` covers `auto_after` for the
ready call only. It does not cover `advance_wave` running under the smoke-test approval, the
watcher starting runs without an operator, or the http exception for the eval forge (its
Git import paragraph still says https only).

Fix: add three short paragraphs.

**L8. Some escalations have no way back yet.** When the watcher escalates a ticket for a
closed, unmerged MR or a closed issue (`waves.go:359-383`), the ticket becomes `failed`. Its
run either succeeded (it opened the MR), so `engine.Retry` refuses it, or it never existed
(a released ticket). `RetryTicketOfRun` is the only path back to `building`, so the plan can
only be cancelled.

Disclose this in DESIGN 5.29's edge cases until rule 16's revision lands.

## Checked and found sound

- One ship run per wave: the partial unique index `runs_plan_wave_idx` covers runs in any
  status, and the losing watcher records the winner. Two concurrent `Advance` calls
  serialise on `LockPlan`, and the second replays `wave_done`
  (`TestAdvanceRunsOnce`).
- `Advance` refuses while paused, for a non-ship run, for a wave that is not being shipped,
  and for a finished plan, and a tracker failure leaves no partial database state.
- A tracker outage never escalates. Errors from the issue list, the branch status and
  labelling are logged and retried, and a listing longer than 10 pages concludes nothing.
- Notices: `smoke_test_waiting`, `wave_done`, `accept_waiting` and `feature_done` are each
  written once, in the transaction that moves the plan, and the sweep covers them.
- No token appears in watcher logs or tool errors (provider errors follow 5.21's rules).
  `forgefake` compares the token in constant time and keeps repository paths to a strict
  pattern.
- T5.29.8 to T5.29.10 are present: `TestGitLabDefaultBranchStatus`/`TestGitHubDefaultBranchStatus`,
  the `TestWatcher*` set (release after the plan merge, merged and green, hold while paused),
  and the `TestAdvance*` set (close, release, last).
