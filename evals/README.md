# Evaluation cases and reports

The eval runner (`yuno eval`, `make eval`) is a plain HTTP client of the api. For each
case it creates a temporary agent, registers the case's script with the scripted model
provider, runs the task, waits for the run to end, judges the stored trace with the
evaluators below and deletes the agent. It needs no cloud access: the model is
`scripted/scripted-1` and replays the script in the case.

```
make eval                      # EVAL_SPLIT=public by default
EVAL_SPLIT=all make eval       # every split
```

| Variable | Default | Meaning |
|---|---|---|
| `EVAL_API_URL` | `http://127.0.0.1:8080` | api to test |
| `EVAL_SPLIT` | `public` | `public`, `private` or `all` |
| `EVAL_CASES_DIR` | `evals/cases` | searched recursively for `*.yaml` / `*.yml` |
| `EVAL_REPORTS_DIR` | `evals/reports` | where `<timestamp>.md` and `.json` are written |
| `EVAL_PROVIDER` | `scripted` | only `scripted` is supported |
| `API_TOKEN` | empty | sent as a bearer token when set |

## Case format

Cases live in `evals/cases/<version>/`, for example `evals/cases/v1/001-quick-run-happy.yaml`.
A case's `version` must equal the name of its version directory (the first directory under
`evals/cases`); a mismatch, an unknown field or any other violation fails the load with the
file name in the message. Case ids are unique.

```yaml
id: 001-quick-run-happy            # lowercase letters, digits, '.', '_', '-'; at most 61 characters
version: v1                        # must equal the directory name
split: public                     # public | private
description: What the case shows.
agent:
  name: Quick Coder                # label for the case's agent, sent as the agent's role
  system_prompt: |                 # the agent's prompt
    ...
  tool_allowlist: [exec, write, finish]
  limits:                          # max_steps, max_duration_seconds, max_node_iterations,
    max_steps: 10                  # max_loop_visits, max_cost_usd, max_tool_calls,
    max_duration_seconds: 120      # approval_timeout_seconds; zero or absent = default
task: |                            # the user task; $RUN_ID is replaced (see below)
  ...
script:                            # what the scripted model does, one entry per model response
  - tool:                          # either a tool call ...
      name: exec
      args: { command: "python3 -c 'print(2+2)'" }
    usage: { prompt: 140, completion: 24 }   # optional token usage the provider reports
  - text: Done.                    # ... or a plain text reply
actions:                           # optional things the runner does to the run
  cancel_after_steps: 1            # POST cancel once step_count >= 1
expect:
  status: succeeded                # required: succeeded | failed | cancelled | timed_out
  failure_reason_contains: ...     # optional, case-insensitive substring
  min_tool_calls: 2                # optional
  min_model_calls: 3               # optional
rubric: []                         # reserved for the rubric evaluator (empty for now)
```

Script steps have exactly one of `text` or `tool`. Tool `args` is a YAML mapping and is
posted as a JSON object. Two names matter and are not the same thing:

- `agent.tool_allowlist` uses registry names as listed by `GET /api/tools`: `exec`,
  `write`, `read`, `ls`, `finish`, ...
- A script step's `tool.name` is what the model calls: OpenClaw built-ins keep their names
  (`exec`, `read`, `write`, `ls`) and platform tools carry the server prefix
  (`yuno__finish`).

A text-only step ends the model's turn, so a case that must keep the agent working (a limit
case, a cancel case) uses tool calls for every step it wants to be executed.

### Placeholders

Cases are written before a run exists, so some values are filled in later by the platform:

| Placeholder | Where | Replaced by |
|---|---|---|
| `$RUN_ID` | `task` | the engine, with the id of the run, in the task message it sends the agent |
| `$RUN_ID` | string values in a script step's `args` | the scripted provider, with the id of the run |
| `$RUN_TOKEN` | string values in a script step's `args` | the scripted provider, with the run token from the task message; a `yuno__finish` step is `{run_token: "$RUN_TOKEN", result: {...}}` |

The runner posts the script exactly as written; it does not substitute anything.

## What the runner does per case

1. `POST /api/agents` creates `eval-<id>-<random>` with the default model and the case's
   prompt, allowlist and limits.
2. `POST /api/evals/scripts` registers the script as `<id>-<random>` (the same random
   suffix).
3. `POST /api/runs` triggers a quick run with model `scripted/scripted-1`, that script id and
   the case's limits.
4. `GET /api/runs/{id}` every 500 ms until the status is terminal, for at most 180 s. A run
   that has not finished by then is cancelled and recorded as failed with the reason
   `runner timeout`. With `actions.cancel_after_steps`, the runner posts one cancel as soon
   as `step_count` reaches that value.
5. `GET /api/runs/{id}/trace`, then the evaluators.
6. `DELETE /api/agents/{id}`, whatever happened.

Cases run one at a time. An api error, a panic or a timeout in one case marks that case
failed (every evaluator fails with the reason) and the suite carries on.

## Evaluators

All three are pure functions of the case and the trace and run on every case.

| Evaluator | Passes when |
|---|---|
| `termination` | the run status is terminal and has `finished_at`; `step_count <= max_steps`; `finished_at - started_at - paused_total_ms <= max_duration_seconds`. Limits come from the run's own snapshot, then the case's agent limits, then the platform defaults. A run that failed with a `step limit` reason may show one step over: the step that trips the limit is recorded before the abort. |
| `trace_integrity` | every tool call has a terminal status (`executed`, `rejected`, `blocked`, `expired`, `failed`) and `finished_at`; every model call has a model and no negative token count; the run has at least one span |
| `expectation` | the status equals `expect.status`, the failure reason contains `failure_reason_contains` (ignoring case), and the trace has at least `min_tool_calls` tool calls and `min_model_calls` model calls |

The rubric evaluator is not part of this release. A case with an empty `rubric` scores 1 by
definition; the score is kept in the JSON report only.

## Reports and exit status

Each run writes `<UTC timestamp>.md` and `.json` to `EVAL_REPORTS_DIR` (`20261015T120000Z.md`);
an existing report is never overwritten. `baseline.md` and `final.md` are copies made by
`make eval-baseline` and `make eval-final`.

The report has the summary line, per-evaluator pass rates, the overall pass rate (cases on
which every evaluator passed), end-to-end success (cases whose run ended with `expect.status`),
mean cost and duration, a per-case table and a list of failures. The summary line is also
printed to standard output:

```
public split: 3 cases, deterministic pass 1.000, e2e success 1.000
```

The command exits 1 when the deterministic pass rate of the public split is below 0.9, after
writing the report. It also exits 1 when the cases cannot be loaded, the api never becomes
ready, or the run is interrupted before every case ran.

## Cases

| Id | Split | Shows | Expected |
|---|---|---|---|
| `001-quick-run-happy` | public | runs a command, writes `runs/$RUN_ID/notes.md`, finishes with a summary | succeeded, at least 2 tool calls |
| `002-step-limit` | public | an agent that keeps calling tools without finishing hits `max_steps: 2` | failed, reason contains `step limit 2 exceeded` |
| `003-cancel` | public | slow steps, cancelled by the runner after the first | cancelled |

To add a case, drop a file into the version directory, keep the `version` equal to the
directory, and run `go test ./internal/evals/...`: the shipped cases are loaded and validated
by the tests.
