# Notes: wave 3, part A (merge request reads, update from default, merge guard)

Branch `w3-mr-reads`. Built: DESIGN wave 3 rules 9 to 15, and the `merge_sha` persistence
named in rules 4 and 13. Not built: rules 1 to 8 (edges, satisfied, gate, watcher, parking).

## For the watcher (rules 8 and 12)

- `(*mergereq.Sync).UpdateFromDefault(ctx, id, expectedHead string) (*store.MergeRequest,
  mergereq.UpdateResult, error)` is the one function the endpoint uses. The watcher passes
  the record's `head_sha`, read fresh. Outcomes are `project.UpdateUpToDate`,
  `UpdateUpdated` and `UpdateConflicts`. Errors are `*project.ErrHeadMoved{Current}`,
  `project.ErrNoMergeBase`, `*mergereq.ErrNotOpen`, `*mergereq.ProviderError{Message,
  Transient}`, the resolver's errors, and database errors.
- The git half is `(project.Config).UpdateFromDefault(ctx, branch, expectedHead)
  (project.BranchUpdate, error)`.
- Rule 4's "no CI configured" project test is `tools.RequestProvider.CIConfigured(ctx,
  branch)`: is there a pipeline or check on any of the branch's last 20 commits. The merge
  guard uses it. The 15-minute window after `merged_at` belongs to rule 4 and is not built.
- The CI of exactly one commit with its jobs is `RequestProvider.Checks(ctx, number, sha)`.
  On GitLab it reads the newest pipeline of the sha on any ref (`gitlabProvider.headPipeline`).
  Rule 4's check of `merge_sha` on the default ref needs `ref=<default>` added: a small
  variant of `headPipeline`.

## Doc text to fold in

**DESIGN (merge requests).** A record's check is the check of exactly one commit.
`ci_sha` is the commit `ci_status` was read for. `ci_pipeline_id` and `ci_url` name the
pipeline (GitLab) or the deciding check (GitHub). `mergeable` is whether the branch merges
without conflicts (null while the provider does not know). `merge_status` is the provider's
own word: GitLab's `detailed_merge_status`, GitHub's `mergeable_state`.

- The sync reads GitLab's pipelines for exactly the head
  (`pipelines?sha=<head>&order_by=id&sort=desc&per_page=1`) and writes `merge_sha` for a
  merged request.
- Every push the platform makes to a recorded request's branch updates the record in one
  write: head and `ci_sha` become the new commit, `ci_status` becomes pending, and the
  pipeline is forgotten. This covers the update from the default branch and an
  `open_merge_request` call on a recorded request. The update from the default branch also
  sets `updated_from_default_at` and emits `merge_request.updated` under the record's run.

**Merge guard.** `POST /api/merge-requests/{id}/merge` needs `ci_status` passed with
`ci_sha = head_sha`. It also accepts `none` on the head of a project with no CI
configured. It sends the head as the provider's `sha`. A head that moved is answered 409
`merge_request_head_moved`; the record is then re-synced, and `details.current` names the
new head.

**New routes.**
- `POST /api/merge-requests/{id}/update-from-default {expected_head_sha}` (routine).
- `GET …/diff`, `GET …/checks` and `GET …/jobs/{job}/log?tail_bytes=` (read). Diffs are
  redacted and capped at 256 KiB. Logs are a redacted tail, 64 KiB by default and 1 MiB at
  most.

**CHANGELOG.**
- Merge requests: the merge is guarded by CI on the exact head commit and names that commit
  to the provider.
- The platform can update a request from the default branch.
- New reads: diff, checks with jobs, and job logs.
- A stale check is invalidated on every platform push.
- `merge_sha` is now recorded by the sync.

**Threat model.**
- The job log read of GitHub follows one redirect to the log's signed address *without*
  the token.
- Job ids are digits only before they reach a URL path.
- The diff and the log pass `obs.Redact` and a literal replacement of the project's token.

## Rules I had to interpret

1. **Error codes.** DESIGN says `ci_not_passed` and `head_moved`. I kept the existing code
   `merge_request_ci_not_passed`, which the web client already knows, and named the new
   one `merge_request_head_moved` in the same family. Other new codes:
   `merge_request_not_updatable`, `merge_request_unreadable` and
   `merge_request_head_unknown`.
2. **GitLab pipelines of the head.** I read `pipelines?sha=<head>` without `ref=<branch>`.
   A detached merge-request pipeline (ref `refs/merge-requests/<iid>/head`) then counts,
   which a project using `workflow: rules` for merge-request events needs. The exact commit
   is the guard.
3. **`none` at merge time.** It is accepted only when `CIConfigured(default branch)` is
   false, read live at merge time. A provider error there is a 502.
4. **Guard order.** The checks run in this order: state, then failed or pending (existing
   text), then the project's token, then head known, then `ci_sha = head_sha`, then `none`
   and CI configured, then the merge with the sha. The provider is asked nothing before the
   CI check.
5. **A provider 409 to a merge means "head moved"** only when the merge sent a sha. Both
   GitLab and GitHub answer 409 for exactly that. A merge without a sha keeps the old
   mapping (not mergeable).
6. **`mergeable` means conflict-free merging.**
   - GitLab: null while `detailed_merge_status` is unchecked, checking, preparing or
     `cannot_be_merged_recheck`. Otherwise it is `!has_conflicts && status != conflict`.
   - GitHub: the PR's `mergeable` as given.
7. **`conflicts.base_sha`** is the default branch's commit the merge tried to bring in.
   Conflicts are cleared by an update that is up to date or pushes, and by any platform
   push.
8. **The tool's push also invalidates the check.** `open_merge_request` pushing onto a
   recorded request writes the same record update as rule 11, without
   `updated_from_default_at` and without an event. This addresses the inventory finding
   that "after a push nothing resets ci_status/head_sha".
9. **What the update merges.** It merges the run's project snapshot `default_branch`. It
   refuses a source branch equal to it. No merge base within 2000 fetched commits gives 409
   `merge_request_not_updatable`.
10. **Conflicts are an outcome, not an error.** The update answers 200 with
    `outcome: conflicts` and the files.
11. **Reads work in every state.** `checks` uses the record's `head_sha`; with none known
    it answers 409 `merge_request_head_unknown`.
12. **The diff cap** counts paths and patches after redaction. The file that crosses the
    cap keeps its whole lines that fit, and later files are left out. Provider paging stops
    at 30 pages of 100, or once 256 KiB of raw patch has been read.
13. **Job ids are strings.** GitHub commit statuses are listed as `status-<id>` and have no
    log (404). GitHub check-run ids are Actions job ids.
14. **Job statuses** are folded to passed, failed, running, pending, canceled, skipped and
    manual. GitHub `neutral` counts as passed.
15. **Log tail.** When the log is cut, the tail starts after its first line break, so no
    line and no credential is cut in two. It is redacted, then re-capped to `tail_bytes`.
16. **`merge_request.updated` is durable.** It is written to `run_events` under the
    record's run, with data `{merge_request}`.
17. **Migration 0030.** Existing records keep `ci_sha` null, so their merge is refused until
    the next sync (at most 120 s) reads the head's own pipeline. There is deliberately no
    backfill from `head_sha`: the old GitLab read was not exact.
18. **`ci_sha` on insert.** `InsertMergeRequest` writes `ci_sha` when given. Only tests
    seed it; the tools never set it.

## Left open

- The update does not refuse while a live run works on the request's branch. That run's
  next guarded push then answers "the branch has new commits". The watcher only updates
  dependents whose runs are parked, which are not live.
- I edited the existing merge operation's description and its 409 line, and the
  `/api/events` description (not their operationId or x-yuno-risk). The builder adding
  operationIds may get a small text conflict there.
- `PARITY.md` names the reads test `TestMergeRequestChecks`. I used DESIGN's name,
  `TestMergeRequestDiffChecksLog`.
