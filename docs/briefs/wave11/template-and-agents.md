# Wave 11 brief T: the Review panel template, its agents, and `run_status.branches`

Spec: `docs/DESIGN.md` 5.26 rule 14 (the template and the three seeded agents) and rule
12's `run_status.branches`, with 5.6 rule 9 (templates) and 5.15 rule 8 (`run_status`);
decisions D-048 and D-064; `api/openapi.yaml` 0.8.0: `WorkflowNode.kind` values `fork`
and `join`, `WorkflowNode.join`, `RunBranch`. Read `docs/briefs/wave11/00-plan.md` first:
the prerequisites there are ground truth and frozen, in particular `workflow.KindFork`,
`workflow.KindJoin`, `Node.Join`, the validation of 5.26 rule 2 (your template must pass
`ValidateStructure`), `store.RunBranch` and `store.ListRunBranches`.

Already in the tree and worth reading before you write: `templates/dev_pipeline.json`
(the shape your template extends: schemas, tasks, the `project` flag, the gate and the
Deployer), `internal/httpapi/templates.go` (`LoadTemplates`: every `templates/*.json`
becomes a workflow row at api start; `instantiate` resolves `agent_name`),
`internal/httpapi/templates_test.go`, `internal/seed/seed.go` (`Agents`, `TemplateKeys`,
`ResetShipped`, how prompts are written) and every `internal/seed/*_test.go`
(`TestShippedPromptsAndToolsForQuestionsAndDeliverables` and friends read the shipped
prompts and the templates: extend them for the new agents and template),
`internal/tools/chief.go` (`NewRunStatus`: the `out` map, `runRequest`, the artifacts
block) and `chief_test.go`, `internal/workflow/def.go` (`Parse`, `Node`),
`internal/evals/cases_wave3_test.go` (how a shipped template is read in a test).

## Deliverables

