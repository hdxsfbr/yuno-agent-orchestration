# Wave 11 engine brief E: the fork and join at run time (coordinator)

Spec: `docs/DESIGN.md` 5.26 rules 3 to 12 with the edge cases, Failure handling and
Consistency; the amendments to 5.2, 5.6 rule 13, 5.21 rule 4, 5.24 rule 5 and 5.25
rule 3; decisions D-048, D-060, D-061 and D-063. Built by the coordinator after the
prerequisites, alongside F and T.

Order of work, each step with its tests before the next:

1. **Store** (`internal/store/branches.go`, `engine_queries.go`): `CreateForkVisit(ctx,
   db, forkRow *RunNode, branches []RunBranch)` in one transaction; `SetBranchCurrent`,
   `FinishBranch(id, status, reason, at)`, `ReopenBranch`, `ReopenRunNode` (the fork's
   row on re-entry), `SetRunStateNodeInBranch(runID, nodeID, output, branchID,
   branchCurrent)` (a `jsonb_set` on `runs.state` and the branch's `current_node` in one
   transaction, `runs.current_node` untouched), `CloseOpenBranches(runID, status,
   reason, at)`, `ParkBranchVisit` (the park of 5.24 without the run's pause),
   `RequeueParkedRun`, `IsParkedOn` and `ListParkedQuestions` accepting the fork visit
   (5.26 rule 8).
2. **The walk** (`internal/engine/fork.go`, `walk.go`): `KindFork` in `execute` →
   `forkNode`; a `branchWalk` (the `walk` with a `branch *store.RunBranch` and the fork's
   span as its visits' parent) whose `setCurrent` and `advance` write the branch;
   `forkNode` creates or reopens the fork visit, decides what runs (rule 9), runs the
   branches in goroutines, collects their outcomes, stops siblings on the first failure
   (rule 6), joins (rule 5) or releases (rule 8). `e.active` becomes a set per run.
3. **Visits** (`node.go`): `visit.branch`; `pause`/`resumeIfClear`/`finish` leave the run
   alone for a branch visit (rule 7); `questionTurnEnd` parks through `ParkBranchVisit`
   for one (rule 8); `newVisit` sets `branch_id`.
4. **Files** (`openclaw/rundir.go`, `walk.carrySources`): `CopyRunDir` gains a `skipRepo`
   option; `carrySources` returns sources with the flag and excludes siblings (rule 10).
5. **Artifacts** (`wave10.go`, `artifacts`): `ArtifactInput.SkipDiff` (rule 11).
6. **Terminal paths** (`run.go`, `approvals.go`): `finishRun`, `closeWaitingRun` and
   `failRun` close open branch rows (rule 12); `Cancel` and `requeueIfUnattended` over
   the active set.
7. **API** (`httpapi`): `TestOpenAPIConformance` steps for the trace with branches and
   the documented fields; markers off.
8. **Tests**: T5.26.2 to T5.26.11 in `internal/engine/wave11_test.go` and the store and
   workflow tests, with the fake gateway (two session keys at once).
