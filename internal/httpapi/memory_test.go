package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yuno/internal/memory"
	"yuno/internal/openclaw"
	"yuno/internal/store"
)

func saveFact(t *testing.T, agentID, key string, savedAt, expires time.Time) *store.MemoryFact {
	t.Helper()
	f := &store.MemoryFact{AgentID: agentID, Key: key, Value: "value of " + key, ExpiresAt: &expires}
	mustDo(t, memory.Save(context.Background(), testPool, f, 0, savedAt))
	return f
}

// DESIGN 5.10 rule 7: the agent's live facts newest first, expired ones hidden before
// the sweep, and delete per fact.
func TestAgentMemoryAPI(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	agentID := testAgent(t, "memory-api")
	otherID := testAgent(t, "memory-other")
	now := time.Now().UTC()
	saveFact(t, agentID, "older", now.Add(-2*time.Hour), now.Add(24*time.Hour))
	newer := saveFact(t, agentID, "newer", now.Add(-time.Hour), now.Add(24*time.Hour))
	saveFact(t, agentID, "expired", now.Add(-3*time.Hour), now.Add(-time.Minute))
	foreign := saveFact(t, otherID, "foreign", now, now.Add(time.Hour))

	code, body := do(t, ts, http.MethodGet, "/api/agents/"+agentID+"/memory", "")
	var facts []store.MemoryFact
	if code != http.StatusOK || json.Unmarshal(body, &facts) != nil {
		t.Fatalf("list: %d %s", code, body)
	}
	if len(facts) != 2 || facts[0].Key != "newer" || facts[1].Key != "older" || facts[0].ExpiresAt == nil {
		t.Fatalf("facts = %s", body)
	}

	if code, _ := do(t, ts, http.MethodDelete, "/api/agents/"+agentID+"/memory/"+foreign.ID, ""); code != http.StatusNotFound {
		t.Fatalf("delete another agent's fact: %d", code)
	}
	if code, _ := do(t, ts, http.MethodDelete, "/api/agents/"+agentID+"/memory/"+newer.ID, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	_, body = do(t, ts, http.MethodGet, "/api/agents/"+agentID+"/memory", "")
	mustDo(t, json.Unmarshal(body, &facts))
	if len(facts) != 1 || facts[0].Key != "older" {
		t.Fatalf("after delete: %s", body)
	}
	if code, _ := do(t, ts, http.MethodDelete, "/api/agents/"+agentID+"/memory/"+newer.ID, ""); code != http.StatusNotFound {
		t.Fatalf("delete twice: %d", code)
	}

	// An agent with no facts lists an empty array; an unknown agent is a 404.
	emptyID := testAgent(t, "memory-empty")
	if code, body := do(t, ts, http.MethodGet, "/api/agents/"+emptyID+"/memory", ""); code != http.StatusOK || string(body) != "[]\n" {
		t.Fatalf("empty: %d %q", code, body)
	}
	if code, _ := do(t, ts, http.MethodGet, "/api/agents/no-such-agent/memory", ""); code != http.StatusNotFound {
		t.Fatalf("unknown agent list: %d", code)
	}
	if code, _ := do(t, ts, http.MethodDelete, "/api/agents/no-such-agent/memory/x", ""); code != http.StatusNotFound {
		t.Fatalf("unknown agent delete: %d", code)
	}
}

// readMemoryMD returns an agent's MEMORY.md under the state directory, "" when absent.
func readMemoryMD(t *testing.T, stateDir, openclawID string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(openclaw.WorkspaceDir(stateDir, openclawID), "MEMORY.md"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

// MEMORY.md is fresh outside node visits too, for conversations (audit #8): rewritten
// after every memory_save and after a fact is deleted.
func TestMemoryFileFollowsFacts(t *testing.T) {
	g := newGateFixture(t, []string{"memory_save"}, nil, 5*time.Second)
	g.setGuardrails(t, store.Guardrails{}, true)
	stateDir, ocID := g.s.cfg.OpenClawStateDir, *g.agent.OpenClawAgentID
	if text, isErr := g.call(t, "memory_save", map[string]any{"run_token": g.token, "key": "release day", "value": "Tuesday"}); isErr {
		t.Fatalf("memory_save: %s", text)
	}
	if got := readMemoryMD(t, stateDir, ocID); !strings.Contains(got, "- release day: Tuesday\n") {
		t.Fatalf("MEMORY.md after memory_save = %q", got)
	}
	if text, isErr := g.call(t, "memory_save", map[string]any{"run_token": g.token, "key": "release day", "value": "Thursday"}); isErr {
		t.Fatalf("memory_save: %s", text)
	}
	got := readMemoryMD(t, stateDir, ocID)
	if !strings.Contains(got, "- release day: Thursday\n") || strings.Contains(got, "Tuesday") {
		t.Fatalf("MEMORY.md after replacing the fact = %q", got)
	}

	facts, err := memory.List(context.Background(), testPool, g.agent.ID, time.Now())
	if err != nil || len(facts) != 1 {
		t.Fatalf("facts = %v %v", facts, err)
	}
	if code, b := do(t, g.ts, http.MethodDelete, "/api/agents/"+g.agent.ID+"/memory/"+facts[0].ID, ""); code != http.StatusNoContent {
		t.Fatalf("delete fact: %d %s", code, b)
	}
	if got := readMemoryMD(t, stateDir, ocID); got != "# Memory\n" {
		t.Fatalf("MEMORY.md after deleting the only fact = %q", got)
	}
}

// Binding an agent to Telegram renders its MEMORY.md, which its conversations read.
func TestBindRendersMemory(t *testing.T) {
	pool := needDB(t)
	unbindAll := func() { _, _ = pool.Exec(context.Background(), `DELETE FROM channel_bindings`) }
	unbindAll()
	t.Cleanup(unbindAll)
	cfg := testConfig()
	cfg.TelegramMode = "polling"
	cfg.OpenClawStateDir = t.TempDir()
	_, ts := newTestServerDeps(t, Deps{Cfg: cfg, Pool: pool, Gateway: newChannelGateway()}, &fakeEngine{pool: pool})
	a := channelAgent(t, "Chatter")
	now := time.Now().UTC()
	saveFact(t, a.ID, "timezone", now, now.Add(time.Hour))
	bind := func() {
		t.Helper()
		if code, b := do(t, ts, http.MethodPut, "/api/channels/telegram/binding", `{"agent_id":"`+a.ID+`"}`); code != http.StatusOK {
			t.Fatalf("bind: %d %s", code, b)
		}
	}

	// Memory off: the file is the materialiser's, never rendered from facts.
	bind()
	if got := readMemoryMD(t, cfg.OpenClawStateDir, "t-"+a.ID); got != "" {
		t.Fatalf("MEMORY.md rendered for an agent with memory off: %q", got)
	}
	stored, err := store.GetAgent(context.Background(), pool, a.ID)
	mustDo(t, err)
	stored.MemoryPolicy.Enabled = true
	mustDo(t, store.UpdateAgent(context.Background(), pool, stored))
	bind()
	if got := readMemoryMD(t, cfg.OpenClawStateDir, "t-"+a.ID); !strings.Contains(got, "- timezone: value of timezone\n") {
		t.Fatalf("MEMORY.md after binding = %q", got)
	}
}
