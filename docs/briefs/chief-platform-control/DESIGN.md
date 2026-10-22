# Design: Chief of Staff platform control

PRD: `PRD.md`. Inventory and the per-route plan: `PARITY.md`. The waves are below. Wave 1 is
specified line by line; waves 2 to 4 are specified to the level a brief needs, and each
gets its full rule list before it starts.

Ground truth this design rests on, from the inventory at aeec968:
- The api is one process, and the engine runs inside it (`httpapi/server.go:117-190`).
- Every `/api` route sits behind one middleware chain: recoverer, requestID, logging, auth
  (`middleware.go:16-17`). The UI and scripts use the same bearer.
- The contract `api/openapi.yaml` (embedded as `api.OpenAPI`) describes the routes, and
  `TestOpenAPIConformance` holds the handlers to it. It has no `operationId` yet.
- Chat tools get `Call{AgentID, Chat, ConversationID}`. A conversation's
  `external_chat_id` is the Telegram peer.
- Approvals require a run (`approvals.run_id`, `run_node_id` NOT NULL, `0001_init.sql:136-150`).

## Decisions (Andre, 2026-10-22)

- **What needs Andre's approval code:** deleting anything; granting an agent tools or
  loosening its rules; changing a project's repo or token, or raising a budget; Telegram
  binding; merging a merge request; cancelling a plan. Everything else runs directly
  (start, cancel, retry, pause and resume runs; edit prompts, models, workflows and
  schedules). This is the `x-yuno-risk` table in `PARITY.md` and rule A5.
- **Where dependencies come from:** both. The Planner declares `depends_on` per ticket, and
  Andre or the Chief can add a dependency between any two merge requests (wave 3).

## Wave 1: the Chief sees everything and acts safely

### A. Platform access for the Chief

Purpose: give the Chief every operator action through the same handlers, validation and
business logic the UI uses, with a record of who did what. Read by the Chief (results) and
by Andre (audit).

Inputs:
- The contract `api.OpenAPI`: paths, methods, `operationId`, `x-yuno-risk`, parameters and
  request bodies.
- The tool call: `Call.AgentID` and `Call.ConversationID`, then the conversation's
  `external_chat_id`.
- The arguments: `operation_id`, `path_params`, `query`, `body`, `idempotency_key`,
  `if_match`.

Units: bytes for caps (response text 32 KiB to the model; audit bodies 64 KiB), seconds
for ages, RFC 3339 UTC for times.

Rules:
1. Every route in `server.go` except the machine ones (`/healthz`, `/readyz`, `/mcp`,
   `/model-proxy/*`) has an operation in `api/openapi.yaml`, with a unique `operationId`
   equal to its handler's name and an `x-yuno-risk` of `read`, `routine`, `destructive`,
   `privileged`, `dedicated` or `machine`. `TestPlatformParity` fails on any route without
   one, and on any documented operation with no route.
2. `platform_operations {query?, tag?}` (read_only, chat) returns at most 20 operations:
   `operation_id`, method, path, summary, risk, path and query parameters, and the
   request-body schema with `$ref`s resolved to depth 2. With no query it returns the
   list of tags and their operation counts.
3. `platform_call {operation_id, path_params?, query?, body?, idempotency_key?, if_match?}`
   (computational, chat) builds the request from the contract (unknown operation: refused
   "unknown operation"), and refuses:
   - `machine`: "not available to agents";
   - `dedicated`: "use <tool>", where `<tool>` is `decide_approval` or `answer_question`;
   - a missing `idempotency_key` on any non-`read` operation: "idempotency_key is required:
     a new one per action, the same one when you repeat this action";
   - a missing `if_match` on a PUT, PATCH or DELETE whose path has a GET twin: "read it
     first with <getOperation> and pass its etag as if_match".

   Otherwise it dispatches in-process through `Server.Handler()`, with the server's own
   bearer, so the auth middleware runs unchanged, and with the request context carrying
   the agent actor (rule 4, `internal/actor`). The context cannot be set from the network:
   the middleware sets the operator or anonymous actor only when the context carries none.
4. The actor of a `platform_call` is `agent:<agent id>` with name, `via=chief`, and
   `on_behalf_of=telegram:<external_chat_id>` of `Call.ConversationID` ("" when there is
   no conversation). An HTTP request with a valid bearer is `operator`, with `via=ui` when
   it sends `X-Yuno-Client: web` (the UI sends it) and `via=api` otherwise. A request with
   no valid bearer is `anonymous`.
5. The risk of a call is the operation's `x-yuno-risk`, raised to `privileged` when:
   - `POST /api/agents` sets a non-empty `tool_allowlist`, `interaction_rules` or
     `guardrails`;
   - `PUT /api/agents/{id}` adds a tool to `tool_allowlist`, adds or loosens an
     `interaction_rules` entry, or loosens a guardrail compared with the stored agent;
   - `PUT /api/agents/{id}` or `PUT /api/agents/{id}/files/{name}` targets the calling
     agent itself (no self-modification);
   - `PUT /api/projects/{id}` changes `repo_url`, provider or token, or raises a budget cap
     (daily or monthly), compared with the stored project;
   - `POST /api/runs` sets `limits` above the platform defaults.
