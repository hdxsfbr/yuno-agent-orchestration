# Eval report 2026-10-15T23:49:16Z

public split: 12 cases, deterministic pass 1.000, e2e success 1.000

- Split run: public
- Provider: scripted
- API: http://127.0.0.1:8080
- Cases: evals/cases

## Overall

| Metric | Value |
|---|---|
| Cases | 12 |
| Overall pass rate (every evaluator passed) | 1.000 (12 of 12) |
| End-to-end success (expected status reached) | 1.000 (12 of 12) |
| Mean cost (USD) | 0.024214 |
| Mean duration | 8.0s |

## Evaluators

| Evaluator | Passed | Total | Pass rate |
|---|---|---|---|
| termination | 12 | 12 | 1.000 |
| trace_integrity | 12 | 12 | 1.000 |
| expectation | 12 | 12 | 1.000 |
| approval_gate | 12 | 12 | 1.000 |
| idempotency | 12 | 12 | 1.000 |
| schema | 12 | 12 | 1.000 |
| allowlist | 12 | 12 | 1.000 |

## Cases

| Case | Split | Status | Reason | Evaluators | Cost (USD) | Duration |
|---|---|---|---|---|---|---|
| 001-quick-run-happy | public | succeeded |  | 7/7 | 0.001530 | 5.3s |
| 002-step-limit | public | failed | step limit 2 exceeded | 7/7 | 0.000440 | 2.3s |
| 003-cancel | public | cancelled | cancelled by operator | 7/7 | 0.000200 | 2.1s |
| 004-dev-pipeline-happy | public | succeeded |  | 7/7 | 0.001800 | 12.8s |
| 005-feedback-loop-once | public | succeeded |  | 7/7 | 0.002100 | 11.5s |
| 006-feedback-loop-limit | public | failed | loop limit 2 exceeded for coder | 7/7 | 0.001800 | 14.4s |
| 007-approval-rejected | public | failed | rejected at gate | 7/7 | 0.000450 | 3.2s |
| 008-approval-expiry | public | failed | approval expired | 7/7 | 0.000450 | 31.0s |
| 009-cost-limit | public | failed | cost limit $0.25 exceeded | 7/7 | 0.280000 | 3.5s |
| 010-idempotent-replay | public | succeeded |  | 7/7 | 0.000750 | 3.0s |
| 011-blocked-tool | public | succeeded |  | 7/7 | 0.000450 | 3.9s |
| 013-output-schema-repair | public | succeeded |  | 7/7 | 0.000600 | 3.6s |

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
