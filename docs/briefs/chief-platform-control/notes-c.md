# Notes, wave 1 part C: liveness from evidence

Branch `w1-liveness`. For the coordinator to fold into `docs/DESIGN.md`, `DECISIONS.md`,
`CHANGELOG.md`, `README.md` and `docs/threat-model.md`.

## DESIGN 5.12 (text to add): liveness from evidence

A run's status says where the state machine is, not whether anything is happening. Each live
agent visit records evidence on its `run_nodes` row, written by its own goroutine
(`engine/evidence.go`, migration `0028_liveness.sql`):

- `heartbeat_at`: at the visit's start, then on the worker's tick every 30 s, in the same
  statement as the run's `updated_at` (`store.TouchRunNode`).
- `progress_at`: the turn's last sign of life, the stall check's own clock (DESIGN 5.5 rule 6):
  a chat or agent event of the turn, or a new transcript entry; also the turn's start, a
  gateway reconnect and a failed abort, which restart the stall window. Stamped at the
  visit's start. Written with any other evidence write, and on its own at most once per
  30 s of the engine's clock.
- `waiting_on`, written when it changes, in this order of precedence:
  `human` (the visit is paused on an exec approval, a tool approval or a question),
  `setup` (before the first turn), `""` (between turns, or the gateway connection is lost),
  `tool` (a call of the live turn is open), `model` (otherwise).
- `model_wait_since`: when `waiting_on` became `model`; null otherwise.
- `open_calls`: `[{id, name, started_at, summary}]` of the live turn, oldest first; `summary`
  is the redacted first 200 characters of the call's `command`, `path` or `file_path`
  argument, on one line. `started_at` is the tool's own start (the item event) when seen,
  else the time the model asked for it.

Fork, join and approval node rows have no agent and no visit goroutine, so they carry no
evidence and are not diagnosed; each branch visit is its own row and is.

### Thresholds (seconds)

