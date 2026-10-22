package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"yuno/internal/engine"
	"yuno/internal/project"
	"yuno/internal/store"
)

func TestCreateRunValidation(t *testing.T) {
	fe := &fakeEngine{}
	_, ts := newTestServer(t, nil, fe, nil)
	tests := []struct {
		name      string
		body      string
		wantPaths []string
	}{
		{name: "nothing to run", body: `{}`, wantPaths: []string{""}},
		{name: "both targets", body: `{"workflow_id":"w","agent_id":"a","task":"t"}`, wantPaths: []string{"agent_id"}},
		{name: "agent without task", body: `{"agent_id":"a"}`, wantPaths: []string{"task"}},
		{name: "input not an object", body: `{"workflow_id":"w","input":[1]}`, wantPaths: []string{"input"}},
		{name: "bad script id", body: `{"agent_id":"a","task":"t","script_id":"has space"}`, wantPaths: []string{"script_id"}},
		{name: "script id the provider cannot read back", body: `{"agent_id":"a","task":"t","script_id":"case-"}`, wantPaths: []string{"script_id"}},
		{name: "a trigger only the platform starts", body: `{"agent_id":"a","task":"t","trigger":"schedule"}`, wantPaths: []string{"trigger"}},
		{name: "unknown trigger", body: `{"agent_id":"a","task":"t","trigger":"cron"}`, wantPaths: []string{"trigger"}},
		{name: "project_id not a string", body: `{"agent_id":"a","task":"t","project_id":true}`, wantPaths: []string{"project_id"}},
		{name: "the removed project flag", body: `{"agent_id":"a","task":"t","project":true}`, wantPaths: []string{"project"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, body := do(t, ts, http.MethodPost, "/api/runs", tc.body)
			if code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d: %s", code, body)
			}
			if got := detailPaths(t, body); !slices.Equal(got, tc.wantPaths) {
				t.Fatalf("paths = %q, want %q", got, tc.wantPaths)
			}
		})
	}
	if len(fe.triggers) != 0 {
		t.Fatalf("engine triggered %d times on invalid input", len(fe.triggers))
	}
}

func TestCreateRunMapsToTrigger(t *testing.T) {
	fe := &fakeEngine{}
	_, ts := newTestServer(t, nil, fe, nil)
	code, body := do(t, ts, http.MethodPost, "/api/runs", `{"agent_id":"ag-1","task":"write fizzbuzz","model":"scripted/scripted-1",
		"script_id":"fizz.1","limits":{"max_steps":5,"max_cost_usd":0.5},"project_id":" pr-1 "}`)
	if code != http.StatusCreated {
		t.Fatalf("status %d: %s", code, body)
	}
	if len(fe.triggers) != 1 {
		t.Fatalf("triggers = %d", len(fe.triggers))
	}
	// The engine checks project_id against the workflow (DESIGN 5.17 rule 4); the API
	// passes it through, trimmed.
	if fe.triggers[0].ProjectID != "pr-1" {
		t.Fatalf("project_id reached the trigger request as %q", fe.triggers[0].ProjectID)
	}
	got := fe.triggers[0]
	if got.AgentID != "ag-1" || got.Task != "write fizzbuzz" || got.Model != "scripted/scripted-1" || got.ScriptID != "fizz.1" ||
		got.Trigger != store.TriggerManual || got.TriggerRef != "" || got.WorkflowID != "" || got.Input != nil ||
		got.Limits.MaxSteps != 5 || got.Limits.MaxCostUSD != 0.5 {
		t.Fatalf("trigger request = %+v", got)
	}

	code, _ = do(t, ts, http.MethodPost, "/api/runs", `{"workflow_id":"wf-1","input":{"repo":"x"}}`)
	if code != http.StatusCreated || string(fe.triggers[1].Input) != `{"repo":"x"}` || fe.triggers[1].WorkflowID != "wf-1" ||
		fe.triggers[1].ProjectID != "" {
		t.Fatalf("workflow trigger: %d %+v", code, fe.triggers[1])
	}

	// The eval runner tags its runs (audit #30); manual says so explicitly too.
	for i, tr := range []string{store.TriggerEval, store.TriggerManual} {
		code, body = do(t, ts, http.MethodPost, "/api/runs", `{"agent_id":"ag-1","task":"t","trigger":"`+tr+`"}`)
		if code != http.StatusCreated || len(fe.triggers) != 3+i || fe.triggers[2+i].Trigger != tr {
			t.Fatalf("trigger %s: %d %s, engine got %+v", tr, code, body, fe.triggers)
		}
	}
}

