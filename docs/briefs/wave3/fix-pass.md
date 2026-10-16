# Wave 3 fix pass (one agent, Opus)

Read `docs/briefs/wave2/common.md` (environment rules: Docker only, do not touch the
running stack, never send Telegram messages or patch bindings on the live gateway, no
commits, no new dependencies), then `docs/reviews/wave3-review.md` (what you fix, with
dispositions), then DESIGN 5.8, 5.9, 5.10 rule 6, 5.11 and the files named below. You
are the only agent editing now; keep each fix minimal and covered by a test that fails
without it (break the fix, confirm, restore; `scripts/mutation-check.sh` helps).

## Fixes, in this order

1. **Scheduler occurrence key (review #1).** `internal/sched/scheduler.go`,
   `internal/store/schedules.go`, a migration `0007_schedule_occurrences.sql` adding a
   unique index on `runs (trigger, trigger_ref) WHERE trigger = 'schedule'`. The tick
   triggers with `TriggerRef = "<schedule id>@<occurrence RFC3339 UTC>"`; on a unique
   violation from `CreateRun` (surface it from the engine as a typed error, e.g.
   `engine.ErrDuplicateTrigger`, in `internal/engine/trigger.go`/`engine.go`), the tick
   treats the occurrence as fired, sets `last_error` empty and advances. Test: a tick
   whose schedule update is rolled back after the run was created fires again and
   creates no second run (T5.9.7).
2. **Exec patterns force approvals on host exec (review #2).**
   `internal/guardrails/tools.go` `ExecPolicy`: when the agent has any exec pattern,
   return `security: allowlist`, `ask: on-miss`, `askFallback: deny` with an **empty**
   allowlist, so every host command raises an exec approval; the engine's auto-deny
   (`node.go`, `guardrails.MatchCommand`) then denies matches and the rest reach the
   inbox. Table test including `*`, `rm *`, and a pattern-free agent (unchanged).
3. **Cron with no future occurrence (review #3).** `internal/sched/sched.go`
   `ValidateCron`/`NextRun`: a zero next time is an error (`no future occurrence`),
   422 at the API with path `cron_expr`; at tick time it disables the schedule with
   `last_error`. Tests for both.
4. **Binding race (review #4).** `internal/store/channels.go` and migration 0007: unique
   index on `channel_bindings (channel)`; `BindChannel` inserts and maps the unique
   violation to `ErrChannelBound`; it deletes an existing row first only when that
   row's agent no longer exists. Test: two goroutines bind different agents, exactly one
   succeeds and the other gets `ErrChannelBound`; the winner's row survives.
5. **Retention on replay and reads (review #5).** `internal/engine/conversations.go`
   and `internal/store/channels.go`: when persisting replayed history, skip entries
   older than the agent's `conversation_ttl_days` (default 7); `ListConversationMessages`
   filters them too. Tests with a fixture older than the TTL.
6. **Catch-up retry (review #6).** `conversations.go`: a failed `chat.history` leaves the
   session in an `unsynced` set that the existing ticker retries with backoff until it
   succeeds; a roster event for an unsynced session also retries. Test: the fake gateway
   fails history once, the message appears after the retry.
7. **OpenAPI (review #7).** `api/openapi.yaml`: `GET /api/channels`, `PUT`/`DELETE
   /api/channels/telegram/binding`, `GET /api/conversations`, `GET
   /api/conversations/{id}/messages`, schemas `Channels`, `Conversation`,
   `ConversationMessage`, `Approval.conversation_id`, event `conversation.message`;
   extend the conformance test to exercise them.
8. **Status scheduler fields (review #8).** `/api/status` gains `scheduler:
   {tick_seconds, next_due_at|null}` (read from the schedules table); OpenAPI updated.
10. **Eval runner: empty error text on a 429 re-trigger (first live eval).** Case
   `019-rate-limit` failed with `extra trigger answered "", which does not contain
   "rate limit: 1 runs/hour"` although the api answered 429 with
   `{error: {code: "rate_limited", message: "rate limit: 1 runs/hour for agent …"}}`
   (confirmed in the api log). Fix the capture in `internal/evals` (the `retrigger`
   action / `decodeAPIError` path) and cover it with a fake-api test that answers 429
   with that body.

9. **Dead store functions (review #9).** Remove `store.SaveMemoryFact` and
   `store.RecallMemoryFacts` and their test if nothing else uses them.

## Then

Run, in Docker, and paste the results: `docker compose --profile tools run --rm --no-deps go sh -c 'gofmt -l ./internal ./cmd; go vet ./... && go test ./... -count=1'`,
`make test-race`, `docker compose --profile tools run --rm --no-deps node pnpm test` and
`pnpm lint`. Do not run the live integration tests, probes, the stack or `make eval`;
the coordinator does.

## Report

What changed per finding, the test that covers it, the mutation check, anything you
could not fix and why, and any rule you had to interpret.
