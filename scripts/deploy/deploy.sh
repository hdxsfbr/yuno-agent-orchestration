#!/usr/bin/env bash
# Deploys a ref to a host prepared by bootstrap-droplet.sh: optionally installs the environment
# file, checks out the ref there, runs `make run-public` and `make smoke`. Used by hand
# (make deploy HOST=...) and by the GitLab deploy job. Never prints anything from .env.
#
# Usage: scripts/deploy/deploy.sh --host HOST [--user yuno] [--ref main]
#          [--dir yuno-agent-orchestration] [--env-file PATH] [--no-smoke]
set -euo pipefail

HOST="" USER_NAME=yuno REF=main DIR=yuno-agent-orchestration ENV_FILE="" SMOKE=1
while [ $# -gt 0 ]; do
  case "$1" in
    --host) HOST="$2"; shift 2 ;;
    --user) USER_NAME="$2"; shift 2 ;;
    --ref) REF="$2"; shift 2 ;;
    --dir) DIR="$2"; shift 2 ;;
    --env-file) ENV_FILE="$2"; shift 2 ;;
    --no-smoke) SMOKE=0; shift ;;
    -h|--help) sed -n '2,7p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done
[ -n "$HOST" ] || { sed -n '6,7p' "$0" >&2; exit 2; }

SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new)
target="$USER_NAME@$HOST"

if [ -n "$ENV_FILE" ]; then
  [ -f "$ENV_FILE" ] || { echo "$ENV_FILE not found" >&2; exit 1; }
  scp -q "${SSH_OPTS[@]}" "$ENV_FILE" "$target:$DIR/.env"
  ssh "${SSH_OPTS[@]}" "$target" "chmod 600 '$DIR/.env'"
  echo "environment file installed on $HOST"
fi

ssh "${SSH_OPTS[@]}" "$target" bash -s -- "$DIR" "$REF" "$SMOKE" <<'REMOTE'
set -euo pipefail
DIR="$1"; REF="$2"; SMOKE="$3"
cd "$DIR"
[ -f .env ] || { echo "no .env in $DIR; pass --env-file once (docs/deploy.md)" >&2; exit 1; }
git fetch -q origin
if git show-ref -q --verify "refs/remotes/origin/$REF"; then
  git checkout -q -B "$REF" "origin/$REF"
else
  git checkout -q "$REF"
fi
echo "deploying $(git rev-parse --short HEAD) ($REF) on $(hostname)"
make run-public
if [ "$SMOKE" = 1 ]; then make smoke; fi
REMOTE
