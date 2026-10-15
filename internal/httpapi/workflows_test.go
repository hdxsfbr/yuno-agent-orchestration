package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"yuno/internal/store"
)

// testAgent creates an agent and returns its id.
func testAgent(t *testing.T, name string) string {
	t.Helper()
	a := &store.Agent{Name: name, Model: "claude-opus-5-5"}
	mustDo(t, store.CreateAgent(context.Background(), testPool, a))
	return a.ID
}

// validDefinition is a start → agent → end workflow for agentID.
func validDefinition(agentID string) string {
	return `{"input_schema":{"type":"object","properties":{"task":{"type":"string"}},"required":["task"]},
		"nodes":[{"id":"start","kind":"start"},
			{"id":"coder","kind":"agent","agent_id":"` + agentID + `","task":"Do {{.input.task}} {{with .state.reviewer}}{{.feedback}}{{end}} {{.state.coder.summary}}",
			 "output_schema":{"type":"object","properties":{"summary":{"type":"string"}}}},
			{"id":"end","kind":"end"}],
		"edges":[{"from":"start","to":"coder"},{"from":"coder","to":"end"}],
		"limits":{"max_steps":10},
		"ui":{"positions":{"start":{"x":0,"y":0}}}}`
}

func TestWorkflowCRUD(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	agentID := testAgent(t, "wf-crud")

	code, body := do(t, ts, http.MethodPost, "/api/workflows", `{"name":"Pipeline","description":"d","definition":`+validDefinition(agentID)+`}`)
	var wf store.Workflow
	if code != http.StatusCreated || json.Unmarshal(body, &wf) != nil || wf.Version != 1 || wf.Name != "Pipeline" {
		t.Fatalf("create: %d %s", code, body)
	}
	if !strings.Contains(string(wf.Definition), `"positions"`) {
		t.Fatalf("unknown definition keys dropped: %s", wf.Definition)
	}
	if code, body = do(t, ts, http.MethodGet, "/api/workflows/"+wf.ID, ""); code != http.StatusOK {
		t.Fatalf("get: %d %s", code, body)
	}
	code, body = do(t, ts, http.MethodGet, "/api/workflows", "")
	var list []store.Workflow
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil ||
		!slices.ContainsFunc(list, func(w store.Workflow) bool { return w.ID == wf.ID }) ||
		slices.ContainsFunc(list, func(w store.Workflow) bool { return w.TemplateKey != nil }) {
		t.Fatalf("list: %d (template rows must not be listed)", code)
	}

	// PUT bumps the version and accepts back what GET returned.
	var fetched map[string]any
	got, _ := json.Marshal(wf)
	_ = json.Unmarshal(got, &fetched)
	fetched["name"] = "Pipeline 2"
	put, _ := json.Marshal(fetched)
	code, body = do(t, ts, http.MethodPut, "/api/workflows/"+wf.ID, string(put))
	var updated store.Workflow
	if code != http.StatusOK || json.Unmarshal(body, &updated) != nil || updated.Version != 2 || updated.Name != "Pipeline 2" {
		t.Fatalf("update: %d %s", code, body)
	}
	if code, body = do(t, ts, http.MethodPut, "/api/workflows/"+wf.ID, `{"name":"x","definition":{"nodes":[]}}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid update: %d %s", code, body)
	}
	if code, _ = do(t, ts, http.MethodDelete, "/api/workflows/"+wf.ID, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		if code, _ = do(t, ts, m, "/api/workflows/"+wf.ID, ""); code != http.StatusNotFound {
			t.Fatalf("%s after delete: %d", m, code)
		}
	}

	// Platform-managed rows (templates, quick-run wrappers) are read-only.
	key := "quickrun:" + agentID
	managed := &store.Workflow{Name: "Quick run", Definition: json.RawMessage(validDefinition(agentID)), TemplateKey: &key}
	mustDo(t, store.CreateWorkflow(context.Background(), pool, managed))
	if code, _ = do(t, ts, http.MethodPut, "/api/workflows/"+managed.ID, `{"name":"x","definition":`+validDefinition(agentID)+`}`); code != http.StatusConflict {
		t.Fatalf("put on a managed row: %d", code)
	}
	if code, _ = do(t, ts, http.MethodDelete, "/api/workflows/"+managed.ID, ""); code != http.StatusConflict {
		t.Fatalf("delete of a managed row: %d", code)
	}
	if code, _ = do(t, ts, http.MethodGet, "/api/workflows/"+managed.ID, ""); code != http.StatusOK {
		t.Fatalf("get of a managed row: %d", code)
	}
	if code, body = do(t, ts, http.MethodPost, "/api/workflows", `{"definition":`+validDefinition(agentID)+`}`); code != http.StatusUnprocessableEntity ||
		!slices.Contains(detailPaths(t, body), "name") {
		t.Fatalf("missing name: %d %s", code, body)
	}
}

// T5.6.1 (API side): each structural rule, and each save-time check, produces its
// path-qualified detail.
func TestValidateWorkflow(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	agentID := testAgent(t, "wf-validate")
	agent := func(id, extra string) string {
		return `{"id":"` + id + `","kind":"agent","agent_id":"` + agentID + `","task":"t"` + extra + `}`
	}
	def := func(nodes, edges string, extra string) string {
		return `{"definition":{"nodes":[` + nodes + `],"edges":[` + edges + `]` + extra + `}}`
	}
	const start, end = `{"id":"start","kind":"start"}`, `{"id":"end","kind":"end"}`
	tests := []struct {
		name, body string
		wantPath   string
		wantMsg    string
	}{
		{"no start", def(agent("a", "")+","+end, `{"from":"a","to":"end"}`, ""), "nodes", "exactly one start node"},
		{"two starts", def(start+`,{"id":"s2","kind":"start"},`+end, `{"from":"start","to":"end"},{"from":"s2","to":"end"}`, ""), "nodes", "exactly one start node required, found 2"},
		{"no end", def(start+","+agent("a", ""), `{"from":"start","to":"a"},{"from":"a","to":"start"}`, ""), "nodes", "at least one end node"},
		{"bad node id", def(start+","+agent("Bad-Id", "")+","+end, `{"from":"start","to":"Bad-Id"},{"from":"Bad-Id","to":"end"}`, ""), "nodes[1].id", "must match"},
		{"duplicate id", def(start+","+agent("a", "")+","+agent("a", "")+","+end, `{"from":"start","to":"a"},{"from":"a","to":"end"}`, ""), "nodes[2].id", "duplicate 'a'"},
		{"unknown edge target", def(start+","+agent("a", "")+","+end, `{"from":"start","to":"a"},{"from":"a","to":"reviewr"},{"from":"a","to":"end"}`, ""), "edges[1].to", "unknown node 'reviewr'"},
		{"no outgoing edge", def(start+","+agent("a", "")+","+end, `{"from":"start","to":"a"},{"from":"start","to":"end"}`, ""), "nodes[1]", "has no outgoing edge"},
		{"unreachable node", def(start+","+agent("a", "")+","+end, `{"from":"start","to":"end"},{"from":"a","to":"end"}`, ""), "nodes[1]", "not reachable from start"},
		{"agent node without task", def(start+`,{"id":"a","kind":"agent","agent_id":"`+agentID+`"},`+end, `{"from":"start","to":"a"},{"from":"a","to":"end"}`, ""), "nodes[1].task", "required"},
		{"unknown kind", def(start+`,{"id":"a","kind":"robot"},`+end, `{"from":"start","to":"a"},{"from":"a","to":"end"}`, ""), "nodes[1].kind", "unknown kind 'robot'"},
		{"unknown agent", def(start+`,{"id":"a","kind":"agent","agent_id":"no-such-agent","task":"t"},`+end, `{"from":"start","to":"a"},{"from":"a","to":"end"}`, ""), "nodes[1].agent_id", "no agent with id 'no-such-agent'"},
		{"agent_name outside a template", def(start+`,{"id":"a","kind":"agent","agent_name":"Coder","task":"t"},`+end, `{"from":"start","to":"a"},{"from":"a","to":"end"}`, ""), "nodes[1].agent_name", "only templates name agents"},
		{"task template error", def(start+","+agent("a", `,"task":"Do {{.input.task"`)+","+end, `{"from":"start","to":"a"},{"from":"a","to":"end"}`, ""), "nodes[1].task", "line 1:"},
		{"message template error", def(start+`,{"id":"gate","kind":"approval","message":"{{nofunc .state}}"},`+end, `{"from":"start","to":"gate"},{"from":"gate","to":"end"}`, ""), "nodes[1].message", "not defined"},
		{"output schema does not compile", def(start+","+agent("a", `,"output_schema":{"type":"nope"}`)+","+end, `{"from":"start","to":"a"},{"from":"a","to":"end"}`, ""), "nodes[1].output_schema", "not a valid JSON Schema"},
		{"input schema does not compile", def(start+","+end, `{"from":"start","to":"end"}`, `,"input_schema":{"type":5}`), "input_schema", "not a valid JSON Schema"},
		{"state schema does not compile", def(start+","+end, `{"from":"start","to":"end"}`, `,"state_schema":{"required":"x"}`), "state_schema", "not a valid JSON Schema"},
		{"negative limit", def(start+","+end, `{"from":"start","to":"end"}`, `,"limits":{"max_steps":-1}`), "limits.max_steps", "must not be negative"},
		{"definition not an object", `{"definition":[1]}`, "definition", "must be an object"},
		{"definition missing", `{}`, "definition", "must be an object"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, body := do(t, ts, http.MethodPost, "/api/workflows/validate", tc.body)
			if code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d: %s", code, body)
			}
			var details []fieldError
			_ = json.Unmarshal(decodeError(t, body).Error.Details, &details)
			if !slices.ContainsFunc(details, func(d fieldError) bool {
				return d.Path == tc.wantPath && strings.Contains(d.Message, tc.wantMsg)
			}) {
				t.Fatalf("details %+v, want %s: …%s…", details, tc.wantPath, tc.wantMsg)
			}
		})
	}
	t.Run("valid", func(t *testing.T) {
		code, body := do(t, ts, http.MethodPost, "/api/workflows/validate", `{"definition":`+validDefinition(agentID)+`}`)
		if code != http.StatusOK || strings.TrimSpace(string(body)) != `{"valid":true}` {
			t.Fatalf("status %d: %s", code, body)
		}
	})
	t.Run("start to end only", func(t *testing.T) {
		if code, body := do(t, ts, http.MethodPost, "/api/workflows/validate", def(start+","+end, `{"from":"start","to":"end"}`, "")); code != http.StatusOK {
			t.Fatalf("status %d: %s", code, body)
		}
	})
}
