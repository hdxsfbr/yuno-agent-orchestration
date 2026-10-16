# Wave 4 fix pass, frontend (Sonnet)

Read `docs/briefs/wave2/common.md` first (Docker only, never run node/pnpm on the host,
do not touch the running stack, no commits, no new dependencies). You own `web/**`
only; a backend agent edits the Go side in parallel and will add the fields named
below; mock them in tests. Findings come from the independent requirements audit.

## Fixes

1. **API token in the UI (audit #2).** The api accepts `Authorization: Bearer
   <API_TOKEN>` and answers 401 when the token is set and missing or wrong. Add a
   token store (`localStorage`, key `yuno.apiToken`), sent on every `api()` call and
   on the SSE fetch in `useEventStream`; on a 401, open a modal "Sign in" asking for the
   platform token (one password field, Save), retry the failed query after saving; a
   "Sign out" entry in the nav footer clears it; the Status page shows whether a token
   is in use. The token never appears in a URL or in logs. Tests: a 401 opens the
   modal, saving sends the header on the retried request and on the stream, sign out
   removes it.
2. **Remove the inert conversation-window field (audit #9)** from the memory section
   of the agent form; `conversation_window` is not sent any more (the backend ignores
   it). Test updated.
3. **Limits copy (audit #10).** The limits section's "A run stops when it reaches any
   limit" becomes: "A quick run stops when it reaches any of these limits; workflow
   runs use the workflow's limits, plus this agent's cost cap."
4. **Agents list (audit #17, #18).** `GET /api/agents` items gain `channels: string[]`
   and `activity: {running: number, waiting_approval: number}`. Add a Channel column
   ("Telegram" or a dash) and make the State column live: "Running (n)",
   "Waiting (n)" or "Idle", refreshed by subscribing to `/api/events` and
   invalidating the agents query on `run.status` and `node.status` events (throttled
   like the Runs page). Tests.
5. **Status page**: show the api version string from `/api/status` as the commit
   (already there if it renders `version`; make sure it is visible near the top).

## Then

`docker compose --profile tools run --rm --no-deps node pnpm test` and `pnpm lint`
green; mutation-check each rule. Report per common.md.
