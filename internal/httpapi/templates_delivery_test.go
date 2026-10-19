package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// T5.28.3: the shipped plan and wave pipelines load, list, validate and instantiate over
// the factory agents their nodes name (renamed per test so other tests' agents do not
// interfere); the instance works on a project and keeps its checks, edges and gates.
func TestDeliveryTemplatesInstantiate(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	sfx := uniqueSuffix()
	dir := t.TempDir()
	keys := map[string]string{"plan_pipeline": "Plan pipeline", "wave_pipeline": "Wave pipeline"}
	agentIDs := map[string]string{} // renamed agent name → id
	for key := range keys {
		raw, err := os.ReadFile("../../templates/" + key + ".json")
		mustDo(t, err)
		var doc map[string]any
		mustDo(t, json.Unmarshal(raw, &doc))
		doc["key"] = key + "_" + sfx
		for _, n := range doc["definition"].(map[string]any)["nodes"].([]any) {
			node := n.(map[string]any)
			name, _ := node["agent_name"].(string)
			if name == "" {
				continue
			}
			node["agent_name"] = name + "-" + sfx
			if agentIDs[name+"-"+sfx] == "" {
				agentIDs[name+"-"+sfx] = testAgent(t, name+"-"+sfx)
			}
		}
		out, err := json.Marshal(doc)
		mustDo(t, err)
		mustDo(t, os.WriteFile(filepath.Join(dir, key+".json"), out, 0o644))
	}
	s, ts := newTestServerDeps(t, Deps{Cfg: testConfig(), Pool: pool, TemplatesDir: dir}, &fakeEngine{pool: pool})
	mustDo(t, s.LoadTemplates(ctx))

	for key, name := range keys {
		code, body := do(t, ts, http.MethodPost, "/api/workflow-templates/"+key+"_"+sfx+"/instantiate", `{"name":"`+name+" "+sfx+`"}`)
		var wf store.Workflow
		if code != http.StatusCreated || json.Unmarshal(body, &wf) != nil || wf.Name != name+" "+sfx {
			t.Fatalf("%s instantiate: %d %s", key, code, body)
		}
		def, err := workflow.Parse(wf.Definition)
		mustDo(t, err)
		mustDo(t, def.ValidateStructure())
		if !def.Project {
			t.Errorf("%s does not work on a project", key)
		}
		if msgs := def.ValidateTemplates(json.RawMessage(`{"task":"x","feature":"x"}`)); len(msgs) > 0 {
			t.Errorf("%s templates: %v", key, msgs)
		}
		for _, n := range def.Nodes {
			if n.Kind == workflow.KindAgent && (n.AgentID == "" || n.AgentName != "" || len(n.OutputSchema) == 0) {
				t.Errorf("%s node %s = %+v", key, n.ID, n)
			}
		}
		if def.Limits.MaxLoopVisits != 3 || def.Limits.MaxCostUSD != 5 || def.Limits.MaxDurationSeconds != 3600 || def.Limits.ApprovalTimeoutSeconds != 86400 {
			t.Errorf("%s limits = %+v", key, def.Limits)
		}
		if key == "plan_pipeline" {
			w, _ := def.Node("waves")
			g, _ := def.Node("design_gate")
			p, _ := def.Node("plan_gate")
			if w == nil || len(w.Checks) != 1 || w.Checks[0] != workflow.CheckWavesPartition || g.OnReject != "architect" || p.OnReject != "slicer" {
				t.Errorf("plan wiring: waves %+v design_gate %+v plan_gate %+v", w, g, p)
			}
			var in struct {
				Required []string `json:"required"`
			}
			mustDo(t, json.Unmarshal(def.InputSchema, &in))
			if len(in.Required) != 2 {
				t.Errorf("input required = %v", in.Required)
			}
		}
	}
}
