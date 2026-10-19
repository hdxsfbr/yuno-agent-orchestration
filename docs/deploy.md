# Deploying to a public host

The stack runs on one Docker host. It needs the Docker socket (the gateway creates a sandbox
container per agent), a shared network namespace between the api and the gateway, a bind
mount both of them write, and 4 GB for the gateway alone. Managed-container platforms
(Railway, DigitalOcean App Platform, Render, Fly) provide none of that, so the public
deployment is a DigitalOcean droplet running the same compose file plus one service:
Caddy, which terminates TLS for a hostname and proxies to nginx (D-035).

## What is public

| Path | Reaches | Protection |
|---|---|---|
| `https://DOMAIN/` and `/api/...` | the UI and the API | the API token (`API_TOKEN`, D-031); the UI asks for it |
| `https://DOMAIN/telegram-webhook` | the gateway's Telegram webhook | the secret header Telegram sends (`TELEGRAM_WEBHOOK_SECRET`) |
| `https://DOMAIN/deploy/releases/...` | the local deploy target's releases | none; they are the demo's deployed artifacts |
| ports 22, 80, 443 | ssh, Caddy | ufw; every other port is bound to loopback |

Docker publishes ports straight past ufw, which is why `WEB_BIND=127.0.0.1` matters: without
it the plain-http port 8080 would answer from the internet. Postgres is loopback-only in
compose already. The Docker socket inside the gateway container is root on the host (threat
model), so the droplet runs nothing else.

## Sizing

| Item | Value |
|---|---|
| Size | `s-4vcpu-8gb` (8 GB, 4 vCPU, 160 GB, $48/month); `s-4vcpu-16gb-amd` if the gateway is OOM-killed |
| Image | `ubuntu-24-04-x64` (the bootstrap installs Docker) |
| Swap | 4 GB file, added by the bootstrap |
| DNS | an A record for `DOMAIN` pointing at the droplet, or `<ip>.sslip.io` for a demo |

## First deployment

Everything below runs from a machine with `doctl` (authenticated) and an SSH key that is on
the DigitalOcean account. Nothing is installed on that machine; the droplet gets Docker.

```bash
IP=$(scripts/deploy/create-droplet.sh --name yuno-prod --size s-4vcpu-8gb --region sfo3)
ssh root@$IP bash -s -- < scripts/deploy/bootstrap-droplet.sh
```

