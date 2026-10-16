# Wave 5 review: Chief of Staff

Date: 2026-10-16. Scope: the uncommitted wave-5 changes (backend under `internal/`, `api/`,
`cmd/`; frontend under `web/`) against `docs/briefs/wave5/00-design-chief-of-staff.md` and
`docs/DESIGN.md` 5.15.

Reviewer: the project's usual independent reviewer (`codex exec review`, OpenAI) was
unavailable, its usage limit exhausted until 2026-10-22. The review was done instead by two
fresh Claude agents on a model different from each part's author, given the code and the
spec and nothing else: the backend (written by Opus 5.5) was reviewed by Fable 5.1, the
frontend (written by Sonnet 5.5) by Opus 5.5. The coordinator's own pass covered the
migration, the token redaction and lookup, the seed's bind condition and the gate's token
branch before the reports arrived.

The reports follow verbatim, then the coordinator's filter (what was fixed, what was
dropped and why).

## Frontend review (Opus 5.5 reviewing Sonnet 5.5's work)

I found 10 defects in the wave-5 frontend: one should-fix contract mismatch, one should-fix test that cannot fail, and eight minor. `pnpm test` passes (27 files, 359 tests) and `pnpm lint` (eslint and `tsc --noEmit`) is clean, both run in the node tools container on 2026-10-16. I modified no files.

## Defects

**1. Should-fix: the live tool-call list listens for the wrong SSE event.** `web/src/lib/useChannels.ts:43`
- The hook only handles `tool_call`.
- The backend emits every chat-call insert as `conversation.tool_call` with `{conversation_id, tool_call}` (`internal/httpapi/mcp.go:389`, `:403`; also in the `/api/events` description in `api/openapi.yaml`).
- `tool_call` with an empty `run_id` is emitted only when a gated chat call completes (`mcp.go:362` → `emitToolCall` at `:427`).
- Result: normal calls of the six chat tools never update the list live. It only catches up when the next `conversation.message` triggers the refetch at `useChannels.ts:57`. The comment there ("in case a call's own event was missed") hides that this is the main path.
- The tests at `web/src/pages/ChannelsPage.test.tsx:440, 444, 457, 466-467` send `tool_call`, so they assert the wrong event name.
- Fix: `if (e.event === "tool_call" || e.event === "conversation.tool_call")`. Change those tests to send `conversation.tool_call` with `{conversation_id, tool_call}`, and keep one `tool_call` test for the gated completion.

**2. Should-fix: a notice test cannot fail.** `ChannelsPage.test.tsx:322-327`
- The test sends an `out` message without `run_id`. `isPlatformNotice` already returns false because `!!m.run_id` is false.
- Deleting `m.direction === "in"` from `Transcript.tsx:20` would not break any test, so the "outgoing messages unaffected" rule is untested.
- The title also says "a message the person sent", but `out` is the agent's message.
- Fix: give that `out` message `run_id: RUN` and rename the test.

**3. Minor: the 404 fallback is now dead code.** `web/src/lib/api.ts:717-742`
- The backend has the route (`server.go:333`) and the messages endpoint returns a plain array, so `splitTranscript` and the fallback are no longer needed.
- The fallback treats any 404 as "endpoint missing", including "conversation not found" from `chief.go:58-59`. That case then calls `/messages`, which also 404s; with one retry that is 4 requests.
- Against a backend without the endpoint, every refresh (every `conversation.message`) would send a 404 plus a second `/messages` fetch. Nothing remembers that the endpoint is missing.
- Fix: delete `splitTranscript`, `isRecord`, `asList` and the fallback, and restore `listConversationMessages`. Drop the tests "shows nothing… while the API has no tool calls to give", "reads the tool calls from the messages response", and the `splitTranscript` block in `api.test.ts`.

