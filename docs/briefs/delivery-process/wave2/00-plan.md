# Delivery process wave 2: tickets, draft MR, escalation (DESIGN 5.29 rules 1 to 9)

Sonnet 5.5 agents on disjoint files, one test database each, never `make`.

P. Prerequisites (alone, first): migration `0017_plans.sql` (rule 1, plus
`merge_requests.draft`), store types and repositories (plans, tickets, events, pause),
`api/openapi.yaml` 1.0.0 with the plan paths marked `x-planned: factory wave 2`.

A. Tickets: `IssueProvider.Create` and `FindByMarker` on GitLab and GitHub (rule 3),
`publish_tickets` (rule 2), registration and `openclaw.PlatformTools`.

B. Draft MR and `auto_after` (rules 4 and 5): `open_merge_request` `draft`, the ready
transition on both providers, title and description update; the node field `auto_after`
(validated at save) honoured by the tool gate for the run's own MR update.

C. Escalation and plan state (rules 6 to 9 backend): `escalate` and the engine path that
fails the calling run; failed ticket runs escalate; ticket status from intake takes and MR
records (rule 10's wave 2 part); notifications to the Chief's conversation; pause and
resume and plans endpoints.

D. Frontend: the project page's Plans section and pause control (rule 9).

E. After A to D (one agent): templates (`plan_pipeline` ends with `publish_tickets`;
`wave_pipeline` per rule 5 with `escalate`), seed tool grants, evals 026 and 027.
