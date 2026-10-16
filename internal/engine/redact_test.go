package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// Run tokens never reach a stored transcript, and so never the trace (audit #22): the
// task message that carries one, a reply that repeats it and the finish call's input.
func TestTranscriptRedactsRunToken(t *testing.T) {
	env := newEnv(t)
	a := env.agent(nil)
	var token string
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		token = tokenIn(p.Message)
		go func() {
			now := time.Now().UnixMilli()
			env.fc.push("session.message", map[string]any{"sessionKey": p.SessionKey, "messageId": "task-" + p.IdempotencyKey,
				"message": map[string]any{"role": "user", "content": []any{textPart(p.Message)}, "timestamp": now,
					"__openclaw": map[string]any{"runId": p.IdempotencyKey, "id": "task-" + p.IdempotencyKey}}})
			env.fc.push("session.message", assistantMsg(p.SessionKey, p.IdempotencyKey,
				[]any{textPart("Working with " + strings.ToUpper(token) + " now.")}, "stop", 10, 5, 0))
			finishThenFinal(t, env, p, `{"summary":"hi"}`)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "Say hi."})
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if token == "" {
		t.Fatal("the task message carried no run token")
	}
	trace, err := store.GetTrace(context.Background(), env.pool, run.ID)
	if err != nil || len(trace.Nodes) != 1 {
		t.Fatalf("trace: %+v %v", trace, err)
	}
	msgs := string(trace.Nodes[0].Messages)
	if strings.Contains(strings.ToLower(msgs), token) {
		t.Fatalf("the run token is in the stored transcript:\n%s", msgs)
	}
	if !strings.Contains(msgs, "run_token: [redacted]") || !strings.Contains(msgs, "Working with [redacted] now.") {
		t.Fatalf("transcript without the redacted task and reply:\n%s", msgs)
	}
}
