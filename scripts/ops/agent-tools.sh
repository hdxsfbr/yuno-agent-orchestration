#!/usr/bin/env bash
# Add tools to (or remove them from) one agent's allowlist through the API, with the
# version it read (If-Match) and an idempotency key, so the change is audited like a UI
# edit (DESIGN 5.31). For a seeded agent the operator has edited: its copy no longer
# follows its template (D-080), so tools a release adds to the template do not reach it.
# Reads API_TOKEN from .env in the checkout and never prints it.
#   scripts/ops/agent-tools.sh --agent "Chief of Staff" --add platform_operations,platform_call [--remove x,y] [--port 8080] [--dry-run]
set -euo pipefail

agent="" add="" remove="" port="${WEB_PORT:-8080}" dry=0
while [ $# -gt 0 ]; do
  case "$1" in
    --agent) agent="$2"; shift 2 ;;
    --add) add="$2"; shift 2 ;;
    --remove) remove="$2"; shift 2 ;;
    --port) port="$2"; shift 2 ;;
    --dry-run) dry=1; shift ;;
    *) echo "agent-tools: unknown argument $1" >&2; exit 2 ;;
  esac
done
[ -n "$agent" ] || { echo "agent-tools: --agent is required" >&2; exit 2; }
[ -n "$add$remove" ] || { echo "agent-tools: --add or --remove is required" >&2; exit 2; }

token=""
if [ -f .env ]; then
  token="$(grep -E '^API_TOKEN=' .env | head -1 | cut -d= -f2-)"
fi

AGENT="$agent" ADD="$add" REMOVE="$remove" BASE="http://localhost:${port}" TOKEN="$token" DRY="$dry" \
python3 - <<'PY'
import json, os, sys, urllib.request, uuid

base, token = os.environ["BASE"], os.environ["TOKEN"]
def call(method, path, body=None, headers=None):
    h = {"Content-Type": "application/json"}
    if token:
        h["Authorization"] = "Bearer " + token
    h.update(headers or {})
    req = urllib.request.Request(base + path, method=method, headers=h,
                                 data=json.dumps(body).encode() if body is not None else None)
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, r.headers, json.load(r)
    except urllib.error.HTTPError as e:
        return e.code, e.headers, json.loads(e.read() or b"{}")

name = os.environ["AGENT"].strip().lower()
_, _, agents = call("GET", "/api/agents")
match = [a for a in agents if a["name"].strip().lower() == name]
if len(match) != 1:
    sys.exit(f"agent-tools: {len(match)} agents named {os.environ['AGENT']!r}")
status, hdr, cur = call("GET", "/api/agents/" + match[0]["id"])
if status != 200:
    sys.exit(f"agent-tools: reading the agent answered {status}")
tools = list(cur.get("tool_allowlist") or [])
for t in filter(None, os.environ["ADD"].split(",")):
    if t not in tools:
        tools.append(t)
drop = set(filter(None, os.environ["REMOVE"].split(",")))
tools = [t for t in tools if t not in drop]
if tools == list(cur.get("tool_allowlist") or []):
    print("agent-tools: nothing to change")
    sys.exit(0)
# The update takes the agent's input fields; a zero max_tokens means "unset" and is left out.
# Every input field of the update (it is a full replace), as the API's agentInput names them.
fields = ["name", "role", "system_prompt", "model", "effort", "thinking", "max_tokens", "tool_allowlist",
          "interaction_rules", "limits", "guardrails", "memory_policy", "exec_policy", "skills", "channels"]
body = {k: cur[k] for k in fields if k in cur and cur[k] not in (None,)}
if not body.get("max_tokens"):
    body.pop("max_tokens", None)
body["tool_allowlist"] = tools
print("agent-tools: " + cur["name"] + " tools -> " + ", ".join(tools))
if os.environ["DRY"] == "1":
    sys.exit(0)
status, _, out = call("PUT", "/api/agents/" + cur["id"], body,
                      {"If-Match": hdr.get("ETag", ""), "Idempotency-Key": "agent-tools-" + uuid.uuid4().hex})
if status != 200:
    sys.exit(f"agent-tools: update answered {status}: {json.dumps(out)[:300]}")
print("agent-tools: updated")
PY
