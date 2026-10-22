# Rules for each wave-1 builder

Your spec is `docs/briefs/chief-platform-control/DESIGN.md`, wave 1, the part your brief
names. Implement it exactly. When you have to interpret a rule, choose and write it down
(see "Report").

## Branch

- First make sure your worktree has `internal/actor/actor.go` with `type Note`, from
  `feat/chief-platform-control`: `git log --oneline -3`. If not, run
  `git merge --ff-only feat/chief-platform-control`.
- Work on the branch your brief names: `git checkout -b <branch>`.
- Commit with clear messages that end with
  `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Do not push, and do not merge into any other branch.

## Ownership (to keep the three branches mergeable)

- **Migrations:** only your assigned number. Part B: `0027_audit.sql`. Part C:
  `0028_liveness.sql`. Part A: `0029_platform_approvals.sql`.
- **Docs:** do not edit `docs/DESIGN.md`, `DECISIONS.md`, `CHANGELOG.md`, `README.md` or
  `docs/threat-model.md`. Write the doc text you would add, and every rule you had to
  interpret, to `docs/briefs/chief-platform-control/notes-<part>.md`. The coordinator
  folds them in.
- **`api/openapi.yaml`:** add your new paths and schemas only. Every new operation gets an
  `operationId` equal to its Go handler name and an `x-yuno-risk` (`read`, `routine`,
  `destructive`, `privileged`, `dedicated` or `machine`). Only part A edits existing
  operations.
- **`internal/httpapi/server.go`:** add your routes only. The middleware chain in
  `middleware.go` `withMiddleware` belongs to part B; part A adds its gate there in one line
  at the place DESIGN B6 names.
- **`internal/actor`** is shared and already written. Do not change its exported API.

## Hard rules

- The local Docker stack is running. Never run `make`, `docker compose`, or anything that
  touches containers named `yuno-*`, except the commands below.
- Never install toolchains on the host. Run every check in a throwaway container.
- Print no tokens. Send no Telegram messages. Test fixtures never use real credentials.
- Every rule in your part has a test, named as in DESIGN where it names one.
- Match the surrounding code style and comment density. Comments cite DESIGN rules the way
  the existing code does ("DESIGN 5.12 rule 4"). Product text (UI strings, error messages
  an operator reads) is for end users only: no developer notes.
- Reuse what exists before writing something new. Approvals, the park and resume machinery,
  `obs.Redact`/`RedactJSON`, the store helpers, `writeError` and `writeJSON` are all there.

## Checks (run once before you report, full speed, against your own fresh database)

Replace X with your part letter, lower case:

    docker exec yuno-postgres-1 psql -U yuno -d postgres -c 'DROP DATABASE IF EXISTS yuno_test_w1X' -c 'CREATE DATABASE yuno_test_w1X'
    docker run --rm --network yuno_default -v "$PWD:/src" -v yuno_gomod:/go/pkg/mod -v yuno_gocache:/root/.cache/go-build -w /src -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 -e DATABASE_URL='postgres://yuno:yuno@postgres:5432/yuno_test_w1X?sslmode=disable' yuno-go-tools:1.26 sh -c 'go build ./... && go vet ./... && go test -p 1 ./... -count=1 && gofmt -l internal cmd api && go run honnef.co/go/tools/cmd/staticcheck@2026.2.2 ./...' < /dev/null > /tmp/w1X.log 2>&1

Read the log file; do not pipe long output. If you touched `web/`, also run, from your
worktree root (copy `web/node_modules` from `/home/hdx/code/yuno-agent-orchestration/web`
first with `cp -a`):

    docker run --rm -u "$(id -u):$(id -g)" -v "$PWD/web:/app" -w /app -e HOME=/tmp -e COREPACK_ENABLE_DOWNLOAD_PROMPT=0 -e COREPACK_HOME=/tmp/corepack -e npm_config_cache=/tmp/npm-cache node:24-slim sh -c 'mkdir -p /tmp/corepack-bin /tmp/corepack && corepack enable --install-directory /tmp/corepack-bin && export PATH=/tmp/corepack-bin:$PATH && pnpm test && pnpm lint' < /dev/null > /tmp/w1X-web.log 2>&1

## Report (keep it brief)

- Branch name and commit ids.
- What you built, against the DESIGN rules.
- **Rules I had to interpret:** each one, with your choice.
- Tests added (names) and the check results (pass/fail counts; quote any failure).
- Anything left open.
