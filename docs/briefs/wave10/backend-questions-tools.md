# Wave 10 backend brief B: questions to the operator, the tools, notices and prompts (5.24); the artifact prompts (5.25 rule 8)

Spec: `docs/DESIGN.md` 5.24 rules 1, 4 (the result shapes), 6, 9 and 12 (the prompts
and `AGENTS.md`), with the amendments they make to 5.7's table and 5.15 rules 2, 8, 10
and 12; 5.25 rules 5 (the `run_status` part) and 8; decisions D-046, D-056 and D-047.
Read `docs/briefs/wave10/00-plan.md` first: the prerequisites there are ground truth
and frozen, in particular `tools.AskOperatorName`, `tools.AnswerQuestionName`, the
`Asker` interface and `PlatformDeps.Asker` (`internal/tools/wave10.go`), `PlatformTools`
in the materialiser, `store.ListRunArtifacts` and `store.RunArtifact`, and the
`Questions:` paragraph in `internal/engine/protocol.go` (the task text; your `AGENTS.md`
section repeats it). A builds the engine side at the same time: `AskOperator`,
`WaitQuestion`, `AnswerQuestion` behave as 5.24 rules 2, 3 and 5 say, and A's engine
also satisfies the optional `tools.QuestionParker` interface you declare (deliverable 1).

