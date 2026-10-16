# Wave 2 — rules common to all four agents

You are one of four agents building wave 2 of the Yuno agent orchestration platform in
parallel, in this checkout, on disjoint files. Read first, in this order:

1. `README.md` (what exists, how it runs), `DECISIONS.md` (settled questions; never
   contradict one), `docs/adr/0001-runtime.md` (why OpenClaw executes agents).
2. `docs/DESIGN.md`: section 4 (domain model), 5.1–5.7 and 5.14 (the OpenClaw contract,
   everything marked **[probed]** is verified fact), plus the sections your brief names.
   Rules are numbered; tests are named `T<section>.<n>`. Implement exactly; when a rule
   is ambiguous, pick the reading that keeps the test list true and **report it** under
   "Rules I had to interpret".
3. `internal/engine/contract.go`: the seam between engine, MCP server, HTTP API and
   eval runner. Do not change signatures that exist there; another agent is coding
   against them right now. Adding helpers is fine.
4. `docs/briefs/wave2/<your letter>.md`, your brief.

## Environment rules (non-negotiable)

- Everything runs in Docker. Never install or run a toolchain on the host (no `go`,
  `node`, `pnpm`, `npx`, `pip` on the host). Use:
  - Go tests for one package: `docker compose --profile tools run --rm go go test ./internal/<pkg>/ -count=1`
    (the container's `DATABASE_URL` is the `yuno_test` database; migrations run in tests).
  - `docker compose --profile tools run --rm go go vet ./internal/<pkg>/` and
    `docker compose --profile tools run --rm go gofmt -l ./internal`.
  - Web: `docker compose --profile tools run --rm node pnpm test` and `... pnpm lint`
    (`make test-web`, `make lint` run everything; lint also runs Go vet and staticcheck).
- The live stack (`yuno-api-1`, `yuno-openclaw`, `yuno-web-1`, postgres, deploytarget) is
  in use. Do not run `make run`, `make clean`, `docker compose down/restart/up`, do not
  rebuild or restart its containers, and do not write to `data/`.
- Four agents share this checkout. Edit only the files your brief lists. If you need a
  change elsewhere, write it under "Requests to other agents" in your report with the
  exact code; the coordinator integrates. Shared files and their rule:
  - `internal/store/types.go`, `internal/config/config.go`, `.env.example`: append only,
    at the end of the file or struct, re-read immediately before each edit, keep it small.
  - Migrations: agent A uses `internal/store/migrations/0003_<name>.sql`, agent B
    `0004_<name>.sql`; nobody else adds one. Prefer no migration; the schema in
    `0001_init.sql` and `0002_openclaw.sql` already has `side_effects`, `approvals`,
    `agent_messages`, `spans`, `run_events`.
  - `go.mod`/`go.sum`: do not add Go dependencies. `web/package.json`: agent C only, and
    only if `@xyflow/react` (already installed) is not enough.
- Another agent may be mid-edit in a package you import: a compile error outside your
  files is theirs; retry a minute later and do not fix it. Your own packages must build,
  vet clean and pass their tests before you report.
- Do not commit. Do not touch `README.md`, `DECISIONS.md`, `AI_USAGE.md`; put what they
  need to say in your report.
- UI copy is for operators: no developer notes, stub explanations or placeholders.
- Test fixtures never borrow any part of a real credential or id: no real bot ids,
  tokens, keys or user ids, even with the secret half changed. Invent values that are
  obviously fake and still match the shape the code checks.

## Conventions to match

- Errors: `{error: {code, message, details?}}`; validation failures are 422 with
  `details: [{path, message}]`, path-qualified (`nodes[2].agent_id`). 404 unknown id,
  409 for a decision on a non-pending approval or a cancel of a finished run.
- Tests are table-driven where a rule has cases; the harness is the existing one in each
  package (`internal/engine/main_test.go` fake gateway + scripted turns;
  `internal/httpapi/main_test.go` `newTestServer`/`do`/`fakeEngine`;
  `internal/evals/*_test.go`; `web/src/pages/RunDetailPage.test.tsx` for the fake fetch
  and SSE pattern). Mutation-check your coverage: for each rule, break the code and
  confirm a test fails.
- Logs: `slog` with `run_id`, `node_id`; secrets are redacted by `internal/obs`.
- Never persist or display reasoning text. Platform tool names reach the model as
  `yuno__<name>`; in our tables and API they are plain (`deploy_release`).
- Comments say why, not what. Keep files small; split by concern.

## Platform tool call rows (A and B both read this)

From this wave the MCP handler (agent B) records every platform tool call
(`yuno__*`: finish, deploy_release, http_fetch, memory_*, gitlab_create_mr) as the
authoritative `tool_calls` row: full input and output, status
(`executed | failed | awaiting_approval | rejected | blocked`), `idempotency_key` for
side effects, `tool_use_id` = `mcp-<new id>`, `model_call_id` NULL, and it increments
`runs.tool_call_count` with the existing store counter update. The engine's item-event
recorder (agent A) therefore **skips** OpenClaw items whose tool name starts with
`yuno__`: no row, no count (it may still open and close a span). OpenClaw built-ins
(`exec`, `write`, …) stay recorded by the engine from item events as today.

## Report format

End with: what you built (by file); tests you ran with the exact commands and results;
"Rules I had to interpret"; "Requests to other agents"; "Known gaps"; "Docs to update"
(README, DECISIONS, OpenAPI); any slip of the environment rules.
