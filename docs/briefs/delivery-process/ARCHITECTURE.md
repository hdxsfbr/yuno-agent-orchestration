# Architecture: the HDX Factory delivery process on Yuno

Every box and arrow of the diagram, mapped to a Yuno mechanism (existing, with code
evidence) or marked NEW. Evidence comes from reading the code on 2026-10-19, not the docs.

## Agents (R1, R2)

The seed (`internal/seed/seed.go:86-181`) stops creating Coder, Reviewer, Deployer,
Researcher, Critic, Writer and the three panel reviewers, and creates the factory's:

| Agent | Model | Boxes | Tools beyond `finish` |
|---|---|---|---|
| Analyst | opus | Clarify requirements | `ask_operator`, `read`, `ls`, `write` |
| Scout | sonnet | Review PRD against the codebase | `read`, `ls`, `exec`, `memory_save`/`recall` (project scope) |
| Architect | opus | Design the architecture | `read`, `ls`, `write`, `edit`, `ask_operator` |
| Planner | opus | Spec as tracer bullets, Plan waves | `read`, `ls`, `write`, `edit` |
| Builder | sonnet | Implement (tests first) | `read`, `ls`, `write`, `edit`, `exec`, `ask_operator`, memory |
| Code Reviewer | opus | Review with a different agent and model | `read`, `ls`, `exec`, `read_merge_request` |
| Doc Auditor | opus | Doc + PRD review | `read`, `ls`, `write`, `edit`, `exec`, `escalate` (NEW) |
| Integrator | sonnet | Open pull request, publish tickets, wave done | `open_merge_request`, `publish_tickets` (NEW), `advance_wave` (NEW), memory |

The Chief of Staff stays as it is and stays bound to Telegram (deleting other agents
does not unbind it, `internal/httpapi/agents.go:355`; `BindChief` only binds when no
agent is bound, `seed.go:376-392`). Model choice per agent already exists and is
validated against `claude-opus-5-5`, `claude-sonnet-5-5`, `claude-haiku-5-5`
(`internal/openclaw/baseline.go:47`): Builder on sonnet, Code Reviewer on opus satisfies
R2. Removal: a one-time seed step deletes the retired agents by name if they exist
(decision D1 on the old templates). Evals create their own agents
(`internal/evals/runner_case.go:28-59`), so they do not depend on the seed.

## Plan lane: template `plan_pipeline` (once per feature)

| Diagram | Yuno mechanism |
|---|---|
| Inputs: PRD doc, tracker ticket, chat | Run dialog (`POST /api/runs`), issue intake (`internal/intake`, input `{task}` from the issue, `intake.go:396`), Chief `delegate_task` with `workflow_name` (`internal/tools/chief.go:659`). Input schema `{task}` plus an optional `feature` slug. |
| Clarify requirements (human) | Agent node Analyst; `ask_operator` blocks up to 9 min then parks and resumes with the answer (`internal/tools/askoperator.go`, `internal/engine/questions.go`), 3 questions per step. Writes `docs/plans/<feature>/PRD.md` in the checkout. |
| PRD.md arrow | The checkout flows to the next node (`walk.go:305-313`); the document is also a run artifact (`out/`, `internal/artifacts/collector.go`). |
| Review PRD against the codebase | Agent node Scout; saves build/test facts with `memory_save` scope `project` (`internal/tools/memory.go:177`), which later tasks receive automatically (`internal/engine/node.go:569-583`). Output `{gaps, constraints}` in state. |
| Design the architecture (human) | Agent node Architect writes `ARCHITECTURE.md`; approval node `design_gate` with the document summary in its message; `on_reject` back to Architect with the reason (`internal/workflow/def.go:40-41`, `walk.go:128-136`). |
| Spec as tracer bullets / TASKS | Agent node Planner writes `TASKS.md` (ticket id, title, slice, files, acceptance tests) and outputs the ticket list as typed state (`output_schema`). |
| Plan waves / WAVES.md | Same Planner agent, second node, writes `WAVES.md` and outputs `{waves:[{n, tickets:[id]}]}`; the output schema forbids a ticket in two waves. NEW validation: the platform checks that no two tickets in a wave list a common file (refuses the finish with a reason). |
| Approve the plan (human) | Approval node `plan_gate`; `on_reject` back to Planner. |
| Publish (after approval) | Agent node Integrator: `open_merge_request` (existing) commits the four documents in one MR; `publish_tickets` (NEW, gap 1) creates one issue per ticket labelled `wave:<n>` and records the plan (below). |
| start wave 1 | NEW: the wave watcher treats the plan MR as wave 0; when it is merged, wave 1's issues are released (R11). |

## Build lane: template `wave_pipeline` (one run per ticket)

