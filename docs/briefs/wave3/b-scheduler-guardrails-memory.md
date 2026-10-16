# Agent B — scheduler, guardrails, memory, skills API (Opus)

Read `docs/briefs/wave3/common.md` first. Design: DESIGN 5.9 (all rules), 5.10 (rules
1–7; the tools exist, the block and sweeper do not), 5.11 (all rules), 5.2 rule 4/5.11
rule 1 (limits), section 6 (API), 5.14 rule 4 (materialisation of `MEMORY.md`).
Tests to make true: T5.9.1–T5.9.6, T5.10.1–T5.10.5, T5.11.1–T5.11.5.

## What exists

`internal/sched/sched.go` is a stub keeping `robfig/cron` pinned. `memory_facts` table
(0004) and the `memory_save`/`memory_recall` tools (`internal/tools/memory.go`; defaults
now 200/20; TTL default, newest-first order and `created_at` eviction are still missing,
see `docs/reviews/wave2-review.md`). Skills: table, store functions and materialisation
to `skills/<name>/SKILL.md` exist from wave 1; there is no HTTP API or page. Guardrails:
`agents.guardrails` JSONB exists with the agent form section; nothing enforces it.
The materialiser (`internal/openclaw/materialize.go`, agent A's file this wave) writes
`MEMORY.md` as a constant; you provide the function that renders it and A calls it
(put it in `internal/memory`, exported, pure: `RenderMemoryMD(facts) string`; A wires it).

## Build

1. **Scheduler (5.9).** `schedules` table (0005: id, workflow_id, kind cron|delay,
   cron_expr, tz, run_at, enabled, next_run_at, last_run_at, last_run_id, last_error,
   input JSONB, created_at, updated_at). `internal/sched`: `NextCron(expr, tz, now)`,
   a ticker (`SCHEDULER_TICK`, default 10 s) that in one transaction selects due rows
   `FOR UPDATE SKIP LOCKED`, triggers through `engine.Service.Trigger` with
   `Trigger: store.TriggerSchedule`, `TriggerRef: schedule id`, advances or disables,
   records `last_error` on a 422, disables with `workflow deleted` when the workflow is
   gone; no catch-up after downtime; SSE `schedule.fired {schedule, run}`. Started from
   `httpapi.Run` next to the engine (add the call in server.go's startup block). API:
   `GET/POST /api/schedules`, `GET/PUT/DELETE /api/schedules/{id}` (422 for invalid
   cron or tz with the parser's message; `enabled` toggle). Injected clock for tests.
2. **Guardrails (5.11).** `internal/guardrails`: the effective run cost limit is the
   minimum of the workflow limit and every participating agent's
   `max_cost_usd_per_run` (clamped) — expose `EffectiveLimits(def, agents)` and have
   the trigger path use it (A owns `internal/engine/trigger.go`; give A the function
   and the one-line call in your report; until A wires it, test the function);
   `max_runs_per_hour` → 429 `rate limit: <n> runs/hour for agent <name>` at trigger
   (same: function here, call in A's trigger), scheduler records it in `last_error`;
   `blocked_tools` are removed from the agent's tool set (the materialiser already maps
   `interaction_rules` blocked → `tools.deny`; extend the mapping input so A can add
   `blocked_tools`, and the MCP gate refuses them too); `blocked_input_patterns`
   (`[{tool, pattern}]`, RE2) checked in the MCP gate against the canonical JSON input →
   `tool_result{is_error, "input blocked by guardrail <pattern>"}`, row status
   `blocked`; patterns compiled at agent save (422 on a bad regex: `guardrails.
   blocked_input_patterns[i].pattern`). For OpenClaw built-ins the patterns map to the
   exec policy (allowlist) — write what the materialiser must receive and A wires it.
3. **Memory (5.10).** `internal/memory`: TTL default 30 days, `max_facts` eviction by
   `created_at`, recall newest-first (fix the tools to match), the sweeper (every 10
   min, expired facts), `RenderMemoryMD(facts)` (20 newest non-expired agent facts,
   ≤ 4000 chars), `GET /api/agents/{id}/memory`, `DELETE /api/agents/{id}/memory/{factId}`.
   Conversation-scoped facts stay out of scope this wave (Telegram history is the
   conversation memory, agent A).
4. **Skills API.** `GET/POST /api/skills`, `GET/PUT/DELETE /api/skills/{id}` (name
   pattern `^[a-z0-9]+(-[a-z0-9]+)*$`, 422), and the agent's `skills` list validated
   against existing names; a change to a skill re-materialises the agents that use it
   (call `engine.MaterializeAgent` for each; A keeps that method).
5. **OpenAPI** for every new path; conformance test green.

## Tests

T5.9.x with a pinned clock and `America/Sao_Paulo` vs UTC; T5.10.x (expiry before the
sweep, cap evicts oldest, disabled policy offers no tools); T5.11.x (min-of across two
agents, rate limit at n and n−1, bad regex 422, pattern blocks `rm -rf` in an exec
command through the materialiser input, blocked tool absent); API tests for schedules,
skills, memory; OpenAPI.

## Report

Per common.md, plus the exact functions and call sites you need agent A to wire
(trigger-time limits and rate limit, materialiser inputs for blocked tools and patterns,
`RenderMemoryMD`).
