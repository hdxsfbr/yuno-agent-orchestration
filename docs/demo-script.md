# Demo walkthrough (5 to 10 minutes)

The video is edited from this list: one scene per section, what is on screen, what the
viewer learns, and the narration. Every scene is recorded from the running build at the
commit named in the video's description; waits for the model and the gateway are cut in
post (the click and about a second of the pending state stay, the dead air goes), nothing
is simulated. The scenes that need the model (3, 4, 5) use the real provider through the
model proxy; the evaluation scene uses the deterministic scripted model, as `make eval`
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

Narration: "An agent is configured entirely from the UI: its role and prompt, the model,
the tools it may use and whether each needs a human, its limits and guardrails, memory
with a retention policy, skills, and the channel it's bound to. Saving pushes the
configuration into OpenClaw; the runtime card shows what the gateway holds, including the
markdown files that define the agent there."

## 3. Building a workflow from a template (60 s)

On screen: Workflows page; "From template" drawer; instantiate the dev pipeline; the
builder opens: start → coder → reviewer, the `changes_requested` edge back to coder with
its condition, the `approved` edge to the human gate, then deployer → end; open the
reviewer node (task template, output schema with the verdict enum), the edge condition
editor, the limits panel (loop visits 3); Validate; Run with the task "implement a
function that returns the nth Fibonacci number, with tests".

Viewer learns: the loop is data (an edge with a condition), the approval gate is a node,
the task is a template over typed state; templates are copied, then edited.

Narration: "Workflows are graphs. This is the dev pipeline template: Coder, Reviewer,
a human gate, Deployer. The feedback loop is just an edge with a condition on the
reviewer's typed output; the loop limit bounds it. Nothing about Coder or Reviewer is
hard-coded in the engine. I'll run it with a small task."

## 4. A successful run, live (150 s after cuts)

On screen: the run page as it happens: node Coder running, model calls and sandboxed
`exec` tool calls appearing, the agent message from coder to reviewer, Reviewer's
verdict; the human gate: run waiting, the Approvals inbox badge; approve in the inbox
(reason "looks good"); Deployer calls `deploy_release`, which blocks on a second
approval (a side-effecting tool); approve it; the deploy target's release URL; the run
succeeds. Then the trace: per-call tokens and cost, cache columns, the state panel, the
agent messages section, the logs.

Viewer learns: two agents cooperated through typed state, every tool call and model call
is recorded with tokens and cost, both kinds of approval blocked until a human decided,
the side effect ran once.

Narration: "Coder writes the code and runs the tests in its sandbox; when it finishes,
its typed output becomes state and a message to Reviewer. Reviewer runs the tests
itself and approves. The run now waits at the human gate; I approve it from the inbox.
Deployer calls the deploy tool, which is side-effecting, so the call itself blocks for
approval; approved, it runs exactly once under its idempotency key and the release
appears on the local deploy target. The trace shows every call, token and cent."

## 5. A handled failure (40 s)

On screen: a quick run on Coder with `max_steps` set to 2 and a task that needs more;
the run fails with reason "step limit 2 exceeded"; the Runs list shows it; the trace
shows the abort after the second model call. Then an approval rejected in the inbox on
another run (reason given) and the agent's run finishing cleanly with "rejected".

Viewer learns: limits terminate runs deterministically with a named reason; a rejection
is a result the agent can act on, not a crash.

Narration: "Limits are enforced by the platform, not by the model's good will: here the
step limit stops a run after two calls, with the reason on the run. And a rejected
approval goes back to the agent as a result, so it finishes cleanly instead of failing."

## 6. Evaluations (45 s)

On screen: a terminal running `make eval` (the public split, 17 cases, deterministic
scripted model), the summary line, then the report `evals/reports/baseline.md` rendered:
per-case table with the seven evaluators and the rubric column, the failures section
empty.

Viewer learns: the suite is reproducible and offline; every case checks termination,
trace integrity, expectations, the approval gate, idempotency, schemas and allowlists;
the rubric is graded heuristically offline and by a model when asked.

Narration: "The evaluation suite runs offline against the same stack with a scripted
model: twenty-four versioned cases, seventeen public, seven evaluators, a rubric. The
baseline report is in the repository and `make eval` reproduces it."

## 7. A live Telegram conversation (60 s)

On screen: the Channels page: the Telegram card with the bot, the allowlisted user and
the bound agent (Coder); the conversation list; a message sent from the phone to the
bot (phone screen recording inset or still); the transcript on the Channels page
updating live with the inbound message and the Coder's reply; the gateway's inbound and
outbound log lines in the Status page's OpenClaw panel or the api logs.

Viewer learns: the channel is OpenClaw's native Telegram integration, bound to a
platform agent from the UI, with the history persisted and visible.

Narration: "One agent is bound to Telegram from the Channels page; the channel stays
closed until an agent is bound and only allowlisted users can talk to it. I'll message
the bot from my phone: the message reaches Coder, which answers with its configured
personality, and the whole conversation is mirrored here."

## 8. Schedules and status (30 s)

On screen: Schedules page: a once-schedule for the research pipeline two minutes from
now, then the fired run in the Runs list with trigger "schedule"; the Status page:
gateway connection, scheduler tick and next due, queue counts.

Narration: "Runs can also start on a schedule, cron or once, in any time zone; the
status page shows the gateway, the scheduler and the queues."

## 9. Closing (10 s)

On screen: closing card with the repository, the README's sections (architecture,
runtime choice, threat model, evaluation), `make run` / `make test` / `make eval`.

Narration: "The README has the architecture, why OpenClaw, the threat model and how to
add a template or a channel; `make run`, `make test` and `make eval` are the whole
interface. Thanks for watching."

## Recording notes

- Pre-flight: the stack at the final commit (`make run`, `make smoke`), the six seeded
  agents, a fresh dev-pipeline copy from the template, the Coder bound on Channels, no
  leftover runs from a dry run on the first screen (filter the Runs list or seed a clean
  database), the Telegram phone ready for scene 7.
- Scene 7 needs a human on the phone during capture; scene 8 is time-bound (the
  schedule fires two minutes after creation); record the time-free scenes first.
- Capture: Playwright in a container, 1440x900, one WebM per scene, `SCENES=04,06` to
  re-record one; title cards are HTML stills; waits recorded to `cuts.json`.
- Post: uniform re-encode, concat, narration muxed per scene, stills held for their
  narration; watch the finished file end to end before calling it done.
