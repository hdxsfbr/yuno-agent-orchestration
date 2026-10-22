# Wave 1 part A: notes for the coordinator

Branch `w1-platform`. What follows is the doc text part A would add, the rules I had to
interpret, and what the merge with parts B and C needs.

## Doc text

### DESIGN 5.15 (chat tools), new rules after the existing ones

- `platform_operations {query?, tag?}` (read_only, chat) searches `api/openapi.yaml`. With
  neither it returns the tags and how many operations each offers. Otherwise it returns at
  most 20 operations, each with its operationId, method, path, summary, risk, path and query
  parameters, request-body schema (`$ref`s resolved two levels deep, less when the answer
  would pass 30 KiB), what it needs (`idempotency_key`, `if_match` with `read_first`), and
  for a gated one that the first call asks a human. `more` says how many matched beyond 20.
  Machine operations are never listed. A dedicated one names its tool in `use_tool`.
- `platform_call {operation_id, path_params?, query?, body?, idempotency_key?, if_match?}`
  (computational, chat, never retried by the platform) builds the request from the contract
  and sends it through `Server.Handler()` in-process, with the server's bearer and the agent
  actor in the context. Refusals before sending: "unknown operation", a missing or unknown
  path parameter, a body for an operation that takes none. The answer is
  `{status, request_id, etag?, idempotent_replayed?, body}` with the body redacted and the
  whole within 32 KiB (a trimmed body becomes text with the truncation marker), a binary body
  `{content_type, bytes, sha256}`; an error status is data. The gate's refusal is a refused
  call with the gate's text; its pending answer is
  `{status: "pending_approval", approval_id, code, expires_at, message}`.
- The platform gate (middleware, B6 order: after auth and the audit, before idempotency)
  holds every request whose context carries an agent actor to the operation's
  `x-yuno-risk`, found from the mux pattern:
  - no operation, or `machine`: 403 `not_available_to_agents`, "not available to agents";
    also `POST /api/runs` with `script_id` or `trigger: eval`;
  - `dedicated`: 403 `use_dedicated_tool`, "use decide_approval" or "use answer_question"
    (`x-yuno-tool`);
  - `read`: passes;
  - otherwise: 428 `idempotency_key_required` without `Idempotency-Key`; 428
    `if_match_required` for PUT, PATCH or DELETE with a GET twin and no `If-Match`; 413
    above 4 MiB; 422 `secret_in_body` for a secret (rule A8);
  - rule A5 raises the risk to privileged with a reason a person reads;
  - destructive and privileged: rule A6 (below). Every outcome is written to the request's
    `actor.Note` (`denied` with the error code, `pending_approval` with the approval id, and
    evidence: operation, risk, reasons, approval id, decided_by, decided_at).
