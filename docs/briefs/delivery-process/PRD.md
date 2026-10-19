# PRD: the HDX Factory delivery process on Yuno

Source: Andre's diagram "HDX Factory" (AI-assisted delivery process v3, three lanes: Plan,
Build, Ship) and his brief of 2026-10-19. Yellow boxes are human steps; dashed boxes are
inputs or notes; arrow labels are the artifact handed forward.

## Problem

Andre delivers software in a fixed process: plan a feature once, build it in waves of
parallel tickets, ship each wave behind human checks. Today he drives every step by hand.
Yuno already runs multi-agent workflows on a project repository with approvals, questions,
loop limits, merge requests and issue intake, but it has no notion of a plan, a ticket, a
wave, or a wave that follows another. The goal is for Yuno to run the whole process end to
end against one project repository, with the human steps as the only pauses.

## Users

One operator (Andre), on the web UI and on Telegram through the Chief of Staff.

## Requirements

### Agents

- R1. Every seeded agent except the Chief of Staff is removed; the Chief stays the only
  agent bound to Telegram. The factory's agents are seeded in their place (`make seed`),
  and `make seed-reset` restores them.
- R2. The reviewer in Build runs on a different model from the implementer.

### Plan (once per feature)

- R3. A plan starts from the Run dialog, from an issue (intake), or from the Chief on
  Telegram, with the feature request as input.
- R4. Clarify requirements: an agent asks the operator about every ambiguity (it waits
  for the answers) and confirms the business problem, then writes `PRD.md`.
- R5. Review the PRD against the codebase: an agent on the project checkout records gaps
  and constraints and saves build and test facts as project notes.
- R6. Design the architecture: an agent drafts `ARCHITECTURE.md`; the operator approves
  it or sends it back with comments.
- R7. Spec as tracer bullets: an agent writes `TASKS.md`, tickets as thin end-to-end
  slices, each with acceptance criteria that name tests.
- R8. Plan waves: an agent writes `WAVES.md`, grouping tickets into waves whose tickets
  are safe to build in parallel (no two touch the same files), waves in dependency order.
- R9. Approve the plan: the operator approves slices, waves and parallel safety, or
  sends it back.
- R10. On approval the four documents are committed through one merge request, and one
  tracker issue per ticket is created, labelled with its wave (`wave:1` ...). Creating
  issues is a side effect: approval-gated and idempotent; a retried step creates no
  duplicates.
- R11. Wave 1 starts once the plan's merge request is merged.

### Build (per wave; tickets of a wave in parallel)

- R12. Each ticket is its own run on its own branch and merge request.
- R13. Implement, tests first, on the project checkout.
- R14. Review by a different agent and model; comments go back to the implementer; the
  loop is bounded (`max_loop_visits`). Hitting the bound escalates.
- R15. Manual test: the operator can pull the ticket's branch and run it before the PR is
  ready; failing it sends the ticket back to the implementer with the operator's notes.
- R16. Doc and PRD review: an agent syncs `ARCHITECTURE.md`, `TASKS.md` and `WAVES.md`
  with the code and traces each PRD requirement the ticket covers to code and tests. An
  unmet requirement or a design the build proved wrong escalates; the agent never patches
  around it.
- R17. Open the pull request with code and docs together; CI runs on it.
- R18. Escalation stops the ticket with a clear reason, pauses the project's waves, and
  tells the operator (web and Telegram). Nothing continues silently.

### Ship (per wave)

- R19. The operator reviews and merges each ticket's PR (outside Yuno or with Merge on
  the run page); "changes requested" sends the ticket back to Build on the same branch.
- R20. Yuno knows when every ticket of the wave is merged and main's pipeline (CI,
  deploy, smoke) is green.
- R21. Then it raises one manual smoke-test approval for the wave.
- R22. Approving it closes the wave: its tickets are closed, lessons are recorded, and
  the next wave's issues are released so their builds start on their own.
- R23. A per-project pause flag holds the next wave; every wave transition is recorded
  and shown on the project page.
- R24. After the last wave a final approval asks the operator to accept the feature,
  live, against the PRD.

### Constraints

- R25. No side effect without the approval gate and an idempotency key.
- R26. New templates load from `templates/`; the evals stay at 100 %; new eval cases
  cover the plan happy path, the review loop-limit escalation, the PRD-gap escalation,
  and wave auto-advance with the pause flag.
- R27. The threat model covers the issue tool and the automatic trigger (anyone who can
  label issues can start builds, bounded by the project budget).

## Out of scope

Running CI, deploys or smoke tests inside Yuno (they stay in the project's pipeline);
several features in flight on one project at once; merging without a human.

## Success

A feature request becomes merged, deployed waves with the operator acting only at the
yellow boxes; a demo shows the plan pipeline producing the four documents with both
approvals and one ticket built through implement, cross-model review, doc review and MR.
