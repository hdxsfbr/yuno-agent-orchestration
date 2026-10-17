# Threat model and known limitations

The platform runs untrusted model output against real tools. This document lists what
can go wrong, what the design does about it, and what it deliberately does not cover.
Rules referenced as 5.x are in `docs/DESIGN.md`; decisions as D-nnn in `DECISIONS.md`.

## Assets

- The operator's credentials: the model provider key, the Telegram bot token, the
  platform tokens (`API_TOKEN`, `YUNO_MCP_TOKEN`, `OPENCLAW_GATEWAY_TOKEN`,
  `PROJECT_TOKEN_KEY`), the optional GitLab token, and the git token of every project
  (stored in the database AES-GCM encrypted under `PROJECT_TOKEN_KEY`; `PROJECT_GIT_TOKEN`
  only bootstraps the first project).
- The host: Docker socket, the state directory with every agent's workspace, Postgres.
- The side effects: releases on the deploy target, merge requests on GitLab, branches
  and merge or pull requests on the project repository, outbound Telegram messages.
- The record: runs, traces, approvals, conversations, which must stay truthful.

## Trust boundaries

| Boundary | Trusted side | Untrusted side | Enforced by |
|---|---|---|---|
| Model output | the engine | everything the model says or calls | tool schemas, allowlists, interaction rules, approval gate, limits |
| Tool output | the engine | files, command output, fetched pages | untrusted-content wrapper (5.7 rule 5), `AGENTS.md` rules, no instruction-following in tool results |
| Sandbox | gateway container | agent commands | per-agent Docker sandbox: no network (unless a project opts in, 5.16 rule 8), read-only root, dropped capabilities, pids and memory limits, only the agent's workspace mounted (5.14 rule 12) |
| Project checkout | api | the repository's content and the agent's edits to it | the api never runs git inside the checkout; the token stays in the api's git environment; the checkout holds the bare URL (5.16 rules 4, 10) |
| Channel | gateway | Telegram users | `dmPolicy: allowlist` of numeric user ids; closed until an agent is bound (D-025, D-026); webhook secret header in webhook mode, bot-token-authenticated polling otherwise |
| Browser | api | the operator's browser | optional `API_TOKEN` bearer; no accounts (single operator) |
| MCP | api | the gateway's MCP client | `YUNO_MCP_TOKEN` bearer, loopback only |
| Model proxy | api | the gateway | loopback only; the provider key never reaches the gateway (5.14 rule 8) |

## Threats and mitigations

**Prompt injection through tool results.** A fetched page, a file in the workspace or a
command's stdout tells the agent to deploy, exfiltrate or change its task.
Mitigation: `http_fetch` output is wrapped as untrusted content with any closing tag
escaped; `AGENTS.md` tells every managed agent that tool output and such blocks are data;
side effects need a human approval regardless of what the model believes (5.3); the
eval suite has a prompt-injection case for command output (012) and for fetched content
(021, private). Residual risk: an operator who sets a side-effecting tool to `auto`
(D-009) removes the human from that path; the UI warns.

**Runaway agents.** Loops, unbounded tool use, cost blow-ups. Mitigation: per-run step,
time, cost, tool-call, node-iteration and loop-visit limits enforced on every event and
on a tick (5.2), cancellation from the UI, a run can only ever wait on a human for
`approval_timeout_seconds`; per-agent cost cap and runs-per-hour guardrails (5.11).
Verified by the `termination` evaluator on every eval case (100% on the public split).

**Side effects executed twice.** A retry, a re-queue or a late approval repeats a deploy.
Mitigation: idempotency keys over run, node visit, tool and canonical input, recorded in
the same transaction as the result; the deploy target enforces the key on its side;
scheduler occurrences carry a unique key (5.4, D-029). Verified by the `idempotency`
evaluator and the replay case (010).

**Side effects before approval.** Mitigation: the MCP gate records the call as
`awaiting_approval` and blocks it; the engine executes a late grant itself with the same
key; the `approval_gate` evaluator checks that no side effect ran before its approval
(100% on the public split).

**Sandbox escape / host access.** An agent's command tries to reach the host or the
network. Mitigation: OpenClaw's Docker sandbox with `network: none`, read-only root,
`cap_drop ALL`, non-root user, only the agent's workspace mounted; the gateway container
is the only one with the Docker socket. Residual risk (known limitation): mounting the
Docker socket into the gateway container is root-equivalent on the host; the gateway is
trusted code from a pinned image, but a gateway compromise is a host compromise.
Exec guardrail patterns do not reach the sandbox (D-028).

