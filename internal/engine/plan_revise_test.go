package engine

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// T5.29.11: plan_pipeline takes an optional revise_plan; with it the Analyst, Architect and
// Planner are told to amend the existing documents, without it they write them new.
func TestPlanPipelineRevisePlanInput(t *testing.T) {
	raw, err := os.ReadFile("../../templates/plan_pipeline.json")
	if err != nil {
		t.Fatal(err)
	}
	var tpl struct {
		Definition workflow.Definition `json:"definition"`
	}
	if err := json.Unmarshal(raw, &tpl); err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(tpl.Definition.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema.Properties["revise_plan"]; !ok || len(schema.Required) != 2 {
		t.Fatalf("input schema = %s", tpl.Definition.InputSchema)
	}

	for _, tc := range []struct {
		name, input string
		revise      bool
	}{
		{"new", `{"task":"x","feature":"x"}`, false},
		{"revise", `{"task":"add a refund","feature":"x","revise_plan":"7d3c4f9e-0000-4000-8000-000000000001"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			wf := templatePipeline(t, env, "plan_pipeline.json")
			sc := env.script(map[string][]string{
				"analyst":   {`{"prd_path":"docs/plans/x/PRD.md","summary":"prd","open_questions":[]}`},
				"scout":     {`{"gaps":[],"constraints":["go test ./..."]}`},
				"architect": {`{"architecture_path":"docs/plans/x/ARCHITECTURE.md","summary":"v1"}`},
			})
			r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(tc.input)})
			if r.Status != store.RunWaitingApproval || deref(r.CurrentNode) != "design_gate" {
				t.Fatalf("run = %s at %s %q", r.Status, deref(r.CurrentNode), deref(r.FailureReason))
			}
			for _, node := range []string{"analyst", "architect"} {
				m := sc.message(node, 1)
				if got := strings.Contains(m, "exists already"); node == "architect" && got != tc.revise {
					t.Errorf("%s message (revise=%v):\n%s", node, tc.revise, m)
				}
				if strings.Contains(m, "{{") || strings.Contains(m, "no value") {
					t.Errorf("%s message has an unrendered template:\n%s", node, m)
				}
			}
			if got := strings.Contains(sc.message("analyst", 1), "Revise the requirements"); got != tc.revise {
				t.Errorf("analyst message (revise=%v):\n%s", tc.revise, sc.message("analyst", 1))
			}
			if got := strings.Contains(sc.message("analyst", 1), "add a refund"); got != tc.revise {
				t.Errorf("analyst lacks the change request")
			}
		})
	}
}
