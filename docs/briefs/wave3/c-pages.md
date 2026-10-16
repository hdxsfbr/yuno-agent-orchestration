# Agent C — Channels, Schedules, Skills, memory and guardrails pages (Sonnet)

Read `docs/briefs/wave3/common.md` first. Design: DESIGN section 7, 5.8 rules 3–4
(what the channel UI shows), 5.9 (schedules), 5.10 rule 7 (memory list), 5.11 (the
agent form's guardrails), and the UI rules in `/home/hdx/.claude/CLAUDE.md`.

## You own

`web/**` only. Match the existing pages (`WorkflowsPage`, `ApprovalsPage`, the agent
form sections) and the fake-fetch test pattern.

## Backend contract (built in parallel by A and B; mock it in tests)

Shapes are the Go structs in `internal/store/types.go` once A and B append them; until
then use these:
- Channels (A): `GET /api/channels` → `{telegram: {enabled, mode, bot_username, allowed_user_ids: number[], binding: {agent_id, agent_name} | null}}`;
  `PUT /api/channels/telegram/binding {agent_id}` → same object (409 when another agent is bound; `DELETE` unbinds);
  `GET /api/conversations` → `[{id, channel, external_chat_id, agent_id, agent_name, last_message_at, message_count}]`;
  `GET /api/conversations/{id}/messages` → `[{id, direction: in|out, text, run_id?, created_at}]`;
  SSE on `/api/events`: `conversation.message {conversation, message}`.
- Schedules (B): `GET/POST /api/schedules`, `GET/PUT/DELETE /api/schedules/{id}` with
  `{id, workflow_id, kind: cron|delay, cron_expr?, tz, run_at?, enabled, input, next_run_at, last_run_at, last_run_id, last_error, created_at, updated_at}`;
  422 details `cron_expr` / `tz` / `input.<field>`; SSE `schedule.fired {schedule, run}`.
- Skills (B): `GET/POST /api/skills`, `GET/PUT/DELETE /api/skills/{id}` with
  `{id, name, description, body, created_at, updated_at}`; agents carry `skills: string[]` (names).
- Memory (B): `GET /api/agents/{id}/memory` → `[{id, key, value, created_at, expires_at}]`;
  `DELETE /api/agents/{id}/memory/{factId}`.
- Guardrails (exists on `Agent.guardrails`): `{max_cost_usd_per_run, max_runs_per_hour, blocked_tools: string[], blocked_input_patterns: [{tool, pattern}]}`; 422 paths `guardrails.blocked_input_patterns[i].pattern`.

## Build

1. **Channels page** (`/channels`): the Telegram card (enabled/mode, bot username,
   allowlisted user ids, bound agent with a picker to bind/unbind, 409 shown as "another
   agent is bound"); conversations list (agent, chat, last message, count) → transcript
   view (in/out bubbles, time, link to the run when `run_id` is set), live via SSE.
2. **Schedules page** (`/schedules`): list (workflow name, kind, expression or time, tz,
   next run, last run → run link, enabled toggle, last error), create/edit form (workflow
   picker, cron or delay, tz picker with the browser zone as default, input JSON built
   from the workflow's `input_schema` like the run dialog), delete with confirmation.
3. **Skills page** (`/skills`): list, create/edit (name with the pattern hint,
   description, markdown body in a textarea), delete; the agent form gets a skills
   multi-select.
4. **Agent page additions**: a Memory section (facts list with delete, empty state when
   the policy is disabled), the Guardrails section wired to the new 422 paths, a
   "Telegram" line showing bound/unbound with a link to Channels.
5. **Status page**: an OpenClaw panel from `GET /api/openclaw/status` (connected,
   version, scopes, channel enabled) if not already there; a Scheduler line (tick,
   next due) from `/api/status` if B exposes it.
6. Nav entries: Channels, Schedules, Skills.

## Tests

Each page: list renders from the fake API, create/edit sends the right body, 422
details land on the field, SSE updates (conversation message, schedule fired), the
binding 409.

## Report

Per common.md plus the shapes you assumed beyond this brief.
