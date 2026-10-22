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
   - A repeat with the same `idempotency_key` and the same body finds the approval: it is
     approved and unused, so the approval is marked used in the same transaction, then the
     call runs. Rejected: refused "rejected by <decided_by>". Expired: refused "expired;
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

## Wave 2: recovery (outline; full rules before it starts)

- **Commands.** `run_commands(id, run_id, kind cancel|pause|resume|retry|steer, node_id,
  payload, actor, received_at, acked_at, applied_at, outcome, detail)`. Every recovery
  route answers 202 `{command}`, and `GET /api/runs/{id}/commands/{cid}` tracks it.
  - Received: the row exists.
  - Acknowledged: the owning worker took it, under its lease.
  - Applied: the effect is confirmed (cancel: the gateway answered "aborted" or reported
    no active turn, and the checkout was committed after).
- **Leases.** `runs.lease_epoch` is incremented at every claim and every re-queue. The
  worker holds it, and every engine write to the run, its nodes, branches, usage and tool
  calls carries `AND lease_epoch = $mine`. Zero rows means the lease is lost, so the worker
  aborts its turn and stops (self-fence). Branches are owned through their run's lease, and
  their rows carry the epoch they were started under.
- **Cancel, then retry.** A cancel that was never confirmed leaves `cancel_state =
  unconfirmed`. Retry answers 409 `cancel_unconfirmed` until a reconcile pass confirms it
  (an abort again, then a session check). There is no force option.
- **Pause and resume.** Pause aborts the turn (confirmed) and parks the visit, reusing
  ParkVisit and RequeueParkedRun. Resume re-queues, and the turn gets "The operator paused
  this step; continue." plus the operator's note.
- **Retry from a step.** `{from_node}` is any node the run visited. Its latest visit's start
  commit is the checkout (D-092), state is kept, and later nodes run again.
- **Steering.** Operator text goes into the live step's session as the next message. It is
  acknowledged when queued and applied when its transcript entry is seen. If the turn ends
  first, it opens the next turn.
- Acceptance: diagnose a stalled step from evidence (the !17 case), then cancel,
  acknowledged, then retry, with no second writer (a stale worker's write is refused).

## Wave 3: dependencies and merge requests (outline)

- **Edges.**
  - `dependencies(dependent_type ticket|merge_request, dependent_id, prerequisite_mr_id,
    created_by)`.
  - Sources: the Planner's ticket `depends_on`, and an operator or Chief `PUT
    /api/merge-requests/{id}/dependencies`.
- **Satisfied** means the prerequisite is merged and the default branch's pipeline for its
  exact `merge_sha` passed. This also fixes `merge_sha` not being stored on sync.
- **Gate.**
  - Intake does not release a dependent ticket.
  - A run on a dependent merge request parks "waiting on !N", reusing the park and resume
    machinery.
  - A dependency watcher, on the MR sync tick, resumes eligible work. No agent polls.
- **Update from default** (platform-only, no agent):
  - Fetch, then merge the default branch into the MR branch in a platform workspace.
  - A conflict is recorded on the MR record and the work stays blocked "conflicts in
    <files>".
  - Otherwise push with the expected head (`git push` refuses if the branch moved). Then
    reset the MR's `ci_status` to pending with the new `head_sha`, which invalidates the
    stale check, and wait for a pipeline on that exact SHA to pass before resuming.
- **MR reads:**
  - provider diff;
  - mergeability (GitLab `detailed_merge_status` and `has_conflicts`, GitHub `mergeable`);
  - exact-SHA pipeline and checks (GitLab `pipelines?sha=`);
  - jobs and job logs (redacted and trimmed);
  - merge with the expected `sha`.
- Acceptance: !24 blocked until !17 is merged with green CI, then automatically updated
  from main, then CI green on the new SHA, then resumed.

## Wave 4: admin parity completion (outline)

- **Rollback:**
  - `GET /api/workflows/{id}/versions` and `POST …/versions/{v}/restore`.
  - `POST /api/audit/{id}/revert` for an agent, schedule, skill or project (tokens are
    never reverted). It goes through the same handlers with `If-Match`.
- Agent restore after archive. Schedules already pause through `enabled` and expose
  `timezone` and `next_run_at`; confirm with tests.
- **Chat acceptance:** create, read, update and delete agents, workflows and schedules from
  chat, with persisted readback (GET after write). Unauthorized actions are refused and
  audited.
- Re-check `PARITY.md` against the code: every row covered with equivalent permissions and
  outcomes.
