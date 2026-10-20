package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// T5.28.3: the shipped plan, wave and ship pipelines load, list, validate and instantiate over
// the factory agents their nodes name (renamed per test so other tests' agents do not
// interfere); the instance works on a project and keeps its checks, edges and gates.
func TestDeliveryTemplatesInstantiate(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	sfx := uniqueSuffix()
	dir := t.TempDir()
	keys := map[string]string{"plan_pipeline": "Plan pipeline", "wave_pipeline": "Wave pipeline", "ship_pipeline": "Ship pipeline"}
	wantCost := map[string]float64{"plan_pipeline": 5, "wave_pipeline": 20, "ship_pipeline": 2}
	// A ticket's build takes many turns: the wave pipeline runs up to three hours.
	wantDuration := map[string]int{"plan_pipeline": 3600, "wave_pipeline": 10800, "ship_pipeline": 3600}
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
		sample := `{"task":"x","feature":"x"}`
		if key == "ship_pipeline" {
			sample = `{"plan_id":"p1","feature":"x","wave":1,"last_wave":2}`
		}
		if msgs := def.ValidateTemplates(json.RawMessage(sample)); len(msgs) > 0 {
			t.Errorf("%s templates: %v", key, msgs)
		}
		for _, n := range def.Nodes {
			if n.Kind == workflow.KindAgent && (n.AgentID == "" || n.AgentName != "" || len(n.OutputSchema) == 0) {
				t.Errorf("%s node %s = %+v", key, n.ID, n)
			}
		}
		if def.Limits.MaxLoopVisits != 3 || def.Limits.MaxCostUSD != wantCost[key] || def.Limits.MaxDurationSeconds != wantDuration[key] || def.Limits.ApprovalTimeoutSeconds != 86400 {
			t.Errorf("%s limits = %+v", key, def.Limits)
		}
		if key == "wave_pipeline" {
			r, _ := def.Node("integrator_ready")
			m, _ := def.Node("manual_test")
			d, _ := def.Node("integrator_draft")
			if _, gap := def.Node("gap_found"); gap || r == nil || r.AutoAfter != "manual_test" || m == nil || m.OnReject != "builder" ||
				d == nil || d.AutoAfter != "" {
				t.Errorf("wave wiring: integrator_ready %+v manual_test %+v integrator_draft %+v", r, m, d)
			}
			// D-084: the doc audit goes on to the ready step on "ok" and back to the Builder,
			// with the operator's decision, on "rework".
			routes := map[string]string{}
			for _, e := range def.OutgoingEdges("doc_audit") {
				if e.When != nil {
					routes[strings.Trim(string(e.When.Value), `"`)] = e.To
				}
			}
			if routes["ok"] != "integrator_ready" || routes["rework"] != "builder" {
				t.Errorf("doc_audit routes = %v", routes)
			}
		}
		if key == "ship_pipeline" {
			i, _ := def.Node("integrator")
			s, _ := def.Node("smoke_test")
			a, _ := def.Node("accept_feature")
			if i == nil || i.AutoAfter != "smoke_test" || s == nil || s.Kind != workflow.KindApproval || s.OnReject != "" ||
				a == nil || a.Kind != workflow.KindApproval || a.OnReject != "" {
				t.Errorf("ship wiring: integrator %+v smoke_test %+v accept_feature %+v", i, s, a)
			}
			if !strings.Contains(string(i.OutputSchema), "lessons") || !strings.Contains(i.Task, "advance_wave") || !strings.Contains(i.Task, "memory_save") ||
				!strings.Contains(i.Task, `paths ["CLAUDE.md"]`) || strings.Contains(i.Task, "with the branch") {
				t.Errorf("ship integrator = %+v", i)
			}
		}
		if key == "plan_pipeline" {
			w, _ := def.Node("waves")
			g, _ := def.Node("design_gate")
			p, _ := def.Node("plan_gate")
			if w == nil || len(w.Checks) != 1 || w.Checks[0] != workflow.CheckWavesPartition || g.OnReject != "architect" || p.OnReject != "slicer" {
				t.Errorf("plan wiring: waves %+v design_gate %+v plan_gate %+v", w, g, p)
			}
			if i, _ := def.Node("integrator"); i == nil || !strings.Contains(i.Task, "publish_tickets") ||
				!strings.Contains(string(i.OutputSchema), "issues_count") || !strings.Contains(string(i.OutputSchema), "plan_id") {
				t.Errorf("plan integrator = %+v", i)
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
