# Demo walkthrough (5 to 10 minutes)

The video is edited from this list: one scene per section, what is on screen, what the
viewer learns, and the narration. Every scene is recorded from the running build at the
commit named in the video's description; waits for the model and the gateway are cut in
post (the click and about a second of the pending state stay, the dead air goes), nothing
is simulated. The scenes that need the model (3, 4, 5, 6) use the real provider through the
model proxy; the evaluation scene (7) uses the deterministic scripted model, as `make eval`
does.

## 1. Title (5 s)

On screen: title card "Yuno Agent Orchestration Platform", the commit, the date.

Narration: "This is the Yuno agent orchestration platform: a Go control plane over an
OpenClaw runtime, with a React UI, Postgres, Docker compose, and one agent on Telegram.
Everything you'll see is the running build at this commit."

## 2. Configuring an agent (60 s)

On screen: Agents list with the six demo agents; open Coder; scroll the form: role,
system prompt, model, the tool picker with per-tool policy (auto, require approval,
blocked), limits, guardrails (cost cap, runs per hour, blocked tools, input patterns),
memory policy and saved facts, skills, the exec policy, the Telegram line, the runtime
card showing what OpenClaw holds for this agent.

Viewer learns: every dimension the brief names is configured here without code, and the
form shows what was materialised into the runtime.

Narration: "An agent is configured entirely from the user interface, without touching code. The Agents page lists the six demo agents with the channel each one is bound to and its live state. Opening the Coder shows every dimension the brief names: its role and system prompt, the model, the tools it may use and, for each tool, whether it runs on its own, needs a human approval, or is blocked. Below that are the operating limits, the guardrails for cost, rate and blocked inputs, the memory policy with retention, the skills it can load, its exec policy, and the Telegram line. Saving pushes all of this into OpenClaw, and the runtime card shows what the gateway actually holds, including the markdown files that define the agent there."

## 3. A project, and a workflow from a template (75 s)

On screen: Projects page; New project: the name, the repository URL, the default branch
and the access token (a password field, so dots on screen); Create project; Check on the
new row answers that the token is accepted and can push. Then the Workflows page; "From
template" drawer; instantiate the dev pipeline; the builder opens: start → coder →
reviewer, the `changes_requested` edge back to coder with its condition, the `approved`
edge to the human gate, then deployer → end; open the reviewer node (task template,
output schema with the verdict enum), the edge condition editor, the limits panel (loop
visits 3); Validate; Run: the dialog asks for the project, then the task "implement a
function that returns the nth Fibonacci number, with tests".

Viewer learns: a pipeline that works on code names a project, whose token is stored
write-only and encrypted; the loop is data (an edge with a condition), the approval gate
is a node, the task is a template over typed state; templates are copied, then edited.

Narration: "A pipeline that works on code starts from a project: a git repository registered on the Projects page with its address and a token that can push. The token is stored encrypted and never shown again; Check asks the provider whether it is accepted and can push. Workflows are graphs. From the Workflows page, the templates drawer offers two shipped pipelines; instantiating the dev pipeline makes an editable copy. In the builder you can see Coder, then Reviewer, then a human gate, then Deployer. The feedback loop is nothing more than an edge from Reviewer back to Coder with a condition on the reviewer's typed verdict; the other edge leads to the gate when the verdict is approved. Each agent node has a task template over the run input and the state so far, and an output schema the agent's result must satisfy. The limits panel bounds the loop. Nothing about Coder or Reviewer is hard-coded in the engine. Validation reports problems on the node or edge they belong to. The dev pipeline works on a project, so the run names one. I'll run it on that repository with a small task: a Fibonacci function with tests."

## 4. A successful run, live (150 s after cuts)

On screen: the run page as it happens: the project and the toolchain in the summary,
node Coder running in a checkout of the repository, model calls and sandboxed `exec`
tool calls appearing, the agent message from coder to reviewer, Reviewer's verdict; the
human gate: run waiting, the Approvals inbox badge; approve in the inbox (reason "looks
good"); Deployer calls `open_merge_request`, which blocks on a second approval (a
side-effecting tool); approve it; the merge request's address in the tool's output and
its link in the run summary; the run succeeds. Then the trace: per-call tokens and cost,
cache columns, the state panel, the agent messages section, the logs.

Viewer learns: two agents cooperated through typed state on a real repository, every
tool call and model call is recorded with tokens and cost, both kinds of approval
blocked until a human decided, the side effect ran once and left a merge request.

