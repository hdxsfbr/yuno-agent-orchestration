# Waves: the HDX Factory delivery process on Yuno

Tracer-bullet order: wave 1 is a thin path that runs end to end and is the demo milestone;
each later wave widens it. Tickets inside a wave touch disjoint files and are built in
parallel by separate agents; waves run in order. After every wave: `make test`, `make
lint`, `make eval` at 100 % (run as CI runs them, under its CPU limits), DESIGN.md,
DECISIONS.md, AI_USAGE.md and README updated in the same commit, then a stop for review.

## Wave 0: design and prerequisites (coordinator)

- W0.1 DESIGN 5.28 (plan, build, ship templates and agents), 5.29 (plans, tickets, waves,
  watcher, tools, pause, notifications), written with the design-first template; decisions
  recorded; `api/openapi.yaml` 1.0.0 contract for the new endpoints marked planned.

## Wave 1: the thin path (demo milestone)

Plan produces the four documents with both human approvals; one ticket runs through
implement, cross-model review, doc review and MR. Waves advance by hand.

| Ticket | Scope | Files |
|---|---|---|
| T1.1 Factory agents | Seed the eight factory agents (prompts, models, tools, memory on for Scout and Builder); a seed step removes the retired agents by name; the Chief untouched and still bound; `seed-reset` restores the factory agents | `internal/seed/` |
| T1.2 `plan_pipeline` | Template: Analyst (ask_operator, PRD.md) → Scout (gaps, project notes) → Architect → `design_gate` (on_reject → Architect) → Planner TASKS → Planner WAVES → `plan_gate` (on_reject → Planner) → Integrator (`open_merge_request` with the four docs) → end; input `{task, feature}`; schemas for each node | `templates/plan_pipeline.json` |
| T1.3 `wave_pipeline` | Template: Builder (tests first) ⇄ Code Reviewer (bounded) → Doc Auditor (verdict `ok` or `gap`; `gap` routes to a failing path with the reason for now) → Integrator (`open_merge_request`) → end | `templates/wave_pipeline.json` |
| T1.4 Retire the old templates | Per decision D1: stop loading `dev_pipeline`, `research_pipeline`, `review_panel` (moved to `templates/legacy/`, not loaded) and drop their shipped workflows on seed-reset; demo scripts and docs that name them updated | `templates/`, `internal/httpapi/templates.go`, `scripts/demo/`, docs |
| T1.5 Eval: plan happy path | Case with scripted turns: Analyst asks and gets an answer, both approvals granted, the four documents produced, MR opened (deploy target or fake forge) | `evals/cases/v1/025-*.yaml` |

Demo: Run "Plan pipeline" on the playground with a small feature; answer the Analyst's
question; approve the design and the plan; the plan MR opens with the four documents; run
"Wave pipeline" by hand for one ticket; it opens its MR.

## Wave 2: tickets, draft MR, escalation

| Ticket | Scope | Files |
|---|---|---|
| T2.1 Issue creation on both providers | `IssueProvider.Create` and `FindByMarker` for GitLab and GitHub, with tests against fake servers | `internal/tools/issues.go`, `providers*.go` |
| T2.2 Plans and tickets data | Migration: `plans`, `plan_tickets`, `plan_events`, `projects.waves_paused`; store functions; API `GET /api/projects/{id}/plans`, plan detail with tickets and events, pause and resume | `internal/store/`, `internal/httpapi/plans.go` |
| T2.3 `publish_tickets` tool | Side-effecting, gated, marker dedupe; records the plan and its tickets; releases nothing yet (wave 1 is released by the watcher once the plan MR merges, wave 3) | `internal/tools/tickets.go` |
| T2.4 Draft MR | `open_merge_request` `draft` and ready transition on both providers; title and description update | `internal/tools/mergerequest.go`, `providers.go` |
| T2.5 `escalate` tool and notifications | Tool that fails its own run with the reason; pause and plan event; delivery to the Chief's Telegram conversation; engine path for a tool-initiated failure | `internal/tools/escalate.go`, `internal/engine/`, `internal/project/` |
| T2.6 Templates use them | `plan_pipeline` ends with `publish_tickets`; `wave_pipeline` gains the draft MR, `manual_test` (on_reject → Builder), the ready update, and Doc Auditor's `escalate` | `templates/*.json` |
| T2.7 Evals | Review loop-limit escalation; PRD-gap escalation | `evals/cases/v1/026-*`, `027-*` |
| T2.8 UI | Project page: Plans section (tickets with issue, run, MR, status; events); pause toggle | `web/src/` |