- Platform approvals (kind `platform`, migration 0029): run_id and run_node_id NULL, node_id
  '', `binding = {agent_id, operation_id, path (with query), body_sha256, idempotency_key}`,
  `used_at`, expires one hour after the request. The message names the agent, the operation
  and its summary, the method and path, the person it acts for, why it needs approval, the
  redacted body and a redacted read of the target ("Now:"; none for a POST to a collection).
  A repeat with the same key and body finds the newest live approval of its binding:
  - none: a new one (an advisory lock on the binding makes two first calls share it),
    `approval.requested` is published with no run, the answer is pending;
  - pending and not expired: the same pending answer;
  - approved and unused, not expired: `used_at` is set, then the call passes on;
  - approved and used within the last 24 hours (the idempotency key's lifetime): passes on,
    so idempotency replays; used longer ago: treated as none, so it asks again;
  - rejected: 403 `approval_rejected`, "rejected by <decided_by>";
  - expired (by the sweeper, or found past `expires_at`, pending or approved and unused):
    403 `approval_expired`, "expired; call again to ask again"; the row becomes expired with
    `used_at` set, and the next call asks again.
- `DecideApproval` of a platform approval only records the decision; the sweeper expires a
  pending one with no run to fail. `GET /api/approvals`, `list_approvals` and
  `decide_approval` read and decide them unchanged (the code check of D-081 applies).
- `answer_question` (rule A7): the answer must appear in the person's newest message
  (ignoring case and spacing), or that message must contain the question's code; otherwise
  "relay only what the person said". `run_status` shows the question's `code`.

### DESIGN 5.31 (platform access), or wherever the coordinator puts rule set A

The risk table is `x-yuno-risk` in `api/openapi.yaml`, as PARITY.md plans it.
`TestPlatformParity` holds every route of `server.go` (recorded at registration) to one
documented operation whose operationId is its handler's name; `/healthz`, `/readyz`, `/mcp`
and `/model-proxy/*` need none (the first two are documented anyway, as machine).

### D-097 (draft)

Platform access for the Chief. The Chief reaches every operator action through two generic
tools, `platform_operations` and `platform_call`, which run the API's own handlers
in-process with the agent as the actor, rather than one tool per action. The contract
carries each operation's risk class; a gate in the middleware chain enforces it for agents
only, so the UI and API clients are unchanged. Destructive and privileged calls need a
human approval of the exact call (operation, path, body hash, idempotency key), relayed by
code as decide_approval already is. Rejected: per-action tools (they drift from the API),
and approving by operation without the body (an approval would cover a different action).

### CHANGELOG

- The Chief of Staff can run any operator action of the API from chat (`platform_operations`,
  `platform_call`); destructive and privileged ones wait for your approval of that exact
  call, in the Approvals page or by sending the Chief the code.
- `answer_question` passes an answer on only when your latest message says it, or carries
  the question's code.
- Every API operation has an `operationId` and an `x-yuno-risk` in `api/openapi.yaml`;
  `GET /api/openapi.yaml` is documented.

### Threat model (the Chief's reach)

New: the Chief can call any API route the contract does not mark machine. Mitigations:
machine and dedicated operations are refused (A3); a non-read call needs an idempotency key
and an update or delete of something readable an If-Match (A3); a grant of tools, looser
rules, guardrails or exec policy, a change to the Chief itself, a project's repository,
provider, token or a higher budget, and run limits above the defaults need a human approval
(A5), as does every destructive operation (A6); approvals bind to the exact call and are
decided by the person (UI, or their code in their newest Telegram message, D-081); a body
with a secret is refused and every answer is redacted (A8); `answer_question` relays only
what the person said (A7). The actor context is set only in-process. Residual: with more
than one allowed Telegram user, `on_behalf_of` is the agent's latest conversation (DESIGN A
edge cases); a body larger than 4 MiB is refused by the gate.

### README settings

None new.

## Rules I had to interpret

1. **`/api/openapi.yaml` (A1).** It was registered with the same handler as `/openapi.yaml`
   and not documented. Two routes cannot both have an operationId equal to one handler, so
   `/api/openapi.yaml` got its own one-line handler `apiOpenAPI`, and is documented with
   `security: []` (it is open). `TestOpenAPISecurity` now treats it as open.
2. **Run columns nullable (A6).** They already were (0006, for conversation approvals);
   0029 restates `DROP NOT NULL` (a no-op) and adds `binding` and `used_at`. node_id is ''.
3. **Where A3 is enforced.** In the gate, not the tool, so every refusal reaches the
   request's note (B's audit). The tool refuses only what it cannot build (unknown
   operation, path params, a body for a bodiless operation).
4. **Interaction rules, "adds or loosens" (A5).** Compared per tool as the effective policy
   (DESIGN 5.3 rule 6's default for an absent entry: require_approval for a side-effecting
   tool, auto otherwise). Adding an entry that is not looser than what applied (e.g.
   blocked) is not escalation; removing an entry whose default is looser is.
5. **Guardrail loosening (A5).** A cap (max_cost_usd_per_run, max_runs_per_hour) raised or
   removed, or a blocked tool or input pattern taken away.
