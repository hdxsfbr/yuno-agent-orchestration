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
root's authorized keys), adds swap, opens 22, 80 and 443 and clones the repository into
`/home/yuno/yuno-agent-orchestration`.

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

`.env.prod` is git-ignored like every `.env.*`. Deploy:

```bash
make deploy HOST=$IP ENV_FILE=.env.prod
```

`scripts/deploy/deploy.sh` copies the file to the host (mode 600), checks out `main` there,
runs `make run-public` (the usual `make run`, then Caddy, then waits for
`https://DOMAIN/readyz`) and `make smoke`, which also checks that the webhook refuses a
wrong secret and that Telegram has the right URL registered. The first certificate takes
up to a minute; `make caddy-logs` on the host shows the issuance.

## Updating

```bash
make deploy HOST=$IP            # main, no environment file
make deploy HOST=$IP REF=<sha>  # any commit or branch; also how to roll back
```

A deploy rebuilds the images on the host (a few minutes on 4 vCPUs), recreates the
containers whose configuration changed and keeps Postgres, the gateway state and the
releases volume. `.env` on the host is only touched when `ENV_FILE` is passed.

## Deploying from GitLab CI

`.gitlab-ci.yml` has two jobs. `test` runs `make run`, `make test`, `make lint` and
`make smoke` on a runner tagged `yuno-ci`; `deploy` runs `make run-public` and
`make smoke` on a runner tagged `yuno-prod`, which is the droplet itself. Both need the
shell executor on a host with Docker: the tool containers bind-mount the checkout, which a
docker-in-docker executor cannot provide. The deploy job is manual on `main` (a play button
on the pipeline once `test` is green); set the project variable `DEPLOY_AUTO` to `true` to
deploy every green `main` pipeline automatically. Pipelines are created only while the
project variable `YUNO_CI` is `true`, so nothing shows as pending before runners exist.

To register the deploy runner on the droplet, create a project runner in GitLab
(Settings, CI/CD, Runners: tag `yuno-prod`, "run untagged jobs" off) and feed its token to
the installer through the environment:

```bash
ssh root@$IP RUNNER_TOKEN=glrt-... bash -s -- --tags yuno-prod < scripts/deploy/install-runner.sh
```

The runner service runs as `yuno`, so the deploy job can reach the checkout, its `.env`
and the docker group. The `test` runner needs another Docker host with at least 8 GB that
does not run the production stack (same ports, same compose project name); a second
droplet or a workstation with the same installer and tag `yuno-ci` works. Until one exists
the `test` job has nowhere to run and `deploy` cannot start, so deploy by hand with
`make deploy`.

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
