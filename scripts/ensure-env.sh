#!/usr/bin/env bash
# Fills empty platform tokens in .env with random values so `make run` works on a fresh
# checkout. Never touches a token that already has a value. Idempotent.
#
# Usage: scripts/ensure-env.sh [--file .env]
set -euo pipefail

FILE=".env"
while [ $# -gt 0 ]; do
  case "$1" in
    --file) FILE="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done
[ -f "$FILE" ] || { echo "$FILE not found" >&2; exit 1; }

gen() { head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'; }

ensure() { # name
  local name="$1"
  if grep -qE "^${name}=\s*$" "$FILE"; then
    local value; value="$(gen)"
    # portable in-place edit
    local tmp; tmp="$(mktemp)"
    sed "s|^${name}=\s*$|${name}=${value}|" "$FILE" > "$tmp" && cat "$tmp" > "$FILE" && rm -f "$tmp"
    echo "generated ${name}"
  elif ! grep -qE "^${name}=" "$FILE"; then
    echo "${name}=$(gen)" >> "$FILE"
    echo "generated ${name}"
  fi
}

ensure OPENCLAW_GATEWAY_TOKEN
ensure YUNO_MCP_TOKEN

# The gateway container runs as uid 1000 and must be able to open the host Docker
# socket to create sandboxes; compose adds this group id to the container.
sock=$( (grep -E '^DOCKER_SOCKET=' "$FILE" || true) | cut -d= -f2-)
sock="${sock:-/var/run/docker.sock}"
if [ -S "$sock" ]; then
  gid=$(stat -c %g "$sock")
  if grep -qE '^DOCKER_GID=' "$FILE"; then
    tmp="$(mktemp)"; sed "s|^DOCKER_GID=.*$|DOCKER_GID=${gid}|" "$FILE" > "$tmp" && cat "$tmp" > "$FILE" && rm -f "$tmp"
  else
    echo "DOCKER_GID=${gid}" >> "$FILE"
  fi
else
  echo "warning: docker socket $sock not found; OpenClaw sandboxes will not start" >&2
fi