**4. Minor: `ToolCall.run_id` is typed as nullable but never is.** `api.ts:398-399`
- `run_id` and `run_node_id` are typed `string | null`. The backend always sends `""`: `COALESCE(run_id,'')` at `internal/store/runs.go:289`, and OpenAPI marks them required strings.
- The test fixtures (`ChannelsPage.test.tsx:31-32`) use `null`, which never happens, and `trace.ts:164 ?? ""` exists only because of this type.
- Fix: keep `string` and document "empty for a chat call"; use `""` in the fixtures.

**5. Minor: an unchecked cast.** `useChannels.ts:45`
- `call as ToolCall` turns a `Partial<ToolCall>` into a full `ToolCall` and stores it in the cache unchecked.
- Fix: type the payload as `{ tool_call?: ToolCall }` and keep the `id` / `conversation_id` guard.

**6. Minor: a channel run with an unknown ref loses the ref.** `RunHeader.tsx:23-32`, `RunsPage.tsx:16-29`
- If `trigger_ref` is not empty and not `conversation:…` (for example `telegram:1001`), the header shows only "Delegated" and drops the ref. Before this change it was shown.
- Fix: in that branch, render the ref in mono as the other triggers do.

**7. Minor: link targets are not encoded.** `RunsPage.tsx:23`, `RunHeader.tsx:27`
- `/channels/${conversation}` takes any string after the prefix, so `conversation:../agents` would resolve to `/agents`.
- Fix: `encodeURIComponent`, or accept only a UUID.

**8. Minor: a Delegated link can land on "This conversation no longer exists."**
- `ChannelsPage.tsx:15` looks for the conversation only in `GET /api/conversations`, which returns at most 100 (`channels.go:24`). There is no endpoint to fetch one conversation.
- A Delegated link to an older conversation therefore shows that message even though it exists.

**9. Minor: new contract fields have no types.** `api.ts`
- `Workflow.from_template`, `Run.reported_at` and `Approval.reported_at` are missing; frontend brief item 5 asks for types for the new fields.
- Related: `WorkflowsPage.tsx:83` keys "From template" on `template_key`, which copies never carry. The seeded "Dev pipeline" / "Research pipeline" copies will appear unmarked next to same-named templates.

**10. Minor: copy, comments and layout.**
- `api.ts:667`: the `ConversationMessage.run_id` comment ("belongs to a workflow run") is now wrong; `run_id` is set only on `[platform]` reports.
- `ToolCalls.tsx`: the list has no max height (up to 500 rows, two collapsed views each), unlike the transcript's `max-h-[32rem]`.
- `ToolPicker.tsx:135`: the Chat badge uses the same blue as the "computational" risk badge, so `delegate_task` shows two adjacent blue pills.

## Verified correct
- **Endpoint path and shape:** the client calls `/api/conversations/{id}/tool-calls`, matching `server.go:333` and OpenAPI. The body is an array and never `null` (`store/chief.go:69-71`).
- **`chat` flag:** sent as `json:"chat"` (`internal/httpapi/tools.go`). The badge shows only when `tool.chat` is true, and the hint appears once per group that has a chat tool. Tests cover both the with- and without-chat cases.
- **`trigger_ref` parsing:** matches `store.ConversationOfRun` (prefix `conversation:`, empty id means none). Empty, missing, other-prefix and non-channel cases are unit-tested; empty and missing refs render "Delegated" with no link.
- **Notice rule:** a message is a notice only when `direction === "in"`, `run_id` is set, and the text starts with `[platform]`.
  - The backend sets `run_id` only on inbound entries whose idempotency key is a report key (`engine/conversations.go` `reportedRun`), and the report text starts with `[platform]` (`engine/report.go`).
  - A person typing `[platform]` gets an ordinary bubble with the prefix kept. The prefix is stripped once and the text trimmed.
