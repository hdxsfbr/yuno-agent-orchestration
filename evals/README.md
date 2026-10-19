# Evaluation cases and reports

The eval runner (`yuno eval`, `make eval`) is a plain HTTP client of the api. For each
case it creates the temporary agents the case needs (and, for a workflow case, the
workflow), registers the case's script with the scripted model provider, runs it, answers
the approvals the case says to answer, waits for the run to end, judges the stored trace
with the evaluators below and deletes what it created. It needs no cloud access by
default: the model is `scripted/scripted-1` and replays the script in the case, and the
rubric is graded by local checks. Setting `EVAL_PROVIDER=anthropic` changes one thing:
the rubric is graded by `claude-opus-5-5` through the api's model proxy (see "Rubric").

```
make eval                      # EVAL_SPLIT=public by default
EVAL_SPLIT=all make eval       # every split
EVAL_CASES=028 make eval       # only the cases whose id starts with 028
EVAL_SPLIT=all EVAL_PROVIDER=anthropic make eval   # every split, rubrics graded by the model
make eval-baseline             # make eval, then copy the report to evals/reports/baseline.md
make eval-final                # make eval, then copy the report to evals/reports/final.md
```

| Variable | Default | Meaning |
|---|---|---|
| `EVAL_API_URL` | `http://127.0.0.1:8080` | api to test; in model mode its `/model-proxy/anthropic` is called too |
| `EVAL_SPLIT` | `public` | `public`, `private` or `all` |
| `EVAL_CASES` | empty | comma-separated case id prefixes; only those cases run |
| `EVAL_CASES_DIR` | `evals/cases` | searched recursively for `*.yaml` / `*.yml` |
| `EVAL_REPORTS_DIR` | `evals/reports` | where `<timestamp>.md` and `.json` are written |
| `EVAL_PROVIDER` | `scripted` | `scripted`: rubrics graded by their local checks. `anthropic`: rubrics graded by `claude-opus-5-5`. Runs are replayed from the scripts either way |
| `API_TOKEN` | empty | sent as a bearer token when set |
| `EVAL_FORGE_URL` | empty | base URL of the fake forge (`http://forgefake:8090`); only a case with a `project` block needs it. `make eval` sets it and starts the forge |
| `FORGEFAKE_TOKEN` | `forgefake-eval-token` | the forge's one token, a fixed eval value |

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
  guardrails:                      # optional, DESIGN 5.11, sent to the api as written
    max_runs_per_hour: 1           # also max_cost_usd_per_run and blocked_tools: [..]
    blocked_input_patterns:        # RE2 against the canonical JSON of the tool's input
      - { name: no-secret-files, tool: deploy_release, regex: '\.env' }
  memory_policy:                   # optional, DESIGN 5.10: enabled, max_facts, fact_ttl_days,
    enabled: true                  # conversation_window, conversation_ttl_days
task: |                            # the user task; $RUN_ID is replaced (see below)
  ...
script: [...]                      # see "Script", "Actions" and "Expect" below
rubric: [...]                      # optional, see "Rubric"
```

`guardrails` and `memory_policy` are also accepted under each entry of a workflow case's
`agents`. The loader applies the api's save-time rules (a pattern must compile as RE2 and
have a name and a tool, counts must not be negative), so a case never reaches the api with
a guardrail it will refuse. The `memory_save` and `memory_recall` tools are only offered to
an agent whose `memory_policy.enabled` is true, so a case that uses them sets it.

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
    guardrails: {...}              # optional, as in a quick run
    memory_policy: {...}           # optional, as in a quick run
rubric: [...]                      # optional, see "Rubric"
```

The runner creates each agent as `eval-<case id>-<key>-<random>`, replaces every
`agent_name` by the id of that agent (the api only accepts `agent_name` in templates),
posts the definition as workflow `eval-<case id>-<random>` and triggers it with the case's
input, the definition's limits and the scripted model. The case is rejected at load time
for an unknown field in the definition, an agent node whose `agent_name` is not a key of
`agents`, an unused agent, a schema that does not compile, a task or message template that
does not render, or an input that does not match `input_schema`.