1. **`templates/review_panel.json`** (`key: "review_panel"`, `name: "Review panel"`, a
   description in the Dev pipeline's style): the graph of 5.26 rule 14: `start` →
   `coder` (agent_name `Coder`; the Dev pipeline's coder task and schema) → `panel`
   (`kind: fork`, `join: "merge"`) → `security` (`Security Reviewer`), `performance`
   (`Performance Reviewer`), `maintainability` (`Maintainability Reviewer`): each an agent
   node with `input_from: ["start", "coder"]`, a task in the Reviewer's style focused on
   its concern (`Review the change for <concern>: ... The coder reports:
   {{.state.coder.summary}} Files: ... If your task's protocol block describes a project
   repository, the change is in that checkout: run git status and git diff <default
   branch>, read new files ... Do not change any file. Finish with verdict "approved"
   when nothing of your concern blocks the change, otherwise "changes_requested" with
   specific, actionable feedback.`), output schema `{verdict: approved |
   changes_requested, feedback}` (both required) → `merge` (`kind: join`, no
   `checkout`) → `lead` (agent_name `Reviewer`; `input_from: ["coder", "security",
   "performance", "maintainability"]`; task: merge the three reviews into one verdict:
   `approved` only when all three approved, otherwise `changes_requested` with every
   point the reviewers raised, grouped and de-duplicated, as feedback the coder can act
   on; the same output schema) → edges `lead → coder` when `/lead/verdict eq
   changes_requested`, `lead → gate` when `approved` → `gate` (the Dev pipeline's
   approval node, message naming the lead's feedback) → `deployer` (`Deployer`, the Dev
   pipeline's task and schema, `input_from: ["coder", "lead"]`) → `end`. The coder's
   task reads the lead's feedback (`{{with .state.lead}}...{{end}}`) as the Dev
   pipeline's reads the reviewer's. `state_schema` lists coder, security, performance,
   maintainability, lead and deployer; `project: true`; `limits` as the Dev pipeline's
   with `max_loop_visits` 3. The edges from `panel` carry no `when`. The template must
   pass `workflow.ValidateStructure` (a test loads the file and checks it, as
   `cases_wave3_test.go` reads one) and instantiate against the seeded agents.
2. **Seeded agents** (`internal/seed/seed.go` `Agents`): `Security Reviewer` (role
   `Security reviewer`), `Performance Reviewer` (`Performance reviewer`),
   `Maintainability Reviewer` (`Maintainability reviewer`), all `claude-sonnet-5-5`,
   allowlist `read, ls, exec, finish`, `MemoryPolicy` off. Prompts in the Reviewer's
   voice, each naming its concern (security: injection, secrets in code, missing
   authorisation and input checks, unsafe deserialisation, dependency risk; performance:
   complexity, repeated queries, needless allocation and blocking I/O, missing bounds;
   maintainability: naming, structure, duplication, tests that prove the change,
   documentation where a reader needs it), each saying: review the change in the
   checkout with `git status` and `git diff <default branch>`, read new files, run the
   project's tests only when your concern needs it, never change a file, and when
   nothing of your concern blocks the change approve it. Append `review_panel` to
   `TemplateKeys` so `make seed` instantiates it once and `make seed-reset` restores its
   definition (5.16 rule 14). Keep the existing agents' prompts unchanged. Tests: the
   seed creates the three (idempotent), `ResetShipped` restores their prompts and the
   template, the shipped prompts test covers them.
3. **`run_status.branches`** (`internal/tools/chief.go`): when the run's `current_node`
   names a node of kind `fork` in its snapshot (`workflow.Parse(run.WorkflowSnapshot)`,
   `Node(current)`), add `branches: [{head, status, current_node}]` with the rows of the
   newest fork visit of that fork (`store.ListRunBranches`, take the rows whose
   `fork_visit_id` is the one with the latest `started_at` among rows with that
   `fork_id`), in `started_at` order; omit the key otherwise. The description sentence
   of the tool gains `the branches it runs in parallel`. Tests in `chief_test.go` with
   rows inserted directly (`store.CreateRunNode` for the fork visit, an `INSERT` into
   `run_branches` through the pool, as the artifacts test inserts rows).

## Rules of the house

- Everything runs in Docker, from this worktree in a throwaway container on the stack's
  network with your own test database:

  ```
  docker run --rm --network yuno_default -v "$PWD:/src" -v yuno_gomod:/go/pkg/mod -v yuno_gocache:/root/.cache/go-build -w /src -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 -e DATABASE_URL='postgres://yuno:yuno@postgres:5432/yuno_test_t?sslmode=disable' yuno-go-tools:1.26 sh -c 'go test ./internal/seed/ ./internal/tools/ ./internal/httpapi/ -run "Template|Seed|Shipped|RunStatus|Reset" -count=1 2>&1 | tail -60'
  ```

  vet with `go vet ./...` and `gofmt -l .` in the same container. Write long outputs to
  a file under `/tmp` and read the file. Never run `make run`, `make down`, `make
  clean`, `make test-*`, `make lint` or `docker compose ...` from this directory: a
  stack is running and must not be restarted; never run the whole suite: the
  coordinator does.
- Files you own: `templates/review_panel.json`; `internal/seed/seed.go` and
  `internal/seed/*_test.go`; `internal/tools/chief.go` and `chief_test.go`; a new
  `internal/httpapi/templates_review_panel_test.go` if `templates_test.go` needs a case
  (do not edit `templates_test.go` itself: the coordinator may). Nothing else:
  `internal/workflow`, `internal/store`, `internal/engine`, `web/`, `docs/`,
  `api/openapi.yaml` are other hands' or frozen. If a frozen type or the validation
  refuses your template for a reason you believe wrong, say so in your report with the
  message; do not edit the validation.
- Rule texts are exact: copy them from 5.26, do not paraphrase. Prompts and tasks are
  for the model and the operator: no developer notes.
- Commit nothing; the coordinator commits. Report: the files changed, the test
  output's last lines, every rule you had to interpret, and anything you found wrong
  in the design.
