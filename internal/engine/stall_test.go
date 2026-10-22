package engine

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// hourLimit gives a quick run the platform's whole hour, room for the clock advances past
// several stall windows (a quick run's own limit is 600 s).
var hourLimit = store.Limits{MaxDurationSeconds: 3600}

// toolResultMsg is the session.message of a turn's toolResult entry, under the entry's own
// id as the gateway sends it.
func toolResultMsg(p openclaw.ChatSendParams, callID, tool, text string) map[string]any {
	entry := toolResultEntry(p.IdempotencyKey, callID, tool, text, false)
	id := entry["__openclaw"].(map[string]any)["id"]
	return map[string]any{"sessionKey": p.SessionKey, "messageId": id, "message": entry}
}

// stallAfterTurnStarted advances the engine's clock past the stall window once turn n has
// started: the visit stamps its progress right after it logs the start, so the advance
// waits a few ticks to land after that stamp.
func stallAfterTurnStarted(t *testing.T, env *testEnv, runID string, n int) {
	t.Helper()
	eventually(t, fmt.Sprintf("turn %d", n), func() bool {
		return env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='log' AND data->>'message'=$2`,
			runID, fmt.Sprintf("turn %d started", n)) == 1
	})
	time.Sleep(5 * env.e.tickInterval)
	env.clock.Advance(env.e.modelStall + time.Minute)
}

// T5.5.6: the model's reply after a tool result never comes → after the stall window the
// turn is aborted and continued in a new turn ("Continue."); the aborted turn's late
// terminal event does not end the visit.
func TestModelStallContinues(t *testing.T) {
	env := newEnv(t)
	env.e.modelStall = 10 * time.Minute
	a := env.agent(nil)
	var first openclaw.ChatSendParams
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if n == 1 {
			first = p
			// A tool call and its result, then nothing: the request for the next reply hangs.
			env.fc.push("session.message", assistantMsg(p.SessionKey, p.IdempotencyKey,
				[]any{toolCallPart("call_ls", "exec", map[string]any{"command": "ls"})}, "toolUse", 100, 10, 0))
			env.fc.push("agent", itemEvent(p.SessionKey, p.IdempotencyKey, "call_ls", "exec", "start", "running", ""))
			env.fc.push("agent", itemEvent(p.SessionKey, p.IdempotencyKey, "call_ls", "exec", "end", "completed", ""))
			env.fc.push("session.message", toolResultMsg(p, "call_ls", "exec", "README.md"))
		} else {
			p.Message = first.Message // the token travels in the first turn's message only
			go finishThenFinal(t, env, p, `{"summary":"after the stall"}`)
		}
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x", Limits: hourLimit})
	// Both entries mirrored: the visit stamped its progress before saving each.
	eventually(t, "the tool result", func() bool {
		return env.count(`SELECT coalesce(max(jsonb_array_length(messages)), 0) FROM run_nodes WHERE run_id=$1`, run.ID) == 2
	})
	env.clock.Advance(env.e.modelStall + time.Minute)
	r := env.wait(done, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	sends := env.fc.sendParams()
	if len(sends) != 2 || sends[1].Message != "Continue." || sends[1].IdempotencyKey != sends[0].IdempotencyKey+":2" {
		t.Fatalf("turns = %+v", sends)
	}
	aborts := env.fc.callsFor("chat.abort")
	if len(aborts) != 1 {
		t.Fatalf("aborts = %d", len(aborts))
	}
	var ab struct{ SessionKey, RunID string }
	_ = json.Unmarshal(aborts[0], &ab)
	if ab.RunID != sends[0].IdempotencyKey || ab.SessionKey != sends[0].SessionKey {
		t.Fatalf("abort = %+v, want the stalled turn %s", ab, sends[0].IdempotencyKey)
	}
	if n := env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='log' AND data->>'level'='warn' AND data->>'message'=$2`,
		run.ID, "model reply stalled for 10 minutes; continuing in a new turn, retry 1 of 3"); n != 1 {
		t.Fatalf("stall warnings = %d", n)
	}
	if n := env.count(`SELECT count(*) FROM run_nodes WHERE run_id=$1`, run.ID); n != 1 {
		t.Fatalf("run nodes = %d", n)
	}
}

// T5.5.7: a tool call without its result is the tool's time, not the model's: however long
// it runs, the turn is not taken for stalled.
func TestModelStallWaitsForOpenTool(t *testing.T) {
	env := newEnv(t)
	env.e.modelStall = 10 * time.Minute
	a := env.agent(nil)
	started := make(chan openclaw.ChatSendParams, 1)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		env.fc.push("session.message", assistantMsg(p.SessionKey, p.IdempotencyKey,
			[]any{toolCallPart("call_tests", "exec", map[string]any{"command": "make test"})}, "toolUse", 100, 10, 0))
		env.fc.push("agent", itemEvent(p.SessionKey, p.IdempotencyKey, "call_tests", "exec", "start", "running", ""))
		started <- p
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x", Limits: hourLimit})
	p := <-started
	eventually(t, "the running tool", func() bool {
		return env.count(`SELECT count(*) FROM tool_calls WHERE run_id=$1`, run.ID) == 1
	})
	env.clock.Advance(3 * env.e.modelStall)
	time.Sleep(10 * env.e.tickInterval)
	if n := len(env.fc.callsFor("chat.abort")); n != 0 {
		t.Fatalf("a turn waiting on its tool was aborted (%d aborts)", n)
	}
	env.fc.push("agent", itemEvent(p.SessionKey, p.IdempotencyKey, "call_tests", "exec", "end", "completed", ""))
	env.fc.push("session.message", toolResultMsg(p, "call_tests", "exec", "ok"))
	finishThenFinal(t, env, p, `{"summary":"tests passed"}`)
	r := env.wait(done, run.ID)
	if r.Status != store.RunSucceeded || len(env.fc.sendParams()) != 1 {
		t.Fatalf("run = %s %q sends=%d", r.Status, deref(r.FailureReason), len(env.fc.sendParams()))
	}
}

// T5.5.8: a model that stalls every turn is continued three times, sharing the budget of
// model errors (rule 3), then the node fails with the reason.
func TestModelStallGivesUp(t *testing.T) {
	env := newEnv(t)
	env.e.modelStall = 10 * time.Minute
	a := env.agent(nil)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) { return startedTurn(p) }
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x", Limits: hourLimit})
	for n := 1; n <= 4; n++ {
		stallAfterTurnStarted(t, env, run.ID, n)
	}
	r := env.wait(done, run.ID)
	if r.Status != store.RunFailed || deref(r.FailureReason) != "model reply stalled for 10 minutes" {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if n := len(env.fc.sendParams()); n != 4 {
		t.Fatalf("turns = %d", n)
	}
	if n := len(env.fc.callsFor("chat.abort")); n < 4 {
		t.Fatalf("aborts = %d, want every stalled turn aborted", n)
	}
	if n := env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='log' AND data->>'level'='warn' AND data->>'message' LIKE 'model reply stalled for 10 minutes; continuing%'`, run.ID); n != 3 {
		t.Fatalf("stall warnings = %d", n)
	}
}

func TestStallText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Minute: "1 minute", 10 * time.Minute: "10 minutes", 90 * time.Second: "1m30s", 2 * time.Hour: "120 minutes",
	} {
		if got := stallText(d); got != want {
			t.Errorf("stallText(%s) = %q, want %q", d, got, want)
		}
	}
}
