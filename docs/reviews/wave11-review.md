# Wave 11 review (2026-10-18)

Two Sonnet 5.5 reviewer agents read the wave's diff against the design commit
(`1e3d28b..7226f64`) with disjoint scopes: the engine, the store and the graph
validation; and the HTTP API, the contract, the web UI, the template, the seeded agents
and `run_status`. Both ran the packages' tests read-only in their own databases and
reproduced every high and medium finding with throwaway probes (removed afterwards); the
engine reviewer also ran the race detector over the fork tests in a Go image with a C
toolchain and found no data race. The coordinator's disposition of each finding follows;
where a finding showed a rule wrong rather than code wrong, the rule was amended first
(noted). The engine fixes are the coordinator's; the UI fixes were made by one Sonnet 5.5
fix agent in one pass.

## Engine, store and validation

| # | Severity | Finding | Disposition |
|---|---|---|---|
| H1 | high | Re-claiming a released fork marks the other parked branch's visit `worker lost` and the first branch's `expireStepQuestions` expires its pending question with `step finished`; the operator's later answer is `not pending` and the branch waits forever (also after a crash with a parked branch and a running sibling) | fixed: a visit still parked on its question (`parked_at` set, `waiting_approval`) is never a lost worker's: left out of the stale list, of `FailRunningNodesExcept` and so of the question expiry (5.26 rule 9) |
| H2 | high | A consumed parked visit (closed `succeeded`, `parked_at` still set) matched the parked predicate while the run waited on the fork visit, so the sweep re-queued the run on an already-consumed answer at every pass, and a run with another branch's expired question cycled `queued` ↔ `waiting_approval` instead of failing | fixed: `RequeueParkedRun`, `IsParkedOn` and `ListParkedQuestions` see a parked visit only while it is still `waiting_approval` |
| M1 | medium | A parked visit stayed open (or was later marked `worker lost`) when the fork continued its branch itself: after an answer during a sibling's run, and after the release re-check, which re-queued through `RequeuePausedRunAt` | fixed: the fork closes the parked visit `succeeded` before continuing the branch, and the release re-check re-queues through `RequeueParkedRun` as a decision does |
| M2 | medium | Fork and branch rows left open on terminal paths: a cancel of a run re-queued after a release (before its claim) skipped the approvals, the visits, the branch rows and the root span; a limit or a cancel found at the fork's re-entry left the fork row `running` in an ended run; the stale sweeper's attempt-limit failure skips `closeBranches` | fixed for the first two: the cancel of a queued run closes it as a waiting run is closed, and `finishRun` fails every visit the ending left open; the stale sweeper's second case is recorded (rare: a worker lost repeatedly) |
| M3 | medium | No `branch.status` event when `RequeueParkedRun` set the branch back to `running` in SQL, and no `node.status` for the visits a stop closed | fixed: both are published |
| M4 | medium | In a loop around the pair the sibling exclusion of rule 10 was defeated: the sibling's agent has a visit from the earlier pass, so its workspace, being written by the sibling's current visit, was a source | fixed: the agents of the sibling branches of the fork visit are never sources, whichever of their visits is looked at (rule 10 amended) |
| M5 | medium | The same agent before the fork and inside a branch the join does not name: after the join nobody supplies `repo/`, since that agent's only workspace now holds the branch's tree | fixed in the rule: in a project workflow that shape is refused at save (`nodes[n].agent_id: agent '<id>' runs before the fork 'F' and inside its branch '<h>', which is not the join's checkout; the checkout from before the fork would be lost`), rule 2 amended |
| L1 | low | Crash window in the join: the fork row closed `succeeded` before `current_node` moved, so a crash between the two re-ran every branch | fixed: the position moves first; a crash then leaves a fork row the next claim marks lost, never a fresh entry |
| L2 | low | An expired parked question while a sibling still runs is acted on only after the release, by the next sweep | not changed: bounded by the sibling's run time; recorded in As built |
| L3 | low | A branch's `no matching edge from <node>` and `node iteration limit` reported the bare reason instead of `branch <head> failed: <reason>` | fixed: those two are the branch's own failures; the run-wide limits (steps, cost, tool calls, time, cancel, loop) keep their reason |
| L4 | low | A sibling that failed on its own just after the first failure was recorded `cancelled` | fixed: recorded as it ended |
| L5 | low | The per-branch cancel functions were called only on the failure path | fixed |
| L6 | low | The retry's loop-visit check counted the fork's visits, refusing a retry whose fork had run `max_loop_visits` times | fixed: at a fork the check compares the unfinished branches' positions, as their entry will |
| L7 | low | `closeFork` ignored write errors | fixed: logged |
| L8 | low | Smaller items: the re-entry span's `branches` attribute null (fixed); `agent_id` and `agent_name` compared apart in the agent-distinctness check (not changed: a stored workflow has ids, a template names, never both); a fork or join with a `task` or an agent accepted silently (not changed: ignored fields); the resume exception also covering an approval-node visit re-queued early (benign) | as noted |

