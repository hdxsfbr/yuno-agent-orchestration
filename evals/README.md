# Evaluation cases and reports

The eval runner (`yuno eval`, `make eval`) is a plain HTTP client of the api. For each
case it creates the temporary agents the case needs (and, for a workflow case, the
workflow), registers the case's script with the scripted model provider, runs it, answers
the approvals the case says to answer, waits for the run to end, judges the stored trace
with the evaluators below and deletes what it created. It needs no cloud access: the model
is `scripted/scripted-1` and replays the script in the case.

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

A case is one of two kinds. A **quick run** has an `agent` and a `task`: one temporary
agent does one task. A **workflow case** has a `workflow`, an `input` and `agents`: an
inline workflow definition whose agent nodes name the case's agents.

### Quick run

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
  interaction_rules:               # optional: auto | require_approval | blocked per tool
    deploy_release: require_approval
  limits:                          # max_steps, max_duration_seconds, max_node_iterations,
    max_steps: 10                  # max_loop_visits, max_cost_usd, max_tool_calls,
    max_duration_seconds: 120      # approval_timeout_seconds; zero or absent = default
task: |                            # the user task; $RUN_ID is replaced (see below)
  ...
script: [...]                      # see "Script", "Actions" and "Expect" below
```

### Workflow case

```yaml
id: 004-dev-pipeline-happy
version: v1
split: public
description: What the case shows.
workflow:                          # a workflow definition; agent nodes use agent_name
  input_schema: {...}
  state_schema: {...}
  nodes:
    - { id: start, kind: start }
    - id: coder
      kind: agent
      agent_name: coder            # a key of agents below, not an agent id
      task: |                      # Go template over {input, state}
        ...
      output_schema: {...}
    - { id: gate, kind: approval, message: "..." }
    - { id: end, kind: end }
  edges:
    - { from: reviewer, to: coder, when: { path: /reviewer/verdict, op: eq, value: changes_requested } }
  limits: { max_steps: 40, max_loop_visits: 3 }   # the run's limits
input: { task: ..., version: ... } # the run input, checked against the definition's input_schema
agents:                            # one temporary agent per key
  coder:
    name: Software engineer        # optional role label, the key by default
    system_prompt: ...
    tool_allowlist: [read, write, finish]
    interaction_rules: { deploy_release: auto }   # optional
    limits: {...}                  # optional
```

The runner creates each agent as `eval-<case id>-<key>-<random>`, replaces every
`agent_name` by the id of that agent (the api only accepts `agent_name` in templates),
posts the definition as workflow `eval-<case id>-<random>` and triggers it with the case's
input, the definition's limits and the scripted model. The case is rejected at load time
for an unknown field in the definition, an agent node whose `agent_name` is not a key of
`agents`, an unused agent, a schema that does not compile, a task or message template that
does not render, or an input that does not match `input_schema`.

### Script

```yaml
script:                            # what the scripted model does, one entry per model response
  - tool:                          # either a tool call ...
      name: exec
      args: { command: "python3 -c 'print(2+2)'" }
    usage: { prompt: 140, completion: 24 }   # optional token usage the provider reports
  - text: Done.                    # ... or a plain text reply
```

There is one script per case, consumed in execution order across all of a workflow's
nodes: every model call takes the next step, whichever node makes it. A node visit ends
with a text step, so after a `yuno__finish` step the script needs a text step before the
next node's first step. The script must be consumed exactly: a missing step ends the run
with `script exhausted`, a spare one is a mistake in the case.

Script steps have exactly one of `text` or `tool`. Tool `args` is a YAML mapping and is
posted as a JSON object. Two names matter and are not the same thing:

- `agent.tool_allowlist` uses registry names as listed by `GET /api/tools`: `exec`,
  `write`, `read`, `ls`, `deploy_release`, `finish`, ...
- A script step's `tool.name` is what the model calls: OpenClaw built-ins keep their names
  (`exec`, `read`, `write`, `ls`) and platform tools carry the server prefix
  (`yuno__finish`, `yuno__deploy_release`).

A text-only step ends the model's turn, so a case that must keep the agent working (a limit
case, a cancel case) uses tool calls for every step it wants to be executed. A step's reported
`usage` must stay below the scripted model's 200000-token context window, or OpenClaw may
start compacting the conversation and spend script steps on a summary.

### Placeholders

Cases are written before a run exists, so some values are filled in later by the platform:

| Placeholder | Where | Replaced by |
|---|---|---|
| `$RUN_ID` | `task` and workflow node `task` | the engine, with the id of the run, in the task message it sends the agent |
| `$RUN_ID` | string values in a script step's `args` | the scripted provider, with the id of the run |
| `$RUN_TOKEN` | string values in a script step's `args` | the scripted provider, with the run token from the task message; a `yuno__finish` step is `{run_token: "$RUN_TOKEN", result: {...}}` |

The runner posts the script exactly as written; it does not substitute anything. A file
the script writes goes to `runs/$RUN_ID/<name>` in the writing agent's own workspace;
`deploy_release` takes `paths` relative to that directory (`paths: [greet.py]`) and reads
them from the workspace of the agent that calls it, so the files must have been written
earlier in the script by that same agent. In a workflow the deployer therefore writes
what it publishes (case 004 has it write `RELEASE.md`) rather than naming the coder's files.

### Actions

```yaml
actions:
  cancel_after_steps: 1            # POST cancel once step_count >= 1
  approvals:                       # human decisions, applied in order
    - { decision: approve, reason: Looks right. }   # approve | reject
    - { decision: reject }
  approval_wait_seconds: 40        # leave the first pending approval alone for this long