Narration: "This is the run, live, on the project's repository, cloned into the Coder's workspace. The Coder's turn executes in OpenClaw: each model call appears with its tokens and cost, and each tool call with its input and output, including the exec calls that run the tests inside the agent's Docker sandbox. When the Coder finishes, its typed output becomes part of the run state and a message to the Reviewer, which you can see in the agent messages section. The Reviewer reads the code, runs the tests itself, and approves. The run now stops at the human gate: it is waiting, the Approvals badge lights up, and the inbox shows exactly what is being approved. I approve it with a reason. The Deployer starts and asks to open a merge request. That tool is side-effecting, so the call itself blocks until a human decides; the second approval shows the full input, the title and the description. Once approved, the platform commits the checkout on a branch, pushes it and opens the merge request exactly once under the run's idempotency key, the run succeeds, and the request is linked from the run page. The trace keeps everything: every call, every token, every cent, the state, and the messages between the agents."

## 5. A review panel: three reviewers at once (90 s after cuts)

On screen: the Workflows page; the Review panel in the builder: start → coder → fork
`panel` → three reviewer branches (security, performance, maintainability) → join
`merge` → lead Reviewer → human gate → Deployer; the fork node's settings (its join),
the join, the lead and the gate selected in turn. The run dialog: the project, the task
"write slugify(text): lowercase, spaces to hyphens, strip punctuation, with tests". The
run page as it happens: the header counts "3 branches running", the fork card shows one
column per branch, each with its own model calls and tool calls; the join card closes
the fork ("Joined 3 branches"); the lead's node card and verdict; the human gate and the
`open_merge_request` approval, decided as in scene 4; the run succeeds and its merge
request is linked in the summary. (A dry run opens an earlier finished panel run
instead, `DEMO_PANEL_RUN_ID`.)

Viewer learns: parallel work is data too: a fork node and a join node in the same graph
editor; the branches run at the same moment, each recorded separately; one lead merges
their verdicts, and the same approvals guard the ship.

Narration: "A review does not have to be one reader. The Review panel workflow has the Coder write the change, and then a fork node splits the run into three branches: a security reviewer, a performance reviewer and a maintainability reviewer, each looking at the same code for its own concern. A join node waits for all three, and a lead Reviewer merges their verdicts into one. If the lead asks for changes the work goes back to the Coder; if it approves, the run stops at the human gate and then the Deployer opens the merge request. Here is a run of it. The Coder finishes, and the three reviewers start at the same moment: the header counts three branches running, and the fork card shows one column per branch, each with its own model calls and tool calls. When the last one finishes, the join card closes the fork, the lead reads the three verdicts and gives one of its own, and the run waits at the gate. I approve it, and approve the merge request when the Deployer asks. The run succeeds, and the merge request is linked from the summary, carrying the work three reviewers have already read."

## 6. A handled failure (40 s)

On screen: a quick run on Coder with `max_steps` set to 2 and a task that needs more;
the run fails with reason "step limit 2 exceeded"; the Runs list shows it; the trace
shows the abort after the second model call. Then a pipeline run on the project: the gate approved, the Deployer's
`open_merge_request` approval rejected in the inbox (reason given), and the run finishing cleanly with
the rejection in its state.

Viewer learns: limits terminate runs deterministically with a named reason; a rejection
is a result the agent can act on, not a crash.

Narration: "Failures are handled by the platform, not left to the model's good will. First, a limit. I set the Coder's maximum steps to two and start a task that needs more. After the second model call the engine aborts the turn and the run fails with the reason spelled out: step limit two exceeded. The Runs list shows it, and the trace shows exactly where it stopped. The limit is restored afterwards. Second, a rejection. I ask the Deployer to publish a release; it calls the deploy tool, which waits for a human. This time I reject it, with a reason. The rejection is not an error thrown at the agent: it comes back as a result, with the reason, so the Deployer reports the status and finishes cleanly. Nothing was deployed, and the trace records the decision and who made it."

## 7. Evaluations (45 s)

On screen: a terminal running `make eval` (the public split, 17 cases, deterministic
scripted model), the summary line, then the report `evals/reports/baseline.md` rendered:
per-case table with the seven evaluators and the rubric column, the failures section
empty.

