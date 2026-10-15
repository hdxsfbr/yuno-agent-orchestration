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

