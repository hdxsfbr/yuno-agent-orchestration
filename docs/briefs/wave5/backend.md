# Wave 5 backend brief: Chief of Staff (chat tools, delegation, report-back, seed)

Spec: `docs/briefs/wave5/00-design-chief-of-staff.md` and `docs/DESIGN.md` 5.15. Implement
exactly; report every rule you had to interpret. Ground truth for conventions: the
existing gate (`internal/httpapi/mcp.go`), tools (`internal/tools/*.go`, note `Tool`,
`Call`, `Result`, the registry in `platform.go`, policy in `policy.go`), engine
(`internal/engine/{engine.go,trigger.go,approvals.go,conversations.go,tokens.go,run.go}`),
runtime contract (`internal/openclaw/{materialize.go,rpc.go,client.go}`; the probe
`scripts/openclaw-probe-deliver.mjs` shows the exact `chat.send` call that works),
store (`internal/store/*.go`, migrations `0001`-`0007`), seed (`internal/seed/seed.go`),
config (`internal/config/config.go`, with the test that checks `.env.example` and
`compose.yaml` cover it).

## Deliverables

1. Migration `0008_chief.sql`: `agents.chat_token TEXT UNIQUE` (filled for existing rows
   by a one-off backfill in Go at start, or generated lazily on first materialise; choose
   one and say which), `runs.reported_at TIMESTAMPTZ NULL`, `approvals.reported_at
   TIMESTAMPTZ NULL`, `tool_calls.run_id` nullable plus `tool_calls.conversation_id TEXT
   NULL REFERENCES conversations(id) ON DELETE SET NULL` with an index. Store methods for
   each new column; `ListToolCallsByConversation`.
2. Gate (`mcp.go`): the chat-token path per rules 1 to 3. The caller resolution for chat
   calls is a new engine method `ResolveChatToken(token) (ChatContext, bool)` returning
   agent id, OpenClaw agent id, workspace and the most recent conversation id. Chat calls
   go through the same validation, policy, guardrail and recording code as run calls
   where it applies; no idempotency key for chat calls (none of the chat tools is
   side-effecting). A chat call to a non-chat tool is refused before validation.
3. Tools (`internal/tools/chief.go` or one file per tool, your call): `list_agents`,
   `list_workflows`, `delegate_task`, `run_status` with JSON schemas, descriptions the
   model can act on, `Chat: true` on the `Tool` struct (also set it on `memory_save` and
   `memory_recall`), and the registry entries for `GET /api/tools` with a `chat` field.
   The tools need store and engine access: add a small interface the executor passes in,
   as the memory tools do. `delegate_task` calls the engine's `Trigger` with
   `Trigger = store.TriggerChannel` and the trigger ref; the rate limit is in-memory per
   agent with a sliding 10-minute window (document that a restart resets it).
   `url` is `PUBLIC_BASE_URL + "/runs/" + id` when `PUBLIC_BASE_URL` is set, else
   `/runs/<id>`.
4. Report-back (`internal/engine/report.go`): on every terminal transition of a run whose
   trigger is `channel` and whose ref names a conversation, and on entering
   `waiting_approval` for such a run, compose the message per rules 9 and 10 and send it
   with `chat.send {sessionKey, agentId, message, idempotencyKey: "report-<run>-<kind>",
   deliver: true}` to the conversation's session key (stored on the conversation row).
   Set `reported_at` in the same transaction as the status change where possible, else
   immediately after; one attempt; log `run reported` / `run report failed`. The mirror
   must store the injected message with `run_id` set: the message id arrives in the
   `session.message` event (the probe shows `message.idempotencyKey` is
   `<idempotencyKey>:user`); match on that.
5. Materialiser: `## Chatting` section in `AGENTS.md` per rule 15, naming the tools that
   work in chat and how to pass `chat_token`; the token is part of the desired-state
   hash so a rotated token re-materialises.
6. Seed: the Chief of Staff per rule 12 (prompt text in the design brief), template
   instantiation per rule 13 (reuse the API's instantiation code), binding per rule 14
   (reuse the channels handler's bind code; the gateway restart it triggers is fine at
   seed time).
7. OpenAPI (`api/openapi.yaml`): `tool_calls` gain `conversation_id`, tools gain `chat`,
   runs' `trigger` enum gains `channel`; `GET /api/conversations/{id}/tool-calls` or
   include chat tool calls in the conversation's messages endpoint, your call, say which.
8. Redaction: `chat_token` joins the redacted names in `internal/obs`; it must never
   appear in logs, traces, API responses or the UI; test it like `run_token`.
9. Tests, all under `make test-go`: the list in the design brief's Tests section, with
   a fake gateway client for report-back (the engine already has one for tests), a fake
   clock for the rate limit, and the seed's bind/no-rebind cases. Keep
   `TestEnvExampleAndComposeCoverConfig` green if you add config.
10. `make lint` clean (`go vet`, staticcheck, gofmt).

## Rules of the house

- Everything runs in Docker through the Makefile: `make test-go`, `make lint`,
  `make test-integration` only if you need it (serial). Never install toolchains on the
  host. Never restart or rebuild the running stack, never patch bindings on the live
  gateway, never send Telegram messages, never print or commit secrets. Test fixtures
  never borrow any part of a real credential or id.
- Do not touch `web/` (a second agent owns it). The contract between you: the API
  fields named in deliverable 7, and `GET /api/tools` entries carrying `chat: true`.
- Match the existing style: 422 details with JSON pointers, `store` structs with JSON
  tags, `DESIGN x.y rule n` comments on the code that implements a rule.
- Report at the end: files changed, the rules you interpreted and how, anything you
  could not verify.
