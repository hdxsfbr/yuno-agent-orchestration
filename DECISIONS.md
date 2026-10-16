# Assumptions and decisions log

Newest first. Each entry says what was decided, by whom, and why. Design rules the entries
refer to are in `docs/DESIGN.md`. Architecture-level choices also have an ADR under
`docs/adr/`.

## 2026-10-16

**D-029 Schedule firings carry an occurrence key; a fired once-schedule stays fired.**
(Claude, from the wave-3 review) Each scheduled run's `trigger_ref` is
`<schedule id>@<occurrence time>`, unique among schedule runs, so a crash between
creating the run and advancing the schedule cannot fire the occurrence twice. A delay
schedule's occurrence is its `run_at`: re-enabling one that already fired does nothing
until it gets a new `run_at`. Why: exactly-once per occurrence is the scheduler's whole
promise (DESIGN 5.9). Cost: "run it again" is a new time, not a toggle; the Schedules
page's edit form makes that one field.

**D-028 Exec guardrail patterns cover host exec and approvals, not the sandbox.**
(Claude, from agent B) OpenClaw's exec approvals apply to host (`elevated`) exec only;
sandboxed commands run without a gateway hook. Patterns therefore shape the exec policy,
auto-deny matching exec approvals, and block platform tools in the MCP gate, but a
matching command inside the sandbox runs. Why: the sandbox has no network and only the
agent's workspace, so the blast radius is the run directory; a `before_tool_call` plugin
hook is the fix if the demand appears. Cost: the guardrail is weaker than the design's
T5.11.4 reads; the README says so.

**D-027 Eval splits are 17 public and 7 private; the model grades only the rubric.**
(Claude, from agent D) `make eval` runs the public split offline; the private cases need
the network, a configured service or a human and run with `make eval-final`
(`EVAL_SPLIT=all`). `EVAL_PROVIDER=anthropic` changes who grades the rubric, never how
runs are driven (always the scripted model); a grader failure scores 0 with the reason,
no silent fallback. The schedule case uses an agentless workflow because a schedule
cannot carry the scripted model override. Why: `make eval` must stay offline and
deterministic; the rubric is the only place a real model adds judgement. Cost: the
channel-triggered reply is not an eval case (it needs a real Telegram message); the
conversation mirroring is covered by unit tests instead.

**D-026 Telegram stays closed until an agent is bound; binding is a config patch plus an
in-place gateway restart.** (Claude, from agent A) The baseline writes
`channels.telegram.dmPolicy: "disabled"`; `PUT /api/channels/telegram/binding` patches
`bindings[]` (dmScope per-channel-peer) and `dmPolicy: allowlist`, then asks
`gateway.restart.request`, which restarts the gateway in place (same container, about
five seconds, the api reconnects); unbinding closes the channel again. The
`channel_bindings` table is the truth and a keeper re-asserts it at every api start and
reconnect. Why: `enabled: false` would stop polling and Telegram would hand the held
messages to the next bound agent; `dmPolicy: disabled` drops them. Cost: a message sent
while unbound is dropped without a platform log line (OpenClaw drops it before logging);
the Channels page shows the channel as closed.

**D-025 The Telegram channel never answers from OpenClaw's default agent.** (Claude,
from the first live probe) With Telegram enabled and no binding, the gateway routed
Andre's first two messages to its built-in `main` agent, which replied on Opus. The
platform must make that impossible: a message is answered only by the platform agent
bound to the channel, otherwise dropped and logged. Why: an unbound channel talking to
an unmanaged agent on a paid model is both a cost and a trust problem. Cost: before the
first binding the bot stays silent. Mechanism chosen in wave 3 (DESIGN 5.8 rule 7).

## 2026-10-15

**D-024 Task templates render missing values as empty; earlier outputs arrive as a context
block.** (Claude, from agent A) `{{.state.reviewer.feedback}}` before the reviewer has run
renders as an empty string rather than `<no value>` or an error, so one template serves
the first and the looped visit; `input_from` appends `Context from earlier nodes:` with
each named node's output as compact JSON (`start` is the run input). Why: Go
`text/template`'s `missingkey=zero` does not cover decoded JSON maps (verified). Cost: a
typo in a state path is silent at save time; the save-time render still catches syntax.

**D-023 Template rows are read-only and hidden from the workflow list.** (Claude, from
agents B and C) The rows `templates/*.json` load into carry `template_key`; the list
endpoint leaves them out, PUT and DELETE on them answer 409, and the builder opens one
with a banner and a "Create editable copy" action (instantiate). Why: a template must
stay what the file says so `make run` on a fresh machine behaves the same. Cost: an
operator cannot tweak the shipped template in place; the copy is one click.

