# Wave 1 part B notes: audit, idempotency and versions

Branch `w1-audit`. Text for the coordinator to fold into the docs, then every rule I had to
interpret.

## DESIGN 5.31 Audit, idempotency and versions (new section)

The rules are DESIGN wave 1 B1 to B6, numbered 1 to 6 here; the code cites them as
"DESIGN 5.31 rule N".

1. **Audit.** Every `/api` request with method POST, PUT, PATCH or DELETE, and every `/api`
   request refused by auth, gets one row in `audit_events` (migration 0027). The row is
   inserted before the handler runs, with outcome `started`, and completed after; when the
   insert fails the request is refused 503 `audit_unavailable` and does not run. Columns as
   in the wave design. `route` is the mux pattern without its method
   (`/api/agents/{id}`), `path` the escaped request path. `outcome`: `ok` (below 400),
   `denied` (401, 403, 412), `failed` (any other 4xx or 5xx), or what an inner layer wrote
   into the request's `actor.Note` (`pending_approval`, `replayed`, `denied`); the note's
   error code, approval id and evidence (redacted) win over what the audit reads from the
   response. A handler that panics completes its row as a 500.
   - `request`: the redacted body, at most 16 KiB. `before`/`after`: at most 64 KiB each.
     A longer redacted document is kept as a JSON string cut with `obs.Truncate`'s marker,
     so the column is always valid JSON and within the cap. A multipart or non-UTF-8 body
     is kept as `{content_type, bytes, sha256}`. A refused request's body is not read.
2. **Before and after.** `target` is the path when a GET route has the same pattern, else
   the path without its last segment when that has a GET route of the pattern without its
   last segment, else "". With a target the audit reads it in-process (a GET through the
   routes, no middleware) before and after the handler. `before`/`after` are null when the
   target does not exist (404) and `{"error": "reading <target> answered <status> <code>"}`
   when the read fails. A POST to a collection (a target whose pattern ends in a literal
   segment: `/api/agents`, `/api/workflows`) has no before, and its after is the response
   body; so has a request with no target. A refused request reads nothing.
3. **Idempotency.** A POST, PUT, PATCH or DELETE under `/api` with an `Idempotency-Key`
   (1 to 200 printable ASCII characters, else 422 `validation_failed` on
   `Idempotency-Key`) is keyed by actor (`operator`, or `agent:<id>`), method and escaped
   path in `idempotency_keys`, with the SHA-256 of its body.
   - First use runs the handler; a response below 500 is kept (status, body up to 1 MiB,
     Content-Type, ETag, Location). A 5xx, or a panic, deletes the claim so the caller can
     retry.
   - Same key and body: the kept response with `Idempotency-Replayed: true`; audit outcome
     `replayed`. A response over 1 MiB is replayed with its status and headers, no body.
   - Same key, other body: 422 `idempotency_key_reused`. Same key while the first runs:
     409 `request_in_progress`. (A different body is checked first.)
   - Keys expire 24 hours after first use: an expired key is free again at once, and the
     retention sweep (every 10 minutes, with the memory, conversation and artifact sweeps)
     deletes expired keys and claims still running an hour after they were made (their
     process died).
4. **Versions.** Every `/api` GET 200 response with Content-Type `application/json` carries
   `ETag: "<hex of the first 8 bytes of the SHA-256 of the body>"`; other GET responses
   (event streams, files, the OpenAPI document, errors) pass through untouched.
   - A PUT, PATCH or DELETE with `If-Match` takes the per-target lock (in this process),
     reads the target in-process and compares. A mismatch answers 412 `version_conflict`
     with `details.current_etag` (the quoted ETag, "" when the target does not exist); a
     match runs the handler holding the lock. `*` matches any existing version; quotes, a
     `W/` prefix and lists are accepted.
   - A request with no target has no version and runs as without `If-Match`.
