package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// M6 (wave 4 review): a run whose revise_plan cannot be revised is refused when it is
// triggered, before any node runs (DESIGN 5.29 rule 16).
func TestTriggerRefusesAnInvalidRevisePlan(t *testing.T) {
	env, _, p := projectEnv(t, map[string]string{"README.md": "x"}, false)
	ctx := context.Background()
	coder := env.agent(nil)
	def := line(workflow.Node{ID: "one", AgentID: coder.ID, Task: "write"})
	def.Project = true
	wf := env.createWorkflow(def)

	first, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	mkPlan := func(project, feature, status string) string {
		plan := &store.Plan{ProjectID: project, Feature: feature, PlanRunID: first.ID, Status: status}
		if err := store.CreatePlan(ctx, env.pool, plan); err != nil {
			t.Fatal(err)
		}
		return plan.ID
	}
	done := mkPlan(p.ID, "old", store.PlanDone)
	building := mkPlan(p.ID, "checkout", store.PlanBuilding)
	other := env.project("https://gitlab.test/g/other.git", false)
	foreign := mkPlan(other.ID, "checkout", store.PlanBuilding)

	run := func(feature, plan string) error {
		in, _ := json.Marshal(map[string]string{"feature": feature, "revise_plan": plan})
		_, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID, Input: in})
		return err
	}
	for _, tc := range []struct{ name, feature, plan, want string }{
		{"not an id", "checkout", "nope", `input.revise_plan: "nope" is not a plan id`},
		{"unknown plan", "checkout", "7d3c4f9e-0000-4000-8000-000000000001", "input.revise_plan: there is no plan 7d3c4f9e-0000-4000-8000-000000000001 to revise"},
		{"done plan", "old", done, fmt.Sprintf("input.revise_plan: plan %s is done and cannot be revised", done)},
		{"other project", "checkout", foreign, fmt.Sprintf("input.revise_plan: plan %s belongs to another project", foreign)},
		{"other feature", "search", building, fmt.Sprintf("input.revise_plan: plan %s is for feature %q, not %q", building, "checkout", "search")},
	} {
		var ve *ValidationError
		if err := run(tc.feature, tc.plan); !errors.As(err, &ve) || !slices.Contains(ve.Messages, tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	if err := run("checkout", building); err != nil {
		t.Errorf("open plan of the feature: %v", err)
	}
}