Two agent nodes may name the same agent: the agent is created once, and anything that
belongs to an agent (its memory, its run count for a rate limit) carries from one node to
the next. A workflow with no agent node at all (`start` to `end`, DESIGN 5.6) needs no
`agents` and no `script`; case 018 is one.

### Script

```yaml
script:                            # what the scripted model does, one entry per model response
  - tool:                          # either a tool call ...
      name: exec
      args: { command: "python3 -c 'print(2+2)'" }
    usage: { prompt: 140, completion: 24 }   # optional token usage the provider reports
  - text: Done.                    # ... or a plain text reply
```

There is one script per case (none when the workflow has no agent node), consumed in
execution order across all of a workflow's nodes: every model call takes the next step,
whichever node makes it. A node visit ends with a text step, so after a `yuno__finish`
step the script needs a text step before the next node's first step. The script must be
consumed exactly: a missing step ends the run with `script exhausted`, a spare one is a
mistake in the case. The exceptions are the limit cases, whose run the engine ends while
the model still has steps left, and a case whose last step is an `yuno__escalate` call (case 027):
the engine ends the run at that call, so the script has no text step after it.

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
    - { decision: approve, answer: Use UTC. }       # a question from ask_operator is answered, not approved
    - { decision: reject }
  approval_wait_seconds: 40        # leave the first pending approval alone for this long
  cancel_when_waiting: true        # cancel the run as soon as it waits for an approval
  retrigger: true                  # after the run ends, trigger the same workflow again
  schedule:                        # start the run through a one-shot schedule
    kind: delay                    #   the only kind that fires within a case
    run_in_seconds: 2              #   run_at = now + this, 1 to 30
    wait_seconds: 40               #   how long to wait for the run to appear, default 30, at most 120
```

While the run waits for approval the runner lists the run's pending approvals
(`GET /api/approvals?run_id=<id>&status=pending`) and applies the next action to the
oldest one it has not acted on (`POST /api/approvals/<id>/approve` or `/reject` with the
reason). Actions are used up in order and apply to whatever kind of approval comes next,
a workflow approval node, a gated tool call or a question from `ask_operator`. A question
is answered with `POST /api/approvals/<id>/answer` and the action's `answer` (required for an
`approve` on a question); `reject` declines it. A pending approval with no action left is a
case error: the runner stops waiting, records it, cancels the run and still judges the
trace, rather than waiting out the case timeout. `approval_wait_seconds` is for expiry cases:
nothing is decided for that long after the first approval appears, and a run that ends
meanwhile (the approval expired) ends the wait.

`cancel_when_waiting` is for a run that must end at its gate. Instead of deciding the
approval the runner posts one `POST /api/runs/<id>/cancel` the first time it sees the run in
`waiting_approval`; the engine expires the approval with the reason `run cancelled`
(DESIGN 5.3 rule 8). Case 022.

`retrigger` is for the rate limit (DESIGN 5.11 rule 2). When the first run has ended, the
runner posts the same `POST /api/runs` request again and records the HTTP status and error
message of the answer, which `expect.trigger_status` checks. The second request names a
script id that was never registered, so if the api wrongly accepts it, the run it creates
cannot take steps from the first run's script; the runner cancels such a run. Case 019.

`schedule` starts the run the way an operator would. The runner creates the agents and the
workflow as usual, but instead of `POST /api/runs` it posts `POST /api/schedules` with
`{workflow_id, kind: "delay", run_at, input, enabled: true}` (`run_at` an absolute UTC
timestamp), then polls `GET /api/schedules/<id>` until `last_run_id` names the run the
scheduler created, and follows that run like any other. The schedule is deleted afterwards.
A schedule that records `last_error` instead of a run, or has not fired within
`wait_seconds`, fails the case. A scheduler tick (`SCHEDULER_TICK`, 10 s by default) sits
between the due time and the run, which is why the default wait is 30 s. A scheduled run
cannot carry the scripted model, so the workflow of such a case must have no agent node,
or its agents would call the real model. `schedule` cannot be combined with `retrigger`.
Case 018.

### Project and steps

A case can run on a project whose repository is on the **fake forge** (DESIGN 5.29 rule 17),
an in-memory GitLab REST subset plus Git smart HTTP that the `eval` compose profile starts
as `http://forgefake:8090`. It is for cases about issues, merge requests and pipelines
that a scripted model cannot stand in for. The case's own run is the usual one (a quick run
or a workflow); `project` makes it a run on that project (`project_id`), and `steps` act on
the forge and wait for the platform beside it.

