# Changelog

Behaviour changes operators notice, newest first. The design reasons are in
`DECISIONS.md` and `docs/DESIGN.md`.

## Unreleased

- Agents can run on xAI Grok models with your SuperGrok subscription: sign the gateway in
  once with `docker compose exec openclaw node openclaw.mjs models auth login --provider xai
  --method oauth --agent main`, then pick `xai/grok-4.7`, `xai/grok-4.6` or `xai/grok-4.5`
  under "xAI (your SuperGrok subscription)" in the agent's form.
- The Chief of Staff on OpenAI can fill in what its calls need again: a workflow's input
  when it delegates, and the path, query and body of a platform call. The gateway's strict
  tool schemas had emptied them, as they had `finish`'s result. They now go as JSON text.
- The Plan and Ship pipelines allow more steps and tool calls (Ship: 60 iterations per
  step; Plan: 120 and two hours), so agents on OpenAI models no longer run out of room
  before finishing. Copies of these workflows you have not edited follow on the next deploy.
- Agents on OpenAI models can finish their steps again. The gateway's strict tool schemas
  emptied the result object of `finish`, so every OpenAI step that delivered a result was
  refused once code mode was off. `finish` now takes the result as JSON text, which the
  task message asks for.
- Runs can be paused, resumed with a note for the step, and steered with a message while
  they work; a retry can start from an earlier step. Every cancel, pause, resume, retry and
  steer is tracked from received to applied. A cancel counts as done only when the gateway
  confirms the agent stopped, and a run cannot be retried or resumed until then. New run
  status `paused`, with Pause, Resume and Steer in the run header.
- Work can depend on other work: the Planner marks a ticket's `depends_on`, and you or the
  Chief can make a merge request depend on another. The dependent waits until its
  prerequisite is merged and green on the merge commit; then the platform brings it up to
  date with main, waits for its CI to pass on the new commit, and lets it go by itself. A
  merge request says what holds it (`blocked_by`).
- The Chief of Staff can do anything the UI can, from chat. It looks up the API's actions
  and runs them through the same code the UI uses. Deleting things, granting tools,
  changing a project's repository or token, raising a budget, binding Telegram, merging a
  merge request and cancelling a plan wait for your approval of that exact action: in the
  Approvals page, or by sending the Chief the code it gives you.
- Every change made through the API (UI, scripts, the Chief) is recorded in an audit log
  with who made it, for whom, and the state before and after: `GET /api/audit`. A change is
  refused when its record cannot be written.
- A repeated change with the same `Idempotency-Key` is answered once and not done twice;
  the UI sends one with every change. Reads carry an `ETag`, and `If-Match` refuses a write
  over a newer version.
- Runs say what they are doing: `GET /api/runs/{id}/diagnosis` reports each live step as
  executing, waiting, blocked, unknown or stalled, with the heartbeat, progress, model wait
  and open tool calls behind it. `GET /api/runs/{id}/events/page` pages a run's events.
- Merge requests are checked on their exact head commit; a merge names that commit, and
  `none` is accepted only when the project has no CI. The platform can bring a request up
  to date with the default branch (`update-from-default`), and you can read its diff,
  checks and job logs.
- Workflows keep every version; you can read and restore one. An archived agent can be
  restored. A change to an agent, schedule, skill or project can be undone from its audit
  row (`POST /api/audit/{id}/revert`), unless it changed again since.
- `answer_question` passes an answer on only when your latest message says it or carries
  the question's code.

- A step whose agent read a binary file no longer stays "running" on the run page after
  the run ends.
- Agents on OpenAI models call their tools directly again. The gateway had switched them
  to an experimental "code mode" that made them poll long commands, such as a test suite,
  every 10 seconds, and delayed their calls to Yuno, `finish` included.
- A step whose model stops answering no longer sits "running" for hours. After 10 minutes
  with no reply and no tool running, the turn is stopped and the agent continues where it
  was, up to three times; then the step fails with "model reply stalled for 10 minutes".
  Set `MODEL_STALL_AFTER` to change the wait, or to `0` to turn it off.
- Retrying a step starts it fresh: answers you gave the failed attempt are not replayed.
  Neither are your decisions on its tool approvals. The retried step is told you retried
  it after it failed, and it may ask you its questions again.