## HTTP API, contract, UI, template and seed

| # | Severity | Finding | Disposition |
|---|---|---|---|
| M1 | medium | The header counted a parked branch as running: `panel (1 branch running)` next to the waiting badge after a release | fixed: only `running` counts; `(1 waiting for an answer)`, `(2 branches running, 1 waiting for an answer)`; rule 13 amended |
| L1 | low | A retry mark lands after the whole fork card rather than inside the failed branch's column, since the fork row is reused across a retry | not changed: recorded as a UI limitation (the branch's two visits are both inside the card, labelled by visit) |
| L2 | low | The fork card never showed the fork row's own `failure_reason` | fixed |
| L3 | low | Branch columns cramped between about 768 px and 1000 px with the sidebar | fixed: columns stack below `lg` (four or five below `xl`) |
| L4 | low | Landmark names repeated across fork visits in a loop; heading levels skipped | fixed: the visit number in the branch and join names when the fork ran more than once; heading levels in order |
| L5 | low | A cancelled sibling's `stopped: …` reason in the same red alert as the failed branch's | fixed: muted text for a cancelled branch (the node card inside keeps the alert) |
| L6 | low | A stale `checkout` after the fork edge to its head was removed or retargeted; a stale `join` with no join nodes could not be cleared | fixed: the builder clears the checkout, and the selector shows a stale join so it can be cleared |
| L7 | low | The reviewer prompts allowed more than rule 14's "run nothing that changes the tree" | fixed: the prompts say so (no installs, no formatters, no git beyond status, diff and log) |
| L8 | low | The contract tests asserted few fields; `RunBranch.required` omitted `current_node`, which Go always emits | fixed: `current_node` required; the trace test asserts position, fork and visit fields |
| L9 | low | The template description names `max_loop_visits` | not changed: inherited from the Dev pipeline's description; the limit is an operator setting shown by that name on the Limits panel |

Recorded, not changed: a replayed older `branch.status` event moves a branch back in time
until the newer one arrives, as `node.status` does (the stream replays in order).

## Found by CI after the fix pass

The CI host runs the engine package about six times slower than the development machine,
and the first pipeline on `main` failed two new tests there: the step-limit test expected
one branch stopped by the fork, but on the slow host both branches saw the summed limit
themselves and were recorded as they ended (what rule 6 allows; the test now accepts
either), and a park test read the branch's park log line as soon as the branch row showed
it parked, a moment before the line was written (it now waits for the line). No code
changed.

## Coverage reported by the reviewers

Rules checked and found correct: 5.26 rule 1 (both kinds, every true fork edge, `no
matching edge` on none); rule 2 over 39 odd shapes (a head reached before the fork, a
join → branch edge, `on_failure` into and out of a region, a fork edge to a post-join
node, two forks sharing nodes or a join, a bypass of the join, nesting, an approval or an
end inside, sibling `input_from`, a duplicate head, six branches, a checkout off a project;
the allowed loops and shapes); rule 3 (one transaction, edge order, events, the log line);
rule 4 (key-wise state, branch positions, `branch_id`, spans under the fork, the active set,
no deadlock or leak in the event loop); rule 5; rule 6 (the stop, the reasons, the summed
counters, limit and cancel pass-through); rule 7; rule 8 (park, release, re-check, the
expiry after the release); rule 9; rule 10 for one pass; rule 11; rule 12; the new SQL; the
redaction of the new log lines; the contract against the Go tags and the TypeScript types;
the Timeline over every state listed (zero rows, a missing fork row, two attempts in one
branch, a parked branch, a cancelled branch, a loop, a foreign run's event); the builder's
rename and delete; the exact copy; dark mode; the template's graph, schemas, tasks and
limits; the three prompts; `run_status.branches` on the newest fork visit.
