#!/usr/bin/env bash
# Live verification of wave 11 (DESIGN 5.26 parallel steps: fork and join) against a
# running instance, from its own host: the API token is read from the checkout's .env
# there and never printed. Run it over ssh with the script on stdin and the flags after
# "--":
#
#   ssh yuno@$IP 'bash -s -- --version' < scripts/deploy/verify-wave11.sh
#   ssh yuno@$IP 'bash -s -- --panel-run' < ...                (a Review panel run on the project with a small task)
#   ssh yuno@$IP 'bash -s -- --run <run id>' < ...             (status, current node, visits with their branch, the branch rows)
#   ssh yuno@$IP 'bash -s -- --overlap <run id>' < ...         (the branch visits' start and end: at once when they overlap)
#   ssh yuno@$IP 'bash -s -- --approve-run <run id>' < ...     (approve the run's pending gate or tool approvals)
#   ssh yuno@$IP 'bash -s -- --workflows' < ...                (the instantiated workflows and the templates, with their node kinds)
#   ssh yuno@$IP 'bash -s -- --agents' < ...                   (the seeded agents' names)
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
  workflow_id() { # by name, the instantiated one
    api GET /api/workflows | jq_ "print(next(w['id'] for w in d if w['name']=='$1' and not w.get('template_key')))"
  }

  local action="" run=""
  while [ $# -gt 0 ]; do
    case $1 in
      --version) action=version ;;
      --panel-run) action=panel_run ;;
      --run) action=run; run=$2; shift ;;
      --overlap) action=overlap; run=$2; shift ;;
      --approve-run) action=approve_run; run=$2; shift ;;
      --workflows) action=workflows ;;
      --agents) action=agents ;;
      *) echo "unknown flag $1" >&2; exit 2 ;;
    esac
    shift
  done

  case $action in
    version)
      api GET /api/status | jq_ "print(json.dumps({k: d.get(k) for k in ('version','ok','scheduler')}))" ;;
    panel_run)
      local body
      body=$(python3 -c "import json,sys; print(json.dumps({'workflow_id': sys.argv[1], 'project_id': sys.argv[2], 'input': {'task': sys.argv[3]}}))" \
        "$(workflow_id 'Review panel')" "$(project_id)" \
        "Add a function slugify(text) to the project that lowercases the text, replaces runs of non-alphanumeric characters with one hyphen and trims hyphens at both ends, with tests for the empty string, spaces and punctuation.")
      api POST /api/runs "$body" | jq_ "print(json.dumps({k: d.get(k) for k in ('id','status','trigger','project_id')} if 'id' in d else d))" ;;
    run)
      api GET "/api/runs/$run" | jq_ "print(json.dumps({k: d.get(k) for k in ('id','status','current_node','step_count','cost_usd_micros','failure_reason','retries')}))"
      echo "visits:"
      api GET "/api/runs/$run/trace" | jq_ "[print(json.dumps({'node': n['node_id'], 'visit': n['visit'], 'status': n['status'], 'branch_id': n.get('branch_id'), 'output': (json.dumps(n.get('output'))[:120] if n['node_id'] in ('panel','merge') else None)})) for n in d['nodes']]"
      echo "branches:"
      api GET "/api/runs/$run/trace" | jq_ "[print(json.dumps({k: b.get(k) for k in ('id','head','status','current_node','failure_reason','started_at','finished_at')})) for b in d.get('branches', [])]" ;;
    overlap)
      api GET "/api/runs/$run/trace" | jq_ "
branches = {b['id']: b['head'] for b in d.get('branches', [])}
rows = [n for n in d['nodes'] if n.get('branch_id')]
for n in rows:
    print(json.dumps({'branch': branches.get(n['branch_id']), 'node': n['node_id'], 'visit': n['visit'], 'started_at': n.get('started_at'), 'finished_at': n.get('finished_at'), 'session_key': n.get('session_key')}))
starts = [n['started_at'] for n in rows if n.get('started_at')]
ends = [n['finished_at'] for n in rows if n.get('finished_at')]
print(json.dumps({'branch_visits': len(rows), 'all_started_before_first_ended': bool(starts) and bool(ends) and max(starts) < min(ends)}))" ;;
    approve_run)
      local ids
      ids=$(api GET "/api/approvals?status=pending&run_id=$run" | jq_ "[print(a['id']) for a in d if a.get('run_id')=='$run' and a.get('kind')!='question']")
      [ -n "$ids" ] || { echo "no pending approval (other than a question) for run $run"; exit 0; }
      for id in $ids; do
        api POST "/api/approvals/$id/approve" '{}' | jq_ "print(json.dumps({k: d.get(k) for k in ('id','kind','status','message')} if 'id' in d else d))"
      done ;;
    workflows)
      api GET /api/workflows | jq_ "[print(json.dumps({'id': w['id'], 'name': w['name'], 'template_key': w.get('template_key'), 'from_template': w.get('from_template'), 'kinds': [n['kind'] for n in w['definition']['nodes']]})) for w in d]" ;;
    agents)
      api GET /api/agents | jq_ "[print(json.dumps({'name': a['name'], 'role': a.get('role'), 'model': a.get('model')})) for a in d]" ;;
    *) echo "give one of --version, --panel-run, --run ID, --overlap ID, --approve-run ID, --workflows, --agents" >&2; exit 2 ;;
  esac
}

main "$@" </dev/null
