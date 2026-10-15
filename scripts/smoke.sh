#!/usr/bin/env bash
# Smoke test for a running stack (make run). Exercises every service through the paths
# the product uses and checks the OpenClaw integration prerequisites. Exit 1 on failure.
#
# Usage: scripts/smoke.sh [--port 8080] [--quiet]
set -u

PORT="${WEB_PORT:-8080}"
QUIET=0
while [ $# -gt 0 ]; do
  case "$1" in
    --port) PORT="$2"; shift 2 ;;
    --quiet) QUIET=1; shift ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

BASE="http://localhost:${PORT}"
FAIL=0
pass() { [ "$QUIET" = 1 ] || echo "ok    $1"; }
fail() { echo "FAIL  $1: $2" >&2; FAIL=1; }
compose() { docker compose "$@"; }

check_http() { # name url expected-substring
  local out
  out=$(curl -fsS -m 5 "$2" 2>&1) || { fail "$1" "request failed: $out"; return; }
  case "$out" in *"$3"*) pass "$1" ;; *) fail "$1" "unexpected body: ${out:0:200}" ;; esac
}

check_http "web serves the app"       "$BASE/"                 "<div id=\"root\">"
check_http "readyz through nginx"     "$BASE/readyz"           "ready"
check_http "api status"               "$BASE/api/status"       "\"db_ok\":true"
check_http "openapi document"         "$BASE/openapi.yaml"     "openapi: 3.1.0"
check_http "deploytarget via proxy"   "$BASE/deploy/releases"  "["

if curl -s -m 3 -N "$BASE/api/events" 2>/dev/null | grep -q ": connected"; then
  pass "sse stream opens"
else
  fail "sse stream opens" "no ': connected' line within 3s"
fi

# OpenClaw gateway healthy and reachable from the api over loopback.
if compose exec -T api wget -qO- -T 3 http://127.0.0.1:18789/healthz >/dev/null 2>&1; then
  pass "gateway reachable from api over loopback"
else
  fail "gateway reachable from api over loopback" "healthz failed"
fi

# Gateway can talk to the Docker daemon (needed to create sandboxes).
out=$(compose exec -T openclaw node -e "
const http=require('http');
const req=http.request({socketPath:'/var/run/docker.sock',path:'/_ping',method:'GET'},r=>{let b='';r.on('data',c=>b+=c);r.on('end',()=>{console.log(r.statusCode,b);process.exit(r.statusCode===200?0:1)})});
req.on('error',e=>{console.log('error',e.message);process.exit(1)});req.end();" 2>&1)
case "$out" in
  200*) pass "gateway reaches the docker socket" ;;
  *) fail "gateway reaches the docker socket" "$out" ;;
esac

if docker image inspect yuno-sandbox:latest >/dev/null 2>&1; then
  pass "sandbox image present"
else
  fail "sandbox image present" "yuno-sandbox:latest missing (make run builds it)"
fi

# Baseline config written by openclaw-init (hash sidecar marks platform ownership).
if [ -f data/openclaw/openclaw.json ] && [ -s data/openclaw/yuno-baseline.hash ] && grep -q '"mcp"' data/openclaw/openclaw.json; then
  pass "baseline openclaw.json written by openclaw-init"
else
  fail "baseline openclaw.json written by openclaw-init" "missing config or hash sidecar"
fi

# Telegram webhook route: a request with a wrong secret must not be accepted (DESIGN 5.8 rule 2).
mode=$(grep -E '^TELEGRAM_MODE=' .env 2>/dev/null | cut -d= -f2)
if [ "$mode" = "webhook" ]; then
  code=$(curl -s -o /dev/null -w '%{http_code}' -m 5 -X POST "$BASE/telegram-webhook" \
    -H 'Content-Type: application/json' -H 'X-Telegram-Bot-Api-Secret-Token: wrong' -d '{"update_id":1}')
  if [ "$code" != "200" ]; then pass "telegram webhook rejects a wrong secret ($code)"; else fail "telegram webhook rejects a wrong secret" "got 200"; fi
else
  [ "$QUIET" = 1 ] || echo "skip  telegram webhook check (TELEGRAM_MODE=$mode)"
fi

[ "$FAIL" = 0 ] && { [ "$QUIET" = 1 ] || echo "smoke: all checks passed"; exit 0; }
echo "smoke: FAILED" >&2
exit 1