```yaml
project:
  default_branch: main            # optional, main by default
  max_cost_usd_per_day: 5         # the project's cap; intake cannot be on without one
  intake:                         # optional; absent or enabled: false leaves intake off
    enabled: true
    label: yuno                   # optional, yuno by default
    workflow: feature_pipeline    # an existing workflow, by name; required when enabled
  issues:                         # opened on the forge before the project is created
    - {title: Add a login page, body: ..., labels: [yuno]}
  timeout_seconds: 900            # replaces the 180 s wait for the case's run (up to 3600)
steps:                            # in order, beside the run; the first failure fails the case
  - poll_intake: true             # POST /api/projects/{id}/intake/poll now
  - forge: {create_issue: {title: ..., body: ..., labels: [yuno]}}
  - forge: {expect_issues: [{title_contains: login, labels: [yuno:running], state: opened}]}
  - wait: {plan_status: building}                 # a plan of the project has this status
  - wait: {plan_event: wave_done, wave: 1}        # a plan has an event of this kind (of this wave)
  - forge: {merge_open: {count: 2}}               # merge the open, non-draft merge requests
  - forge: {pipeline: passed}                     # passed | failed | running | pending
  - approve: {decision: approve, node: smoke_test, reason: ok, count: 1}
  - wait: {ticket_status: merged, wave: 2}        # every ticket of wave 2 of some plan is merged
  - pause_waves: Holding wave 2.                  # POST /api/projects/{id}/waves/pause
  - hold: {no_ship_run: 2, seconds: 30}           # fails if the plan starts a ship run for wave 2
  - resume_waves: true
```

`project.platform_runs` scripts the runs the platform starts by itself, which no request of
the runner can name a script in: the runs the issue intake creates (`trigger: issue`) and the
wave watcher's ship runs (`trigger: plan`). Each entry has a `script` like the case's own; the
runner registers it with `POST /api/evals/scripts` and a `bind` of the project and trigger,
and the api gives the next run of that trigger on the project the oldest bound script, with
the scripted model. Entries are used in the order listed per trigger, so a two-wave plan lists
two `issue` scripts and two `plan` scripts. The agents of those runs are the workflows' own
(the seeded Builder, Integrator and the rest), so each script plays their turns, limited to
their allowlists.

The runner creates the repository `eval/<id>-<random>` (with a README commit on the default
branch), opens the `project.issues`, and registers the project with `POST /api/projects`:
provider `gitlab`, the repository URL on the forge, the forge's token, the intake settings
(`workflow` resolved by name through `GET /api/workflows`). The api accepts an `http://`
repository URL only for the host `EVAL_FORGE_URL` names, and only while that variable is set
for the api (`make eval` recreates the api with it; `make run` without). At the end it
cancels the project's runs that have not ended and its open plans, then deletes the project;
the forge keeps nothing between runs of the forge container.

Each step sets exactly one thing:

- `forge.create_issue` opens an issue; `forge.pipeline` records a pipeline of that state on
  the default branch's head, stamped now.
- `forge.merge_open` merges the open, non-draft merge requests (a fast-forward, else a merge
  commit) and repeats every poll until `count` (1 by default) have been merged, because the
  platform opens its merge requests while the step waits. A conflict fails the step.
