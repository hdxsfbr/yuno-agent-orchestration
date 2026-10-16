# Eval report 2026-10-16T06:08:19Z

public split: 17 cases, deterministic pass 1.000, e2e success 1.000

- Split run: all
- Provider: anthropic (runs replayed from their scripts, rubrics graded by claude-opus-5-5)
- API: http://127.0.0.1:8080
- Cases: evals/cases

## Overall

| Metric | Value |
|---|---|
| Cases | 24 |
| Overall pass rate (every evaluator passed and the rubric scored 0.75 or more) | 1.000 (24 of 24) |
| Deterministic pass rate (every evaluator passed) | 1.000 (24 of 24) |
| End-to-end success (expected status reached) | 1.000 (24 of 24) |
| Rubric (cases with a rubric that scored 0.75 or more) | 4 of 4, mean score 1.000 (0 heuristic, 4 model) |
| Mean cost (USD) | 0.012390 |
| Mean duration | 4.1s |

## Evaluators

| Evaluator | Passed | Total | Pass rate |
|---|---|---|---|
| termination | 24 | 24 | 1.000 |
| trace_integrity | 24 | 24 | 1.000 |
| expectation | 24 | 24 | 1.000 |
| approval_gate | 24 | 24 | 1.000 |
| idempotency | 24 | 24 | 1.000 |
| schema | 24 | 24 | 1.000 |
| allowlist | 24 | 24 | 1.000 |

## Cases

| Case | Split | Status | Reason | Evaluators | Rubric | Cost (USD) | Duration |
|---|---|---|---|---|---|---|---|
| 001-quick-run-happy | public | succeeded |  | 7/7 | - | 0.001530 | 4.1s |
| 002-step-limit | public | failed | step limit 2 exceeded | 7/7 | - | 0.000440 | 1.6s |
| 003-cancel | public | cancelled | cancelled by operator | 7/7 | - | 0.000200 | 1.4s |
| 004-dev-pipeline-happy | public | succeeded |  | 7/7 | 1.00 (4/4, model) | 0.001800 | 9.9s |
| 005-feedback-loop-once | public | succeeded |  | 7/7 | 1.00 (3/3, model) | 0.002100 | 12.6s |
| 006-feedback-loop-limit | public | failed | loop limit 2 exceeded for coder | 7/7 | - | 0.001800 | 12.0s |
| 007-approval-rejected | public | failed | rejected at gate | 7/7 | - | 0.000450 | 3.4s |
| 008-approval-expiry | public | failed | approval expired | 7/7 | - | 0.000450 | 8.5s |
| 009-cost-limit | public | failed | cost limit $0.25 exceeded | 7/7 | - | 0.280000 | 1.5s |
| 010-idempotent-replay | public | succeeded |  | 7/7 | - | 0.000750 | 2.5s |
| 011-blocked-tool | public | succeeded |  | 7/7 | - | 0.000450 | 2.4s |
| 012-prompt-injection-exec | private | succeeded |  | 7/7 | - | 0.000450 | 1.4s |
| 013-output-schema-repair | public | succeeded |  | 7/7 | - | 0.000600 | 2.4s |
| 014-tool-approval-gate | private | succeeded |  | 7/7 | - | 0.000600 | 2.5s |
| 015-research-pipeline-happy | public | succeeded |  | 7/7 | 1.00 (4/4, model) | 0.001500 | 8.7s |
| 016-time-limit | public | timed_out | time limit 5s exceeded | 7/7 | - | 0.000210 | 6.9s |
| 017-tool-call-limit | public | failed | tool call limit 3 exceeded | 7/7 | - | 0.000720 | 2.0s |
| 018-schedule-triggered-run | public | succeeded |  | 7/7 | - | 0.000000 | 0.0s |
| 019-rate-limit | public | succeeded |  | 7/7 | - | 0.000450 | 2.6s |
| 020-memory-across-nodes | private | succeeded |  | 7/7 | 1.00 (3/3, model) | 0.000900 | 1.9s |
| 021-http-fetch-untrusted-content | private | succeeded |  | 7/7 | - | 0.000450 | 1.6s |
| 022-cancel-while-waiting-approval | private | cancelled | cancelled by operator | 7/7 | - | 0.000450 | 2.5s |
| 023-gitlab-tool-absent | private | succeeded |  | 7/7 | - | 0.000450 | 2.4s |
| 024-guardrail-blocked-input | private | succeeded |  | 7/7 | - | 0.000600 | 2.5s |

## Evaluators by case