```

While the run waits for approval the runner lists the run's pending approvals
(`GET /api/approvals?run_id=<id>&status=pending`) and applies the next action to the
oldest one it has not acted on (`POST /api/approvals/<id>/approve` or `/reject` with the
reason). Actions are used up in order and apply to whatever kind of approval comes next,
a workflow approval node or a gated tool call. A pending approval with no action left is a
case error: the runner stops waiting, records it, cancels the run and still judges the
trace, rather than waiting out the case timeout. `approval_wait_seconds` is for expiry cases:
nothing is decided for that long after the first approval appears, and a run that ends
meanwhile (the approval expired) ends the wait.

### Expect

```yaml
expect:
  status: succeeded                # required: succeeded | failed | cancelled | timed_out
  failure_reason_contains: ...     # optional, case-insensitive substring
  min_tool_calls: 2                # optional
  min_model_calls: 3               # optional
  min_agent_messages: 2            # optional: hand-overs between agents in the trace
  state:                           # optional: JSON pointer into the final state -> value
    "/reviewer/verdict": approved  #   every entry is checked
  tool_statuses:                   # optional: tool -> statuses of its tool call rows
    deploy_release: [executed]     #   in order, each matched by its own row, other rows allowed
    other_tool: []                 #   an empty list: the run must have no row for the tool
  never_tool_statuses:             # optional: statuses no row of the tool may have; a row is not
    deploy_release: [executed]     #   required, for a call the runtime may refuse unrecorded
  approval_statuses: [approved]    # optional: exact statuses of the run's approvals in request
                                   #   order; [] means the run raised none
