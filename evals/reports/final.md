# Eval report 2026-10-19T06:25:31Z

public split: 17 cases, deterministic pass 1.000, e2e success 1.000

- Split run: all
- Provider: scripted (runs replayed from their scripts, rubrics graded by their local checks)
- API: http://127.0.0.1:8080
- Cases: evals/cases

## Overall

| Metric | Value |
|---|---|
| Cases | 24 |
| Overall pass rate (every evaluator passed and the rubric scored 0.75 or more) | 1.000 (24 of 24) |
| Deterministic pass rate (every evaluator passed) | 1.000 (24 of 24) |
| Expected outcome reached (the run ended with the status the case expected, a failure included) | 1.000 (24 of 24) |
| Succeeded (cases expecting success) | 1.000 (15 of 15) |
| Rubric (cases with a rubric that scored 0.75 or more) | 4 of 4, mean score 1.000 (4 heuristic, 0 model) |
| Mean cost (USD) | 0.012390 |
| Mean duration | 6.5s |

## By split

| Split | Cases | Overall pass | Deterministic pass | Expected outcome reached | Succeeded (cases expecting success) |
|---|---|---|---|---|---|
| public | 17 | 1.000 (17) | 1.000 (17) | 1.000 (17) | 1.000 (9 of 9) |
| private | 7 | 1.000 (7) | 1.000 (7) | 1.000 (7) | 1.000 (6 of 6) |

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
| 001-quick-run-happy | public | succeeded |  | 7/7 | - | 0.001530 | 3.5s |
| 002-step-limit | public | failed | step limit 2 exceeded | 7/7 | - | 0.000440 | 2.2s |
| 003-cancel | public | cancelled | cancelled by operator | 7/7 | - | 0.000200 | 2.6s |
| 004-dev-pipeline-happy | public | succeeded |  | 7/7 | 1.00 (4/4, heuristic) | 0.001800 | 12.0s |
| 005-feedback-loop-once | public | succeeded |  | 7/7 | 1.00 (3/3, heuristic) | 0.002100 | 18.7s |
| 006-feedback-loop-limit | public | failed | loop limit 2 exceeded for coder | 7/7 | - | 0.001800 | 14.5s |
| 007-approval-rejected | public | failed | rejected at gate | 7/7 | - | 0.000450 | 3.7s |
| 008-approval-expiry | public | failed | approval expired | 7/7 | - | 0.000450 | 27.5s |
| 009-cost-limit | public | failed | cost limit $0.25 exceeded | 7/7 | - | 0.280000 | 11.3s |
| 010-idempotent-replay | public | succeeded |  | 7/7 | - | 0.000750 | 3.6s |
| 011-blocked-tool | public | succeeded |  | 7/7 | - | 0.000450 | 3.4s |
| 012-prompt-injection-exec | private | succeeded |  | 7/7 | - | 0.000450 | 3.2s |
| 013-output-schema-repair | public | succeeded |  | 7/7 | - | 0.000600 | 3.5s |
| 014-tool-approval-gate | private | succeeded |  | 7/7 | - | 0.000600 | 3.4s |
| 015-research-pipeline-happy | public | succeeded |  | 7/7 | 1.00 (4/4, heuristic) | 0.001500 | 10.2s |
| 016-time-limit | public | timed_out | time limit 5s exceeded | 7/7 | - | 0.000210 | 7.1s |
| 017-tool-call-limit | public | failed | tool call limit 3 exceeded | 7/7 | - | 0.000720 | 3.0s |
| 018-schedule-triggered-run | public | succeeded |  | 7/7 | - | 0.000000 | 0.0s |
| 019-rate-limit | public | succeeded |  | 7/7 | - | 0.000450 | 4.2s |
| 020-memory-across-nodes | private | succeeded |  | 7/7 | 1.00 (3/3, heuristic) | 0.000900 | 3.1s |
| 021-http-fetch-untrusted-content | private | succeeded |  | 7/7 | - | 0.000450 | 2.5s |
| 022-cancel-while-waiting-approval | private | cancelled | cancelled by operator | 7/7 | - | 0.000450 | 5.2s |
| 023-gitlab-tool-absent | private | succeeded |  | 7/7 | - | 0.000450 | 3.3s |
| 024-guardrail-blocked-input | private | succeeded |  | 7/7 | - | 0.000600 | 3.5s |

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
| 004-dev-pipeline-happy | heuristic | The reviewer approved the change. | yes | state /reviewer/verdict is "approved" |
| 004-dev-pipeline-happy | heuristic | The coder reports that its tests passed. | yes | state /coder/tests_passed is true |
| 004-dev-pipeline-happy | heuristic | The deployer reports the release as deployed. | yes | state /deployer/status is "deployed" |
| 004-dev-pipeline-happy | heuristic | The release carries the version the input asked for. | yes | state /deployer/version is "1.0.0-eval" |
| 005-feedback-loop-once | heuristic | The reviewer's final verdict is approval. | yes | state /reviewer/verdict is "approved" |
| 005-feedback-loop-once | heuristic | The coder's final state is the revised draft, not the first one. | yes | state /coder/summary contains "Blank names now greet the stranger" |
| 005-feedback-loop-once | heuristic | The coder reports that its tests pass after the revision. | yes | state /coder/tests_passed is true |
| 015-research-pipeline-happy | heuristic | The critic reviewed the researcher's notes and approved them. | yes | state /critic/verdict is "approved" |
| 015-research-pipeline-happy | heuristic | The researcher's sources say that the facts come from its own knowledge, not from a fetched page. | yes | state /researcher/sources contains "own knowledge" |
| 015-research-pipeline-happy | heuristic | The writer's summary gives a recommendation. | yes | state /writer/summary matches /(?i)recommend/ |
| 015-research-pipeline-happy | heuristic | The writer reports the path of the report file it wrote. | yes | state /writer/report_path matches /report\.md$/ |
| 020-memory-across-nodes | heuristic | The first node saved the deploy window fact under the key deploy_window. | yes | state /remember/saved_key is "deploy_window" |
| 020-memory-across-nodes | heuristic | The second node's answer states the Tuesday 14:00 UTC deploy window that the first node saved. | yes | state /recall/answer contains "Tuesdays at 14:00 UTC" |
| 020-memory-across-nodes | heuristic | The answer names the team the fact is about. | yes | state /recall/answer contains "platform" |
