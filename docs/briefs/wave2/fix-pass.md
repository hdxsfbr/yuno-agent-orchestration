# Wave 2 fix pass (one agent, Opus)

Read `docs/briefs/wave2/common.md` first (environment rules: Docker only, do not touch
the running stack, no commits, no new dependencies), then `docs/reviews/wave2-review.md`
(the findings you fix, with their dispositions), then DESIGN 5.3, 5.6 (rule 13 is new)
and the engine files named below. You are the only agent working now, so you may edit
any file; keep each fix minimal and covered by a test that fails without it (break the
fix and confirm, then restore).

## Fixes, in this order

1. **Run directory copy-forward (review #2, DESIGN 5.6 rule 13, D-019).** In
   `internal/engine/walk.go` before an agent node visit starts, copy `runs/<run_id>/`
   from the workspace of every earlier visit's agent of this run, in visit order (a later
   visit's file wins), into this visit's agent workspace, through the state directory
   (`Deps.StateDir`, workspace `workspace-<openclaw agent id>`; see how `prepareRunDir`
   finds it). Same agent twice: nothing to copy. Copy files and directories, never follow
   symlinks (skip them), preserve nothing special; a failure fails the node with reason
   `run directory: <error>`. Test T5.6.12: two agents, node 1 writes a file (the fake
   gateway can write into the workspace directly), node 2 sees it; a file rewritten by
   node 2 wins on node 3.
2. **Deploy packaging symlink escape (review #1).** `internal/tools/package.go`: open
   the workspace with `os.OpenRoot`, then the run directory *through* that root, and
   refuse any entry that is a symlink or resolves outside; add the reviewer's
   reproduction as a test (a `runs/<run_id>` symlink to a temp dir outside the
   workspace must be rejected, and a symlinked file inside must be skipped or
   rejected).
3. **Approval recovery bound to the visit (review #3)** and **publish-then-pause race
   (review #4)** in `walk.go`, and **unblock on the early-decision path (review #5)** in
   `approvals.go`. Read agent A's own notes in the file comments first. For #4 the
   simplest correct order is: create the approval row and move the run to
   `waiting_approval` in one step, then publish; `DecideApproval` must find the run
   paused. For #3 persist which visit is being resumed (the existing
   `resume_node_visit_id`) and treat a gate reached again by the walk as a new entry.
   Each with a test that reproduces the race deterministically (inject the decision
   between the two steps through the fake gateway or a hook).
4. **Empty condition groups (review #7).** `internal/workflow/cond.go` /
   `def.go`: reject `{"all": []}` and `{"any": []}` at validation with
   `edges[i].when: empty all/any group`, and make `Evaluate` treat them the same way the
   validator does. Table test.
5. **Test race (review #8).** `approvals_test.go` `TestDecisionAfterRestartRequeues`:
   join the gated-call goroutine before swapping the engine. Run the engine package with
   `-race` once and keep it clean.
6. **Memory defaults (review #9).** `internal/tools/memory.go` (and store): `max_facts`
   default 200, recall returns up to 20, per DESIGN 5.10 rules 3–4; adjust tests.
7. **Builder rename (review #6).** `web/src/lib/workflow.ts`: on node rename, rewrite
   condition `path` values that start with `/<old id>` and `.state.<old id>` references
   in task and approval message templates to the new id; the node panel shows a one-line
   note that references were updated. Unit test in `lib/workflow.test.ts` (or the
   existing test file) and keep the builder tests green.

## Then

Run, in Docker, and paste the results in your report:
`docker compose --profile tools run --rm --no-deps go sh -c 'gofmt -l ./internal ./cmd; go vet ./... && go test ./... -count=1'`,
the engine package once with `-race`, `docker compose --profile tools run --rm node pnpm test`
and `pnpm lint`. Do not run the live integration tests, probes, the stack or `make eval`;
the coordinator does.

## Report

What changed per finding, the test that covers it, the mutation check, anything you
could not fix and why, and any rule you had to interpret.