**Project repository with sandbox network on (DESIGN 5.16, D-037).** With a project's
network opt-in (the page's switch, or `PROJECT_SANDBOX_NETWORK=true` for the bootstrapped
one) the sandboxes of its runs get a bridge network so agents can install the project's
dependencies. That is a deliberate widening of the
sandbox boundary: anything injected into the repository (a README, a test, a
dependency's install script, a package the lockfile names) runs in a container that
can reach the internet, so it can exfiltrate whatever the sandbox holds (the checkout,
the agent's workspace, the task text) and can call any service, and side effects
inside the sandbox are not gated by the approval gate (D-028 applies). The container
can also reach the host's published ports, which is why `API_TOKEN` matters on any host
with the opt-in. Mitigation: off by default; the opt-in is per project, for a
repository the operator trusts; the token never enters the sandbox (the checkout's
`.git/config` has the bare URL, the api's git gets the token through its environment
only); the api never executes anything from the checkout (no hooks, no config: the
merge request is built from a fresh private clone that receives the working tree as
files, 5.16 rule 10); the api has no Docker socket; the merge request itself stays an
approval-gated side effect; the redactor knows GitLab and GitHub token shapes and basic
credentials. Residual risk: with network on, the sandbox is a general-purpose network
client in the hands of whatever the model reads; use it only with repositories whose
contents you would run on your own machine. With network off the clone still happens
and the only widening is the checkout's content in the agent's workspace, which the
untrusted-content rules already cover.

**Project tokens at rest (DESIGN 5.17 rule 2, D-038).** A project's git token is the
credential that pushes branches and opens merge requests. Mitigation: it is written
through the API only, never returned (`token_set` is the only trace of it in any
response), redacted wherever a token shape could appear, and stored AES-256-GCM
encrypted under SHA-256 of `PROJECT_TOKEN_KEY` with the project id as additional data,
so a database dump without the `.env` yields no token and a blob cannot be moved to
another project's row; the api reads a token only when a run clones or pushes and when
the operator presses Check. Residual risk: the key lives in `.env` next to the other
platform secrets, so whoever reads the host's `.env` and the database has every token;
a lost key is a re-entry of every token, not a breach.

**Secrets in logs, traces, fixtures or the repository.** Mitigation: the log handler
redacts keys, tokens and anything shaped like a provider key or a Telegram token (5.12
rule 4); tool inputs and node transcripts are redacted before storage (provider keys, bot tokens
and run tokens); the gateway
config endpoint redacts the bot token; `.env` is git-ignored and `.env.example` carries
names and defaults only; the coordinator's pre-commit scan catches token-shaped strings
(it caught one test fixture in wave 3). Residual risk: a model may echo a secret it was
given in a task; tasks should not contain secrets.

**Chain-of-thought exposure.** Mitigation: reasoning streams from the gateway are never
stored or shown; the proxy requests no reasoning display (5.14 rule 11); a trace test
asserts no reasoning text is persisted.

**Unauthorised channel use.** A stranger messages the bot. Mitigation: `allowFrom` is the
numeric allowlist; the channel is closed (`dmPolicy: disabled`) while no agent is bound;
exec approvals from chat go to approvers only (the same ids). Residual risk: the
allowlist is static configuration, not a UI setting.

**Unauthorised API use.** Mitigation: a bearer token (`API_TOKEN`) on every API request
and stream, entered once in the UI and stored in the browser; the public tunnel exposes
only the webhook path; Postgres listens on loopback. Known limitation: one shared
token, no users, roles or audit of who approved beyond `decided_by: api`.

**Supply chain.** Pinned OpenClaw image (`2026.10.3`), Go module sums, pnpm lockfile,
derived gateway image adds only the static Docker CLI.

## Known limitations

- Single operator, no authentication beyond the optional bearer token.
- The Docker socket is mounted into the gateway container (needed for sibling sandboxes).
- Exec guardrail patterns cover host exec and approvals, not commands inside the sandbox.
- Approvals can be answered from the web UI; a reply to the prompt the bot sends in
  Telegram has not been verified to resolve the approval.
- Conversation memory is the mirrored Telegram transcript; agent facts are agent-scoped.
- The scheduler is a single process; two api instances would both be safe (row locks)
  but only one is run.
- No OpenTelemetry export; spans live in Postgres and the UI.
- The eval suite's private cases need the network or a human and are not part of
  `make eval`'s default public split.
- Chat tokens (the identity of an agent's chat-time tool calls, DESIGN 5.15) are
  long-lived, stored in plain text in the database and written into the agent's
  `AGENTS.md`; they are redacted everywhere like run tokens, but a database dump or a
  workspace read yields them. Rotating one means re-materialising the agent.
- `require_approval` on a chat-capable tool disables it in conversations rather than
  gating it, since a conversation has no run to pause; approvals stay in the UI.
- Projects are cloned at depth 1 from their default branch; the merge request flattens
  the agent's changes into one commit by Yuno. A project run with sandbox network on is
  a trusted-repository mode (above). An agent that edits its checkout's `.git` directory
  only breaks its own `git diff`; the platform does not read that directory. Schedules
  do not name a project, so a scheduled run of a workflow that opts in fails its
  validation when it fires. Project tokens are only as safe as `PROJECT_TOKEN_KEY` in
  `.env` (above).