| Diagram | Yuno mechanism |
|---|---|
| Implement the wave: one agent per ticket, isolated worktree | Each ticket issue becomes its own run through intake (one run per labelled issue, parallel, bounded by budget and 10 takes per pass, `intake.go:340`). Each run has its own checkout and branch (gap 4 confirmed: fork/join carries one branch's checkout only, so tickets are runs, not branches). |
| tests first | Builder's task and output schema require the failing test first, then the code, and `tests_passed`. |
| Review with a different agent/model; comments (max N rounds) | Code Reviewer node; edge `changes_requested` back to Builder, `approved` forward; bounded by `max_loop_visits` (`walk.go:159-168`). |
| Loop limit hit → escalate | Today the run fails with "loop limit N exceeded for builder" and intake marks the issue `:failed` (`intake.go:603`). NEW: the wave watcher sees a failed ticket run and escalates (pause + notify), R18. |
| Manual test (human); bugs found | Gap 3. NEW `draft` input on `open_merge_request`: Integrator opens a draft MR so the branch exists; approval node `manual_test` names the branch; `on_reject` back to Builder with the operator's notes ("bugs found"); a later call in the same run updates the same MR (`internal/tools/mergerequest.go:174-198`, `:274-304`) and can mark it ready (NEW). |
| Doc + PRD review | Doc Auditor node: updates the docs in the checkout; on an unmet requirement or a wrong design it calls `escalate` (NEW) instead of finishing. |
| PRD gap found → back to Design | `escalate` fails the run with the reason, pauses the project's waves and notifies; the operator runs `plan_pipeline` again with `revise_plan` input to amend the documents and tickets (R18, decision D6). |
| Open pull request; code + docs together | Integrator's second `open_merge_request` call pushes the docs onto the same branch and marks it ready; CI runs on the MR; `merge_requests` sync keeps state and `ci_status` (`internal/mergereq/sync.go`). `Closes #<n>` is already appended for issue runs (`mergerequest.go:223`). |

## Ship lane: template `ship_pipeline` (one run per wave, started by the watcher)

| Diagram | Yuno mechanism |
|---|---|
| Review core code and docs (human); PR merged | Outside Yuno or the run page's Merge button (`internal/httpapi/mergerequests.go:94-143`). "changes requested": the operator iterates on the MR with a `wave_pipeline` run naming it (`merge_request_id`, wave 9 iteration). |
| CI on main, deploy, CI smoke | Outside Yuno (the project's pipeline). NEW `DefaultBranchStatus` on both providers (`internal/tools/providers.go`) reads the latest pipeline on the default branch (GitLab pipelines API, GitHub check runs and combined status on the branch head). |
| deployed / all merged | NEW wave watcher (a loop like `mergereq.Syncer`): for each plan's current wave, when every ticket's MR is `merged` and the default branch status is `passed` on a commit at or after the last merge, it starts one `ship_pipeline` run for the wave (unique per plan and wave). |
| Manual smoke test (human) | Approval node `smoke_test` in `ship_pipeline`. |
| Wave done: close tickets, lessons, auto-trigger | Integrator node: saves lessons as project notes (and decision D5), then `advance_wave` (NEW, side-effecting): closes the wave's issues that are still open, records the transition, and adds the project's intake label to the next wave's issues so intake starts their builds. |
| last wave → Accept the feature (human) | `advance_wave` reports `last: true`; the template's edge goes to approval `accept_feature` (message: the live app against PRD.md), then end; the plan is marked done. |
| Pause flag | NEW `projects.waves_paused`; the watcher starts no ship run and `advance_wave` refuses while it is set; escalation sets it; the project page toggles it. |
| Transitions visible | NEW `plan_events` rows (plan created, wave released, ticket failed, escalated, paused, resumed, wave done, accepted) shown on the project page and pushed over SSE. |

## New data (one migration)

- `plans(id, project_id, feature, plan_run_id, plan_mr_id, status [planning|building|done|cancelled], current_wave, created_at)`
- `plan_tickets(plan_id, ticket_id, wave, title, issue_number, issue_url, run_id, merge_request_id, status [pending|released|building|in_review|merged|failed|closed])`
- `plan_events(id, plan_id, at, kind, wave, ticket_id, detail)`
- `projects.waves_paused BOOLEAN`
- A project has at most one plan in `building` (out of scope: several features at once).

Ticket runs are tied to their plan by the issue: intake records `trigger_ref` `<project>#<n>`
(`internal/store/migrations/0013_wave9.sql:18`), and `plan_tickets.issue_number` joins it.

## New tools

- `publish_tickets` (side-effecting, approval-gated): input the plan's tickets and waves;
  creates one issue per ticket with labels `wave:<n>` (and the feature label), body = the
  ticket's slice and acceptance tests plus links to the plan documents and a hidden marker
  `<!-- yuno-ticket:<plan>:<ticket> -->`. Idempotency: the platform key
  (`internal/tools/canonical.go`) covers a replay in the same visit; across visits the
  tool searches for the marker before creating, so a retry never duplicates (the same
  approach `open_merge_request` takes with its branch). NEW `IssueProvider.Create` and
  `FindByMarker` on GitLab and GitHub (`internal/tools/issues.go`).
- `advance_wave` (side-effecting, approval-gated, idempotent per plan and wave).
- `escalate` (computational, no approval): `{reason, kind: prd_gap | design_wrong | other}`;
  ends the run `failed` with the reason (NEW engine path: a tool that fails its own run),
  pauses the project's waves, writes a plan event, notifies.
- `open_merge_request` gains `draft` (create as draft; a later call with `draft: false`
  marks it ready: GitLab by the title's `Draft:` prefix, GitHub by the GraphQL
  `markPullRequestReadyForReview` mutation) and may update title and description.

## Notifications (R18, R21, R24)

Today only channel runs report to Telegram (`internal/engine/report.go:99-101`). NEW: plan
events of kind escalated, ship approval raised and feature accepted are delivered to the
Chief's Telegram conversation like budget alerts (`internal/project/budget.go`), and the
approvals appear in the web inbox as usual.

## Security

- The issue and wave tools are side-effecting and gated; the escalate tool can only stop
  its own run.
- The automatic trigger: whoever can add the intake label on the tracker can start builds;
  the release of a wave is done by the platform, but a person with label rights can also
  label tickets early. Bounded by the project budget cap (required for intake,
  `internal/httpapi/projects_intake.go:103-106`), the take limit and the pause flag.
- The ticket body Yuno writes is wrapped as untrusted when intake reads it back
  (`internal/tools/issues.go:331`), as any issue is.
- Threat model updated in the same wave as the tools.
