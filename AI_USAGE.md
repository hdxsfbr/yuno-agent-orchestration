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

**CI test runner (2026-10-16).** Andre chose a second 8 GB droplet for the test job. The
coordinator created it and its runner with the same scripts, hardened the job (loopback web
bind, package caches kept, a full clean after each job) and set the project variable that
adds the test job. The first two pipelines failed on real problems the development machine
had hidden: a config test that did not know compose reads `COMPOSE_PROFILES` itself, and
web tests that assumed installed dependencies. Both fixed at the source; the third
pipeline went green and its deploy job, played by hand, put that commit on the public
instance. docs/test-results.md records the CI run.

**Automatic deploys (2026-10-16).** Andre chose automatic deploys of green `main`
pipelines; the coordinator set `DEPLOY_AUTO` and proved it with pipeline 57577, whose
deploy job started on its own after the test job and put that commit on the public
instance. Every push to `main` from here on tests and deploys itself.

**Wave 5: the Chief of Staff (2026-10-16).** Andre asked for one agent to talk to that
knows the others and delegates. The coordinator wrote the design (fifteen rules, five
product decisions Andre approved; D-036) after reading the gateway's code for an identity
on MCP calls (none) and for a way to deliver a reply to Telegram (`chat.send` with
`deliver`), probed the latter live with `make probe-deliver`, then briefed two agents:
backend on Opus 5.5 (chat-token gate, four tools, report-back, seed, migration; 163 tool
uses, 30 min) and frontend on Sonnet 5.5 (badges, notices, tool-call list; 54 tool uses,
8 min). The usual reviewer (codex) was out of quota, so two fresh Claude agents on a model
different from each author reviewed instead; the coordinator filtered their 20 findings
(docs/reviews/wave5-review.md), one fix pass per part followed, and the backend agent
correctly declined one fix whose premise was wrong. The coordinator ran the suite, lint
and the image builds itself before the single wave commit; CI tested and deployed it.
Verified live by Andre from his phone the same night (roster, a delegation to Coder, the
report relayed in a second, a follow-up through `run_status`); the one defect that showed,
a mirrored reply lost after parallel tool calls, was fixed with a test the next hour.

**Projects: a repository, its toolchain, a merge request (2026-10-17).** Andre decided
that projects are git repositories configured by environment variables, that toolchains
are read from the repository and served by prebuilt generic images, that sandbox
network is a per-project opt-in, and that no agent ever clones or pushes (D-037). A
coordinator session wrote the design first (DESIGN 5.16: sixteen rules, edge cases,
failure handling, a test list), then probed the gateway for the facts the design
depends on with a new `scripts/openclaw-probe-sandbox.mjs` (`make probe-sandbox`): a
per-agent `sandbox` block set through `config.patch` reloads hot, picks the image and
network of the agent's next session while the defaults stay untouched, and a `null` in
`replacePaths` restores them; the same probe found that the gateway sets `HOME` and its
own `PATH` for every exec, which moved the Go toolchain onto `/usr/local/bin` in its
image. The implementation followed the design: `internal/project` (configuration,
detection table, git with the token in the environment only), the engine's clone step
and per-visit sandbox holds, `open_merge_request` with GitLab and GitHub providers
behind one interface, the seed's reset switch for installs that predate the feature,
the toolchain badge and request link in the UI, and the docs. One security choice came
from writing the tool: the api never runs git inside the agent-writable checkout (a
planted hook or config entry would run on the api side), so the merge request is built
from a fresh private clone that receives the working tree as files; a test plants such
a hook and checks it never ran. Tests run against local bare repositories with the real
git (no network), httptest fakes for both providers, and the fake gateway; `make
test-go`, `make test-web` and `make lint` were green before the merge.

**Wave 6 verification (2026-10-17).** The coordinator created the playground project and
its scoped access token through the GitLab API at Andre's request, wired the public
instance to it, and ran one Dev pipeline on it; Andre approved the gate and, after the
tool approval had timed out and the run had ended, the merge-request card, which the
engine executed as a late grant. The first real merge request opened by the platform is
`yuno-playground!1`.

