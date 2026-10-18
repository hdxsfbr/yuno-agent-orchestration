# Wave 10 plan: questions to the operator, run artifacts

Spec: `docs/DESIGN.md` 5.24 and 5.25 (decisions D-046, D-047, D-056 and D-057), the
roadmap `docs/ROADMAP.md`, and the contract `api/openapi.yaml` 0.7.0 (the wave 10
paths, schemas and fields carry `x-planned: wave 10` or "Planned for wave 10" in their
descriptions; the coordinator removes the markers at integration, once each is served).
The two open lines at the end of 5.24 are Andre's; the wave does not start until they
are answered and recorded (D-058, D-059). The rules carry the recommended answers, so a
"yes" to both changes nothing below.

## Agents

| Agent | Brief | Sections | Model |
|---|---|---|---|
| P, prerequisites (lands first, alone) | `prerequisites.md` | the columns, types, config, interfaces and stubs every agent builds on | Sonnet 5.5 |
| A, questions: engine, store, API | `backend-questions-engine.md` | 5.24 rules 2, 3, 4, 5, 8, 10, 11 (the API part) | Sonnet 5.5 |
| B, questions: tools, notices, prompts | `backend-questions-tools.md` | 5.24 rules 1, 6, 7 (the text), 9, 12 (prompts and `AGENTS.md`); 5.25 rule 8 | Sonnet 5.5 |
| C, artifacts | `backend-artifacts.md` | 5.25 rules 2 to 6 | Sonnet 5.5 |
| D, frontend | `frontend.md` | 5.24 rule 12 (UI), 5.25 rule 7 | Sonnet 5.5 |

Andre chose Sonnet 5.5 for every agent (2026-10-18, "use Sonnet subagents"); the
delicate parts (the decision delivery order and the park, the loop count, the diff
without git in the checkout, the download headers) are covered by the designs' exact
rules and the independent review rather than by a stronger builder. 5.24 is split along
the engine/tools seam as 5.22 was: A owns the state machine, B owns what the model and
the Chief see.

P runs first and alone; A, B, C and D start together once P's commit is in. No two
write the same file (partition below). Shared types, the migration, the routes, the
config, the engine and tool interfaces and the protocol block constants are P's and
frozen for the wave.

## Prerequisites (brief `prerequisites.md`; ground truth for every agent)

