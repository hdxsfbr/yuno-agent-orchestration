//go:build integration

package openclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"yuno/internal/store"
)

// countingClient counts the write methods that reach the gateway.
type countingClient struct {
	Client
	mu     sync.Mutex
	writes []string
}

func (c *countingClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	switch method {
	case "agents.create", "config.patch", "exec.approvals.set":
		c.mu.Lock()
		c.writes = append(c.writes, method)
		c.mu.Unlock()
	}
	return c.Client.Call(ctx, method, params)
}

// TestIntegrationGateway runs against the gateway of the running stack (compose `go`
// service, loopback): scopes, materialise create and update, read back, clean up.
func TestIntegrationGateway(t *testing.T) {
	token := os.Getenv("OPENCLAW_GATEWAY_TOKEN")
	if token == "" {
		t.Skip("OPENCLAW_GATEWAY_TOKEN not set")
	}
	url := os.Getenv("OPENCLAW_GATEWAY_URL")
	if url == "" {
		url = "ws://127.0.0.1:18789"
	}
	stateDir := os.Getenv("OPENCLAW_STATE_DIR")
	if stateDir == "" {
		stateDir = "/home/node/.openclaw"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	raw, err := Dial(ctx, DialConfig{URL: url, Token: token, Version: "integration-test"}, slog.Default())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	info, up := raw.Info()
	if !up {
		t.Fatal("not connected")
	}
	for _, s := range RequiredScopes {
		if !slices.Contains(info.Scopes, s) {
			t.Fatalf("scope %s not granted: %v", s, info.Scopes)
		}
	}
	t.Logf("gateway %s, %d methods", info.Version, info.Methods)
	c := &countingClient{Client: raw}

	a := &store.Agent{
		ID:               fmt.Sprintf("00000000-0000-7000-8000-%012x", time.Now().UnixNano()&0xffffffffffff),
		Name:             "Yuno integration probe",
		Role:             "Temporary agent created by the integration test.",
		SystemPrompt:     "Do nothing.",
		Model:            "claude-haiku-5-5",
		Thinking:         "low",
		MaxTokens:        2048, // materialised as params.maxTokens; the gateway must accept it
		ToolAllowlist:    []string{"read", "exec", "finish"},
		InteractionRules: map[string]string{"http_fetch": "blocked"},
		ExecPolicy:       store.ExecPolicy{Allowlist: []string{"/usr/bin/python3"}},
	}
	id := DeriveAgentID(a.ID)
	cleanup := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_ = SessionsDelete(cctx, raw, "agent:"+id+":yuno-itest")
		if err := AgentsDelete(cctx, raw, id); err != nil {
			t.Logf("cleanup agents.delete: %v", err)
		}
		if got, err := ExecApprovalsGet(cctx, raw); err == nil {
			var file map[string]any
			if json.Unmarshal(got.File, &file) == nil {
				if agents, ok := file["agents"].(map[string]any); ok {
					delete(agents, id)
					b, _ := json.Marshal(file)
					if err := ExecApprovalsSet(cctx, raw, b, got.Hash); err != nil {
						t.Logf("cleanup exec approvals: %v", err)
					}
				}
			}
		}
		_ = os.RemoveAll(WorkspaceDir(stateDir, id))
		_ = os.Remove(filepath.Join(stateDir, "agents", id))
	}
	cleanup()
	defer cleanup()

	skills := []store.Skill{{Name: "itest-skill", Description: "Integration: temporary", Body: "Nothing to see."}}
	hash, err := Materialize(ctx, c, stateDir, a, skills)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if got := *a.OpenClawAgentID; got != id {
		t.Fatalf("openclaw id = %s, want %s", got, id)
	}
	list, err := AgentsList(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ag := range list.Agents {
		if ag.ID == id {
			found = true
			if ag.Model.Primary != "yuno-anthropic/claude-haiku-5-5" {
				t.Errorf("agents.list model = %s", ag.Model.Primary)
			}
		}
	}
	if !found {
		t.Fatalf("%s not in agents.list", id)
	}
	cfg, err := ConfigGet(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	entry := ConfigEntries(cfg.Config)[id]
	if len(entry) == 0 {
		t.Fatalf("config.get has no entry for %s", id)
	}
	if !entryMatches(entry, desiredEntry(a, []string{"itest-skill"})) {
		t.Fatalf("live entry differs: %s", entry)
	}
	t.Logf("live entry: %s", entry)
	for _, f := range []string{"SOUL.md", "AGENTS.md", "MEMORY.md", "skills/itest-skill/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(WorkspaceDir(stateDir, id), f)); err != nil {
			t.Errorf("workspace file %s: %v", f, err)
		}
	}

	// Re-materialise unchanged: no writes.
	a.MaterializedHash = hash
	before := len(c.writes)
	if _, err := Materialize(ctx, c, stateDir, a, skills); err != nil {
		t.Fatal(err)
	}
	if len(c.writes) != before {
		t.Fatalf("writes on unchanged agent: %v", c.writes[before:])
	}

	// Update against the real replacePaths rules: shrink the allowlist, drop the skill.
	a.ToolAllowlist = []string{"exec", "finish"}
	if _, err := Materialize(ctx, c, stateDir, a, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	v, err := Runtime(ctx, raw, stateDir, a)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Materialized || !v.InSync {
		t.Fatalf("runtime after update: %+v", v)
	}
	var live map[string]any
	_ = json.Unmarshal(v.ConfigEntry, &live)
	if _, ok := live["skills"]; ok {
		t.Fatalf("skills still set: %v", live["skills"])
	}
	t.Logf("exec policy: %s", v.ExecPolicy)

	// D-091 against the real merge rules: an OpenAI model with its runtime pin, another one
	// (the old ref's pin nulled), then back to Claude (the per-model settings deleted). No
	// sign-in is needed: nothing here runs a turn.
	for _, model := range []string{"openai/gpt-6-astra", "openai/gpt-5.6-luna", "claude-haiku-5-5"} {
		a.Model = model
		if a.MaterializedHash, err = Materialize(ctx, c, stateDir, a, nil); err != nil {
			t.Fatalf("switch to %s: %v", model, err)
		}
		if v, err = Runtime(ctx, raw, stateDir, a); err != nil || !v.InSync {
			t.Fatalf("runtime on %s: %+v %v", model, v, err)
		}
		var e map[string]any
		_ = json.Unmarshal(v.ConfigEntry, &e)
		models, _ := e["models"].(map[string]any)
		if IsOpenAIModel(model) != (len(models) == 1 && models[model] != nil) {
			t.Fatalf("models on %s: %v", model, e["models"])
		}
	}

	// A session created by sessions.patch exposes the effective tool list; log it so the
	// MCP tool name prefix can be read off once the api serves /mcp.
	key := "agent:" + id + ":yuno-itest"
	if err := SessionsPatchModel(ctx, raw, key, "scripted/scripted-1"); err != nil {
		t.Fatalf("sessions.patch: %v", err)
	}
	if eff, err := raw.Call(ctx, "tools.effective", map[string]any{"sessionKey": key, "agentId": id}); err == nil {
		t.Logf("tools.effective: %s", eff)
	} else {
		t.Logf("tools.effective: %v", err)
	}
}