6. A `destructive` or `privileged` call runs only with an approved, unused approval bound
   to it:
   - The first call creates an approval: kind `platform`, no run, with a message that
     names the operation, the path, the redacted body and a redacted "before" read of the
     target, plus `{agent_id, operation_id, path, body_sha256, idempotency_key}`. It
     expires after 1 hour.
   - The call returns `{status: "pending_approval", approval_id, code, expires_at}`. The
     Chief relays the code. Andre approves in the UI, or by sending the code in Telegram
     (then `decide_approval`, with its existing check that the code is in the person's
     newest message).
   - A repeat with the same `idempotency_key` and the same body finds the approval. If it
     is approved and unused, it is marked used, then the call runs. If it is approved and
     already used by this same key and body, the call passes to idempotency, which replays. Rejected: refused "rejected by <decided_by>". Expired: refused "expired;
     call again to ask again". Still pending: the same pending answer.
   - `platform_call` never decides an approval. `decide_approval` refuses a `platform`
     approval whose `agent_id` is the deciding agent unless the code is in the person's
     newest message, which is the existing rule; there is no other path.
7. `answer_question` gains the evidence check `decide_approval` has. The answer must
   appear in the person's newest message, or the person's newest message must contain
   the question's code. Otherwise it is refused "relay only what the person said".
8. A body sent through `platform_call` must not contain a secret: a value under a secret
   key (`obs.IsSecretKey`) is refused "secrets are entered by the operator in the UI", so
   the Chief never sets a project token. Responses go back redacted
   (`obs.RedactJSON`), trimmed to 32 KiB with a truncation marker. A binary response
   returns `{content_type, bytes, sha256}`.
9. A `platform_call` result is `{status, request_id, etag?, idempotent_replayed?, body}`.
   A 4xx or 5xx returns the error body as data, not as a tool failure, so the model can
   act on `version_conflict` or `not_pending`.

Edge cases:
- A route the UI never calls, such as `GET /api/projects/{id}`, is still offered (parity
  is with the API).
- Two quick `platform_call`s with different keys for the same create make two objects. The
  key is the model's statement of intent, and a name uniqueness rule still stops duplicate
  names where one exists.
- `POST /api/workflows/validate` is `routine`, but it changes nothing; the key costs
  nothing there.
- An approval that is approved after its call was retried with a new key binds to the old
  key only. The new key asks again, and that is correct.
- With more than one allowed Telegram user, `ConversationID` is the agent's most recent
  conversation (inventory, `engine/chat.go:39`). The actor can then name the wrong person.
  Disclosed; one allowed user today.

Time: the approval expiry is server-clock `now() + 1h`, checked at use.

Failure handling: a failed in-process dispatch (panic recovered, 5xx) returns the status and
body. Approval creation failing refuses the call "could not ask for approval" and is
audited.

Consistency: the approval check, the mark "used" and the dispatch are not one transaction,
because the handler has its own. The approval is marked used first, so a crash between the
two leaves the action not done and the approval spent. The Chief asks again; this is the
safe direction.

Outputs: the tool result; one audit row per call (B); approval rows.

Tests:
- TestPlatformParity: every route documented, with a unique `operationId` equal to the
  handler and a risk; no operation without a route.
- TestPlatformOperationsSearch: by text, by tag, and the empty query.
- TestPlatformCallRead, TestPlatformCallRoutine (key required), TestPlatformCallIfMatch
  (missing is refused, stale gives 412, fresh succeeds).
- TestPlatformCallRefusesMachine, TestPlatformCallRefusesDedicated,
  TestPlatformCallRefusesSecrets.
- TestDestructiveNeedsApproval: pending, then approved by code, then the repeat executes
  once; a second repeat replays (B2) and does not execute again.
- TestPrivilegedNeedsApproval: each rule 5 case, both sides (adding a tool versus removing
  one; raising a cap versus lowering it; own agent versus another).
- TestPlatformApprovalRejectedExpired.
- TestAnswerQuestionNeedsPersonMessage.
- TestPlatformCallBinary.
- TestPlatformCallActor: the audit row names the agent and `telegram:<id>`.

Out of scope: user accounts and roles (one operator), and rotating the chat token.

### B. Audit, idempotency and versions (middleware, for the UI and the Chief alike)

Purpose: every change to the platform is attributable and repeat-safe, whoever makes it.

Rules:
1. **Audit.** Every `/api` request with method POST, PUT, PATCH or DELETE, and every `/api`
   request refused by auth, gets one row in `audit_events`. The row is inserted before the
   handler runs, with outcome `started`, and completed after. If that insert fails, the
   request is refused 503 `audit_unavailable` (fail closed).
   - Columns: `id` (uuidv7), `at`, `request_id`, `actor_kind`, `actor_id`, `actor_name`,
     `via`, `on_behalf_of`, `method`, `route` (the mux pattern), `path`, `target` (a
     resource path or ""), `idempotency_key`, `status`, `outcome`, `error_code`,
     `request` (redacted body, at most 16 KiB), `before`, `after` (redacted, at most 64 KiB
     each), `approval_id`, `evidence` (jsonb), `duration_ms`.
   - `outcome` is one of `ok` (2xx), `denied` (401, 403, 412, or refused by the platform
     gate), `failed` (any other 4xx or 5xx), `pending_approval`, or `replayed`.