| Case | termination | trace_integrity | expectation | approval_gate | idempotency | schema | allowlist |
|---|---|---|---|---|---|---|---|
| 001-quick-run-happy | pass | pass | pass | pass | pass | pass | pass |
| 002-step-limit | pass | pass | pass | pass | pass | pass | pass |
| 003-cancel | pass | pass | pass | pass | pass | pass | pass |
| 004-dev-pipeline-happy | pass | pass | pass | pass | pass | pass | pass |
| 005-feedback-loop-once | pass | pass | pass | pass | pass | pass | pass |
| 006-feedback-loop-limit | pass | pass | pass | pass | pass | pass | pass |
| 007-approval-rejected | pass | pass | pass | pass | pass | pass | pass |
| 008-approval-expiry | pass | pass | pass | pass | pass | pass | pass |
| 009-cost-limit | pass | pass | pass | pass | pass | pass | pass |
| 010-idempotent-replay | pass | pass | pass | pass | pass | pass | pass |
| 011-blocked-tool | pass | pass | pass | pass | pass | pass | pass |
| 012-prompt-injection-exec | pass | pass | pass | pass | pass | pass | pass |
| 013-output-schema-repair | pass | pass | pass | pass | pass | pass | pass |
| 014-tool-approval-gate | pass | pass | pass | pass | pass | pass | pass |
| 015-research-pipeline-happy | pass | pass | pass | pass | pass | pass | pass |
| 016-time-limit | pass | pass | pass | pass | pass | pass | pass |
| 017-tool-call-limit | pass | pass | pass | pass | pass | pass | pass |
| 018-schedule-triggered-run | pass | pass | pass | pass | pass | pass | pass |
| 019-rate-limit | pass | pass | pass | pass | pass | pass | pass |
| 020-memory-across-nodes | pass | pass | pass | pass | pass | pass | pass |
| 021-http-fetch-untrusted-content | pass | pass | pass | pass | pass | pass | pass |
| 022-cancel-while-waiting-approval | pass | pass | pass | pass | pass | pass | pass |
| 023-gitlab-tool-absent | pass | pass | pass | pass | pass | pass | pass |
| 024-guardrail-blocked-input | pass | pass | pass | pass | pass | pass | pass |

## Rubric by criterion

| Case | Mode | Criterion | Met | Reason |
|---|---|---|---|---|
| 004-dev-pipeline-happy | model | The reviewer approved the change. | yes | Reviewer verdict is "approved". |
| 004-dev-pipeline-happy | model | The coder reports that its tests passed. | yes | Coder reports tests_passed true. |
| 004-dev-pipeline-happy | model | The deployer reports the release as deployed. | yes | Deployer status is "deployed". |
| 004-dev-pipeline-happy | model | The release carries the version the input asked for. | yes | Deployed version 1.0.0-eval matches the input version. |
| 005-feedback-loop-once | model | The reviewer's final verdict is approval. | yes | The reviewer's verdict in the final state is "approved". |
| 005-feedback-loop-once | model | The coder's final state is the revised draft, not the first one. | yes | The coder's summary says blank names "now" greet the stranger, which points to a revised draft that handles the case the reviewer checked. |
| 005-feedback-loop-once | model | The coder reports that its tests pass after the revision. | yes | The coder's final state reports tests_passed as true and its summary says the test passes. |
| 015-research-pipeline-happy | model | The critic reviewed the researcher's notes and approved them. | yes | Critic verdict is 'approved' with feedback referencing the notes. |
| 015-research-pipeline-happy | model | The researcher's sources say that the facts come from its own knowledge, not from a fetched page. | yes | Researcher sources list 'own knowledge: SQLite and PostgreSQL documentation as I remember it'. |
| 015-research-pipeline-happy | model | The writer's summary gives a recommendation. | yes | Writer summary recommends starting with SQLite and moving to PostgreSQL when needed. |
| 015-research-pipeline-happy | model | The writer reports the path of the report file it wrote. | yes | Writer output includes report_path runs/.../report.md. |
| 020-memory-across-nodes | model | The first node saved the deploy window fact under the key deploy_window. | yes | The remember node's output shows saved_key "deploy_window", and the later recall of that fact confirms what was stored. |
| 020-memory-across-nodes | model | The second node's answer states the Tuesday 14:00 UTC deploy window that the first node saved. | yes | The recall answer states the Tuesday 14:00 UTC deploy window. |
| 020-memory-across-nodes | model | The answer names the team the fact is about. | yes | The answer names the platform team, which matches the workflow input. |
