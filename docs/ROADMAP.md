# Roadmap: from a working pipeline to a factory

Written 2026-10-17 from the state of main at a250615 (wave 7, projects as records,
live on the public instance). It plans the nine features Andre picked from the ranked
list, in the order they should be built, and lists the product decisions each one needs
before its design section is written.

## How planning works in this repository

There is no speckit, openspec or similar tool here, and none is proposed. The project
has its own spec-driven loop, and the code, the reviews and the skills already refer to
it:

| Artifact | Role | Where |
|---|---|---|
| Design section | The spec: numbered rules, edge cases, failure handling, outputs, tests, out of scope. Code comments and test names cite rules as `5.17 rule 4`. | `docs/DESIGN.md`, one `5.x` section per feature |
| Decision | One settled product question, dated, with the alternative that lost. | `DECISIONS.md` (`D-nnn`) |
| Brief | What one agent builds in one wave: the rules it owns, the files it may touch, the tests that prove it. | `docs/briefs/wave<n>/*.md` |
| Review | The independent review of the wave and what was filtered out of it. | `docs/reviews/wave<n>-review.md` |
| Results | Suite numbers and the live verification of the wave. | `docs/test-results.md` |

The procedure per wave (the `design-first-brief` and `wave-loop` skills): write the
design section with every line filled; stop on the lines that are product decisions and
get them answered; record each answer as a decision; write one brief per agent with
disjoint files; build in parallel; run the whole suite; one independent review, filtered
visibly; one fix pass; one commit; merge to main when green (main auto-deploys).

What this document adds is the layer above a section: the order of the features, the
dependencies between them, and the decisions to settle before each design is written.
It is not a spec. Each feature gets its section number below so that briefs and code
can cite it before the section exists.

## The order

The goal is a factory: work comes in without a person typing it, a merge request is the
unit of work and gets iterated until it is merged, and the same machinery delivers
things that are not code. The order below builds the cheap project-level pieces first,
then makes the merge request a record the platform can follow and update, then opens
the intake, then the conversation and deliverable pieces, and leaves the engine
rewrite for last because everything else is useful without it.

| Wave | Sections | Features | Why in this order | Size |
|---|---|---|---|---|
| 8 | 5.18, 5.19, 5.20, 5.21 | Project instructions and memory; project budgets; merge request record and merge; retry from the failed step | All four are additive to what exists, none depends on another, and 5.20 is the prerequisite of wave 9. | 1 day, three agents |
| 9 | 5.22, 5.23 | Iterate on an open merge request; issue intake | The merge request becomes the unit of work: an issue starts one, a follow-up run updates it. Needs the record of 5.20. | 1 to 2 days, two agents plus UI |
| 10 | 5.24, 5.25 | Questions to the operator mid-run; run artifacts | What makes the platform useful for jobs that are not code: a run can ask, and it can deliver files. Independent of wave 9. | 1 to 2 days, two agents plus UI |
| 11 | 5.26 | Parallel steps | The one engine change; largest, riskiest, built alone with the stronger model after the rest is stable. | 2 days, one agent |

Each wave ends with the demo and the README re-derived where they changed, as the wave
loop requires. The demo recording with projects (deferred to 2026-10-18) is not part of
any wave here.

Dependencies, so that a reorder stays safe: 5.22 needs 5.20 (the record it updates).
5.23 needs 5.20 (it posts the merge request link on the issue) and uses 5.19 (intake
stops when the budget is reached). 5.21 and 5.24 both pause and resume a node visit;
5.24 reuses what 5.21 builds. Nothing depends on 5.26.

## 5.18 Project instructions and project memory (feature 7)

Purpose: a run on a project starts knowing how that project works, and what earlier runs
on it learned, instead of rediscovering it every time.

Today: the task protocol block tells the agent where the checkout is and how to install
and test for the detected toolchain (5.16 rule 9). Memory is per agent (5.10): a fact
the Coder saves about the playground is recalled on an unrelated project too, and the
Reviewer never sees it.

