# Wave 9 review (2026-10-18)

Two Sonnet 5.5 reviewer agents read the wave's diff against `main` (`ecdba4a..2b49301`)
with disjoint scopes: the engine, tools, project, intake, sched, seed and openclaw
packages with the store's merge request and intake queries; and the HTTP API, the store
types and plumbing, the migration, the contract and the web UI. Both ran the packages'
tests read-only in their own databases; the backend reviewer reproduced its high, one
medium and one low with throwaway probes, the API reviewer one medium. The coordinator's
disposition of each finding follows; where a finding showed a rule wrong rather than
code wrong, the rule was amended first (noted) and the fix pass built the amended rule.
The fix pass covered every kept item in one round with two Sonnet 5.5 fix agents
(backend, frontend).

## Backend

| # | Severity | Finding | Disposition |
|---|---|---|---|
| H1 | high | The taker counts only live runs as "taken": when the label swap never succeeds (a token that cannot label) and the first run ends, the next poll creates a second run for the same issue, and again after every run until the budget stops it | fixed: 5.23 rule 3 amended (a finished run whose outcome is not yet posted counts as taken; the taker skips to the swap), test added |
| M1 | medium | The closer takes the 50 oldest unreported runs every pass, so 50 runs that can never be posted (a deleted project, a deleted issue) starve every newer run | fixed: 5.23 rule 5 amended (newest first; a run whose project row is gone or whose issue answers 404 or 410 is marked reported with a log line; an unreadable token keeps it pending), test added |
| M2 | medium | The budget check compares spend so far, so a poll can put many runs in flight against a small cap before any of them costs anything | fixed: 5.23 rule 2 (a) amended (no take when the cap minus the window's spend minus the `max_cost_usd` of the project's live runs is `<= 0`), test added |
| L1 | low | `read_merge_request`'s fit loop panics under an output cap smaller than the fixed fields (`cutText` with a negative length) | fixed: the cut is clamped and the halving stops at a floor; test at a 300-byte cap |
| L2 | low | The GitHub label swap deletes the intake label before adding `:running`, so a failure between the two leaves the issue unlisted with no state label | fixed on GitHub: add first, then remove; GitLab stays one `PUT` with both lists, which the provider applies together |
| L3 | low | Retrying a run that updates a request does not check that the request's opening run is idle, so both can push onto the branch | fixed: the retry refuses with `is being updated by run <id>` when the opener is live (5.22 rule 1's check on the retry path), test added |
| L4 | low | The approval sentence of `delegate_task` says `merge request !12` for a bare number on a GitHub project | not changed: `Describe` has only the input, no database (the same class as wave 8's L10); the Chief is told to write `#12` on GitHub. Recorded |
| L5 | low | The issue listing reads one page of 50 oldest first, so 50 issues that stay labelled (refused every poll, labelled pull requests) hide every newer one | fixed: 5.23 rule 2 (c) amended (pages of 50 until 10 takeable issues are found or 4 pages were read) |
| L6 | low | The closer's 30 s timeout is per run and up to 50 runs are processed in sequence, so a hanging provider holds a tick for 25 minutes | fixed: one 5-minute deadline per closer pass; a provider timeout ends the pass early (5.23 rule 2 amended) |
| L7 | low | GitLab reads `labels=None` as "unlabelled" and `Any` as "any label", so an intake label named so would take every issue | fixed: `None` and `Any` are refused by the validator (5.23 rule 1 amended) |
| L8 | low | Confidential GitLab issues are taken like any other and their text can reach a public merge request description | fixed: confidential issues are skipped (5.23 rule 2 (c) amended) |
| L9 | low | No index backs the closer's list or the live-run lookup, which binds `trigger` as a parameter and cannot use the partial index | fixed in migration 0013 (not yet deployed): a partial index on unreported issue runs; the live issue lookup writes the literal trigger |
| L10 | low | Every push deletes tracked symlinks (`clearWorkingTree` removes them, `copyTree` skips links), pre-existing since 5.16 and now also hit by updates | not changed: 5.16 rule 10's copy rule (links are never followed or committed); recorded as a limitation in the threat model |

## HTTP API, store and UI

| # | Severity | Finding | Disposition |
|---|---|---|---|
| M1 | medium | A PUT that leaves `merge_request_id` out re-checks the kept request, so a schedule whose request ended cannot be disabled or edited (422), and the Schedules toggle shows only `invalid schedule` without the detail | fixed: 5.22 rule 7 amended (the kept request is re-checked only when `merge_request_id` is in the body or `workflow_id` or `project_id` changes); the toggle shows the first detail; tests added |
| M2 | medium | `intake.workflow_name` is joined without `deleted_at IS NULL`, so a soft-deleted workflow keeps its name on the card next to `intake: no workflow` | fixed: the join excludes deleted workflows; the store test that pinned the opposite inverted |
| M3 | medium | The "+N updates" count on the project page comes from the 200 newest runs, so older updates vanish and with them the only link to the filtered runs list | fixed: when the window is full the link is shown without a count |
| L1 | low | A control character in `merge_request_id` answers 500 on four paths | fixed: treated as an unknown id (422 on writes, an empty list on `GET /api/runs`), test added |
| L2 | low | The budget sentence on the project form shows only while the Enabled switch is on; 5.23 rule 9 ties it to the project having no cap | not changed: the narrower condition is the useful one (the sentence is about enabling); 5.23 rule 9 amended to say so |
| L3 | low | A schedule on a workflow without a project answers `merge_request_id: needs a project` where the run path says `the workflow does not use a project` | fixed: the schedule handler calls the engine's check with a nil project so the texts match |
| L4 | low | `ProjectIntake` lacks `required: [enabled, label]`, `Project` does not require `intake`, and `label`'s `maxLength: 50` counts characters where the server counts bytes | fixed in the contract |
| L5 | low | The save-time sample `{"task":"x"}` refuses a `minLength` above 1 and accepts a `maxLength` every real task would break | fixed: the sample is the task preamble of 5.23 rule 4 with a 16 KiB body |
| L6 | low | Accessibility and copy: the filter chip's button is named "Clear", every "+N updates" link has the same name, the poll result's status region mounts with its text, "Took 2, closed 1" is cryptic | fixed: "Clear merge request filter", the request number in each link's name, the status region rendered empty first, "Started 2 runs, reported 1 finished"; "Poll now" keeps its disabled state with a title saying intake is off |
| L7 | low | Test gaps for M1, L1 and M2; a stale comment about the 422 the contract "does not list yet" | fixed with the items above |

## Coverage reported by the reviewers

Rules checked and found correct: 5.22 rules 1 to 10 (check order and all five texts, the
typed final-state error and the schedule tick, the index mapping with the holder's id,
the source-branch clone with its span attributes and `checkout_sha` before `toolchain`,
the lookup order and the guard in `open_merge_request`, the protocol paragraph,
`read_merge_request`'s forms, mappings, cuts, drop order, escaped closing tag and
redaction, `delegate_task` and `run_status.role`, the report line, the prompts, allowlists
and template); 5.23 rules 1 to 8 (validation order and paths, D-055 on create, update and
the resulting state, the taker's 4xx, 5xx and race cases, label order and texts, the task
block, the closer's texts and `reported_at` written last, `Closes #n`); the amended older
rules; token and untrusted-content handling (no token in a result, record, log line or
fixture; issue text only inside the block); the migration's names against the 23505
mapping; the channel report queries never claiming an issue run; the contract against
`api.ts` and every project query going through the joined select; the UI against 5.22
rule 12 and 5.23 rule 9 (selector only once a project is chosen, "None: open a new one",
loading and error states, `""` to clear, absent-keeps for the Intake section, the Trigger
filter, the issue link derivation for both providers, no developer commentary).
