package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"yuno/api"
	"yuno/internal/store"
)

// listedAgent finds an agent in GET /api/agents (with the query given).
func listedAgent(t *testing.T, body []byte, id string) (agentListItem, bool) {
	t.Helper()
	var list []agentListItem
	mustDo(t, json.Unmarshal(body, &list))
	i := slices.IndexFunc(list, func(a agentListItem) bool { return a.ID == id })
	if i < 0 {
		return agentListItem{}, false
	}
	return list[i], true
}

// DESIGN W4 rule 4: an archived agent is listed with include_archived=true and archived_at;
// a restore makes it live and materialises it; a live agent is not_archived; a live agent
// with the same name (ignoring case) makes it name_taken; a failed materialisation leaves
// it restored and answers 502. Restoring grants the agent's tools again, so the contract
// marks the operation privileged.
func TestAgentRestore(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	fe := &fakeEngine{pool: pool}
	_, ts := newTestServer(t, pool, fe, nil)
	sfx := uniqueSuffix()
	name := "Restorable " + sfx

	code, body := do(t, ts, http.MethodPost, "/api/agents", `{"name":"`+name+`","model":"claude-opus-5-5","role":"keeps","tool_allowlist":["read","finish"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var a store.Agent
	mustDo(t, json.Unmarshal(body, &a))
	if code, _ = do(t, ts, http.MethodDelete, "/api/agents/"+a.ID, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}

	// Archived: out of the default list, in the list with include_archived, with archived_at.
	_, body = do(t, ts, http.MethodGet, "/api/agents", "")
	if _, ok := listedAgent(t, body, a.ID); ok {
		t.Fatal("an archived agent is in the default list")
	}
	code, body = do(t, ts, http.MethodGet, "/api/agents?include_archived=true", "")
	item, ok := listedAgent(t, body, a.ID)
	if code != http.StatusOK || !ok || item.ArchivedAt == nil || !strings.Contains(string(body), `"archived_at"`) {
		t.Fatalf("include_archived: %d %v %+v", code, ok, item.ArchivedAt)
	}
	if code, body = do(t, ts, http.MethodGet, "/api/agents?include_archived=maybe", ""); code != http.StatusUnprocessableEntity ||
		!slices.Contains(detailPaths(t, body), "include_archived") {
		t.Fatalf("include_archived=maybe: %d %s", code, body)
	}

	// Restore: live again, materialised, with its configuration.
	fe.materialized = nil
	code, body = do(t, ts, http.MethodPost, "/api/agents/"+a.ID+"/restore", "")
	var restored store.Agent
	if code != http.StatusOK || json.Unmarshal(body, &restored) != nil || restored.ID != a.ID || restored.DeletedAt != nil ||
		restored.Role != "keeps" || !slices.Equal(restored.ToolAllowlist, []string{"read", "finish"}) {
		t.Fatalf("restore: %d %s", code, body)
	}
	if !slices.Equal(fe.materialized, []string{a.ID}) || restored.MaterializedHash != "hash-1" {
		t.Fatalf("not materialised: %v %q", fe.materialized, restored.MaterializedHash)
	}
	if code, _ = do(t, ts, http.MethodGet, "/api/agents/"+a.ID, ""); code != http.StatusOK {
		t.Fatalf("read back: %d", code)
	}
	_, body = do(t, ts, http.MethodGet, "/api/agents?include_archived=true", "")
	if item, ok := listedAgent(t, body, a.ID); !ok || item.ArchivedAt != nil {
		t.Fatalf("restored agent listed as archived: %+v", item.ArchivedAt)
	}
	if code, body = do(t, ts, http.MethodPost, "/api/agents/"+a.ID+"/restore", ""); code != http.StatusConflict || decodeError(t, body).Error.Code != "not_archived" {
		t.Fatalf("restore of a live agent: %d %s", code, body)
	}

	// Name taken: archived again, and a live agent took the name meanwhile.
	if code, _ = do(t, ts, http.MethodDelete, "/api/agents/"+a.ID, ""); code != http.StatusNoContent {
		t.Fatalf("delete again: %d", code)
	}
	taker := testAgent(t, "  "+strings.ToUpper(name)+" ")
	code, body = do(t, ts, http.MethodPost, "/api/agents/"+a.ID+"/restore", "")
	e := decodeError(t, body)
	var holder struct {
		AgentID string `json:"agent_id"`
	}
	if code != http.StatusConflict || e.Error.Code != "name_taken" || json.Unmarshal(e.Error.Details, &holder) != nil || holder.AgentID != taker {
		t.Fatalf("name taken: %d %s", code, body)
	}
	if code, _ = do(t, ts, http.MethodGet, "/api/agents/"+a.ID, ""); code != http.StatusNotFound {
		t.Fatalf("a refused restore made the agent live: %d", code)
	}

	// The live one is archived: the restore goes through, and a gateway that refuses the
	// push leaves the agent restored with a 502.
	mustDo(t, store.DeleteAgent(ctx, pool, taker))
	fe.materializeErr = errors.New("config.patch: base hash mismatch")
	code, body = do(t, ts, http.MethodPost, "/api/agents/"+a.ID+"/restore", "")
	if code != http.StatusBadGateway || decodeError(t, body).Error.Code != "materialize_failed" {
		t.Fatalf("restore with the gateway refusing: %d %s", code, body)
	}
	if code, _ = do(t, ts, http.MethodGet, "/api/agents/"+a.ID, ""); code != http.StatusOK {
		t.Fatalf("restored agent after a failed push: %d", code)
	}
	if code, _ = do(t, ts, http.MethodPost, "/api/agents/no-such-agent/restore", ""); code != http.StatusNotFound {
		t.Fatalf("unknown agent: %d", code)
	}

	// Restoring is routine in the contract; the gate makes it privileged when the agent has
	// tools (DESIGN 5.35 rule 4, TestPrivilegedNeedsApproval).
	doc, err := openapi3.NewLoader().LoadFromData(api.OpenAPI)
	mustDo(t, err)
	op := doc.Paths.Find("/api/agents/{id}/restore")
	if op == nil || op.Post == nil || op.Post.OperationID != "restoreAgent" || op.Post.Extensions["x-yuno-risk"] != "routine" {
		t.Fatalf("restore operation = %+v", op)
	}
}
