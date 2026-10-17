# Assumptions and decisions log

Newest first. Each entry says what was decided, by whom, and why. Design rules the entries
refer to are in `docs/DESIGN.md`. Architecture-level choices also have an ADR under
`docs/adr/`.

## 2026-10-17

**D-038 Projects are records created in the UI; a run names its project; tokens are
encrypted at rest; `PROJECT_*` only bootstraps.** (Andre, 2026-10-17) The single
environment-configured project of D-037 becomes a `projects` table an operator fills in
on a Projects tab: name, https URL, provider (inferred from the host when left empty),
default branch, sandbox network opt-in, description, and a git token stored AES-GCM
encrypted under `PROJECT_TOKEN_KEY` (generated into `.env` like the other platform
tokens) and never returned by the API (`token_set` says one is stored). A run request,
the Run dialog, the quick-run dialog and the Chief of Staff's `delegate_task` name the
project; a workflow that opts in (`project: true`) requires one and one that does not
refuses one; the run keeps a snapshot of the settings and reads the token from the row
at clone and push time, so a deleted project finishes its runs and a replaced token
applies to runs in flight. `open_merge_request` is registered always and takes the
project from the run. A Check action lists the repository through the provider's API
and reports the default branch and whether the token can push, without a clone. At
start the api turns a configured `PROJECT_*` into a project once, named after the
repository path, so the current deployment keeps working; after that the page is the
truth. Why: the operator wants several repositories and wants to say "on the playground
project" to the Chief, and a token in `.env` cannot be rotated or added without a
redeploy; records with a write-only, encrypted token keep the credential on the api
side (D-037's rule) while making projects a product object. Cost: one more secret in
`.env` (`PROJECT_TOKEN_KEY`), whose loss makes every stored token unreadable (the check
says so and the operator enters the tokens again); schedules do not name a project yet,
so a scheduled run of a project workflow fails its validation until they do. Design:
`docs/DESIGN.md` 5.17.

**D-037 Projects are git repositories configured by environment variables; toolchains
come from the repository and run in prebuilt generic images; sandbox network is a
per-project opt-in; no agent clones or pushes.** (Andre, 2026-10-17) One project per
platform, set by `PROJECT_REPO_URL`, `PROJECT_GIT_TOKEN`, `PROJECT_DEFAULT_BRANCH`,
`PROJECT_PROVIDER` and `PROJECT_SANDBOX_NETWORK`; a workflow or quick run opts in with
`project: true` (the seeded Dev pipeline does). The engine clones the default branch at
depth 1 into the first agent's run directory before its turn and the copy-forward hands
the checkout on; the toolchain is read from the manifest (go.mod, package.json plus a
lockfile, pyproject.toml, requirements*.txt, uv.lock) and selects one of three shipped
images (`yuno-sandbox-{python,node,go}`) with nothing project-specific inside; the
materialiser sets `agents.entries.<id>.sandbox` for the visit's duration with scope
`session`, because the gateway keeps a hot agent-scoped container on its old image.
Sandbox network stays off unless `PROJECT_SANDBOX_NETWORK=true`, which exists so agents
can install the project's dependencies themselves; the no-network rule of D-012 was a
design choice, not a brief requirement, and it is now a per-project choice recorded in
the threat model. The token stays on the api side: git receives it as an
`http.extraheader` through the environment, the checkout's config holds the bare URL,
and the Deployer opens the merge request through the platform tool `open_merge_request`
(GitLab, then GitHub behind the same interface), which commits and pushes from the api.
Why: a demo that writes loose files into a run directory shows the mechanism, not the
job; a run that starts from a real repository with its own toolchain and ends in a merge
request is what the pipeline is for, and keeping every credential and every push on the
platform side keeps the model out of the trust boundary. Cost: one more container per
node visit of a project run (session scope), three more images to build, and with
network on the sandbox can exfiltrate, so the opt-in is for trusted repositories only.
Design: `docs/DESIGN.md` 5.16.

## 2026-10-16

**D-036 One coordinator agent on Telegram; chat-time tools identified by a chat token.**
(Andre, 2026-10-16) The person talks to one agent, the Chief of Staff, which delegates
to the others as platform runs and relays their outcomes; it is seeded, bound when
nothing else is, and talks through OpenClaw's native chat. Because the gateway's MCP
client carries no identity, chat tool calls carry a per-agent `chat_token` the way run
calls carry `run_token`; only six tools accept it. Delegation needs no approval by
default (the delegated run's side effects keep theirs); approvals are decided in the
UI and the chat is only told; both templates are instantiated at seed so workflows can
be named. Why: the demo had six peers and no front door; a coordinator that lives
inside the platform's runs keeps every delegated task traced, limited and gated, which
OpenClaw's own sub-agent spawning would bypass. Design: `docs/DESIGN.md` 5.15.

**D-035 Public deployment is a plain Docker host behind Caddy; CI deploys through
runners we register.** (Andre, 2026-10-16) The stack needs the Docker socket, a network
namespace shared between api and gateway, a bind mount both write and 4 GB for the
gateway, so Railway, App Platform and the other managed-container products cannot run it;
a DigitalOcean droplet runs the same compose file with a `public` profile that adds Caddy
for TLS, with every other port bound to loopback because Docker publishes past ufw.
`.gitlab-ci.yml` tests on a `yuno-ci` shell runner and deploys from a `yuno-prod` shell
runner on the droplet, manual on `main` by default and automatic with `DEPLOY_AUTO=true`;
pipelines exist only while `YUNO_CI=true`, so the repository shows no stuck pipelines
before a runner is registered. Why: the approved vendor list offers DigitalOcean, Vultr and
Railway for hosting, and only a VM fits; a shell runner is the only executor that can
bind-mount the checkout into the tool containers. See `docs/deploy.md`.

**D-034 Runtime reconciliation details.** (Claude, from the fix pass) An agent deleted
while one of its node visits is running stays in the runtime until the next full sync,
so the run finishes on its snapshot (DESIGN 5.6 edge case); the api re-materialises
out-of-date agents every 30 s besides the full sync at start and reconnect, so agents
seeded by `make run` are usable at once. The UI keeps the API token in the browser's
local storage and sends it only as a header; the sign-in dialog cannot be dismissed
while the API refuses the token; streams reconnect when the token changes. Why: the
simplest behaviours that keep the runtime's agents equal to the platform's and the
approval gate closed to the network. Cost: a deleted busy agent lingers for one run.

**D-033 Disclosed deviations from the audit, kept on purpose.** (Andre, 2026-10-16, on the
coordinator's recommendation) The runtime keeps its own session transcripts, thinking
included, inside its state directory; the platform never stores or shows reasoning and
that is the brief's "never store/expose chain-of-thought" as read here. Schedules target
workflows; a per-agent schedule is that agent's quick-run workflow. The deterministic
local fallback is the evaluation path (scripted model); UI runs need a model key. The
evaluation suite replays scripted runs and judges outcomes against expectations; the
real-model run is the demo. Guardrails and limits cover runs, not chat conversations
(the sender allowlist bounds those). The termination evaluator tolerates one extra step
and the cost limit may overshoot by one call. Idempotency keys include the node visit by
design, so a loop revisit is a new effect. Why: each is a deliberate reading of the
brief recorded in `docs/prd-compliance.md` with its reason; changing them now would
trade verified behaviour for unverified behaviour on the last day.

**D-032 The demo and the submission run Telegram in webhook mode through a public tunnel
that exposes only the webhook path.** (Andre) `make tunnel` starts a cloudflared quick
tunnel to a second nginx listener that serves `/telegram-webhook` and nothing else;
`PUBLIC_BASE_URL` and `TELEGRAM_MODE=webhook` go in `.env`, `openclaw-init` refuses
webhook mode without an https URL and a 32-character secret, the gateway registers the
URL with Telegram, and a wrong secret is answered 401 (smoke asserts it, and asserts the
registered URL). Polling stays supported for local use. Why: the brief says "verified
inbound webhooks"; polling verifies by token but shows no verification. Cost: a quick
tunnel's URL changes when it restarts, so `make tunnel` runs before `make run` on the
demo machine.

**D-031 The UI carries the API token.** (Andre) When `API_TOKEN` is set the UI asks for
it once (stored in the browser's local storage), sends it on every request and stream,
and signs out on request; the OpenAPI document declares the bearer scheme. Why: the
approval gate is only a gate if the API is not open to the network it listens on.
Cost: still a single shared token, no users or roles (known limitation).

**D-030 Deleting an agent deletes it in the runtime; the api reconciles at start.**
(Claude, from the audit) The gateway's heap crash came from 133 agents evals had left
behind. Deletion now removes the gateway entry and the workspace, and at api start
every stale gateway agent is deleted and every unmaterialised platform agent is pushed
(so `make seed` agents are usable at once). Why: the runtime's state must mirror the
platform's. Cost: a deleted agent's sandbox containers are removed by the gateway's
own prune, not immediately.

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