1. `internal/store/migrations/0014_wave10.sql`: `approvals.options JSONB`,
   `approvals.answer TEXT`; `run_nodes.question_id TEXT REFERENCES approvals (id) ON
   DELETE SET NULL`, `run_nodes.parked_at TIMESTAMPTZ`; the partial index
   `approvals_question_step_idx ON approvals (run_id, node_id) WHERE kind = 'question'`
   (5.24 rule 2's count); the table `run_artifacts` (5.25 rule 4) with `UNIQUE (run_id,
   path)`.
2. `internal/store/types.go`: `ApprovalKindQuestion = "question"`; `Approval` gains
   `Options json.RawMessage` (`db:"options" json:"options,omitempty"`) and `Answer
   *string` (`db:"answer" json:"answer,omitempty"`), carried by `approvalSelect` and
   `InsertApproval` (`runs.go`); `RunNode` gains `QuestionID *string` (`db:"question_id"
   json:"question_id,omitempty"`) and `ParkedAt *time.Time` (`db:"parked_at" json:"-"`),
   carried by the run node select and `CreateRunNode`; `RunArtifact{ID, RunID, NodeID
   string; Visit int; Path string; Size int64; SHA256, ContentType string; Generated
   bool; CreatedAt, UpdatedAt time.Time}` with the contract's JSON names.
   `internal/store/artifacts.go`: `ListRunArtifacts(ctx, q, runID) ([]RunArtifact,
   error)` ordered by path (B's `run_status` calls it; C extends the file).
3. `internal/config/config.go`, `.env.example`, `compose.yaml`: `ARTIFACT_DIR` (default
   `/data/artifacts`) and `ARTIFACT_TTL_DAYS` (default 30); the named volume `artifacts`
   mounted on the api at `/data/artifacts`; `docker/api.Dockerfile` creates
   `/data/artifacts` owned by `app`.
4. `internal/tools/wave10.go`: `AskOperatorName = "ask_operator"`, `AnswerQuestionName =
   "answer_question"`; the `Asker` interface (`AskOperator(ctx, token, question string,
   options []string) (*store.Approval, error)`, `WaitQuestion(ctx, id string)
   (*store.Approval, error)`, `AnswerQuestion(ctx, id, answer, by string)
   (*store.Approval, error)`), `PlatformDeps.Asker Asker` and `PlatformDeps.MCPToolWait`. `internal/openclaw/materialize.go`:
   `PlatformTools` gains both names (the prefixing list only).
5. `internal/engine/contract.go`: the engine's interface gains the three `Asker`
   methods; `Deps.Artifacts ArtifactCollector` with `ArtifactCollector interface {
   Collect(ctx context.Context, in ArtifactInput) }` and `ArtifactInput{Workspace string;
   Run *store.Run; Node *store.RunNode; Project *project.Config; Branch string}`
   (`Project` nil without a checkout; `Branch` the request's source branch for a 5.22
   run, else the default). `internal/engine/wave10.go`: the three methods returning
   `ErrNotImplemented` (A replaces the bodies in `questions.go`), `Emit(ctx, runID, event
   string, payload map[string]any)` and `LogEvent(ctx, runID, level, msg string,
   attrs ...any)` as exported wrappers of `emit` and `logEvent`, and the call site in
   `walk.agentNode` right after `v.finish(wctx, res)`: `e.collectArtifacts(wctx, v)`,
   which does nothing while `Deps.Artifacts` is nil; the stub `announceQuestion` in
   `report.go` (A calls it, B fills it).
6. `internal/engine/protocol.go`: `DeliverablesLine` and `QuestionsParagraph` (the exact
   texts of 5.25 rule 1 and 5.24 rule 12, `<run id>` substituted) and their insertion in
   `taskMessageWith` after the repository lines: the line for every visit, the paragraph
   when `v.agent.ToolAllowlist` holds `ask_operator`.
7. `internal/httpapi/server.go`: the routes `POST /api/approvals/{id}/answer` →
   `answerApproval` (`approvals.go`, 501 `not_implemented` until A), `GET
   /api/runs/{id}/artifacts` → `listArtifacts` and `GET
   /api/runs/{id}/artifacts/{artifactId}` → `downloadArtifact` (`artifacts.go`, 501 until
   C); `platformDeps` passes `pd.Asker = eng` and `pd.MCPToolWait`. `internal/openclaw/rundir.go`: `OpenRunDir(stateDir,
   openclawID, runID string) (*os.Root, error)` (today's `openRunDir` over the workspace
   root, exported; C reads `out/` through it).

## File partition

| Agent | Owns (create or edit) | Must not touch |
|---|---|---|
| A | `internal/engine/{questions.go (new), approvals.go, node.go, walk.go, retry.go}` and the new `internal/engine/wave10_questions_test.go`; `internal/store/{approvals.go, engine_queries.go}` and tests; `internal/httpapi/{approvals.go, agents.go}` and their tests | everything of B, C and D; `types.go`, `runs.go` (store), `contract.go`, `wave10.go` (engine and tools), `protocol.go`, migrations, `server.go`, `main_test.go`, `report.go`, `api/openapi.yaml` (report mismatches instead) |
| B | `internal/tools/{askoperator.go, answerquestion.go, chief.go, platform.go}` and their tests (`askoperator_test.go`, `answerquestion_test.go`, `chief_test.go`); `internal/engine/report.go` and `report_test.go`; `internal/project/budget.go` (only to export the bound agent's conversation lookup) and its test; `internal/seed/seed.go` and tests; `internal/openclaw/materialize.go` and tests; `templates/research_pipeline.json` | A's, C's and D's files; `wave10.go` (tools), `types.go`, `api/openapi.yaml`, `web/`, `docs/` |
| C | new package `internal/artifacts/`; `internal/project/diff.go` and `diff_test.go`; `internal/store/artifacts.go` and `artifacts_test.go`; `internal/httpapi/artifacts.go` and `artifacts_test.go`; `internal/openclaw/rundir.go` (additions only) | A's, B's and D's files; `types.go`, `contract.go`, `walk.go`, `node.go`, `server.go`, migrations, `api/openapi.yaml`, `web/`, `docs/` |
| D | `web/src/**` | everything outside `web/` |

A touches `node.go` for the park and the next-turn delivery (`turnFinal`, the
`parkedOn` state, `pause`), `walk.go` for the paused outcome of an agent visit and the
continuation visit's `question_id`, `retry.go` for `CountNodeLoopVisits`, and
`approvals.go` for `decideQuestion`, the expiry cases and the sweeper. B's `report.go`
edit adds the question notice and leaves the existing texts alone. C's `rundir.go`
edit adds helpers and changes nothing P exported. If an agent finds it needs a file it
does not own, it stops and says so in its report rather than editing it.

## Integration (coordinator, after the agents report)

1. `server.go` `Run`: build C's collector (`artifacts.New(cfg.ArtifactDir, pool, log,
   eng.Emit, eng.LogEvent)`; a nil collector with the log line when the directory is
   unusable), set `Deps.Artifacts`, add `collector.Sweep` to the memory sweeper's `Also`
   with the TTL; nothing else to wire for A and B (P's `pd.Asker = eng` and the engine's
   own paths).
2. `openapi_test.go`: append `questionConformanceSteps` (A) and
   `artifactConformanceSteps` (C) to `TestOpenAPIConformance`, add the new fields to
   `TestOpenAPIDocumentsChannelAndSchedulerFields`'s table, and remove the `x-planned`
   markers and "Planned for wave 10" phrases from `api/openapi.yaml`. Full suite in
   throwaway containers from this worktree (Go per package group, web, `gofmt`, `go
   vet`, `pnpm lint`), as in wave 9; never `make test-*` while the stack runs.
3. One independent review scoped to the wave's new code (two Sonnet 5.5 reviewer agents
   with disjoint scopes as in wave 9), filtered visibly into
   `docs/reviews/wave10-review.md`; one fix pass; one commit; merge to main when green
   (main auto-deploys).
4. Derived docs: README (asking the operator, answering from Telegram, artifacts and
   the Artifacts section, `ARTIFACT_DIR` and `ARTIFACT_TTL_DAYS`), `docs/deploy.md`
   (the new volume, the two settings, `make seed-reset` for the prompts and allowlists),
   `docs/threat-model.md` (an answer is operator text and reaches the model unwrapped;
   artifacts are served as attachments with nosniff and never inline; the diff is
   produced in a private clone, never with the checkout's `.git`; whoever can message
   the Chief can answer a question), `AI_USAGE.md`, `docs/test-results.md` (suite
   numbers, live verification), `docs/ROADMAP.md` "What happens next".
5. Live verification on the public instance after the deploy and `make seed-reset`
   (`scripts/deploy/verify-wave10.sh`, run over ssh with the token read on the host as
   `verify-wave9.sh` does; nothing printed):
   - a question answered within the wait: a Dev pipeline run on the playground whose
     task says to ask the operator which greeting language to use before writing the
     code; the inbox shows the Question card with the options; Andre answers from the
     UI; the trace shows `ask_operator` returning `answered` in the same turn and the
     run continuing;
   - a question past the wait: the same task, left unanswered for ten minutes; the run
     page shows the step `waiting_approval` with `Asks:` on the header; Andre answers;
     the run re-queues and the continuation visit (visit 2 of the Coder, `question_id`
     set) finishes the task; the Timeline shows both visits;
   - Telegram: the question notice arrives through the Chief (the run has no
     conversation, so line 1's route), Andre replies with the answer, the Chief calls
     `answer_question`, the question card shows `chat:Chief of Staff`;
   - artifacts: a research pipeline run delivers `out/report.md`; the Artifacts section
     lists it with its size, Download saves it, Preview shows it; the Dev pipeline run
     above shows `diff.patch` with the Coder's hunk; `run_status` from the Chief names
     the files.

## Review scope

New code only: the files in the partition, plus the prerequisites. The reviewer is told
which decisions stand (D-046, D-047, D-056 to D-059) so it does not re-open them, and
which limitation stays (a parked step restarts as a new visit, not in the old session,
5.24 out of scope).
