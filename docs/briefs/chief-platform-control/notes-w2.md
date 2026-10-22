# Notes, wave 2: recovery

Branch `w2-recovery`, from `feat/chief-platform-control` at `0b58d53` (Merge branch
'w1-liveness'). For the coordinator to fold into `docs/DESIGN.md`, `DECISIONS.md`,
`CHANGELOG.md`, `README.md` and `docs/threat-model.md`.

## DESIGN 5.36 (text to add): recovery commands, worker leases, pause, resume and steering

The operator or the Chief stops, holds, restarts and redirects a run through commands. Each
request is a `run_commands` row from receipt to outcome, and two workers never write one run
(migration `0032_recovery.sql`, `engine/commands.go`, `store/commands.go`,
`store/lease.go`).

### Commands

- `run_commands(id, run_id, kind, node_id, payload, actor, request_id, received_at, acked_at,
  applied_at, outcome, detail)`; `kind` cancel | pause | resume | retry | steer; `outcome`
  pending | applied | rejected | failed | superseded. A command's times are the database
  server's clock.
- Routes: `POST /api/runs/{id}/cancel`, `/pause`, `/resume` (`{note?}`, at most 2000
  characters), `/retry` (`{from_node?, limits?}`) and `/steer` (`{node_id?, message}`, 1 to
  4000 characters). Each answers 202 with the run, which carries the recorded `command`; a
  refusal is 409 with the command's detail as the message and `details.command`; a command
  that could not be recorded is 503 `command_not_taken`. `GET /api/runs/{id}/commands` (newest
  first, at most 100) and `GET /api/runs/{id}/commands/{cid}`. Operations `pauseRun`,
  `resumeRun`, `steerRun` (routine) and `listRunCommands`, `getRunCommand` (read).
- The receipt, the decision and, for a run no worker holds, the outcome are one transaction
  that holds the run's row (`SELECT ... FOR UPDATE`): the decision is serialised with a claim
  and with the worker's own reading of its commands. A run no worker holds (queued, paused,
  waiting on a decision with no live visit, or ended) has the command applied or rejected at
  once (`acked_at = applied_at`). A held run (running, or waiting on an approval with a live
  visit whose turn goes on) has it left pending and its visits woken; each visit also reads
  the run's pending commands every 2 s.
- Commands are processed in `received_at` order. A cancel supersedes the pause, resume and
  steer commands still pending before it ("superseded by cancel <id>").
- When a run ends, its pending commands are settled: a cancel of a cancelled run is applied
  (or stays acknowledged while the stop is unconfirmed); everything else is rejected, "the run
  already ended (<status>)". When a worker lets a run go (a park, a pause, an approval node),
  the commands still pending are decided as the api decides those of a run no worker holds.
- Every change is a `run.command` event `{command}`.

### Leases

- `runs.lease_epoch` goes up with every claim and every move to queued: a claim, a stale
  re-queue (and the sweeper's "worker lost" failure), a retry, a resume, the re-queue of a
  parked run and the re-queue after an approval node's decision. The worker keeps the epoch
  it claimed with in a `store.Lease`, carried in the context of everything it does for the
  run.
- Fenced writes carry `AND lease_epoch = $epoch` in their own statement (for node and branch
  rows through their run): `TransitionRun`, `PauseRunAt`, `ResumeRunAt`, `TouchRun` and
  `TouchRunNode` (the heartbeat renews the lease), `SetRunCurrentNode`, `SetRunStateNode` (and
  in a branch), `AddRunUsage`; the visit's progress and finish (`UpdateRunNodeProgress`,
  `FinishRunNode`, `FailRunningNodes`), its parks (`ParkVisit`, `ParkBranchVisit`,
  `PauseVisit`) and evidence; `SetBranchStatus`, `SetBranchCurrent`; the checkout commit's
  `repo_sha` (and `start_sha`); and the acknowledgement of a command. Tool-call, model-call,
  span and event rows are records and are not fenced.
- A fenced write that touches nothing because the epoch moved on returns `ErrLeaseLost` and
  marks the lease lost. The visit then stops itself: it aborts its turn, writes nothing more to
  the run, and the worker logs "worker lost its lease; stopped" through the event recorder. A
  branch that finds its lease lost stops alone: nothing stops its siblings as a failure would,
  each stops at its own next fenced write, and the fork writes nothing.
- A visit's setup (materialisation, clone, sandbox, the first send) has a heartbeat of its own
  every 30 s until the event loop starts, so a long setup renews the lease and does not read
  as a silent worker.

### Cancel