**D-022 Idempotency and gating details.** (Claude, from agents B and D) The idempotency
key excludes `run_token` from the canonical input (the key already carries the node
visit, and stored inputs have the token redacted); `finish` is always permitted and
always `auto`; a read-only tool set to `require_approval` is gated without a key; a
`blocked` platform tool is removed from the agent's OpenClaw tool list, so OpenClaw
refuses the call before it reaches the MCP gate and no `tool_calls` row exists for it
(the gate still refuses it when reached). Why: keys must be reproducible from the row;
finish is the protocol, not a side effect. Cost: a blocked attempt is visible only in
OpenClaw's own log, not in the trace.

**D-021 Workflow runs layer limits as request, workflow, defaults, clamp.** (Claude, from
agent A) Agent-level limits are not applied to workflow runs this wave; the min-of rule
across participating agents is the guardrails work of wave 3 (DESIGN 5.11 rule 1). Why:
one place to read a run's limits. Cost: an agent's own `max_cost_usd` is not yet a cap
inside a workflow.

**D-020 A run waits only while a tool call waits; tool approvals outlive a successful
run.** (Claude, from agent A) The run is `waiting_approval` only while an MCP call is
blocked on the decision; once the wait ends the run is `running` again with the approval
still pending, and a pending tool approval stays decidable after the run succeeded so the
late grant of D-014 can execute the effect. On a failed, cancelled or timed-out run every
pending approval expires. This supersedes the revision 1 test T5.1.2 ("terminal runs
reject approval") for kind `tool`. Why: D-014's block-then-fall-back needs it. Cost: an
inbox may show a pending approval for a run that already succeeded; the card shows the
run status.

**D-019 The run directory is copied forward between agents.** (Claude, from agent D's
finding) OpenClaw gives every agent its own workspace and its sandbox sees only that, so
the Reviewer could not read the Coder's files. Before each agent node visit the engine
copies `runs/<run_id>/` from the earlier visits' agent workspaces into the next agent's
workspace (later visit wins); the deploy tools read the calling agent's copy. Why: a
copy through the shared state directory needs no gateway configuration and keeps the
sandbox isolation; a shared bind mount would need a probe of how OpenClaw's file tools
resolve paths. Cost: large run directories are copied once per node visit. DESIGN 5.6
rule 13.

**D-018 Templates name agents; `make seed` creates them.** (Claude, unobjected) A template's
agent node carries `agent_name` ("Coder") instead of an id, and instantiating a template
resolves names to the agents of that name, failing with a path-qualified 422 when one is
missing. `yuno seed` creates the six demo agents once and never overwrites an edited one.
Why: templates are files in the repository and cannot know database ids; the alternative,
embedding agent definitions in templates, would duplicate the agent model. Cost: a
renamed agent breaks instantiation until the template is edited; the error names it.

**D-017 Side-effect approvals register with the gateway two-phase; the gateway's own
timeout is not a decision.** (Claude, from the probe) `plugin.approval.request` blocks
until a decision unless `twoPhase` is set, and its timeout (at most 10 minutes) ends in a
`deny` event with `resolvedBy: null`. The engine registers two-phase, treats that event
as no decision, and keeps the approval pending until the platform's own expiry, decidable
from the web UI. Why: approval timeouts are a product setting (default 24 h), not a
gateway constant. Cost: after the gateway's window only the platform's surfaces can
decide; chat-side decisions are verified with the Telegram channel in wave 3. Details in
`docs/DESIGN.md` 5.3 rule 5a; `make probe-approval` re-verifies.

**D-016 Model upstream is configurable; the demo runs through OpenRouter.** (Andre)
`MODEL_UPSTREAM=anthropic|openrouter`. OpenRouter serves the same Claude models through
the Anthropic messages format (verified 2026-10-15: `/api/v1/messages`, streaming and
tool use), so the proxy only switches the auth header to a bearer token and rewrites
model ids (`claude-opus-5-5` to `anthropic/claude-opus-5.5`); agents, the UI and the
price table are unchanged. Why: Andre's key is an OpenRouter key. Cost: OpenRouter's
usage block carries its own cost figure, which OpenClaw and the platform ignore in
favour of the price table, identical for these models.

**D-015 OpenClaw Tool Search is off for the platform gateway.** (Claude, from integration)
`tools.toolSearch: false` in the baseline and re-asserted at every api start. OpenClaw
otherwise hides MCP tools behind `tool_search` / `tool_call` meta-tools and a direct
`yuno__finish` call fails with "not found". Why: the finish protocol and the scripted
eval provider call platform tools by name; platform agents have small allowlists, so
there is no catalogue to compact. Cost: an agent given a very large tool set would get
a long prompt; none exists in this platform.

**D-014 Gated platform tools block, then fall back.** (Andre) An approval-gated MCP tool
call waits up to `MCP_TOOL_WAIT` for the human decision so the agent sees the outcome in
its turn; on timeout it returns `pending_approval` and the engine executes the effect
itself once approved, with the same idempotency key. Why: best demo experience with both
paths tested (DESIGN 5.3 rule 5).

**D-013 Telegram is OpenClaw's native channel. Supersedes D-002 and D-008 in part.**
(Andre) The agent is bound to OpenClaw's Telegram plugin (`channels.telegram` with
`webhookUrl` and `webhookSecret`, `bindings[]`), which verifies the secret token, keeps
the durable ingress queue and the conversation history, and handles exec approvals from
chat. The platform surfaces that history in its UI from the gateway and mirrors it to
Postgres. Why: Andre prefers the runtime's own multi-channel support over a parallel
adapter. Cost: the webhook verification and history code the brief grades are OpenClaw's,
documented and demonstrated rather than written here; side-effect approvals from chat
depend on OpenClaw's plugin-approval routing (verified in wave 2, web UI otherwise).

