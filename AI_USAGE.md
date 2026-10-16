# AI usage log

How AI tools were used to build this project, what they produced, and what a human
decided or verified. Kept in chronological order; updated at every wave.

## Tools

- Claude Code (Claude Fable 5.1) in the Claude desktop app, driven by Andre Batista.
  Used for: reading the brief, drafting the design, scaffolding, implementing waves with
  parallel subagents, writing tests, reviewing its own output with an independent review
  pass, and drafting documentation.
- Claude Opus 5.5 / Sonnet 5.5 via the Anthropic API: the models the platform's agents
  run on at runtime. Not a build tool, listed for completeness.

## 2026-10-15

**Brief intake.** Claude read the Gauntlet portal page and the attached PDF through the
browser and found that the two disagree (runtime requirement, grading weights). Andre
decided to build to the portal PRD and record the conflict (DECISIONS.md D-001).

**Decisions.** Claude proposed options with a recommendation for runtime, language,
channel and demo workflow; Andre chose native Go runtime, Go, Telegram, and the dev
pipeline (Claude had recommended a payments-triage demo; Andre overrode). Andre added
two process constraints: everything runs in Docker, and build tracer-bullet style.

**Design.** Claude wrote `docs/DESIGN.md` from the brief using a design-first template
(rules, units, edge cases, failure handling, tests per subsystem). Four product
decisions were put to Andre and answered (D-007 to D-010). The rest of the rules follow
from the brief.

**Scaffold.** Claude wrote the compose file, Makefile, Dockerfiles, Go skeleton
(config, logging with redaction, migrations, SSE hub, model and tool types, sandbox and
deploy-target services with tests) and the React skeleton. Verified in containers:
`go vet`, `go test` (one real bug found by a test: `http.ServeFile` redirecting
`index.html`, fixed), `pnpm test`, `pnpm lint`, `pnpm build`, and `make run`.

**Reassessment.** Andre asked for the design to be reassessed with both brief versions
binding. Claude researched OpenCode, Goose and OpenClaw against current docs, ran OpenCode
1.19 and OpenClaw 2026.10.3 in throwaway containers, captured OpenCode's OpenAPI document
and OpenClaw's method and event catalogue, and drove a full OpenClaw turn (provider patch,
`chat.send`, exec approval round trip, history and usage) against a mock model server.
Claude recommended OpenCode on API fit; Andre chose OpenClaw. Claude rewrote
`docs/DESIGN.md` (revision 2), `docs/adr/0001-runtime.md` and DECISIONS.md D-011.

**Scaffold rework for OpenClaw.** Claude replaced the Go sandbox service with the
OpenClaw gateway service (Docker socket, host bind mounts, derived image with the
docker CLI), wrote `yuno openclaw-init` (baseline config writer that preserves foreign
keys, with tests), the shared store layer, the workflow definition type, the gateway
client contract and the wave-1 OpenAPI document. Verified in containers: `go test`,
`make run`, `openclaw config validate` inside the gateway, `make smoke` (11 checks) and
`make probe-openclaw`, which ran a real turn whose `exec` executed in a sibling sandbox
container with per-message usage and cost. Three real problems were found and fixed by
those checks: the schema rejects platform keys under `meta`, explicit ownership needs a
seeded agent, and the upstream image lacks the docker CLI the gateway needs to inspect
itself.

**Wave 1, tracer bullet.** Andre approved four written briefs. Four subagents built in
parallel on disjoint files, each briefed with the design sections, the verified probe
script and the OpenAPI contract as ground truth, and asked to report every rule they had
to interpret: A (gateway client, materialiser, engine; Claude Opus 5.5), B (HTTP API, MCP
server, model proxy, scripted provider; Opus 5.5), C (frontend; Claude Sonnet 5.5), D
(eval runner and cases; Sonnet 5.5). Their interpretations were folded back into
`docs/DESIGN.md` (agent id derivation, transition table, limit layering, step-limit
tolerance, loopback-only proxy). The coordinator re-ran every agent's verification
rather than trusting the reports, then integrated: aligned agent model validation with
the models OpenClaw exposes, ran `go mod tidy`, and found and fixed three integration
problems the unit tests could not see (OpenClaw unsets `agents.ownership` when the roster
shrinks; it keeps the platform MCP server marked unavailable after an api restart; its
Tool Search hides MCP tools so a direct `yuno__finish` call fails). With those fixed,
`make eval` passed all three cases through the real gateway with sandboxed `exec` and
produced `evals/reports/baseline.md`.

