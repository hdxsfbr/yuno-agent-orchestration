# Wave 4 fix pass, backend (Opus)

Read `docs/briefs/wave2/common.md` first (environment rules: Docker only, never run
go/node on the host, do not restart or rebuild the running stack, never send Telegram
messages, no commits, no new dependencies). You own everything except `web/**`; another
agent edits `web/**` in parallel. The findings come from the independent requirements
audit (its numbers below); the design is `docs/DESIGN.md`. Every fix gets a test that
fails without it (mutation-check with `scripts/mutation-check.sh`). Keep each change
minimal and in the style of the surrounding code. Do not touch `.env` (the live
secrets) or `data/`.

## Fixes

1. **Agent deletion reaches the runtime (audit #5, root cause of the gateway's heap
   crash: 133 leftover agents).** `DELETE /api/agents/{id}` soft-deletes only
   (`internal/store/agents.go`); after the soft delete the engine must delete the agent
   in OpenClaw (`openclaw.AgentsDelete`, `internal/openclaw/rpc.go`) and remove its
   workspace directory under the state dir (`workspace-<openclaw id>`), logging but not
   failing the request when the gateway refuses. Add a reconciliation at api start,
   after the gateway connects (next to the materialisation keeper you add in item 2):
   every `agents.entries` id of the gateway that belongs to a soft-deleted platform
   agent, or to no platform agent at all except the baseline `main`, is deleted the
   same way, with a counted log line. Tests with the fake gateway: delete calls the
   RPC and removes the directory; the start reconciliation deletes stale entries and
   keeps live ones and `main`.
2. **Materialise at start (demo pre-flight: five seeded agents show "Not
   materialised").** `yuno seed` writes to the database only. At api start, after the
   gateway connects, materialise every non-deleted agent whose `materialized_hash`
   differs from the desired hash (the engine's `materialize` is idempotent), with a
   counted log line; a failure for one agent is logged and the loop continues. Test.
3. **Webhook mode is refused unless it can be verified (audit #1).** In
   `internal/openclaw/baseline.go` / `WriteBaseline`: `TELEGRAM_MODE=webhook` requires
   `PUBLIC_BASE_URL` starting with `https://` and `TELEGRAM_WEBHOOK_SECRET` of at least
   32 characters; otherwise `openclaw-init` exits with a clear error naming the missing
   piece, and nothing is written. `scripts/smoke.sh`: in webhook mode the wrong-secret
   request must get exactly 401 or 403 (not any non-200), and a second check calls
   Telegram's `getWebhookInfo` with the token from `.env` (never printed) and asserts
   the registered URL equals `PUBLIC_BASE_URL/telegram-webhook`. Unit tests for the
   validation; the smoke change is reviewed by the coordinator live.
4. **Chat agents get fresh memory and chat instructions (audit #8).** `MEMORY.md` is
   re-rendered for an agent after every `memory_save` (`internal/tools/memory.go` →
   `openclaw.WriteMemoryMD`) and when it is bound to a channel
   (`internal/channels`). `AGENTS.md` (`internal/openclaw/materialize.go`) gains one
   sentence: when the agent is chatting with a person on a channel and the message
   carries no run token, it answers the person directly, keeps to its role, and does
   not call `yuno__finish` or work in a run directory. Tests: the file content after a
   save; the sentence present.
5. **Configuration hygiene (audit #15).** `.env.example`: replace the Go-expression
   defaults (`180*time.Second`, `300*time.Second`) with valid values (`180s`, `300s`),
   add `MAX_COST_USD` with its default; `compose.yaml` `x-api-env`: pass every name the
   config reads through (`${NAME:-default}` form, defaults matching `config.go`);
   remove the dead `MODEL_CALL_TIMEOUT` from config and the example. A test that every
   `env("…")` name in `config.go` appears in `.env.example` and in `compose.yaml`
   (read both files from the test).
6. **Run state validated against `state_schema` at runtime (audit #16).** In
   `internal/engine/walk.go` after a node's output is merged, validate the whole state
   against the definition's `state_schema` when present; a failure fails the node with
   reason `state schema: <first error>` (and follows `on_failure` like other node
   failures). Test with the dev pipeline definition and a bad verdict.
7. **Redaction (audit #22).** `internal/obs/redact.go`: OpenRouter keys
   (`sk-or-v1-[A-Za-z0-9]{10,}`) and run tokens (`r_[A-Z2-7]{24}`) are redacted; run
   tokens are redacted in the node transcripts the engine stores (`node.go`, where
   `messages` are mirrored) and therefore in `/trace`. Tests.
8. **Event hub data race (audit #26).** `internal/events/hub.go`: the counter under the
   read lock becomes atomic; `go test -race ./internal/events/` clean.
9. **`/api/channels` never blocks on the bot name (audit #27).** Cache the bot username
   at api start and at bind; the handler reads the cache (empty string until known);
   a request takes under 50 ms in the test.
10. **OpenAPI security scheme (audit #28).** `api/openapi.yaml`: a `bearerAuth`
    (`http`, `bearer`) scheme applied to every `/api/*` operation, described as
    optional when `API_TOKEN` is unset; the conformance test stays green.
11. **Postgres bound to localhost (audit #29).** `compose.yaml`: publish
    `127.0.0.1:${POSTGRES_PORT:-5432}:5432`.
12. **Eval runs tagged (audit #30).** `POST /api/runs` accepts `trigger: "eval"`
    (and `"manual"`, the default; anything else 422); the eval runner sends `eval`
    (`internal/evals/client.go`). The Runs page filter already lists triggers. Tests.
13. **Recalled facts are untrusted content (audit #31).** `memory_recall`'s result wraps
    the facts in `<untrusted_content source="memory_recall">…</untrusted_content>` with
    the same escaping as `http_fetch`; `memory_save` stays as is. Tests.
14. **Pinned tools (audit #34).** `Makefile`: `staticcheck@2025.1.1` (or the version
    currently resolved by `@latest`, check `go run … version` in the container);
    `compose.yaml`: `cloudflare/cloudflared:2025.9.1` (or the tag currently pulled).
15. **Version from the commit (demo pre-flight).** `docker/api.Dockerfile`: `ARG
    VERSION=dev` passed to `-ldflags "-X yuno/internal/config.Version=$VERSION"`;
    `compose.yaml` build args `VERSION: ${VERSION:-dev}` for api, deploytarget, eval;
    `Makefile`: `run` and `build` export `VERSION=$(git rev-parse --short HEAD)`.
    `/api/status` and the Status page then show the commit.
16. **Agents list fields (audit #17, #18).** `GET /api/agents` items gain `channels`
    (`["telegram"]` when bound, from `channel_bindings`) and `activity`
    (`{running, waiting_approval}` counts of this agent's active node visits); OpenAPI
    updated. The frontend agent renders them.
17. **Make targets from a fresh checkout (audit #12).** `make run` seeds after
    readiness (`$(COMPOSE) run --rm api seed`); `make test`, `make test-go`,
    `make test-web`, `make eval` depend on `env`; `make eval` checks `/readyz` first and
    prints "run `make run` first" when the stack is down.

## Then

In Docker: `gofmt -l`, `go vet ./...`, `go test ./... -count=1`, `make test-race`,
staticcheck on the packages you touched. Do not run the live integration tests, probes,
`make run`, smoke or evals; the coordinator does. Report per common.md: per item, the
test, the mutation check, anything you could not do and why.
