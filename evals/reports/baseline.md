# Eval report 2026-10-16T02:59:37Z

public split: 17 cases, deterministic pass 1.000, e2e success 1.000

- Split run: public
- Provider: scripted (runs replayed from their scripts, rubrics graded by their local checks)
- API: http://127.0.0.1:8080
- Cases: evals/cases

## Overall

| Metric | Value |
|---|---|
| Cases | 17 |
| Overall pass rate (every evaluator passed and the rubric scored 0.75 or more) | 1.000 (17 of 17) |
| Deterministic pass rate (every evaluator passed) | 1.000 (17 of 17) |
| End-to-end success (expected status reached) | 1.000 (17 of 17) |
| Rubric (cases with a rubric that scored 0.75 or more) | 3 of 3, mean score 1.000 (3 heuristic, 0 model) |
| Mean cost (USD) | 0.017262 |
| Mean duration | 6.6s |

## Evaluators

| Evaluator | Passed | Total | Pass rate |
|---|---|---|---|
| termination | 17 | 17 | 1.000 |
| trace_integrity | 17 | 17 | 1.000 |
| expectation | 17 | 17 | 1.000 |
| approval_gate | 17 | 17 | 1.000 |
| idempotency | 17 | 17 | 1.000 |
| schema | 17 | 17 | 1.000 |
| allowlist | 17 | 17 | 1.000 |

## Cases

| Case | Split | Status | Reason | Evaluators | Rubric | Cost (USD) | Duration |
|---|---|---|---|---|---|---|---|
| 001-quick-run-happy | public | succeeded |  | 7/7 | - | 0.001530 | 4.1s |
| 002-step-limit | public | failed | step limit 2 exceeded | 7/7 | - | 0.000440 | 2.0s |
| 003-cancel | public | cancelled | cancelled by operator | 7/7 | - | 0.000200 | 3.3s |
| 004-dev-pipeline-happy | public | succeeded |  | 7/7 | 1.00 (4/4, heuristic) | 0.001800 | 13.5s |
| 005-feedback-loop-once | public | succeeded |  | 7/7 | 1.00 (3/3, heuristic) | 0.002100 | 20.0s |
| 006-feedback-loop-limit | public | failed | loop limit 2 exceeded for coder | 7/7 | - | 0.001800 | 16.0s |
| 007-approval-rejected | public | failed | rejected at gate | 7/7 | - | 0.000450 | 4.8s |
| 008-approval-expiry | public | failed | approval expired | 7/7 | - | 0.000450 | 10.0s |
| 009-cost-limit | public | failed | cost limit $0.25 exceeded | 7/7 | - | 0.280000 | 2.0s |
| 010-idempotent-replay | public | succeeded |  | 7/7 | - | 0.000750 | 4.5s |
| 011-blocked-tool | public | succeeded |  | 7/7 | - | 0.000450 | 3.0s |
| 013-output-schema-repair | public | succeeded |  | 7/7 | - | 0.000600 | 4.6s |
| 015-research-pipeline-happy | public | succeeded |  | 7/7 | 1.00 (4/4, heuristic) | 0.001500 | 11.3s |
| 016-time-limit | public | timed_out | time limit 5s exceeded | 7/7 | - | 0.000210 | 5.9s |
| 017-tool-call-limit | public | failed | tool call limit 3 exceeded | 7/7 | - | 0.000720 | 3.0s |
| 018-schedule-triggered-run | public | succeeded |  | 7/7 | - | 0.000000 | 0.0s |
| 019-rate-limit | public | succeeded |  | 7/7 | - | 0.000450 | 4.4s |

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
| 013-output-schema-repair | pass | pass | pass | pass | pass | pass | pass |
| 015-research-pipeline-happy | pass | pass | pass | pass | pass | pass | pass |
| 016-time-limit | pass | pass | pass | pass | pass | pass | pass |
| 017-tool-call-limit | pass | pass | pass | pass | pass | pass | pass |
| 018-schedule-triggered-run | pass | pass | pass | pass | pass | pass | pass |
| 019-rate-limit | pass | pass | pass | pass | pass | pass | pass |

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