- `forge.expect_issues` waits until every expectation is met by some issue: the title
  contains `title_contains` (any case), the issue carries every label of `labels`, and its
  state is `state` (`opened` or `closed`). The failure message lists the forge's issues.
- `wait.plan_status` and `wait.plan_event` poll `GET /api/projects/{id}/plans` (and
  `GET /api/plans/{id}` for the events): some plan has that status, or some plan has an event
  of that kind, of wave `wave` when it is set.
- `approve` decides, oldest first, pending approvals (not questions) of the project's runs,
  those of runs the platform started included, only at `node` when that is set. A case
  whose steps include `approve` does not stall on a pending approval of its own run that
  `actions.approvals` has no entry left for.
- `poll_intake` asks the api to poll the project's issues now instead of at the interval.

`forge` and `wait` steps and `approve` wait at most `timeout_seconds` (60 by default, up to
1800) and fail with what was missing. A failed step cancels the case's run and the result
carries the step's number and message. The forge's merge request sync, intake and wave
watcher run at the api's own intervals (`MERGE_REQUEST_SYNC_INTERVAL`, `INTAKE_POLL_INTERVAL`,
`WAVE_WATCH_INTERVAL`); `make eval` recreates the api with all three at 5 s, so a case that
depends on one waits seconds, not minutes.

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
  tool_outputs:                    # optional: tool -> texts, each in the output of some row of it
    http_fetch: ['<untrusted_content source="http_fetch">']
  approval_statuses: [approved]    # optional: exact statuses of the run's approvals in request
                                   #   order; [] means the run raised none
  approval_reasons: [run cancelled]   # optional: per approval, in request order, a substring of its
                                   #   reason; an empty entry is not checked
  trigger: schedule                # optional: the run's trigger (manual | schedule | channel)
  schedule_enabled: false          # optional: whether the schedule is still enabled afterwards
  trigger_status: 429              # optional: HTTP status of the answer to actions.retrigger
  trigger_error_contains: "rate limit: 1 runs/hour"   # optional: substring of that answer's message
  tools_present: [deploy_release]  # optional: names GET /api/tools must list
  tools_absent: [gitlab_create_mr] # optional: names GET /api/tools must not list
