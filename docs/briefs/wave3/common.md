# Wave 3 — rules common to all four agents

Same rules as `docs/briefs/wave2/common.md` (read it: environment, Docker-only commands,
shared files, conventions, report format), with these changes for wave 3:

- Read first: `README.md`, `DECISIONS.md` (D-001 to D-024), `docs/DESIGN.md` sections
  4, 5.8 to 5.13 (now carrying the full rules), 5.14, and `docs/reviews/wave2-review.md`
  (the deferred items some of you own). Then your brief.
- Ownership this wave (edit nothing outside your set; requests to others go in your
  report):
  - **A**: `internal/channels/**` (new), `internal/engine/**`, `internal/openclaw/**`,
    `internal/store/channels.go` (new), `internal/store/migrations/0006_channels.sql`,
    `internal/httpapi/channels.go` + `channels_test.go` (new), the startup retry in
    `internal/httpapi/server.go` (that function only), `scripts/openclaw-probe-telegram.mjs`.
  - **B**: `internal/sched/**`, `internal/guardrails/**` (new), `internal/memory/**`
    (new, moving what is in `internal/tools/memory.go` only if you must; prefer calling
    it), `internal/store/schedules.go` (new), `internal/store/migrations/0005_schedules.sql`,
    `internal/httpapi/schedules.go`, `skills.go`, `memory.go` + tests (new), the MCP gate's
    guardrail checks in `internal/httpapi/mcp.go` (that file, coordinate nothing else
    there), `internal/tools/memory.go`, `api/openapi.yaml`.
  - **C**: `web/**`.
  - **D**: `internal/evals/**`, `evals/**`.
  - Append-only shared files as before: `internal/store/types.go`, `internal/config/config.go`,
    `.env.example`. `internal/httpapi/server.go` route registration: A adds channel routes,
    B adds schedules/skills/memory routes, each in a clearly separated block; re-read
    before editing.
- The live stack now has Telegram enabled in polling mode with Andre's bot and his user
  id allowlisted. **Do not send Telegram messages, bind agents on the live gateway, or
  restart it**; the coordinator ran the channel probe and its findings are in brief A
  and DESIGN 5.8. Unit tests use the fake gateway.
- Migrations: A `0006_channels.sql`, B `0005_schedules.sql`; nobody else.
