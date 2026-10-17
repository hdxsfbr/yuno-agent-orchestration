#!/usr/bin/env bash
# Deploys a ref to a host prepared by bootstrap-droplet.sh: optionally installs the environment
# file, pushes the ref from this checkout to the host over ssh (no GitLab credentials on the
# host), checks it out there, runs `make run-public` and `make smoke`. Used by hand
# (make deploy HOST=...). The GitLab deploy job does the same with its job token instead of a
# push (.gitlab-ci.yml). Never prints anything from .env.
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
    -h|--help) sed -n '2,9p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done
[ -n "$HOST" ] || { sed -n '8,9p' "$0" >&2; exit 2; }

SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new)
target="$USER_NAME@$HOST"

if [ -n "$ENV_FILE" ]; then
  [ -f "$ENV_FILE" ] || { echo "$ENV_FILE not found" >&2; exit 1; }
  scp -q "${SSH_OPTS[@]}" "$ENV_FILE" "$target:$DIR/.env"
  ssh "${SSH_OPTS[@]}" "$target" "chmod 600 '$DIR/.env'"
  echo "environment file installed on $HOST"
fi

REF_SHA=$(git rev-parse --verify --quiet "$REF^{commit}") || { echo "unknown ref: $REF" >&2; exit 1; }
echo "pushing $(git rev-parse --short "$REF_SHA") ($REF) to $HOST"
GIT_SSH_COMMAND="ssh ${SSH_OPTS[*]}" git push -q -f "ssh://$target/~/$DIR" "$REF_SHA:refs/heads/deploy"

# The remote script arrives on bash's stdin. It is one function, so bash reads all of it
# before running anything, and the call redirects stdin from /dev/null so no child command
# can swallow the rest of the script (compose and curl otherwise do).
ssh "${SSH_OPTS[@]}" "$target" bash -s -- "$DIR" "$SMOKE" <<'REMOTE'
main() {
  set -euo pipefail
  local dir="$1" smoke="$2"
  cd "$dir"
  [ -f .env ] || { echo "no .env in $dir; pass --env-file once (docs/deploy.md)" >&2; exit 1; }
  # A deploy checkout holds nothing untracked but ignored files (.env, data/); anything
  # else is a stray copy and would block a checkout that now tracks the same path.
  git clean -q -fd
  git checkout -q -B deploy refs/heads/deploy
  git reset -q --hard refs/heads/deploy
  echo "deploying $(git rev-parse --short HEAD) on $(hostname)"
  make run-public
  if [ "$smoke" = 1 ]; then make smoke; fi
}
main "$@" </dev/null
REMOTE
