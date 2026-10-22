package engine

import (
	"testing"

	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// D-096: a tool result with a NUL character (an agent read a binary file) is stored
// without it, and the node's row keeps being written: its transcript, then its final
// status. On run 01a16451 one such entry left the doc audit "running" after the run
// succeeded.
func TestTranscriptNULStored(t *testing.T) {
	env := newEnv(t)
	a := env.agent(nil)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			env.fc.push("session.message", assistantMsg(p.SessionKey, p.IdempotencyKey,
				[]any{toolCallPart("call_cat", "exec", map[string]any{"command": "cat model.bin"})}, "toolUse", 100, 10, 0))
			env.fc.push("session.message", toolResultMsg(p, "call_cat", "exec", "PK\x03\x04\x00\x00header"))
			finishThenFinal(t, env, p, `{"summary":"read it"}`)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x"})
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if n := env.count(`SELECT count(*) FROM run_nodes WHERE run_id=$1 AND status='succeeded' AND messages::text LIKE '%PK%header%'`, run.ID); n != 1 {
		t.Fatalf("node rows succeeded with the transcript = %d", n)
	}
	if n := env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='node.status' AND data->'node'->>'status'='succeeded'`, run.ID); n != 1 {
		t.Fatalf("node.status succeeded events = %d", n)
	}
}