Scope: a free-text `instructions` field on the project (markdown, at most 8 KiB, edited
on the project form), rendered into the task protocol block of every visit of a run on
that project, after the repository lines, under a heading that says it comes from the
operator. A new memory scope `project` (`memory_facts.scope = project`, `scope_id` =
project id): `memory_save` and `memory_recall` accept `scope: "project"` in a run on a
project (refused elsewhere with `this run has no project`), any agent of the run may
read and write, the prompt's memory block adds the 10 newest project facts, and the
project page lists and deletes them. The shipped Coder and Reviewer prompts say to save
a fact when they learn how the project is built or tested. Cap and TTL per project:
100 facts, 90 days.

Out of scope: reading `AGENTS.md` or `CLAUDE.md` from the checkout into the prompt (the
agent can read it itself; repository text is untrusted), per-agent instructions per
project, instructions on the run request.

Decisions (settled 2026-10-17 by Andre with the recommended answer on every line; recorded as D-040 to D-048):

1. Should the checkout's own `AGENTS.md` (first 8 KiB) be injected too? Recommended: no,
   for the trust reason above; the Coder prompt tells the agent to read it.
2. May any agent of a project run write project facts, or only the ones the operator
   allows? Recommended: any agent whose memory policy is enabled; the allowlist already
   gates the memory tools.

Size: S to M. Files: `internal/store/projects.go` and migration, `internal/project`,
the protocol block builder in `internal/engine`, `internal/tools` memory tools,
`web/src/components/project`, `web/src/pages/ProjectsPage.tsx`, seed prompts.

## 5.19 Project budgets and alerts (feature 8)

Purpose: a project cannot spend more than the operator allowed in a day or a month, and
the operator hears about it before it stops.

Today: cost caps exist per run (5.2) and per agent per run and per hour (5.11). Nothing
bounds a project, so an intake that opens many issues (5.23) or a schedule on a project
could spend without limit.

Scope: optional `max_cost_usd_per_day` and `max_cost_usd_per_month` on the project
(whole USD with cents, stored as micro-USD like everything else; the UI form has both).
Spend is the sum of `runs.cost_usd_micros` of the project's runs whose `created_at` falls in the
window; windows are UTC calendar day and UTC calendar month. A run request, a delegation,
a schedule firing or an issue intake on a project whose spend already reached a cap is
refused at queue time (429 `project_budget_reached` like a rate limit, the tool error `project <name> has
reached its <daily|monthly> budget`, a schedule `last_error`); running runs finish under
their own caps, so a project can overshoot by at most the sum of the running runs' caps.
The project page and list show today's and this month's spend next to the caps. One
alert per window when spend crosses 80 % and one when it reaches the cap, sent as a
`[platform]` message like a report-back.

Out of scope: a platform-wide budget, aborting running runs, budgets per agent per
project, currency other than USD.

Decisions (settled 2026-10-17 by Andre with the recommended answer on every line; recorded as D-040 to D-048):

1. Where does the alert go? Recommended: into the most recent conversation of the agent
   bound to Telegram (the Chief of Staff), the same route report-backs use; without a
   binding the alert is a run event on Status only. Alternative: a dedicated
   `ALERT_CHAT_ID`, which needs its own send path.
