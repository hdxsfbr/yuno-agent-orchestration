# Wave 10 frontend brief D: the Question card and the Artifacts section

Spec: `docs/DESIGN.md` 5.24 rule 12 (the UI part) and 5.25 rule 7, with their edge
cases; `api/openapi.yaml` 0.7.0: `Approval.kind` value `question`, `Approval.options`,
`Approval.answer`, `POST /api/approvals/{id}/answer` (`AnswerBody`), `RunNode.question_id`,
`Artifact`, `GET /api/runs/{id}/artifacts`, `GET /api/runs/{id}/artifacts/{artifactId}`.
Read `docs/briefs/wave10/00-plan.md` first. The backend lands at the same time, so
build against the contract and `msw`-style fakes as the existing tests do; the
coordinator runs the UI against the real API at integration.

Already in the tree and worth reading before you write: `web/src/lib/api.ts` (`Approval`,
`ApprovalItem`, `approveApproval`, `rejectApproval`, `api<T>`, `withBody`, `seg`, how
the bearer token is attached), `web/src/lib/useApprovals.ts` (the lists, the cache
update after a decision, the SSE handling), `web/src/components/approval/ApprovalCard.tsx`
(the card you extend: kind labels, the reason box, the decision mutation, the 409
handling), `web/src/components/run/ApprovalsSection.tsx`, `web/src/components/run/RunHeader.tsx`
(the header lines: `Updates merge request !n`, the issue link), `web/src/components/run/Timeline.tsx`
(`ApprovalRow`), `web/src/pages/RunDetailPage.tsx` (the sections and their order),
`web/src/pages/ApprovalsPage.tsx`, `web/src/lib/useRunTrace.ts` and `web/src/lib/trace.ts`
(the run view and how SSE events refresh it), `web/src/lib/useEventStream.ts`,
`web/src/lib/format.ts` (sizes and dates; add `formatBytes` if none),
`web/src/components/run/MergeRequestCard.tsx` and its test (a section with its own
query and mutations, the pattern for Artifacts), `web/src/components/QueryState.tsx`.

## Deliverables

1. **API client** (`api.ts`): `Approval.kind` gains `"question"`; `Approval` gains
   `options?: string[]` and `answer?: string`; `RunNode` gains `question_id?: string`;
   `answerApproval(id, { answer })` → `POST /api/approvals/${id}/answer`; `Artifact`
   type as the contract; `listRunArtifacts(runId)`; `downloadArtifact(runId,
   artifactId): Promise<Blob>` (a `fetch` with the token header, `res.blob()`, an
   `ApiError` on a non-200); `artifactDownloadUrl` is not needed (links carry no
   header). SSE: `run.artifacts` handled in `useRunTrace` or a small
   `useRunArtifacts(runId)` hook that invalidates its query on that event for the run.
2. **The Question card (5.24 rule 12).** `ApprovalCard`: kind label `Question` for
   `question`; for a pending question: the question text (pre-wrap), one button per
   option (click → `answerApproval` with that option), a text box with an "Answer"
   button (disabled while empty; 1 to 4096 characters, the server's 422 detail shown
   inline), and "Decline" with the existing optional reason box (→ `rejectApproval`);
   no Approve or Approve-always button for a question. A decided question shows the
   answer (`Answered: <answer>`), a declined one `Declined<: <reason>>`, an expired
   one `Not answered in time`. The 409 handling stays (`alreadyDecided`). The inbox's
   pending count already includes every pending row; verify with a test that a
   question is counted. The run page's Approvals section uses the same card.
3. **The header line and the Timeline.** `RunHeader`: while `run.status ===
   "waiting_approval"` and the trace holds a pending approval of kind `question`, a
   line `Asks: <first 120 characters of the question>` linking to `#approvals` (give the
   Approvals section that id). `Timeline`'s `ApprovalRow`: the badge reads `Question`
   for the kind and the row shows the answer under the message when decided. A
   continuation visit (`node.question_id` set) is labelled `visit n, continued after a
   question` where visit numbers are shown (the node group title), so the operator
   sees why there are two visits.
4. **The Artifacts section (5.25 rule 7).** `web/src/components/run/ArtifactsSection.tsx`,
   rendered in `RunDetailPage` after the Merge request card and before Approvals, with
   its own query (`listRunArtifacts`): a table of path, size (`formatBytes`), `<node>
   visit <n>`, and `updated_at` as a relative time; per row a Download button (fetches
   the blob, creates an object URL, clicks a hidden `<a download="<base name>">`,
   revokes the URL) and, when `content_type` starts with `text/` or is
   `application/json` or `text/x-patch` and `size <= 262144`, a Preview toggle that
   fetches the blob, reads it as text and shows it in a `<pre>` (never `innerHTML`;
   `image/svg+xml` previews as text), or, for `image/png`, `image/jpeg`, `image/gif`,
   `image/webp` up to 256 KiB, an `<img>` from an object URL with the path as alt text;
   larger or other files show Download only. Empty states: `No files delivered yet.`
   while the run is not terminal, `No files were delivered.` when it is; a query error
   shows the existing inline error. The section refreshes on SSE `run.artifacts` for
   this run. Accessibility: the table has a caption, the buttons name the file
   (`Download report.md`, `Preview report.md`).
5. **Tests** (vitest + testing-library, as the existing component tests): the Question
   card's three states and its actions (an option click sends that option; the text
   box sends its text; Decline sends reject; a 422 detail is shown; a 409 shows the
   decided note); the header line and the Timeline label; the Artifacts section (rows,
   Download creates and revokes an object URL with the file name, Preview for a text
   and an image, no Preview for a 300 KiB text or a zip, the two empty states, the
   refresh on `run.artifacts`); `api.test.ts` for the new calls; `pnpm lint` clean.

## Rules of the house

- Everything runs in Docker, from this worktree in a throwaway container:

  ```
  docker run --rm -u "$(id -u):$(id -g)" -v "$PWD/web:/app" -w /app -e HOME=/tmp -e COREPACK_ENABLE_DOWNLOAD_PROMPT=0 -e COREPACK_HOME=/tmp/corepack -e npm_config_cache=/tmp/npm-cache node:24-slim sh -c 'mkdir -p /tmp/corepack-bin /tmp/corepack && corepack enable --install-directory /tmp/corepack-bin && export PATH=/tmp/corepack-bin:$PATH && pnpm test && pnpm lint'
  ```

  Write long outputs to a file under `/tmp` and read the file. Never run `make run`,
  `make down`, `make clean`, `make test-*`, `make lint` or `docker compose ...` from
  this directory: a stack is running and must not be restarted.
- Files you own: `web/src/**`. Nothing outside `web/`: the contract, the Go code and the
  docs are other hands'. If the contract lacks something you need, say so in your
  report; do not invent a field.
- Product copy is for the operator: the texts above and the design's, no developer
  notes, no placeholders, no "coming soon". Match the existing components (`Button`,
  `Badge`, `Card`, `fields`), dark mode included.
- Commit nothing; the coordinator commits. Report: the files changed, the test
  output's last lines, every rule you had to interpret, and anything you found wrong
  in the design.