**Wave 1 review.** One independent review by a second model (codex-cli 0.162.1, high
reasoning, read-only; its sandbox cannot start on this host, so it ran unsandboxed with
a read-only instruction). 25 findings; the coordinator dropped one with a stated reason
and sent 24 to a single fix-pass agent (Opus 5.5). The list and dispositions are in
`docs/reviews/wave1-review.md`.

**Wave 1 fix pass.** One Opus 5.5 agent fixed the 24 surviving findings, each with a
test, and checked its own coverage by reverting every fix in a scratch copy and
confirming the covering test failed (26 Go reverts and 2 UI reverts caught). It found that
the two live integration test packages cannot run in parallel against one gateway
(concurrent config patches are superseded), so `make test-integration` runs them
serially. The coordinator then re-ran the full suite, the integration tests, smoke, the
probe and the eval suite before committing.

Slips disclosed by agents: C ran an `npx` command on the host once (cleaned up); D ran
`gofmt` as root inside a container and fixed the resulting file ownership.

Human verification so far: Andre reviewed the design decisions, chose the runtime, the
sandboxing mode and the channel integration, and approved the wave-1 briefs. The UI has
not yet been exercised in a browser by a human.

**Wave 2 prep (2026-10-15, coordinator).** Before briefing the four wave-2 agents the
coordinator wrote the shared seams so no two agents would touch the same one: the
approval methods on the engine contract and the `SideEffectExecutor` fallback, the
approval and side-effect store types, `agent_name` on template nodes, the two shipped
templates, the `seed` command for the six demo agents, and a second gateway probe
(`make probe-approval`) for the plugin-approval RPCs. The probe found that
`plugin.approval.request` blocks until the decision unless `twoPhase` is set and that the
gateway's own timeout arrives as a `deny` with `resolvedBy: null`; both went into the
design (5.3 rule 5a) and the decision log (D-017) before any agent started. The
revision 1 rules for idempotency, retries and the workflow graph, which revision 2 had
referenced without carrying, were folded back into `docs/DESIGN.md` with the wave-2
additions, so every brief points at text that exists in the repository. Andre's first
hands-on UI session (three findings: wasted exec turns, two misleading labels, a Runs
list without agent, model or a clickable row) was fixed and committed first.

**Wave 2 launch.** Four agents in parallel, briefs in `docs/briefs/wave2/` (approved
by Andre as summarised in chat): A engine and approvals (Opus 5.5), B tools, MCP gate
and API (Opus 5.5), C builder and inbox (Sonnet 5.5), D evaluators and cases
(Sonnet 5.5). File ownership and the one cross-cutting rule (the MCP handler records
platform tool calls; the engine skips them in its item recorder) are in
`docs/briefs/wave2/common.md`.

**Wave 2 agents (2026-10-15).** All four finished within 45 minutes of wall clock: A
(engine, Opus 5.5, 107 tool uses), B (tools and API, Opus 5.5, 122), C (frontend,
Sonnet 5.5, 96), D (evaluators, Sonnet 5.5, 256). Each reported the rules it had to
interpret, which the coordinator turned into decisions D-020 to D-024 and design text.
Two findings came from the agents rather than the reviewer: D found that every agent's
OpenClaw workspace is separate, so a Reviewer could not read the Coder's files (now
DESIGN 5.6 rule 13, D-019); B found the memory rules were among the revision 1 text the
design had dropped, which the coordinator restored for sections 5.9 to 5.13. B also
asked A for a shared default rejection text mid-wave, relayed by the coordinator.

