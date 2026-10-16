# Agent A — Telegram on OpenClaw's native channel, conversations, api startup retry (Opus)

Read `docs/briefs/wave3/common.md` first. Design: DESIGN 5.8 (rules 1–7; rule 3 and
rule 7 carry probed facts from 2026-10-16), 5.3 (approvals from chat), 5.14, section 4
(`conversations`, `conversation_messages`, `channel_bindings`, `channel_updates`),
section 6, D-013, D-008, D-025. Tests to make true: T5.8.2–T5.8.5, plus the ones below.

## You own

`internal/channels/**` (new), `internal/engine/**`, `internal/openclaw/**`,
`internal/store/channels.go`, `internal/store/migrations/0006_channels.sql`,
`internal/httpapi/channels.go` + test, the gateway-dial retry in
`internal/httpapi/server.go` (that function only; agent B edits other parts of the
file, re-read before each edit), `scripts/openclaw-probe-telegram.mjs`.

## Verified facts (do not re-derive; build on them)

- The live stack has `channels.telegram` enabled in polling mode (baseline from
  `TELEGRAM_*` env), `dmPolicy: allowlist`, one allowlisted user, exec approvals
  `target: dm`. The gateway log shows `[telegram] Inbound message telegram:<user> ->
  @bot (direct, N chars)` and `outbound send ok ... chatId=<user> messageId=N`.
- With no binding, DMs are routed to OpenClaw's default agent `main` on the default
  model and answered (session `agent:main:main`, `createdVia: "channel"`, `origin:
  {provider: "telegram", from: "telegram:<user>", to: "telegram:<user>", chatType:
  "direct"}`, `channel: "telegram"`, `displayName: "<name> (@handle) id:<user>"`). D-025
  says this must never happen: implement the mechanism and test it (see item 1).
- Binding schema and reload behaviour: DESIGN 5.8 rule 3. A `bindings` patch is not
  hot-reloaded. The bundle exposes RPC methods `config.apply`, `gateway.reload`,
  `gateway.reload.mode`, `gateway.restart`, `gateway.restart.request`,
  `gateway.restart.safe`, `gateway.restart.preflight`: probe which of them applies a
  bindings change without recreating the container (a process restart inside the
  container keeps the api's shared network namespace alive; the api must then
  reconnect, which the client already does with backoff). Record the answer in
  DESIGN 5.8 rule 3 and the probe script.
- `sessions.messages.subscribe` and `plugin.approval.request` refuse an `agentId` that
  does not match the session key's agent. With `session.dmScope: "per-channel-peer"`
  the DM session key has a per-channel-peer suffix (verify the exact format with
  `sessions.list`; the probe prints it).
- The current live binding routes Telegram to the Coder (`agb79ae974ba`), kept from the
  coordinator's probe; the human has not yet sent a message since the restart. The
  moment one arrives, `scripts/openclaw-probe-telegram.mjs --no-bind` shows the routing;
  you may run it with `--no-bind` only (never patch bindings on the live gateway, never
  send messages). If no message arrives during your work, say so; the coordinator
  verifies with Andre.

## Build

1. **Bindings (5.8 rule 3, D-025).** `channel_bindings` table (0006: id, channel,
   agent_id, external_id (bot username), config JSONB, created_at). `PUT
   /api/channels/telegram/binding {agent_id}` (409 when another agent is bound),
   `DELETE` unbinds, `GET /api/channels` → `{telegram: {enabled, mode, bot_username,
   allowed_user_ids, binding}}` (bot username from the gateway's channel status or
   `getMe` through the gateway, never the token). Writing a binding patches
   `bindings[]` with `session.dmScope: "per-channel-peer"` and applies it with the RPC
   you verified; the api also re-asserts bindings from the table at every start (after
   the baseline), so a gateway restart never loses them. D-025 mechanism: when no
   platform agent is bound, the channel must not answer from `main`: prefer keeping
   `channels.telegram.enabled` false until a binding exists if that is hot-reloadable;
   otherwise give `main` a model that cannot answer (e.g. an unknown provider ref so the
   turn fails fast and is logged) — probe, pick, record in DESIGN 5.8 rule 7, test with
   the fake gateway.
2. **Conversations (5.8 rule 4).** Tables `conversations` (id, channel,
   external_chat_id, agent_id, session_key, display_name, created_at, last_message_at)
   and `conversation_messages` (id, conversation_id, direction in|out,
   external_message_id, text, run_id NULL, created_at). The engine subscribes to the
   session roster (`sessions.subscribe` → `sessions.changed` events; the probe shows the
   payload: `sessionKey`, `agentId`, `phase: "message"`, `channel`, `origin`) and to
   each Telegram session of a bound agent (`sessions.messages.subscribe` with that
   agent's id), mirroring `session.message` entries (user → in, assistant → out, with
   usage) into the tables, idempotently by `messageId`; SSE `conversation.message
   {conversation, message}`. API: `GET /api/conversations`, `GET
   /api/conversations/{id}/messages`. Conversation turns run in OpenClaw outside the
   platform's run engine (they are the agent's own session, not a workflow run);
   record them as conversations, not runs, and say so in the UI copy agent C writes.
3. **Approvals from chat (5.8 rule 5, 5.3).** Exec approvals raised by a Telegram turn
   reach the approver's DM (baseline `execApprovals.target: dm`) and are mirrored by the
   existing `session.approval` path once the engine subscribes to that session (today
   it subscribes only to run sessions: extend to bound agents' channel sessions,
   `run_id` NULL → make `approvals.run_id` nullable in 0006 and handle NULL in the
   inbox and trace code paths you own; agent C shows "conversation" instead of a run
   link). Plugin approvals: verify with `plugin.approval.request {agentId, sessionKey}`
   on a Telegram session whether `deliveryRoute` becomes the chat and the human's reply
   resolves it (`resolvedBy`); the probe has the call. Record the result; if it does
   not route to chat, the README says decisions for side effects come from the web UI
   (DESIGN 5.3 rule 5 last sentence stands).
4. **Api startup retry.** `httpapi.Run` exits when the gateway answers `connect
   rejected: gateway starting; retry shortly` past the dial window; per DESIGN 5.5
   rule 1 it must keep retrying with backoff (1, 2, 4 … 30 s) indefinitely until the
   context ends, logging at `warn` every attempt and at `info` on success. Test with
   the fake gateway refusing N times.
5. **Guardrail and limit wiring from agent B.** B provides `guardrails.EffectiveLimits`
   and the rate-limit check and asks you for two call sites in `internal/engine/trigger.go`
   and materialiser inputs for `blocked_tools` and exec patterns; wire them when B's
   report lands (the coordinator forwards it), behind tests.
6. **`MEMORY.md` rendering from B's `RenderMemoryMD`** in the materialiser before each
   turn of an agent with memory enabled (5.10 implementation note).

## Tests

Fake gateway: binding patch + apply RPC; re-assert at start; D-025 mechanism; roster
and session mirroring from fixture events (T5.8.4); exec approval from a channel
session with `run_id` NULL; token absent from any log line when the config is fetched
(T5.8.5); baseline renders `channels.telegram` per mode (T5.8.2, exists? extend);
startup retry; API tests for channels and conversations.

## Report

Per common.md, plus every gateway fact you verified (RPC names, payload shapes, the
session key format, whether plugin approvals reach the chat) so the coordinator can
record them in DESIGN 5.8.
