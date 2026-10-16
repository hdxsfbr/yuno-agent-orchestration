package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"yuno/internal/memory"
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
