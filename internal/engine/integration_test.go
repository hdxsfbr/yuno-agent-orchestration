//go:build integration

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"yuno/internal/events"
	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// mockModel is an OpenAI-compatible streaming endpoint the gateway can reach over
// loopback (the go service shares the gateway's network namespace).
func mockModel(reply string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(v any) { b, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", b) }
		chunk(map[string]any{"id": "cmpl-itest", "object": "chat.completion.chunk", "model": "m1",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": reply}, "finish_reason": nil}}})
		chunk(map[string]any{"id": "cmpl-itest", "object": "chat.completion.chunk", "model": "m1",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 50, "completion_tokens": 5, "total_tokens": 55}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
}

// TestIntegrationQuickRun drives one quick run through the real gateway: materialise,
// model override, subscribe, chat.send, transcript mirror, final, session delete.
func TestIntegrationQuickRun(t *testing.T) {
	token := os.Getenv("OPENCLAW_GATEWAY_TOKEN")
	if token == "" {
		t.Skip("OPENCLAW_GATEWAY_TOKEN not set")
	}
	pool := needDB(t)
	stateDir := os.Getenv("OPENCLAW_STATE_DIR")
	if stateDir == "" {
		stateDir = "/home/node/.openclaw"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	gw, err := openclaw.Dial(ctx, openclaw.DialConfig{URL: "ws://127.0.0.1:18789", Token: token, Version: "engine-itest"}, slog.Default())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mockModel(`Done. {"summary":"integration ok"}`)}
	go srv.Serve(ln) //nolint:errcheck
	defer srv.Close()
	const provider = "yunoitest"
	providerCfg := map[string]any{"models": map[string]any{"providers": map[string]any{provider: map[string]any{
		"baseUrl": fmt.Sprintf("http://%s/v1", ln.Addr()), "apiKey": "itest", "api": "openai-completions",
		"models": []any{map[string]any{"id": "m1", "name": "Integration mock", "reasoning": false, "input": []any{"text"},
			"cost": map[string]any{"input": 1, "output": 5, "cacheRead": 0, "cacheWrite": 0}, "contextWindow": 100000, "maxTokens": 4096}},
	}}}}
	patchConfig := func(v any, replace []string) error {
		cfg, err := openclaw.ConfigGet(ctx, gw)
		if err != nil {
			return err
		}
		b, _ := json.Marshal(v)
		_, err = openclaw.ConfigPatch(ctx, gw, string(b), cfg.Hash, replace)
		return err
	}
	if err := patchConfig(providerCfg, nil); err != nil {
		t.Fatalf("add mock provider: %v", err)
	}
	defer func() {
		del := map[string]any{"models": map[string]any{"providers": map[string]any{provider: nil}}}
		if err := patchConfig(del, []string{"models.providers." + provider, "models.providers." + provider + ".models"}); err != nil {
			t.Errorf("remove mock provider: %v", err)
		}
	}()

	hub := events.NewHub()
	rec := &events.Recorder{Hub: hub, Append: func(ctx context.Context, runID, typ string, data json.RawMessage) (int64, error) {
		return store.AppendRunEvent(ctx, pool, runID, typ, data)
	}}
	e := New(Deps{Cfg: testConfig(), Log: slog.Default(), Pool: pool, Gateway: gw, Hub: hub, Recorder: rec, StateDir: stateDir})
	// read keeps a callable tool while the MCP server (yuno__finish) is not registered:
	// OpenClaw refuses a turn whose allowlist resolves to no tools.
	a := &store.Agent{Name: "Engine integration probe", Model: "claude-haiku-5-5", ToolAllowlist: []string{"read", "finish"}}
	if err := store.CreateAgent(ctx, pool, a); err != nil {
		t.Fatal(err)
	}
	ocID := openclaw.DeriveAgentID(a.ID)
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		if err := openclaw.AgentsDelete(cctx, gw, ocID); err != nil {
			t.Errorf("agents.delete: %v", err)
		}
		if got, err := openclaw.ExecApprovalsGet(cctx, gw); err == nil {
			var file map[string]any
			if json.Unmarshal(got.File, &file) == nil {
				if agents, ok := file["agents"].(map[string]any); ok {
					delete(agents, ocID)
					b, _ := json.Marshal(file)
					_ = openclaw.ExecApprovalsSet(cctx, gw, b, got.Hash)
				}
			}
		}
		_ = os.RemoveAll(openclaw.WorkspaceDir(stateDir, ocID))
		t.Logf("cleaned up %s; remove its sandbox container (openclaw-sbx-*) from the host if one was created", ocID)
	}()

	run, err := e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "Reply with a JSON summary.", Model: provider + "/m1"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimQueuedRun(ctx, pool)
	if err != nil || claimed.ID != run.ID {
		t.Fatalf("claim: %v", err)
	}
	e.runRun(ctx, claimed)

	tr, err := store.GetTrace(ctx, pool, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Run.Status != store.RunSucceeded {
		t.Fatalf("run = %s %v", tr.Run.Status, tr.Run.FailureReason)
	}
	if len(tr.ModelCalls) != 1 || tr.ModelCalls[0].Provider != provider || tr.ModelCalls[0].Model != "m1" ||
		tr.ModelCalls[0].InputTokens == 0 || tr.ModelCalls[0].CostUSDMicros == 0 {
		t.Fatalf("model calls = %+v", tr.ModelCalls)
	}
	node := tr.Nodes[0]
	if string(node.Output) != `{"summary": "integration ok"}` {
		t.Fatalf("output = %s", node.Output)
	}
	var msgs []map[string]any
	_ = json.Unmarshal(node.Messages, &msgs)
	if len(msgs) < 2 {
		t.Fatalf("messages = %s", node.Messages)
	}
	t.Logf("run %s: %d steps, cost %d µUSD, %d messages, session %s", run.ID, tr.Run.StepCount, tr.Run.CostUSDMicros, len(msgs), *node.SessionKey)
	if h, err := openclaw.ChatHistory(ctx, gw, *node.SessionKey, ocID, 10); err == nil && len(h.Messages) > 0 {
		t.Errorf("session not deleted: %d messages left", len(h.Messages))
	}
}