Viewer learns: the suite is reproducible and offline; every case checks termination,
trace integrity, expectations, the approval gate, idempotency, schemas and allowlists;
the rubric is graded heuristically offline and by a model when asked.

Narration: "The evaluation suite runs offline against the same stack with a deterministic scripted model, so no cloud is needed to evaluate the platform. Twenty-four versioned cases, seventeen public and seven private, each judged by seven deterministic evaluators: termination, trace integrity, expectations, the approval gate, idempotency, schemas and allowlists, plus a rubric graded heuristically offline or by a real model when asked. The report in the repository is reproduced by make eval; this one is the baseline with every case passing."

## 8. The Chief of Staff on Telegram (60 s)

On screen: the Channels page: the Telegram card with the bot, the allowlisted user and
the bound agent (the Chief of Staff); the conversation list; the Chief's conversation. A
message sent from the phone to the bot appears in the transcript, followed by the
Chief's reply a few seconds later, with the link to the run it made while answering,
which is opened; when the reply names the panel run from scene 5, that run's fork card
is shown again.

The message to send from the phone (the recorder prints it with its WAITING line):
"How did run <the panel run's id> go? Tell me what each reviewer found." (the Chief looks a run up by its id) The Chief answers
with the run_status tool, which reports the run's branches. Record scene 5 first, so the
run exists.

Viewer learns: the channel is OpenClaw's native Telegram integration, bound to a
platform agent from the UI; the agent on the other end is the coordinator, which reads
the platform's own runs, branches included, and answers from them; the history is
persisted and visible.

Narration: "The agent on Telegram is the Chief of Staff, the one that coordinates the others. The Channels page shows the bot, the allowlisted user, and the agent bound to the channel; the channel stays closed until an agent is bound, and only allowlisted users can talk to it. Inbound messages arrive over a verified webhook on the instance's public address. From my phone I ask how the review panel run went, naming the run, and what each reviewer found. The message appears here, and the Chief answers by looking the run up: it reads the run's status together with its three branches, and reports what the security, performance and maintainability reviewers each said, and what the lead decided. The whole conversation is mirrored here, live, and the history is persisted, so it survives a restart and can be inspected afterwards."

## 9. Schedules and status (30 s)

On screen: Schedules page: a once-schedule for the research pipeline two minutes from
now, then the fired run in the Runs list with trigger "schedule"; the Status page:
gateway connection, scheduler tick and next due, queue counts.

Narration: "Runs can also start on a schedule. From the Schedules page I create a once-schedule for the research pipeline, two minutes from now, with its input. The scheduler picks it up on its next tick; the row shows the last run and, for repeating schedules, the next one, in any time zone. The run appears in the Runs list with the trigger set to schedule, and the Researcher starts working. The Status page shows the commit the stack was built from, the gateway connection, the scheduler's tick and next due time, and the queues."

## 10. Closing (10 s)

On screen: closing card with the repository, the README's sections (architecture,
runtime choice, threat model, evaluation), `make run` / `make test` / `make eval`.

Narration: "The README has the architecture, why OpenClaw, the threat model and how to
add a template or a channel; `make run`, `make test` and `make eval` are the whole
interface. Thanks for watching."

## Recording notes

- Pre-flight: the stack at the final commit (`make run`, `make smoke`), the six seeded
  agents, a fresh dev-pipeline copy from the template, an editable copy of the Review panel, the project's repository and token
  in `.env` (`PROJECT_REPO_URL`, `PROJECT_GIT_TOKEN`; scene 3 creates the project, or
  reuses the one that already has its name), the Chief of Staff bound on Channels, no leftover
  runs from a dry run on the first screen (filter the Runs list or seed a clean
  database), the Telegram phone ready for scene 8.
- The dev pipeline works on a project. A workflow that does not opt in
  (`DEMO_PIPELINE_WORKFLOW`) skips the Projects page and ends with `deploy_release` on the
  local deploy target instead of a merge request; the recorder handles both, and the
  narration above describes the project path.
- Scene 8 needs a human on the phone during capture, and a panel run (scene 5) to ask about; scene 9 is time-bound (the
  schedule fires two minutes after creation); record the time-free scenes first.
- Capture: Playwright in a container, 1440x900, one WebM per scene, `SCENES=04,07` to
  re-record one; title cards are HTML stills; waits recorded to `cuts.json`.
- Post: uniform re-encode, concat, narration muxed per scene, stills held for their
  narration; watch the finished file end to end before calling it done.
