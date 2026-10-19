# Delivery process, wave 2 review (DESIGN 5.29 rules 1 to 10)

One independent reviewer (Claude Opus 5.5), read-only, 2026-10-19. Two high, nine medium,
nine low.

| # | Sev | Finding | Disposition |
|---|---|---|---|
| H1 | high | Issue bodies read `slice`/`acceptance_tests`, which the slicer never outputs | fix: the slicer outputs `slice` and `tests`; the tool reads them; fixtures use real slicer output |
| H2 | high | A retried escalated ticket stays `failed`; a second escalation is silent | fix: a retry moves the ticket back to `building`/`in_review` |
| M1 | medium | The ready call pushes the whole checkout under the manual-test approval | fix: the ready call passes `paths: docs/plans/<feature>`; `auto_after` requires paths under `docs/` |
| M2 | medium | Deferred or interrupted notices are lost | fix: `plan_events.notified_at` and a resend sweep |
| M3 | medium | Escalation is three statements | fix: one transaction |
| M4 | medium | `EscalateRun` can report success without ending the run | fix: end the run first (fallback running → failed), then the plan side |
| M5 | medium | The plan MR id is set only at plan creation | fix: set it on every call while null |
| M6 | medium | Issue doc links point at the plan MR's branch, removed on merge | fix: link the default branch |
| M7 | medium | `publish_tickets` makes 100+ calls and can time out at the cap | fix: list once and match markers locally; a longer timeout |
| M8 | medium | Integrator schemas require a URL the task says may be empty | fix: drop `minLength` on the wave pipeline's Integrators |
| M9 | medium | A plan cannot be cancelled; a feature slug can never be reused | fix: `POST /api/plans/{id}/cancel`; feature unique among open plans only |
| L1 | low | A failed ready transition is an internal error | fix: model error |
| L2 | low | GitHub draft PRs fail on private repos without draft support | disclose |
| L3 | low | The marker is matched anywhere in the body | fix: match the last line |
| L4 | low | Feature and titles not checked in the tool | fix: validate the slug, cap titles at 200 |
| L5 | low | A missed merged transition leaves a ticket `in_review` | fix: the sync re-applies merged tickets each pass |
| L6 | low | The page listens to a non-existent event; MR links publish nothing | fix: publish `plan.updated` on MR link; drop the dead listener |
| L7 | low | Scout and Integrator lack `escalate` | fix: grant it |
| L8 | low | Eval 026 has no plan ticket (offline) | disclose: rule 7 is covered by Go tests |
| L9 | low | A manual relabel's run is not linked to its ticket | disclose |
| — | note | An operator's cancel pauses the waves (rule 7) | accept, as specified |
| — | test | A leaked goroutine in `escalate_test.go` | fix |
