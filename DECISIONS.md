# Assumptions and decisions log

Newest first. Each entry says what was decided, by whom, and why. Design rules the entries
refer to are in `docs/DESIGN.md`. Architecture-level choices also have an ADR under
`docs/adr/`.

## 2026-10-15

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