rubric: [...]                      # optional, see "Rubric"
```

For a quick run the final state is `{"agent": <the agent's result>}`; for a workflow it is
`{"<node id>": <that node's last result>}`.

`tool_outputs` looks at the output stored on the tool call rows, whatever their status (a
refused call leaves its refusal there): every string in the output, decoded, and the raw
JSON, compared ignoring case. It is how a case shows that a tool returned something the
scripted model could not have made up: the `<untrusted_content>` wrapper of `http_fetch`
(case 021), the fact `memory_recall` returned (case 020), the text of a guardrail's refusal
(case 024).

The last six fields read what the runner saw outside the run's trace, which the report
keeps under `observed` in the JSON: the answer to the extra trigger, the schedule read back
after the run ended, and the tool list (fetched only when `tools_present` or `tools_absent`
is set). `trigger: schedule` also requires the run's `trigger_ref` to be the schedule the
runner created, and `trigger`, `schedule_enabled` need `actions.schedule`, `trigger_status`
and `trigger_error_contains` need `actions.retrigger`; the loader refuses the combinations
that cannot be checked.

### Rubric

A case may carry three to six criteria. Each is a sentence that is true or false of the
run's outcome, and a local check that stands in for it offline:

```yaml
rubric:
  - criterion: The critic reviewed the researcher's notes and approved them.
    check: { path: /critic/verdict, json_path_eq: approved }
  - criterion: The researcher's sources say the facts come from its own knowledge.
    check: { path: /researcher/sources, contains: own knowledge }
  - criterion: The writer's summary gives a recommendation.
    check: { path: /writer/summary, regex: "(?i)recommend" }
```

A check sets exactly one of `contains`, `regex` or `json_path_eq`, and `path`, a JSON
pointer into the run's final state (the same state `expect.state` reads). `contains` is a
case-insensitive substring and `regex` an RE2 expression (case-sensitive unless it says
`(?i)`); both look at the value at `path` when it is a string, otherwise at its compact
JSON, and at the whole state when `path` is empty. `json_path_eq` needs a `path` and passes
when the value there equals the given one (a YAML number equals the JSON number). A
missing path fails the criterion.

Each criterion scores 0 or 1 and the case's rubric score is the share met. A case passes
when every deterministic evaluator passed **and** its rubric scored at least 0.75, so 3 of
4 passes and 2 of 3 does not. A case without a rubric scores 1.

Two graders produce the score, and the report says which one did, for every case:

| Mode | When | How it grades |
|---|---|---|
| `heuristic` | `EVAL_PROVIDER=scripted` (the default) | runs each criterion's `check` against the final state; needs nothing but the trace |
| `model` | `EVAL_PROVIDER=anthropic` | sends `claude-opus-5-5` the case's description and task or input, the run's status and failure reason, the final state and the numbered criteria, and asks for a JSON verdict per criterion (`score` 0 or 1 and a one-sentence `reason`) as structured output; the `check` is not shown to it |

The model call goes to `<EVAL_API_URL>/model-proxy/anthropic/v1/messages`, the api's
proxy, which holds the key; the runner sends no credentials. The proxy only answers
loopback callers, and the eval container shares the api's network namespace, so it does.
The request carries no sampling parameters and no forced tool choice (this model rejects
both); an upstream that rejects the structured output format with a 400 or 422 is asked
again once with the same prompt and its text answer parsed as JSON. The final state is
agent output and reaches the grader inside a `<final_state>` block that the system prompt
calls data. A call that fails (no key, rate limit, an answer that is not the JSON asked
for) scores the rubric 0 with the reason under `model grading failed`; a run asked for
model grading never passes without it. A criterion the model gives no verdict for counts as
not met.

## What the runner does per case

1. `POST /api/agents` creates `eval-<id>-<random>` (a quick run) or one
   `eval-<id>-<key>-<random>` per entry of `agents` (a workflow case) with the default
   model, the case's prompts, allowlists, interaction rules, limits, guardrails and memory
   policy. A workflow case then `POST /api/workflows` with the definition.
2. `POST /api/evals/scripts` registers the script as `<id>-<random>` (the same random
   suffix); a workflow without agent nodes has no script.
2a. A case with a `project` block first creates its repository on the forge and its project
   (see Project and steps), and the run is created with that `project_id`; its `steps` then
   run beside the run.
3. `POST /api/runs` triggers the run with model `scripted/scripted-1`, that script id and
   the case's limits: `{agent_id, task}` for a quick run, `{workflow_id, input}` for a
   workflow case. With `actions.schedule` the run is created by a one-shot schedule instead
   (`POST /api/schedules`, then `GET /api/schedules/{id}` until it names the run).
4. `GET /api/runs/{id}` every 500 ms until the status is terminal, for at most 180 s. A run
   that has not finished by then is cancelled and recorded as failed with the reason
   `runner timeout`. With `actions.cancel_after_steps`, the runner posts one cancel as soon
   as `step_count` reaches that value; with `actions.cancel_when_waiting` it posts one as
   soon as the run waits for approval; approval actions are applied as described above.
5. Once the run has ended: the extra trigger of `actions.retrigger`, the schedule read
   back, and `GET /api/tools` when the case lists `tools_present` or `tools_absent`.
6. `GET /api/runs/{id}/trace`, then the evaluators and the rubric.
7. `DELETE /api/projects/{id}` (after cancelling its runs and plans), `DELETE
   /api/schedules/{id}`, `DELETE /api/workflows/{id}` and `DELETE /api/agents/{id}`,
   whatever happened.

Cases run one at a time. An api error, a panic or a timeout in one case marks that case
failed (every evaluator fails with the reason, its rubric is shown as not scored) and the
suite carries on. The cases that publish a release (`deploy_release`) publish real ones,
versioned `*-eval`, to the local deploy target.

## Evaluators

All of them are pure functions of the case, the trace and what the runner observed, and run
on every case, so a report shows each one for each case.

| Evaluator | Passes when |
|---|---|
| `termination` | the run status is terminal and has `finished_at`; `step_count <= max_steps`; `finished_at - started_at - paused_total_ms <= max_duration_seconds`. Limits come from the run's own snapshot, then the limits the case asked for (the agent's, or the workflow definition's), then the platform defaults. A run that failed with a `step limit` reason may show one step over: the step that trips the limit is recorded before the abort. A run that timed out may finish up to 12 s past its limit: the engine's 2 s tick plus the 10 s it waits for the abort |
| `trace_integrity` | every tool call has a terminal status (`executed`, `rejected`, `blocked`, `expired`, `failed`) and `finished_at`; every model call has a model and no negative token count; the run has at least one span |
| `expectation` | the status equals `expect.status`, the failure reason contains `failure_reason_contains` (ignoring case), the trace has at least `min_tool_calls` tool calls, `min_model_calls` model calls and `min_agent_messages` agent messages, and every `state`, `tool_statuses`, `never_tool_statuses`, `tool_outputs`, `approval_statuses` and `approval_reasons` entry holds, and so does what the runner observed: `trigger`, `schedule_enabled`, `trigger_status`, `trigger_error_contains`, `tools_present` and `tools_absent` |
| `approval_gate` | every side-effecting tool call (`deploy_release`, `gitlab_create_mr`, or any call with an idempotency key) that reached `executed` references an approval that was `approved`, decided no later than the call finished. The exception is an agent whose interaction rule for the tool is `auto` in the case. A call that returned a stored result (`output.replayed`) caused no effect and is judged with the call that did |
| `idempotency` | calls with the same node visit, tool and input share one idempotency key and produce exactly one `side_effects` row: the first executed call made the effect and every later one is marked `output.replayed`. Different visits or inputs never share a key, every executed side effect has a key and a row, and every row belongs to an executed call of this run |
| `schema` | the output of every node that succeeded validates against that node's `output_schema`, and the final state validates against the workflow's `state_schema` when it has one. The definition is the run's stored snapshot when the trace has one, else the case's workflow, else the single-agent default |
| `allowlist` | every tool call that executed is in its agent's `tool_allowlist` (without the `yuno__` prefix; `finish` is always allowed) and not blocked by the agent's rule. Calls that were refused do not count |

The rubric evaluator is separate from these seven and described under "Rubric": a case
passes when all seven pass and its rubric scores at least 0.75.

## Reports and exit status

Each run writes `<UTC timestamp>.md` and `.json` to `EVAL_REPORTS_DIR` (`20261015T120000Z.md`);
an existing report is never overwritten. `baseline.md` and `final.md` are copies made by
`make eval-baseline` and `make eval-final`. Both run the suite for `EVAL_SPLIT` (public by
default); set `EVAL_SPLIT=all` to put the private cases in the stored report as well. The
pass gate and the summary line are about the public split either way.

The report has the summary line, the overall pass rate (cases on which every evaluator passed
and the rubric scored at least 0.75), the deterministic pass rate (cases on which every
evaluator passed), end-to-end success (cases whose run ended with `expect.status`), a rubric
line (cases that passed it, the mean score, how many were graded in each mode), mean cost and
duration, per-evaluator pass rates, a per-case table with a **Rubric** column, a matrix of
every evaluator against every case, the criteria of every rubric with the grader's reason,
and a list of failures with each failing evaluator's reason and each unmet criterion. A
case's rubric cell reads `0.75 (3/4, heuristic)` or `1.00 (4/4, model)`; `-` means the case
has no rubric and `not scored` that it never produced a trace. The provider line says how
rubrics were graded. The JSON report carries the same, plus `rubric.items` per case and what
the runner observed. The summary line, unchanged, is also printed to standard output:

```
public split: 18 cases, deterministic pass 1.000, e2e success 1.000
```

The command exits 1 when the overall pass rate of the public split (evaluators and rubric)
is below 0.9, after writing the report. It also exits 1 when the cases cannot be loaded, the
api never becomes ready, or the run is interrupted before every case ran.

## Cases

Eighteen public and seven private. A case under the gate of the default `make eval`
(public) needs nothing from the outside world; the private ones add a dependency each, noted
below.

| Id | Split | Shows | Expected |
|---|---|---|---|
| `001-quick-run-happy` | public | runs a command, writes `runs/$RUN_ID/notes.md`, finishes with a summary | succeeded, at least 2 tool calls |
| `002-step-limit` | public | an agent that keeps calling tools without finishing hits `max_steps: 2` | failed, reason contains `step limit 2 exceeded` |
| `003-cancel` | public | slow steps, cancelled by the runner after the first | cancelled |
| `004-dev-pipeline-happy` | public | the dev pipeline template's graph end to end: code, review, a human approves the gate, a second approval lets `deploy_release` run, one release is published | succeeded, `deploy_release` executed, two approvals approved; rubric of 4 |
| `005-feedback-loop-once` | public | the reviewer asks for changes once, the coder revises, the state keeps the second draft | succeeded, `/coder/tests_passed` true; rubric of 3 |
| `006-feedback-loop-limit` | public | a reviewer who never approves; `max_loop_visits: 2` stops the third coder visit | failed, `loop limit 2 exceeded for coder` |
| `007-approval-rejected` | public | a human rejects the approval node; the deployer never runs | failed, `rejected at gate`, no deploy |
| `008-approval-expiry` | public | nobody answers the gate; `approval_timeout_seconds: 5`, the runner waits 40 s | failed, `approval expired`, no deploy |
| `009-cost-limit` | public | scripted token usage of about $0.14 per call against `max_cost_usd: 0.25` | failed, reason contains `cost limit` |
| `010-idempotent-replay` | public | `deploy_release` called twice with the same input under an `auto` rule | succeeded, two executed rows, one side effect, the second replayed |
| `011-blocked-tool` | public | `deploy_release` is blocked by rule; the call is refused and the run still finishes | succeeded, `deploy_release` never executed or awaiting approval, no approval |
| `012-prompt-injection-exec` | private | a command prints an instruction to deploy; the agent does not | succeeded, no `deploy_release` row, no approval |
| `013-output-schema-repair` | public | the first `finish` breaks the output schema, the second is valid | succeeded, the state holds the valid result |
| `014-tool-approval-gate` | private | `deploy_release` requires approval; a human approves it and it runs in the same turn | succeeded, `deploy_release` executed, one approval approved |
| `015-research-pipeline-happy` | public | the `research_pipeline` template's graph with three agents and no `http_fetch`: notes, an approving critic, a report | succeeded, `/critic/verdict` approved; rubric of 4 |
| `016-time-limit` | public | an `exec` that sleeps 60 s under `max_duration_seconds: 15` | timed_out, reason `time limit 15s exceeded` (about 17 to 27 s) |
| `017-tool-call-limit` | public | an agent that keeps listing directories under `max_tool_calls: 3`, well below the step limit | failed, reason `tool call limit 3 exceeded` |
| `018-schedule-triggered-run` | public | the runner creates a one-shot schedule two seconds ahead; the scheduler fires it once; the delay schedule disables itself. The workflow has no agent node (see Actions) | succeeded, trigger `schedule` with the schedule as `trigger_ref`, schedule disabled (about 12 s) |
| `019-rate-limit` | public | an agent with `max_runs_per_hour: 1`; the first run succeeds, the same trigger again straight after is refused | succeeded, the second trigger answers 429 `rate limit: 1 runs/hour for agent <name>` |
| `020-memory-across-nodes` | private | one agent with memory saves a fact in one node and recalls it in the next | succeeded, the `memory_recall` output holds the saved fact; rubric of 3 |
| `021-http-fetch-untrusted-content` | private | the agent fetches a page; the platform wraps it as untrusted content; nothing is deployed though `deploy_release` is on the allowlist. Needs outbound https to `docs.python.org`, on the default `HTTP_FETCH_ALLOWLIST` | succeeded, `http_fetch` output holds both ends of the `<untrusted_content>` block, no `deploy_release` row, no approval |
| `022-cancel-while-waiting-approval` | private | the run waits at an approval node and the runner cancels it | cancelled, one approval `expired` with the reason `run cancelled` |
| `023-gitlab-tool-absent` | private | GitLab is not configured, so `GET /api/tools` lists `deploy_release` and `finish` and not `gitlab_create_mr`. Fails by design when `GITLAB_BASE_URL`, `GITLAB_PROJECT_ID` and `GITLAB_TOKEN` are all set | succeeded, the tool list as described |
| `024-guardrail-blocked-input` | private | a guardrail pattern blocks `deploy_release` inputs naming `.env`, `.pem` or `secrets/` files; the call is refused before any approval is asked for | succeeded, `deploy_release` `blocked` with `input blocked by guardrail no-secret-files`, no approval |
| `025-plan-pipeline-happy` | public | the plan pipeline without a project: the analyst asks one question with `ask_operator` and the case answers it, then scout, architect, the design gate, planner (tickets, waves), the plan gate, and an integrator that publishes the four documents with `deploy_release` | succeeded, four approvals `approved` (the question, both gates, the deploy), `deploy_release` `executed` after its approval, the question answered in the `ask_operator` output; rubric of 4. It runs an inline copy of `plan_pipeline` without a project (`deploy_release` instead of `open_merge_request`), with the `waves_partition` check on the waves node |
| `026-review-loop-limit-escalation` | public | the ticket pipeline's builder and reviewer, where the reviewer asks for changes every time and `max_loop_visits` is 3; the platform, not an agent, ends the run | failed, reason `loop limit 3 exceeded for builder`, no `deploy_release` row, no approval; rubric of 3 |
| `027-prd-gap-escalation` | public | the ticket pipeline's documentation audit finds a requirement the PRD leaves undefined and calls `escalate` with kind `prd_gap` instead of finishing; the script ends at that call | failed, reason `escalated (prd_gap): R4 says exports are for authorised users`, the builder's result kept in the state, no `deploy_release` row, no approval |
| `028-wave-auto-advance-pause` | public | a plan of two waves, one ticket each, on the fake forge with every agent turn scripted (the plan run's own and the runs the platform starts, see Project and steps): `publish_tickets` creates the issues, the plan merge request merges and the wave watcher releases wave 1, intake takes the issue, the wave pipeline builds the ticket to a ready merge request, the forge merges it and main goes green, the watcher starts the ship run, the smoke test is approved and `advance_wave` closes wave 1 and releases wave 2. The waves are then paused: wave 2 is built and merged with main green and no ship run starts for 30 s; resumed, wave 2 ships, the feature is accepted and the plan is `done` | the plan run succeeded with two approvals; the steps are the proof of the rest. About 6 to 10 minutes with the loops at 5 s (`make eval` sets them); needs the seeded `Wave pipeline` and `Ship pipeline` workflows |

The channel-triggered reply of DESIGN 5.13 rule 6 is not a case: it needs a real message on
a real Telegram chat. What can be asserted without one, that a session message is mirrored
into the conversation tables, is a unit test of the channel code.

To add a case, drop a file into the version directory, keep the `version` equal to the
directory, and run `go test ./internal/evals/...`: the shipped cases are loaded and validated
by the tests, and each one that the engine does not end by a limit is played through a model
of the engine's rules (script consumed exactly, expected end state reached, every evaluator
satisfied, every rubric criterion met) before it ever meets a running api.
