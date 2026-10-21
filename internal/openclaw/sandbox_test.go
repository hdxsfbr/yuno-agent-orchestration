package openclaw

import (
	"context"
	"encoding/json"
	"fmt"
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
	// The project visit's block keeps the agent's workspace out of the container, as the
	// defaults do (DESIGN 5.30 rule 1).
	if sandbox["scope"] != "session" || sandbox["workspaceAccess"] != "none" || docker["image"] != "yuno-sandbox-go:latest" || docker["network"] != "bridge" {
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

// The P3 acceptance for configuration (DESIGN 5.30 rule 1): nothing the gateway runs
// mounts the agent's own workspace. The defaults and every block the platform sets on an
// agent, whatever the image and the network, say scope "session" and workspaceAccess
// "none" and add no bind mounts of their own; an agent that was only materialised has no
// block and so gets the defaults.
func TestNoSandboxBlockMountsTheAgentsWorkspace(t *testing.T) {
	m := newMatEnv(t)
	ctx := context.Background()
	// problem says what a sandbox block gets wrong; "" means nothing.
	problem := func(sb map[string]any) string {
		if sb["scope"] != "session" || sb["workspaceAccess"] != "none" {
			return fmt.Sprintf("scope/workspaceAccess = %v/%v, want session/none", sb["scope"], sb["workspaceAccess"])
		}
		if docker, _ := sb["docker"].(map[string]any); docker["binds"] != nil {
			return fmt.Sprintf("docker.binds = %v", docker["binds"])
		}
		return ""
	}
	defaults, _ := Baseline(inputs())["agents.defaults.sandbox"].(map[string]any)
	if p := problem(defaults); p != "" {
		t.Fatalf("defaults: %s", p)
	}

	a := testAgent()
	if _, err := Materialize(ctx, m.gw, m.dir, a, nil); err != nil {
		t.Fatal(err)
	}
	id := *a.OpenClawAgentID
	entry := m.entry(t, id)
	if sb, has := entry["sandbox"]; has {
		t.Fatalf("a materialised agent has a sandbox block of its own: %v", sb)
	}
	if entry["workspace"] != WorkspaceDir(m.dir, id) {
		t.Fatalf("workspace = %v, want the agent's own, which no sandbox mounts", entry["workspace"])
	}

	for _, image := range []string{"yuno-sandbox:latest", "yuno-sandbox-go:latest", "yuno-sandbox-node:latest", "yuno-sandbox-python:latest"} {
		for _, network := range []bool{false, true} {
			if err := SetAgentSandbox(ctx, m.gw, id, SandboxSpec{Image: image, Network: network}); err != nil {
				t.Fatal(err)
			}
			sb, _ := m.entry(t, id)["sandbox"].(map[string]any)
			if p := problem(sb); p != "" {
				t.Errorf("%s, network %v: %s", image, network, p)
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
