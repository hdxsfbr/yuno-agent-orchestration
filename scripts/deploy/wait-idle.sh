#!/usr/bin/env bash
# Wait until no run on this host is mid-step, so a deploy does not restart the api under a
# working agent. A run waiting for an approval or an answer is parked (DESIGN 5.24, D-085)
# and survives a restart, so only queued and running runs count. Run it after the images
# are built and right before the containers are replaced: the build takes minutes, and a
# parked run can resume during it. Reads API_TOKEN from .env in the checkout and never
# prints it.
#   scripts/deploy/wait-idle.sh [timeout seconds] [--port 8080]
# Exit 0 when idle (or when the api does not answer: nothing to protect), 1 on timeout.
set -u

main() {
  local timeout=1800 port="${WEB_PORT:-8080}"
  while [ $# -gt 0 ]; do
    case "$1" in
      --port) port="$2"; shift 2 ;;
      *) timeout="$1"; shift ;;
    esac
  done
  local token="" auth=() base="http://localhost:${port}" deadline=$(( $(date +%s) + timeout ))
  if [ -f .env ]; then
    token="$(grep -E '^API_TOKEN=' .env | head -1 | cut -d= -f2-)"
  fi
  [ -n "$token" ] && auth=(-H "Authorization: Bearer ${token}")
  local waited=0
  while :; do
    local status busy
    if ! status="$(curl -fsS -m 5 "${auth[@]}" "${base}/api/status" 2>/dev/null)"; then
      echo "wait-idle: the api does not answer; nothing to wait for"
      return 0
    fi
    busy="$(printf '%s' "$status" | python3 -c 'import json,sys; r=json.load(sys.stdin).get("runs",{}); print(int(r.get("queued",0))+int(r.get("running",0)))' 2>/dev/null || echo 0)"
    if [ "$busy" = "0" ]; then
      [ "$waited" = "1" ] && echo "wait-idle: no run is mid-step now"
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "wait-idle: ${busy} run(s) still mid-step after ${timeout}s; deploying anyway" >&2
      return 1
    fi
    [ "$waited" = "0" ] && echo "wait-idle: ${busy} run(s) mid-step; waiting before replacing the containers"
    waited=1
    sleep 15
  done
}

main "$@"
