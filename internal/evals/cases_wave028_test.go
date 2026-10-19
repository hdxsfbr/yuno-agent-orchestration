package evals

import (
	"encoding/json"
	"os"
	"testing"

	"yuno/internal/seed"
	"yuno/internal/store"
)

// Case 028 scripts the runs the platform starts by itself, on the shipped wave and ship
// templates and the seed's factory agents. No api is needed to know that those scripts fit
// the pipelines: they are played through the model of simulate_test.go on the templates'
// real graphs, schemas and tasks, with each agent limited to its seeded allowlist. The
// gates the platform keeps (the draft merge request's approval, the ready call that
// auto_after lets through) are left to the live run: here open_merge_request is allowed.
func TestCase028PlatformScriptsPlayOutOnTheShippedTemplates(t *testing.T) {
	c := shippedCase(t, "028-wave-auto-advance-pause")
	agents := map[string]AgentSpec{}
	for _, a := range seed.Agents {
		spec := AgentSpec{Name: a.Name, ToolAllowlist: a.ToolAllowlist}
		if a.Name == "Integrator" {
			spec.InteractionRules = map[string]string{"open_merge_request": "auto"}
		}
		agents[a.Name] = spec
	}
	definition := func(key string) map[string]any {
		t.Helper()
		raw, err := os.ReadFile("../../templates/" + key + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var tpl struct {
			Definition map[string]any `json:"definition"`
		}
		if err := json.Unmarshal(raw, &tpl); err != nil {
			t.Fatal(err)
		}
		return tpl.Definition
	}
	approve := func(n int) []ApprovalAction {
		out := make([]ApprovalAction, n)
		for i := range out {
			out[i] = ApprovalAction{Decision: DecisionApprove}
		}
		return out
	}

	type play struct {
		trigger   string
		template  string
		input     map[string]any
		approvals int
		wantLast  bool
	}
	plays := []play{
		{"issue", "wave_pipeline", map[string]any{"task": "[greeting T1] Greet a name\n\nFeature: greeting"}, 1, false},
		{"plan", "ship_pipeline", map[string]any{"plan_id": "p1", "feature": "greeting", "wave": 1, "last_wave": 2}, 1, false},
		{"issue", "wave_pipeline", map[string]any{"task": "[greeting T2] Print the greeting\n\nFeature: greeting"}, 1, false},
		{"plan", "ship_pipeline", map[string]any{"plan_id": "p1", "feature": "greeting", "wave": 2, "last_wave": 2}, 2, true},
	}
	if len(c.Project.PlatformRuns) != len(plays) {
		t.Fatalf("the case scripts %d platform runs, want %d", len(c.Project.PlatformRuns), len(plays))
	}
	for i, p := range plays {
		pr := c.Project.PlatformRuns[i]
		if pr.Trigger != p.trigger {
			t.Fatalf("platform run %d is for trigger %s, want %s", i+1, pr.Trigger, p.trigger)
		}
		sc := Case{ID: "028-play", Workflow: definition(p.template), Input: p.input, Agents: agents, Script: pr.Script,
			Actions: Actions{Approvals: approve(p.approvals)}}
		s := simulate(t, sc)
		for _, problem := range s.problems {
			t.Errorf("platform run %d (%s): %s", i+1, p.template, problem)
		}
		if s.status != store.RunSucceeded {
			t.Errorf("platform run %d (%s) ends %s (%s)", i+1, p.template, s.status, s.reason)
		}
		if p.template == "ship_pipeline" {
			if last, _ := s.state["integrator"].(map[string]any)["last"].(bool); last != p.wantLast {
				t.Errorf("platform run %d: the integrator reports last = %v, want %v", i+1, last, p.wantLast)
			}
		}
	}
}
