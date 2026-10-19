# ADR 0001: OpenClaw as the execution engine, Go as the control plane

Status: accepted, 2026-10-15 (revision 2; revision 1 chose a native Go runtime and was
superseded the same day, see DECISIONS.md D-001 and D-011).

## Context

The brief exists in two versions and both are treated as binding. The portal PRD (version
1, 2026-10-12) grades runtime and orchestration, evaluation engineering, software
engineering, tools and safety, judgment, completeness and observability, and says AI
frameworks are optional. The PDF attached to it (2026-03-10) says the platform "must
integrate one of these agent runtimes as the execution engine: OpenClaw, OpenCode, Goose"
and that "the chosen runtime must actually execute the agent logic".

Both versions require: configurable agents without code changes; two or more cooperating
agents over persisted typed state; a visual workflow builder with conditions and feedback
loops; templates; schedules; memory; skills; guardrails; interaction rules; a human
approval gate; deterministic termination; retries; idempotent side effects; a Telegram,
Slack or WhatsApp channel with verified inbound webhooks; live monitoring; traces with
token and cost; an offline-capable evaluation runner; no stored chain of thought.

## Options considered

1. **OpenCode** (probed live, 1.19): documented HTTP API with per-session permission
   rulesets, `permission.asked` events, abort, per-message tokens and cost, JSON-schema
   structured output, MCP, custom providers. Strongest technical fit for a coding
   pipeline; a different product from the one the author of the brief uses.
2. **Goose** (2.0 beta): standardising on ACP; the old REST server is being removed and
   the HTTP transport is still a proposal; headless permission behaviour undocumented.
3. **OpenClaw** (probed live, 2026.10.3): gateway WebSocket RPC with 496 methods, among
   them `agents.create`, `config.patch` with hot reload for providers and models,
   `chat.send` / `chat.abort`, per-session subscriptions with transcript entries carrying
   usage and cost, exec approvals with request and resolve RPCs, native MCP client, custom
   providers for Anthropic-compatible and OpenAI-compatible endpoints, workspace skills.
   Andre runs a four-agent OpenClaw fleet already.
4. **Native Go runtime** (revision 1): full control, but does not satisfy the PDF.

## Decision

OpenClaw executes every agent turn. A Go control plane owns everything the portal PRD
grades around the runtime: the workflow engine and typed state, approvals, idempotency,
limits and cancellation, retries, traces and SSE, scheduler, the Telegram binding and conversation mirror (the channel itself is OpenClaw's native one), memory and
skills materialisation, and the evaluation runner. The control plane talks to the
gateway over loopback as a trusted backend client, serves the platform's own tools to
OpenClaw over MCP, and fronts the model API with a proxy so the key never enters the
container that runs untrusted code.

## Why OpenClaw over OpenCode

OpenCode is the better-shaped API for a coding pipeline, and this ADR records that so a
reader does not think the comparison was skipped. OpenClaw was chosen because the brief's
author named it first and described its features (markdown-defined agents, built-in
scheduler, memory, multi-channel) in the PDF's own words, because Andre operates it, and
because its gateway exposes, over one connection, the four things the control plane must
observe: the turn lifecycle, every tool call, every approval, and per-message usage. The
probe showed the cost: a client outside loopback gets no scopes without device pairing,
headless exec approvals are denied unless the client declares the approvals capability,
and the system prompt it builds is large (about 47 KB with the coding profile).

## Language

Go for the control plane (the brief suggests Kotlin or Go). Reasons: one static binary
serves every compose role (`api`, `migrate`, `seed`, `openclaw-init`, `deploytarget`,
`eval`), so the stack is one image and one toolchain; the standard library covers HTTP,
SSE, JSON and concurrency without a framework; `go test` with a real Postgres gives
integration tests that run in the same container as the build; the type system and
`staticcheck` keep a 23-table data model and the gateway client honest;
and the module system pins every dependency by hash. TypeScript with React and Vite for
the UI because React Flow is the right tool for a visual graph editor and the operator
team already uses it. Kotlin would have done the job on the JVM at the cost of a heavier
image and slower container builds in the three-day timebox.

## Consequences

- The agent loop, context assembly, built-in tools and skills prompt are OpenClaw's. We do
  not reimplement them; we configure them per agent over RPC and workspace files.
- Typed output is a platform MCP tool (`finish`) rather than a native structured-output
  feature.
- Step and cost limits are enforced from events by aborting the turn, not before each
  model call; one call may overshoot a cost limit.
- The api container shares the gateway's network namespace. Approved `exec` commands run
  in OpenClaw's Docker sandbox (read-only root, all capabilities dropped, no network unless a project turns its sandbox network on),
  which needs the Docker socket mounted into the gateway container. The threat model says so.
- OpenClaw's session store duplicates part of our trace; Postgres is the record, and node
  sessions are deleted once a run is stored.
- A pinned image (`2026.10.3`) and a probe script in `scripts/` keep the integration
  contract (`docs/DESIGN.md` 5.14) honest when the version changes.
