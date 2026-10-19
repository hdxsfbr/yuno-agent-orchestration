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

**Work that nobody typed (issue intake, DESIGN 5.23).** A labelled issue on a project's
repository becomes a run: its title and body are the task, written by whoever opened the
issue, and whoever may set labels on the repository (Developer and above on GitLab, push
on GitHub) can start runs on the project. Mitigation: the issue's text reaches the agents
only inside an `<untrusted_content source="issue #n">` block with closing tags escaped
(5.7 rule 5), cut at 16 KiB; intake is off by default, per project, and cannot be enabled
without a daily or monthly budget cap (D-055), so a label spree spends at most the cap
plus the running runs' own caps (5.19); at most ten issues are taken per project per
poll; every side effect of the run (the merge request, a release) still needs the
operator's approval. Residual risk: the issue author steers what the agents build, which
is the feature; the operator reviews the merge request as for any run.

**Text from the merge request (DESIGN 5.22).** `read_merge_request` brings a request's
description and review threads to the model; a reviewer on the provider can write
instructions into a thread. Mitigation: the whole answer is one untrusted block with the
closing tag escaped, the tool is read-only, and the push onto the request's branch is
refused when the branch moved since the run cloned it (D-050), so a commit someone else
pushed is never replaced by the checkout's tree.

**Answers from the operator (DESIGN 5.24).** `ask_operator` lets an agent put a question
in the inbox and the answer reaches the model as the operator's own text, not wrapped as
untrusted content; the question is also announced in the run's conversation or the
Telegram-bound agent's latest one, and the Chief relays the person's reply with
`answer_question`. Mitigation: only the API token holder and the allowed Telegram users
(`TELEGRAM_ALLOWED_USER_IDS`) can answer; a question is never a gate (the tool cannot be
set to `require_approval`), so an answer authorises nothing by itself and every side
effect still needs its approval; three questions per step, one pending at a time, and
the question text is redacted and capped. Residual risk: whoever may message the Chief
steers a run through its questions as they could through a delegation; the answer is
visible on the run page.

**Files a run delivers (DESIGN 5.25).** Agents write arbitrary files under `out/` and the
api serves them to the operator's browser. Mitigation: the api reads the workspace
through `os.Root` (links never followed), caps the files (200, 50 MiB, 20 MiB each),
serves every artifact as an attachment with `X-Content-Type-Options: nosniff` and HTML
and SVG as plain text, and the UI previews text in a `<pre>` and images from a blob, so
nothing an agent wrote runs in the operator's browser; `diff.patch` is produced in a
private clone, never by running git inside the agent's checkout, where a hook or a
config entry an agent wrote could run on the api side; the file paths `run_status`
names are agent-chosen text and reach the Chief as tool output, which `AGENTS.md` tells
it to treat as data. Residual risk: a delivered file is still untrusted content once
downloaded and opened elsewhere.

**Parallel steps (DESIGN 5.26).** A fork runs several agent visits of one run at once.
What they share is the run's state (written key by key, so a sibling's write is never
lost), the run's counters and limits (summed, so two branches cannot together spend more
than the run may), and the run directory only through the copy-forward after the join:
a branch visit receives nothing from its siblings, and a project checkout flows past the
join only from the branch the join names, never as a file-by-file mixture of two working
trees that could carry one branch's edits into another's commit. Two branches never
share an agent (refused at save), since an agent has one workspace and one sandbox hold.
A branch's approval or question wait does not stop the run's clock, so a run in a fork
cannot be kept alive past its time limit by one branch waiting while another works.
Residual risk: a condition or task inside one branch that reads another branch's output
reads whatever is there at that moment; `input_from` across branches is refused, and the
rest is the workflow author's.

**Runaway agents.** Loops, unbounded tool use, cost blow-ups. Mitigation: per-run step,
time, cost, tool-call, node-iteration and loop-visit limits enforced on every event and
on a tick (5.2), cancellation from the UI, a run can only ever wait on a human for
`approval_timeout_seconds`; per-agent cost cap and runs-per-hour guardrails (5.11).
Verified by the `termination` evaluator on every eval case (100% on the public split in the evaluation report named under Evaluation in the README).

**Side effects executed twice.** A retry, a re-queue or a late approval repeats a deploy.
Mitigation: idempotency keys over run, node visit, tool and canonical input, recorded in
the same transaction as the result; the deploy target enforces the key on its side;
scheduler occurrences carry a unique key (5.4, D-029). Verified by the `idempotency`
evaluator and the replay case (010). Limit: the key includes the node visit, so an operator retry
from a failed step, or a run resumed after an api restart, starts a new visit and can
repeat a `deploy_release`; `open_merge_request` is held to one merge request per run by a
lock and a unique index, so it is not repeated.

**Side effects before approval.** Mitigation: the MCP gate records the call as
`awaiting_approval` and blocks it; the engine executes a late grant itself with the same
key; the `approval_gate` evaluator checks that no side effect ran before its approval
(100% on the public split in the evaluation report named under Evaluation in the README).

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