2. Refuse at queue time only (recommended, deterministic, at most one run's overshoot)
   or also abort running runs when the cap is crossed?

Size: S to M. Files: `internal/store/projects.go` and migration, `internal/engine`
trigger path, `internal/sched`, `internal/tools/chief.go`, project form and page.

## 5.20 Merge request record, status sync and merge (feature 6)

Purpose: the platform knows the state of every merge request it opened, shows it on the
run, reports it on Telegram, and can merge it on an approved request.

Today: the merge request is an output of a tool call in the trace. The run header and
`run_status` find it by reading the newest executed `open_merge_request` call. Nothing
knows whether CI passed, a reviewer commented, or it was merged or closed.

Scope: a `merge_requests` table (id, project id, run id, provider, number, url, source
branch, target branch, title, `state` open | merged | closed, `ci_status` pending |
passed | failed | none, `head_sha`, `unresolved_threads`, `synced_at`, timestamps),
written when `open_merge_request` succeeds (and backfilled once from existing tool
calls by the migration's follow-up job). A sync pass every 2 minutes refreshes every
open record of the last 14 days through the provider's API (GitLab: the merge request
and its latest pipeline; GitHub: the pull request and the check runs of its head), and
a run page view refreshes that run's record on demand when it is older than 30 s. The
run page gets a Merge request card (state, CI, unresolved threads, link, Merge button);
the project page lists the project's merge requests; `run_status` returns `merge_request
{url, state, ci_status, unresolved_threads}`; the report-back of 5.15 rule 9 is unchanged
and a later state change is not reported (the Chief answers when asked). Merge is
`POST /api/merge-requests/{id}/merge`, operator only, refused unless `state = open` and
`ci_status` is `passed` or `none`, executed through the provider (GitLab `merge`, GitHub
`merge` with the merge method the repository allows), recorded as a side effect with an
idempotency key.

Out of scope: merging from chat (a chat tool with `require_approval` is refused by 5.15
rule 7; it comes with 5.24's answer path if wanted later), rebasing, resolving
conflicts, review comments as a tool (5.22), webhooks (polling only, no inbound
surface).

Decisions (settled 2026-10-17 by Andre with the recommended answer on every line; recorded as D-040 to D-048):

1. Merge from the UI only (recommended for this wave) or also a Deployer tool
   `merge_merge_request`, approval-gated, usable inside a run after CI passed?
2. Should a merged or closed merge request be reported into the conversation that
   started the run, once? Recommended: yes, one line per final state, keyed like the
   late-grant follow-up of 5.17, since the factory's loop closes there.

Size: M. Files: new store file and migration, `internal/tools/mergerequest.go` and
`providers.go`, a new sync loop beside `internal/sched`, `internal/httpapi` merge
requests handler, `internal/tools/chief.go` (`run_status`), run page card, project
page list.

## 5.21 Retry from the failed step (feature 9)

Purpose: a run that failed at its third node is re-queued from that node with the same
state and checkout, instead of being started over at full cost.

Today: a failed run is final; the operator starts a new run and pays for every node
again. The engine already re-queues an attempt of a run already cloned after a crash
(5.16 rule 3), which is the mechanism this feature exposes.

Scope: `POST /api/runs/{id}/retry` on a run in `failed`, `timed_out` or `cancelled`,
with an optional `limits` override. The same run row continues: `runs.attempt`
increments, `current_node` is the node whose visit failed, `runs.state` and the
checkout (through the copy-forward of the failed visit's run directory) are kept, the
failed visit's rows stay in the trace marked `attempt 1`, and a new visit of that node
runs. Counters continue (a run keeps its true cost and step count) and the caps apply to
the whole run, so a run that failed on a cap is refused with `raise the limits to retry`
unless `limits` raises them. A retried visit is a new visit with new idempotency keys; the merge request is not
repeated because 5.20 gives a run one request that later calls update.
The run page gets a Retry button; `run_status` shows the attempt; the Chief gets no new
tool (it delegates a new run; a retry is an operator action).

Out of scope: retrying from an earlier node than the failed one, editing state before
the retry, retrying a run whose project or agent was deleted since.

Decisions (settled 2026-10-17 by Andre with the recommended answer on every line; recorded as D-040 to D-048):

1. Same run with an attempt counter (recommended: keys, branch and trace stay one
   story) or a new run linked by `retry_of` (cleaner cost accounting, but it would open a
   second merge request on a second branch)?
2. Is a cancelled run retryable? Recommended: yes; cancelling is the operator's, so is
   changing their mind.

Size: M. Files: `internal/engine` run and walk, `internal/store/runs.go` and migration,
`internal/httpapi/runs.go`, run page.

## 5.22 Iterate on an open merge request (feature 1)

Purpose: "fix the review comments on !2" updates !2, from its branch, instead of opening
!3 from main.

Today: every run clones the default branch and `open_merge_request` creates a branch
named from the run's idempotency key. A follow-up on an existing merge request is not
expressible.

Scope: a run request, a delegation and a schedule may name `merge_request_id` (a record
of 5.20, which must be open and belong to the run's project; 422 otherwise, and the
Chief accepts a number or URL and resolves it against the project). The clone checks out
that record's source branch; `open_merge_request` on such a run commits the working
tree as a new commit on top of that branch (not a flattened replacement), pushes, and
returns the existing record with `updated: true`; a run without `merge_request_id`
behaves as today. A read-only tool `read_merge_request` (run and chat capable; input
the number, scoped to the run's or the named project) returns the description, the
unresolved threads with their file and line, and the CI status, every text wrapped as
untrusted content (5.7). The Dev pipeline's Coder is told, when the run has a merge
request, to read it first and address its threads; the Deployer's wording covers
"update" as well as "open". The run header links the merge request it works on; the
Chief's prompt gains the pattern "on !<n>" and `run_status` says which merge request
the run updates.

Out of scope: resolving threads on the provider after a fix (the reviewer does it),
rebasing onto a moved default branch, replying to individual threads, picking a merge
request that another run is currently updating (refused: `merge request !<n> is being
updated by run <id>`).

Decisions (settled 2026-10-17 by Andre with the recommended answer on every line; recorded as D-040 to D-048):

1. Thread texts delivered by a tool the agent calls (recommended: it reads what it
   needs, and the task stays short) or injected into the task up to a cap?
2. When the merge request was merged or closed between the request and the clone:
   refuse at queue time only (recommended) or also fail the run at clone time?

Size: L. Files: `internal/engine` trigger and clone, `internal/tools/mergerequest.go`,
a new `internal/tools/readmergerequest.go`, `internal/tools/chief.go`, templates and
seed prompts, Run dialog and run header, `api/openapi.yaml`.

## 5.23 Issue intake (feature 2)

Purpose: a labelled issue on the project's repository becomes a run without anyone
typing a task, and the issue shows where the work went.

Today: work enters through the UI, a schedule or a Telegram message to the Chief. The
factory has no inbox of its own.

Scope: per-project intake settings `{enabled, label (default "yuno"), workflow_id}`
(the workflow must opt in to a project). A poll every 2 minutes per enabled project
lists open issues carrying the label (GitLab `issues?labels=&state=opened`, GitHub
`issues?labels=&state=open`, excluding pull requests), and for each one not yet taken
creates a run with `trigger = issue`, `trigger_ref = "<project id>#<issue number>"`
(unique, so a poll after a crash finds the run), input `task` = the issue title and body
as untrusted content with the issue URL, then swaps the label to `yuno:running` and
comments `Yuno started run <url>`. When the run ends the label becomes `yuno:done` with
a comment carrying the merge request link, or `yuno:failed` with the reason; removing
`yuno:failed` and adding `yuno` again is an explicit retry that creates a new run. A
project whose budget is reached (5.19) is skipped with one log line per poll. The
project page shows the intake settings and the last poll; the runs list filters by
trigger `issue` and links the issue.

Out of scope: webhooks (a later wave can add them behind the same taker), issues
assigned to a user instead of labelled, GitLab epics, GitHub projects, closing the issue
(the merge does, through the provider's keywords in the merge request description:
`Closes #<n>`, which `open_merge_request` adds when the run has an issue).

Decisions (settled 2026-10-17 by Andre with the recommended answer on every line; recorded as D-040 to D-048):

1. Polling (recommended: no inbound endpoint, no webhook secret, both providers the
   same) or webhooks from the start?
2. Label swap as the state machine on the issue (recommended: visible on the provider,
   makes retry explicit) or comments only?
3. One run per issue, or re-run on every edit of the issue? Recommended: one, retry by
   re-labelling.

Size: M. Files: a new `internal/intake` package beside `internal/sched`, provider client
methods in `internal/tools/providers.go`, `internal/store/projects.go` and migration,
`internal/engine` trigger (new trigger kind), project form, runs list.

## 5.24 Questions to the operator mid-run (feature 3)

Purpose: an agent that is missing one fact asks for it and waits, instead of guessing or
failing, and the operator answers from the UI or Telegram.

Today: approvals are yes or no. The Chief can ask on Telegram before delegating, but
once a run is going the agent has nobody to ask.

Scope: a platform tool `ask_operator {run_token, question, options?}` (at most 3 per
visit, the fourth is the tool error `ask fewer questions`; question at most 1 KiB). It
creates an approval of kind `question` with the question text and options, sets the
run to `waiting_approval`, sends the `[platform] Run <id> asks: <question>. Answer at
<url>` line into the run's conversation when it has one, and blocks the MCP call up to
`MCP_TOOL_WAIT` like a gated tool (5.3 rule 5). An answer within the wait returns
`{answer}` to the model in the same turn. After the wait the tool returns `{status:
"pending", question_id}` and the protocol block tells the agent to end its turn by
calling `finish` with `{"waiting_for": "<question id>"}`, which parks the visit; when the
answer arrives the engine runs the node again (5.21's attempt path) with `Answer to your
question "<q>": <answer>` appended to the task. The Approvals inbox renders a text box
(and the options as buttons) for kind `question`; the Chief gets a chat tool
`answer_question {question_id, answer}` so the operator can answer in Telegram by
replying to the Chief. An unanswered question expires with `approval_timeout_seconds`
and the visit fails with `question unanswered`.

Out of scope: questions from the Chief itself (it is in a conversation and simply asks),
questions to someone other than the operator, attachments in answers, more than one
pending question per visit.

Decisions (settled 2026-10-17 by Andre with the recommended answer on every line; recorded as D-040 to D-048):

1. Block then park and resume (recommended for a factory, reuses 5.21) or block only,
   with the question lost after the wait (simpler, fine for a demo)?
2. Answering in Telegram through the Chief's `answer_question` tool (recommended: no new
   channel surface) or a `/answer <id> <text>` command like `/approve`?

Size: M. Files: `internal/tools` (new tool, chief tool), `internal/engine` approvals and
walk, `internal/store/approvals.go` and migration, Approvals inbox, run page, seed
prompts and the protocol block.

## 5.25 Run artifacts (feature 4)

Purpose: a run can deliver files, not only a merge request: a report, a dataset, a
rendered document, the diff it produced.

Today: run output is `runs.state` (JSON, capped) and the trace. Files an agent writes in
its workspace stay in the sandbox and are never reachable from the UI.

Scope: the convention `runs/<run id>/out/` in the agent's workspace, told in the
protocol block ("put files you want to deliver under out/"). At the end of every visit
the engine collects that directory (regular files only, links skipped, at most 200
files and 50 MiB per run, larger is truncated with a run event naming what was left)
into the api's artifact store (a new named volume mounted on the api, which mounts none
today, under `artifacts/<run id>/<node id>/<visit>/`) and records each file in `run_artifacts` (run
id, node id, visit, path, size, sha256, content type sniffed, created at). A project run
additionally gets `diff.patch` (the checkout against the base branch) as an artifact of
the visit. API: `GET /api/runs/{id}/artifacts` (list) and
`GET /api/runs/{id}/artifacts/{artifact id}` (download, `Content-Disposition:
attachment`, `X-Content-Type-Options: nosniff`, never rendered inline as HTML). The run
page gets an Artifacts section with sizes, download, and an inline preview for text and
images up to 256 KiB. `run_status` lists artifact names and sizes with the run URL (the
Chief links, it does not send files). A sweeper deletes artifacts older than
`ARTIFACT_TTL_DAYS` (default 30) and the rows with them.

Out of scope: sending files through Telegram, artifacts as inputs of a later run, object
storage, versioning, artifacts from the Chief's conversations.

Decisions (settled 2026-10-17 by Andre with the recommended answer on every line; recorded as D-040 to D-048):

1. Collect at the end of every visit (recommended: a failed later node still leaves the
   earlier deliverables) or only at the end of the run?
2. Include the checkout diff automatically for project runs? Recommended: yes; it is the
   cheapest review aid and costs nothing when there are no changes (then no file).
3. Retention of 30 days (recommended) or forever until deleted by hand?

Size: M. Files: `internal/engine` visit end, a new `internal/artifacts` package, store
file and migration, `internal/httpapi/artifacts.go`, run page section,
`internal/tools/chief.go`, config and compose volume.

## 5.26 Parallel steps (feature 5)

Purpose: a workflow can run several agents at once and continue when all of them are
done: three reviewers on one change, or a task split across coders.

Today: the graph is sequential with loops (5.6; fan-out and fan-in are listed out of
scope there). One node visit runs at a time per run.

Scope: two node kinds, `fork` and `join`. A `fork` takes every outgoing edge whose
condition is true (not only the first), starting one branch per edge; every branch must
reach the fork's `join` (named on the fork: `join: "<node id>"`) without leaving the
branch region, and no edge may enter a branch from outside it; forks do not nest in this
version; loops inside a branch are allowed, loops around a fork-join pair are allowed.
The join waits for every started branch (policy `all`) and continues along its first
true edge; a branch that fails fails the run with `branch <node> failed: <reason>` and
the other branches are cancelled. Branch visits run concurrently as separate node
visits with their own sessions, and the copy-forward of a project checkout goes to each
branch; the join's successor receives the state written by every branch under its node
ids, and a project checkout from branches is not merged (a branch that edits the
checkout is a validation error when another branch does too, unless the operator marks
one branch `checkout: true`). Limits stay per run: cost, steps and tool calls sum across
branches; the time limit is wall clock. The builder gets fork and join nodes and
validates the region rules with path-qualified messages; the run page shows branches
side by side with their visit numbers; the Dev pipeline template is unchanged and a new
`review_panel` template (three reviewers in parallel, a join, a summariser) ships as the
example.

Out of scope: nested forks, `any` or `first` join policies, dynamic fan-out (one branch
per item of a list), merging checkouts across branches, sub-workflows.

Decisions (settled 2026-10-17 by Andre with the recommended answer on every line; recorded as D-040 to D-048):

1. Explicit `fork` and `join` nodes (recommended: the first-true-edge rule of every other
   node stays untouched, validation is local) or implicit fan-out on multiple true edges?
2. On a branch failure: fail the run and cancel the siblings (recommended for a first
   version) or let the join apply the node's `on_failure` edge with the failed branch's
   state missing?
3. Ship the `review_panel` template as the example (recommended) or extend the Dev
   pipeline with a parallel review?

Size: L. Built alone, with the stronger model, after waves 8 to 10. Files:
`internal/engine` graph validation, walk and worker, `internal/store/run_nodes`,
builder (`web/src/pages/WorkflowBuilder*`), run page, `templates/`.

## What happens next

1. Done 2026-10-17: Andre settled every decision line with the recommended answer
   (D-040 to D-048).
2. Done 2026-10-17 for wave 8: `docs/DESIGN.md` 5.18 to 5.21 are written in full with
   the `design-first-brief` template, and `api/openapi.yaml` carries the wave 8 contract.
   The sections for waves 9 to 11 are written when their wave starts, since they build
   on what wave 8 lands.
3. Done 2026-10-18: wave 8 built, reviewed, merged as `0ff8401` and verified live
   (`docs/test-results.md`).
4. Done 2026-10-18 for wave 9: `docs/DESIGN.md` 5.22 and 5.23 are written in full, the
   decisions they settle beyond D-044 and D-045 are D-049 to D-053, `api/openapi.yaml`
   0.6.0 carries the wave 9 contract marked `x-planned: wave 9`, and the briefs are
   under `docs/briefs/wave9/` (four Sonnet 5.5 agents: iteration engine, iteration
   tools, intake, frontend).
5. Next: Andre answers the two open lines at the end of 5.22 (the merged report's
   second destination) and 5.23 (a budget cap required before intake is enabled),
   recorded as D-054 and D-055, and approves the briefs; the coordinator lands the
   prerequisites of `docs/briefs/wave9/00-plan.md`; the wave runs through the loop;
   the demo, README, threat model and test results are re-derived where the wave
   changed them; merge to main.
