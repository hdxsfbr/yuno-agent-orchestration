#!/usr/bin/env sh
# Measures the latency of GET /api/status (DESIGN 5.12 rule 5, portal brief: p95 < 500 ms).
# Runs N sequential requests through nginx from the host and prints p50/p95/max in ms.
# Usage: scripts/status-p95.sh [N] [BASE]   (env: API_TOKEN from .env when set)
set -eu
N=${1:-200}
BASE=${2:-http://127.0.0.1:8080}
TOKEN=$(grep -E '^API_TOKEN=' .env 2>/dev/null | cut -d= -f2- || true)
AUTH=""
[ -n "$TOKEN" ] && AUTH="Authorization: Bearer $TOKEN"
tmp=$(mktemp)
i=0
while [ "$i" -lt "$N" ]; do
  curl -s -o /dev/null -w '%{time_total}\n' -m 5 ${AUTH:+-H "$AUTH"} "$BASE/api/status" >> "$tmp" || echo "9.999" >> "$tmp"
  i=$((i + 1))
done
sort -n "$tmp" | awk -v n="$N" '
  { a[NR] = $1 * 1000 }
  END {
    p50 = a[int(n * 0.50 + 0.5)]; p95 = a[int(n * 0.95 + 0.5)]; max = a[n]
    printf "GET /api/status over %d requests: p50 %.1f ms, p95 %.1f ms, max %.1f ms\n", n, p50, p95, max
    exit (p95 < 500) ? 0 : 1
  }'
rc=$?
rm -f "$tmp"
exit $rc