- A step interrupted by a platform restart, or retried, gets its checkout back. A step whose
  starting code cannot be found stops before the agent starts and says why, instead of
  running without it.
- Agents can run on OpenAI models with your ChatGPT/Codex subscription: sign the gateway in
  once with `docker exec -it yuno-openclaw openclaw models auth login --provider openai
  --device-code --agent main`, then pick `openai/gpt-6-astra`, `openai/gpt-5.6-terra` or
  `openai/gpt-5.6-luna` under "OpenAI (your ChatGPT subscription)" in the agent's form.
  The costs shown for these calls are estimates at OpenAI's API prices, and budgets count
  them. The shipped agents stay on Claude; `make seed-reset` moves one you switched back to
  its Claude model.
- The Code Reviewer now gives all its feedback in its first review, as numbered findings
  (R1, R2 ...). The Builder fixes each one or says why not, and later rounds only check
  those answers and the bugs you sent back from the manual test, with nothing new added, so
  a ticket no longer runs out of review rounds on fresh requests. The Code Reviewer only
  reads the code now and runs nothing: the Builder runs the whole test suite before every
  hand-over and reports the command and the result.
- Ticket merge requests no longer change the plan's shared documents. A ticket's request
  carries only its own `docs/plans/<feature>/tickets/<id>.md` among them; an edit to
  `PRD.md`, `ARCHITECTURE.md`, `TASKS.md` or `WAVES.md` is left out, and the run is told
  which files it left out. The Doc Auditor writes "as built" notes into the ticket's file,
  and the wave's ship run applies them to the plan documents in the same merge request as
  the lessons, which you approve. While a plan revision runs, no new wave is released and
  no ship run starts for that plan; tickets already building carry on, and the waves go on
  by themselves once the revision ends, even if it failed.
- A conflict the documentation audit finds now asks you and waits; your decision goes back to the Builder instead of ending the ticket.
- Each run step works in its own private workspace: runs no longer see or change each other's folders, finished runs' folders are deleted, and the Python sandbox has libgomp.

- A plan can now have up to 100 tickets in up to 30 waves (it was 50 and 10). A plan over
  either limit is sent back to the Planner with the reason before you are asked to approve
  it, so an approved plan is never refused when its tickets are published. The limits are
  in the README and in DESIGN 5.29.
- The Integrator's merge request descriptions end with a "How it was checked" line. It
  reports what the earlier steps verified (the Builder's tests, the Code Reviewer's
  verdict, your manual test). When the run verified nothing, as in a quick run of the
  Integrator, it says "Not run in this task; the project's CI checks this merge request."
  instead of "NOT VERIFIED".
- The Builder and the Scout now install a project's dependencies from its lockfile, and
  when a new release of an unpinned lint or test tool breaks the project they report it
  and propose pinning the tool in the project, instead of changing unrelated code. The
  sandbox images already pin every tool they install; a test now keeps it that way.
- A draft merge request now says in its description not to merge it until Yuno marks it ready. If you merge it before the run's last push anyway, Yuno opens a small "Follow-up: ..." merge request with the files that were left instead of failing the ticket, and the run's report links it; the follow-up needs your merge too.
- Opening a merge request no longer fails when GitLab or GitHub is slow to show the branch Yuno just pushed: Yuno waits for the branch, and names it if it never appears. The manual test step names the branch to pull only when the draft merge request exists; otherwise it tells you the draft is missing instead of showing a placeholder.
- A plan revision that changes a ticket whose merge request has already merged no longer
  loses that change: it becomes a new follow-up ticket ("Follow-up to T8: ...") with its own
  issue in the current wave, and the approval message names it. The merged ticket and its
  issue stay as they were.
- Runs see the history of their branch and of main. A run that updates a merge request
  starts with main merged into the request's branch: when they conflict, the agent is
  told which files to resolve, and the push carries the merge, so the request can be
  merged without conflicts.

- A merge request now carries only what its run changed. Work that reached main after the
  run started (another ticket's merge) is kept instead of being deleted by the run's
  commit. When the run and main changed the same file, nothing is pushed and the agent is
  told which files conflict, so it can ask you how to resolve them.

