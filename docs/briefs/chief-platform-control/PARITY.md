# Parity matrix: UI/API actions and the Chief of Staff

Inventory of every route registered in `internal/httpapi/server.go`: 83 at aeec968, read
from the code, not from the docs (PRD requirement 1); re-checked in wave 4 (DESIGN W4 rule 8)
at 96 documented operations and three machine mounts (`/mcp` and the two model proxies).

**Before this work (aeec968):** one shared bearer, `API_TOKEN`, on every `/api` route; no
users, roles or owner checks; no idempotency key, no version check (every PUT was
last-write-wins) and no audit log. The Chief had 12 chat tools (`internal/tools/chief.go`),
five of them mutating with no approval and no idempotency key, and the person behind a chat
call was never recorded.

**Now:** every change and every refused `/api` request has an audit row (DESIGN 5.31 rule 1);
an `Idempotency-Key` makes a change repeat-safe and `If-Match` refuses a stale write (rules 3
and 4). The Chief reaches every route through two generic tools, `platform_operations`
(discovery from `api/openapi.yaml`) and `platform_call` (in-process dispatch through the same
handlers and middleware the UI uses; DESIGN rule set A). Each operation carries a risk class in
the contract (`x-yuno-risk`):
- `read`: no gate.
- `routine`: no gate; needs an idempotency key, plus `If-Match` on an update or delete.
- `destructive` and `privileged`: an explicit human approval of that exact call (rule A6).
- `dedicated`: refused by `platform_call`, which names the purpose-built tool that carries
  the person's evidence.
- `machine`: never offered to the Chief.

The "Backend" column names each route's handler, which is its `operationId`. "Chief before"
names the tool the Chief had at aeec968, and "—" means none. The "Acceptance" column names the
tests that prove parity. Every name there is a `func Test<Name>` of the repository, which
`TestParityMatrix` checks along with a row for every operation of the contract; a name
followed by "(wave 2)" or "(wave 3 gate)" is planned and not built yet, and the test fails once
such a test exists, so the mark is dropped when its wave lands. Every operation is also held by
`TestPlatformParity` to its route, its `operationId` and its risk class.

## Ops, health, streams, audit

| # | Action | Backend | Chief before | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 1 | GET /healthz | healthz | — | none needed | machine | TestReadyz |
| 2 | GET /readyz | readyz | — | none needed | machine | TestReadyz |
| 3-4 | GET /openapi.yaml, /api/openapi.yaml | openapi, apiOpenAPI | — | discovery | `platform_operations` reads the embedded contract (read, W1) | TestPlatformOperationsSearch, TestOpenAPISecurity |
| 5 | GET /api/status | status | — | read | read, W1 | TestPlatformParity |
| 6 | GET /api/openclaw/status | openclawStatus | — | read | read, W1 | TestPlatformParity |
| 7 | GET /api/events (SSE) | eventsAll | — | a stream cannot be a tool call | machine; the paged JSON read in row 8a replaces it | TestPlatformCallRefusesMachine |
| 8 | GET /api/runs/{id}/events (SSE) | eventsRun | — | no paging, replay capped at 1000 | machine; row 8a | TestPlatformCallRefusesMachine |
| 8a | **new** GET /api/runs/{id}/events/page | listRunEventsPage | — | paged, redacted logs and events (PRD 2) | read, W1 | TestRunEventsPage |
| 8b | **new** GET /api/runs/{id}/diagnosis, GET /api/runs/diagnosis | getRunDiagnosis, listRunDiagnoses | run_status shows the status only | liveness from evidence (PRD 2) | read, W1 | TestDiagnosisRules, TestDiagnosisRun, TestRunDiagnosis |
| 8c | **new** GET /api/audit | listAudit | — | audit read (PRD 6) | read, W1 | TestAuditList, TestUnauthorizedAudited |
| 8d | **new** POST /api/audit/{id}/revert | revertAudit | — | undo a change of an agent, schedule, skill or project (PRD 5) | routine in the contract; the gate holds it to the change it writes back (A5), W4 | TestAuditRevert, TestAuditRevertByChief |

## Agents and tools

