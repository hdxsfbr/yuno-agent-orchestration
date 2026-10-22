# Parity matrix: UI/API actions and the Chief of Staff

Inventory of every route registered in `internal/httpapi/server.go` (83, at aeec968), read
from the code, not from the docs. PRD requirement 1.

**Authorization today, every `/api` route:** one shared bearer, `API_TOKEN`
(`middleware.go:80-97`). There are no users, roles or owner checks. There is no
idempotency key, no version check (every PUT is last-write-wins) and no audit log; the only
records are partial (`approvals.decided_by='api'`, `runs.cancel_by='operator'`,
`plan_events` with no actor, `merge_requests.merged_by='operator'`).

**Chief today:** 12 chat tools (`internal/tools/chief.go:37-38`). Five of them mutate, all
with no approval and no idempotency key. The person behind a chat call is never recorded.

**Plan:** the Chief reaches every route through two generic tools,
`platform_operations` (discovery from `api/openapi.yaml`) and `platform_call` (in-process
dispatch through the same handler and middleware the UI uses; DESIGN rule set A). Each
operation carries a risk class in the contract (`x-yuno-risk`):
- `read`: no gate.
- `routine`: no gate; needs an idempotency key, plus `If-Match` on an update or delete.
- `destructive` and `privileged`: an explicit human approval of that exact call (rule set A6).
- `dedicated`: refused by `platform_call`, which names the purpose-built tool that carries
  the person's evidence.
- `machine`: never offered to the Chief.

The "Chief today" column names the existing tool, and "—" means none. The acceptance
column names the test that proves parity. Every `platform_call` row is also covered by
`TestPlatformParity`, which fails when a route has no documented operation, operation id or
risk class.

## Ops, health, streams

| # | Action | Backend | Chief today | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 1 | GET /healthz | healthz | — | none needed | machine | n/a |
| 2 | GET /readyz | readyz | — | none needed | machine | n/a |
| 3-4 | GET /openapi.yaml, /api/openapi.yaml | openapi | — | discovery | `platform_operations` reads the embedded contract (read, W1) | TestPlatformOperationsSearch |
| 5 | GET /api/status | status | — | read | read, W1 | TestPlatformParity |
| 6 | GET /api/openclaw/status | openclawStatus | — | read | read, W1 | TestPlatformParity |
| 7 | GET /api/events (SSE) | eventsAll | — | a stream cannot be a tool call | machine; the paged JSON read in row 8a replaces it | n/a |
| 8 | GET /api/runs/{id}/events (SSE) | eventsRun | — | no paging, replay capped at 1000 | machine; row 8a | n/a |
| 8a | **new** GET /api/runs/{id}/events/page | — | — | paged, redacted logs and events (PRD 2) | read, W1 | TestRunEventsPage |
| 8b | **new** GET /api/runs/{id}/diagnosis, GET /api/runs/diagnosis | — | run_status shows the status only | liveness from evidence (PRD 2) | read, W1 | TestDiagnosis* |
| 8c | **new** GET /api/audit | — | — | audit read (PRD 6) | read, W1 | TestAuditList |

## Agents and tools

| # | Action | Backend | Chief today | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 9 | GET /api/agents | listAgents | list_agents (partial view) | full fields | read, W1 | TestPlatformParity |
| 10 | POST /api/agents | createAgent | — | create | routine; privileged when it grants tools (A5) | TestChiefAgentCRUD |
| 11 | GET /api/agents/{id} | getAgent | — | read, ETag | read, W1 | TestChiefAgentCRUD |
| 12 | PUT /api/agents/{id} | updateAgent | — | update, version | routine; privileged when tools, rules or guardrails grow, or for the Chief's own agent (A5) | TestChiefAgentCRUD, TestPrivilegedNeedsApproval |
| 13 | DELETE /api/agents/{id} | deleteAgent (soft delete = archive) | — | archive | destructive | TestDestructiveNeedsApproval |
| 14 | POST /api/agents/{id}/materialize | materializeAgent | — | push again | routine | TestPlatformParity |
| 15 | GET /api/agents/{id}/runtime | agentRuntime | — | read | read | TestPlatformParity |
| 16 | GET /api/agents/{id}/files | listAgentFiles | — | read | read | TestPlatformParity |
| 17 | PUT /api/agents/{id}/files/{name} | putAgentFile | — | prompt files | routine; privileged for the Chief's own agent | TestChiefAgentCRUD |
| 18 | GET /api/tools | listTools | — | read | read | TestPlatformParity |
| 76 | GET /api/agents/{id}/memory | listAgentMemory | memory_recall (own only) | other agents | read | TestPlatformParity |
| 77 | DELETE /api/agents/{id}/memory/{factId} | deleteAgentMemory | — | forget | destructive | TestDestructiveNeedsApproval |

## Approvals and questions

