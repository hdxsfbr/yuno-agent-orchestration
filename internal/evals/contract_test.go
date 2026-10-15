package evals

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"yuno/api"
	"yuno/internal/store"
)

// The runner is a plain client of api/openapi.yaml. These tests check what it sends
// and which endpoints it calls against that document, so the two cannot drift apart.

func loadSpec(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(api.OpenAPI, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	return doc
}

func dig(t *testing.T, v any, path ...string) map[string]any {
	t.Helper()
	for i, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("openapi.yaml: %q is not a mapping", strings.Join(path[:i], "."))
		}
		if v, ok = m[p]; !ok {
			t.Fatalf("openapi.yaml: missing %q", strings.Join(path[:i+1], "."))
		}
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("openapi.yaml: %q is not a mapping", strings.Join(path, "."))
	}
	return m
}

// checkBody asserts that every key of body is declared in the schema's properties and
// that every required property is present.
func checkBody(t *testing.T, what string, body any, schema map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%s is not a JSON object: %s", what, raw)
	}
	props, _ := schema["properties"].(map[string]any)
	var unknown []string
	for k := range got {
		if _, ok := props[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("%s sends keys the api does not declare: %v (body %s)", what, unknown, raw)
	}
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			if _, ok := got[r.(string)]; !ok {
				t.Errorf("%s is missing required key %q (body %s)", what, r, raw)
			}
		}
	}
}

func TestRequestBodiesMatchTheOpenAPIDocument(t *testing.T) {
	doc := loadSpec(t)
	schemas := func(name string) map[string]any { return dig(t, doc, "components", "schemas", name) }

	fullLimits := store.Limits{MaxSteps: 1, MaxDurationSeconds: 1, MaxNodeIterations: 1, MaxLoopVisits: 1, MaxCostUSD: 1, MaxToolCalls: 1, ApprovalTimeoutSeconds: 1}
	checkBody(t, "Limits", fullLimits, schemas("Limits"))

	agent := AgentRequest{Name: "n", Role: "r", SystemPrompt: "p", Model: "m", ToolAllowlist: []string{"exec"}, Limits: fullLimits}
	checkBody(t, "POST /api/agents", agent, schemas("AgentInput"))

	run := RunRequest{AgentID: "a", Task: "t", Model: ScriptedModel, ScriptID: "s", Limits: fullLimits}
	checkBody(t, "POST /api/runs", run, schemas("RunCreate"))

	cases := shippedCases(t)
	for _, c := range cases {
		steps, err := c.ModelSteps()
		if err != nil {
			t.Fatal(err)
		}
		reg := scriptRegistration{ScriptID: c.ID + "-abc123", Steps: steps}
		checkBody(t, "POST /api/evals/scripts", reg, schemas("ScriptRegistration"))

		pattern, _ := dig(t, schemas("ScriptRegistration"), "properties", "script_id")["pattern"].(string)
		if pattern != "^[a-zA-Z0-9._-]{1,80}$" {
			t.Fatalf("script id pattern changed: %q", pattern)
		}
		for i, s := range steps {
			checkBody(t, c.ID+" script step", s, schemas("ScriptStep"))
			if s.Tool != nil {
				toolSchema := dig(t, schemas("ScriptStep"), "properties", "tool")
				checkBody(t, c.ID+" script tool", s.Tool, toolSchema)
				var args map[string]any
				if err := json.Unmarshal(s.Tool.Args, &args); err != nil {
					t.Errorf("%s step %d: tool args must be a JSON object: %v", c.ID, i+1, err)
				}
			}
			if s.Usage != nil {
				checkBody(t, c.ID+" script usage", s.Usage, dig(t, schemas("ScriptStep"), "properties", "usage"))
			}
		}
	}
}

func TestEndpointsExistInTheOpenAPIDocument(t *testing.T) {
	paths := dig(t, loadSpec(t), "paths")
	used := []struct{ method, path string }{
		{"get", "/readyz"},
		{"post", "/api/agents"},
		{"delete", "/api/agents/{id}"},
		{"post", "/api/evals/scripts"},
		{"post", "/api/runs"},
		{"get", "/api/runs/{id}"},
		{"post", "/api/runs/{id}/cancel"},
		{"get", "/api/runs/{id}/trace"},
	}
	for _, u := range used {
		p, ok := paths[u.path].(map[string]any)
		if !ok {
			t.Errorf("openapi.yaml has no path %s", u.path)
			continue
		}
		if _, ok := p[u.method]; !ok {
			t.Errorf("openapi.yaml has no %s %s", strings.ToUpper(u.method), u.path)
		}
	}
}

// The trace the api documents decodes into store.Trace with every collection the
// evaluators read.
func TestTraceSchemaFieldsAreDecodedByStoreTrace(t *testing.T) {
	doc := loadSpec(t)
	raw, _ := json.Marshal(store.Trace{})
	var keys map[string]any
	_ = json.Unmarshal(raw, &keys)
	trace := dig(t, doc, "components", "schemas", "Trace", "properties")
	for k := range trace {
		if _, ok := keys[k]; !ok {
			t.Errorf("Trace schema property %q is not a field of store.Trace", k)
		}
	}
}
