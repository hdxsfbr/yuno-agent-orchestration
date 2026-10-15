package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"yuno/internal/store"
)

// The trace carries the run's side effects (DESIGN 5.4 outputs) and, T5.7.8 on the
// API side, never serves reasoning parts even if a row holds them.
func TestTraceSideEffectsAndNoReasoning(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	ctx := context.Background()
	run, err := createTestRun(ctx, pool, store.TriggerManual, nil, store.Limits{})
	mustDo(t, err)
	now := time.Now().UTC()
	node := &store.RunNode{RunID: run.ID, NodeID: "coder", Status: store.NodeSucceeded, StartedAt: &now,
		Messages: json.RawMessage(`[
			{"role":"user","content":"task"},
			{"role":"assistant","content":[{"type":"thinking","thinking":"secret plan"},{"type":"text","text":"done"},
				{"type":"redacted_thinking","data":"x"},{"type":"toolCall","id":"c1","name":"exec","arguments":{}}]},
			{"role":"reasoning","content":"more secret plan"}]`)}
	mustDo(t, store.CreateRunNode(ctx, pool, node))
	key := strings.ReplaceAll(store.NewID()+store.NewID(), "-", "")
	_, _, err = store.RunSideEffect(ctx, pool, key, "deploy_release", run.ID, "tc-1", func(context.Context) (json.RawMessage, error) {
		return json.RawMessage(`{"release_id":"rel-1"}`), nil
	})
	mustDo(t, err)

	code, body := do(t, ts, http.MethodGet, "/api/runs/"+run.ID+"/trace", "")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	if strings.Contains(string(body), "secret plan") || strings.Contains(string(body), "redacted_thinking") {
		t.Fatalf("reasoning served: %s", body)
	}
	var tr store.Trace
	mustDo(t, json.Unmarshal(body, &tr))
	if !strings.Contains(string(tr.Nodes[0].Messages), `"done"`) || !strings.Contains(string(tr.Nodes[0].Messages), "toolCall") {
		t.Fatalf("visible parts lost: %s", tr.Nodes[0].Messages)
	}
	if len(tr.SideEffects) != 1 || tr.SideEffects[0].IdempotencyKey != key || !strings.Contains(string(tr.SideEffects[0].Result), "rel-1") {
		t.Fatalf("side_effects = %+v", tr.SideEffects)
	}
}

func TestStripReasoningLeavesOtherShapes(t *testing.T) {
	for _, in := range []string{`[]`, `[{"role":"user","content":"hi"}]`, `{"not":"a list"}`, `null`} {
		if got := string(stripReasoning(json.RawMessage(in))); got != in {
			t.Errorf("stripReasoning(%s) = %s", in, got)
		}
	}
}