2. **Before and after.** `target` is the request path when a GET route with the same
   pattern exists (`PUT /api/agents/{id}`), otherwise the path with its last segment
   removed when that has a GET route (`POST /api/runs/{id}/cancel` targets
   `/api/runs/{id}`), otherwise "". With a target, the middleware reads it in-process
   before and after the handler, and stores both redacted. For a POST to a collection,
   `after` is the response body.
3. **Idempotency.** A mutating request with an `Idempotency-Key` (1 to 200 printable
   ASCII characters) is keyed by actor id, method and path, and stored with the SHA-256 of
   its body.
   - First use: the handler runs. A response below 500 is stored (status, body up to
     1 MiB, Content-Type, ETag, Location); a 5xx is not stored, so the caller can retry.
   - Same key and same body: the stored response is replayed with
     `Idempotency-Replayed: true`, and the audit outcome is `replayed`.
   - Same key and a different body: 422 `idempotency_key_reused`.
   - Same key while the first is still running: 409 `request_in_progress`.
   - Keys expire after 24 hours, swept with the approval sweeper.
   - The UI sends a fresh key per click. Plain API clients may omit the key, and the
     Chief cannot (A3).
4. **Versions.** Every GET 200 JSON response under `/api` carries
   `ETag: "<hex of the first 8 bytes of SHA-256 of the body>"`.
   - A PUT, PATCH or DELETE with `If-Match` takes a per-target lock (in-process), reads
     the target, and compares. On a mismatch it answers 412 `version_conflict`
     `{current_etag}`; on a match the handler runs while holding the lock.
   - Without `If-Match` (the UI today) nothing changes. The Chief must send it (A3).
5. **Audit read.** `GET /api/audit?actor=&target=&outcome=&since=&cursor=&limit=` returns
   rows newest first, with `limit` at most 200 and an opaque cursor.
6. **Middleware order:** recoverer → requestID → logging → auth → actor (sets the operator
   or anonymous actor when the context has none) → audit (creates the `actor.Note`) →
   platform gate (agent actors only: rules A3, A5 and A6; it fills the note) →
   idempotency → If-Match → mux.
   - The gate sits outside idempotency, so a "pending_approval" answer is never stored as
     the key's response.
   - When a later repeat finds its approval already used by the same key and body, the gate
     lets it through, and idempotency replays the stored response.
   - The gate finds the operation from the route pattern (`mux.Handler(r)`) and the
     contract.

Edge cases:
- An SSE or GET request is not audited.
- A 401 storm from scanners writes rows, but only under `/api`. A public instance may see
  some; there is no rate cap yet (disclosed).
- An action route with no GET twin and no parent target records only the request and the
  response.

Failure handling: a failed "complete" update leaves the row `started`, which shows an
incomplete record and is the honest state. A failed before or after read stores
`{"error": ...}` in that column.

Tests:
- TestAuditRowPerMutation (UI, API and Chief).
- TestAuditDenied (401, and the platform gate).
- TestAuditFailClosed.
- TestAuditBeforeAfterRedacted (a token never appears).
- TestIdempotencyReplay, TestIdempotencyMismatch, TestIdempotencyInProgress,
  TestIdempotency5xxNotStored.
- TestETagIfMatch412, TestETagIfMatchOK.
- TestAuditList (paging).

### C. Observability: liveness from evidence

Purpose: say what each live run is doing right now, with the timestamps that prove it. A
"running" status alone proves nothing (PRD 2).

Evidence, persisted per live visit (new `run_nodes` columns, written by the visit
goroutine):
- `heartbeat_at`: the worker's tick, every 30 s, in the same write as `TouchRun`.
- `progress_at`: last progress, meaning a chat or agent event of the turn or a new
  transcript entry. It is written when it changes, at most once per 30 s.
- `waiting_on`: one of `model`, `tool`, `human`, `setup` or "", written when it changes.
  - `model`: the turn is live and no tool call is open.
  - `tool`: a tool call is open.
  - `human`: the visit is waiting on an approval or a question.
  - `setup`: before the first turn.
- `model_wait_since`: when `waiting_on` became `model`; null otherwise.
- `open_calls` (jsonb): `[{id, name, started_at, summary}]`, written when it changes.
  `summary` is the redacted first 200 characters of the command or path.

Thresholds (one table in DESIGN 5.12, all in seconds):