**Projects as records (2026-10-17).** Andre asked for projects created and configured in
the UI and for the Chief of Staff to delegate "on the playground project" by name. A
coordinator session started by the wave-6 session wrote DESIGN 5.17 (fourteen rules,
edge cases, failure handling, a test list) and D-038 first, then the contract in
`api/openapi.yaml`, the store layer, the migration and the `project` package's cipher,
validation helpers, resolver and bootstrap as the shared seams, and briefed two agents
from `docs/briefs/wave7/` with disjoint file scopes: the API (Opus 5.5: the projects
CRUD with the write-only encrypted token, the provider check against the existing
httptest fakes, the bootstrap at start, `run_status` on approval items) and the UI
(Sonnet 5.5: the Projects tab and form, the selectors in both run dialogs, the project
link on runs, the Status list, the ended-run note on approval cards, with a mutation
check of its own tests). The coordinator did the engine (the resolver in place of the
environment project, validation of `project_id` at trigger time, the follow-up report for
a merge request opened by a late grant), the Chief's `list_projects` and the project on
`delegate_task` and `run_status`, the seed, and their tests, and ran the full suite, lint
and the review itself. Two nits from the first live run were folded in: the merge
request title is capped at 72 characters and described as a commit subject, and a tool
approval on a run that has already ended says that approving still executes the call.
The design was not approved by a human before coding: the session ran autonomously on
the brief, so the design text is what a reviewer should read first.

**Planning waves 8 to 11 and building wave 8 (2026-10-17).** After wave 7 Andre asked for
a ranked list of missing features towards "a software factory" and picked nine of the
ten (not the Slack channel). The coordinator session wrote `docs/ROADMAP.md` (the order,
the dependencies, the product decisions per feature), Andre settled every decision
with the recommended answer (D-040 to D-048), and the session wrote DESIGN 5.18 to 5.21
in full with the design-first template, the wave 8 contract in `api/openapi.yaml`, one
plan and four briefs under `docs/briefs/wave8/`. At Andre's request every builder was
Sonnet 5.5: the coordinator landed the shared prerequisites first (migration 0012, the
store types, the config, the routes behind stubs, the engine interface, the exported
report-back helpers) and launched four agents on disjoint files: project instructions,
notes and budgets; merge request records, sync and merge; retry from the failed step;
the UI for all four. Each reported its tests, the rules it interpreted and the
integration it left (server wiring, the Chief's `run_status`, the conformance steps,
the budget check on retry), which the coordinator did together with one design
question an agent raised (a later `open_merge_request` on a merged or closed request is
refused) and one it decided (the shipped Coder and Reviewer get the memory tools). The
"As built" paragraphs of 5.18 to 5.21 record the interpretations. Two Sonnet 5.5
reviewer agents reviewed the backend and the API, store and UI separately; the
coordinator ran the full suite, lint and the fix pass.

**Designing and building wave 9 (2026-10-18).** A side session (Claude Fable 5.1 as the
coordinator) read the roadmap entries, the settled decisions, the wave 8 design sections,
briefs and review, and the code the two features touch, then wrote DESIGN 5.22 (iterate
on an open merge request) and 5.23 (issue intake) in full with the design-first template,
amended the older rules they change with "since 5.22/5.23" notes, recorded the product
decisions it settled beyond Andre's (D-049 to D-053), extended `api/openapi.yaml` to
0.6.0 with the wave 9 contract marked `x-planned`, and wrote one plan and four briefs
under `docs/briefs/wave9/`, stopping on the two lines Andre had to answer. Andre answered
both with the recommended answer (D-054, D-055) and asked that every subagent be Sonnet
5.5 to spare the stronger model's tokens, so the coordinator wrote the prerequisites as a
fifth brief and had a Sonnet agent land them (migration 0013, the shared types and store
plumbing, the config, the branch guard in the git helpers, the route stub), verified the
whole Go suite itself in a throwaway container with its own test database, and launched
four Sonnet agents on disjoint files with one test database each: the engine side of the
iteration (queue-time checks, the branch clone, the protocol paragraph, schedules, the
report), the tools side (`open_merge_request` onto a named request, `read_merge_request`,
the Chief's `merge_request` argument, prompts and the template), the intake (settings,
provider issue client, the poll and closer loop, the API), and the UI for both. The
coordinator did the integration, the full suite and lint, the review filtering and the
derived docs; the "As built" paragraphs of 5.22 and 5.23 record the agents'
interpretations.