func TestCreateRunEngineErrors(t *testing.T) {
	budget := &project.BudgetError{ProjectID: "pr-1", ProjectName: "Playground", Window: project.WindowDay, CapMicros: 5_000_000,
		SpendMicros: 5_250_000, WindowEnd: time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)}
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "unknown agent", err: fmt.Errorf("agent ag-1: %w", store.ErrNotFound), want: http.StatusNotFound},
		{name: "engine validation", err: fmt.Errorf("trigger: %w", &engine.ValidationError{Messages: []string{"limits.max_steps: must not be negative", "bare message"}}), want: http.StatusUnprocessableEntity},
		{name: "project budget", err: fmt.Errorf("trigger: %w", budget), want: http.StatusTooManyRequests},
		{name: "other", err: fmt.Errorf("boom"), want: http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ts := newTestServer(t, nil, &fakeEngine{triggerErr: tc.err}, nil)
			code, body := do(t, ts, http.MethodPost, "/api/runs", `{"agent_id":"ag-1","task":"t"}`)
			if code != tc.want {
				t.Fatalf("status %d: %s", code, body)
			}
			e := decodeError(t, body).Error
			if tc.want == http.StatusUnprocessableEntity {
				if got := detailPaths(t, body); !slices.Equal(got, []string{"limits.max_steps", ""}) {
					t.Fatalf("paths = %q", got)
				}
			}
			if tc.want == http.StatusTooManyRequests {
				// DESIGN 5.19 rule 3: the refusal's text and the window's numbers in USD.
				var details map[string]any
				mustDo(t, json.Unmarshal(e.Details, &details))
				wantDetails := map[string]any{"project_id": "pr-1", "window": "day", "max_cost_usd": 5.0, "spent_usd": 5.25, "until": "2026-10-18T00:00:00Z"}
				if e.Code != "project_budget_reached" || e.Message != budget.Error() || fmt.Sprint(details) != fmt.Sprint(wantDetails) {
					t.Fatalf("error = %s %q %v, want project_budget_reached %q %v", e.Code, e.Message, details, budget.Error(), wantDetails)
				}
			}
		})
	}
}

