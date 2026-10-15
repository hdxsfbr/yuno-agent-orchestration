# Agent B — gated tools, idempotency, MCP gate, approvals and workflows API (Opus)

Read `docs/briefs/wave2/common.md` first. Design: DESIGN 5.3 (rules 5–8, 5a is verified
fact), 5.4, 5.5 rule 4, 5.6 rules 9 and 11 (API side), 5.7, 5.10 rules 3–4 (the two
memory tools only), 5.12 rule 4, section 6. Tests to make true: T5.3.1 (policy), T5.3.3–
T5.3.6 (handler side, with a fake engine), T5.4.1–T5.4.6, T5.5.6–T5.5.7, T5.7.1–T5.7.8,
T5.6.1 and T5.6.10 (API side).

## You own

`internal/tools/**`, `internal/httpapi/**` (including `server.go` wiring and
`main_test.go`), `api/openapi.yaml`, `internal/deploytarget/**`, new files
`internal/store/side_effects.go`, `internal/store/tool_calls.go`,
`internal/store/approvals.go` (move nothing; add what you need),
`internal/store/migrations/0004_*.sql` if you need one. Append-only:
`internal/store/types.go`, `internal/config/config.go`, `.env.example`.

The engine implements `RequestApproval`, `WaitApproval`, `DecideApproval` (contract.go)
in parallel; until it lands they return `engine.ErrNotImplemented`. Code and test
against the contract with `fakeEngine`.

## Build

1. **Tools (5.7 table).** In `internal/tools`, each with schema, risk, hits, timeout,
   tests:
   - `deploy_release {run_token, version, notes, paths[]}`: packages
     `<workspace>/runs/<run_id>/<path>` (workspace = `<cfg.OpenClawStateDir>/workspace-<openclaw agent id>`,
     from `RunContext`; paths validated with `internal/pathcheck`, ≤ `DeployTargetMaxMB`),
     `POST <DEPLOYTARGET_URL>/releases` with `Idempotency-Key`; result `{release_id,
     version, url, replayed?}`. Read `internal/deploytarget/server.go` for the contract
     it enforces (T5.4.4 is there; add the test if missing).
   - `gitlab_create_mr {run_token, title, description, paths[]}`: registered only when
     `GITLAB_BASE_URL`, `GITLAB_PROJECT_ID`, `GITLAB_TOKEN` are set; branch
     `yuno/<first 12 hex of the idempotency key>`, commit the files through the
     repository commits API, open the MR; a second call with the same key finds the
     existing branch/MR and replays. Test against an `httptest` fake GitLab.
   - `http_fetch {run_token, url}`: https only, host in `HTTP_FETCH_ALLOWLIST`, ≤ 1 MiB,
     ≤ 3 redirects each on-list, 15 s; output wrapped
     `<untrusted_content source="http_fetch">…</untrusted_content>` with any literal
     closing tag inside escaped (T5.7.4).
   - `memory_save {run_token, key, value, ttl_days?}` and `memory_recall {run_token,
     query}` per 5.10 rules 3–4 on a `memory_facts` table (add it in `0004_memory.sql`
     with the columns of DESIGN section 4; scope `agent` only this wave).
   - `finish` stays; move its row recording into the common handler path (item 3).
2. **Idempotent executor (5.4).** `idempotency_key = sha256(run_id || node_visit_id ||
   tool_name || canonical_json(input))`, hex; canonical JSON = sorted keys, no
   whitespace. `store/side_effects.go`: insert-or-replay in one transaction with
   `SELECT … FOR UPDATE`, result written after the external call succeeds, key deleted on
   failure; replay returns the stored result and marks `output.replayed = true`.
   Retries per 5.5 rule 4 (transient classification by the tool). Implement
   `engine.SideEffectExecutor` (`ExecuteApproved(toolCallID)`): load the row, rebuild the
   call context (run, node visit, agent → workspace), execute under the same key, update
   the row; wire it into `engine.Deps.Effects` in `server.go`.
3. **MCP gate (`mcp.go`, 5.3 rules 5–8, 5.7 rule 3).** For every call: `run_token` →
   `engine.ResolveRunToken` (else result `invalid run token`); not in the agent's
   `ToolAllowlist` or `interaction_rules[tool] == blocked` → `tool <name> is blocked for
   this agent`, row status `blocked`; policy = `interaction_rules[tool]` else
   `require_approval` for `side_effecting` else `auto`. Record the `tool_calls` row
   first (see common.md; `awaiting_approval` with the key for gated calls), then: gated
   → `RequestApproval` → `WaitApproval` with `cfg.MCPToolWait`; approved → execute →
   `executed`, result to the model; rejected or expired → `{"status":"rejected",
   "reason":…}` (not an error); wait ended → `{"status":"pending_approval",
   "approval_id":…}` and the row stays `awaiting_approval` for the engine's fallback.
   `auto` side effects execute under the key and the row's output carries
   `"policy":"auto"` (T5.3.6). Output capped at `ToolOutputMaxBytes` with the marker
   and redacted before storage.
4. **HTTP API + OpenAPI (section 6, 5.6 rules 9 and 11).**
   - `GET /api/approvals?status=&run_id=&limit=` → `[{...Approval, tool_call?:
     ToolCall}]` (the tool call joined when `tool_call_id` is set, so the inbox shows
     name and input); `POST /api/approvals/{id}/approve {reason?, always?}` and
     `POST /api/approvals/{id}/reject {reason?}` → `Approval`; 404 unknown, 409
     `ErrNotPending`; `decided_by` is the API principal (`api` or the bearer's name if
     you have one).
   - Workflows: `GET/POST /api/workflows`, `GET/PUT/DELETE /api/workflows/{id}`
     (`PUT` bumps `version`), `POST /api/workflows/validate {definition}` → 200
     `{valid: true}` or 422 with one detail per message (`workflow.ValidateStructure`
     plus: every `agent_id` exists, `agent_name` only allowed on template rows, task
     templates render against empty input and state, `input_schema`/`state_schema`/
     `output_schema` compile). Soft delete.
   - Templates: load `templates/*.json` at api start into `workflows` rows with
     `template_key` (update in place when the file changed; log each);
     `GET /api/workflow-templates` → `[{key, name, description, definition}]`;
     `POST /api/workflow-templates/{key}/instantiate {name?}` → new `Workflow` with
     every `agent_name` resolved case-insensitively to an agent id, 422
     `nodes[i].agent_name: no agent named 'X'` otherwise (T5.6.10).
   - `GET /api/tools` already lists tools; make sure the new ones appear with risk.
   - `GET /api/runs/{id}/trace` gains `side_effects[]` (store.SideEffect) for the run.
   - OpenAPI: every new path and schema, and `TestOpenAPIConformance` stays green.
5. **Agent validation.** `tool_allowlist` and `interaction_rules` may name the new
   tools; `interaction_rules` values `auto | require_approval | blocked`; setting a
   side-effecting tool to `auto` is allowed (D-009).

## Tests

Handler tests with `fakeEngine` extended to script `RequestApproval`/`WaitApproval`
outcomes (approved in time, rejected, expired, wait ended); tool tests with counting
fakes for the deploy target and GitLab; T5.4.x on the real `yuno_test` database;
T5.7.x; API tests for approvals (incl. 409 and 404), workflows CRUD and validation
messages, templates load and instantiate (incl. the missing-agent 422); OpenAPI
conformance.

## Report

Per common.md. Include the exact JSON of the approvals list item, the instantiate
response, and the three MCP tool results (`rejected`, `pending_approval`, replayed) so
agents C and D match them.
