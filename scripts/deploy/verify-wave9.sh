#!/usr/bin/env bash
# Live verification of wave 9 (DESIGN 5.22 iterate on an open merge request, 5.23 issue
# intake) against a running instance, from its own host: the API token is read from the
# checkout's .env there and never printed. Run it over ssh with the script on stdin and
# the flags after "--":
#
#   ssh yuno@$IP 'bash -s -- --version' < scripts/deploy/verify-wave9.sh
#   ssh yuno@$IP 'bash -s -- --iterate 4 --task "address the review thread on the gcd tests"' < ...
#   ssh yuno@$IP 'bash -s -- --run <run id>' < ...
#   ssh yuno@$IP 'bash -s -- --intake-enable "Dev pipeline"' < ...     (label yuno, the project's caps must be set)
#   ssh yuno@$IP 'bash -s -- --poll' < ...                              (POST .../intake/poll)
#   ssh yuno@$IP 'bash -s -- --issue-runs' < ...                        (GET /api/runs?trigger=issue)
#   ssh yuno@$IP 'bash -s -- --intake-disable' < ...
#
# Everything is one function called at the end with stdin from /dev/null, so no child
# command can read the rest of the script (docs/deploy.md). Output is JSON summaries with
# ids, statuses and URLs; the token and the project token never appear.
set -euo pipefail

main() {
  local dir="${YUNO_DIR:-$HOME/yuno-agent-orchestration}"
  local base="${YUNO_API:-http://127.0.0.1:8080}"
  local project="${YUNO_PROJECT:-yuno-playground}"
  local token
  token=$(sed -n 's/^API_TOKEN=//p' "$dir/.env" | head -1)
  [ -n "$token" ] || { echo "no API_TOKEN in $dir/.env" >&2; exit 1; }

  api() { # method path [json body]
    local method=$1 path=$2 body=${3:-}
    if [ -n "$body" ]; then
      curl -sS -X "$method" -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
        --data "$body" "$base$path"
    else
      curl -sS -X "$method" -H "Authorization: Bearer $token" "$base$path"
    fi
  }
  jq_() { python3 -c "import json,sys; d=json.load(sys.stdin); $1"; }

  project_id() {
    api GET /api/projects | jq_ "print(next(p['id'] for p in d if p['name']=='$project'))"
  }
  workflow_id() { # by name
    api GET /api/workflows | jq_ "print(next(w['id'] for w in d if w['name']=='$1' and not w.get('template_key')))"
  }

  local task="" mr="" run="" wf="" action=""
  while [ $# -gt 0 ]; do
    case $1 in
      --version) action=version ;;
      --iterate) action=iterate; mr=$2; shift ;;
      --task) task=$2; shift ;;
      --run) action=run; run=$2; shift ;;
      --intake-enable) action=intake_enable; wf=$2; shift ;;
      --intake-disable) action=intake_disable ;;
      --poll) action=poll ;;
      --issue-runs) action=issue_runs ;;
      *) echo "unknown flag $1" >&2; exit 2 ;;
    esac
    shift
  done

  case $action in
    version)
      api GET /api/status | jq_ "print(json.dumps({k: d.get(k) for k in ('version','ok','scheduler')}))" ;;
    iterate)
      local pid rec
      pid=$(project_id)
      rec=$(api GET "/api/merge-requests?project_id=$pid&state=open" | jq_ "print(json.dumps(next(({'id': m['id'], 'number': m['number'], 'url': m['url'], 'branch': m['source_branch']} for m in d if m['number']==$mr), None)))")
      [ "$rec" != "null" ] || { echo "no open merge request !$mr on $project" >&2; exit 1; }
      echo "record: $rec"
      local rid
      rid=$(echo "$rec" | jq_ "print(d['id'])")
      local body
      body=$(python3 -c "import json,sys; print(json.dumps({'workflow_id': sys.argv[1], 'project_id': sys.argv[2], 'merge_request_id': sys.argv[3], 'input': {'task': sys.argv[4]}}))" \
        "$(workflow_id 'Dev pipeline')" "$pid" "$rid" "${task:-Address every unresolved review thread on this merge request.}")
      api POST /api/runs "$body" | jq_ "print(json.dumps({k: d.get(k) for k in ('id','status','merge_request_id','trigger')} if 'id' in d else d))"
      echo "second request while the first is live (expect 422 is being updated by run):"
      api POST /api/runs "$body" | jq_ "print(json.dumps(d.get('error', d)))" ;;
    run)
      api GET "/api/runs/$run" | jq_ "print(json.dumps({k: d.get(k) for k in ('id','status','current_node','step_count','cost_usd_micros','merge_request_id','checkout_sha','failure_reason')})); mr=d.get('merge_request'); print('merge_request:', json.dumps({k: mr.get(k) for k in ('number','state','ci_status','head_sha','unresolved_threads')} if mr else None))"
      echo "tool calls:"
      api GET "/api/runs/$run/trace" | jq_ "[print(json.dumps({'tool': t['tool_name'], 'status': t['status'], 'output': (json.dumps(t.get('output'))[:160] if t.get('output') else None)})) for t in d['tool_calls'] if t['tool_name'] in ('open_merge_request','read_merge_request','yuno__open_merge_request','yuno__read_merge_request')]" ;;
    intake_enable|intake_disable)
      local pid cur enabled=true wid=""
      pid=$(project_id)
      [ "$action" = intake_enable ] && wid=$(workflow_id "$wf") || enabled=false
      cur=$(api GET "/api/projects/$pid")
      body=$(echo "$cur" | python3 -c "import json,sys; d=json.load(sys.stdin); d['intake']={'enabled': sys.argv[1]=='true', 'label': 'yuno', 'workflow_id': sys.argv[2] or None}; d.pop('token', None); print(json.dumps(d))" "$enabled" "$wid")
      api PUT "/api/projects/$pid" "$body" | jq_ "print(json.dumps(d.get('intake', d.get('error', d))))" ;;
    poll)
      local pid
      pid=$(project_id)
      api POST "/api/projects/$pid/intake/poll" | jq_ "print(json.dumps(d))"
      api GET "/api/projects/$pid" | jq_ "print('intake:', json.dumps(d.get('intake')))" ;;
    issue_runs)
      api GET "/api/runs?trigger=issue&limit=20" | jq_ "[print(json.dumps({k: r.get(k) for k in ('id','status','trigger_ref','reported_at','step_count')})) for r in d]" ;;
    *) echo "give one of --version, --iterate N, --run ID, --intake-enable NAME, --intake-disable, --poll, --issue-runs" >&2; exit 2 ;;
  esac
}

main "$@" </dev/null
