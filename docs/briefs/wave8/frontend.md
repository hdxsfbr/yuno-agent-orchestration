# Wave 8 frontend brief D: project page, budgets, merge request card, retry

Spec: `docs/DESIGN.md` 5.18 rule 7, 5.19 rule 7, 5.20 rule 9, 5.21 rule 7, and
`api/openapi.yaml` 0.5.0 (the contract: `Project.instructions`, `max_cost_usd_per_day`,
`max_cost_usd_per_month`, `spend` (`ProjectSpend`), the same inputs on `ProjectInput`,
`ProjectFact` with `GET /api/projects/{id}/facts` and `DELETE .../facts/{factId}`,
`MergeRequest` with `GET /api/merge-requests`, `POST /api/merge-requests/{id}/sync` and
`POST /api/merge-requests/{id}/merge`, `Run.merge_request`, `Run.retries`, `POST
/api/runs/{id}/retry` with `RunRetry` and its 409 codes `run_not_retryable`,
`retry_limit`, `limits_exhausted` (details `[{path, message}]` naming each exhausted
limit)). The backend is other agents' work; code against the contract. The retry's
boundary reaches the page as the SSE event `run.retry` with `{retries, from_node, by,
at}` on the run's event stream (replayed on load like log lines).

Already in the tree and worth reading before you write: `web/src/pages/ProjectsPage.tsx`
(the list; `/projects/:id` opens the edit dialog today), `web/src/components/project/ProjectDialog.tsx`,
`web/src/lib/useProjects.ts`, `web/src/lib/api.ts` (`Project`, `Run`, `mergeRequestOf`,
`cancelRun`), `web/src/lib/trace.ts` (`RunView`, `applyEvent`: how SSE events become
view state), `web/src/components/run/{RunHeader.tsx,Timeline.tsx,ApprovalsSection.tsx}`,
`web/src/pages/RunDetailPage.tsx`, `web/src/pages/AgentFormPage.tsx` (the saved-facts
list with delete is the pattern for Notes), `web/src/test/{fakeApi.ts,fixtures.ts}`.

## Deliverables

1. `web/src/lib/api.ts`: the new fields and types (`ProjectSpend`, `ProjectFact`,
   `MergeRequest`, `Run.merge_request?`, `Run.retries?`, `ProjectInput.instructions?`,
   `max_cost_usd_per_day?: number | null`, `max_cost_usd_per_month?: number | null`);
   functions `listProjectFacts`, `deleteProjectFact`, `listMergeRequests({project_id?,
   run_id?, state?, limit?})`, `syncMergeRequest`, `mergeMergeRequest`, `retryRun(id,
   limits?)`. Delete `mergeRequestOf` and read `run.merge_request` instead (the backend
   backfills old runs). `trace.ts`: handle `run.retry` into `view.retries: RetryMark[]`
   (deduped by `retries`).
2. **Project form** (`ProjectDialog.tsx`): an Instructions textarea (hint: "Standing
   instructions for every run on this project: conventions, how to build and test, what
   to leave alone."; client check at 8192 characters as a first guard, the server's 422
   detail maps onto the field) and two budget fields under one "Budget" label, "Per day
   (USD)" and "Per month (USD)" (hint: "Leave empty for no cap. New runs are refused
   once the cap is reached; running runs finish."; empty sends `null`; a non-number or
   a value at or below 0 is a client error; server 422 details map onto the fields).
3. **Projects list** (`ProjectsPage.tsx`): the name links to `/projects/<id>`; a "Spend"
   column showing `$<day> / $<cap>` today and `$<month> / $<cap>` this month when a cap
   is set (warning tone from 80 %, danger tone at the cap; without a cap just the
   amounts, muted), two decimals; the row's Edit, Check and Delete stay.
