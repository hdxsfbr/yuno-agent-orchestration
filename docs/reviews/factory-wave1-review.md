# Delivery process, wave 1 review (DESIGN 5.28)

One independent reviewer (Claude Opus 5.5), read-only, 2026-10-19. Three high, eight
medium, ten low. Kept for the fix pass unless noted.

| # | Sev | Finding | Disposition |
|---|---|---|---|
| H1 | high | The seed deletes operator copies of retired templates (renamed or edited), then their agents | fix: delete only an untouched shipped copy; keep and log the rest |
| H2 | high | Intake or a schedule pointing at a retired workflow silently stops | fix: keep any retired workflow an intake setting or enabled schedule references; log it |
| H3 | high | `max_steps` 40 and `max_node_iterations` 12 stop real plan and ticket runs | fix: 150 steps, 30 iterations |
| M1 | medium | An agent with a run in flight (quick run) is retired mid-run | fix: skip agents with activity |
| M2 | medium | A rejection without a reason reruns the node with no notice | fix: notice on `decision == rejected`, reason when present |
| M3 | medium | The plan MR commits everything left in the checkout, not the four documents | fix: `paths` on `open_merge_request` (commit only those) |
| M4 | medium | Every ticket's Doc Auditor edits the shared plan documents, so a wave's tickets always conflict | fix: the Auditor writes its trace to `docs/plans/<feature>/tickets/<id>.md` and edits ARCHITECTURE.md only when the design changed; no ticket status in the repository |
| M5 | medium | A check failure on a result from the reply text fails the run as "did not call finish" | fix: refuse with the reason and retry like a finish |
| M6 | medium | Gate entries in state can fail a strict `state_schema` | fix: leave gate entries out of state validation |
| M7 | medium | The Integrator is told to use `memory_save` with memory off | fix: memory on for the Integrator (lessons, D-075) |
| M8 | medium | Eval 025 runs an inline copy without the waves check | fix: add the check to the copy; disclose the copy in evals/README |
| L1 | low | Race between the in-flight check and the delete | fix: one statement |
| L2 | low | Deleted agents are not logged with ids | fix: log name, id, created_at |
| L3 | low | Ticket ids pin a wave before waves exist | fix: ids `T<n>`, the wave comes from WAVES.md |
| L4 | low | A refused MR still ends the run succeeded | fix: `merge_request_url` minLength 1 |
| L5 | low | A gap rejection drops the operator's reason | fix: append the reason |
| L6 | low | The Builder is not told which feature's plan to read | fix now in the task text (the ticket names it); wave 2's issues carry it |
| L7 | low | A gate rejected then approved still reads "rejected" | fix: write `approved` on approval |
| L8 | low | Duplicate ticket ids pass the check | fix: refuse duplicates |
| L9 | low | Reviewer and Auditor can run commands; "never change code" is prompt-only | accept: same as the former Reviewer |
| L10 | low | An eval action with `answer` can approve a non-question | fix: report a mismatch |