**D-012 Agent shell commands run in OpenClaw's Docker sandbox, always on.** (Andre)
`agents.defaults.sandbox.mode: "all"`; the gateway container gets the Docker socket and
host-path-mapped workspaces so sibling sandbox containers can mount them. Why: untrusted
code never runs in the gateway container. Cost: mounting the Docker socket is
root-equivalent on the host, recorded in the threat model; more setup surface.

**D-011 Both brief versions bind; OpenClaw is the execution engine. Supersedes D-001.**
(Andre) After reassessing with the PDF treated as binding alongside the portal PRD,
Andre chose OpenClaw over OpenCode and Goose as the embedded runtime. The Go control
plane (API, workflow engine, scheduler, store, UI, evaluation runner) orchestrates
OpenClaw agents over the gateway WebSocket protocol; OpenClaw executes every agent
turn and its tools. Platform tools that need idempotency and the approval gate are
served to OpenClaw over MCP from the Go binary. Why: the PDF requires one of the three
runtimes and Andre runs a four-agent OpenClaw fleet already, so the platform becomes a
real control plane for a runtime he uses. Trade-offs and the integration contract are
in `docs/adr/0001-runtime.md` (rewritten) and `docs/DESIGN.md` section 5.14.

**D-010 Deploy side effects: local deploy target plus optional GitLab MR.** (Andre)
`deploy_release` targets the `deploytarget` compose service and is always available, so
the approval-gated side effect works offline. `gitlab_create_mr` is a second
side-effecting tool, registered only when `GITLAB_TOKEN`, `GITLAB_PROJECT_ID` and
`GITLAB_BASE_URL` are set. Why: the brief mandates a local fallback and also rewards a
real external service.

**D-009 Operators may set a side-effecting tool to `auto` per agent.** (Andre)
The agent form shows a warning on that field; shipped templates keep `deploy_release`
and `gitlab_create_mr` gated. Why: the brief's interaction rules are "what the agent can
do autonomously vs what requires approval", which implies the operator chooses.

**D-008 Approvals can be decided from Telegram.** (Andre)
`/approve <id> [reason]` and `/reject <id> [reason]` act on pending approvals of runs
that were triggered from that conversation, and only from user ids in
`TELEGRAM_ALLOWED_USER_IDS` when that list is set. Why: makes the live conversation in
the demo meaningful; the restriction keeps a random chat member from approving a deploy.

**D-007 Rubric evaluator has an offline heuristic mode.** (Andre)
Every rubric criterion carries a local check (`contains`, `regex`, `json_path_eq`). With
`EVAL_PROVIDER=scripted` the rubric is scored by those checks and the report labels the
mode. Why: `make eval` must run with no cloud dependency.

**D-006 Approval wait time does not count toward the run time limit.** (Claude,
unobjected) Approvals have their own expiry, default 24 h. Why: a human may take hours;
a 10-minute run limit should bound agent work, not human latency.

**D-005 Everything runs in Docker.** (Andre) No toolchains installed on the host. The
Makefile wraps `docker compose`; Go and pnpm caches are named volumes. Why: keep the host
clean.

**D-004 Tracer-bullet build order.** (Andre) Wave 1 is a thin end-to-end slice (agent
form, run, real model, real tool, Postgres, SSE to the browser), later waves widen each
layer. Why: integration risk surfaces on day one and the first eval report is a true
baseline.

**D-003 Demo workflow is the dev pipeline.** (Andre) Coder writes code, Reviewer approves
or sends feedback back, Deployer deploys after a human approval. The loop is data in the
workflow definition, not Go code. Why: it is the brief's own example and exercises every
required mechanism (loop, typed state, sandbox, gated side effect).

**D-002 Telegram is the external channel.** (Andre) Webhook mode with the
`X-Telegram-Bot-Api-Secret-Token` header check; long-polling fallback for local runs
without a tunnel. Why: no workspace admin needed, easy to demo from a phone; the adapter
interface keeps Slack and WhatsApp addable.

**D-001 Native Go runtime, not Goose, OpenClaw or OpenCode.** (Andre) The attached PDF
(March 2026) requires embedding one of those; the portal PRD (October 2026, the version
the submission is tied to) makes frameworks optional. We build to the portal PRD. Why:
step and time limits, the approval gate, idempotency keys and per-call token accounting
are the heaviest-weighted rubric items, and a loop we own enforces them directly. See
`docs/adr/0001-runtime.md`.
