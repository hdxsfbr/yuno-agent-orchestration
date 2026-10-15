# Wave 2 review: findings and dispositions

Independent review of the wave-2 change set by a second model (codex-cli 0.162.1,
reasoning high, read-only run of `codex exec review --uncommitted` on the integrated
tree, unsandboxed because its sandbox cannot start on this host). Eight findings, all
kept; the coordinator added three cross-agent items from the agents' own reports. One fix
pass applied them, then the suite, the live integration tests, smoke, the probes and
`make eval` were re-run before the wave commit.

| # | Severity | Finding | Disposition |
|---|---|---|---|
| 1 | P1 | `internal/tools/package.go`: `os.OpenRoot` on `runs/<run_id>` follows a symlink an agent could plant, so the deploy tools can package files outside the workspace. | Fix: open the workspace root, then the run directory through it; reject escaping symlinks (reproduced by the reviewer). |
| 2 | P1 | `internal/engine/walk.go`: consecutive nodes with different agents start without the earlier agent's run files; the shipped pipeline's Reviewer cannot read the Coder's work. | Fix: copy the run directory forward before each visit (DESIGN 5.6 rule 13, D-019; agent D found the same gap). |
| 3 | P1 | `walk.go`: on recovery after a crash between advancing `current_node` and creating the new gate visit, a looped-back approval node reuses the previous visit's decision. | Fix: bind the resumed approval to the visit being resumed; a newly entered gate always asks. |
| 4 | P1 | `walk.go`: a decision arriving between `approval.requested` and the pause finds the run still `running`, so the requeue matches nothing and the run pauses for ever. | Fix: pause before publishing, or reconcile an already-decided approval right after pausing. |
| 5 | P2 | `internal/engine/approvals.go`: a tool approval decided between `RequestApproval` and `WaitApproval` returns the decided row without unblocking the visit, so the run stays `waiting_approval` while the call proceeds. | Fix: the fallback path unblocks like the waiter path. |
| 6 | P2 | `web/src/lib/workflow.ts`: renaming a node updates edges but not condition paths or template references to its state. | Fix: rewrite condition paths and `.state.<id>` references on rename, and say so in the editor. |
| 7 | P2 | `internal/workflow/cond.go`: `{"all": []}` validates and evaluates true, but `omitempty` drops it on the way through the snapshot, where it evaluates false. | Fix: reject empty `all`/`any` groups at validation with a path-qualified message, so the two paths agree. |
| 8 | P2 | `internal/engine/approvals_test.go`: a goroutine still uses the old engine when the test swaps it; `go test -race` reports it. | Fix: join the caller before replacing the engine. |

Cross-agent items from the agents' reports, added to the same fix pass:

| # | Source | Item | Disposition |
|---|---|---|---|
| 9 | B | Memory tool defaults (100 facts, 10 recalled) differ from DESIGN 5.10 rules 3–4 (200, 20). | Fix: follow the design. |
| 10 | D | `make eval` ran a stale eval image. | Fixed by the coordinator: the target now builds before running. |
| 11 | A, B, C, D | Documentation each agent asked for. | Coordinator: README, DECISIONS D-019 to D-024, DESIGN 5.1/5.6/5.13, this file. |

Applied by the fix pass (one Opus 5.5 agent, 24 minutes, every fix mutation-checked; the
engine package clean under `-race`). Deferred, recorded here so they are not lost: the
memory tools still ignore the 30-day default TTL, recall best-match-first rather than
newest-first and evict by `updated_at` (DESIGN 5.10 rules 3–4; wave 3 owns memory); the
`os.SameFile` and `O_EXCL` guards in packaging and copying have no deterministic test; a
run that passed a gate before the fix keeps a stale resume pointer until its next node
move.

Found by the coordinator while re-running the eval after the fix pass, outside the
review: `docker compose run --build` on the eval service rebuilt the gateway image and
recreated the gateway container, which killed the api (it shares that container's
network namespace), and the api then gave up on the gateway's "starting; retry shortly"
answer after its dial window instead of retrying with backoff (DESIGN 5.5 rule 1 says
indefinitely). The eval target now builds only its own image and runs without touching
dependencies; the api's startup retry is a wave-3 item.