**Wave 2 verification by the coordinator.** Full Go suite (12 packages), web suite (196
tests), lint, the serial live integration tests, both gateway probes, a rebuilt stack,
smoke, then `make eval`: the first eval ran a stale image (`make eval` now rebuilds),
the second passed 11 of 12 public cases with end-to-end success 12 of 12; the one
failure was a case expectation (a tool OpenClaw refuses itself records no model call),
corrected by the coordinator.

**Wave 2 review.** One independent review (codex-cli 0.162.1, high reasoning,
unsandboxed read-only) of the uncommitted tree: eight findings, four P1 (a symlink
escape in deploy packaging, the run-directory hand-off, two approval-recovery races),
four P2; all kept, none contradicted a decision. The list and dispositions are in
`docs/reviews/wave2-review.md`; one Opus 5.5 fix-pass agent took them plus the memory
defaults from B's report, briefed in `docs/briefs/wave2/fix-pass.md`.

**Wave 2 fix pass.** The fix-pass agent applied all seven fixes with a test each,
mutation-checked, and left one disagreement for the coordinator: the live run of the
blocked-tool case recorded three model calls where the offline simulation of the engine
rules counted four. The live run is the truth (OpenClaw refuses a denied tool itself and
records no assistant message for the attempt), so the coordinator aligned the simulation
and kept the corrected case. The agent also added `scripts/mutation-check.sh`, under the
rule that a command run twice becomes a script, and `make test-race` was added for the
race run it did by hand.

**Wave 3 prep (2026-10-16, coordinator).** Andre supplied the Telegram bot token and his
user id through his local config; the coordinator wired them into the git-ignored `.env`,
restarted the stack and probed the channel before briefing. The first probe caught the
gateway answering Andre's messages from its own default agent on Opus (D-025) and
produced a wrong intermediate conclusion (that bindings need a restart) that agent A
later corrected from the gateway's reload table; the design records the corrected fact.
Briefs in `docs/briefs/wave3/`.

**Wave 3 agents.** A Telegram and conversations (Opus 5.5, 214 tool uses, 38 min), B
scheduler, guardrails, memory and skills (Opus 5.5, 170, 28 min), C pages (Sonnet 5.5,
60, 13 min), D rubric evaluator and cases to 24 (Sonnet 5.5, 193, 32 min). B's wiring
requests to A were relayed mid-wave and landed in A's report; A's store changes for
conversation approvals arrived as diffs the coordinator applied with their test; the
coordinator also wired the rate-limit 429 and the agent-save validation B could not own.