5. **Audit read.** `GET /api/audit?actor=&target=&outcome=&since=&cursor=&limit=`
   (`listAudit`, risk `read`) returns `{events, next_cursor}`, newest first (`at`, then
   id). `actor` matches the actor kind or the agent id; `target` the target exactly;
   `since` (RFC 3339) is inclusive; `limit` 1 to 200, default 50; the cursor is opaque
   (base64url of the last row's time and id) and `next_cursor` is "" on the last page.
   Bad parameters answer 422.
6. **Order.** recoverer → requestID → logging → auth (judges the bearer) → actor → audit →
   auth's 401 → platform gate → idempotency → If-Match and ETags → routes.

## DECISIONS entry (D-098 audit, idempotency and versions)

D-098: every change and every refused /api request is recorded before it runs and
completed after, and the record fails closed: no audit row, no change. Repeats are made
safe by an Idempotency-Key per actor, method and path, kept 24 hours, and concurrent
edits by `If-Match` against an ETag of the GET representation. Chosen over database
row versions because the ETag needs no schema change per resource and is the same for
every route the Chief can call; the cost is a GET of the target before and after each
change, and a per-target lock that holds in one process only (the api is one process).

## CHANGELOG

- Every change made through the API (UI, scripts, the Chief) is recorded in an audit log,
  readable at `GET /api/audit`; requests refused for a bad bearer are recorded too.
- The UI sends an `Idempotency-Key` with every change, and the API replays the first
  answer to a repeated key instead of acting twice.
- Every JSON read under `/api` carries an `ETag`; `If-Match` on a PUT, PATCH or DELETE
  refuses a stale write with 412 `version_conflict`.

## Threat model additions

- Mitigation (B1): every change is attributable to the operator (UI or API), an agent
  (with the person it acted for), or an anonymous caller; the record is written before
  the change and the change is refused when it cannot be written.
- Residual: a scanner without the bearer writes audit rows under `/api` (no rate cap); the
  request body of a refused request is not read or kept.
- Residual: the If-Match lock is per process; a second api process would not share it.

## README

No new settings. The UI sends `X-Yuno-Client: web` on every request and an
`Idempotency-Key` on every change; API clients may send either.

## Rules I had to interpret

1. **Auth refusals inside the audit (B1 and B6).** B6 puts auth before the actor and the
   audit, and B1 wants auth refusals audited. `auth` now only judges the bearer and records
   its verdict in the context; `refuseUnauthorized`, right after the audit, answers the
   401. The chain is otherwise exactly B6.
2. **The operator's actor id.** The operator has no id (`actor_id` ""); idempotency keys
   are scoped by `operator` or `agent:<id>`. The UI and API clients share the operator's
   key space; the UI's keys are UUIDs.
3. **`via` of an anonymous caller** follows the same header as the operator's (ui or api).
4. **Server without a database (tests only).** `Run` always has a pool; a server built
   without one has no audit and no idempotency, so the existing pool-less handler tests
   keep running. With a pool whose insert fails, the request is refused 503.
5. **"Same pattern" for the GET twin and the parent (B2)** compares mux patterns, so
   `POST /api/workflows/validate` is not taken for `GET /api/workflows/{id}`; its parent
   `/api/workflows` is its target, a collection, so it records the response.
6. **POST to a collection (B2)** means any POST whose target's pattern ends in a literal
   segment. Such a POST, and a request with no target, record the response as `after`.
   PUT and DELETE that target a list (an agent file, a project fact, an agent memory fact)
   read the list before and after.
7. **A missing target (404) reads as null**, not `{"error": ...}`: it is the state (gone,
   or not yet created). Other statuses store the error object.
8. **Outcome of statuses below 400** (B1 says `ok` for 2xx): 1xx and 3xx map to `ok`;
   the api answers neither for a change.
9. **A note outcome that is not one of the allowed values** is ignored and the status
   mapping applies, so a bad note cannot make the completion fail the database check.
10. **Over-1-MiB responses (B3)** are kept as done with status and headers but no body,
    and replayed without a body: replaying is the safe direction, running again is not.
11. **Abandoned claims.** A claim still `running` an hour after it was made is deleted by
    the sweep (its process died); until then a repeat gets 409. A claim whose completion
    could not be written also stays running until then, rather than being released.
12. **"Swept with the approval sweeper" (B3).** The approval expiry loop lives in the
    engine; I added the sweep to the server's retention sweeper (`memory.Sweeper.Also`,
    started in `Run` with the other loops) instead, to stay out of the engine.
13. **If-Match without a target** runs as without If-Match (nothing to compare); with a
    target that does not exist it answers 412 with `current_etag` "".
14. **ETag scope.** Only `application/json` 200 GETs under `/api`. JSON is held back to
    hash it; streams and files pass through.
15. **Request body read before the handler** up to 32 MiB (every handler's own limit is
    lower; the skill import's 25 MiB is the highest); the hash and the audit cover what
    was read.
16. **Header text** (X-Request-Id, Idempotency-Key) is made valid UTF-8 without NUL before
    it is stored, so a header byte cannot make the audit insert fail and refuse the request.
17. **`server.go`** also gained the `versionLocks` field on `Server` and the sweep entry in
    `Run`, besides the route: the locks must be shared by every `Handler()` (part A
    dispatches through it).
18. **OpenAPI.** `GET /api/audit` uses the existing `ops` tag (no new top-level tag), and
    the new headers and error codes on existing operations are left to part A, which owns
    them (described in this file instead).
