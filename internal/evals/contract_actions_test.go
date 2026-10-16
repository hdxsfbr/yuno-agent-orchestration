package evals

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"yuno/internal/store"
)

// The wave 3 runner actions are clients of the schedule, tools and guardrail parts of
// api/openapi.yaml. These tests keep what the runner sends and reads inside that document.

func TestAgentPoliciesMatchTheOpenAPIDocument(t *testing.T) {
	doc := loadSpec(t)
	schemas := func(name string) map[string]any { return dig(t, doc, "components", "schemas", name) }

	g := store.Guardrails{MaxCostUSDPerRun: 1, MaxRunsPerHour: 1, BlockedTools: []string{"x"},
		BlockedInputPatterns: []store.BlockedPattern{{Name: "n", Tool: "t", Regex: "r"}}}
	m := store.MemoryPolicy{Enabled: true, MaxFacts: 1, FactTTLDays: 1, ConversationWindow: 1, ConversationTTLDays: 1}
	full := AgentRequest{Name: "n", Model: "m", ToolAllowlist: []string{"exec"}, Guardrails: g, MemoryPolicy: m}
	checkBody(t, "POST /api/agents", full, schemas("AgentInput"))
	checkBody(t, "guardrails", g, schemas("Guardrails"))
	checkBody(t, "memory_policy", m, schemas("MemoryPolicy"))
	pattern := dig(t, schemas("Guardrails"), "properties", "blocked_input_patterns", "items")
	checkBody(t, "blocked_input_patterns[0]", g.BlockedInputPatterns[0], pattern)

	// What a case's guardrails turn into on the wire is exactly this shape.
	spec := Guardrails{MaxCostUSDPerRun: 1, MaxRunsPerHour: 1, BlockedTools: []string{"x"},
		BlockedInputPatterns: []BlockedPattern{{Name: "n", Tool: "t", Regex: "r"}}}
	checkBody(t, "case guardrails", spec.Store(), schemas("Guardrails"))
}

func TestScheduleEndpointsMatchTheOpenAPIDocument(t *testing.T) {
	doc := loadSpec(t)
	paths := dig(t, doc, "paths")
	used := []struct{ method, path string }{
		{"post", "/api/schedules"},
		{"get", "/api/schedules/{id}"},
		{"delete", "/api/schedules/{id}"},
		{"get", "/api/tools"},
	}
	var missing []string
	for _, u := range used {
		if p, ok := paths[u.path].(map[string]any); !ok || p[u.method] == nil {
			missing = append(missing, strings.ToUpper(u.method)+" "+u.path)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("api/openapi.yaml lacks endpoints the runner calls: %v", missing)
	}

	input := refSchema(t, doc, dig(t, paths, "/api/schedules", "post", "requestBody", "content", "application/json", "schema"))
	req := ScheduleRequest{WorkflowID: "w", Kind: ScheduleKindDelay, RunAt: t0, Input: json.RawMessage(`{"a":1}`), Enabled: true}
	checkBody(t, "POST /api/schedules", req, input)
	kinds, _ := dig(t, input, "properties", "kind")["enum"].([]any)
	found := false
	for _, k := range kinds {
		found = found || k == ScheduleKindDelay
	}
	if !found {
		t.Errorf("the schedule kinds %v do not include %q", kinds, ScheduleKindDelay)
	}

	// The fields the runner reads back are declared on the Schedule response.
	props := dig(t, doc, "components", "schemas", "Schedule", "properties")
	view, _ := json.Marshal(ScheduleView{ID: "s", WorkflowID: "w", Kind: "delay", Enabled: true,
		NextRunAt: &t0, LastRunAt: &t0, LastRunID: ptr("r"), LastError: ptr("e")})
	var got map[string]any
	if err := json.Unmarshal(view, &got); err != nil {
		t.Fatal(err)
	}
	for k := range got {
		if _, ok := props[k]; !ok {
			t.Errorf("ScheduleView reads %q, which the Schedule schema does not declare", k)
		}
	}
	for _, want := range []string{"last_run_id", "last_error", "enabled"} {
		if _, ok := props[want]; !ok {
			t.Errorf("the Schedule schema lacks %q, which the runner polls", want)
		}
	}
}

// The runner finds the run a schedule created by the Schedule's last_run_id string, and
// a 429 by the Error schema's message; both are decoded from the documented shapes.
func TestRunnerDecodesTheDocumentedAnswers(t *testing.T) {
	var s ScheduleView
	if err := json.Unmarshal([]byte(`{"id":"s1","workflow_id":"w","workflow_name":"n","kind":"delay","tz":"UTC","enabled":false,
		"last_run_at":"2026-10-16T10:00:00Z","last_run_id":"run-1","input":{},"created_at":"2026-10-16T10:00:00Z"}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.RunID() != "run-1" || s.LastErrorText() != "" || s.Enabled {
		t.Errorf("schedule = %+v", s)
	}
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		code    string
		message string
		details []string
	}{
		{"rate limit without details", 429, `{"error":{"code":"rate_limited","message":"rate limit: 1 runs/hour for agent Scribe"}}`,
			"rate_limited", "rate limit: 1 runs/hour for agent Scribe", nil},
		// What the api sends (first live eval, case 019): details is an object.
		{"rate limit as the api sends it", 429, `{"error":{"code":"rate_limited","message":"rate limit: 1 runs/hour for agent Scribe","details":{"agent_id":"a1","limit":1}}}`,
			"rate_limited", "rate limit: 1 runs/hour for agent Scribe", nil},
		{"validation with details", 422, `{"error":{"code":"validation_failed","message":"invalid run","details":[{"path":"input.task","message":"missing"},{"message":"bad"}]}}`,
			"validation_failed", "invalid run", []string{"input.task: missing", "bad"}},
		{"not the Error schema", 502, `<html>bad gateway</html>`, "", "", nil},
	} {
		ae := decodeAPIError("POST", "/api/runs", tc.status, []byte(tc.body))
		if ae.Status != tc.status || ae.Code != tc.code || ae.Message != tc.message || !slices.Equal(ae.Details, tc.details) {
			t.Errorf("%s: error = %+v", tc.name, ae)
		}
	}
}
