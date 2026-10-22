package openclaw

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"testing"
)

// D-091: an Anthropic id goes through the api's proxy provider; an openai/<id> ref is
// OpenClaw's own provider and is written as given.
func TestModelRef(t *testing.T) {
	for model, want := range map[string]string{
		"claude-opus-5-5":      "yuno-anthropic/claude-opus-5-5",
		"openai/gpt-6-astra":   "openai/gpt-6-astra",
		"openai/gpt-5.6-terra": "openai/gpt-5.6-terra",
	} {
		if got := ModelRef(model); got != want {
			t.Errorf("ModelRef(%q) = %q, want %q", model, got, want)
		}
	}
	if IsOpenAIModel("claude-opus-5-5") || IsOpenAIModel("openaix/gpt") || !IsOpenAIModel("openai/gpt-5.6-luna") {
		t.Fatal("IsOpenAIModel")
	}
	if got := AgentModels(); !slices.Equal(got, append(slices.Clone(ManagedModels), OpenAIModels...)) {
		t.Fatalf("AgentModels = %v", got)
	}
}

// An OpenAI agent's entry names the ref as its primary, pins the embedded runtime for it
// and turns Code Mode off for it (D-095); thinkingDefault, reasoningDefault off and
// params.maxTokens are written as for a Claude agent. A Claude agent's entry carries no
// models key.
func TestDesiredEntryOpenAI(t *testing.T) {
	a := testAgent()
	a.MaxTokens = 4096
	claude := desiredEntry(a, nil)
	if _, ok := claude["models"]; ok {
		t.Fatalf("a Claude agent has per-model settings: %v", claude["models"])
	}
	if got := slices.Sorted(maps.Keys(claude)); !slices.Equal(got, []string{"identity", "model", "name", "params", "reasoningDefault", "thinkingDefault", "tools"}) {
		t.Fatalf("Claude entry keys = %v", got)
	}

	a.Model = "openai/gpt-6-astra"
	e := desiredEntry(a, nil)
	if p := e["model"].(map[string]any)["primary"]; p != "openai/gpt-6-astra" {
		t.Fatalf("primary = %v", p)
	}
	models, _ := e["models"].(map[string]any)
	pin, _ := models["openai/gpt-6-astra"].(map[string]any)
	if len(models) != 1 || pin["agentRuntime"].(map[string]any)["id"] != "openclaw" || pin["codeMode"] != false {
		t.Fatalf("models = %v", e["models"])
	}
	if e["thinkingDefault"] != "low" || e["reasoningDefault"] != "off" || e["params"].(map[string]any)["maxTokens"] != 4096 {
		t.Fatalf("entry = %v", e)
	}
	for k, v := range claude {
		x, _ := json.Marshal(v)
		y, _ := json.Marshal(e[k])
		if k != "model" && string(x) != string(y) {
			t.Fatalf("%s differs between the providers: %s vs %s", k, x, y)
		}
	}
}

// Switching an agent between providers and models changes the hash each time (so the
// gateway is updated), moves the runtime pin to the new ref, and a switch back to Claude
// removes the per-model settings; the live entry is in sync after every step.
func TestMaterializeSwitchesProvider(t *testing.T) {
	m := newMatEnv(t)
	ctx := context.Background()
	a := testAgent()
	hashes := map[string]bool{}
	var id string
	for _, step := range []struct {
		model string
		pins  []string
	}{
		{"claude-opus-5-5", nil},
		{"openai/gpt-6-astra", []string{"openai/gpt-6-astra"}},
		{"openai/gpt-5.6-luna", []string{"openai/gpt-5.6-luna"}},
		{"claude-opus-5-5", nil},
	} {
		a.Model = step.model
		want := DesiredHash(m.dir, a, nil)
		got, err := Materialize(ctx, m.gw, m.dir, a, nil)
		if err != nil {
			t.Fatalf("%s: %v (refused %v)", step.model, err, m.state.rejected)
		}
		if got != want {
			t.Fatalf("%s: Materialize hash %s, DesiredHash %s", step.model, got, want)
		}
		hashes[got] = true
		a.MaterializedHash = got
		id = *a.OpenClawAgentID
		e := m.entry(t, id)
		if p := e["model"].(map[string]any)["primary"]; p != ModelRef(step.model) {
			t.Fatalf("%s: primary = %v", step.model, p)
		}
		models, has := e["models"].(map[string]any)
		if step.pins == nil && has {
			t.Fatalf("%s: models left behind: %v", step.model, models)
		}
		if got := slices.Sorted(maps.Keys(models)); step.pins != nil && !slices.Equal(got, step.pins) {
			t.Fatalf("%s: pinned %v, want %v", step.model, got, step.pins)
		}
		v, err := Runtime(ctx, m.gw, m.dir, a)
		if err != nil || !v.InSync {
			t.Fatalf("%s: runtime %+v, %v", step.model, v, err)
		}
	}
	// claude, astra, luna: three distinct hashes; back on claude the first one returns.
	if len(hashes) != 3 {
		t.Fatalf("distinct hashes = %d, want 3", len(hashes))
	}
}