| Name | Value |
|---|---|
| H (heartbeat fresh) | 90 (3 × the 30 s heartbeat) |
| P (progress fresh) | 120 |
| M | `MODEL_STALL_AFTER`, 600; 0 turns the restart off and nothing is called stalled by rule 6 |
| S | `ENGINE_STALE_AFTER`, 300; 0 turns the sweeper off and nothing is called stalled by rule 2 or between steps |
| T (tool overdue) | 1860 (the gateway's 1800 s exec timeout plus 60) |

### Rules: a live visit, in order, at `checked_at`

0. A parked visit (DESIGN 5.24 rule 5: no worker, no turn) is `waiting`, "parked, waiting on
   <question|approval> <id> for <age>".
1. No `heartbeat_at`: `unknown`, "no heartbeat recorded".
2. Heartbeat older than S: `stalled`, "worker silent <age>; the sweeper re-queues after S".
3. Heartbeat older than H: `unknown`, "heartbeat <age> old".
4. `human`: `waiting`, "waiting on <question|kind approval> <id> for <age>" (the visit's
   oldest pending decision).
5. `tool`: the oldest open call older than T is `stalled`, "<name> running <age>, past its
   timeout"; otherwise `executing`, "running <name> <summary> for <age>". No open call, or one
   with no start time, is `unknown`.
6. `model`: the wait runs from the later of `model_wait_since` and `progress_at` (a reply
   streaming in restarts it, as it restarts the engine's stall window). At most P:
   `executing`, "model replying"; at most M: `waiting`, "waiting on the model <age>;
   restarted at M"; otherwise `stalled`. No `model_wait_since` is `unknown`.
7. `setup` or `""`: `progress_at` within P is `executing`; older or missing is `unknown`,
   "no activity for <age>" / "no activity recorded".

Missing telemetry never yields `executing`.

### Run liveness

- A finished run reports its status as its liveness and its failure reason as the reason.
- `queued`: `blocked` when its project has reached a budget cap (the budget check's own
  refusal text, DESIGN 5.19 rule 3) and/or its waves are paused ("the project's waves are
  paused: <reason>"), joined with "; "; otherwise `waiting`, "queued <age>" since its last
  write (queued or re-queued).
- `waiting_approval`: `waiting`, naming the run's oldest pending decision and its age, or
  "paused <age>, waiting on a decision" when no pending row is left.
- `running`: the worst of its live visits, stalled > unknown > blocked > waiting > executing
  (the first listed wins a tie), as "<node id>: <reason>". With no live visit (between
  steps): `runs.updated_at` within H `executing`, within S `unknown`, older `stalled`.

### Outputs

- `GET /api/runs/{id}/diagnosis` (`getRunDiagnosis`, read): `{run_id, status, liveness,
  reason, checked_at, thresholds: {heartbeat_fresh_s, progress_fresh_s, model_stall_s,
  worker_stale_s, tool_overdue_s}, nodes: [{node_id, visit, branch_id, agent, status,
  liveness, reason, heartbeat_at, progress_at, waiting_on, model_wait_since, open_calls:
  [{name, summary, started_at, age_s}], last_event: {seq, event, at} | null}], pending:
  [{approval_id, kind, requested_at, age_s}]}`. `agent` is the agent's name; `last_event` is
  the newest node.status, model_call, tool_call or approval event naming the visit.
- `GET /api/runs/diagnosis?project_id=` (`listRunDiagnoses`, read): an array of the same for
  every queued, running and waiting_approval run, newest first, at most 100, one
  `checked_at`.
- `GET /api/runs/{id}/events/page?after=&limit=&event=` (`listRunEventsPage`, read):
  `{events: [{seq, event, at, data}], next_after}`, ascending; `limit` 1 to 500 (default
  100, anything else 422), `after` ≥ 0, `event` comma-separated types; `next_after` is the
  last seq returned, or `after` for an empty page; every `data` goes through
  `obs.RedactJSON` again.

## DECISIONS (D-099 text)

D-099 Liveness from evidence. A live visit writes heartbeat, progress, what it waits on and
its open tool calls to its own `run_nodes` row; a pure classifier (`internal/liveness`) turns
them into executing, waiting, blocked, unknown or stalled with the reason and ages, against
H 90, P 120, M (MODEL_STALL_AFTER), S (ENGINE_STALE_AFTER) and T 1860 seconds. Chosen over a
separate heartbeat table (one more write path per tick) and over deriving liveness from
`run_events` (no event for "nothing happened"). The model wait counts from the later of the
wait's start and the last progress, so the diagnosis says what the engine's stall check will
do. Costs: one extra statement per visit start and per change of what the visit waits on;
the heartbeat rides on the existing 30 s touch.

## CHANGELOG

- Runs say what they are doing: `GET /api/runs/{id}/diagnosis` and `GET /api/runs/diagnosis`
  report each live run as executing, waiting, blocked, unknown or stalled, with the
  heartbeat, progress, model wait and open tool calls that prove it.
- `GET /api/runs/{id}/events/page` pages a run's events, filtered by type and redacted.

## README settings

No new setting. `MODEL_STALL_AFTER` and `ENGINE_STALE_AFTER` now also set M and S of the
diagnosis.

## Threat model

- The diagnosis shows the first 200 characters of each open call's command or path. They
  come from the stored tool input, which is already redacted, and are redacted again.
- The events page re-redacts each event's data on the way out, so a row written before a
  redaction rule existed does not leak through it. Both endpoints are `read` and sit behind
  the same bearer as the rest of `/api`.

## Rules I had to interpret

1. **First heartbeat at the visit's start.** The tick runs only once the first turn's loop
   starts, so a visit would read "no heartbeat recorded" for its first 30 s. The visit's row
   gets `heartbeat_at = progress_at = start`, `waiting_on = setup`, when it is created.
2. **Clocks.** `heartbeat_at`, `progress_at`, `model_wait_since` and the 30 s progress
   throttle use the engine's clock (testable with the fake clock); the heartbeat cadence stays
   real time, as the existing touch was. In production both are the wall clock.
   `checked_at` is the api's wall clock.
3. **"Same write as TouchRun"** is one statement: a data-modifying CTE updating the visit's
   `heartbeat_at` and the run's `updated_at`.
4. **Rule 6's age** counts from the later of `model_wait_since` and `progress_at`. Read
   literally, a reply streaming for more than M would be called stalled while the engine,
   which counts from the last progress, does not restart it.
5. **`progress_at` reuses the stall check's `progressAt`** as the brief asks, including the
   resets at turn start, gateway reconnect and a failed abort; the diagnosis then agrees
   with when the engine will act.
6. **"At most once per 30 s"** applies to writes made for progress alone. When `waiting_on`
   or `open_calls` changes, the current progress rides on that write (no extra statement). A
   held-back progress is written on the first loop pass after 30 s.
7. **`waiting_on` precedence:** human > setup > "" > tool > model. "human" is the visit's
   node paused (`waiting_approval`) by an exec approval, a tool approval or a question. A
   lost gateway connection is "" (the stall check is off then too), so rule 7 applies.
8. **Open calls** are listed only while the turn is live. `summary` takes `command`, then
   `path`, then `file_path`; whitespace is collapsed to one line; the 200 limit is in
   characters (runes), after redaction.
9. **Parked visits** (`parked_at` set) are classified `waiting` on their decision before rule
   1: they have no worker by design, so the heartbeat rules would call every parked visit
   stalled.
10. **Which rows are "live nodes":** rows with an agent, not finished, status pending, running
    or waiting_approval. Fork, join and approval node rows are left out (no goroutine, so they
    would read unknown forever and make every fork run unknown). Live rows of a queued run
    (left by a lost worker) are listed with their verdicts; the run rule ignores them.
11. **M or S set to 0** (the platform does not act): the rule that names it never says
    stalled; the thresholds report 0.
12. **Missing pieces inside a rule** are `unknown`: model without `model_wait_since`, tool with
    no open call or a call without a start, an unrecognised `waiting_on`.
13. **Blocked queued runs:** the reason is `project.CheckBudget`'s `BudgetError` text and/or
    "the project's waves are paused[: reason]", joined with "; "; a deleted project blocks
    nothing. Neither condition stops a worker from claiming a queued run today (the budget
    is checked when a run is queued; paused waves stop wave releases and ship runs), so a
    run reported blocked may still start. Reported as DESIGN C says.
14. **Queued age** is from `runs.updated_at` (when it was queued or re-queued), not
    `created_at`, so a retried run does not read as queued for days.
15. **Terminal runs:** liveness is the status, reason the failure reason ("" on success),
    no nodes, no pending.
16. **Worst-of ties** go to the first visit listed (oldest start); the run's reason is
    prefixed with that visit's node id.
17. **`agent`** is the agent's name (its id when the agent row is gone). **`last_event`** is
    the newest node.status, model_call, tool_call or approval event naming the visit's row id;
    log lines carry no node, so they are not considered.
18. **List endpoint** returns a JSON array, newest first by `created_at`, one `checked_at`; a
    `project_id` with a control character lists none (as `GET /api/runs` does).
19. **Events page limits:** a `limit` outside 1..500 is refused 422 with a detail on `limit`
    (as `GET /api/runs` refuses its limit), not clamped; `after` must be a non-negative
    integer; an unknown run is 404; blank entries in `event` are ignored.
20. **Threshold keys** in the output are `heartbeat_fresh_s` (H), `progress_fresh_s` (P),
    `model_stall_s` (M), `worker_stale_s` (S), `tool_overdue_s` (T).
21. The heartbeat cadence became an engine field (`heartbeat`, default 30 s) so
    TestEvidenceWritten can watch the tick write it.

## Left open

- During setup (materialisation, clone, sandbox) no tick runs, so a setup longer than 90 s
  (each git step may take up to 2 minutes) reads `unknown`, and longer than S `stalled`,
  although it is working. The stale-run sweeper reads the same silence on `updated_at`
  today. A heartbeat during setup would fix both; out of scope here.
- No UI view of the diagnosis (no `web/` changes), and no Chief tool beyond part A's
  `platform_call`, which reaches the three operations.