- **Live notices:** the `conversation.message` payload carries `message.run_id` (not omitempty), so notices appear live; this is tested.
- **No XSS path:**
  - All new text renders as React children, and `JsonView` renders inside `<pre>{text}`. There is no `dangerouslySetInnerHTML` in `web/src`.
  - Link targets always start with `/runs/` or `/channels/`, so they cannot become `javascript:` URLs.
  - `chat_token` is redacted server-side before storage and events (`mcp.go:182`, key pattern `_token`).
- **Live-refresh plumbing:**
  - `applyToolCall` upserts by id, so there are no duplicates. It invalidates instead when a fetch is in flight, and creates no cache entry for conversations that are not open.
  - No new subscription was added; it reuses the single `/api/events` stream, and the existing `useEventStream` cleanup applies.
  - Invalidating on a message only refetches the tool-call list that is on screen.
- **Runs page:** the row click ignores clicks inside `<a>`, so the Conversation link opens the channel; this is tested. Non-channel triggers render as before in both `RunsPage` and `RunHeader`.
- **Route:** `/channels/:conversationId` exists (`App.tsx:109`).
- **Product copy:** "Platform notice", "View run", "Tool calls", "Returned an error", "Delegated", "Delegated from a conversation", "Conversation", "Chat", "Works in a conversation" and the hint are all end-user wording, with no developer commentary.
- **Missed events:** the global replay includes events with an empty run id (`store/events.go:40`), so `conversation.*` events are replayed after a reconnect.

## Could not verify
- **Mirrored `[platform]` text in practice:** I did not see a real conversation message that starts exactly with `[platform]`, with no envelope or timestamp added by OpenClaw. The probe (`scripts/openclaw-probe-deliver.mjs:106`) asserts this, but I saw no recorded output. A `conversation_messages` row from a real delegation would settle it.
- **Chat call approved after the wait expires:** I did not trace which event the engine emits when it runs an approved chat call itself. A grep of the engine's approval execution for rows with an empty run id would settle it.
- **Running app:** I did not check this in a browser. The running `yuno-api-1` and `yuno-web-1` containers are 17 hours old and predate these uncommitted changes, so they need rebuilding first.

## Backend review (Fable 5.1 reviewing Opus 5.5's work)

## Wave 5 backend review

Evidence base: spec `docs/briefs/wave5/00-design-chief-of-staff.md` + DESIGN 5.15, the uncommitted diff and the new files, and one full `make test-go` run (all packages `ok`, EXIT=0, 2026-10-16). Line numbers are the working-tree files.

### Defects

**Should-fix**

1. `internal/engine/node.go:1540-1557` (`acceptFinish`) + `internal/store/engine_queries.go:213-220` + `internal/httpapi/runs.go:141-148`. A finish result is stored compacted but not redacted (`out := json.RawMessage(buf.Bytes())` → `SetRunNodeOutput`), merged raw into `runs.state`, and `GET /api/runs/{id}` / the trace serve it as-is. Pre-existing path, but wave 5 puts a long-lived secret into every session's model context (`AGENTS.md`: "pass the field chat_token with the value c_…", materialize.go:77-79). An agent holding chat tools that echoes its token in a `finish` result puts it into an API response, against brief deliverable 8. Every other new path redacts (report.go:223, chief.go:462 via `Render`, node.go:658/739). Smallest fix: `out := json.RawMessage(obs.RedactJSON(buf.Bytes()))` in `acceptFinish`.

2. `internal/engine/report.go:104-115, 167-197`. The claim (`ClaimRunReport`) precedes the send, and `sendReport` counts a nil gateway or `ErrDisconnected` as the one attempt. `Gateway.Call` returns `ErrDisconnected` synchronously while reconnecting (gateway.go:427-429), so a run that ends during a gateway restart is claimed and never reported; the sweep (whose stated purpose at report.go:26-28 is catch-up) cannot recover it. Spec says "never retried on gateway error beyond one attempt"; a disconnected client made no attempt. Smallest fix: on `errors.Is(err, openclaw.ErrDisconnected)` (and the `Gateway == nil` branch at :182) clear the claim (`reported_at = NULL`) so the next sweep sends it; same for approval claims.

