package tools

import "testing"

// T5.3.1: effective policy = the agent's interaction rule, else require_approval for
// side-effecting tools, else auto; blocked or not allowlisted tools are refused.
func TestEffectivePolicy(t *testing.T) {
	deploy := &Tool{Name: "deploy_release", Risk: RiskSideEffecting}
	fetch := &Tool{Name: "http_fetch", Risk: RiskReadOnly}
	save := &Tool{Name: "memory_save", Risk: RiskComputational}
	finish := &Tool{Name: FinishName, Risk: RiskComputational}
	tests := []struct {
		name  string
		tool  *Tool
		rules map[string]string
		want  string
	}{
		{"side effect defaults to require_approval", deploy, nil, PolicyRequireApproval},
		{"side effect set to auto by the operator (D-009)", deploy, map[string]string{"deploy_release": "auto"}, PolicyAuto},
		{"side effect blocked", deploy, map[string]string{"deploy_release": "blocked"}, PolicyBlocked},
		{"rule under the model-facing name", deploy, map[string]string{"yuno__deploy_release": "auto"}, PolicyAuto},
		{"read-only defaults to auto", fetch, nil, PolicyAuto},
		{"read-only gated by the operator", fetch, map[string]string{"http_fetch": "require_approval"}, PolicyRequireApproval},
		{"computational defaults to auto", save, map[string]string{"deploy_release": "require_approval"}, PolicyAuto},
		{"computational blocked", save, map[string]string{"memory_save": "blocked"}, PolicyBlocked},
		{"empty rule value falls back", deploy, map[string]string{"deploy_release": ""}, PolicyRequireApproval},
		{"finish is always auto", finish, map[string]string{"finish": "blocked"}, PolicyAuto},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectivePolicy(tc.tool, tc.rules); got != tc.want {
				t.Fatalf("EffectivePolicy = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestPermitted(t *testing.T) {
	tests := []struct {
		name      string
		tool      string
		allowlist []string
		want      bool
	}{
		{"allowlisted", "deploy_release", []string{"read", "deploy_release"}, true},
		{"allowlisted under the model-facing name", "deploy_release", []string{"yuno__deploy_release"}, true},
		{"not allowlisted", "deploy_release", []string{"read", "finish"}, false},
		{"empty allowlist", "http_fetch", nil, false},
		{"finish needs no entry", FinishName, nil, true},
	}
	for _, tc := range tests {
		if got := Permitted(tc.tool, tc.allowlist); got != tc.want {
			t.Errorf("%s: Permitted = %v, want %v", tc.name, got, tc.want)
		}
	}
}
