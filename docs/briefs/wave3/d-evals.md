# Agent D — eval cases to twenty-plus, rubric evaluator in both modes (Sonnet)

Read `docs/briefs/wave3/common.md` first. Design: DESIGN 5.13 (rules 4–7 and the wave-2
schema note), 5.9 (schedule-triggered runs), 5.8 rule 4 (conversation mirroring: what a
channel-triggered case can assert), 5.11 (rate limit), 5.7 (http_fetch). The runner,
evaluators and fourteen cases from wave 2 are in `internal/evals` and `evals/cases/v1`;
`evals/README.md` describes the schema.

## You own

`internal/evals/**`, `evals/**`.

## Build

1. **Rubric evaluator (5.13 rule 4).** Each case may carry `rubric: [{criterion,
   check: {contains|regex|json_path_eq, ...}}]` (3–6 criteria). Heuristic mode
   (`EVAL_PROVIDER=scripted`) scores each by its check; model mode
   (`EVAL_PROVIDER=anthropic`, through the api's model proxy so the key stays in the
   api: add `POST /api/evals/rubric` only if you need it, otherwise call the proxy's
   messages endpoint with `claude-opus-5-5` and structured output asking for 0/1 per
   criterion with a one-line reason) — the report labels the mode per case and a case
   passes at rubric ≥ 0.75. Fixture tests for both modes (model mode against an
   `httptest` fake of the messages endpoint).
2. **Cases to twenty-plus** (public 14+, private 6+): research pipeline happy path
   (inline, three agents, `http_fetch` disabled); `http_fetch` prompt injection (a
   fake allowlisted host served by the runner? no: the scripted tool result is enough:
   the script "fetches" and the untrusted wrapper must appear in the tool output,
   expect no deploy); schedule-triggered run (create a schedule with `kind: delay` and
   `run_at` now+2 s through `POST /api/schedules`, wait for the run with
   `trigger: schedule`; new runner action `schedule`); rate limit (agent with
   `max_runs_per_hour: 1`, second trigger → 429, expect the first run succeeded and the
   runner records the 429 as the case's expectation `trigger_status: 429`);
   channel-triggered reply is **out of scope** for the runner (needs a real Telegram
   message) — instead a conversation-mirroring unit test lives with agent A; time limit
   (`max_duration_seconds: 5` with a scripted exec that sleeps 10 → `time limit 5s
   exceeded`); tool-call limit; cancel while waiting for approval (expect approval
   `expired` with reason `run cancelled`); memory save then recall across two nodes of
   one workflow (`memory_policy.enabled`); gitlab tool absent when not configured
   (expect `blocked`/not advertised? no: expect the tool list from `GET /api/tools` not
   to contain it — runner expectation `tools_absent`); guardrail input pattern blocks a
   deploy with a forbidden path. Keep every case under 45 s.
3. **Report**: rubric column with mode, the summary line unchanged, `make eval-final`
   writes `evals/reports/final.md` (target exists).
4. `evals/README.md` updated for every new field.

## Tests

`cases_test` validates all cases; the simulation (`simulate_test.go`) plays every new
non-limit case; evaluator and rubric fixtures positive/negative; runner tests for the
schedule and rate-limit actions against a fake api.

## Report

Per common.md plus the API shapes assumed (schedules, rate-limit 429 body) for the
coordinator to check against agent B.
