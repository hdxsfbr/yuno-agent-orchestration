package openclaw

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

// T5.16.4: the sandbox block a project visit sets and clears (DESIGN 5.16 rule 7).
func TestSetAndClearAgentSandbox(t *testing.T) {
	m := newMatEnv(t)
	ctx := context.Background()
	a := testAgent()
	if _, err := Materialize(ctx, m.gw, m.dir, a, nil); err != nil {
		t.Fatal(err)
	}
	id := *a.OpenClawAgentID
	before := len(m.f.callsFor("config.patch"))

	if err := SetAgentSandbox(ctx, m.gw, id, SandboxSpec{Image: "yuno-sandbox-go:latest", Network: true}); err != nil {
		t.Fatal(err)
	}
	patches := m.f.callsFor("config.patch")
	if len(patches) != before+1 {
		t.Fatalf("%d patches, want %d", len(patches), before+1)
	}
	var p ConfigPatchParams
	if err := json.Unmarshal(patches[before], &p); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(p.Raw), &raw); err != nil {
		t.Fatal(err)
	}
	entry := raw["agents"].(map[string]any)["entries"].(map[string]any)[id].(map[string]any)
	sandbox := entry["sandbox"].(map[string]any)
	docker := sandbox["docker"].(map[string]any)
	if sandbox["scope"] != "session" || docker["image"] != "yuno-sandbox-go:latest" || docker["network"] != "bridge" {
		t.Fatalf("sandbox block = %v", sandbox)
	}
	if p.BaseHash == "" || len(p.ReplacePaths) != 0 {
		t.Fatalf("patch params = %+v", p)
	}
	live := m.entry(t, id)
	if got := live["sandbox"].(map[string]any)["docker"].(map[string]any)["image"]; got != "yuno-sandbox-go:latest" {
		t.Fatalf("live entry sandbox = %v", live["sandbox"])
	}
	if live["name"] != "Quick Coder" {
		t.Fatal("the patch replaced the rest of the entry")
	}

	// Network off is "none".
	if err := SetAgentSandbox(ctx, m.gw, id, SandboxSpec{Image: "yuno-sandbox-python:latest"}); err != nil {
		t.Fatal(err)
	}
	live = m.entry(t, id)
	if got := live["sandbox"].(map[string]any)["docker"].(map[string]any)["network"]; got != "none" {
		t.Fatalf("network = %v", got)
	}

	// Clear: null with the path in replacePaths.
	if err := ClearAgentSandbox(ctx, m.gw, id); err != nil {
		t.Fatal(err)
	}
	patches = m.f.callsFor("config.patch")
	if err := json.Unmarshal(patches[len(patches)-1], &p); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.ReplacePaths, []string{"agents.entries." + id + ".sandbox"}) {
		t.Fatalf("replacePaths = %v", p.ReplacePaths)
	}
	if err := json.Unmarshal([]byte(p.Raw), &raw); err != nil {
		t.Fatal(err)
	}
	if v, ok := raw["agents"].(map[string]any)["entries"].(map[string]any)[id].(map[string]any)["sandbox"]; !ok || v != nil {
		t.Fatalf("clear patch sandbox = %v (present %v)", v, ok)
	}
	if _, has := m.entry(t, id)["sandbox"]; has {
		t.Fatal("the sandbox block survived the clear")
	}
	// The defaults were never touched.
	m.state.mu.Lock()
	defaults, _ := m.state.config["agents"].(map[string]any)["defaults"]
	m.state.mu.Unlock()
	if b, _ := json.Marshal(defaults); string(b) != "null" && string(b) != "{}" && string(b) != `{"sandbox":null}` {
		if d, ok := defaults.(map[string]any); ok {
			if _, touched := d["sandbox"]; touched {
				t.Fatalf("defaults touched: %s", b)
			}
		}
	}
}

// A sandbox patch retries a stale baseHash like the materialiser.
func TestSetAgentSandboxRetriesStaleHash(t *testing.T) {
	m := newMatEnv(t)
	bumped := false
	m.f.on("config.get", func(json.RawMessage) (any, *RPCError) {
		m.state.mu.Lock()
		defer m.state.mu.Unlock()
		cfg, _ := json.Marshal(m.state.config)
		h := m.state.cfgHash()
		if !bumped {
			bumped = true
			h = "stale-" + h
		}
		return map[string]any{"hash": h, "config": json.RawMessage(cfg)}, nil
	})
	if err := SetAgentSandbox(context.Background(), m.gw, "agx", SandboxSpec{Image: "i"}); err != nil {
		t.Fatal(err)
	}
	if n := len(m.f.callsFor("config.patch")); n != 2 {
		t.Fatalf("config.patch calls = %d, want 2 (one refused, one retried)", n)
	}
	if err := SetAgentSandbox(context.Background(), nil, "agx", SandboxSpec{Image: "i"}); err == nil {
		t.Fatal("nil client must fail")
	}
}
