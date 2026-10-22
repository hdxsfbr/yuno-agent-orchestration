package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"yuno/internal/liveness"
	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// evidenceRow is a visit's liveness evidence as its row holds it (DESIGN 5.12).
type evidenceRow struct {
	heartbeat, progress, modelWait *time.Time
	waitingOn                      string
	calls                          []liveness.OpenCall
}

func (env *testEnv) evidence(runID string) evidenceRow {
	env.t.Helper()
	var r evidenceRow
	var raw json.RawMessage
	err := env.pool.QueryRow(context.Background(), `SELECT heartbeat_at, progress_at, model_wait_since, waiting_on, open_calls
		FROM run_nodes WHERE run_id=$1 ORDER BY visit DESC LIMIT 1`, runID).Scan(&r.heartbeat, &r.progress, &r.modelWait, &r.waitingOn, &raw)
	if err != nil {
		env.t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &r.calls); err != nil {
		env.t.Fatalf("open_calls %s: %v", raw, err)
	}
	return r
}

// sameInstant compares a stored timestamp with the engine's clock at the database's
// microsecond precision.
func sameInstant(stored *time.Time, want time.Time) bool {
	return stored != nil && stored.Equal(want.Truncate(time.Microsecond))
}

// DESIGN 5.12: a live visit writes its evidence. The heartbeat at its start and on the
// worker's tick; waiting_on from setup to the model, a tool, a person and back, with
// model_wait_since only while it waits on the model; open_calls with the redacted summary;
// progress_at with those writes or, alone, at most once per 30 s of the engine's clock.
func TestEvidenceWritten(t *testing.T) {
	env := newEnv(t)
	env.e.heartbeat = 50 * time.Millisecond
	env.e.modelStall = 10 * time.Minute
	a := env.agent(nil)
	t0 := env.clock.Now()
	sending, proceed := make(chan openclaw.ChatSendParams, 1), make(chan struct{})
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		sending <- p
		<-proceed
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x", Limits: hourLimit})
	p := <-sending

	// Before the first turn: setup, with the visit's first heartbeat and progress.
	r := env.evidence(run.ID)
	if r.waitingOn != store.WaitingOnSetup || !sameInstant(r.heartbeat, t0) || !sameInstant(r.progress, t0) ||
		r.modelWait != nil || len(r.calls) != 0 {
		t.Fatalf("at setup: %+v", r)
	}
	close(proceed)
	eventually(t, "waiting on the model", func() bool {
		r = env.evidence(run.ID)
		return r.waitingOn == store.WaitingOnModel
	})
	if !sameInstant(r.modelWait, t0) || len(r.calls) != 0 {
		t.Fatalf("on the model: %+v", r)
	}

	// The model asks for a command: waiting on the tool, the call open with its summary
	// redacted, the progress written with the change.
	env.clock.Advance(10 * time.Second)
	secret := "ghp_" + strings.Repeat("a1B2", 9) // a token-shaped value, not a credential
	env.fc.push("session.message", assistantMsg(p.SessionKey, p.IdempotencyKey,
		[]any{toolCallPart("call_tests", "exec", map[string]any{"command": "make test\n  GH_TOKEN=" + secret})}, "toolUse", 100, 10, 0))
	env.fc.push("agent", itemEvent(p.SessionKey, p.IdempotencyKey, "call_tests", "exec", "start", "running", ""))
	eventually(t, "waiting on the tool", func() bool {
		r = env.evidence(run.ID)
		return r.waitingOn == store.WaitingOnTool && len(r.calls) == 1 && r.calls[0].Summary != ""
	})
	c := r.calls[0]
	if c.ID != "call_tests" || c.Name != "exec" || c.Summary != "make test GH_TOKEN=[redacted]" ||
		time.Since(c.StartedAt) > time.Minute || r.modelWait != nil || !sameInstant(r.progress, t0.Add(10*time.Second)) {
		t.Fatalf("on the tool: %+v %+v", r, c)
	}

	// Progress alone waits for 30 s of the engine's clock since the last write.
	env.clock.Advance(10 * time.Second)
	env.fc.push("agent", map[string]any{"runId": p.IdempotencyKey, "sessionKey": p.SessionKey, "agentId": "x",
		"stream": "command_output", "ts": time.Now().UnixMilli(), "data": map[string]any{"toolCallId": "call_tests", "output": "ok 1"}})
	time.Sleep(10 * env.e.tickInterval)
	if r = env.evidence(run.ID); !sameInstant(r.progress, t0.Add(10*time.Second)) {
		t.Fatalf("progress written within 30 s: %v", r.progress)
	}
	env.clock.Advance(25 * time.Second)
	eventually(t, "the progress after 30 s", func() bool {
		return sameInstant(env.evidence(run.ID).progress, t0.Add(20*time.Second))
	})

	// The heartbeat follows the worker's tick on the engine's clock.
	eventually(t, "a heartbeat on the tick", func() bool {
		return sameInstant(env.evidence(run.ID).heartbeat, t0.Add(45*time.Second))
	})

	// An exec approval pauses the visit: waiting on a person, until it is decided.
	env.fc.push("session.approval", map[string]any{"sessionKey": p.SessionKey, "phase": "pending", "approval": map[string]any{
		"id": "exec-1", "status": "pending", "presentation": map[string]any{"kind": "exec", "commandText": "make deploy"}}})
	eventually(t, "waiting on a person", func() bool { return env.evidence(run.ID).waitingOn == store.WaitingOnHuman })
	env.fc.push("session.approval", map[string]any{"sessionKey": p.SessionKey, "phase": "terminal", "approval": map[string]any{
		"id": "exec-1", "status": "allowed", "decision": "allow-once"}})
	eventually(t, "back on the tool", func() bool { return env.evidence(run.ID).waitingOn == store.WaitingOnTool })

	// The tool's result: back on the model, from now, with no open call.
	env.fc.push("agent", itemEvent(p.SessionKey, p.IdempotencyKey, "call_tests", "exec", "end", "completed", ""))
	env.fc.push("session.message", toolResultMsg(p, "call_tests", "exec", "ok"))
	eventually(t, "back on the model", func() bool {
		r = env.evidence(run.ID)
		return r.waitingOn == store.WaitingOnModel && len(r.calls) == 0
	})
	if !sameInstant(r.modelWait, t0.Add(45*time.Second)) {
		t.Fatalf("model_wait_since = %v, want the moment the tool ended", r.modelWait)
	}

	finishThenFinal(t, env, p, `{"summary":"tests passed"}`)
	if got := env.wait(done, run.ID); got.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", got.Status, deref(got.FailureReason))
	}
}

// An open call's summary is the redacted first 200 characters of its command or path, on
// one line.
func TestCallSummary(t *testing.T) {
	long := strings.Repeat("é", 250)
	for _, tc := range []struct{ in, want string }{
		{`{"command":"ls -la\n  /tmp"}`, "ls -la /tmp"},
		{`{"path":"runs/r1/notes.md","content":"x"}`, "runs/r1/notes.md"},
		{`{"file_path":"README.md"}`, "README.md"},
		{`{"command":"` + long + `"}`, strings.Repeat("é", liveness.SummaryChars)},
		{`{"command":"curl -H 'Authorization: Bearer abcdefghijklmnopqrstuvwxyz'"}`, "curl -H 'Authorization: [redacted]'"},
		{`{"query":"weather"}`, ""},
		{`{"command":["ls","-l"]}`, ""},
		{`not json`, ""},
	} {
		if got := callSummary(json.RawMessage(tc.in)); got != tc.want {
			t.Errorf("callSummary(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