## Wave 3: waves that advance themselves

| Ticket | Scope | Files |
|---|---|---|
| T3.1 Default-branch status | `DefaultBranchStatus` on both providers | `internal/tools/providers.go` |
| T3.2 Wave watcher | Loop: plan MR merged → release wave 1; every ticket MR merged and main green → start one `ship_pipeline` run per wave; a failed ticket run → escalate; honours the pause flag; plan events and SSE | `internal/waves/` |
| T3.3 `advance_wave` tool | Close the wave's issues, label the next wave's issues with the intake label, record events; `last: true` after the final wave; refuses while paused; idempotent per plan and wave | `internal/tools/waves.go` |
| T3.4 `ship_pipeline` template | `smoke_test` approval → Integrator (lessons as project notes, `advance_wave`) → `accept_feature` approval on the last wave → end | `templates/ship_pipeline.json` |
| T3.5 Fake forge for evals | A local service implementing the GitLab endpoints the platform uses (issues, labels, MRs, pipelines) so wave cases run offline | `cmd/yuno` role `forgefake`, compose profile `eval` |
| T3.6 Eval: auto-advance and pause | Two waves of one ticket each run to the end; with the pause flag set the second wave is held until resumed | `evals/cases/v1/028-*` |
| T3.7 UI | Wave timeline on the project page; ship and accept approvals link to their plan | `web/src/` |

## Wave 4: the loops back and the docs

| Ticket | Scope | Files |
|---|---|---|
| T4.1 Revise the plan | `plan_pipeline` input `revise_plan` (plan id): the agents amend the documents and tickets, `publish_tickets` updates and adds issues, the plan resumes at its current wave | templates, `internal/tools/tickets.go` |
| T4.2 Changes requested | The project page's ticket row starts a `wave_pipeline` iteration on the ticket's MR | `web/src/`, `internal/httpapi/plans.go` |
| T4.3 Lessons | Per decision D5 (project notes, and the repository's `CLAUDE.md` through the ship run's MR) | `templates/ship_pipeline.json`, Integrator prompt |
| T4.4 Docs | README section "The delivery process", DESIGN, threat model (issue tool, automatic trigger), test results, live verification on the playground | docs |

## Open decisions (recommendation first)

- D1 Old templates. Recommended: retire `dev_pipeline`, `research_pipeline` and
  `review_panel` with their agents (kept in `templates/legacy/`, not loaded), since their
  agents are being removed. Alternative: keep them by mapping their agent names to the new
  agents (Coder → Builder, Reviewer → Code Reviewer, Deployer → Integrator) and drop only
  the research and panel templates.
- D2 Repository. Recommended: tag the submitted state (`submitted-2026-10-19`, at
  `fcd1eb6`) and continue on `main` here. Alternatives: a new repository; a long-lived
  branch.
- D3 Approvals per ticket. Every `open_merge_request` call is gated today, so a ticket asks
  three times (draft MR, manual test, ready). Recommended: keep the tool gate on the draft
  call and let the ready call that only pushes to the same MR run under the manual-test
  approval already given (a per-node policy `auto_after_approval`), recorded as a
  decision. Alternative: keep all three.
- D4 Main green. Recommended: the default branch's latest pipeline (including its deploy
  and smoke jobs) must have passed on a commit at or after the wave's last merge.
  Alternative: the operator confirms it in the smoke-test approval only.
- D5 Lessons. Recommended: project notes (they reach every later task automatically) plus
  an append to the repository's `CLAUDE.md` in a small MR from the ship run, as the
  diagram says. Alternative: project notes only.
- D6 PRD gap. Recommended: escalate pauses the waves and the operator re-runs the plan in
  revise mode (wave 4); until then the wave holds. Alternative: the Doc Auditor's gap
  opens a new plan run automatically.
- D7 Intake label. Recommended: tickets are created with `wave:<n>` and without the intake
  label; releasing a wave adds the project's intake label (`yuno`), so intake stays one
  label and needs no change. Alternative: intake learns to filter by two labels.
- D8 Agent names and models as in ARCHITECTURE.md (eight agents, Builder on sonnet, Code
  Reviewer on opus).