- A queued run, and one no worker holds, is cancelled at once.
- A live run: the worker acknowledges the command and aborts the turn. The stop is confirmed
  by the turn's terminal chat event, by `chat.abort` answering that nothing was running, or,
  after `abortWait` (10 s), by `sessions.list` showing no active run for the turn (no
  `hasActiveRun`, or the turn's id not in `activeRunIds`, or the session not listed). Then
  the checkout is saved, the run goes `cancelled` with `cancel_state = confirmed`, and the
  command is applied. Unconfirmed: the run goes `cancelled` with `cancel_state = unconfirmed`,
  the visit's row `turn_unconfirmed`, and the command stays acknowledged.
- A run the stale sweeper re-queues or fails as "worker lost" gets `cancel_state =
  unconfirmed` for the unfinished turn its lost worker recorded.
- The reconcile pass, on the sweeper's 60 s tick, repeats `chat.abort` and the session check
  for every unconfirmed run until the gateway shows no active run, then sets `confirmed` and
  applies the acknowledged cancels. A gateway that is down confirms nothing.
- Retry and resume answer 409 `cancel_unconfirmed` while `cancel_state = unconfirmed`,
  naming the session and the turn. There is no force.
- Any run that ends with a turn not confirmed stopped (a time limit's abort, for example) gets
  `cancel_state = unconfirmed` the same way, so its retry waits for the reconcile pass too.

### Pause and resume

- New run status `paused`: not terminal, no worker holds it. Transitions: queued → paused,
  running → paused, paused → queued | cancelled | failed.
- Pause of a live run: the worker acknowledges, aborts the turn (confirmed as for a cancel),
  saves the checkout, parks the visit (`waiting_approval`, `parked_at`, `park_reason =
  paused`) and moves the run to `paused` in one transaction, then lets it go. An unconfirmed
  stop fails the command, "could not confirm the turn stopped", and the step goes on. A
  pause of a queued run is applied at once. Refused: a run waiting on an approval or a
  question ("the run is waiting on <question|approval> <id>"), a paused or finished run, a run
  being cancelled, and a run with parallel branches running.
- Resume of a paused run: in one statement the paused visit is closed succeeded without
  output, marked `resumed_at` with the note, and the run re-queued with the epoch up and the
  paused interval added to `paused_total_ms` (a pause does not count toward the time limit).
  The step's next visit continues from the paused visit's checkout (the carry rules pick its
  commit) and its task gains "The operator paused this step and resumed it." and, per resume,
  "The operator's note: <note>". A paused visit does not count toward `max_loop_visits`.
  Refused: any other status, "the run is not paused".

### Retry from a step

- `from_node` must be a node with a visit in the run, outside a fork branch ("from_node must
  be a step this run reached", "retry the fork step instead"; 409 `run_not_retryable`).
- The run's current node becomes `from_node`, its state is kept, and the latest visit of it
  and of every node visited after it is marked `retried_at`. The latest visit of `from_node`
  is marked `retry_from_at`: the step's next visit starts from that visit's `start_sha` (the
  commit its checkout started from, recorded at every visit's start). Later steps run again
  as the graph reaches them, from the commits the carry rules name. Every other retry
  precondition stands.

### Steering

- A steer names the step with `node_id`, or the run's one live visit. The visit acknowledges it
  and sends `chat.send` with `queueMode: "steer"`, message "Operator: <message>" and key
  `<visit id>:steer:<command id>`; the visit's turn id does not change. Applied when a user
  transcript entry with that key (plus `:user`) or containing the message appears. If the turn
  ends first, or no turn is live, the message opens the step's next turn (appended to its
  message), and is applied when that turn starts; a turn that ends without a result and with a
  message waiting continues with it instead of failing. A message the step ends without
  delivering is rejected, "the step ended before the message reached it".
- Refused: nothing running ("nothing is running; retry or resume with a note"), several live
  steps and none named ("name the step: <ids>"), a named step that is not running.

### Diagnosis

`RunDiagnosis` gains `commands_pending` (`[{command_id, kind, received_at, acked_at,
age_s}]`) and `cancel_state`. A paused run is `waiting`, "paused by the operator <age> ago;
waiting for a resume"; its paused step is `waiting`, "paused by the operator; a resume
continues the step".

## DECISIONS (D-102, recovery)

Commands are rows from receipt to outcome, decided under the run's row lock; workers hold
runs under an epoch lease that fences their writes in the same statement; a stop is
"confirmed" only on the gateway's word (terminal event, `aborted: false`, or the session
check), and a retry or resume waits for that word, so two writers never overlap. Pause is a
park with its own run status; resume continues the step in a new visit from the paused
visit's checkout.

## CHANGELOG

- Runs can be paused, resumed (with a note for the step) and steered with an operator's
  message; retry can start from an earlier step. Every cancel, pause, resume, retry and steer
  is a command whose state the run's Commands list and events show.
- A cancel is applied when the gateway confirms the turn stopped; an unconfirmed stop is
  checked every minute, and the run cannot be retried or resumed until it is confirmed.
- Workers hold runs under a lease: a worker that lost its run (re-queued by the sweeper)
  stops itself instead of writing over the new one.
- New run status `paused` in the API, the UI badge and filters, and the status counts.

## Threat model

The Chief reaches the recovery commands through `platform_call` at risk `routine` (no
approval code), as the decisions table says for start, cancel, retry, pause and resume. A
steer injects text into a live agent turn: it is the operator's or the Chief's own text,
redacted like an answer, prefixed "Operator: ", and recorded with its actor and request id.

## Probed versus assumed gateway behaviour

- `hasActiveRun` on the session snapshot: probed (the `session` block of `session.message`
  in `internal/engine/testdata/turn_toolcall.jsonl` and `internal/openclaw/testdata/events.jsonl`).
- `activeRunIds`: probed as present but `null` in those payloads; its array of run ids is
  assumed from [docs] gateway/protocol/rpc-session-control.md.
- That `sessions.list` rows carry `hasActiveRun` / `activeRunIds`: assumed (the probe
  scripts list sessions but record neither field); a session the list does not hold is taken
  as having no active run.
- `chat.history`'s `sessionInfo`: not used (not probed).
- `chat.send` with `queueMode: "steer"` and the gateway's injection at the next tool boundary:
  assumed from [docs] concepts/messages.md and tools/steer.md, not probed; the answer's run id
  is ignored. The `idempotencyKey + ":user"` on the user entry a `chat.send` records is
  probed (2026-10-16) for ordinary sends and assumed for steered ones; the message text is
  matched as well.
- `chat.abort` `{aborted}` and the "aborted" chat event: probed.

## Rules I had to interpret

1. **Pause in a fork.** Rule 14 parks "the visit"; a run with several live branch visits has
   no single visit to park. The pause is rejected there ("the run has parallel branches
   running (<ids>); pause it outside its fork").
2. **Where a cancel counts as confirmed.** Besides the "aborted" event and the session check
   of rule 11, any terminal chat event of the turn (it ended anyway) and `chat.abort`
   answering `aborted: false` (nothing was running) confirm the stop. A session that
   `sessions.list` does not list has no active run.
3. **cancel_state outside cancels.** Any run that ends with a turn not confirmed stopped gets
   `unconfirmed` (not only cancels), so rule 13's "two writers never overlap" holds for a
   timed-out or failed run's retry too; `confirmed` is set for a cancelled run and by the
   reconcile pass.
4. **The sweeper and the epoch.** The sweeper's "worker lost" failure increments the epoch
   too (the Time paragraph calls the sweeper the leases' expiry). A re-queued run with an
   unconfirmed turn is still claimable: rule 13 blocks only retry and resume; the next visit
   aborts the lost turn before its own (as before), and the reconcile pass confirms it.
5. **What "start commit" is for a retry from a step.** Every visit now records `start_sha`
   (the commit its checkout started from). With `from_node`, only the checkout is rewound;
   the files of `runs/<run id>/` follow the carry rules as for any visit. A `from_node` equal
   to the current node is a rewind too (rule 17 applies whenever `from_node` is given); with
   no `from_node` the retry is today's. A visit with no `start_sha` (older than this wave, or
   without a checkout) falls back to the carry rules.
6. **Refusals answer 409.** Rejected commands answer 409 (`command_rejected`, `run_finished`,
   `cancel_unconfirmed`; a retry keeps its own codes, and `from_node` refusals are
   `run_not_retryable`), with the rejected command in `details.command`. A command whose
   worker rejects it later shows only in the command's row and events.
7. **Bodies and receipts.** A body that fails validation (422) is not a command and records
   nothing; a retry's limits are validated by the engine after the receipt, so a bad limit is a
   rejected command.
8. **Paused time.** A pause does not count toward the run's time limit, like a wait for an
   approval (`paused_at` / `paused_total_ms`).
9. **Several resumes.** The continuation's task carries the notice once and every resume's note
   since the step last succeeded or was retried, oldest first.
10. **A steer's next turn.** "The message opens the next turn as its message" is done by
    appending the operator line(s) to that turn's message (the first turn's task, "Continue.",
    or the continue sentence). A turn that ends without a result while a message waits
    continues with it instead of failing "agent did not call finish". A steer that comes after
    the step's result was accepted is rejected.
11. **Resume or pause while a cancel is pending.** Both are rejected, "the run is being
    cancelled" (rule 6: the cancel wins).
12. **The paused visit's question.** A visit paused while it had a question pending closes the
    question ("step paused"), as a visit's end does; the continuation asks again if it needs to.
13. **Actor.** The command's `actor` is the request's actor when the context carries one
    (`internal/actor`), else `{"kind": "operator", "name": <by>}`.
14. **`park_reason` in the contract.** `RunNode.park_reason` (`paused`) is in the contract so
    the UI can say "Paused by the operator." on the paused step.

## Existing tests changed

- `engine.TestTransitionTable`: its expected table gains `paused` (64 pairs), as the
  transition table does (DESIGN W2 rule 14).
- `web` Badge test: one more status in its table.
- `httpapi` fake engine: implements the new `engine.Service` methods and refuses a cancel of a
  finished run as the engine does.