**Designing and building wave 10 (2026-10-18).** The same side session wrote DESIGN 5.24
(questions to the operator) and 5.25 (run artifacts) with the design-first template after
reading the approval, retry, walk, run-directory and report code they build on, amended
the older rules with "since 5.24/5.25" notes, recorded the decisions it settled (D-056,
D-057) and the two Andre answered with the recommended answer (D-058, D-059), extended
`api/openapi.yaml` to 0.7.0 marked `x-planned`, and wrote the plan, a prerequisites brief
and four agent briefs under `docs/briefs/wave10/`. A Sonnet 5.5 agent landed the
prerequisites (migration 0014, the question and artifact types, the config and the
`artifacts` volume, the engine and tool interfaces, the protocol block texts, the route
stubs), which the coordinator re-verified in a throwaway container; four Sonnet 5.5
agents then built on disjoint files with one test database each: the engine side of
questions (the row and the pause, the decision delivery order, the park and the
continuation visit, the answer endpoint), the tools side (`ask_operator`,
`answer_question`, the notice, `run_status`, prompts and `AGENTS.md`), the artifacts (the
collector over `os.Root`, the diff from a private clone, the store, the endpoints, the
sweep) and the UI (the Question card, the Artifacts section). The coordinator did the
integration, the suite and lint, the review filtering, the fix pass and the derived docs;
the "As built" paragraphs of 5.24 and 5.25 record the agents' interpretations.

**Designing and building wave 11 (2026-10-18).** The same session wrote DESIGN 5.26
(parallel steps: fork and join) with the design-first template after reading the walk,
the visit, the approval and question paths, the copy-forward and the collector it
changes, recorded the decisions it settled (D-060, D-061) and the three Andre answered
with the recommended answer (D-062 to D-064), extended `api/openapi.yaml` to 0.8.0, and
wrote the plan and the briefs under `docs/briefs/wave11/`. As the roadmap had planned for
the one structural engine change, the coordinator built the engine itself: the
prerequisites (migration 0015, the branch rows, the two node kinds with the region
validation of rule 2 and its tests), then the fork node, the branch walks, the stop of
siblings, the parks and the release, the re-entry, the copy-forward's sibling exclusion
and checkout rule, the collector's diff skip and the engine tests (T5.26.2 to T5.26.10),
while two Sonnet 5.5 agents built in parallel on disjoint files: the builder's Fork and
Join nodes with the run page's fork card (web), and the review panel template with its
three seeded reviewer agents and `run_status.branches`. The template agent's report that
the Dev pipeline's limits are too small for a panel round was taken (the template's
limits are doubled); the frontend agent's note that the trace carries no explicit edge
order for branches led to the contract stating that branch rows are created in edge
order. The coordinator ran the full suite and lint, the review and the fix pass, and
wrote the derived docs.

2026-10-19, after wave 11 (Claude Opus 5.5 as coordinator, Sonnet 5.5 subagents, taking
over from an earlier session): the demo was re-recorded against the public instance in ten
scenes, with two new ones (the review panel, the Chief of Staff on Telegram). A Sonnet
agent rewrote the scenes and the script; the coordinator ran the recordings, caught three
broken takes (a Deployer task its tools could not do, a page selector, a project Check
timing out at GitLab) and re-recorded them, and the narration was voiced locally with
Chatterbox from Andre's own reference recording. Andre sent the Telegram message himself;
the first take showed the Chief could not find a run without its id, and the retake named
the run. Then the final audit: two independent Opus auditors, one per spec, read the code
with the project's documents excluded as evidence; the coordinator checked the deliverables
and the OpenAPI contract and re-ran `make eval`, which had fallen to 6 of 17 on a gateway
stuck after quick agent churn. Two Sonnet agents fixed the gateway recovery and the memory
gaps, and the untrusted wrapping, the channels field and the eval labels, on disjoint
files; the coordinator rewrote the README, ADR, threat model and compliance documents
against the code, re-ran the evals (24 of 24) and the CI job's commands under the CI
runner's CPU limits before pushing.