**Wave 3 verification, review and fix pass.** The coordinator ran the suites, lint, the
race run, the serial live integration tests, both probes, a full `make run` on the new
closed-by-default Telegram baseline, smoke, the binding through the platform's own API
(the keeper had removed the probe's hand-made binding first), and the eval: 16 of 17
public cases, the miss being the runner's own 429 decoding. One independent review
(codex-cli, high reasoning, read-only) found six issues, two P1 (a scheduler occurrence
that could fire twice across a crash, a host-exec allowlist that bypassed guardrail
patterns); all six, the agents' leftovers and the runner bug went to one Opus 5.5 fix
pass, dispositions in `docs/reviews/wave3-review.md`. The fix pass's one interpretation
became D-029.

**Secret hygiene (2026-10-16).** The coordinator's pre-commit scan of the wave-3 diff
caught a token-shaped test fixture that an agent had modelled on the real bot: the bot's
public numeric id with an invented secret half (hash and prefix comparison showed no
overlap with the real secret beyond the two characters every Telegram token starts
with). The fixture was replaced with a plainly fake value before the commit was final,
and the agent rules now say fixtures never borrow any part of a real credential.

**Wave 4: audit (2026-10-16).** Per the operator's requirements-audit procedure, both
spec texts were copied into `docs/spec/` and a fresh Opus 5.5 agent read them line by
line against the code with the project's own documents excluded as evidence, while the
coordinator checked deliverables, environment coverage, named tests and timing. The
agent returned 34 findings (three blockers: webhook verification never exercised, an
API that ran open because the UI sent no token, missing deliverables); the coordinator
reconciled them with its own list, classified each as fix / disclose / accept, and
Andre approved the recommended set. During the audit the full-split eval exposed a
real incident: the gateway's Node process ran out of heap after 133 eval agents had
accumulated, and the api, which shares its network namespace, lost its database
address. The fix (runtime deletion and reconciliation, heap and memory limits, restart
policies, an api watchdog, nginx re-resolution) was measured: after an in-place gateway
restart the stack recovers unaided in about two minutes.

**Wave 4: fixes and documents.** Two agents in parallel (backend Opus 5.5, 231 tool
uses, 40 min; frontend Sonnet 5.5, 112, 16 min), both mutation-checked; the backend
agent also replaced three test fixtures that carried Andre's real Telegram id. The
coordinator wrote the threat model, the README's final sections, the compliance
document, the test-results record and the decisions D-030 to D-034, switched the live
stack to webhook mode through a tunnel limited to the webhook path, and generated the
demo narration with a local text-to-speech model in a container rather than a paid
service. A Sonnet 5.5 agent built the Playwright capture pipeline from the operator's
demo-recording procedure (one video per scene, waits cut in post, title cards,
ffmpeg stitch); the live scenes were recorded by the coordinator with Andre on the
Telegram side.

**Demo video (2026-10-16).** Recorded by the coordinator from the running build at
commit `0c3afdd` with the Playwright pipeline, one scene at a time: the title card, the
Coder's configuration, the dev pipeline built from its template and run on the real
model with both approvals granted from the inbox, a step-limit failure and a rejected
deploy, the evaluation report, a Telegram conversation with Andre on the phone, a
once-schedule firing the research pipeline, and the closing card. Only the model and
gateway waits were cut (the cut intervals are recorded), footage runs at natural speed,
and the narration was generated from `docs/demo-script.md` with a local
text-to-speech model. The coordinator verified the result from a frame sample every
fifteen seconds and the per-scene timing table; Andre watched the finished file.

**Architecture diagrams (2026-10-16).** After the submission commit, Andre asked for
oh-my-mermaid so he could explore the architecture visually. The coordinator set it up
without installing Node on the host (a pinned image, a compose service, a wrapper script,
two project skills adapted to the container) and wrote the eight perspectives under
`.omm/` from its knowledge of the code: every node carries a description and the files
it covers, every edge a label, and the diagrams pass the tool's validator. Nothing in the
product changed; the viewer's default port moved to 3030 because 3000 was in use on the
machine.

**Public deployment path (2026-10-16).** Andre chose DigitalOcean from the approved vendor
list after the coordinator showed why the stack cannot run on Railway or any managed
container platform (Docker socket, shared network namespace, a 4 GB gateway). The
coordinator wrote the `public` compose profile with Caddy, the loopback binding, the
droplet creation, bootstrap, deploy and runner scripts, the GitLab pipeline with its
manual-or-automatic deploy job, and `docs/deploy.md`; the compose file, the Caddyfile, the
shell scripts and the pipeline were validated with their own tools in containers. The
droplet was then created from the same scripts with Andre's go-ahead on the size and
cost (sfo2, because sfo3 did not offer the size; Andre asked whether 8 GB was needed and
kept it after seeing the measured 1.8 GB idle footprint), bootstrapped and deployed by
the coordinator, with three script fixes found on the way (apt lock on a fresh droplet,
missing `make`, a remote script losing its tail to a child reading stdin) committed as
they were found. Verified from outside: readiness, a 401 without the token, a 401 on the
webhook without the secret, a Let's Encrypt certificate, plain http unreachable, the
webhook registered; smoke 12 of 12 on the host.

**CI wired to the droplet (2026-10-16).** On Andre's go-ahead the coordinator created a
project runner through the GitLab API, installed it on the droplet as the deploy user with
the token passed over stdin, set the two project variables, triggered a pipeline and played
its manual deploy job, which redeployed the public instance and passed smoke. Two mistakes
on the way, both recorded: a host check printed the runner token into the coordinator's
own output through colour codes that defeated the mask, so the token was rotated through
the API and the runner re-registered before anything else; and the first job died on
Ubuntu's default `.bash_logout`, fixed in the installer. The test job still has no runner.
