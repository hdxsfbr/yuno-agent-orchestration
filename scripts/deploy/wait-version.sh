#!/usr/bin/env bash
# Wait until the api on this host serves a given commit: polls /api/status until its
# `version` starts with the wanted sha. Reads API_TOKEN from .env in the checkout and
# never prints it. Meant to run on the host after a push to main, from the checkout:
#   scripts/deploy/wait-version.sh <sha> [timeout seconds] [--port 8080]
# or fed over ssh before the new checkout exists there:
#   ssh yuno@HOST 'cd yuno-agent-orchestration && bash -s -- <sha>' < scripts/deploy/wait-version.sh
# Exit 0 when the version matches, 1 on timeout, 2 on usage.
set -u

main() {
  local want="" timeout=1200 port="${WEB_PORT:-8080}"
  while [ $# -gt 0 ]; do
    case "$1" in
      --port) port="$2"; shift 2 ;;
      *) if [ -z "$want" ]; then want="$1"; else timeout="$1"; fi; shift ;;
    esac
  done
  [ -n "$want" ] || { echo "usage: wait-version.sh <sha> [timeout seconds] [--port N]" >&2; return 2; }
  local token auth=() base="http://localhost:${port}" deadline=$(( $(date +%s) + timeout )) version=""
  token=$(grep -E '^API_TOKEN=' .env 2>/dev/null | cut -d= -f2-)
  [ -n "$token" ] && auth=(-H "Authorization: Bearer $token")
  while [ "$(date +%s)" -lt "$deadline" ]; do
    version=$(curl -fsS -m 5 "${auth[@]}" "$base/api/status" 2>/dev/null | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
    case "$version" in
      "$want"*) echo "version $version is live at $(date -u +%FT%TZ)"; return 0 ;;
    esac
    sleep 30
  done
  echo "timeout: version is '${version:-unreachable}', wanted $want" >&2
  return 1
}

main "$@" < /dev/null