| # | Action | Backend | Chief before | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 9 | GET /api/agents (`include_archived=true` lists archived ones, W4) | listAgents | list_agents (partial view) | full fields | read, W1 | TestPlatformParity, TestAgentRestore |
| 10 | POST /api/agents | createAgent | — | create | routine; privileged when it grants tools or looser rules (A5) | TestChiefAgentCRUD, TestPrivilegedNeedsApproval |
| 11 | GET /api/agents/{id} | getAgent | — | read, ETag | read, W1 | TestChiefAgentCRUD |
| 12 | PUT /api/agents/{id} | updateAgent | — | update, version | routine; privileged when tools, rules, guardrails or the exec policy grow, or for the Chief's own agent (A5) | TestChiefAgentCRUD, TestPrivilegedNeedsApproval, TestAuditRevert |
| 13 | DELETE /api/agents/{id} | deleteAgent (soft delete = archive) | — | archive | destructive | TestChiefAgentCRUD |
| 13a | **new** POST /api/agents/{id}/restore | restoreAgent | — | undo an archive (PRD 5) | routine; privileged when the agent has tools (restoring grants them again), W4 | TestAgentRestore, TestPrivilegedNeedsApproval |
| 14 | POST /api/agents/{id}/materialize | materializeAgent | — | push again | routine | TestPlatformParity |
| 15 | GET /api/agents/{id}/runtime | agentRuntime | — | read | read | TestPlatformParity |
| 16 | GET /api/agents/{id}/files | listAgentFiles | — | read | read | TestPlatformParity |
| 17 | PUT /api/agents/{id}/files/{name} | putAgentFile | — | prompt files | routine; privileged for the Chief's own agent | TestAgentFilesPut, TestPrivilegedNeedsApproval |
| 18 | GET /api/tools | listTools | — | read | read | TestPlatformParity |
| 76 | GET /api/agents/{id}/memory | listAgentMemory | memory_recall (own only) | other agents | read | TestPlatformParity |
| 77 | DELETE /api/agents/{id}/memory/{factId} | deleteAgentMemory | — | forget | destructive | TestDestructiveNeedsApproval |

## Approvals and questions

