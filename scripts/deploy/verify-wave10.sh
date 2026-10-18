#!/usr/bin/env bash
# Live verification of wave 10 (DESIGN 5.24 questions to the operator, 5.25 run
# artifacts) against a running instance, from its own host: the API token is read from
# the checkout's .env there and never printed. Run it over ssh with the script on stdin
# and the flags after "--":
#
#   ssh yuno@$IP 'bash -s -- --version' < scripts/deploy/verify-wave10.sh
#   ssh yuno@$IP 'bash -s -- --ask-run' < ...                 (a Dev pipeline run on the project whose task makes the Coder ask)
#   ssh yuno@$IP 'bash -s -- --ask-run --no-project' < ...    (the same on the research pipeline: a run that delivers out/report.md)
#   ssh yuno@$IP 'bash -s -- --questions' < ...               (pending questions: id, run, text, options)
#   ssh yuno@$IP 'bash -s -- --answer <question id> "Portuguese"' < ...
#   ssh yuno@$IP 'bash -s -- --decline <question id>' < ...
#   ssh yuno@$IP 'bash -s -- --run <run id>' < ...            (status, visits with question_id, the ask_operator calls)
#   ssh yuno@$IP 'bash -s -- --approve-run <run id>' < ...    (approve the run's pending gate or tool approvals, not its questions)
#   ssh yuno@$IP 'bash -s -- --artifacts <run id>' < ...      (the list, and the download headers of the first one)
#
# Everything is one function called at the end with stdin from /dev/null, so no child
# command can read the rest of the script (docs/deploy.md). Output is JSON summaries with
# ids, statuses and URLs; the token never appears.
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

  local action="" run="" qid="" answer="" with_project=true
  while [ $# -gt 0 ]; do
    case $1 in
      --version) action=version ;;
      --ask-run) action=ask_run ;;
      --no-project) with_project=false ;;
      --questions) action=questions ;;
      --answer) action=answer; qid=$2; answer=$3; shift 2 ;;
      --decline) action=decline; qid=$2; shift ;;
      --run) action=run; run=$2; shift ;;
      --approve-run) action=approve_run; run=$2; shift ;;
      --artifacts) action=artifacts; run=$2; shift ;;
      *) echo "unknown flag $1" >&2; exit 2 ;;
    esac
    shift
  done

  case $action in
    version)
      api GET /api/status | jq_ "print(json.dumps({k: d.get(k) for k in ('version','ok','scheduler')}))" ;;
    ask_run)
      local body
      if $with_project; then
        body=$(python3 -c "import json,sys; print(json.dumps({'workflow_id': sys.argv[1], 'project_id': sys.argv[2], 'input': {'task': sys.argv[3]}}))" \
          "$(workflow_id 'Dev pipeline')" "$(project_id)" \
          "Add a function greet(name) to the project that returns a greeting, with a test. Before writing it, ask the operator with ask_operator which language the greeting should be in (offer the options English, Portuguese and Spanish) and use the answer.")
      else
        body=$(python3 -c "import json,sys; print(json.dumps({'workflow_id': sys.argv[1], 'input': {'topic': sys.argv[2]}}))" \
          "$(workflow_id 'Research pipeline')" \
          "Write a one-page report on the trade-offs between polling and webhooks for a small integration, and put the finished report under the run's out/ directory.")
      fi
      api POST /api/runs "$body" | jq_ "print(json.dumps({k: d.get(k) for k in ('id','status','trigger','project_id')} if 'id' in d else d))" ;;
    questions)
      api GET "/api/approvals?status=pending" | jq_ "[print(json.dumps({'question_id': a['id'], 'run_id': a.get('run_id'), 'node': a.get('node_id'), 'text': a['message'][:200], 'options': a.get('options'), 'expires_at': a.get('expires_at')})) for a in d if a.get('kind')=='question']" ;;
    answer)
      body=$(python3 -c "import json,sys; print(json.dumps({'answer': sys.argv[1]}))" "$answer")
      api POST "/api/approvals/$qid/answer" "$body" | jq_ "print(json.dumps({k: d.get(k) for k in ('id','kind','status','answer','decided_by')} if 'id' in d else d))" ;;
    decline)
      api POST "/api/approvals/$qid/reject" '{"reason":"decide yourself"}' | jq_ "print(json.dumps({k: d.get(k) for k in ('id','kind','status','reason')} if 'id' in d else d))" ;;
    run)
      api GET "/api/runs/$run" | jq_ "print(json.dumps({k: d.get(k) for k in ('id','status','current_node','step_count','cost_usd_micros','failure_reason','retries')}))"
      echo "visits:"
      api GET "/api/runs/$run/trace" | jq_ "[print(json.dumps({'node': n['node_id'], 'visit': n['visit'], 'status': n['status'], 'question_id': n.get('question_id')})) for n in d['nodes']]"
      echo "questions:"
      api GET "/api/runs/$run/trace" | jq_ "[print(json.dumps({'id': a['id'], 'status': a['status'], 'text': a['message'][:120], 'answer': a.get('answer'), 'decided_by': a.get('decided_by')})) for a in d['approvals'] if a.get('kind')=='question']"
      echo "ask_operator calls:"
      api GET "/api/runs/$run/trace" | jq_ "[print(json.dumps({'status': t['status'], 'output': (json.dumps(t.get('output'))[:200] if t.get('output') else None)})) for t in d['tool_calls'] if t['tool_name'] in ('ask_operator','yuno__ask_operator')]" ;;
    approve_run)
      local ids
      ids=$(api GET "/api/approvals?status=pending&run_id=$run" | jq_ "[print(a['id']) for a in d if a.get('run_id')=='$run' and a.get('kind')!='question']")
      [ -n "$ids" ] || { echo "no pending approval (other than a question) for run $run"; exit 0; }
      for id in $ids; do
        api POST "/api/approvals/$id/approve" '{}' | jq_ "print(json.dumps({k: d.get(k) for k in ('id','kind','status','message')} if 'id' in d else d))"
      done ;;
    artifacts)
      local list first
      list=$(api GET "/api/runs/$run/artifacts")
      echo "$list" | jq_ "[print(json.dumps({k: a.get(k) for k in ('id','path','size','content_type','generated','node_id','visit')})) for a in d]"
      first=$(echo "$list" | jq_ "print(d[0]['id'] if d else '')")
      if [ -n "$first" ]; then
        echo "download headers of $first:"
        curl -sS -D - -o /dev/null -H "Authorization: Bearer $token" "$base/api/runs/$run/artifacts/$first" | grep -i '^HTTP/\|^content-type\|^content-disposition\|^x-content-type-options\|^cache-control\|^content-length'
      fi ;;
    *) echo "give one of --version, --ask-run [--no-project], --questions, --answer ID TEXT, --decline ID, --run ID, --approve-run ID, --artifacts ID" >&2; exit 2 ;;
  esac
}

main "$@" </dev/null
