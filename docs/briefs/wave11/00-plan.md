# Wave 11 plan: parallel steps (fork and join)

Spec: `docs/DESIGN.md` 5.26 (decisions D-048, D-060 and D-061; the three open lines at
its end are D-062 to D-064 once Andre answers them), the roadmap `docs/ROADMAP.md` 5.26,
and the contract `api/openapi.yaml` 0.8.0 (the wave 11 fields and schema carry
`x-planned: wave 11` or "Planned for wave 11" in their descriptions; the coordinator
removes the markers at integration, once each is served). The rules stand as written
until a line is answered otherwise.

## Who builds what

The roadmap planned this wave "built alone, with the stronger model, after the rest is
stable" (D-048's cost line): the engine change is the one place where a mistake is a
lost decision, a doubled visit or a corrupted checkout. So the coordinator (this
session, Fable 5.1) builds the engine, the store, the graph validation and the API
itself, in two steps, and two Sonnet 5.5 agents build what sits beside it in parallel.

| Who | Brief | Sections | Model |
|---|---|---|---|
| P, prerequisites (coordinator, lands first) | `prerequisites.md` | migration 0015, `RunBranch` and `branch_id`, the `fork` and `join` kinds with 5.26 rule 2's validation, `Trace.branches`, `ListRunBranches` | Fable 5.1 |
| E, engine (coordinator, after P, alongside F and T) | `engine.md` | 5.26 rules 3 to 12 (the engine, the store queries, `CopyRunDir`'s `repo/` exclusion, the collector's `SkipDiff`, the contract's conformance steps) | Fable 5.1 |
| F, frontend | `frontend.md` | 5.26 rule 13 | Sonnet 5.5 |
| T, template, agents, run_status | `template-and-agents.md` | 5.26 rule 14 and rule 12's `run_status.branches` | Sonnet 5.5 |

P lands as one commit before F and T start; E, F and T then run at the same time. No
two write the same file (partition below). Shared types, the migration, the kinds and
the validation messages, the contract and the store's branch reads are P's and frozen
for the wave.

## Prerequisites (brief `prerequisites.md`; ground truth for every agent)

1. `internal/store/migrations/0015_wave11.sql`: the table `run_branches` (5.26 rule 3)
   with `UNIQUE (fork_visit_id, head)` and an index on `(run_id, started_at)`;
   `run_nodes.branch_id TEXT REFERENCES run_branches (id) ON DELETE SET NULL` with a
   partial index.
2. `internal/store/types.go`: `RunBranch` with the contract's JSON names and the five
   status constants (`BranchRunning`, `BranchWaitingApproval`, `BranchSucceeded`,
   `BranchFailed`, `BranchCancelled`); `RunNode.BranchID *string`
   (`json:"branch_id,omitempty"`), carried by `runNodeColumns`, `CreateRunNode` and
   `UpdateRunNodeProgress`; `Trace.Branches []RunBranch` (`json:"branches"`), filled by
   `GetTrace`. `internal/store/branches.go`: `ListRunBranches(ctx, q, runID)
   ([]RunBranch, error)` by `started_at, id` and `ListForkBranches(ctx, q,
   forkVisitID)`; the writes are E's (same file, E extends it).
3. `internal/workflow/def.go`: `KindFork = "fork"`, `KindJoin = "join"`; `Node.Join` and
   `Node.Checkout` (`json:"join,omitempty"`, `json:"checkout,omitempty"`); `MaxBranches
   = 5`; `ValidateStructure` grows the checks of 5.26 rule 2 with its exact texts
   (`internal/workflow/fork.go`), built on `Region(forkID) (heads []string, members
   map[string]string, err)` (node id → head) and `ForkOf(joinID)`, exported for the
   engine; `Next` is unchanged (the engine takes every true fork edge through
   `OutgoingEdges`). Tests: T5.26.1 in `fork_test.go`.
4. `api/openapi.yaml` 0.8.0 is already written (markers on).

## File partition

| Who | Owns (create or edit) | Must not touch |
|---|---|---|
| E (coordinator) | `internal/engine/**`, `internal/store/{branches.go, engine_queries.go, runs.go}` and tests, `internal/openclaw/rundir.go` and tests, `internal/artifacts/**`, `internal/httpapi/{openapi_test.go, runs*.go, server.go}` and the wave 11 tests, `api/openapi.yaml` (markers off at integration) | F's and T's files |
| F | `web/src/**` | everything outside `web/` |
| T | `templates/review_panel.json`; `internal/seed/seed.go` and its tests; `internal/tools/chief.go` and `chief_test.go`; the tests that enumerate the shipped templates or the seeded agents outside `web/` and `internal/engine` (listed in the brief) | `internal/workflow`, `internal/store`, `internal/engine`, `internal/httpapi` (except the listed tests), `web/`, `docs/`, `api/openapi.yaml` (report mismatches instead) |

If an agent finds it needs a file it does not own, it stops and says so in its report
rather than editing it.

## Integration (coordinator, after the agents report)

1. Wire nothing new in `server.go` beyond what E adds (the `branch.status` event is
   emitted by the engine through the existing `emit`). Remove the `x-planned: wave 11`
   markers and the "Planned for wave 11" phrases; add the conformance steps and the
   documented fields to `openapi_test.go`'s table.
2. Full suite in throwaway containers from this worktree (Go per package group, web,
   `gofmt`, `go vet`, `pnpm lint`), as in wave 10; never `make test-*` while the stack
   runs. CI is not waited for between pushes (Andre, 2026-10-18): the pipeline that
   deploys is watched at the end.
3. One independent review scoped to the wave's new code (two Sonnet 5.5 reviewer agents
   with disjoint scopes: the engine, store and validation; the API, contract, UI,
   template and seed), filtered visibly into `docs/reviews/wave11-review.md`; one fix
   pass; one commit; merge to `main` when green (main auto-deploys).
4. Derived docs: README (parallel steps, the review panel), `docs/deploy.md` (`make
   seed-reset` creates the three agents and the template), `docs/threat-model.md`
   (branches share the run's counters and limits; a branch's checkout never merges into
   another's), `AI_USAGE.md`, `docs/test-results.md` (suite numbers, live
   verification), `docs/ROADMAP.md` "What happens next".
5. Live verification on the public instance after the deploy and `make seed-reset`
   (`scripts/deploy/verify-wave11.sh`, run over ssh with the token read on the host as
   `verify-wave10.sh` does; nothing printed): a Review panel run on the playground with a
   small task; the three reviewers run at once (three `running` branch rows, three node
   sessions open at the same time in the trace); the join passes; the lead's verdict;
   the gate; the Deployer opens a merge request; the Timeline shows the fork card with
   three columns; `run_status` names the branches while they run.

## Review scope

New code only: the files in the partition, plus the prerequisites. The reviewers are
told which decisions stand (D-048, D-060 to D-064) so they do not re-open them, and
which limitations stay (nested forks, `any` joins, checkout merges, the clock during a
branch's wait: 5.26 out of scope).
