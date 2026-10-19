package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// reviewPanelDir copies the shipped Review panel into a directory of its own under a key
// unique to the test, with every agent name made unique too, and creates those agents:
// the shipped file is exercised as it is, without depending on the agents other tests
// created under its real names.
func reviewPanelDir(t *testing.T, sfx string, skip ...string) (dir, key string, agents map[string]string) {
	t.Helper()
	raw, err := os.ReadFile("../../templates/legacy/review_panel.json")
	mustDo(t, err)
	var doc map[string]any
	mustDo(t, json.Unmarshal(raw, &doc))
	key = "review_panel_" + sfx
	doc["key"] = key
	agents = map[string]string{} // node id → agent id
	nodes, _ := doc["definition"].(map[string]any)["nodes"].([]any)
	for _, n := range nodes {
		node := n.(map[string]any)
		name, _ := node["agent_name"].(string)
		if name == "" {
			continue
		}
		node["agent_name"] = name + "-" + sfx
		if !slices.Contains(skip, name) {
			agents[node["id"].(string)] = testAgent(t, name+"-"+sfx)
		}
	}
	out, err := json.Marshal(doc)
	mustDo(t, err)
	dir = t.TempDir()
	mustDo(t, os.WriteFile(filepath.Join(dir, key+".json"), out, 0o644))
	return dir, key, agents
}

// T5.26.13: the shipped Review panel loads, lists, and instantiates over the agents its
// nodes name; the fork and the join come through whole, and a missing reviewer is reported
// at its node.
func TestReviewPanelTemplateInstantiates(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	sfx := uniqueSuffix()
	dir, key, agents := reviewPanelDir(t, sfx)
	s, ts := newTestServerDeps(t, Deps{Cfg: testConfig(), Pool: pool, TemplatesDir: dir}, &fakeEngine{pool: pool})
	mustDo(t, s.LoadTemplates(ctx))

	code, body := do(t, ts, http.MethodGet, "/api/workflow-templates", "")
	var tpls []workflowTemplate
	if code != http.StatusOK || json.Unmarshal(body, &tpls) != nil {
		t.Fatalf("list: %d %s", code, body)
	}
	if !slices.ContainsFunc(tpls, func(w workflowTemplate) bool { return w.Key == key && w.Name == "Review panel" }) {
		t.Fatalf("templates = %s", body)
	}

	code, body = do(t, ts, http.MethodPost, "/api/workflow-templates/"+key+"/instantiate", `{"name":"Panel `+sfx+`"}`)
	var wf store.Workflow
	if code != http.StatusCreated || json.Unmarshal(body, &wf) != nil || wf.Name != "Panel "+sfx || wf.FromTemplate == nil || *wf.FromTemplate != key {
		t.Fatalf("instantiate: %d %s", code, body)
	}
	def, err := workflow.Parse(wf.Definition)
	mustDo(t, err)
	mustDo(t, def.ValidateStructure())
	for id, agentID := range agents {
		if n, ok := def.Node(id); !ok || n.AgentID != agentID || n.AgentName != "" {
			t.Errorf("%s = %+v, want agent %s", id, n, agentID)
		}
	}
	if p, _ := def.Node("panel"); p == nil || p.Kind != workflow.KindFork || p.Join != "merge" {
		t.Errorf("panel = %+v", p)
	}
	if m, _ := def.Node("merge"); m == nil || m.Kind != workflow.KindJoin || m.Checkout != "" {
		t.Errorf("merge = %+v", m)
	}
	if !def.Project {
		t.Error("the instance does not work on a project")
	}

	// One reviewer missing: 422 at the node that names it.
	dir, key, _ = reviewPanelDir(t, uniqueSuffix(), "Performance Reviewer")
	s, ts = newTestServerDeps(t, Deps{Cfg: testConfig(), Pool: pool, TemplatesDir: dir}, &fakeEngine{pool: pool})
	mustDo(t, s.LoadTemplates(ctx))
	code, body = do(t, ts, http.MethodPost, "/api/workflow-templates/"+key+"/instantiate", `{}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("a missing reviewer: %d %s", code, body)
	}
	var details []fieldError
	mustDo(t, json.Unmarshal(decodeError(t, body).Error.Details, &details))
	if len(details) != 1 || details[0].Path != "nodes[4].agent_name" {
		t.Fatalf("details = %+v", details)
	}
}