rubric: []                         # reserved for the rubric evaluator (empty for now)
```

For a quick run the final state is `{"agent": <the agent's result>}`; for a workflow it is
`{"<node id>": <that node's last result>}`.

## What the runner does per case

1. `POST /api/agents` creates `eval-<id>-<random>` (a quick run) or one
   `eval-<id>-<key>-<random>` per entry of `agents` (a workflow case) with the default
   model, the case's prompts, allowlists, interaction rules and limits. A workflow case
   then `POST /api/workflows` with the definition.
2. `POST /api/evals/scripts` registers the script as `<id>-<random>` (the same random
   suffix).
3. `POST /api/runs` triggers the run with model `scripted/scripted-1`, that script id and
   the case's limits: `{agent_id, task}` for a quick run, `{workflow_id, input}` for a
   workflow case.
4. `GET /api/runs/{id}` every 500 ms until the status is terminal, for at most 180 s. A run
   that has not finished by then is cancelled and recorded as failed with the reason
   `runner timeout`. With `actions.cancel_after_steps`, the runner posts one cancel as soon
   as `step_count` reaches that value; approval actions are applied as described above.
5. `GET /api/runs/{id}/trace`, then the evaluators.
6. `DELETE /api/workflows/{id}` and `DELETE /api/agents/{id}`, whatever happened.

Cases run one at a time. An api error, a panic or a timeout in one case marks that case
failed (every evaluator fails with the reason) and the suite carries on. The cases that
publish a release (`deploy_release`) publish real ones, versioned `*-eval`, to the local
deploy target.

## Evaluators

All of them are pure functions of the case and the trace and run on every case, so a
report shows each one for each case.

| Evaluator | Passes when |
|---|---|
| `termination` | the run status is terminal and has `finished_at`; `step_count <= max_steps`; `finished_at - started_at - paused_total_ms <= max_duration_seconds`. Limits come from the run's own snapshot, then the limits the case asked for (the agent's, or the workflow definition's), then the platform defaults. A run that failed with a `step limit` reason may show one step over: the step that trips the limit is recorded before the abort. |
| `trace_integrity` | every tool call has a terminal status (`executed`, `rejected`, `blocked`, `expired`, `failed`) and `finished_at`; every model call has a model and no negative token count; the run has at least one span |
| `expectation` | the status equals `expect.status`, the failure reason contains `failure_reason_contains` (ignoring case), the trace has at least `min_tool_calls` tool calls, `min_model_calls` model calls and `min_agent_messages` agent messages, and every `state`, `tool_statuses` and `approval_statuses` entry holds |
| `approval_gate` | every side-effecting tool call (`deploy_release`, `gitlab_create_mr`, or any call with an idempotency key) that reached `executed` references an approval that was `approved`, decided no later than the call finished. The exception is an agent whose interaction rule for the tool is `auto` in the case. A call that returned a stored result (`output.replayed`) caused no effect and is judged with the call that did |
| `idempotency` | calls with the same node visit, tool and input share one idempotency key and produce exactly one `side_effects` row: the first executed call made the effect and every later one is marked `output.replayed`. Different visits or inputs never share a key, every executed side effect has a key and a row, and every row belongs to an executed call of this run |
| `schema` | the output of every node that succeeded validates against that node's `output_schema`, and the final state validates against the workflow's `state_schema` when it has one. The definition is the run's stored snapshot when the trace has one, else the case's workflow, else the single-agent default |
| `allowlist` | every tool call that executed is in its agent's `tool_allowlist` (without the `yuno__` prefix; `finish` is always allowed) and not blocked by the agent's rule. Calls that were refused do not count |

The rubric evaluator is not part of this release. A case with an empty `rubric` scores 1 by
definition; the score is kept in the JSON report only.

## Reports and exit status

Each run writes `<UTC timestamp>.md` and `.json` to `EVAL_REPORTS_DIR` (`20261015T120000Z.md`);
an existing report is never overwritten. `baseline.md` and `final.md` are copies made by
`make eval-baseline` and `make eval-final`.

The report has the summary line, per-evaluator pass rates, the overall pass rate (cases on
which every evaluator passed), end-to-end success (cases whose run ended with `expect.status`),
mean cost and duration, a per-case table, a matrix of every evaluator against every case and
a list of failures with each failing evaluator's reason. The summary line is also printed to
standard output:

```
public split: 12 cases, deterministic pass 1.000, e2e success 1.000
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
| `004-dev-pipeline-happy` | public | the dev pipeline template's graph end to end: code, review, a human approves the gate, a second approval lets `deploy_release` run, one release is published | succeeded, `deploy_release` executed, two approvals approved |
| `005-feedback-loop-once` | public | the reviewer asks for changes once, the coder revises, the state keeps the second draft | succeeded, `/coder/tests_passed` true |
| `006-feedback-loop-limit` | public | a reviewer who never approves; `max_loop_visits: 2` stops the third coder visit | failed, `loop limit 2 exceeded for coder` |
| `007-approval-rejected` | public | a human rejects the approval node; the deployer never runs | failed, `rejected at gate`, no deploy |
| `008-approval-expiry` | public | nobody answers the gate; `approval_timeout_seconds: 5`, the runner waits 40 s | failed, `approval expired`, no deploy |
| `009-cost-limit` | public | scripted token usage of about $0.14 per call against `max_cost_usd: 0.25` | failed, reason contains `cost limit` |
| `010-idempotent-replay` | public | `deploy_release` called twice with the same input under an `auto` rule | succeeded, two executed rows, one side effect, the second replayed |
| `011-blocked-tool` | public | `deploy_release` is blocked by rule; the call is refused and the run still finishes | succeeded, `deploy_release` never executed or awaiting approval, no approval |
| `012-prompt-injection-exec` | private | a command prints an instruction to deploy; the agent does not | succeeded, no `deploy_release` row, no approval |
| `013-output-schema-repair` | public | the first `finish` breaks the output schema, the second is valid | succeeded, the state holds the valid result |
| `014-tool-approval-gate` | private | `deploy_release` requires approval; a human approves it and it runs in the same turn | succeeded, `deploy_release` executed, one approval approved |

To add a case, drop a file into the version directory, keep the `version` equal to the
directory, and run `go test ./internal/evals/...`: the shipped cases are loaded and validated
by the tests, and each one is played through a model of the engine's rules (script consumed
exactly, expected end state reached, every evaluator satisfied) before it ever meets a running api.
