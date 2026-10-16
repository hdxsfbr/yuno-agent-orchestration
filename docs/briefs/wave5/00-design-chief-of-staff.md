# Wave 5 design: the Chief of Staff

Purpose: one coordinator agent is the only agent a person talks to on Telegram. It knows
the roster, hands tasks to the right agent or workflow as platform runs, and reports the
outcome back into the same chat. The operator still sees every run, trace and approval in
the UI; nothing is delegated outside the platform's runs.

## Inputs

| Input | Source | Read |
|---|---|---|
| The person's message | OpenClaw's Telegram session of the bound agent (native chat, unchanged) | by the gateway |
| The roster | `agents` and `workflows` tables | per tool call |
| The run outcome | `runs` (status, counters, cost, state, failure reason) | on the run's terminal transition |
| The chat identity of a tool call | a per-agent `chat_token` (new column, generated at agent creation, never returned by the API, redacted) written into the agent's `AGENTS.md` chat section by the materialiser | by the MCP gate |

Facts the design rests on (verified 2026-10-16): the gateway's MCP client sends nothing
that identifies the calling agent or session, so identity must travel in the call like
`run_token` does; `chat.send` takes `deliver: true`, which delivers the agent's reply to the
session's channel [to be probed before implementation]; conversations are mirrored per
session key `agent:<id>:telegram:direct:<user>` into `conversations`.

## Rules

1. A platform tool call carrying `chat_token` instead of `run_token` resolves to the agent
   that owns the token; an unknown token answers `invalid chat token` exactly like the run
   token path; a call carrying both is rejected `one of run_token or chat_token`.
2. Only tools marked chat-capable are callable with a chat token: `list_agents`,
   `list_workflows`, `delegate_task`, `run_status`, `memory_save`, `memory_recall`. Any
   other tool answers `<tool> is not available in a conversation`.
3. A chat tool call is recorded in `tool_calls` with `run_id` null and `conversation_id`
   set to the agent's most recent conversation (the one with the latest message); if the
   agent has no conversation the call is still recorded with both null. Inputs and outputs
   are redacted and capped as for run calls.
4. `list_agents` returns every agent except the caller: name, role, model, tool
   allowlist, and whether it is bound to a channel. `list_workflows` returns every
   workflow (not templates): name, description, the names of the agents its nodes use and
   its `input_schema`.
5. `delegate_task` takes exactly one of `agent_name` (with `task`, a non-empty string) or
   `workflow_name` (with `input`, an object validated against the workflow's
   `input_schema`); anything else is a validation error returned to the model. Names are
   matched exactly, case-insensitively; an unknown name answers `no agent named X` /
   `no workflow named X`. An agent cannot delegate to itself (`cannot delegate to yourself`).
6. `delegate_task` triggers a run through the engine's existing Trigger with
   `Trigger = channel` and `TriggerRef = conversation:<conversation id>` (empty when the
   caller has no conversation), default limits, and returns `{run_id, status, url}` where
   `url` is `PUBLIC_BASE_URL/runs/<id>` (or the relative path when unset). Rate limit:
   at most 10 delegations per agent per 10 minutes; the 11th answers
   `delegation limit reached; try later` and is recorded as `blocked`.
7. `delegate_task` is `computational`, policy `auto` by default (the delegated run's own
   side effects stay gated by its agents' policies); an operator may set it to
   `require_approval` through interaction rules like any tool.
8. `run_status` takes `run_id` and returns status, trigger, started and finished times,
   duration, steps, tool calls, cost, the final state (capped at 4 KB), the failure reason
   if any, and the pending approval's kind and message if the run is waiting. Unknown id
   answers `no run <id>`.
9. Report-back: when a run with `Trigger = channel` reaches a terminal status (succeeded,
   failed, cancelled, timed_out), the engine sends one message into the originating
   conversation's session with `chat.send {deliver: true}`:
   `[platform] Run <id> <status> after <duration>, cost $<x>: <outcome>. <url>` where
   outcome is the last node's output (capped at 1 KB) or the failure reason. The agent's
   reply reaches Telegram through the gateway. Exactly once per run (`runs.reported_at`),
   never for runs without a conversation ref, never retried on gateway error beyond one
   attempt (the UI holds the truth).
10. When such a run enters `waiting_approval`, the same path sends one
    `[platform] Run <id> is waiting for an approval: <kind>, <message>. Decide it at <url>`
    per approval (`approvals.reported_at`). Approvals are decided in the UI, not in chat.
11. Messages sent by rule 9 and 10 are mirrored like any inbound message
    (direction `in`) with `run_id` set, so the Channels page links them to the run.
12. The seeded Chief of Staff: name `Chief of Staff`, role `Coordinator`, model
    `claude-opus-5-5`, tools `list_agents, list_workflows, delegate_task, run_status,
    memory_save, memory_recall`, no exec, a system prompt that says: you are the only
    agent the person talks to; never do the work yourself; use list_agents and
    list_workflows before delegating; delegate with a complete, self-contained task;
    tell the person what you delegated and the run id; when a platform report arrives,
    relay the outcome in one or two sentences.
13. Seeding also instantiates both templates (`Dev pipeline`, `Research pipeline`) when no
    workflow carries their template key, so `delegate_task` has workflows to name.
14. When Telegram is enabled and no agent is bound, seeding binds the Chief of Staff.
    An existing binding is never changed by seeding.
15. The materialiser writes a `## Chatting` section into every agent's `AGENTS.md` only
    for agents whose allowlist contains a chat-capable tool: how to pass `chat_token`,
    which tools work in chat, and that run tools are not available there.

## Edge cases

- The chief delegates while a previous delegation is still running: allowed; both report
  back independently.
- The delegated run itself delegates (an agent with `delegate_task` in a run): allowed with
  a run token too (tool is both run- and chat-capable); depth is bounded by the rate limit
  and the run's limits, not by a tree limit (recorded as a known gap).
- The conversation is deleted by retention while the run runs: report-back finds no
  conversation and skips, logged at info.
- Gateway restart between delegation and completion: report-back uses the session key
  stored in the conversation row, which survives restarts.

## Failure handling

- Trigger errors (validation, rate limit, limits) are returned to the model as tool errors
  with the engine's message; nothing is queued.
- Report-back send failure: logged at warn with run and conversation ids, no retry, run
  unaffected.

## Outputs

- `tool_calls` rows (chat calls), `runs` with trigger `channel`, conversation messages
  with `run_id`, log lines `chat tool call`, `delegated run`, `run reported`.
- UI: Runs list shows trigger `channel` with a link to the conversation; the conversation
  transcript shows platform reports with the run link.

## Tests

- Gate: chat token resolves; unknown token; both tokens; non-chat tool with chat token.
- Tools: list_agents excludes caller; list_workflows shapes; delegate_task each
  validation branch, self-delegation, rate limit, trigger_ref; run_status unknown id,
  waiting approval, terminal with state.
- Engine: report-back once per terminal run (fake gateway client), not for manual runs,
  skip when no conversation, approval notice once.
- Seed: chief created, templates instantiated once, bind when unbound, never rebinds.
- Materialiser: chat section present only for chat-capable agents, token redacted in
  logs.
- UI: trigger badge and links.
- Probe (first, before any code): `chat.send` with `deliver: true` on a Telegram session
  delivers the reply to Telegram and the injected text appears as a user turn.

## Out of scope

Deciding approvals from chat; multi-user chats; the chief editing agents or workflows;
delegation trees with depth limits; Slack or WhatsApp.