| # | Action | Backend | Chief before | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 19 | GET /api/approvals | listApprovals | list_approvals (pending, ≤20) | history, filters | read | TestPlatformParity |
| 20 | POST /api/approvals/{id}/approve | approveApproval | decide_approval (code in the person's message) | `always` | dedicated: decide_approval. A decision always needs the person's code; `always` stays UI-only (it grows an agent's exec allowlist) | TestPlatformCallRefusesDedicated |
| 21 | POST /api/approvals/{id}/reject | rejectApproval | decide_approval | — | dedicated | TestPlatformCallRefusesDedicated |
| 22 | POST /api/approvals/{id}/answer | answerApproval | answer_question | no evidence check | dedicated; W1 adds the evidence check (A7) | TestAnswerQuestionNeedsPersonMessage, TestPlatformCallRefusesDedicated |

## Workflows and templates

| # | Action | Backend | Chief before | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 23 | GET /api/workflows | listWorkflows | list_workflows (partial) | full fields | read | TestPlatformParity |
| 24 | POST /api/workflows | createWorkflow | — | create, with DAG validation in the handler | routine | TestChiefWorkflowCRUD |
| 25 | POST /api/workflows/validate | validateWorkflow | — | validate (structure, agents, schemas) | read-like; routine with no side effect | TestChiefWorkflowCRUD (an invalid definition is refused), TestPlatformCallRoutine |
| 26 | GET /api/workflows/{id} | getWorkflow | — | read, ETag | read | TestChiefWorkflowCRUD |
| 27 | PUT /api/workflows/{id} | updateWorkflow (version bump; runs keep their snapshot) | — | update, version | routine | TestChiefWorkflowCRUD (runs in flight stay on their version) |
| 28 | DELETE /api/workflows/{id} | deleteWorkflow (soft) | — | delete | destructive | TestChiefWorkflowCRUD |
| 29 | GET /api/workflow-templates | listTemplates | — | read | read | TestPlatformParity |
| 30 | POST /api/workflow-templates/{key}/instantiate | instantiateTemplate | — | copy | routine | TestPlatformParity |
| 84 | **new** GET /api/workflows/{id}/versions, GET …/versions/{version} | listWorkflowVersions, getWorkflowVersion | — | versions read (PRD 5) | read, W4 | TestWorkflowVersionsWritten, TestWorkflowVersionsBackfill, TestChiefWorkflowCRUD |
| 85 | **new** POST /api/workflows/{id}/versions/{version}/restore | restoreWorkflowVersion | — | rollback (PRD 5) | routine, W4 | TestWorkflowRestore, TestChiefWorkflowCRUD |

## Runs

| # | Action | Backend | Chief before | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 31 | GET /api/runs | listRuns | — | list | read | TestPlatformParity |
| 32 | POST /api/runs | createRun | delegate_task (no limits/model, no key) | limits, model, idempotency | routine; privileged with limits above the defaults (A5). `trigger=eval` and `script_id` are refused for the Chief (machine fields) | TestPlatformCallRefusesMachine, TestPrivilegedNeedsApproval |
| 33 | GET /api/runs/{id} | getRun | run_status | — | read | TestPlatformParity |
| 34 | POST /api/runs/{id}/cancel | cancelRun (a command: received, acknowledged, applied when the gateway confirms the stop) | — | — | routine, W2 | TestRunEndpoints, TestCommandLifecycle, TestCancelLiveConfirmedByEvent, TestCancelUnconfirmedThenReconciled |
| 35 | POST /api/runs/{id}/retry | retryRun (a command; `from_node` picks the step; refused while a stop is unconfirmed) | — | — | routine, W2 | TestRetryRunAccepts, TestRetryFromStep, TestRetryRefusedUnconfirmedCancel, TestResumeRefusedWhileBlocked |
| 35a | **new** POST /api/runs/{id}/pause | pauseRun | — | run pause (PRD 3) | routine, W2 | TestPauseResume, TestPauseOnWaitingRejected |
| 35b | **new** POST /api/runs/{id}/resume | resumeRun | — | run resume with a note (PRD 3) | routine, W2 | TestPauseResume, TestResumeRefusedUnconfirmedStop, TestResumeRefusedWhileBlocked |
| 35c | **new** POST /api/runs/{id}/steer | steerRun | — | operator text into a live step (PRD 3) | routine, W2 | TestSteerLive, TestSteerBetweenTurns |
| 35d | **new** GET /api/runs/{id}/commands | listRunCommands | — | command status (PRD 3) | read, W2 | TestRunCommandReads |
| 35e | **new** GET /api/runs/{id}/commands/{cid} | getRunCommand | — | one command's status (PRD 3) | read, W2 | TestRunCommandReads, TestCommandLifecycle |
| 36 | GET /api/runs/{id}/trace | getTrace | run_status (subset) | full trace | read (large; the tool trims it) | TestPlatformParity |
| 37 | GET /api/runs/{id}/artifacts | listArtifacts | run_status (names) | read | read | TestPlatformParity |
| 38 | GET /api/runs/{id}/artifacts/{artifactId} | downloadArtifact | — | binary | read; the tool returns text up to 32 KiB and metadata for binaries | TestPlatformCallBinary, TestPlatformCallArtifacts |
| 39 | POST /api/evals/scripts | registerScript | — | eval only | machine | TestPlatformCallRefusesMachine |

## Schedules

| # | Action | Backend | Chief before | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 40 | GET /api/schedules | listSchedules (with next_run_at) | — | read | read | TestScheduleEnabledTimezoneNextRun |
| 41 | POST /api/schedules | createSchedule (cron, timezone) | — | create | routine | TestChiefScheduleCRUD |
| 42 | GET /api/schedules/{id} | getSchedule | — | read, ETag | read | TestChiefScheduleCRUD |
| 43 | PUT /api/schedules/{id} | updateSchedule (enabled = pause/resume; recomputes next_run_at) | — | update, pause/resume | routine | TestChiefScheduleCRUD, TestScheduleEnabledTimezoneNextRun, TestAuditRevert |
| 44 | DELETE /api/schedules/{id} | deleteSchedule (hard) | — | delete | destructive | TestChiefScheduleCRUD, TestDestructiveNeedsApproval |

## Skills

| # | Action | Backend | Chief before | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 45, 47, 52, 53, 56 | GET skills, skill, files, file, export | listSkills, getSkill, listSkillFiles, getSkillFile, exportSkill | — | read (export is a zip: metadata only) | read | TestPlatformParity |
| 46, 48 | POST, PUT /api/skills[/{id}] | createSkill, updateSkill (a rename is pushed to agents) | — | write | routine | TestSkillCRUD, TestAuditRevert |
| 49 | DELETE /api/skills/{id} | deleteSkill (hard; removed from agents) | — | delete | destructive | TestDestructiveNeedsApproval |
| 50 | POST /api/skills/import (zip upload) | importSkillZip | — | multipart | machine (not expressible as JSON); import-git covers it | TestPlatformCallRefusesMachine |
| 51 | POST /api/skills/import-git | importSkillGit (host allowlist) | — | import | routine | TestSkillGitImport |
| 54 | PUT …/files/{path...} | putSkillFile | — | edit | routine | TestSkillFileAPI |
| 55 | DELETE …/files/{path...} | deleteSkillFile | — | delete | destructive | TestDestructiveNeedsApproval |

## Projects, plans, waves

| # | Action | Backend | Chief before | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 57 | GET /api/projects | listProjects | list_projects | — | read | TestPlatformParity |
| 58 | POST /api/projects | createProject | add_project (no token, caps or description) | full fields | routine; the Chief never sends a token (A8) | TestPlatformCallRefusesSecrets |
| 59 | GET /api/projects/{id} | getProject | — | read, ETag | read | TestPlatformParity |
| 60 | PUT /api/projects/{id} | updateProject (full replace) | — | update | routine; privileged when the token, repository or provider changes or a budget cap rises (A5) | TestPrivilegedNeedsApproval, TestAuditRevert |
| 61 | DELETE /api/projects/{id} | deleteProject (soft) | — | delete | destructive | TestDestructiveNeedsApproval |
| 62 | POST /api/projects/{id}/check | checkProject | — | check token | routine | TestPlatformParity |
| 63 | POST /api/projects/{id}/intake/poll | pollIntake | — | poll now | routine | TestPollIntakeEndpointT5239 |
| 64, 65 | GET project plans, plan | listPlans, getPlan | list_projects (open_plan) | full plan | read | TestPlatformParity |
| 66 | POST /api/plans/{id}/cancel | cancelPlan | — | cancel | destructive | TestCancelPlanAPI, TestDestructiveNeedsApproval |
| 67 | POST /api/plans/{id}/tickets/{ticket}/changes | requestChanges | delegate_task with merge_request | — | routine | TestRequestChangesAPI |
| 68, 69 | POST waves pause/resume | pauseWaves, resumeWaves | — | pause/resume | routine | TestPlatformParity |
| 70 | GET /api/projects/{id}/facts | listProjectFacts | — | read | read | TestPlatformParity |
| 71 | DELETE …/facts/{factId} | deleteProjectFact | — | delete | destructive | TestDestructiveNeedsApproval |

## Merge requests

| # | Action | Backend | Chief before | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 72, 73 | GET merge requests, merge request | listMergeRequests, getMergeRequest (stored record) | read_merge_request (provider read) | exact-SHA checks, diff, mergeability, CI logs (PRD 4) | read; W3 adds the new reads | TestMergeRequestListAndGet |
| 74 | POST /api/merge-requests/{id}/sync | syncMergeRequest | — | sync | routine | TestSyncEndpoint |
| 75 | POST /api/merge-requests/{id}/merge | mergeMergeRequest (CI passed on the head SHA; the provider gets the expected SHA) | — | exact-SHA guard (W3) | destructive (merging to the default branch) | TestMergeNeedsExactSHA, TestDestructiveNeedsApproval |
| 86 | **new** GET …/diff, …/checks, …/jobs/{job}/log | getMergeRequestDiff, getMergeRequestChecks, getMergeRequestJobLog | — | PRD 4 | read, W3 | TestMergeRequestDiffChecksLog |
| 87 | **new** POST …/update-from-default | updateMergeRequestFromDefault | — | safe branch update with expected head (PRD 4) | routine, W3 | TestUpdateFromDefault, TestUpdateFromDefaultEndpoint |
| — | **new** dependencies (GET, PUT /api/merge-requests/{id}/dependencies) | getMergeRequestDependencies, putMergeRequestDependencies | — | dependency gating (PRD 4) | read (GET), routine (PUT), W3 | TestDependencyEdges, TestRunOnDependentParks, TestDependencyAcceptance, TestDependencyAcceptanceResumesRun |

## Channels and conversations

| # | Action | Backend | Chief before | Gap | Plan (risk, wave) | Acceptance |
|---|---|---|---|---|---|---|
| 78 | GET /api/channels | getChannels | — | read | read | TestPlatformParity |
| 79 | PUT /api/channels/telegram/binding | bindTelegram | — | rebinding moves the operator's channel | privileged | TestPrivilegedNeedsApproval |
| 80 | DELETE /api/channels/telegram/binding | unbindTelegram | — | the Chief would cut its own line | destructive | TestDestructiveNeedsApproval |
| 81-83 | GET conversations, messages, tool calls | listConversations, listConversationMessages, listConversationToolCalls | — | read | read | TestPlatformParity |

## Not routes: Chief-only tools that stay

`delegate_task`, `run_status`, `list_*`, `decide_approval`, `answer_question`,
`add_project`, `memory_save`/`memory_recall` and `read_merge_request` stay: they are
shaped for chat and carry the evidence rules. `platform_call` adds the parity, and these are
not removed.
