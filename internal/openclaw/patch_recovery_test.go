package openclaw

import (
	"context"
	"testing"
	"time"
)

func TestConfigPatchRecoveryRestart(t *testing.T) {
	recoveryBackoff = time.Millisecond
	t.Cleanup(func() { recoveryBackoff = 250 * time.Millisecond })
	m := newMatEnv(t)
	ctx := context.Background()
	m.state.mu.Lock()
	m.state.recoveryPatches = 1
	m.state.mu.Unlock()

	cfg, err := ConfigGet(ctx, m.gw)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"agents":{"entries":{"main":{"name":"Renamed"}}}}`
	if _, err := ConfigPatch(ctx, m.gw, raw, cfg.Hash, nil); err != nil {
		t.Fatalf("patch should be treated as persisted: %v", err)
	}
	if n := len(m.f.callsFor("gateway.restart.request")); n != 1 {
		t.Fatalf("restart requests = %d, want 1", n)
	}
	if got := m.entry(t, "main")["name"]; got != "Renamed" {
		t.Fatalf("name = %v", got)
	}

	// A later patch needs no restart once the gateway is back.
	cfg, _ = ConfigGet(ctx, m.gw)
	if _, err := ConfigPatch(ctx, m.gw, `{"agents":{"entries":{"main":{"name":"Again"}}}}`, cfg.Hash, nil); err != nil {
		t.Fatal(err)
	}
	if n := len(m.f.callsFor("gateway.restart.request")); n != 1 {
		t.Fatalf("restart requests = %d, want still 1", n)
	}
}

func TestContainsPatch(t *testing.T) {
	if !containsPatch(map[string]any{"a": map[string]any{"b": 1.0}}, map[string]any{"a": map[string]any{"b": 1.0}}) ||
		containsPatch(map[string]any{"a": map[string]any{}}, map[string]any{"a": map[string]any{"b": 1.0}}) ||
		containsPatch(map[string]any{"a": 1.0}, map[string]any{"a": nil}) {
		t.Fatal("containsPatch wrong")
	}
}
