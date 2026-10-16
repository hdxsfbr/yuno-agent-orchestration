# Wave 3 review: findings and dispositions

Independent review of the wave-3 change set by a second model (codex-cli 0.162.1,
reasoning high, read-only `codex exec review --uncommitted` on the integrated tree,
unsandboxed because its sandbox cannot start on this host). Six findings, all kept, none
contradicting a recorded decision; the coordinator added the agents' leftover requests.
One fix pass applied them, then the suites, the live integration tests, the probes,
smoke and `make eval` were re-run before the wave commit.

| # | Severity | Finding | Disposition |
|---|---|---|---|
| 1 | P1 | `internal/sched/scheduler.go`: the run is created by the engine outside the schedule's transaction, so a crash between the two fires that occurrence again on restart (agent B had noted and accepted it). | Fix: an occurrence key, `trigger_ref = "<schedule id>@<occurrence RFC3339>"` with a unique index on `(trigger, trigger_ref)`; a duplicate on re-fire is treated as already fired and the schedule advances. |
| 2 | P1 | `internal/guardrails/tools.go`: a broad host-exec allowlist entry (`*`) survives the pattern filter, so a blocked command runs on the host without an approval and the auto-deny never sees it. | Fix: when an agent has exec patterns, host exec runs with an empty allowlist and `ask: on-miss`, so every host command raises an approval and the pattern denies matches; sandboxed exec is unchanged (D-028). |
| 3 | P2 | `internal/sched/sched.go`: a cron expression with no future occurrence (`0 0 31 2 *`) yields the zero time, stored as year 0001 and fired every tick. | Fix: reject at validation (422 `cron_expr: no future occurrence`) and disable with `last_error` if it ever happens at tick time. |
| 4 | P2 | `internal/store/channels.go`: two concurrent bind requests can both pass the unbound check; the second deletes the first's live binding instead of 409. | Fix: claim the channel atomically (insert with a unique index on `channel`, conflict → 409); delete only a binding whose agent is gone. |
| 5 | P2 | `internal/engine/conversations.go`: history replay after a restart reinserts messages the retention sweep deleted, and reads have no TTL filter. | Fix: apply the agent's conversation TTL when persisting replayed entries and when listing. |
| 6 | P2 | `conversations.go`: a transient `chat.history` failure during the first sync marks the session synced and nothing retries. | Fix: keep an unsynced set the ticker retries independently of the roster subscription. |

Coordinator additions from the agents' reports:

| # | Source | Item | Disposition |
|---|---|---|---|
| 7 | A | OpenAPI lacks the five channel and conversation routes, `conversation.message`, `Approval.conversation_id`. | Fix pass adds them; conformance test extended. |
| 8 | C | `/api/status` may carry `scheduler: {tick_seconds, next_due_at}` for the Status page. | Fix pass adds it. |
| 9 | B | `store.SaveMemoryFact` / `RecallMemoryFacts` are dead after `internal/memory`. | Fix pass removes them with their test. |
| 10 | A | Answering a plugin approval from the chat is unverified (needs a human reply within the window). | Deferred to the demo rehearsal with Andre; README says side-effect decisions come from the web UI. |
| 11 | A | Exec approvals already pending when the engine subscribes (gateway replay) are not stored. | Deferred to wave 4 polish. |
| 12 | first live eval | The runner recorded an empty error text for the 429 the api returned on the rate-limit case. | Fix pass, with a fake-api test. |

Applied by the fix pass (one Opus 5.5 agent, 21 minutes, every fix mutation-checked; the
binding race needed a deterministic interleaving test, the two-goroutine test alone
missed it in 80 rounds). The 429 capture bug was a list-typed `details` field in the
runner's error decoder. Decision taken on its one interpretation: D-029. Deferred as
above: #10 (approval reply from chat) and #11 (pending exec approvals at subscribe time).