| Name | Value |
|---|---|
| H (heartbeat fresh) | 90 (3 × the 30 s heartbeat) |
| P (progress fresh) | 120 |
| M | `MODEL_STALL_AFTER`, 600 |
| S | `ENGINE_STALE_AFTER`, 300 |
| T (tool overdue) | 1860 (the gateway's 1800 s exec timeout plus 60) |

Rules. Liveness of a live node, evaluated in order at `checked_at = now()`:
1. No `heartbeat_at` → `unknown`, "no heartbeat recorded".
2. `checked_at - heartbeat_at > S` → `stalled`, "worker silent <age>; the sweeper re-queues
   after S".
3. `checked_at - heartbeat_at > H` → `unknown`, "heartbeat <age> old".
4. `waiting_on = human` → `waiting`, naming the pending approval or question and its age.
5. `waiting_on = tool`:
   - the oldest open call is older than T → `stalled`, "<name> running <age>, past its
     timeout";
   - otherwise `executing`, "running <name> <summary> for <age>".
6. `waiting_on = model`:
   - age of `model_wait_since` is at most P → `executing`, "model replying";
   - at most M → `waiting`, "waiting on the model <age>; restarted at M";
   - otherwise `stalled`.
7. `waiting_on = setup` or "":
   - `progress_at` within P → `executing`;
   - `progress_at` older → `unknown`, "no activity for <age>".

Run liveness:
- A terminal run reports its status.
- `queued`: `blocked` with the reason when its project is over budget or its waves are
  paused; otherwise `waiting`, "queued <age>".
- `waiting_approval`: `waiting`, with the approval and its age; a parked visit is the same.
- `running`: the worst of its live nodes, in the order stalled > unknown > blocked >
  waiting > executing.
- `running` with no live node row (between steps): `runs.updated_at` within H →
  `executing`; within S → `unknown`; older → `stalled`.

Missing telemetry never yields `executing` (rules 1, 3 and 7).

Outputs:
- `GET /api/runs/{id}/diagnosis` returns:
  `{run_id, status, liveness, reason, checked_at, thresholds, nodes: [{node_id, visit,
  branch_id, agent, status, liveness, reason, heartbeat_at, progress_at, waiting_on,
  model_wait_since, open_calls: [{name, summary, started_at, age_s}], last_event:
  {seq, event, at}}], pending: [{approval_id, kind, requested_at, age_s}]}`.
- `GET /api/runs/diagnosis?project_id=` returns the same for every non-terminal run, at
  most 100.
- `GET /api/runs/{id}/events/page?after=<seq>&limit=<1..500, default 100>&event=<type,...>`
  returns `{events: [{seq, event, at, data}], next_after}` in ascending sequence order.
  Each `data` goes through `obs.RedactJSON` again before it is returned.

Edge cases:
- A visit parked on a question has no live turn, so its run is `waiting_approval` and the
  run rule applies.
- A fork has one node row per branch visit, and each is classified.
- An api restart leaves `heartbeat_at` aging, so the visit shows `unknown`, then `stalled`.
  That is the truth until the sweeper re-queues it.

Tests:
- TestDiagnosisRules: one case per rule, with both sides of every threshold (H, P, M, S and
  T, at value−1 and value+1).
- TestDiagnosisRun: each run status, worst-of, and between steps.
- TestDiagnosisMissingTelemetry: never `executing`.
- TestEvidenceWritten: an engine run writes `heartbeat_at`, `waiting_on` transitions and
  `open_calls`.
- TestRunEventsPage: paging, the event filter, redaction, and the 500 cap.

Out of scope for W1: worker leases (W2) and dependency blocking (W3).

### Wave 1 docs and checks

- DESIGN 5.12: thresholds table and liveness rules.
- DESIGN 5.15: chat tools `platform_operations`, `platform_call` and the risk gate.
- DESIGN 5.2, a new 5.31 "Audit, idempotency and versions".
- One DECISIONS entry per part (D-097 platform access, D-098 audit, idempotency and
  versions, D-099 liveness).
- CHANGELOG, threat model (new: the Chief's reach; mitigations rules A3 to A8 and B1),
  README settings.
- The Chief's prompt: when to use `platform_call` versus the purpose-built tools, and the
  key and etag rules.
- `make ci-test` and `ci-lint` once locally. Run `make eval` once, because the Chief's
  prompt changes.

## Wave 2: recovery

Purpose: let the operator or the Chief stop, hold, restart and redirect a run. Each request
is tracked from receipt to applied outcome, and two workers never write one run.

Inputs:
- The recovery routes.
- The run row, with a new `lease_epoch`.
- The visit's live turn (`openclaw_run_id`, session key).
- The gateway's answers: `chat.abort` `{aborted}`; `sessions.list` / `chat.history`
  `sessionInfo.hasActiveRun` and `activeRunIds` (authoritative, [docs]
  gateway/protocol/rpc-session-control.md); and `chat.send` with
  `queueMode: "steer"`, which injects into an active run at the next tool boundary
  ([docs] concepts/messages.md, tools/steer.md).

Units: times are server-clock UTC; message lengths are characters.

Rules:

Commands
1. Table `run_commands(id uuidv7, run_id, kind, node_id, payload jsonb, actor jsonb,
   request_id, received_at, acked_at, applied_at, outcome, detail)`.
   - `kind` is one of cancel, pause, resume, retry or steer.
   - `outcome` is one of pending, applied, rejected, failed or superseded.
2. The routes are `POST /api/runs/{id}/cancel`, `/pause`, `/resume`, `/retry` (body
   `{from_node?, limits?}`) and `/steer` (body `{node_id?, message}`, 1 to 4000
   characters). Each inserts a command (receipt) and answers 202 with the run and
   `command`. The existing cancel and retry keep their paths and run body, and gain the
   `command` field.
3. `GET /api/runs/{id}/commands` (newest first, at most 100) and
   `GET /api/runs/{id}/commands/{cid}`.
4. **Acknowledged** means the worker holding the run's lease took the command (`acked_at`).
   **Applied** means the effect is confirmed (`applied_at`, outcome `applied`).
   **Rejected** means the command is not valid in the run's state when it is processed;
   `detail` says why.
5. Delivery:
   - A run with a live worker: the API inserts the row and wakes the worker in-process; the
     worker also reads its pending commands on every 2 s tick.
   - A queued, parked or paused run: no worker holds it, so the API applies the command
     itself in the same transaction (`acked_at = applied_at = now()`) wherever the rule
     below allows it.
6. Commands for one run are processed in `received_at` order. A cancel supersedes every
   still-pending pause, resume or steer received before it.

Leases
7. `runs.lease_epoch` (bigint, default 0) is incremented by every claim and by every move to
   queued: claim, stale re-queue, retry, resume, and re-queue of a parked run. The worker
   keeps the epoch it claimed with.
8. These writes carry `AND lease_epoch = $epoch` (for node and branch rows, through their
   run):
   - TransitionRun, TouchRun (the heartbeat renews the lease), SetRunCurrentNode,
     SetRunStateNode and AddRunUsage;
   - run_nodes progress, finish, park and evidence writes;
   - SetBranchStatus and SetBranchCurrent;
   - the checkout commit's `repo_sha` write.

   Tool-call and model-call rows are records of what happened and are not fenced.
9. A fenced write that touches 0 rows returns `ErrLeaseLost`. The visit then self-fences:
   - it aborts its turn (`chat.abort`), writes nothing more to the run, and ends;
   - it logs and emits `log` "worker lost its lease; stopped" through the event recorder,
     which is not fenced.

Cancel
10. On a queued run, cancel is applied at once (`cancelled`).
11. On a live run, the worker acknowledges and aborts the turn.
    - Applied when the gateway's "aborted" chat event arrives, or when `sessions.list`
      shows no active run for the session (`hasActiveRun = false`, or the turn's id not
      in `activeRunIds`). The checkout commit is saved after that, then the run goes
      `cancelled` with `runs.cancel_state = confirmed`.
    - If neither happens within `abortWait` (10 s): the run goes `cancelled` with
      `cancel_state = unconfirmed`, and the command stays acknowledged but not applied.
    - A reconcile pass on the 60 s sweeper tick repeats `chat.abort` and the session check
      for every `unconfirmed` run until the gateway shows no active run. It then sets
      `confirmed` and the command `applied`.
12. A run the sweeper fails as "worker lost", or re-queues, gets `cancel_state =
    unconfirmed` for its last turn. That turn may still run, so the same reconcile pass
    applies.
13. Retry (and resume) answer 409 `cancel_unconfirmed` while `cancel_state = unconfirmed`,
    with `detail` naming the session and the turn. There is no force option. The reconcile
    pass is the only way out, so two writers never overlap.

Pause and resume
14. Pause on a live run: the worker acknowledges, aborts the turn (confirmed as in rule 11;
    unconfirmed leaves the command `failed`, "could not confirm the turn stopped", and the
    run keeps going), saves the checkout, parks the visit with reason `paused`, and sets
    the run status `paused` (a new non-terminal status; the worker is released).
    - Pause on a queued run: `paused` at once.
    - Pause on `waiting_approval`: rejected, "the run is waiting on <approval>".
15. Resume on a paused run: re-queued (lease epoch + 1). The step starts a continuation
    visit from the parked visit's checkout, as a parked question does. Its task gains
    "The operator paused this step and resumed it." plus the note from the resume body
    (`{note?}`, at most 2000 characters).
    - Resume on any other status: rejected "the run is not paused".

Retry from a step
16. Retry `{from_node}`:
    - It must be a node with at least one visit in this run, outside a fork branch.
      Otherwise it is rejected: "from_node must be a step this run reached" or "retry the
      fork step instead".
    - With no `from_node`: today's behaviour (the current node).
17. The retry sets `current_node = from_node`, keeps the run state, and marks
    `retried_at` (D-093) on the latest visit of `from_node` and of every node visited
    after it.
    - The retried step starts from the start commit of `from_node`'s latest visit (a fresh
      attempt).
    - Later nodes run again as the graph reaches them.
    - Every other retry precondition stands (terminal status, at most 5 retries, limits,
      budget, the live-run conflict checks, rule 13).

Steering
18. Steer on a run with exactly one live visit (or `node_id` naming one):
    - The worker acknowledges and sends `chat.send` with `queueMode: "steer"`, message
      "Operator: <message>", and idempotency key `<visit id>:steer:<command id>`. The
      visit's turn id is not changed.
    - Applied when a transcript user entry containing the message appears. If the turn
      ends first, the message opens the next turn as its message, and that is applied when
      the turn starts.
    - No live visit: rejected "nothing is running; retry or resume with a note". Several
      live visits and no `node_id`: rejected "name the step: <ids>".

Edge cases:
- A cancel that races a finishing turn: the run may end succeeded first. The command is
  then rejected "the run already ended (succeeded)".
- A resume while a cancel is pending: the cancel wins (rule 6).
- A steer during a gated tool's approval wait is injected at the next boundary, after the
  tool returns.

Time: abortWait is 10 s; the reconcile pass runs on the 60 s sweeper tick; leases have no
time-to-live of their own (the stale sweeper, `ENGINE_STALE_AFTER`, is the expiry, and it
increments the epoch).

Failure handling:
- A gateway that is down during cancel leaves `unconfirmed` (rule 11), never `confirmed`.
- A failed command write returns 503 and the request is not taken.
- An `ErrLeaseLost` in a fork branch stops that branch's visit only; the run-level writer
  is the new lease holder.

Consistency: the lease check and the write are one statement. The API's inline application
(rule 5) and the claim are serialised by the run row lock (`FOR UPDATE`).

Outputs:
- Command rows.
- Events `run.command` `{command}` on every state change.
- Diagnosis (C) gains `commands_pending` and `cancel_state`.
- The Chief reaches it all through `platform_call` (risk `routine`).

Tests:
- TestCommandLifecycle (received, acknowledged, applied).
- TestCancelQueued, TestCancelLiveConfirmedByEvent, TestCancelLiveConfirmedBySession,
  TestCancelUnconfirmedThenReconciled.
- TestRetryRefusedUnconfirmedCancel.
- TestSweeperLeavesUnconfirmed.
- TestLeaseFencing (a write after a re-queue is refused; the old visit self-fences and
  does not end the run).
- TestBranchFencing.
- TestPauseResume, TestPauseOnWaitingRejected, TestPauseUnconfirmedFails.
- TestRetryFromStep (checkout from that visit's start commit; later nodes re-run),
  TestRetryFromStepRejected.
- TestSteerLive, TestSteerBetweenTurns, TestSteerRejected.
- TestCancelSupersedes.
- **Acceptance, TestStalledStepRecovery:** the fake gateway hangs a turn; the diagnosis
  says stalled with evidence; cancel is acknowledged, aborted and applied; retry starts a
  new lease; the hung visit's late write is refused.

Out of scope: checkpoints inside a visit (one commit per visit end stays); multi-process
workers (one api process; the lease makes a second one safe but it is not deployed).

## Wave 3: dependencies and merge requests

Purpose: dependent work waits for its prerequisite to be merged and green on the exact
commit, is brought up to date with the default branch by the platform, and resumes by
itself. No agent spends turns polling.

Inputs:
- `merge_requests` rows and provider reads:
  - GitLab: MR fields `detailed_merge_status`, `has_conflicts`, `sha`;
    `pipelines?sha=<sha>&ref=<branch>`; pipeline jobs; job trace; MR diffs; merge with
    `sha`.
  - GitHub: PR `mergeable`, `mergeable_state`, `head.sha`; check-runs and the combined
    status for a SHA; Actions job logs; PR files; merge with `sha`.
- The Planner's ticket `depends_on`.
- The dependency routes.
- The project's token, used for the platform's own git push.

Units: SHAs are full 40-character hex, compared exactly. Sizes are bytes. Times are UTC.

Rules:

Edges
1. Table `dependencies(id, project_id, dependent_kind ticket|merge_request,
   dependent_id, prerequisite_kind ticket|merge_request, prerequisite_id, source
   planner|operator|chief, created_by jsonb, created_at)`.
   - Unique on (dependent, prerequisite).
   - A self-edge is refused, as is an edge that would close a cycle: "would make a cycle:
     <path>".
2. **Planner:** the `publish_tickets` ticket input gains `depends_on: [ticket id]`. Each id
   must be a ticket of the same plan in the same or an earlier wave; otherwise it is
   refused: "T9 depends on T12, which is in a later wave". Edges are stored ticket to
   ticket.
3. **Operator and Chief:** `GET` and `PUT /api/merge-requests/{id}/dependencies`
   `{prerequisites: [merge request id]}` replace that MR's operator-set prerequisites
   (risk routine). Planner edges are listed and are not editable there.

Satisfied
4. A merge-request prerequisite is satisfied when all of these hold:
   - its state is `merged`;
   - its `merge_sha` is known (this wave persists `merge_sha` from the sync, today's bug);
   - the default branch's CI for exactly `merge_sha` has passed (GitLab: a pipeline with
     that `sha` on the default ref has status `success`; GitHub: every check-run and the
     combined status for that SHA are success, neutral or skipped).

   If the project has no CI (no pipeline or check for the default branch in its last 20
   commits) and none appears for `merge_sha` within 15 minutes of the merge, the
   prerequisite is satisfied with the note "no CI configured".

   A ticket prerequisite is satisfied when its merge request is. A ticket closed without a
   merge never is: the dependent is blocked "prerequisite T9 was closed without merging",
   and the operator is told.
5. Unsatisfied reasons, in order: "!17 is not merged", "!17 merged; CI on <sha7> is
   <pending|running|failed>", "!17 merged; the merge commit is not known yet".

Gate
6. **Tickets:** a released wave's ticket with an unsatisfied prerequisite is not labelled
   for intake. The wave watcher labels it when it is satisfied. A new ticket run clones the
   default branch, so it needs no update.
7. **Runs on a merge request:** a run that would work on a dependent open merge request
   (request changes, `delegate_task` with `merge_request`, a retry of such a run) is
   created in status `paused` with park reason `dependency` and the reason text, and no
   worker is used.
   - Operator or Chief resume of such a run is refused, "blocked by <reason>; remove the
     dependency to override".
   - The dependency is removed through rule 3 (operator or Chief edges) or a plan revision
     (Planner edges).
8. **The dependency watcher** runs on the merge-request sync tick, and right after a sync
   that changed a prerequisite's state or CI. For every blocked dependent whose
   prerequisites are all satisfied:
   - **(a) An open dependent merge request:** update from the default branch (rules
     9-11), then wait for CI on the new head (rule 12), then re-queue its paused
     dependency runs with the task note "Its prerequisite !17 is merged; the platform
     merged <default> into this branch (<sha7>) and CI passed."
   - **(b) An unreleased ticket:** label it (rule 6).

Update from the default branch (the platform alone; no agent)
9. `POST /api/merge-requests/{id}/update-from-default {expected_head_sha}` (routine) and
   the watcher use one function:
   - Fetch the branch and the default branch into a throwaway bare repository, with the
     project's token.
   - If the remote branch head differs from `expected_head_sha` (the watcher passes the
     record's `head_sha`), answer 409 `head_moved` `{current}`.
   - Merge the default branch into the branch:
     - Already contained: outcome `up_to_date`.
     - Conflicts: outcome `conflicts`, recorded on the merge request (`conflicts: {files,
       at, base_sha}`). Nothing is pushed, and the dependent stays blocked "conflicts with
       <default> in <files>".
     - Otherwise: a two-parent merge commit, authored by the platform.
10. The push is not forced: `<sha>:refs/heads/<branch>`, so a branch that moved since the
    fetch is refused by the provider. That is reported as `head_moved`, and the next
    watcher pass starts again from the new head.
11. After a successful push the record is updated in one write, which invalidates the stale
    check:
    - `head_sha = <new sha>`;
    - `ci_status = pending`, `ci_sha = <new sha>`, `ci_pipeline_id = null`;
    - `updated_from_default_at = now()`.

    An event `merge_request.updated` is emitted.
12. Dependent work resumes only when CI for exactly `head_sha` has passed. A failure keeps
    it blocked: "CI failed on <sha7>: <pipeline url>". The operator is told once per SHA
    (a run-report style message to the Chief's conversation).

Merge-request reads and the merge guard
13. The merge-request record gains `ci_sha`, `ci_pipeline_id`, `ci_url`, `mergeable`
    (bool, null when unknown), `merge_status` (the provider's word), `conflicts` (jsonb),
    `updated_from_default_at` and `merge_sha` (now read and written).
    - The sync reads GitLab CI for exactly the head SHA (`pipelines?sha=`), not "the newest
      MR pipeline".
14. New reads (risk read):
    - `GET /api/merge-requests/{id}/diff`: provider files and patches, redacted with
      `obs.Redact`, at most 256 KiB, then `truncated: true`.
    - `GET /api/merge-requests/{id}/checks`: the pipeline or check-runs for `head_sha`, with
      jobs `{id, name, stage, status, url}`.
    - `GET /api/merge-requests/{id}/jobs/{job}/log?tail_bytes=` (default 64 KiB, at most
      1 MiB): the job's log tail, redacted.
15. Merge (the existing route, risk destructive):
    - It requires `ci_status = passed` with `ci_sha = head_sha` (`none` is accepted only
      under rule 4's "no CI configured").
    - It sends the provider the expected `sha`, so the provider refuses a head that moved.
    - It answers 409 `ci_not_passed` or `head_moved`.

Edge cases:
- A prerequisite merged before the dependency was declared is evaluated on the next
  watcher pass and may be satisfied at once.
- A dependent merge request whose branch already contains the prerequisite's merge commit
  is `up_to_date`, and waits only for CI on its current head.
- A dependent merge request that is closed or merged meanwhile: its edges stay for history
  and nothing resumes.
- Two dependents of one prerequisite are updated one after the other, each with its own
  expected head.

Time: the 15-minute "no CI configured" window counts from `merged_at`. The watcher runs at
the sync interval (120 s); rules 8 and 12 are not faster than one sync.

Failure handling: a provider error during a watcher pass is recorded in
`last_sync_error`, the dependent stays blocked, and the next pass tries again. One
dependent failing does not stop the others.

Consistency: rule 11's single write keeps the head and the check together. The watcher
reads the record fresh before each update.

Outputs: the reason text on the paused run, the diagnosis (`blocked`) and the merge-request
record; events; one operator message per failed SHA or per conflict.

Tests:
- TestDependencyEdges (unique, self-edge, cycle, Planner later-wave refused).
- TestPrerequisiteSatisfied (each rule 4 clause on both sides; the no-CI window at 14 and
  16 minutes).
- TestTicketNotReleasedUntilSatisfied.
- TestRunOnDependentParks, TestResumeRefusedWhileBlocked.
- TestUpdateFromDefault (up to date, clean push, conflict recorded, head moved before the
  fetch, push refused because the head moved).
- TestStaleCheckInvalidated.
- TestResumeAfterCIOnNewSHA, TestCIFailedKeepsBlocked.
- TestMergeSHAPersistedOnSync.
- TestMergeNeedsExactSHA.
- TestMergeRequestDiffChecksLog (redaction, caps).
- **Acceptance, TestDependencyAcceptance** (the fake forge): !24 depends on !17; a run on
  !24 parks; !17 merges and the default pipeline passes on its `merge_sha`; the watcher
  updates !24 from main (push), `ci_status` goes pending, then the fake pipeline passes on
  the new SHA; the run resumes with the note.

Out of scope: forge webhooks (the sync tick is the trigger); rebasing (merge only, as D-088);
required-check policies beyond "all checks for the SHA".

## Wave 4: admin parity completion

Purpose: everything an operator configures can be changed from chat, read back, and rolled
back, with the same permissions as the UI.

Inputs:
- The routes;
- `audit_events.before` and `after` (wave 1 B);
- `runs.workflow_snapshot` (the only place old workflow versions survive today);
- soft-deleted agents (`deleted_at`).

Rules:
1. Table `workflow_versions(workflow_id, version, name, description, definition jsonb,
   created_at, created_by jsonb)`, primary key `(workflow_id, version)`.
   - Workflow create and every update insert the new version, in the same transaction as
     the workflow write.
   - The migration backfills each workflow's current version and every distinct
     `(workflow_id, workflow_version, workflow_snapshot)` held by a run, with
     `created_by = {"kind":"backfill"}`.
2. `GET /api/workflows/{id}/versions` returns versions newest first, without definitions;
   `GET /api/workflows/{id}/versions/{v}` returns one with its definition (risk read).
3. `POST /api/workflows/{id}/versions/{v}/restore {expected_version}` (risk routine):
   - It validates version v's definition as a PUT would (DAG, agents present, schemas).
     If it is invalid now, 422 with the validation errors.
   - It writes the definition as a new version, current + 1.
   - It answers 412 `version_conflict` when `expected_version` is not the current version.
   - Runs in flight keep their snapshot, as they already do.
4. `POST /api/agents/{id}/restore` undoes an archive: it clears `deleted_at` and
   re-materialises.
   - 409 `name_taken` when a live agent has the name.
   - Risk is `privileged` when the agent's tool allowlist is not empty (rule A5: restoring
     grants tools), and `routine` otherwise.
   - `GET /api/agents?include_archived=true` lists archived agents with `archived_at`.
5. `POST /api/audit/{id}/revert` (risk: that of the reverted operation, evaluated on the
   revert's body by rule A5):
   - Allowed for an audit row with outcome `ok`, method PUT, a target that is an agent,
     schedule, skill or project, and a `before` value.
   - It maps `before` to the route's input, with each resource's mapper: agent input
     fields; schedule fields; skill fields; project fields **without** token fields
     (tokens are never reverted; a change to the token alone answers 422 "nothing to
     revert").
   - It applies the result through the same handler with `If-Match` = the ETag of the
     row's `after`. If the resource changed since, 412: "changed again since; revert the
     later change first".
   - Workflows use rule 3.
6. Schedules already pause and resume through `enabled`, and expose `timezone` and
   `next_run_at`. Tests confirm each of these through `platform_call`.
7. **Chat acceptance, end to end** (`platform_call` through the tools and MCP wiring
   against the real handlers):
   - create, read, update and delete an agent, a workflow and a schedule, each followed by
     a GET that shows the change (persisted readback);
   - a privileged change without approval is refused and audited `denied` with the
     actor;
   - an anonymous request is refused 401 and audited;
   - a revert restores an agent's prompt;
   - a workflow restore pins in-flight runs.
8. `PARITY.md` is re-checked against the code, and every row's "Acceptance" test exists.
   `TestPlatformParity` covers the routes added in waves 1 to 4.

Edge cases:
- A revert of a create is not offered: delete is the undo, and it is destructive and needs
  approval.
- A revert of a delete is the restore route, for agents only. Workflows, schedules and skills
  are recreated from their audit `before` with a POST: offered as "recreate" with the old
  body, as a new id. Disclosed.

Tests:
- TestWorkflowVersionsBackfill, TestWorkflowVersionsWritten, TestWorkflowRestore (valid,
  invalid now, version conflict, run pinned).
- TestAgentRestore (name taken; tools make it privileged).
- TestAuditRevert (agent, schedule, project without token; changed since; nothing to
  revert).
- TestChiefAgentCRUD, TestChiefWorkflowCRUD, TestChiefScheduleCRUD.
- TestUnauthorizedAudited.

Out of scope: user accounts and roles; rollback of skills' files (a skill revert restores its
metadata; files are edited one by one).