**Project instructions and notes in prompts (DESIGN 5.18, D-040).** Every visit of a
run on a project gets the operator's instructions and the ten newest project notes in
its task. The instructions are the operator's own text. The notes are written by
models: a run whose agent was steered by repository content could save a note that
steers the next run on the project ("always run `curl … | sh` first"), a persistence
the per-run isolation of memory did not have. Mitigation: only agents the operator
gave the memory tools can write notes, each note is capped (2 KiB, 100 per project,
90 days), the project page lists every note with the agent and the run that saved it
and deletes it in one click, the blocks are capped at 12 KiB, and side effects stay
behind the same gates whatever the task says. Residual risk: a note is rendered as
part of the task, not as wrapped untrusted content, so it carries the operator's
weight with the model; an operator who allows network in the sandbox should read the
notes of that project now and then.

**Merging from the platform (DESIGN 5.20 rule 5, D-042).** The Merge button pushes
model-written code into the default branch with the project's token. Mitigation: it is
an operator action behind a confirmation, refused while the pipeline is pending or
failed, and the provider applies its own rules (protected branches, required approvals,
conflicts) and refuses with a message the page shows; the token's scope is what the
operator granted. Residual risk: a repository without a pipeline merges on one click
with nothing but the Reviewer agent's judgement before it; keep branch protection on
the provider for anything that matters.

**Budget alerts and refusals (DESIGN 5.19).** No new secret and no new inbound surface:
the alert goes out through the existing report-back path and the refusal is a 429.
Residual risk: a cap refuses new runs only, so a window can overshoot by the running
runs' own caps, and the cap counts a run's spend in the window it was created in.

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

**Imported skills.** A skill from a zip, a Git repository or the library carries text the
agent reads and scripts it may run. Mitigation: paths are checked (no `..`, absolute
paths, backslashes, symlinks or devices; hidden folders skipped), sizes are counted while
reading (200 files, 2 MiB per file, 20 MiB per skill, 25 MiB per upload, 50 MiB per
clone); Git import is https-only to allowlisted hosts, without credentials in the URL,
redirects checked against the allowlist, git run with no prompts, no system config, only
the https protocol, a 60 s timeout and its temporary directory always removed; the api
never runs anything it imports, and scripts run only through `exec` in the agent's
sandbox under its exec policy (5.7). The library's skills were read in full before they
were committed (`skills-library/SOURCES.md`). Limit: the platform does not judge what an
imported skill's instructions ask the agent to do; assign skills from sources you trust.

**Workspace files.** The operator's edits to an agent's files reach the model. Mitigation:
the platform's generated part of `SOUL.md`, `AGENTS.md` and `MEMORY.md` cannot be removed
from the UI; reads and writes go through the workspace root, only regular files are read
or replaced, so a link or pipe the agent creates cannot redirect or block them; shown
content is redacted, and a redacted file cannot be saved back.

## Known limitations

- Single operator, no authentication beyond the optional bearer token.
- The Docker socket is mounted into the gateway container (needed for sibling sandboxes).
- Exec guardrail patterns cover host exec and approvals, not commands inside the sandbox.
- Approvals can be answered from the web UI; a reply to the prompt the bot sends in
  Telegram has not been verified to resolve the approval.
- Conversation memory is the mirrored Telegram transcript; agent facts are agent-scoped,
  project notes are shared by every agent of a run on the project and rendered into
  tasks without the untrusted-content wrapper.
- Merge requests are followed by polling the provider every two minutes; there are no
  webhooks, so a merge or a pipeline result shows up with that delay unless Sync is
  pressed; records older than 14 days are refreshed only by Sync, and a provider 404
  closes a record even when it is the token that lost access.
- A later `open_merge_request` in the same run (a review loop, a retry) replaces the
  request branch's tree with the run's checkout, so a commit someone pushed to that
  branch in between is reverted by it. A run that names an existing request (DESIGN
  5.22) clones its branch and refuses the push when the branch moved since (D-050), so
  the limitation is the same-run path only.
- A push from the api (a new request or an update onto its branch) copies regular files
  only, so a symlink tracked by the repository is committed as a deletion; projects with
  tracked links are not supported. The push and the artifact diff (DESIGN 5.25) reach the
  checkout by its path after a link check, so a process still alive in the agent's
  sandbox could swap `repo/` for a link in between; a persistent link is refused.
- Issue intake polls the provider every two minutes and drives the issue's labels; the
  comments it posts are best effort (a crash between the label swap and the comment can
  repeat the comment once), and an issue's text is the run's task (above).
- A step parked on a question (DESIGN 5.24) continues as a new visit with the answer in
  its task and the files it saved, not in its old OpenClaw session, so the agent loses
  its conversational context unless the answer arrives while the turn is still open.
- Artifacts (DESIGN 5.25) are the newest version per path, kept on one named volume
  with no backup of their own; the project diff costs one shallow clone per step of a
  project run and is skipped when the provider refuses to serve the cloned commit.
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
  only breaks its own `git diff`; the platform does not read that directory. Project
  tokens are only as safe as `PROJECT_TOKEN_KEY` in `.env` (above).