4. **Project page**, new `web/src/pages/ProjectPage.tsx` at `/projects/:id` (an unknown id
   leads back to the list as today): a header with the name, repository and provider; a
   Settings card (branch, network badge, description, Edit opening the dialog, Check with
   the result line as on the list, Delete with the same confirmation); a Budget card
   (today's and this month's spend over the caps with the window end as "resets at
   <time>"; "No cap set." otherwise); an Instructions card (the text, or "No instructions
   yet."); a Notes card (key, value, who saved it, when, a delete button with a
   confirmation; "No notes yet. Agents save what they learn about the project while they
   work on it."); a Merge requests table (number linking to the provider, title, run
   link, state badge, CI badge, unresolved threads, updated; "None yet."); the project's
   10 newest runs (status, created, link; from `listRuns` filtered client-side by
   `project_id` or, if the list is capped, from `/api/runs?limit=200`). Add the route in
   `App.tsx` (the dialog route stays reachable from the page's Edit button).
5. **Merge request card** (`web/src/components/run/MergeRequestCard.tsx`, rendered under
   `RunHeader` on `RunDetailPage` when `run.merge_request` exists): title and number
   linking to the provider ("Merge request !12" on GitLab, "Pull request #12" on
   GitHub), a state badge (Open / Merged / Closed, with the time once final), a CI badge
   (None / Pending / Passed / Failed), "<n> unresolved threads" or "No unresolved
   threads" or "Threads unknown" when null, `last_sync_error` as a muted line, "Synced
   <relative time>", a Sync button (calls sync, replaces the record), and a Merge button
   (primary; disabled with a title when the state is not open or CI is pending or
   failed) that opens `ConfirmDialog` ("Merge !12 into main?" with the title; "The
   request is merged on <provider> and its branch deleted."); a 409 shows the server's
   message in the card. On mount, call sync when `synced_at` is null or older than 30 s.
   `RunHeader` drops its own merge request link in favour of the card.
6. **Retry** (`RunHeader.tsx` and `web/src/components/run/RetryDialog.tsx`): a Retry
   button for a `failed`, `timed_out` or `cancelled` run with `current_node` set, next to
   where Cancel is for a live run; one click calls `retryRun(id)` and applies the
   returned run (`onRunChange`); a 409 `limits_exhausted` opens the dialog listing each
   exhausted limit from the details, with numeric fields prefilled from `run.limits`
   for `max_steps`, `max_tool_calls`, `max_cost_usd` (only the exhausted ones are
   required to rise; a hint says "Raise the limits that are used up to retry from
   where the run stopped."), and Retry sends `{limits}`; a 409 `retry_limit` or
   `run_not_retryable` shows the message inline. A "Retried <n>×" badge in the header
   when `retries > 0`. `Timeline.tsx`: a marker row "Retry <n> from <node>" at the
   event's time from `view.retries`.
7. **Tests** (vitest, `installFakeApi` and fixtures): `ProjectDialog`/`ProjectsPage`
   (fields round-trip, null for an empty cap, the spend column tones, the name link),
   `ProjectPage.test.tsx` (every card with data and empty, note delete, merge request
   table), `MergeRequestCard.test.tsx` (badges per state and CI, Merge disabled cases,
   the confirmation then POST, a 409 message, Sync), `RunHeader`/`RetryDialog` tests
   (button per status, the dialog from a 409 with details, the body sent, the badge),
   `Timeline` marker, `trace.test.ts` for `run.retry`. Keep every existing test green or
   update it where the contract changed (`RunDetailPage*.test.tsx` for the removed
   header link, `ProjectsPage.test.tsx`).
8. `make test-web` and `make lint` clean (eslint, tsc).

## Rules of the house

- Everything runs in Docker through the Makefile (`make test-web`, `make lint`; a
  single file with `docker compose --profile tools run --rm node pnpm vitest run
  src/pages/ProjectPage.test.tsx`). Nothing installed on the host. Never run `make run`,
  `make down` or `make clean`: a stack is running and must not be restarted.
- Touch only `web/`. Do not edit `internal/`, `api/`, `docs/`, the Makefile or compose.
- No developer commentary in product copy: labels and hints are for the end user, never
  mention design rules, waves, stubs or the backend. Money shows with two decimals and a
  dollar sign; times as the rest of the UI shows them.
- Match the existing components (`Badge`, `Card`, `Modal`, `ConfirmDialog`, `fields`,
  `QueryState`, `Button`) and the pages' patterns (`SkillsPage`, `AgentFormPage` for the
  facts list, `RunsPage`).
- Report any rule you had to interpret, and anything you left out.