// GET /api/runs?project_id= lists the runs that work on the project, newest first, with
// status and limit applying as before; a project nobody ran on, or an unknown one, lists
// none (the page of a project whose runs are older than the newest hundreds needs this).
func TestListRunsByProject(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	sfx := uniqueSuffix()
	mine := seedProject(t, testCipher(t), "runs-a-"+sfx, acceptedTokenPrefix+sfx)
	other := seedProject(t, testCipher(t), "runs-b-"+sfx, acceptedTokenPrefix+sfx)
	idle := seedProject(t, testCipher(t), "runs-c-"+sfx, acceptedTokenPrefix+sfx)
	first, second, third := projectRun(t, mine), projectRun(t, mine), projectRun(t, mine)
	elsewhere := projectRun(t, other)
	for i, r := range []*store.Run{first, second, third} {
		if _, err := pool.Exec(context.Background(), `UPDATE runs SET created_at = now() - make_interval(secs => $2) WHERE id = $1`, r.ID, float64(100-i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(context.Background(), `UPDATE runs SET status = $2 WHERE id = $1`, second.ID, store.RunFailed); err != nil {
		t.Fatal(err)
	}
	// A run with no project, newer than all of them, must not leak in.
	plain, err := createTestRun(context.Background(), pool, store.TriggerManual, nil, store.Limits{})
	mustDo(t, err)

	ids := func(query string) []string {
		t.Helper()
		code, body := do(t, ts, http.MethodGet, "/api/runs?"+query, "")
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", query, code, body)
		}
		var runs []store.Run
		mustDo(t, json.Unmarshal(body, &runs))
		out := make([]string, len(runs))
		for i, r := range runs {
			out[i] = r.ID
		}
		return out
	}
	if got, want := ids("project_id="+mine.ID), []string{third.ID, second.ID, first.ID}; !slices.Equal(got, want) {
		t.Fatalf("project runs = %v, want %v (newest first)", got, want)
	}
	if got := ids("project_id=" + other.ID); !slices.Equal(got, []string{elsewhere.ID}) {
		t.Fatalf("other project = %v", got)
	}
	if got := ids("project_id=" + mine.ID + "&limit=2"); !slices.Equal(got, []string{third.ID, second.ID}) {
		t.Fatalf("limit 2 = %v", got)
	}
	if got := ids("project_id=" + mine.ID + "&status=failed"); !slices.Equal(got, []string{second.ID}) {
		t.Fatalf("status failed = %v", got)
	}
	if got := ids("project_id=" + idle.ID); got == nil || len(got) != 0 {
		t.Fatalf("a project with no runs = %#v", got)
	}
	if got := ids("project_id=no-such-project"); got == nil || len(got) != 0 {
		t.Fatalf("an unknown project = %#v", got)
	}
	// Without the filter every run is listed, the project's and the plain one.
	all := ids("limit=500")
	for _, id := range []string{first.ID, elsewhere.ID, plain.ID} {
		if !slices.Contains(all, id) {
			t.Fatalf("the unfiltered list lacks %s", id)
		}
	}
	// A project that was deleted keeps its runs listed.
	mustDo(t, store.DeleteProject(context.Background(), pool, mine.ID))
	if got := ids("project_id=" + mine.ID); len(got) != 3 {
		t.Fatalf("after deleting the project: %v", got)
	}
}

func TestRunEndpoints(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	fe := &fakeEngine{pool: pool}
	_, ts := newTestServer(t, pool, fe, nil)

	run, err := createTestRun(ctx, pool, store.TriggerManual, nil, store.Limits{MaxSteps: 3})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := do(t, ts, http.MethodGet, "/api/runs/"+run.ID, ""); code != http.StatusOK || !strings.Contains(string(body), run.ID) {
		t.Fatalf("get: %d %s", code, body)
	}
	if code, _ := do(t, ts, http.MethodGet, "/api/runs/does-not-exist", ""); code != http.StatusNotFound {
		t.Fatalf("get unknown: %d", code)
	}
	code, body := do(t, ts, http.MethodGet, "/api/runs?status=queued&limit=500", "")
	if code != http.StatusOK || !strings.Contains(string(body), run.ID) {
		t.Fatalf("list queued: %d", code)
	}
	var runs []store.Run
	_ = json.Unmarshal(body, &runs)
	for _, r := range runs {
		if r.Status != store.RunQueued {
			t.Fatalf("status filter leaked %s", r.Status)
		}
	}
	for _, q := range []string{"?limit=0", "?limit=501", "?limit=x", "?status=done"} {
		if code, _ := do(t, ts, http.MethodGet, "/api/runs"+q, ""); code != http.StatusUnprocessableEntity {
			t.Fatalf("list %s: %d", q, code)
		}
	}

	if code, body := do(t, ts, http.MethodGet, "/api/runs/"+run.ID+"/trace", ""); code != http.StatusOK || !strings.Contains(string(body), `"nodes":[]`) {
		t.Fatalf("trace: %d %s", code, body)
	}
	if code, _ := do(t, ts, http.MethodGet, "/api/runs/does-not-exist/trace", ""); code != http.StatusNotFound {
		t.Fatalf("trace unknown: %d", code)
	}

	code, body = do(t, ts, http.MethodPost, "/api/runs/"+run.ID+"/cancel", "")
	if code != http.StatusAccepted || !strings.Contains(string(body), `"cancel_requested":true`) {
		t.Fatalf("cancel: %d %s", code, body)
	}
	if fe.cancels[0] != run.ID+"/operator" {
		t.Fatalf("cancel by %q", fe.cancels[0])
	}
	if err := store.SetRunStatus(ctx, pool, run.ID, store.RunCancelled, nil); err != nil {
		t.Fatal(err)
	}
	code, body = do(t, ts, http.MethodPost, "/api/runs/"+run.ID+"/cancel", "")
	if code != http.StatusConflict || decodeError(t, body).Error.Code != "run_finished" {
		t.Fatalf("cancel terminal: %d %s", code, body)
	}
	if code, _ := do(t, ts, http.MethodPost, "/api/runs/does-not-exist/cancel", ""); code != http.StatusNotFound {
		t.Fatalf("cancel unknown: %d", code)
	}
}

// The list carries each run's agents and models: from its nodes and model calls once it
// ran, first appearance first; from the snapshot's agent nodes (and the model override)
// while it is still queued.
func TestListRunsParticipants(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)

	coder := &store.Agent{Name: "Coder " + store.NewID(), Role: "coder", SystemPrompt: "x", Model: "claude-sonnet-5-5", ToolAllowlist: []string{"exec"}}
	reviewer := &store.Agent{Name: "Reviewer " + store.NewID(), Role: "reviewer", SystemPrompt: "x", Model: "claude-opus-5-5", ToolAllowlist: []string{"read"}}
	for _, a := range []*store.Agent{coder, reviewer} {
		if err := store.CreateAgent(ctx, pool, a); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func(override string) json.RawMessage {
		snap := map[string]any{
			"nodes": []map[string]any{
				{"id": "start", "kind": "start"},
				{"id": "code", "kind": "agent", "agent_id": coder.ID},
				{"id": "review", "kind": "agent", "agent_id": reviewer.ID},
				{"id": "end", "kind": "end"},
			},
			"edges": []any{},
		}
		if override != "" {
			snap["run_overrides"] = map[string]any{"model": override}
		}
		b, _ := json.Marshal(snap)
		return b
	}
	newRun := func(snap json.RawMessage) *store.Run {
		t.Helper()
		w := &store.Workflow{Name: "participants", Definition: snap}
		if err := store.CreateWorkflow(ctx, pool, w); err != nil {
			t.Fatal(err)
		}
		run := &store.Run{WorkflowID: w.ID, WorkflowVersion: 1, WorkflowSnapshot: snap, Trigger: store.TriggerManual, Limits: store.Limits{MaxSteps: 3}}
		if err := store.CreateRun(ctx, pool, run); err != nil {
			t.Fatal(err)
		}
		return run
	}

	ran := newRun(snapshot(""))
	n1 := &store.RunNode{RunID: ran.ID, NodeID: "code", AgentID: &coder.ID}
	n2 := &store.RunNode{RunID: ran.ID, NodeID: "review", AgentID: &reviewer.ID}
	for _, n := range []*store.RunNode{n1, n2} {
		if err := store.CreateRunNode(ctx, pool, n); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Now().Add(-time.Minute)
	for i, mc := range []struct {
		node  string
		model string
	}{{n1.ID, "claude-sonnet-5-5"}, {n2.ID, "claude-opus-5-5"}, {n2.ID, "claude-sonnet-5-5"}} {
		m := &store.ModelCall{RunID: ran.ID, RunNodeID: mc.node, Provider: "anthropic", Model: mc.model, StartedAt: t0.Add(time.Duration(i) * time.Second)}
		if err := store.InsertModelCall(ctx, pool, m); err != nil {
			t.Fatal(err)
		}
	}
	overridden := newRun(snapshot("scripted/scripted-1"))
	plain := newRun(snapshot(""))
	// An OpenAI call is listed as the agent names its model (D-091).
	openai := newRun(snapshot(""))
	n3 := &store.RunNode{RunID: openai.ID, NodeID: "code", AgentID: &coder.ID}
	if err := store.CreateRunNode(ctx, pool, n3); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertModelCall(ctx, pool, &store.ModelCall{RunID: openai.ID, RunNodeID: n3.ID, Provider: "openai", Model: "gpt-6-astra", StartedAt: t0}); err != nil {
		t.Fatal(err)
	}

	code, body := do(t, ts, http.MethodGet, "/api/runs?limit=500", "")
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}
	var runs []store.Run
	if err := json.Unmarshal(body, &runs); err != nil {
		t.Fatal(err)
	}
	find := func(id string) store.Run {
		t.Helper()
		for _, r := range runs {
			if r.ID == id {
				return r
			}
		}
		t.Fatalf("run %s not listed", id)
		return store.Run{}
	}
	names := []string{coder.Name, reviewer.Name}
	if r := find(ran.ID); !slices.Equal(r.AgentNames, names) || !slices.Equal(r.Models, []string{"claude-sonnet-5-5", "claude-opus-5-5"}) {
		t.Fatalf("ran: agents %v models %v", r.AgentNames, r.Models)
	}
	if r := find(overridden.ID); !slices.Equal(r.AgentNames, names) || !slices.Equal(r.Models, []string{"scripted/scripted-1"}) {
		t.Fatalf("overridden: agents %v models %v", r.AgentNames, r.Models)
	}
	if r := find(plain.ID); !slices.Equal(r.AgentNames, names) || !slices.Equal(r.Models, []string{"claude-sonnet-5-5", "claude-opus-5-5"}) {
		t.Fatalf("plain: agents %v models %v", r.AgentNames, r.Models)
	}
	if r := find(openai.ID); !slices.Equal(r.Models, []string{"openai/gpt-6-astra"}) {
		t.Fatalf("openai: models %v", r.Models)
	}
}
