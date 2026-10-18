package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"yuno/internal/config"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// DESIGN 5.24 rules 2 and 9: the platform tools reach the engine through the Asker, and
// ask_operator blocks for MCP_TOOL_WAIT. The Asker is the server's adapter, which reads
// s.engine at call time: Run builds the registry before the engine exists and sets
// s.engine afterwards, so an Asker taken from the Deps at build time would be nil in
// production (the first wave 10 deploy answered "not configured" to every question).
func TestPlatformDepsCarryTheAskerAndTheWait(t *testing.T) {
	cfg := config.Config{MCPToolWait: 7 * time.Second}
	s := &Server{}
	pd := s.platformDeps(Deps{Cfg: cfg})
	if pd.Asker == nil {
		t.Fatal("Asker = nil, want the server's adapter")
	}
	if pd.MCPToolWait != 7*time.Second {
		t.Errorf("MCPToolWait = %v, want 7s", pd.MCPToolWait)
	}
	ctx := context.Background()
	if _, err := pd.Asker.AnswerQuestion(ctx, "q", "yes", "api"); !errors.Is(err, errEngineUnavailable) {
		t.Errorf("AnswerQuestion before the engine exists: %v, want engine unavailable", err)
	}
	// The engine arrives later, as in Run: the same Asker now reaches it.
	reached := errors.New("reached the engine")
	s.engine = &fakeEngine{askOperator: func(_ context.Context, token, question string, options []string) (*store.Approval, error) {
		if token != "r_tok" || question != "Which one?" || len(options) != 2 {
			t.Errorf("AskOperator got %q %q %v", token, question, options)
		}
		return nil, reached
	}}
	if _, err := pd.Asker.AskOperator(ctx, "r_tok", "Which one?", []string{"a", "b"}); !errors.Is(err, reached) {
		t.Errorf("AskOperator after the engine exists: %v, want it reached", err)
	}
	// The fake engine parks nothing: QuestionPending is a no-op rather than a failure.
	if err := pd.Asker.(tools.QuestionParker).QuestionPending(ctx, "r_tok", "q"); err != nil {
		t.Errorf("QuestionPending on an engine that does not park: %v", err)
	}
}

// The production order end to end: New builds the registry with no engine, the engine is
// set afterwards, and ask_operator taken from that registry reaches it.
func TestAskOperatorReachesTheEngineSetAfterTheRegistryWasBuilt(t *testing.T) {
	s, err := New(Deps{Cfg: testConfig(), Pool: testPool})
	if err != nil {
		t.Fatal(err)
	}
	reached := errors.New("reached the engine")
	s.engine = &fakeEngine{pool: testPool, askOperator: func(context.Context, string, string, []string) (*store.Approval, error) {
		return nil, reached
	}}
	tool, ok := s.tools.Get(tools.AskOperatorName)
	if !ok {
		t.Fatal("ask_operator is not registered")
	}
	_, err = tool.Exec(context.Background(), tools.Call{RunID: "run", Input: json.RawMessage(`{"run_token":"r_tok","question":"Which one?"}`)})
	if err == nil || !errors.Is(err, reached) || strings.Contains(err.Error(), "not configured") {
		t.Fatalf("ask_operator = %v, want the engine reached", err)
	}
}
