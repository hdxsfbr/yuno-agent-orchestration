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
# With API_TOKEN set, /api requests and the event stream need the bearer (never printed).
TOKEN=$(grep -E '^API_TOKEN=' .env 2>/dev/null | cut -d= -f2-)
AUTH=()
[ -n "$TOKEN" ] && AUTH=(-H "Authorization: Bearer $TOKEN")
pass() { [ "$QUIET" = 1 ] || echo "ok    $1"; }
fail() { echo "FAIL  $1: $2" >&2; FAIL=1; }
compose() { docker compose "$@"; }

check_http() { # name url expected-substring
  local out
  out=$(curl -fsS -m 5 "${AUTH[@]}" "$2" 2>&1) || { fail "$1" "request failed: $out"; return; }
  case "$out" in *"$3"*) pass "$1" ;; *) fail "$1" "unexpected body: ${out:0:200}" ;; esac
}

check_http "web serves the app"       "$BASE/"                 "<div id=\"root\">"
check_http "readyz through nginx"     "$BASE/readyz"           "ready"
check_http "api status"               "$BASE/api/status"       "\"db_ok\":true"
check_http "openapi document"         "$BASE/openapi.yaml"     "openapi: 3.1.0"
check_http "deploytarget via proxy"   "$BASE/deploy/releases"  "["

if curl -s -m 3 -N "${AUTH[@]}" "$BASE/api/events" 2>/dev/null | grep -q ": connected"; then
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

# Telegram webhook (DESIGN 5.8 rule 2): a request with a wrong secret is refused as
# unauthorised (401 or 403, not just any error), and Telegram itself has the webhook
# registered at PUBLIC_BASE_URL/telegram-webhook.
env_value() { # NAME: the last value .env gives it, without surrounding quotes
  sed -n "s/^$1=//p" .env 2>/dev/null | tail -1 | sed -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'$/\1/"
}
mode=$(env_value TELEGRAM_MODE)
if [ "$mode" = "webhook" ]; then
  code=$(curl -s -o /dev/null -w '%{http_code}' -m 5 -X POST "$BASE/telegram-webhook" \
    -H 'Content-Type: application/json' -H 'X-Telegram-Bot-Api-Secret-Token: wrong' -d '{"update_id":1}')
  case "$code" in
    401|403) pass "telegram webhook rejects a wrong secret ($code)" ;;
    *) fail "telegram webhook rejects a wrong secret" "got $code, want 401 or 403" ;;
  esac
  want="$(env_value PUBLIC_BASE_URL)"
  want="${want%/}/telegram-webhook"
  # The bot token reaches curl as a config file on stdin: never on a command line, in
  # the output or in an error message.
  if [ -z "$(env_value TELEGRAM_BOT_TOKEN)" ]; then
    fail "telegram webhook registered" "TELEGRAM_BOT_TOKEN is empty in .env"
  else
    info=$(printf 'url = "https://api.telegram.org/bot%s/getWebhookInfo"\n' "$(env_value TELEGRAM_BOT_TOKEN)" \
      | curl -s -m 10 -K - 2>/dev/null) || info=""
    registered=$(printf '%s' "$info" | sed -n 's/.*"url":"\([^"]*\)".*/\1/p' | sed 's#\\/#/#g')
    if [ -z "$info" ]; then
      fail "telegram webhook registered" "getWebhookInfo did not answer"
    elif [ "$registered" = "$want" ]; then
      pass "telegram webhook registered at $want"
    else
      fail "telegram webhook registered" "Telegram has '${registered:-no url}', want '$want'"
    fi
  fi
else
  [ "$QUIET" = 1 ] || echo "skip  telegram webhook checks (TELEGRAM_MODE=$mode)"
fi

[ "$FAIL" = 0 ] && { [ "$QUIET" = 1 ] || echo "smoke: all checks passed"; exit 0; }
echo "smoke: FAILED" >&2
exit 1