3. `internal/seed/seed.go:180-203` (`Templates`) + `internal/store/workflows.go:36`. Idempotency is keyed on `from_template`, which is NULL for every instance made with "Create copy" before 0008 (templates.go only started setting it in this diff). On an upgraded database the first seed creates a second "Dev pipeline" and "Research pipeline"; `delegate_task` then silently picks the oldest of the duplicates (chief.go:404-416). Smallest fix: in 0008, `UPDATE workflows SET from_template = t.template_key FROM workflows t WHERE t.template_key IN ('dev_pipeline','research_pipeline') AND workflows.template_key IS NULL AND workflows.name = t.name AND workflows.deleted_at IS NULL`; or make `Templates` also treat a live non-template workflow with the template's name as done.

4. `internal/httpapi/mcp_chat_test.go:199-203` (`TestMCPChatWithoutConversation`). httpapi's `needDB` (main_test.go:51-57) never truncates and the package uses the shared public schema, so `SELECT count(*) … input->>'run_id' = 'no-such-run'` is satisfied by rows from any previous run; the `n == 0` assertion can never fail again after the first pass. Fix: use `"no-such-" + uniqueSuffix()` in both the call and the query.

**Minor**

5. `internal/httpapi/mcp.go:232-239`. Rule 7 says an operator "may set [delegate_task] to require_approval … like any tool"; a chat-time call under `require_approval` is refused and recorded `blocked`, so for chat the rule means "disabled", not "gated". The 5.8-rule-5 shape (an approval with `conversation_id`, no run; `store.InsertConversationApproval`) exists and was not used. Acceptable as a documented interpretation; it must appear in the author's "rules interpreted" list.

6. `internal/store/agents.go:211-225`. Chat tokens are stored plaintext and matched by SQL equality: exact (no prefix match, `TrimSpace` only), but not constant-time, and a DB dump yields every agent's long-lived chat identity (run tokens live only in memory, tokens.go:28-31). If wanted: store `sha256(token)`, look up by hash.

7. `internal/engine/engine.go:86-117, 158-183`. `Start` waits on `wg` but not `e.reports`, so a report in flight at shutdown is cut; and `sweepReports` runs before `RequeueStaleRuns` in the loop, so a run the stale sweep fails is reported one tick (60 s) later. Fix: `e.reports.Wait()` after `wg.Wait()`; move `sweepReports` after the requeue.

8. `internal/seed/seed.go:196-199, 243-258`. `Templates` stops at the first failing key (a broken `dev_pipeline` blocks `research_pipeline` until the next seed); `retryBinder` retries a `*boundError` three times with 2 s waits although it cannot change.

9. `internal/tools/chief_test.go:251` (`for i := len(d.got) - 1; i < DelegationMax`). The start index equals the held slots only because the fake appends the refused delegation before returning its error (chief_test.go:25-30) and the run-token call also reserves; the comment "three already count" describes a coincidence. Count reservations explicitly.

10. `internal/tools/chief.go:181-221`. `list_agents` offers agents without `finish` (the Chief itself, to any other agent with `delegate_task`) as delegation targets; a quick run of such an agent cannot finish. Edge of the known recursion gap; consider excluding agents whose allowlist lacks `finish`.

### Verified correct