Already in the tree and worth reading before you write: `internal/tools/finish.go` and
`internal/tools/readmergerequest.go` (a run-and-chat tool: token handling, `ModelError`,
`JSONResult`), `internal/tools/chief.go` (`NewRunStatus`, `NewDelegateTask`,
`ChatToolNames`, `RunURL`, how a chat token resolves to the agent), `internal/tools/platform.go`
(`NewPlatformRegistry`), `internal/httpapi/mcp.go` (`callPlatformTool`, `callChatTool`:
how a chat-only tool is refused with a run token and the reverse), `internal/engine/report.go`
(`afterApproval`, `reportApprovals`, the waiting line at `approvalReportText`,
`sendReport`, `SendPlatformMessage`, `reportKey`), `internal/project/budget.go` (the
Telegram-bound agent's most recent conversation lookup the alerts use),
`internal/seed/seed.go` (the agents, `ChiefPrompt`, how `make seed-reset` applies
prompts and allowlists), `internal/openclaw/materialize.go` (`AgentsMD`,
`memorySection`, `chatSection`, how sections are composed), `templates/research_pipeline.json`,
`internal/tools/chief_test.go` and `internal/engine/report_test.go` (the harnesses).

## Deliverables

1. **`ask_operator` (rules 1 and 4).** `internal/tools/askoperator.go`: `NewAskOperator(d
   PlatformDeps) *Tool` registered in `NewPlatformRegistry`; `Name: AskOperatorName`,
   `Risk: RiskComputational`, `Hits: HitsPostgres`, `Timeout: MCPToolWait + 30 s`
   (`PlatformDeps` gains `MCPToolWait time.Duration`; the coordinator wires
   `cfg.MCPToolWait`; 0 means 540 s), `Chat: false`; schema `{run_token, question,
   options?}` with the description of rule 12's paragraph in two sentences; the
   validation texts of rule 1 as `ModelError`s (`question: 1 to 1024 characters` after
   trimming; `options: 2 to 5 distinct options of at most 100 characters`, also for an
   empty, duplicate or control-character entry). Exec: `a, err :=
   d.Asker.AskOperator(ctx, token, question, options)` (a `*tools.ModelError` from the
   engine passes through; `engine.ErrInvalidRunToken` becomes `invalid run token`),
   then `wctx` bounded by `MCPToolWait`, `d.Asker.WaitQuestion(wctx, a.ID)`: a decided
   row → `{"status": "answered", "question_id", "answer"}`, `{"status": "declined",
   "question_id", "reason"}` (the row's reason, else `the operator declined to answer`)
   or `{"status": "unanswered", "question_id"}` by status; `wctx` done first →
   `d.Asker.(QuestionParker).QuestionPending(ctx, token, a.ID)` when the engine
   satisfies it (declare `type QuestionParker interface { QuestionPending(ctx
   context.Context, token, id string) error }` in `askoperator.go`; A implements it) and
   `{"status": "pending", "question_id"}`. Never an error after the row exists. A chat
   token answers `ask_operator is not available in a conversation` (the MCP layer's
   text for a non-chat tool, 5.15 rule 2: verify it with a test).
2. **`answer_question` (rule 9).** `internal/tools/answerquestion.go`: `NewAnswerQuestion(d
   PlatformDeps) *Tool`, `Chat: true`, chat-only: a run token answers
   `answer_question is not available in a run` (`tool.go` is frozen and has no chat-only
   flag: a chat call reaches Exec with `Call.RunID` empty, a run call with it set;
   refuse with the text when it is set). Input `{chat_token,
   question_id, answer}` (`answer` trimmed, 1 to 4096 bytes: `answer: 1 to 4096
   characters`). Exec: `d.Asker.AnswerQuestion(ctx, id, answer, "chat:"+agent.Name)`
   with `engine.ErrNotFound`/`store.ErrNotFound` → `no question <id>`,
   `engine.ErrNotPending` → `question <id> is already <status>` (read the row for the
   status), `engine.ErrNotQuestion` → `<id> is not a question`; result `{"status":
   "answered", "run_id", "url"}` with `RunURL`. `ChatToolNames` gains it (the Chief's
   seeded allowlist).
3. **The notice (rule 6).** `internal/engine/report.go`: `func (e *Engine)
   announceQuestion(ctx context.Context, run *store.Run, a *store.Approval)` (A calls it
   from `AskOperator` by this name; you own the body): (a) a run that reports to a
   conversation (`reportsTo(run)`): `afterApproval`'s path sends, for a row of kind
   `question`, `[platform] Run <id> asks: <question> Options: <a> | <b>. Answer it at
   <url>, or tell me the answer and I will pass it on (question <id>).` (the `Options:`
   sentence only with options, joined by ` | `; `<url>` the run page from
   `PublicBaseURL`) instead of the waiting line: branch in `approvalReportText` on the
   kind, keep the claim through `approvals.reported_at` and the deferral give-back as
   they are; (b) otherwise: the most recent conversation of the agent bound to Telegram
   (export the lookup `budget.go` uses as `store.BoundAgentConversation(ctx, q)
   (conversationID string, ok bool, err error)` or the equivalent, and make the budget
   watcher call the exported one), `e.SendPlatformMessage(ctx, run.ID, convID,
   "question-"+a.ID, text)` with the same text; no binding or conversation: the log
   line `question <id> has nowhere to be announced: no conversation and no Telegram
   binding`. Do not send twice when (a) applies. `report_test.go`: the channel run's
   notice (and no waiting line for it), the manual run's notice under the key, the
   log line, a deferred (b) retried by whoever calls again (one attempt per call, as the
   alerts).
4. **`run_status` (5.24 rule 11, 5.25 rule 5).** `chief.go` `NewRunStatus`: while the
   run is `waiting_approval` and a pending approval of kind `question` exists, add
   `question: {id, text, options?, asked_at}` (keep the existing pending-approval entry
   too); always add `artifacts: [{path, size_bytes}]` (the first 20 by path from
   `store.ListRunArtifacts`) and `artifact_count`, omitting both when the count is 0.
   Tests in `chief_test.go`.
5. **Prompts, allowlists and `AGENTS.md` (5.24 rule 12, 5.25 rules 1 and 8).**
   `seed.go`: the Coder and the Researcher gain `ask_operator` in `ToolAllowlist` and
   the sentence `Ask the operator with ask_operator only when a missing fact decides the
   work; otherwise decide and state the assumption.` at the end of their prompts; the
   Writer's prompt gains `Put the finished report under runs/<run id>/out/ so the
   operator receives it.`; `ChiefPrompt` gains the answering sentence of rule 9 (`When a
   platform message says a run asks a question, put the question and its options to the
   person in your own words; when they answer, call answer_question with the question
   id from that message and their answer as they wrote it, then confirm. Never answer a
   run's question yourself.`). `materialize.go`: in `AgentsMD` the sentence `Do not ask
   questions; decide, act, and finish.` becomes `Do not ask questions in your reply:
   nobody reads it while you work; decide, act, and finish.` and `AgentsMD` gains the
   `Deliverables` sentence of 5.25 rule 1 (with `<run_id>` as the file writes it);
   `questionsSection(allow []string) string` returns `\n## Questions\n\n` plus the text
   of `engine.QuestionsParagraph` with `<run_id>` in place of the id (do not import the
   engine from openclaw if that cycles: copy the text into a constant and add a test
   that the two texts match up to the placeholder) only when `allow` holds
   `yuno__ask_operator`; composed next to `memorySection`. `templates/research_pipeline.json`:
   the writer node's task gains the same `Put the finished report under runs/$RUN_ID/out/`
   sentence. Seed and materialiser tests updated; `make seed-reset` applies the prompts
   (the coordinator runs it on the host).
6. **Tests**: T5.24.1 (the tool's validation texts and the chat token), the result
   shapes of rule 4 against a fake `Asker` (answered, declined with and without a
   reason, unanswered, pending with `QuestionPending` called), T5.24.11
   (`answer_question`: happy path with `decided_by`, unknown, decided, a tool
   approval's id, a run token), T5.24.10 (the notices), T5.24.13 and T5.25.8
   (`run_status`), T5.24.14 (the `## Questions` section only for holders; the amended
   sentence for all), the seed and template changes.

## Rules of the house

- Everything runs in Docker, from this worktree in a throwaway container on the stack's
  network with your own test database:

  ```
  docker run --rm --network yuno_default -v "$PWD:/src" -v yuno_gomod:/go/pkg/mod -v yuno_gocache:/root/.cache/go-build -w /src -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 -e DATABASE_URL='postgres://yuno:yuno@postgres:5432/yuno_test_a2?sslmode=disable' yuno-go-tools:1.26 sh -c 'go test ./internal/tools/ ./internal/engine/ -run "Report|Question|Chief|RunStatus" ./internal/seed/ ./internal/openclaw/ ./internal/project/ -count=1 2>&1 | tail -60'
  ```

  (adjust the `-run` filter to your test names; run the whole `internal/tools`,
  `internal/seed`, `internal/openclaw` and `internal/project` packages at least once).
  vet with `go vet ./...` and `gofmt -l .` in the same container. Write long outputs to
  a file under `/tmp` and read the file. Never run `make run`, `make down`, `make
  clean`, `make test-*`, `make lint` or `docker compose ...` from this directory; never
  run the whole suite: the coordinator does.
- Files you own: `internal/tools/{askoperator.go, answerquestion.go, chief.go,
  platform.go}` and their tests; `internal/engine/report.go` and `report_test.go`;
  `internal/project/budget.go` (the export only) and its test; `internal/seed/seed.go`
  and tests; `internal/openclaw/materialize.go` and tests;
  `templates/research_pipeline.json`. Nothing else: `tool.go`, `wave10.go` (tools and
  engine), `types.go`, `contract.go`, `questions.go`, `approvals.go`, `node.go`,
  `walk.go`, `mcp.go`, `server.go`, `web/`, `docs/`, `api/openapi.yaml` are other hands'
  or frozen. If `mcp.go` cannot tell a run token from a chat token inside a tool,
  report it instead of editing `mcp.go`.
- Rule texts are exact: copy them from 5.24 and 5.25, do not paraphrase. No reasoning
  text in any fixture; no real token in any test.
- Commit nothing; the coordinator commits. Report: the files changed, the test
  output's last lines, every rule you had to interpret, and anything you found wrong
  in the design.
