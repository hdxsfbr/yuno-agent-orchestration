# Wave 10 prerequisites: what is landed and frozen before the four agents start

These are the "Prerequisites" of `docs/briefs/wave10/00-plan.md`, written as one brief
because a Sonnet 5.5 agent lands them (Andre, 2026-10-18: every subagent on Sonnet).
Spec: `docs/DESIGN.md` 5.24 rules 2, 5, 11 and 12 and 5.25 rules 1, 2 and 4 for what the
columns, types and texts mean; decisions D-056 and D-057. Nothing here implements a
feature: it adds columns, types, config, interfaces, exact texts and stubs that the
agents' code will fill, so that no two of them edit one file. Every existing test must
stay green; the new helpers get their own small tests.

Already in the tree and worth reading before you write: `internal/store/migrations/0013_wave9.sql`
(the style), `internal/store/types.go` (`Approval`, `RunNode`, `ApprovalKindTool` and
friends), `internal/store/runs.go` (`approvalSelect`, `InsertApproval`, the run node
select and `CreateRunNode`), `internal/config/config.go` with `.env.example`,
`compose.yaml` and `internal/config/config_test.go` (`TestEnvExampleAndComposeCoverConfig`
fails when a setting is missing from either file), `docker/api.Dockerfile` (the `/data`
ownership), `internal/tools/platform.go` (`PlatformDeps`, `NewPlatformRegistry`),
`internal/tools/finish.go` (how a tool reaches the engine through a func or interface),
`internal/engine/contract.go` (the engine interface with `AcceptFinish`, `Deps`),
`internal/engine/engine.go` (`emit`, `logEvent`), `internal/engine/walk.go` (`agentNode`:
where `v.finish(wctx, res)` is), `internal/engine/node.go` (`taskMessageWith`: the
protocol block), `internal/openclaw/materialize.go` (`PlatformTools`),
`internal/openclaw/rundir.go` (`openRunDir`), `internal/httpapi/server.go` (`routes`,
`platformDeps`) and `internal/httpapi/projects_intake.go` at commit `b5c6e96` (`git show
b5c6e96:internal/httpapi/projects_intake.go`: how wave 9's 501 stub looked).

## Deliverables

1. **Migration** `internal/store/migrations/0014_wave10.sql`, commented like 0013:
   ```sql
   ALTER TABLE approvals ADD COLUMN options JSONB;
   ALTER TABLE approvals ADD COLUMN answer TEXT;
   CREATE INDEX approvals_question_step_idx ON approvals (run_id, node_id) WHERE kind = 'question';
   ALTER TABLE run_nodes ADD COLUMN question_id TEXT REFERENCES approvals (id) ON DELETE SET NULL;
   ALTER TABLE run_nodes ADD COLUMN parked_at TIMESTAMPTZ;
   CREATE TABLE run_artifacts (
       id           TEXT PRIMARY KEY,
       run_id       TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
       node_id      TEXT NOT NULL,
       visit        INT NOT NULL,
       path         TEXT NOT NULL,
       size         BIGINT NOT NULL,
       sha256       TEXT NOT NULL,
       content_type TEXT NOT NULL,
       generated    BOOLEAN NOT NULL DEFAULT false,
       created_at   TIMESTAMPTZ NOT NULL,
       updated_at   TIMESTAMPTZ NOT NULL,
       UNIQUE (run_id, path)
   );
   ```
2. **Types and columns.** `types.go`: `ApprovalKindQuestion = "question"` beside the
   other kinds; `Approval` gains `Options json.RawMessage` (`db:"options"
   json:"options,omitempty"`) after `Message` and `Answer *string` (`db:"answer"
   json:"answer,omitempty"`) after `Reason`; `RunNode` gains `QuestionID *string`
   (`db:"question_id" json:"question_id,omitempty"`) and `ParkedAt *time.Time`
   (`db:"parked_at" json:"-"`). `runs.go`: `approvalSelect` lists `options, answer`;
   `InsertApproval` writes them (`options` as `$n::jsonb`, nil → NULL); the run node
   select and `CreateRunNode` carry `question_id` and `parked_at` (both nullable). A
   store test round-trips an approval with options and an answer and a node with a
   `question_id`.
   `RunArtifact` in `types.go`:
   ```go
   type RunArtifact struct {
       ID          string    `db:"id" json:"id"`
       RunID       string    `db:"run_id" json:"run_id"`
       NodeID      string    `db:"node_id" json:"node_id"`
       Visit       int       `db:"visit" json:"visit"`
       Path        string    `db:"path" json:"path"`
       Size        int64     `db:"size" json:"size"`
       SHA256      string    `db:"sha256" json:"sha256"`
       ContentType string    `db:"content_type" json:"content_type"`
       Generated   bool      `db:"generated" json:"generated"`
       CreatedAt   time.Time `db:"created_at" json:"created_at"`
       UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
   }
   ```
   `internal/store/artifacts.go` (new): `const artifactColumns`, `ListRunArtifacts(ctx,
   q Querier, runID string) ([]RunArtifact, error)` ordered by `path` (an empty slice,
   not nil, when none), with a test that inserts two rows by hand and lists them.
3. **Config and compose.** `config.go`: `ArtifactDir string` (`ARTIFACT_DIR`, default
   `/data/artifacts`) and `ArtifactTTLDays int` (`ARTIFACT_TTL_DAYS`, default 30), in
   the "API and engine" group with a comment citing 5.25 rules 4 and 6. `.env.example`:
   both, commented out with their defaults, next to `TOOL_OUTPUT_MAX_BYTES`.
   `compose.yaml`: both in `x-api-env` (`${ARTIFACT_DIR:-/data/artifacts}`,
   `${ARTIFACT_TTL_DAYS:-30}`); `api.volumes` gains `- artifacts:/data/artifacts` with a
   comment (the first volume the api mounts: the files runs deliver, 5.25 rule 4);
   `volumes:` gains `artifacts: {}`. `docker/api.Dockerfile`: the `mkdir -p /data`
   becomes `mkdir -p /data /data/artifacts` and the `chown` covers both (`chown -R
   app:app /data`), with the comment extended: the named volume inherits the ownership
   when first created. `TestEnvExampleAndComposeCoverConfig` stays green.
4. **Tool names and the engine's door.** `internal/tools/wave10.go` (new):
   ```go
   const (
       AskOperatorName    = "ask_operator"
       AnswerQuestionName = "answer_question"
   )
   // Asker is what ask_operator and answer_question call on the engine (DESIGN 5.24
   // rules 2, 3 and 9); the engine satisfies it.
   type Asker interface {
       AskOperator(ctx context.Context, token, question string, options []string) (*store.Approval, error)
       WaitQuestion(ctx context.Context, id string) (*store.Approval, error)
       AnswerQuestion(ctx context.Context, id, answer, by string) (*store.Approval, error)
   }
   ```
   `PlatformDeps` gains `Asker Asker` with a comment (nil leaves both tools unable to
   run) and `MCPToolWait time.Duration` (the wait `ask_operator` blocks for; `server.go`'s
   `platformDeps` passes `cfg.MCPToolWait`). `internal/openclaw/materialize.go`: `PlatformTools` gains `"ask_operator"` and
   `"answer_question"` (so the model sees `yuno__ask_operator`); the materialiser's
   tests that enumerate the list are updated.
5. **Engine interface, deps and wrappers.** `internal/engine/contract.go`: the engine
   interface gains the three `Asker` methods (same signatures, documented with the rule
   numbers); `Deps` gains `Artifacts ArtifactCollector`:
   ```go
   // ArtifactCollector collects the files a visit left under runs/<run id>/out/ (DESIGN
   // 5.25 rule 2); nil disables collection.
   type ArtifactCollector interface {
       Collect(ctx context.Context, in ArtifactInput)
   }
   // ArtifactInput is one visit's end: the agent's workspace directory, the run and the
   // visit, the project configuration when the run has a checkout (nil otherwise), and
   // the branch the checkout is on (the request's source branch for a 5.22 run, else
   // the default branch).
   type ArtifactInput struct {
       Workspace string
       Run       *store.Run
       Node      *store.RunNode
       Project   *project.Config
       Branch    string
   }
   ```
   `internal/engine/wave10.go` (new): `AskOperator`, `WaitQuestion` and
   `AnswerQuestion` on `*Engine` returning `nil, ErrNotImplemented` (A replaces them in
   `questions.go` and deletes these three); `func (e *Engine) Emit(ctx, runID, event
   string, payload map[string]any)` and `func (e *Engine) LogEvent(ctx, runID, level,
   msg string, attrs ...any)` wrapping `emit` and `logEvent`; `func (e *Engine)
   collectArtifacts(ctx context.Context, v *visit)` that returns at once when
   `e.deps.Artifacts == nil` and otherwise calls `Collect` with `Workspace:
   openclaw.WorkspaceDir(e.deps.Cfg.OpenClawStateDir, v.ocID)`, `Run: v.run`, `Node:
   v.rn`, `Project: v.project` (nil when `v.toolchain.Name == ""`), `Branch` the merge
   request's `SourceBranch` when `v.mergeRequest != nil`, else the project's
   `DefaultBranch`. `walk.go` `agentNode`: the one line `e.collectArtifacts(wctx, v)`
   right after `v.finish(wctx, res)`. A test with a fake collector asserts it is called
   once per agent visit with the workspace path and the node (the scripted provider
   harness of `engine_test.go`). `internal/engine/report.go`: the stub `func (e *Engine)
   announceQuestion(ctx context.Context, run *store.Run, a *store.Approval)` with a
   one-line debug log body (A calls it from `AskOperator`, B writes the body).
6. **Protocol block texts.** `internal/engine/protocol.go` (new):
   ```go
   // DeliverablesLine is DESIGN 5.25 rule 1's line of the task protocol block.
   func DeliverablesLine(runID string) string {
       return "Deliverables: put files the operator should receive (reports, data, documents) under runs/" +
           runID + "/out/; the platform collects them when your step ends and shows them on the run page.\n"
   }
   // QuestionsParagraph is DESIGN 5.24 rule 12's paragraph, for a visit whose agent holds
   // ask_operator.
   func QuestionsParagraph(runID string) string {
       return "Questions: when one fact you cannot find out yourself decides the work, ask the operator with " +
           "yuno__ask_operator: a short question, with options when a few answers are likely; at most 3 per step. " +
           "The answer usually comes back in the tool result. If the result says \"pending\", save your progress " +
           "in files under runs/" + runID + "/ and end your turn without calling yuno__finish: the platform runs " +
           "this step again with the answer added to your task. Decide yourself what you can.\n"
   }
   ```
   `node.go` `taskMessageWith`: after the project lines (the `if v.project != nil ...`
   block) and before the `When finished, call the tool yuno__finish` sentence, write
   `DeliverablesLine(v.run.ID)` always and `QuestionsParagraph(v.run.ID)` when
   `slices.Contains(v.agent.ToolAllowlist, tools.AskOperatorName)`. Update the task
   block tests that compare the whole text (`rundir_test.go`, `project_test.go`,
   `wave9_iteration_test.go` or wherever the exact block is asserted), and add one
   asserting the paragraph appears only for an agent holding the tool.
7. **Routes and stubs.** `server.go` `routes`: `POST /api/approvals/{id}/answer` →
   `s.answerApproval` (in `approvals.go`, answering 501 `not_implemented` with the
   message `planned for wave 10` until A replaces it), `GET /api/runs/{id}/artifacts` →
   `s.listArtifacts` and `GET /api/runs/{id}/artifacts/{artifactId}` →
   `s.downloadArtifact` (both in a new `internal/httpapi/artifacts.go`, 501 until C).
   `platformDeps`: `pd.Asker = eng` (the engine value already passed for finish; keep
   the nil-engine case nil) and `pd.MCPToolWait = cfg.MCPToolWait`. `internal/openclaw/rundir.go`: export `OpenRunDir(stateDir,
   openclawID, runID string) (*os.Root, error)`: opens the workspace root then
   `openRunDir`, returning `fs.ErrNotExist` for a missing workspace or run directory and
   `errRunDirLink` (exported as `ErrRunDirLink`) for a link; `CopyRunDir` unchanged.
   A test opens a fixture workspace and refuses a link.

## Tests and the gate

- `go build ./...`, `go vet ./...`, `gofmt -l .` clean; the store, config, engine,
  tools, openclaw and httpapi packages green (your own database, below); the migration
  applies on an empty database and on one migrated to 0013 (the test harness does
  both).
- The 501 stubs are reached by the conformance test only once the coordinator adds
  steps, so nothing in `openapi_test.go` changes here.

## Rules of the house

- Everything runs in Docker, from this worktree in a throwaway container on the stack's
  network with your own test database:

  ```
  docker run --rm --network yuno_default -v "$PWD:/src" -v yuno_gomod:/go/pkg/mod -v yuno_gocache:/root/.cache/go-build -w /src -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 -e DATABASE_URL='postgres://yuno:yuno@postgres:5432/yuno_test_pre?sslmode=disable' yuno-go-tools:1.26 sh -c 'go build ./... && go vet ./... && gofmt -l . && go test ./internal/store/ ./internal/config/ ./internal/engine/ ./internal/tools/ ./internal/openclaw/ ./internal/httpapi/ -count=1 2>&1 | tail -60'
  ```

  Write long outputs to a file under `/tmp` and read the file. Never run `make run`,
  `make down`, `make clean`, `make test-*`, `make lint` or `docker compose ...` from this
  directory: a stack is running and must not be restarted.
- Touch only the files named above. Do not start any feature: no `questions.go`, no
  `internal/artifacts`, no UI. If a frozen name above cannot work as written, stop and
  say why in your report instead of inventing another.
- Commit nothing; the coordinator commits. Report: the files changed, the test
  output's last lines, and every rule you had to interpret.
