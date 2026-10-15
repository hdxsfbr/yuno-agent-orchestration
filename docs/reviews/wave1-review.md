# Wave 1 review: findings and disposition

Independent review of the wave-1 diff by a second model (codex-cli 0.162.1, high
reasoning effort, read-only static pass; its bubblewrap sandbox cannot start on this host
because unprivileged user namespaces are disabled, so it ran with its sandbox off and a
read-only instruction). 25 unique findings. Disposition decided by the coordinator on
2026-10-15 before the fix pass; one finding dropped, 24 fixed in the single fix pass.

| # | Sev | Finding (file) | Disposition |
|---|---|---|---|
| 1 | P1 | Session keys use the first 8 hex of a UUIDv7, a timestamp; two quick runs of one agent inside ~65 s share a session (engine/node.go) | Fix: full run id in the session key, test concurrent runs of one agent |
| 2 | P1 | Workspace writes and stale-skill cleanup follow symlinks an agent can plant in its sandbox workspace (openclaw/materialize.go) | Fix: rooted, symlink-safe file operations (os.Root) for materialisation and cleanup |
| 3 | P1 | Transcript, tool inputs/outputs and error spans are persisted and published unredacted; SSE log message unredacted (engine/node.go) | Fix: redact at the persistence and publish boundary; tests with credentials in each payload |
| 4 | P1 | Setup RPCs (sessions.patch, subscribe, materialise) run on the process context; cancel and deadline cannot interrupt them (engine/node.go) | Fix: bounded contexts tied to run cancellation and remaining duration |
| 5 | P1 | A turn may start although the chat.send response is lost; the engine neither aborts nor reconciles it (engine/node.go) | Fix: record the idempotency key before sending; abort or reconcile ambiguous sends before ending the visit |
| 6 | P1 | API restart mid-turn leaves OpenClaw executing; stale recovery starts a second visit without aborting the first (engine/run.go) | Fix: abort active turns on orderly shutdown; abort the persisted openclaw run id before starting a replacement visit |
| 7 | P1 | Tool calls executed during a disconnect or whose events were dropped never get rows or counters (engine/node.go) | Fix: idempotent reconstruction of calls and results from chat.history, including counters |
| 8 | P1 | Full subscription buffers drop usage and terminal events silently; the engine never checks the drop counter (openclaw/gateway.go) | Fix: per-subscription gap signal; engine reconciles from history as soon as a gap is seen |
| 9 | P1 | A valid JSON fallback on chat.final commits success before usage is reconciled and limits re-checked (engine/node.go) | Fix: reconcile and check limits before fixing the terminal outcome |
| 10 | P1 | Finish-grace abort can win over a concurrent cancellation or deadline (engine/node.go) | Fix: check fresh cancel and time state before choosing success and during the abort wait |
| 11 | P2 | AcceptFinish can race visit closure and overwrite terminal output; concurrent finishes reset grace (engine/node.go) | Fix: serialise acceptance and closure; reject closed visits; first accepted result wins |
| 12 | P2 | Policy not enforced at the MCP boundary for `finish` (httpapi/mcp.go) | Dropped: `finish` is always allowed by design (DESIGN 5.7); server-side policy enforcement arrives with the gated tools in wave 2 |
| 13 | P2 | Rejected finish submissions are not counted toward the three attempts of DESIGN 5.6 rule 7 (engine/engine.go) | Fix: count per visit, including MCP schema rejections, fail the node at the third |
| 14 | P2 | Run-token expiry ignores approval pauses (engine/node.go) | Fix: revoke on visit termination instead of a fixed clock expiry |
| 15 | P2 | max_tokens and effort are accepted but never materialised (openclaw/materialize.go) | Fix: materialise `params.maxTokens`; effort has no OpenClaw equivalent, so remove it from the form and document that `thinking` is the control |
| 16 | P2 | Negative limits inherited from the agent are skipped silently (engine/limits.go) | Fix: validate on agent write and reject at trigger |
| 17 | P2 | Scripted markers are taken from anywhere in any message (model/scripted.go) | Fix: parse the last platform protocol block only |
| 18 | P2 | Registered scripts are never reclaimed (httpapi/scripts.go) | Fix: remove on exhaustion and expire after one hour |
| 19 | P2 | Schema-validation error results bypass redaction and the output cap (tools/tool.go) | Fix: route through the same sanitisation |
| 20 | P2 | Node iteration count never persisted (engine/node.go) | Fix: persist with each recorded step |
| 21 | P2 | Sessions created by sessions.patch before a turn are not cleaned up on early failure (engine/run.go) | Fix: clean up in every exit path |
| 22 | P2 | UI run counters not refreshed on live call events (web/src/lib/trace.ts) | Fix |
| 23 | P2 | UI does not reload the trace for terminal events that arrived while buffering (web/src/lib/useRunTrace.ts) | Fix |
| 24 | P2 | Eval runner does not cancel runs on interrupted or failed polling exits (evals/runner.go) | Fix |
| 25 | P2 | Termination evaluator ignores the abort window (evals/evaluators.go) | Fix: allow the engine's abort wait on top of max_duration |

Integration findings from the coordinator's own runs, fixed before the review:

- OpenClaw resets `agents.ownership` when the roster shrinks to the default agent; the
  materialiser now asserts it on every patch.
- After an api restart OpenClaw keeps the platform MCP server marked unavailable; the api
  now patches the server entry after it starts listening, which hot-reloads the client.
- OpenClaw's Tool Search hides MCP tools behind meta-tools, so a direct `yuno__finish`
  call fails; `tools.toolSearch` is now `false` in the baseline and re-asserted at start.
- The probe reused an agent name whose asynchronous deletion was still pending; it now
  uses a unique name per run.