The bootstrap installs Docker Engine, git and unattended upgrades, creates the `yuno` user
(uid 1000 so the bind-mounted state matches the containers' user, in the docker group, with
root's authorized keys), adds swap, opens 22, 80 and 443 and prepares the repository at
`/home/yuno/yuno-agent-orchestration`. The GitLab project is internal, so the host cannot
clone it without credentials; the bootstrap leaves an empty repository and every deploy
pushes the commit from your checkout over SSH (git updates the working tree on push).
Nothing on the host can read GitLab.

Then prepare the host's environment file from your local one. Copy `.env` to `.env.prod`
and change these lines (the rest, including the model and Telegram secrets, stay as they
are):

```
DOMAIN=yuno.example.com          # or <ip>.sslip.io
WEB_BIND=127.0.0.1
COMPOSE_PROFILES=public
PUBLIC_BASE_URL=https://yuno.example.com
TELEGRAM_MODE=webhook            # Telegram posts to https://DOMAIN/telegram-webhook
API_TOKEN=<a long random value>  # the UI and every API call need it
```

Projects (the git repositories runs work on, README "Projects") are created on the
Projects tab once the stack is up; their tokens are stored encrypted under
`PROJECT_TOKEN_KEY`, which `make run-public` generates into the host's `.env` on its
first run when the file has none. Back that key up with the `.env`: a lost key makes
every stored token unreadable (the Check button says so) and each has to be entered
again on the page. The `PROJECT_*` settings may still be put in the file as a bootstrap:
at the api's start they become the first project, once, and are not read again.

`.env.prod` is git-ignored like every `.env.*`. Deploy:

```bash
make deploy HOST=$IP ENV_FILE=.env.prod
```

`scripts/deploy/deploy.sh` copies the file to the host (mode 600), pushes your local `main`
to the host and checks it out there, runs `make run-public` (the usual `make run`, then Caddy, then waits for
`https://DOMAIN/readyz`) and `make smoke`, which also checks that the webhook refuses a
wrong secret and that Telegram has the right URL registered. The first certificate takes
up to a minute; `make caddy-logs` on the host shows the issuance.

## Updating

```bash
make deploy HOST=$IP            # your local main, no environment file
make deploy HOST=$IP REF=<sha>  # any local commit or branch; also how to roll back
```

A deploy ships what your checkout has, so pull first. It rebuilds the images on the host (a few minutes on 4 vCPUs), recreates the
containers whose configuration changed and keeps Postgres, the gateway state and the
releases volume. `.env` on the host is only touched when `ENV_FILE` is passed. The
per-toolchain sandbox images (`yuno-sandbox-python`, `-node`, `-go`) are built on the host
by the same `make run` the deploy runs, so a project run never pulls an image at run
time; `make smoke` checks that all four sandbox images exist. An install that predates
projects keeps its seeded agents and workflows; run `make seed-reset` once on the host
so the Deployer gets `open_merge_request`, the Chief of Staff gets `list_projects` and
its new prompt, the Coder and the Reviewer get the memory tools and the prompt lines
about project notes (wave 8), and the Dev pipeline opts into projects. The reset keeps
memory settings, so after it enable memory on the Coder's and the Reviewer's agent
pages once. `BUDGET_TICK` and `MERGE_REQUEST_SYNC_INTERVAL` (wave 8) and
`INTAKE_POLL_INTERVAL` (wave 9, how often projects with issue intake enabled are polled)
have defaults and need no entry in `.env`. After the wave 9 deploy run `make seed-reset`
once more: the Coder and the Reviewer gain `read_merge_request` and the prompt lines
about a merge request the run updates, the Deployer the update wording, the Chief the
`merge_request` argument and the intake sentence, and the Dev pipeline's deployer task
names both statuses. After the wave 10 deploy run it once more: the Coder and the
Researcher gain `ask_operator` and the sentence about asking, the Writer the sentence
about `out/`, the Chief the answering sentence, and the research template's writer task
names `out/`. `ARTIFACT_DIR` and `ARTIFACT_TTL_DAYS` (wave 10) have defaults; the deploy
creates the `artifacts` volume on the api, the first named volume it mounts (back it up
with `pgdata` if the delivered files matter).

## Deploying from GitLab CI

`.gitlab-ci.yml` has two jobs. `test` runs `make run`, `make test`, `make lint` and
`make smoke` on a runner tagged `yuno-ci`; `deploy` runs `make run-public` and
`make smoke` on a runner tagged `yuno-prod`, which is the droplet itself; it fetches the
commit with the job token, so the host still holds no GitLab credential. Both need the
shell executor on a host with Docker: the tool containers bind-mount the checkout, which a
docker-in-docker executor cannot provide. The deploy job is manual on `main` (a play button
on the pipeline once `test` is green); set the project variable `DEPLOY_AUTO` to `true` to
deploy every green `main` pipeline automatically. Pipelines are created only while the
project variable `YUNO_CI` is `true`, so nothing shows as pending before runners exist.

To register the deploy runner on the droplet, create a project runner in GitLab
(Settings, CI/CD, Runners, or `POST /user/runners`: tag `yuno-prod`, "run untagged jobs"
off) and feed its token to the installer as the first line of stdin, ahead of the script,
so it never appears on a command line:

```bash
{ cat runner-token.txt; cat scripts/deploy/install-runner.sh; } \
  | ssh root@$IP 'read -r RUNNER_TOKEN; export RUNNER_TOKEN; bash -s -- --tags yuno-prod'
```

Until a `yuno-ci` runner exists, leave the project variable `YUNO_CI_TEST` unset: the
pipeline then contains only the manual deploy job and its name says that tests were run
by hand. Set `YUNO_CI_TEST` to `true` once the test runner is registered and `test` becomes
a prerequisite of `deploy`.

The runner service runs as `yuno`, so the deploy job can reach the checkout, its `.env`
and the docker group. The `test` runner needs another Docker host with at least 8 GB that
does not run the production stack (same ports, same compose project name); a second
droplet or a workstation with the same installer and tag `yuno-ci` works.

## Current deployment

| Item | Value |
|---|---|
| Host | `yuno-prod`, `s-4vcpu-8gb` in `sfo2` (sfo3 did not offer that size), tag `yuno` |
| URL | <https://143.110.128.97.sslip.io> (Let's Encrypt certificate through Caddy) |
| Deployed | wave 10 (`2aad95c`, then the docs commit `6199a40`; questions to the operator, run artifacts) on 2026-10-18 by the pipeline on `main`, the question paths (answered within the wait, parked and resumed) and the artifacts verified live on 2026-10-19 00:10 to 00:26 UTC (docs/test-results.md); before it wave 9 (`6db1207`, iterate on an open merge request, issue intake), verified live; wave 8 (`0ff8401`), wave 7 (`f08aac5`), wave 5 (`12e6478`) by pipeline 57708 and wave 6 (`b953f2b`). First deploy: `make deploy HOST=143.110.128.97` at `4488049` |
| CI | runner 310 (`yuno-prod`) on the public droplet deploys; runner 311 (`yuno-ci`) on `yuno-ci` (`s-4vcpu-8gb`, `sfo2`, 157.245.188.91) runs the test job; project variables `YUNO_CI`, `YUNO_CI_TEST`, `DOMAIN`. Every push to `main` runs the tests and, with `DEPLOY_AUTO=true` (set), deploys itself when they pass; unset it to get a manual button instead |
| Sign-in | the `API_TOKEN` in the operator's `.env.prod` (never in the repository) |
| Telegram | webhook registered at `https://143.110.128.97.sslip.io/telegram-webhook`. Bound to the Chief of Staff since wave 5 (rebound from Coder through the API on 2026-10-16). Verified live on 2026-10-17: the Chief listed the roster, delegated a task to Coder (run succeeded in 25 s, $0.06), relayed the platform's report one second after the run ended, and answered a follow-up with `run_status`. The bot has one webhook, so this instance owns the bot while it runs; an unbound channel drops messages |
| Memory | about 1.7 GB used of 8 GB idle, the gateway 1 GB of its 4 GB cap |
| Model provider | Anthropic directly (`MODEL_UPSTREAM=anthropic`, `ANTHROPIC_API_KEY` in the host's `.env`) since 2026-10-19 00:00 UTC. Before that OpenRouter (`MODEL_UPSTREAM=openrouter`), whose key carries a weekly spending limit on OpenRouter's side: it was reached on 2026-10-18 at about 23:20 UTC (`403 Key limit exceeded (weekly limit)` on `/model-proxy`, the gateway says "Couldn't sign in to the AI service") and every run failed at its first model call. The OpenRouter key is still in `.env` for switching back; a change of `.env` takes effect with `make run-public` on the host, which recreates the api (the file is backed up there as `.env.bak-<time>` first) |
| Projects | `yuno-playground` (private, GitLab, a project access token named `yuno-deployer` with `api` and `write_repository`, Developer role, expires 2026-11-16), network off: bootstrapped from the `PROJECT_*` values of `.env.prod` at the first start of wave 7 and now a record on the Projects tab, where the token is replaced when it expires. `PROJECT_TOKEN_KEY` was generated into the host's `.env` by that deploy: back it up with the file. Merge requests !1 (wave 6), !2 (wave 7) and !4 (wave 8, recorded and synced) were opened by the Dev pipeline. The token's Developer role cannot merge into the protected `main`, so Merge on the run page answers the 409 refusal until the role is raised on GitLab. Instructions set on 2026-10-18; budget $5 a day and $30 a month; memory enabled on the Coder and the Reviewer after `make seed-reset`. Wave 9: `!4` was updated by a run that named it (one commit on top of its branch), and issue intake is enabled on the project (label `yuno`, the Dev pipeline): an issue labelled `yuno` becomes a run within two minutes; issue #1 became the run that opened `!5` |

Four things bit on the first runs and are now in the scripts: a fresh droplet holds the apt
lock for cloud-init's first pass (the bootstrap waits for it), `make` was missing (installed),
a script fed to `bash -s` over ssh lost its tail to a child command that read stdin (every
remote script is now one function called with stdin from `/dev/null`), and the runner's
first job died in "prepare environment" because Ubuntu's default `.bash_logout` runs
`clear_console`, which fails without a terminal (the installer empties that file).

## Operations on the host

```bash
ssh yuno@$IP
cd yuno-agent-orchestration
make ps                 # containers and health
make logs               # the api
make openclaw-logs      # the gateway
make caddy-logs         # TLS and proxy
make smoke              # every service through the public paths
make stop               # keep state; make run-public brings it back
```

What is not covered: backups (the `pgdata` volume and `data/openclaw` are the state),
alerting, more than one host, and users beyond the single API token.