| # | Action | Backend | Chief today | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 19 | GET /api/approvals | listApprovals | list_approvals (pending, ≤20) | history, filters | read | TestPlatformParity |
| 20 | POST /api/approvals/{id}/approve | approveApproval | decide_approval (code in the person's message) | `always` | dedicated: decide_approval. A decision always needs the person's code; `always` stays UI-only (it grows an agent's exec allowlist) | TestPlatformCallRefusesDedicated |
| 21 | POST /api/approvals/{id}/reject | rejectApproval | decide_approval | — | dedicated | same |
| 22 | POST /api/approvals/{id}/answer | answerApproval | answer_question | no evidence check | dedicated; W1 adds the evidence check (A7) | TestAnswerQuestionNeedsPersonMessage |

## Workflows and templates

| # | Action | Backend | Chief today | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 23 | GET /api/workflows | listWorkflows | list_workflows (partial) | full fields | read | TestPlatformParity |
| 24 | POST /api/workflows | createWorkflow | — | create, with DAG validation in the handler | routine | TestChiefWorkflowCRUD |
| 25 | POST /api/workflows/validate | validateWorkflow | — | validate (cycles, schema) | read-like; routine with no side effect | TestChiefWorkflowCRUD (cyclic is refused) |
| 26 | GET /api/workflows/{id} | getWorkflow | — | read, ETag | read | TestChiefWorkflowCRUD |
| 27 | PUT /api/workflows/{id} | updateWorkflow (version bump; runs keep their snapshot) | — | update, version | routine | TestChiefWorkflowCRUD (active run stays pinned) |
| 28 | DELETE /api/workflows/{id} | deleteWorkflow (soft) | — | delete | destructive | TestDestructiveNeedsApproval |
| 29 | GET /api/workflow-templates | listTemplates | — | read | read | TestPlatformParity |
| 30 | POST /api/workflow-templates/{key}/instantiate | instantiateTemplate | — | copy | routine | TestPlatformParity |
| — | **new** GET /api/workflows/{id}/versions, POST …/versions/{v}/restore | — | — | versions read, rollback (PRD 5) | read; routine (W4) | TestWorkflowRollback |

## Runs

| # | Action | Backend | Chief today | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 31 | GET /api/runs | listRuns | — | list | read | TestPlatformParity |
| 32 | POST /api/runs | createRun | delegate_task (no limits/model, no key) | limits, model, idempotency | routine. `trigger=eval` and `script_id` are refused for the Chief (machine fields) | TestPlatformCallIdempotent |
| 33 | GET /api/runs/{id} | getRun | run_status | — | read | TestPlatformParity |
| 34 | POST /api/runs/{id}/cancel | cancelRun (202 before the worker stops) | — | request identity, acknowledgement (PRD 3) | routine; W2 turns it into a command with received/acked/applied | TestCancelAcknowledged |
| 35 | POST /api/runs/{id}/retry | retryRun (whole run, from current_node) | — | a chosen step; refuse while a cancel is unconfirmed | routine; W2 adds `from_node` and the guard | TestRetryFromStep, TestRetryRefusedUnconfirmedCancel |
| — | **new** POST /api/runs/{id}/pause, /resume | — | — | run pause and resume (PRD 3) | routine, W2 | TestPauseResume |
| — | **new** POST /api/runs/{id}/steer | — | — | operator text into a live step (PRD 3) | routine, W2 | TestSteerDelivered |
| — | **new** GET /api/runs/{id}/commands[/{cid}] | — | — | command status (PRD 3) | read, W2 | TestCancelAcknowledged |
| 36 | GET /api/runs/{id}/trace | getTrace | run_status (subset) | full trace | read (large; the tool trims it) | TestPlatformParity |
| 37 | GET /api/runs/{id}/artifacts | listArtifacts | run_status (names) | read | read | TestPlatformParity |
| 38 | GET /api/runs/{id}/artifacts/{artifactId} | downloadArtifact | — | binary | read; the tool returns text up to 32 KiB and metadata for binaries | TestPlatformCallBinary |
| 39 | POST /api/evals/scripts | registerScript | — | eval only | machine | TestPlatformParity (refused) |

## Schedules

| # | Action | Backend | Chief today | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 40 | GET /api/schedules | listSchedules (with next_run_at) | — | read | read | TestChiefScheduleCRUD |
| 41 | POST /api/schedules | createSchedule (cron, timezone) | — | create | routine | TestChiefScheduleCRUD |
| 42 | GET /api/schedules/{id} | getSchedule | — | read, ETag | read | TestChiefScheduleCRUD |
| 43 | PUT /api/schedules/{id} | updateSchedule (enabled = pause/resume; recomputes next_run_at) | — | update, pause/resume | routine | TestChiefScheduleCRUD |
| 44 | DELETE /api/schedules/{id} | deleteSchedule (hard) | — | delete | destructive | TestDestructiveNeedsApproval |

## Skills

| # | Action | Backend | Chief today | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 45, 47, 52, 53, 56 | GET skills, skill, files, file, export | list/get/export | — | read (export is a zip: metadata only) | read | TestPlatformParity |
| 46, 48 | POST, PUT /api/skills[/{id}] | create/update (a rename is pushed to agents) | — | write | routine | TestPlatformParity |
| 49 | DELETE /api/skills/{id} | deleteSkill (hard; removed from agents) | — | delete | destructive | TestDestructiveNeedsApproval |
| 50 | POST /api/skills/import (zip upload) | importSkillZip | — | multipart | machine (not expressible as JSON); import-git covers it | n/a |
| 51 | POST /api/skills/import-git | importSkillGit (host allowlist) | — | import | routine | TestPlatformParity |
| 54 | PUT …/files/{path...} | putSkillFile | — | edit | routine | TestPlatformParity |
| 55 | DELETE …/files/{path...} | deleteSkillFile | — | delete | destructive | TestDestructiveNeedsApproval |

## Projects, plans, waves

| # | Action | Backend | Chief today | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 57 | GET /api/projects | listProjects | list_projects | — | read | TestPlatformParity |
| 58 | POST /api/projects | createProject | add_project (no token, caps or description) | full fields | routine; the Chief never sends a token (A8) | TestPlatformParity |
| 59 | GET /api/projects/{id} | getProject | — | read, ETag | read | TestPlatformParity |
| 60 | PUT /api/projects/{id} | updateProject (full replace) | — | update | routine; privileged when the token or repo changes or a budget cap rises (A5) | TestPrivilegedNeedsApproval |
| 61 | DELETE /api/projects/{id} | deleteProject (soft) | — | delete | destructive | TestDestructiveNeedsApproval |
| 62 | POST /api/projects/{id}/check | checkProject | — | check token | routine | TestPlatformParity |
| 63 | POST /api/projects/{id}/intake/poll | pollIntake | — | poll now | routine | TestPlatformParity |
| 64, 65 | GET project plans, plan | listPlans/getPlan | list_projects (open_plan) | full plan | read | TestPlatformParity |
| 66 | POST /api/plans/{id}/cancel | cancelPlan | — | cancel | destructive | TestDestructiveNeedsApproval |
| 67 | POST /api/plans/{id}/tickets/{ticket}/changes | requestChanges | delegate_task with merge_request | — | routine | TestPlatformParity |
| 68, 69 | POST waves pause/resume | pauseWaves/resumeWaves | — | pause/resume | routine | TestPlatformParity |
| 70 | GET /api/projects/{id}/facts | listProjectFacts | — | read | read | TestPlatformParity |
| 71 | DELETE …/facts/{factId} | deleteProjectFact | — | delete | destructive | TestDestructiveNeedsApproval |

## Merge requests

| # | Action | Backend | Chief today | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 72, 73 | GET merge requests, merge request | list/get (stored record) | read_merge_request (provider read) | exact-SHA checks, diff, mergeability, CI logs (PRD 4) | read; W3 adds the new reads | TestMergeRequestChecks |
| 74 | POST /api/merge-requests/{id}/sync | syncMergeRequest | — | sync | routine | TestPlatformParity |
| 75 | POST /api/merge-requests/{id}/merge | mergeMergeRequest (CI passed or none; no expected SHA) | — | exact-SHA guard (W3) | destructive (merging to the default branch) | TestDestructiveNeedsApproval, TestMergeNeedsExactSHA |
| — | **new** GET …/diff, …/checks, …/jobs/{job}/log | — | — | PRD 4 | read, W3 | TestMergeRequestChecks |
| — | **new** POST …/update-from-default | — | — | safe branch update with expected head (PRD 4) | routine, W3 | TestUpdateFromDefault |
| — | **new** dependencies (PUT …/dependencies) | — | — | dependency gating (PRD 4) | routine, W3 | TestDependencyGate |

## Channels and conversations

| # | Action | Backend | Chief today | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 78 | GET /api/channels | getChannels | — | read | read | TestPlatformParity |
| 79 | PUT /api/channels/telegram/binding | bindTelegram | — | rebinding moves the operator's channel | privileged | TestPrivilegedNeedsApproval |
| 80 | DELETE /api/channels/telegram/binding | unbindTelegram | — | the Chief would cut its own line | destructive | TestDestructiveNeedsApproval |
| 81-83 | GET conversations, messages, tool calls | list handlers | — | read | read | TestPlatformParity |

## Not routes: Chief-only tools that stay

`delegate_task`, `run_status`, `list_*`, `decide_approval`, `answer_question`,
`add_project`, `memory_save`/`memory_recall` and `read_merge_request` stay: they are
shaped for chat and carry the evidence rules. `platform_call` adds the parity, and these are
not removed.