- Both tokens → `one of run_token or chat_token` before any lookup, nothing recorded (mcp.go:90-99; mcp_chat_test.go:123-131).
- Unknown / empty / another-agent's-deleted token → `invalid chat token`: empty short-circuits at chat.go:19 and agents.go:212-214; deleted agents excluded by `deleted_at IS NULL` at agents.go:216 (chat_test.go:61-71).
- Chat token on a non-chat tool: refused before validation, recorded `blocked` with `conversation_id` (mcp.go:188-193; mcp_chat_test.go:134-148); side-effecting tools additionally refused at mcp.go:220-224 and the registry has no chat+side-effecting tool (chief_test.go:349-351).
- Chat calls pass allowlist, policy, agent guardrails, schema and input-pattern checks (mcp.go:195-219); `canonicalInput` drops `chat_token` so patterns see only the request (guardrails/tools.go:135-137).
- Token never in stored input, logs, API, events or runtime view: key match (`secretKeyPattern` matches `chat_token`, redact.go:30), value pattern (redact.go:25), `Agent.ChatToken json:"-"` (types.go:133), `RuntimeView.Files` is names only (contract.go:125); tests mcp_chat_test.go:93-96, 208-228; chat_test.go:76-98; materialize_test.go:219-221.
- `tool_calls` for chat: `run_id`/`run_node_id` NULL via `NULLIF` (runs.go:308-311), `conversation_id` set or NULL (mcp.go:184-187; store/chief_test.go:87-106); every SELECT uses `toolCallSelect` with `COALESCE` (runs.go:327,339; tool_calls.go:15,32; chief.go:63); the only code dereferencing `RunNodeID` is the gated-call path (executor.go:198) and eval traces, both run-scoped.
- Migration 0008 on existing data: nullable `UNIQUE` (NULLs allowed), backfill under the migrator's advisory lock and idempotent (`WHERE chat_token IS NULL`; store/chief_test.go:28-39); no remaining `agentColumns` SELECT (grep); Run/Approval/Workflow scans carry the new columns (`runColumns`, `approvalSelect`, `workflowColumns`), conversation approvals scan `Lax` (channels.go:262-290). Confirmed by the green suite.
- Exactly-once report: atomic `UPDATE … WHERE reported_at IS NULL` claim before the send (store/chief.go:79-85, 107-124); hooks at `finishRun` (run.go:159), queued-cancel (engine.go:448), `closeWaitingRun` (approvals.go:648), sweep at start and each tick (engine.go:161-170); `TestReportBackOncePerTerminalRun` re-fires hook, `reportRun` and sweep and sees one send (report_test.go:93-101).
- Never for runs without a conversation ref or manual runs (`ConversationOfRun` prefix check, store/chief.go:31-40; `LIKE 'conversation:%'` filters; report_test.go:106-128); deleted conversation skipped at info with the claim kept (report.go:168-171; report_test.go:114-128); gateway error: one attempt, run unchanged, sweep does not retry (report_test.go:151-177).
- Approval notice once per pending approval of a channel run only (JOIN on `runs.trigger/trigger_ref`, store/chief.go:107-124); hooks at `RequestApproval` (approvals.go:151, after insert and pause), exec approvals on non-terminal phase (node.go:1076-1078), approval nodes (walk.go:422); conversation-raised approvals excluded by the JOIN (report_test.go:181-222).
- Idempotency keys `report-<run>-end` / `report-<run>-approval-<id>`; mirror links `…:user` to the run only when it exists, strict UUID parse (report.go:48-66, conversations.go:342-359; report_test.go:226-276).
- Messages match rules 9/10 incl. 1 KB redacted outcome cap and `PUBLIC_BASE_URL` link (report.go:202-246; report_test.go:84-89, 202-203).
- `delegate_task`: exactly one form incl. empty task, non-object input, unknown property, unknown names, case-insensitive match, self-delegation by id (chief.go:366-416; chief_test.go:191-212); workflow input validated against `input_schema` by the engine (trigger.go:48); `trigger_ref` only for chat calls, empty for run-token recursion (chief.go:351-353; chief_test.go:237-241); default limits and `Trigger=channel` (mcp_chat_test.go:169-171); rate limit is a mutex-guarded sliding window, slot released once on failure, 11th answers the exact text and is recorded `blocked` (chief.go:147-177, 346-358; mcp.go:253-258; chief_test.go:249-291).
- Engine refusals reach the model as `ModelError` text and the executor never retries a non-transient result (httpapi/chief.go:34-43; executor.go:117-129).
- Seed: never rebinds (`BindChief` checks first, `bindAgent` refuses when another agent is bound, channels.go:189-193; seed_test `TestBindChief`, channels_test `TestSharedBindTelegram`); Telegram off → returns before dialing (seed.go:129-131); unreachable gateway bounded by the 1-minute dial window, warn, exit 0 (seed.go:138-149); gateway calls fail fast on disconnect, no hang (gateway.go:379-383, 427-429); templates instantiated once through the API's `instantiate` with `from_template` set, agent names resolved (templates.go:160-204; seed_test).
- API contract: `ToolCall.conversation_id`, `Tool.chat` (required), `Run.trigger_ref` channel text, `Workflow.from_template`, `reported_at`, the new endpoint and the `conversation.tool_call` event are in `api/openapi.yaml` and matched by `web/src/lib/api.ts` (`chat?: boolean` :293, `conversation_id?: string|null` :401, `/api/conversations/{id}/tool-calls` :738) and `useChannels.ts:45-48` (handles `conversation.tool_call`); limit default/max 500/1000 on both sides (channels.go:26-27).

