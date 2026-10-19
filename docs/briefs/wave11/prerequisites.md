# Wave 11 prerequisites: what is landed and frozen before the agents start

The coordinator lands these as one commit (the "Prerequisites" of
`docs/briefs/wave11/00-plan.md`). Spec: `docs/DESIGN.md` 5.26 rules 1, 2, 3 and 12;
decisions D-048 and D-060. Nothing here runs a branch: it adds the table, the types,
the two node kinds with their validation and the trace's branch list, so that the
engine, the frontend and the template agent build on the same ground.

1. **Migration** `internal/store/migrations/0015_wave11.sql`, commented like 0014:
   ```sql
   CREATE TABLE run_branches (
       id             TEXT PRIMARY KEY,
       run_id         TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
       fork_visit_id  TEXT NOT NULL REFERENCES run_nodes (id) ON DELETE CASCADE,
       fork_id        TEXT NOT NULL,
       head           TEXT NOT NULL,
       status         TEXT NOT NULL,            -- running | waiting_approval | succeeded | failed | cancelled
       current_node   TEXT NOT NULL,
       failure_reason TEXT,
       started_at     TIMESTAMPTZ NOT NULL,
       finished_at    TIMESTAMPTZ,
       UNIQUE (fork_visit_id, head)
   );
   CREATE INDEX run_branches_run_idx ON run_branches (run_id, started_at);
   ALTER TABLE run_nodes ADD COLUMN branch_id TEXT REFERENCES run_branches (id) ON DELETE SET NULL;
   CREATE INDEX run_nodes_branch_idx ON run_nodes (branch_id) WHERE branch_id IS NOT NULL;
   ```
2. **Types.** `store.RunBranch` (`db` and `json` names as the columns and the contract),
   `BranchRunning`, `BranchWaitingApproval`, `BranchSucceeded`, `BranchFailed`,
   `BranchCancelled`; `RunNode.BranchID *string` (`db:"branch_id"
   json:"branch_id,omitempty"`) in `runNodeColumns`, `CreateRunNode` and
   `UpdateRunNodeProgress`; `Trace.Branches []RunBranch` (`json:"branches"`, `[]` not
   null), filled by `GetTrace` through `ListRunBranches`. `internal/store/branches.go`:
   `ListRunBranches(ctx, q, runID)` by `started_at, id`, `ListForkBranches(ctx, q,
   forkVisitID)` the same order. A store test round-trips a branch row and a node with
   `branch_id`; the migration upgrade test pattern of `wave10_test.go` is followed for
   0015.
3. **The kinds and the validation.** `workflow.KindFork`, `workflow.KindJoin`,
   `Node.Join`, `Node.Checkout`, `MaxBranches = 5`. `ValidateStructure` accepts the two
   kinds (a fork needs no task or agent) and runs `validateForks` (new file `fork.go`)
   with the exact messages of 5.26 rule 2, path-qualified (`nodes[i]...`, `edges[k]...`).
   Exported helpers the engine uses: `Region(forkID) (*ForkRegion, error)` with `Join
   string`, `Heads []string` (edge order), `Members map[string]string` (node id → head)
   and `Edges []int` (the fork's edge indexes); `ForkOf(joinID) (string, bool)`;
   `Node.IsFork()`/`IsJoin()`. `Next` is unchanged. `fork_test.go`: T5.26.1, one case
   per message, plus the valid shapes (a loop inside a branch, a loop around the pair,
   the same agent before and inside, the review panel's shape).
4. **Contract.** `api/openapi.yaml` 0.8.0 with the `x-planned: wave 11` markers (done
   with the design).
