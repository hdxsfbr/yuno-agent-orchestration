# Eval report 2026-10-15T19:35:50Z

public split: 3 cases, deterministic pass 1.000, e2e success 1.000

- Split run: public
- Provider: scripted
- API: http://127.0.0.1:8080
- Cases: evals/cases

## Overall

| Metric | Value |
|---|---|
| Cases | 3 |
| Overall pass rate (every evaluator passed) | 1.000 (3 of 3) |
| End-to-end success (expected status reached) | 1.000 (3 of 3) |
| Mean cost (USD) | 0.000723 |
| Mean duration | 2.5s |

## Evaluators

| Evaluator | Passed | Total | Pass rate |
|---|---|---|---|
| termination | 3 | 3 | 1.000 |
| trace_integrity | 3 | 3 | 1.000 |
| expectation | 3 | 3 | 1.000 |

## Cases

| Case | Split | Status | Reason | Evaluators | Cost (USD) | Duration |
|---|---|---|---|---|---|---|
| 001-quick-run-happy | public | succeeded |  | 3/3 | 0.001530 | 4.3s |
| 002-step-limit | public | failed | step limit 2 exceeded | 3/3 | 0.000440 | 1.7s |
| 003-cancel | public | cancelled | cancelled by operator | 3/3 | 0.000200 | 1.6s |