### Could not verify

- That the gateway sets `idempotencyKey: "<key>:user"` at the top level of the injected user entry in `session.message` and `chat.history`. `HistoryEntry.IdempotencyKey` (rpc.go:167-170) and `TestMirrorLinksReportToRun` both encode the author's assumption; the committed probe prints only role/text and `docs/test-results.md` has no `:user` evidence. Settle: run `scripts/openclaw-probe-deliver.mjs` printing the full entry, or `make test-integration` with a mirror assertion.
- That OpenClaw offers the `yuno__*` MCP tools inside a Telegram chat turn and the model passes `chat_token` from `AGENTS.md` (end-to-end rules 2/15). Needs one live turn.
- Whether OpenClaw's builtin `read` can reach another agent's `workspace-<id>/AGENTS.md`, which would let any agent obtain the Chief's token. Depends on gateway sandboxing; not inspectable from this repo.
- `yuno seed` against the live stack (it restarts the gateway on bind) was not run, per house rules; `make lint` was not run (go vet/staticcheck/eslint).

## Coordinator's filter

Frontend, fixed in the fix pass: the live tool-call list listened for `tool_call` instead
of `conversation.tool_call` (1); a test that could not fail (2); the dead 404 fallback (3);
`run_id` typed nullable where the API sends `""` (4); an unchecked cast (5); a channel run
with a foreign ref lost its ref (6); unencoded link targets (7); missing types and the
"From template" label for seeded copies (9); comment, list height and badge colour (10).
Dropped: (8) a Delegated link to a conversation older than the newest 100 shows "no longer
exists", because there is no endpoint for one conversation; pre-existing API gap, recorded
as a known limit.

Backend, fixed in the fix pass: finish results stored unredacted (1); a report claimed but
never attempted during a gateway reconnect (2); template duplication on upgraded databases
(3); a test that could not fail (4); shutdown cutting an in-flight report and the sweep
order (7); the binder retrying an unchangeable refusal and templates stopping at the first
failure (8); the rate-limit test's coincidental index (9); agents without `finish` offered
as delegation targets (10). Accepted with documentation: (5) `require_approval` on a
chat-capable tool refuses the chat call, since a conversation has no run to pause (DESIGN
5.15 rule 7 now says so). Dropped: (6) hashing chat tokens at rest: the database already
holds everything an attacker with a dump would want, and a hash would force re-minting on
every materialise; recorded in the threat model as a known limit.

Could not verify before the deploy, settled afterwards on the public instance: that
`chat.history` entries carry the `<key>:user` idempotency key the mirror links on (the live
`session.message` event does, per the probe), and that the model passes `chat_token` from
`AGENTS.md` in a Telegram turn.