6. **Exec policy (beyond A5's list).** A wider security or fallback, a laxer ask, or a new
   allowlist pattern on `PUT /api/agents/{id}` is also privileged: it is a permission grant
   (PRD 6). Remove `loosensExecPolicy` if the coordinator wants A5 to the letter.
7. **`POST /api/agents` with guardrails (A5).** Literal: a non-empty guardrails object is
   privileged, although guardrails restrict.
8. **Budget caps (A5).** Raising includes removing one (absent clears it on update).
9. **Run limits (A5).** Any limit in the request above `store.DefaultLimits`.
10. **Machine fields of `POST /api/runs`** (PARITY row 32): `script_id` or `trigger: eval`
    are refused "not available to agents" by the gate.
11. **Expiry "checked at use" (A6).** Applies to a pending approval and to an approved one
    not yet used: both lapse at `expires_at`. An approved-but-lapsed row becomes expired
    (its decision kept). "call again to ask again" works with the same key: the refusal
    marks the row used, and the next call creates a new approval.
12. **Used approvals and replay (A6).** A used approval lets the same key and body through
    only within 24 hours (B3's key lifetime); after that a repeat asks again, so a call can
    never run unapproved once the idempotency record is gone.
13. **Binding path.** The request path with its query (`RequestURI`), so a query change is
    another action.
14. **Secrets (A8).** Refused: a non-empty string or a number under a secret key, and a
    credential-shaped string (the `obs.Redact` patterns) anywhere. Not refused: null, "",
    booleans, and objects under a secret key (a JSON schema may name a property `token`).
    platform_call drops the `[redacted]` placeholder under a secret key from a body (it can
    only come from a redacted read and means unchanged), so read-then-write of a project
    works; the gate still refuses the placeholder from any other caller.
15. **Answer evidence (A7).** "Appears" is containment, ignoring case and runs of
    whitespace. The newest message is `decide_approval`'s (inbound, no run, not a
    [platform] text). The question's code is `ApprovalCode` of the question id; `run_status`
    now shows it so the Chief can ask for it.
16. **The approval's "before" read.** B2's target through the mux alone (no middleware);
    skipped for a POST to a collection (the list says nothing about what is created).
17. **`platform_call` in a run.** Allowed for an agent whose allowlist has it (via chief,
    on_behalf_of ""); the Chief's runs are rare and the gate is the same.
18. **Search.** All words must appear (case-insensitive) in the id, method, path, summary,
    description or tags; order is the contract's (path, then method).

## For the merge

- `withMiddleware` on this branch: `recoverer → requestID → logging → auth → platformGate →
  mux`. With part B it must read `… auth → actor → audit → platformGate → idempotency →
  If-Match → mux`. The gate only reads `actor.From` and fills `actor.NoteFrom` (nil-safe).
  `platformgate.go` replaces part B's pass-through file.
- TestPlatformCallActor asserts the actor at the handler and the note the gate fills,
  standing in for B's audit row. Once B is merged, add an assertion on the audit row
  (`actor_kind=agent`, `actor_id`, `via=chief`, `on_behalf_of=telegram:<id>`).
- TestPlatformCallRoutine, TestPlatformCallIfMatch, TestDestructiveNeedsApproval and
  TestPrivilegedNeedsApproval run platform_call through B6's chain with fakes of B3 and B4
  that keep to their headers (`Idempotency-Key`, `Idempotency-Replayed`, `ETag`, `If-Match`,
  412 `version_conflict`). After the merge they can use the real chain
  (`platformDispatcher{s: s}`) and drop `fakeIdempotency` and `fakeVersions`.
- B3 stores a 412 from the If-Match layer under the key; an approved call that meets a 412
  needs a new key, so a new approval. That is the safe direction; the Chief's prompt says
  to use a new key for a new action.
- `routes()` keeps its `mux.HandleFunc` lines; `mux` is now a `routeRecorder` that records
  each registration, so B's and C's new routes are recorded without change, and
  `TestPlatformParity` holds them to the contract: each needs `operationId` = its handler and
  `x-yuno-risk` (PARITY: read for the audit, diagnosis and events-page reads).
- The Chief's prompt changed: run `make eval` once (DESIGN wave 1 checks).
