# Wave 8 review (2026-10-17)

Two Sonnet 5.5 reviewer agents read the wave's diff against `main` with disjoint
scopes: the engine, tools, project, memory, mergereq, sched, seed and openclaw packages;
and the HTTP API, the store, the contract and the web UI. Both ran the packages' tests
read-only; the backend reviewer reproduced its two mediums with throwaway probes.
Neither found a high-severity defect or a token leak. The coordinator's disposition of
each finding follows; the fix pass covered every kept item in one round.

## Backend

| # | Severity | Finding | Disposition |
|---|---|---|---|
| M1 | medium | A retry is accepted while the node's `max_loop_visits` is used up and the run fails again at once with `loop limit <n> exceeded`; the dialog never names that limit | fixed: `Retry` counts the current node's visits and adds `max_loop_visits` to the exhausted details (5.21 rule 2 amended) |
| M2 | medium | A provider error body cut at 300 bytes mid-rune is invalid UTF-8; writing it to `last_sync_error` fails and `TickOnce` returns, so that record sorts first on every pass and stalls the loop | fixed: the text is always made valid UTF-8 and cut on a rune boundary |
| L1 | low | No engine test for a project at its budget refusing a retry (T5.21.9) | fixed: test added |
| L2 | low | A failed `SetRunStartedAt` on the first claim after a retry leaves `started_at` null, so the attempt has no time limit | fixed: the in-memory run keeps the stamp when the write fails |
| L3 | low | A run failed by the stale-run sweeper and then retried reuses its open root span, without the `retry` attribute | fixed: the retry ends an open root span before re-queueing |
| L4 | low | A later `open_merge_request` replaces the branch's tree with the checkout, reverting commits others pushed to the branch in between | not changed: the one-commit rule of 5.16 rule 10; wave 9's iteration on a merge request clones the branch instead. Recorded as a limitation |
| L5 | low | A provider 404 closes the record for good; a token that lost access also answers 404 | not changed: 5.20 rule 3 says so; a closed record can be synced by hand. Recorded |
| L6 | low | Records older than 14 days are never auto-synced | not changed: 5.20 rule 3; Sync on the page covers them |
| L7 | low | A sync that wins the race against the merge handler leaves `merged_by` null | not changed: 5.20 allows it |
| L8 | low | A redaction assertion checks for `ghp_` on a project whose token is `glpat-` | fixed: asserts on the fake's token |
| L9 | low | Two concurrent saves at 100 notes leave 101 until the next save | not changed: the same pattern as agent memory |
| L10 | low | The approval card says "Open merge request" for a call that will push onto the run's existing request | not changed: `Describe` has no database access; noted |
| L11 | low | A GitLab merge 2xx is trusted as merged without reading the state back | fixed: one `Status` call after the merge; a request the provider still reports open stays open with `last_sync_error` |
| L12 | low | A retry's spend counts in its run's creation window; a crash between the provider's create and the commit could open a second request; GitHub check runs read the first 100 only | not changed: spec-conformant or minor; noted |

## HTTP API, store and UI

| # | Severity | Finding | Disposition |
|---|---|---|---|
| M1 | medium | Cancel stays disabled on a run that was cancelled and then retried (the mutation's success flag is never reset) | fixed |
| M2 | medium | The project page's recent runs are filtered in the browser from the newest 200 runs, so a project whose runs are older shows none | fixed: `GET /api/runs?project_id=` and the page uses it |
| L1 | low | The 429 `project_budget_reached` mapping has no HTTP test and no conformance step | fixed |
| L2 | low | The trace's embedded run never carries `merge_request`, so the page sniffs tool calls and fetches the record | fixed: `getTrace` fills it; the live hook stays for a record that appears while the page is open |
| L3 | low | The schema-documentation test was not extended with the wave's fields | fixed |
| L4 | low | A provider's mergeability message reaches the 409 body without token redaction | fixed |
| L5 | low | The contract does not name the 429 on `POST /api/runs`; `POST /api/projects` answers without `spend` | fixed both |
| L6 | low | The retry dialog's comment says an empty field falls back to the platform default; the server keeps the run's value | fixed: the copy says what happens |
| L7 | low | A merged or closed request's card keeps a disabled Merge and a Sync that does nothing | fixed: hidden once final |

## Coverage reported by the reviewers

Rules checked and found correct: 5.18 rules 1 to 8 (limits, upsert, eviction, recall,
refusal texts, the two task blocks and their caps, the sweep); 5.19 rules 1 to 8
(windows, `>=`, order of checks, schedule `last_error`, both alert texts and the key,
the 80 % comparison, the no-destination case, `list_projects.budget`, the UI tones);
5.20 rules 1 to 9 (the transaction, the advisory lock, the mapping tables, the report
key, the merge preconditions and provider refusals, the list limits, the backfill, the
UI states); 5.21 rules 1 to 7 (texts, the retry cap, the `>=` checks with clamping and
agent caps, the single locked transaction, the new visit with state and files carried
forward, the time limit restart, the report key, the UI button per status). Contract
paths checked: the facts, merge request and retry paths with every documented status.
