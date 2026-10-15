package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// ---- pure rules -----------------------------------------------------------------------

func TestTransitionTable(t *testing.T) {
	allowed := map[string][]string{
		store.RunQueued:          {store.RunRunning, store.RunCancelled},
		store.RunRunning:         {store.RunQueued, store.RunWaitingApproval, store.RunSucceeded, store.RunFailed, store.RunCancelled, store.RunTimedOut},
		store.RunWaitingApproval: {store.RunQueued, store.RunRunning, store.RunFailed, store.RunCancelled, store.RunTimedOut},
	}
	n := 0
	for _, from := range RunStatuses {
		for _, to := range RunStatuses {
			n++
			err := ValidTransition(from, to)
			if slices.Contains(allowed[from], to) {
				if err != nil {
					t.Errorf("%s→%s refused: %v", from, to, err)
				}
				continue
			}
			if err == nil || err.Error() != "invalid transition "+from+"→"+to {
				t.Errorf("%s→%s: err = %v", from, to, err)
			}
		}
	}
	if n != 49 {
		t.Fatalf("checked %d pairs", n)
	}
}

func TestCheckLimitsBoundaries(t *testing.T) {
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	by := "andre"
	base := func() *store.Run { return &store.Run{StartedAt: &t0} }
	l := store.Limits{MaxSteps: 5, MaxDurationSeconds: 60, MaxNodeIterations: 4, MaxCostUSD: 0.5, MaxToolCalls: 3}
	tests := []struct {
		name     string
		mut      func(r *store.Run)
		now      time.Time
		iter     int
		accepted bool
		want     string // "" = no violation
	}{
		{name: "deadline one ms before", now: t0.Add(60*time.Second - time.Millisecond)},
		{name: "deadline exactly", now: t0.Add(60 * time.Second), want: "timed_out: time limit 60s exceeded"},
		{name: "paused time does not count", now: t0.Add(70 * time.Second), mut: func(r *store.Run) { r.PausedTotalMs = 10_001 }},
		{name: "open pause does not count", now: t0.Add(70 * time.Second), mut: func(r *store.Run) {
			p := t0.Add(50 * time.Second)
			r.PausedAt = &p
		}},
		{name: "steps N-1", now: t0, mut: func(r *store.Run) { r.StepCount = 4 }},
		{name: "steps N", now: t0, mut: func(r *store.Run) { r.StepCount = 5 }, want: "failed: step limit 5 exceeded"},
		{name: "cost below", now: t0, mut: func(r *store.Run) { r.CostUSDMicros = 499_999 }},
		{name: "cost at limit", now: t0, mut: func(r *store.Run) { r.CostUSDMicros = 500_000 }, want: "failed: cost limit $0.50 exceeded"},
		{name: "node iterations N-1", now: t0, iter: 3},
		{name: "node iterations N", now: t0, iter: 4, want: "failed: node iteration limit 4 exceeded"},
		{name: "tool calls N", now: t0, mut: func(r *store.Run) { r.ToolCallCount = 3 }, want: "failed: tool call limit 3 exceeded"},
		{name: "cancel first", now: t0.Add(time.Hour), mut: func(r *store.Run) { r.CancelRequested, r.CancelBy = true, &by }, want: "cancelled: cancelled by andre"},
		{name: "counters ignored after finish", now: t0, accepted: true, mut: func(r *store.Run) { r.StepCount = 99 }},
		{name: "time still applies after finish", now: t0.Add(time.Minute), accepted: true, want: "timed_out: time limit 60s exceeded"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := base()
			if tc.mut != nil {
				tc.mut(r)
			}
			v := checkLimits(limitState{run: r, limits: l, now: tc.now, nodeIterations: tc.iter, finishAccepted: tc.accepted})
			got := ""
			if v != nil {
				got = v.status + ": " + v.reason
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	small := checkLimits(limitState{run: &store.Run{StartedAt: &t0, CostUSDMicros: 600}, limits: store.Limits{MaxCostUSD: 0.0005}, now: t0})
	if small == nil || small.reason != "cost limit $0.0005 exceeded" {
		t.Fatalf("small cost limit: %+v", small)
	}
}

func TestResolveLimits(t *testing.T) {
	cfg := testConfig()
	cfg.MaxCostUSD = 1
	got, err := resolveLimits(store.Limits{MaxDurationSeconds: 30, MaxCostUSD: 5}, store.Limits{MaxSteps: 7, MaxDurationSeconds: 99}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := store.DefaultLimits
	want.MaxSteps, want.MaxDurationSeconds, want.MaxCostUSD = 7, 30, 1
	if got != want {
		t.Fatalf("limits = %+v, want %+v", got, want)
	}
	cfg.MaxSteps = 5
	if got, _ := resolveLimits(store.Limits{MaxSteps: 50}, store.Limits{}, cfg); got.MaxSteps != 5 {
		t.Fatalf("clamp: %d", got.MaxSteps)
	}
	_, err = resolveLimits(store.Limits{MaxSteps: -1}, store.Limits{}, cfg)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Messages[0] != "limits.max_steps: must not be negative" {
		t.Fatalf("err = %v", err)
	}
	// A negative limit inherited from the agent is refused, not replaced by the default.
	_, err = resolveLimits(store.Limits{}, store.Limits{MaxToolCalls: -2, MaxCostUSD: -1}, cfg)
	if !errors.As(err, &ve) || !slices.Equal(ve.Messages, []string{"agent limits.max_cost_usd: must not be negative",
		"agent limits.max_tool_calls: must not be negative"}) {
		t.Fatalf("inherited negative limits: %v", err)
	}
}

func TestRetryBackoff(t *testing.T) {
	for i, want := range []time.Duration{1, 2, 4, 8, 16, 30, 30} {
		if got := retryBackoff(i + 1); got != want*time.Second {
			t.Fatalf("attempt %d: %v", i+1, got)
		}
	}
}

func TestTokenRegistryExpiry(t *testing.T) {
	clock := NewFakeClock(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	r := newTokenRegistry(clock)
	rc := RunContext{RunID: "run-1", NodeID: "agent"}
	r.add("r_a", &tokenEntry{rc: rc, expiresAt: clock.Now().Add(time.Minute)})
	r.add("r_b", &tokenEntry{rc: rc})
	if e, ok := r.get("r_a"); !ok || e.rc.RunID != "run-1" {
		t.Fatal("fresh token not resolved")
	}
	clock.Advance(time.Minute - time.Millisecond)
	if _, ok := r.get("r_a"); !ok {
		t.Fatal("token expired early")
	}
	clock.Advance(time.Millisecond)
	if _, ok := r.get("r_a"); ok {
		t.Fatal("expired token resolved")
	}
	r.remove("r_b")
	if _, ok := r.get("r_b"); ok {
		t.Fatal("revoked token resolved")
	}
	if _, ok := r.get("r_unknown"); ok {
		t.Fatal("unknown token resolved")
	}
}

func TestValidateOutputAndTextFallback(t *testing.T) {
	s, err := compileSchema(json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"},"n":{"type":"integer"}},"required":["summary"]}`))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ in, want string }{
		{`{"summary":"ok","n":2}`, ""},
		{`{"n":2}`, "result at /: missing property 'summary'"},
		{`{"summary":3}`, "result at /summary: got number, want string"},
		{`[1]`, "result must be a JSON object"},
		{`{"summary":`, "result is not valid JSON"},
	}
	for _, tc := range tests {
		if got := validateOutput(s, json.RawMessage(tc.in)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.in, got, tc.want)
		}
	}
	out, ok := findJSONResult(s, "Done. {\"n\": 1} and then ```json\n{\"summary\": \"all good\"}\n```")
	if !ok || string(out) != `{"summary": "all good"}` {
		t.Fatalf("fallback = %s, %v", out, ok)
	}
	if _, ok := findJSONResult(s, "no json here {nope"); ok {
		t.Fatal("fallback found a result in plain text")
	}
}

func TestTaskMessage(t *testing.T) {
	run := &store.Run{ID: "0199f0a2-0000-7000-8000-000000000001", Input: json.RawMessage(`{"task":"Count to 3."}`)}
	// A wave 1 snapshot's task ("{{input.task}}") still renders.
	node := &workflow.Node{Task: "{{input.task}} Save runs/$RUN_ID/notes.md.", OutputSchema: json.RawMessage("{\n  \"type\": \"object\"\n}")}
	w := &walk{run: run, def: &workflow.Definition{Nodes: []workflow.Node{{ID: "start", Kind: workflow.KindStart}}}}
	task, err := w.renderTask(node)
	if err != nil {
		t.Fatal(err)
	}
	v := &visit{
		run:   run,
		node:  node,
		task:  task,
		token: "r_abcdefghijklmnopqrstuvwx",
		over:  runOverrides{ScriptID: "001-quick-run-happy"},
	}
	want := "Count to 3. Save runs/0199f0a2-0000-7000-8000-000000000001/notes.md.\n\n" +
		"--- platform protocol ---\n" +
		"run_id: 0199f0a2-0000-7000-8000-000000000001\n" +
		"run_token: r_abcdefghijklmnopqrstuvwx\n" +
		"script_id: 001-quick-run-happy\n" +
		"Work inside the directory runs/0199f0a2-0000-7000-8000-000000000001/ of your workspace.\n" +
		`When finished, call the tool yuno__finish with {"run_token": "r_abcdefghijklmnopqrstuvwx", "result": <object matching this JSON schema>}:` + "\n" +
		`{"type":"object"}`
	if got := v.taskMessage(); got != want {
		t.Fatalf("message:\n%s\nwant:\n%s", got, want)
	}
	v.over = runOverrides{}
	if strings.Contains(v.taskMessage(), "script_id") {
		t.Fatal("script_id line without a script")
	}
}

func TestSanitizeContentDropsReasoning(t *testing.T) {
	in := json.RawMessage(`[{"type":"thinking","thinking":"secret","thinkingSignature":"s"},{"type":"text","text":"hi","reasoning":"x"},{"type":"redacted_thinking","data":"y"}]`)
	if got := string(sanitizeContent(in)); got != `[{"text":"hi","type":"text"}]` {
		t.Fatalf("got %s", got)
	}
	if got := string(sanitizeContent(json.RawMessage(`"plain"`))); got != `"plain"` {
		t.Fatalf("got %s", got)
	}
}

// ---- database: state machine, claiming, trigger -------------------------------------

func TestTransitionsInDB(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := &store.Workflow{Name: "w", Definition: json.RawMessage(`{}`)}
	if err := store.CreateWorkflow(ctx, pool, wf); err != nil {
		t.Fatal(err)
	}
	e := New(Deps{Cfg: testConfig(), Pool: pool})
	for _, from := range RunStatuses {
		for _, to := range RunStatuses {
			r := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: json.RawMessage(`{}`), Trigger: "manual", Status: from}
			if err := store.CreateRun(ctx, pool, r); err != nil {
				t.Fatal(err)
			}
			got, err := e.transition(ctx, r.ID, to, nil, time.Now())
			if ValidTransition(from, to) == nil {
				if err != nil || got.Status != to {
					t.Errorf("%s→%s: %v", from, to, err)
				}
				if store.IsTerminal(to) && got.FinishedAt == nil {
					t.Errorf("%s→%s: finished_at not set", from, to)
				}
				continue
			}
			if from == to {
				continue // already there: a no-op, not a transition
			}
			if from == store.RunWaitingApproval && ValidTransition(store.RunRunning, to) == nil {
				continue // resumed in place, then moved on: two valid steps
			}
			if err == nil || err.Error() != "invalid transition "+from+"→"+to {
				t.Errorf("%s→%s: err = %v", from, to, err)
			}
		}
	}
}

func TestWorkersClaimEachRunOnce(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := &store.Workflow{Name: "w", Definition: json.RawMessage(`{}`)}
	if err := store.CreateWorkflow(ctx, pool, wf); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		r := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: json.RawMessage(`{}`), Trigger: "manual"}
		if err := store.CreateRun(ctx, pool, r); err != nil {
			t.Fatal(err)
		}
	}
	e := New(Deps{Cfg: testConfig(), Pool: pool, Log: slog.New(slog.DiscardHandler)})
	e.pollInterval = 10 * time.Millisecond
	var mu sync.Mutex
	claims := map[string]int{}
	e.execute = func(ctx context.Context, run *store.Run) {
		mu.Lock()
		claims[run.ID]++
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		if _, err := store.TransitionRun(context.Background(), pool, run.ID, []string{store.RunRunning}, store.RunSucceeded, nil, time.Now()); err != nil {
			t.Errorf("finish %s: %v", run.ID, err)
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() { _ = e.Start(runCtx); close(stopped) }()
	eventually(t, "20 claims", func() bool { mu.Lock(); defer mu.Unlock(); return len(claims) == 20 })
	eventually(t, "20 runs processed", func() bool {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE status = 'succeeded'`).Scan(&n)
		return n == 20
	})
	cancel()
	<-stopped
	for id, n := range claims {
		if n != 1 {
			t.Errorf("run %s claimed %d times", id, n)
		}
	}
	var queued int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE status <> 'succeeded'`).Scan(&queued)
	if queued != 0 {
		t.Fatalf("%d runs not processed", queued)
	}
}

func TestTrigger(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(func(a *store.Agent) { a.Limits = store.Limits{MaxSteps: 7} })
	var ve *ValidationError
	if _, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "  "}); !errors.As(err, &ve) || ve.Messages[0] != "task: required" {
		t.Fatalf("empty task: %v", err)
	}
	if _, err := env.e.Trigger(ctx, TriggerRequest{Task: "x"}); !errors.As(err, &ve) || ve.Messages[0] != "agent_id: required" {
		t.Fatalf("no agent: %v", err)
	}
	if _, err := env.e.Trigger(ctx, TriggerRequest{AgentID: "0199f0a2-0000-7000-8000-00000000dead", Task: "x"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown agent: %v", err)
	}
	if _, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "x", Limits: store.Limits{MaxCostUSD: -1}}); !errors.As(err, &ve) {
		t.Fatalf("negative limit: %v", err)
	}
	neg := env.agent(func(a *store.Agent) { a.Limits = store.Limits{MaxSteps: -3} }) // stored around the API
	if _, err := env.e.Trigger(ctx, TriggerRequest{AgentID: neg.ID, Task: "x"}); !errors.As(err, &ve) ||
		ve.Messages[0] != "agent limits.max_steps: must not be negative" {
		t.Fatalf("negative agent limit: %v", err)
	}
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "Say hi.", Limits: store.Limits{MaxDurationSeconds: 30},
		Trigger: store.TriggerEval, TriggerRef: "case-1", Model: "scripted/scripted-1", ScriptID: "case-1"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunQueued || string(run.Input) != `{"task": "Say hi."}` || run.Trigger != store.TriggerEval || *run.TriggerRef != "case-1" {
		t.Fatalf("run = %+v input=%s", run, run.Input)
	}
	if run.Limits.MaxSteps != 7 || run.Limits.MaxDurationSeconds != 30 || run.Limits.MaxCostUSD != store.DefaultLimits.MaxCostUSD {
		t.Fatalf("limits = %+v", run.Limits)
	}
	var snap runSnapshot
	if err := json.Unmarshal(run.WorkflowSnapshot, &snap); err != nil {
		t.Fatal(err)
	}
	node, ok := snap.Definition.Node("agent")
	if !ok || node.AgentID != a.ID || node.Task != "{{.input.task}}" || len(node.OutputSchema) == 0 {
		t.Fatalf("snapshot node = %+v, %v", node, ok)
	}
	if snap.RunOverrides == nil || snap.RunOverrides.Model != "scripted/scripted-1" || snap.RunOverrides.ScriptID != "case-1" || snap.Limits != run.Limits {
		t.Fatalf("snapshot = %s", run.WorkflowSnapshot)
	}
	wf, err := store.GetWorkflowByTemplateKey(ctx, env.pool, QuickRunTemplateKey(a.ID))
	if err != nil || wf.ID != run.WorkflowID {
		t.Fatalf("quick-run workflow: %v", err)
	}
	if _, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "again"}); err != nil {
		t.Fatal(err)
	}
	if n := env.count(`SELECT count(*) FROM workflows`); n != 1 {
		t.Fatalf("%d workflows, want one hidden workflow per agent", n)
	}
	if n := env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='run.status'`, run.ID); n != 1 {
		t.Fatalf("run.status events = %d", n)
	}
	cancelled, err := env.e.Cancel(ctx, run.ID, "andre")
	if err != nil || cancelled.Status != store.RunCancelled || *cancelled.FailureReason != "cancelled by andre" {
		t.Fatalf("cancel queued: %+v, %v", cancelled, err)
	}
}

// ---- database: full runs through the fake gateway ------------------------------------

func TestFullRunFromFixture(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	ocID := openclaw.DeriveAgentID(a.ID)
	var (
		mu       sync.Mutex
		rc       RunContext
		rcOK     bool
		tokenVal string
	)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			token := tokenIn(p.Message)
			got, ok := env.e.ResolveRunToken(token)
			mu.Lock()
			rc, rcOK, tokenVal = got, ok, token
			mu.Unlock()
			for _, ev := range loadFixture(t, "turn_toolcall", p.SessionKey, p.IdempotencyKey, ocID) {
				env.fc.push(ev.Name, ev.Payload)
				var ae openclaw.AgentEvent
				if ev.Name == "agent" && json.Unmarshal(ev.Payload, &ae) == nil && ae.Stream == "command_output" {
					if err := env.e.AcceptFinish(ctx, token, json.RawMessage(`{"summary":"echo was denied"}`)); err != nil {
						t.Errorf("AcceptFinish: %v", err)
					}
				}
			}
		}()
		return openclaw.ChatSendResult{RunID: p.IdempotencyKey, Status: "started"}, nil
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "please run the command"})
	r := env.wait(done, run.ID)

	if r.Status != store.RunSucceeded || r.FailureReason != nil {
		t.Fatalf("run = %s (%v)", r.Status, r.FailureReason)
	}
	if r.StepCount != 2 || r.ToolCallCount != 1 || r.CostUSDMicros != 157+158 || r.InputTokens != 122+123 || r.OutputTokens != 14 {
		t.Fatalf("counters: steps=%d tools=%d cost=%d in=%d out=%d", r.StepCount, r.ToolCallCount, r.CostUSDMicros, r.InputTokens, r.OutputTokens)
	}
	if string(r.State) != `{"agent": {"summary": "echo was denied"}}` {
		t.Fatalf("state = %s", r.State)
	}
	tr, err := store.GetTrace(ctx, env.pool, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Nodes) != 1 {
		t.Fatalf("nodes = %d", len(tr.Nodes))
	}
	node := tr.Nodes[0]
	wantKey := "agent:" + ocID + ":yuno-" + run.ID + "-agent-1"
	if node.Status != store.NodeSucceeded || *node.SessionKey != wantKey || *node.OpenClawRunID != node.ID ||
		string(node.Output) != `{"summary": "echo was denied"}` {
		t.Fatalf("node = %+v output=%s", node, node.Output)
	}
	if node.Iteration != 2 {
		t.Fatalf("node iteration = %d, want the visit's 2 steps", node.Iteration)
	}
	var msgs []map[string]any
	_ = json.Unmarshal(node.Messages, &msgs)
	if len(msgs) != 3 || msgs[0]["role"] != "user" || msgs[1]["role"] != "assistant" || msgs[2]["role"] != "assistant" {
		t.Fatalf("messages = %s", node.Messages)
	}

	if len(tr.ModelCalls) != 2 {
		t.Fatalf("model calls = %d", len(tr.ModelCalls))
	}
	mc := tr.ModelCalls[0]
	if mc.Provider != "mock" || mc.Model != "mock-1" || *mc.StopReason != "toolUse" || *mc.RequestID != "chatcmpl-2" ||
		mc.InputTokens != 122 || mc.OutputTokens != 7 || mc.CostUSDMicros != 157 || mc.Attempt != 1 || mc.DurationMs != 10 {
		t.Fatalf("model call = %+v", mc)
	}
	if len(tr.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(tr.ToolCalls))
	}
	tc := tr.ToolCalls[0]
	if tc.ToolUseID != "call_2" || tc.ToolName != "exec" || tc.Status != store.ToolFailed || !tc.IsError ||
		string(tc.Input) != `{"command": "echo hi-from-exec"}` || tc.ModelCallID == nil || *tc.ModelCallID != mc.ID ||
		tc.DurationMs != 97 || !strings.Contains(string(tc.Output), `"status": "error"`) {
		t.Fatalf("tool call = %+v input=%s output=%.120s", tc, tc.Input, tc.Output)
	}
	if len(tr.Approvals) != 1 {
		t.Fatalf("approvals = %d", len(tr.Approvals))
	}
	ap := tr.Approvals[0]
	if ap.Kind != "exec" || ap.Status != "rejected" || *ap.Reason != "no-route" || ap.Message != "echo hi-from-exec" ||
		*ap.ExternalID != "7dc94647-9b8a-42d0-9002-76e7c5deaad3" || ap.RunNodeID != node.ID || ap.ExpiresAt.UnixMilli() != 1792088501740 {
		t.Fatalf("approval = %+v", ap)
	}

	kinds := map[string]int{}
	var root, nodeSpan string
	for _, s := range tr.Spans {
		kinds[s.Kind]++
		if s.EndedAt == nil {
			t.Errorf("span %s/%s not ended", s.Kind, s.Name)
		}
		switch s.Kind {
		case "run":
			root = s.ID
		case "node":
			nodeSpan = s.ID
		}
	}
	if kinds["run"] != 1 || kinds["node"] != 1 || kinds["model_call"] != 2 || kinds["tool_call"] != 1 || kinds["approval_wait"] != 1 {
		t.Fatalf("span kinds = %v", kinds)
	}
	for _, s := range tr.Spans {
		switch {
		case s.Kind == "node" && (s.ParentID == nil || *s.ParentID != root):
			t.Errorf("node span parent = %v", s.ParentID)
		case s.Kind != "run" && s.Kind != "node" && (s.ParentID == nil || *s.ParentID != nodeSpan):
			t.Errorf("%s span parent = %v", s.Kind, s.ParentID)
		}
	}

	// Call events carry the run's counters, so a live run summary follows them.
	if n := env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='model_call' AND (data->'run'->>'step_count')::int > 0`, run.ID); n != 2 {
		t.Errorf("model_call events with the run's counters = %d, want 2", n)
	}
	if n := env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='tool_call' AND (data->'run'->>'tool_call_count')::int = 1`, run.ID); n == 0 {
		t.Error("no tool_call event carries the run's tool call count")
	}
	for _, typ := range []string{"run.status", "node.status", "model_call", "tool_call", "approval.requested", "approval.decided", "log"} {
		if env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event=$2`, run.ID, typ) == 0 {
			t.Errorf("no %s event", typ)
		}
	}
	if env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='run.status' AND data->'run'->>'status'='waiting_approval'`, run.ID) != 1 {
		t.Error("run did not enter waiting_approval while the exec approval was pending")
	}

	sends := env.fc.sendParams()
	if len(sends) != 1 || sends[0].SessionKey != wantKey || sends[0].AgentID != ocID || sends[0].IdempotencyKey != node.ID {
		t.Fatalf("chat.send = %+v", sends)
	}
	if subs := env.fc.callsFor("sessions.messages.subscribe"); len(subs) != 1 || !strings.Contains(string(subs[0]), `"includeApprovals":true`) {
		t.Fatalf("subscribe = %s", subs)
	}
	if dels := env.fc.callsFor("sessions.delete"); len(dels) != 1 || !strings.Contains(string(dels[0]), wantKey) {
		t.Fatalf("sessions.delete = %s", dels)
	}
	if len(env.fc.callsFor("sessions.patch")) != 0 {
		t.Fatal("sessions.patch without a model override")
	}
	mu.Lock()
	defer mu.Unlock()
	if !rcOK || rc.RunID != run.ID || rc.RunNodeID != node.ID || rc.NodeID != "agent" || rc.AgentID != a.ID || rc.OpenClawAgentID != ocID {
		t.Fatalf("run context = %+v (%v)", rc, rcOK)
	}
	if _, ok := env.e.ResolveRunToken(tokenVal); ok {
		t.Fatal("token still valid after the visit")
	}
	if err := env.e.AcceptFinish(ctx, tokenVal, json.RawMessage(`{"summary":"late"}`)); !errors.Is(err, ErrInvalidRunToken) {
		t.Fatalf("late finish: %v", err)
	}
	ws := openclaw.WorkspaceDir(env.dir, ocID)
	if _, err := os.Stat(filepath.Join(ws, "AGENTS.md")); err != nil {
		t.Fatalf("agent not materialised: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "runs", run.ID)); err != nil {
		t.Fatalf("run directory: %v", err)
	}
	got, _ := store.GetAgent(ctx, env.pool, a.ID)
	if got.OpenClawAgentID == nil || *got.OpenClawAgentID != ocID || got.MaterializedHash == "" {
		t.Fatalf("agent materialisation not recorded: %+v", got)
	}
}

func TestModelOverrideAndFinishValidation(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			token := tokenIn(p.Message)
			err := env.e.AcceptFinish(ctx, token, json.RawMessage(`{"summary":42}`))
			var fe *FinishValidationError
			if !errors.As(err, &fe) || fe.Message != "result at /summary: got number, want string" {
				t.Errorf("invalid finish: %v", err)
			}
			if err := env.e.AcceptFinish(ctx, "r_unknownunknownunknownunk", json.RawMessage(`{}`)); !errors.Is(err, ErrInvalidRunToken) {
				t.Errorf("unknown token: %v", err)
			}
			finishThenFinal(t, env, p, `{"summary":"fixed"}`)
		}()
		return openclaw.ChatSendResult{RunID: p.IdempotencyKey, Status: "started"}, nil
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x", Model: "scripted/scripted-1", ScriptID: "s-1"})
	r := env.wait(done, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("status = %s %v", r.Status, r.FailureReason)
	}
	patches := env.fc.callsFor("sessions.patch")
	if len(patches) != 1 || !strings.Contains(string(patches[0]), `"model":"scripted/scripted-1"`) {
		t.Fatalf("sessions.patch = %s", patches)
	}
	if !strings.Contains(env.fc.sendParams()[0].Message, "\nscript_id: s-1\n") {
		t.Fatal("script_id missing from the task message")
	}
}

func TestCancelDuringTurn(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			env.fc.push("session.message", assistantMsg(p.SessionKey, p.IdempotencyKey,
				[]any{toolCallPart("call_1", "exec", map[string]any{"command": "sleep 4"})}, "toolUse", 110, 18, 0.0002))
			env.fc.push("agent", itemEvent(p.SessionKey, p.IdempotencyKey, "call_1", "exec", "start", "running", ""))
		}()
		return openclaw.ChatSendResult{RunID: p.IdempotencyKey, Status: "started"}, nil
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "slow"})
	eventually(t, "first step", func() bool { return env.count(`SELECT count(*) FROM tool_calls WHERE run_id=$1`, run.ID) == 1 })
	if _, err := env.e.Cancel(ctx, run.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	r := env.wait(done, run.ID)
	if r.Status != store.RunCancelled || *r.FailureReason != "cancelled by tester" {
		t.Fatalf("run = %s %v", r.Status, r.FailureReason)
	}
	aborts := env.fc.callsFor("chat.abort")
	if len(aborts) != 1 || !strings.Contains(string(aborts[0]), `"runId"`) || !strings.Contains(string(aborts[0]), `"sessionKey"`) {
		t.Fatalf("chat.abort = %s", aborts)
	}
	tr, _ := store.GetTrace(ctx, env.pool, run.ID)
	if tr.Nodes[0].Status != store.NodeFailed || *tr.Nodes[0].FailureReason != "cancelled by tester" {
		t.Fatalf("node = %+v", tr.Nodes[0])
	}
	if tr.ToolCalls[0].Status != store.ToolFailed || tr.ToolCalls[0].FinishedAt == nil {
		t.Fatalf("open tool call not closed: %+v", tr.ToolCalls[0])
	}
}

func TestDeadlineBothSides(t *testing.T) {
	env := newEnv(t)
	a := env.agent(func(a *store.Agent) { a.Limits.MaxDurationSeconds = 60 })
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "wait"})
	eventually(t, "turn started", func() bool { return len(env.fc.sendParams()) == 1 })
	env.clock.Advance(60*time.Second - time.Millisecond)
	time.Sleep(150 * time.Millisecond) // several limit ticks
	if n := len(env.fc.callsFor("chat.abort")); n != 0 {
		t.Fatalf("aborted one ms before the deadline (%d abort calls)", n)
	}
	if r, _ := store.GetRun(context.Background(), env.pool, run.ID); r.Status != store.RunRunning {
		t.Fatalf("status one ms before the deadline = %s", r.Status)
	}
	env.clock.Advance(time.Millisecond)
	r := env.wait(done, run.ID)
	if r.Status != store.RunTimedOut || *r.FailureReason != "time limit 60s exceeded" {
		t.Fatalf("run = %s %v", r.Status, r.FailureReason)
	}
}

// pushSteps sends n assistant steps that each request a tool, so the turn continues.
func pushSteps(env *testEnv, p openclaw.ChatSendParams, from, n int) {
	for i := from; i < from+n; i++ {
		env.fc.push("session.message", assistantMsg(p.SessionKey, p.IdempotencyKey,
			[]any{toolCallPart(fmt.Sprintf("call_%d", i), "ls", map[string]any{"path": "."})}, "toolUse", 122, 7, 0.000157))
	}
}

func TestStepLimitBothSides(t *testing.T) {
	env := newEnv(t)
	a := env.agent(nil)
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "loop", Limits: store.Limits{MaxSteps: 3}})
	eventually(t, "turn started", func() bool { return len(env.fc.sendParams()) == 1 })
	p := env.fc.sendParams()[0]
	pushSteps(env, p, 1, 2)
	eventually(t, "N-1 steps", func() bool { return env.count(`SELECT step_count FROM runs WHERE id=$1`, run.ID) == 2 })
	time.Sleep(100 * time.Millisecond)
	if len(env.fc.callsFor("chat.abort")) != 0 {
		t.Fatal("aborted at N-1 steps")
	}
	if n := env.count(`SELECT iteration FROM run_nodes WHERE run_id=$1`, run.ID); n != 2 {
		t.Fatalf("persisted node iteration = %d while the visit is at 2 steps", n)
	}
	pushSteps(env, p, 3, 1)
	r := env.wait(done, run.ID)
	if r.Status != store.RunFailed || *r.FailureReason != "step limit 3 exceeded" || r.StepCount != 3 {
		t.Fatalf("run = %s %v steps=%d", r.Status, r.FailureReason, r.StepCount)
	}
	if len(env.fc.callsFor("chat.abort")) != 1 {
		t.Fatal("turn not aborted")
	}
}

func TestCostOvershootByOneCall(t *testing.T) {
	env := newEnv(t)
	a := env.agent(nil)
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "spend", Limits: store.Limits{MaxCostUSD: 0.0005}})
	eventually(t, "turn started", func() bool { return len(env.fc.sendParams()) == 1 })
	p := env.fc.sendParams()[0]
	pushSteps(env, p, 1, 3) // 3 × 157 = 471 µUSD, under the limit
	eventually(t, "three calls", func() bool { return env.count(`SELECT step_count FROM runs WHERE id=$1`, run.ID) == 3 })
	time.Sleep(100 * time.Millisecond)
	if len(env.fc.callsFor("chat.abort")) != 0 {
		t.Fatal("aborted under the cost limit")
	}
	pushSteps(env, p, 4, 1) // 628 µUSD: the call that crosses the limit is recorded
	r := env.wait(done, run.ID)
	if r.Status != store.RunFailed || *r.FailureReason != "cost limit $0.0005 exceeded" || r.CostUSDMicros != 628 {
		t.Fatalf("run = %s %v cost=%d", r.Status, r.FailureReason, r.CostUSDMicros)
	}
}

// T5.5.2: chat.send RPC failure twice then success → one run, one node, the same
// idempotency key, backoff 1 s then 2 s, a span per failed attempt.
func TestChatSendRetryThenSuccess(t *testing.T) {
	env := newEnv(t)
	a := env.agent(nil)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if n <= 2 {
			return nil, &openclaw.RPCError{Code: "UNAVAILABLE", Message: "gateway busy"}
		}
		go finishThenFinal(t, env, p, `{"summary":"ok"}`)
		return openclaw.ChatSendResult{RunID: p.IdempotencyKey, Status: "started"}, nil
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x"})
	r := env.wait(done, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %v", r.Status, r.FailureReason)
	}
	sends := env.fc.sendParams()
	if len(sends) != 3 {
		t.Fatalf("chat.send calls = %d", len(sends))
	}
	nodeID := ""
	_ = env.pool.QueryRow(context.Background(), `SELECT id FROM run_nodes WHERE run_id=$1`, run.ID).Scan(&nodeID)
	for _, s := range sends {
		if s.IdempotencyKey != nodeID {
			t.Fatalf("idempotency key %s, want %s", s.IdempotencyKey, nodeID)
		}
	}
	if !slices.Equal(env.clock.Sleeps, []time.Duration{time.Second, 2 * time.Second}) {
		t.Fatalf("backoff = %v", env.clock.Sleeps)
	}
	if n := env.count(`SELECT count(*) FROM run_nodes WHERE run_id=$1`, run.ID); n != 1 {
		t.Fatalf("run nodes = %d", n)
	}
	if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND name LIKE 'chat.send attempt %'`, run.ID); n != 2 {
		t.Fatalf("attempt spans = %d", n)
	}
}

func TestChatSendGivesUpAfterFiveAttempts(t *testing.T) {
	env := newEnv(t)
	a := env.agent(nil)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		return nil, &openclaw.RPCError{Code: "UNAVAILABLE", Message: "gateway busy"}
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x"})
	r := env.wait(done, run.ID)
	if r.Status != store.RunFailed || !strings.HasPrefix(*r.FailureReason, "chat.send failed after 5 attempts") || len(env.fc.sendParams()) != 5 {
		t.Fatalf("run = %s %v sends=%d", r.Status, r.FailureReason, len(env.fc.sendParams()))
	}
	// Any of the five may have started the turn: it is aborted under the known run id,
	// and the session the sends created is deleted.
	key := env.fc.sendParams()[0]
	aborts := env.fc.callsFor("chat.abort")
	if len(aborts) != 1 || !strings.Contains(string(aborts[0]), `"runId":"`+key.IdempotencyKey+`"`) ||
		!strings.Contains(string(aborts[0]), key.SessionKey) {
		t.Fatalf("chat.abort = %s", aborts)
	}
	if dels := env.fc.callsFor("sessions.delete"); len(dels) != 1 || !strings.Contains(string(dels[0]), key.SessionKey) {
		t.Fatalf("sessions.delete = %s", dels)
	}
}

func TestNonRetryableModelError(t *testing.T) {
	env := newEnv(t)
	a := env.agent(nil)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go env.fc.push("chat", chatEvent(p.SessionKey, p.IdempotencyKey, "error", map[string]any{
			"errorKind": "invalid_request", "errorMessage": "messages: bad shape",
			"errorDetail": map[string]any{"provider": "scripted", "model": "scripted-1", "httpStatus": 400}}))
		return openclaw.ChatSendResult{RunID: p.IdempotencyKey, Status: "started"}, nil
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x"})
	r := env.wait(done, run.ID)
	if r.Status != store.RunFailed || *r.FailureReason != "model error: invalid_request" {
		t.Fatalf("run = %s %v", r.Status, r.FailureReason)
	}
	if len(env.fc.sendParams()) != 1 {
		t.Fatal("non-retryable error was retried")
	}
	if n := env.count(`SELECT count(*) FROM model_calls WHERE run_id=$1 AND error IS NOT NULL`, run.ID); n != 1 {
		t.Fatalf("failed model calls = %d", n)
	}
}

// T5.5.3: provider 529 twice then success → three turns ("Continue."), one node.
func TestRetryableModelErrorContinues(t *testing.T) {
	env := newEnv(t)
	a := env.agent(nil)
	var first openclaw.ChatSendParams
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if n == 1 {
			first = p
		}
		if n <= 2 {
			go env.fc.push("chat", chatEvent(p.SessionKey, p.IdempotencyKey, "error", map[string]any{
				"errorKind": "provider_error", "errorMessage": "overloaded", "errorDetail": map[string]any{"httpStatus": 529}}))
		} else {
			p.Message = first.Message // the token travels in the first turn's message only
			go finishThenFinal(t, env, p, `{"summary":"third time"}`)
		}
		return openclaw.ChatSendResult{RunID: p.IdempotencyKey, Status: "started"}, nil
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x"})
	r := env.wait(done, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %s", r.Status, deref(r.FailureReason))
	}
	sends := env.fc.sendParams()
	if len(sends) != 3 || sends[1].Message != "Continue." || sends[2].Message != "Continue." {
		t.Fatalf("turns = %+v", sends)
	}
	nodeID := sends[0].IdempotencyKey
	if sends[1].IdempotencyKey != nodeID+":2" || sends[2].IdempotencyKey != nodeID+":3" {
		t.Fatalf("keys = %s %s %s", sends[0].IdempotencyKey, sends[1].IdempotencyKey, sends[2].IdempotencyKey)
	}
	if n := env.count(`SELECT count(*) FROM run_nodes WHERE run_id=$1`, run.ID); n != 1 {
		t.Fatalf("run nodes = %d", n)
	}
	if n := env.count(`SELECT count(*) FROM model_calls WHERE run_id=$1 AND error IS NOT NULL`, run.ID); n != 2 {
		t.Fatalf("failed attempts recorded = %d", n)
	}
	if n := env.count(`SELECT max(attempt) FROM model_calls WHERE run_id=$1`, run.ID); n != 3 {
		t.Fatalf("max attempt = %d", n)
	}
}

func TestNoReasoningStored(t *testing.T) {
	env := newEnv(t)
	a := env.agent(nil)
	const secret = "SECRET-REASONING-42"
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			env.fc.push("agent", map[string]any{"runId": p.IdempotencyKey, "sessionKey": p.SessionKey, "stream": "reasoning",
				"data": map[string]any{"text": secret, "delta": secret}})
			env.fc.push("session.message", assistantMsg(p.SessionKey, p.IdempotencyKey, []any{
				map[string]any{"type": "thinking", "thinking": secret, "thinkingSignature": "sig"},
				textPart("Let me finish."),
			}, "toolUse", 50, 5, 0))
			finishThenFinal(t, env, p, `{"summary":"done"}`)
		}()
		return openclaw.ChatSendResult{RunID: p.IdempotencyKey, Status: "started"}, nil
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "think"})
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %v", r.Status, r.FailureReason)
	}
	for _, q := range []string{
		`SELECT count(*) FROM run_nodes WHERE messages::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM run_events WHERE data::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM spans WHERE attrs::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM tool_calls WHERE input::text LIKE '%' || $1 || '%' OR output::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM model_calls WHERE coalesce(error, '') LIKE '%' || $1 || '%'`,
	} {
		if n := env.count(q, secret); n != 0 {
			t.Errorf("reasoning text stored: %s", q)
		}
	}
	if env.count(`SELECT count(*) FROM run_nodes WHERE messages::text LIKE '%Let me finish.%'`) != 1 {
		t.Error("visible text of the reasoning message was dropped too")
	}
}

func TestTurnEndWithoutFinish(t *testing.T) {
	tests := []struct {
		name, reply, status, reason, output string
	}{
		{name: "json in the reply", reply: "Here you go: {\"summary\": \"from text\"}", status: store.RunSucceeded, output: `{"summary": "from text"}`},
		{name: "plain reply", reply: "I am done.", status: store.RunFailed, reason: "agent did not call finish"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			a := env.agent(nil)
			env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
				go func() {
					env.fc.push("session.message", assistantMsg(p.SessionKey, p.IdempotencyKey, []any{textPart(tc.reply)}, "stop", 50, 5, 0))
					env.fc.push("chat", chatEvent(p.SessionKey, p.IdempotencyKey, "final", map[string]any{"stopReason": "stop",
						"message": map[string]any{"role": "assistant", "content": []any{textPart(tc.reply)}}}))
				}()
				return openclaw.ChatSendResult{RunID: p.IdempotencyKey, Status: "started"}, nil
			}
			run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x"})
			r := env.wait(done, run.ID)
			if r.Status != tc.status || (tc.reason != "" && *r.FailureReason != tc.reason) {
				t.Fatalf("run = %s %v", r.Status, r.FailureReason)
			}
			if tc.output != "" {
				tr, _ := store.GetTrace(context.Background(), env.pool, run.ID)
				if string(tr.Nodes[0].Output) != tc.output {
					t.Fatalf("output = %s", tr.Nodes[0].Output)
				}
			}
		})
	}
}

func TestFinishGraceAbort(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			if err := env.e.AcceptFinish(ctx, tokenIn(p.Message), json.RawMessage(`{"summary":"early"}`)); err != nil {
				t.Errorf("AcceptFinish: %v", err)
			}
		}()
		return openclaw.ChatSendResult{RunID: p.IdempotencyKey, Status: "started"}, nil
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x"})
	eventually(t, "finish accepted", func() bool {
		return env.count(`SELECT count(*) FROM run_nodes WHERE run_id=$1 AND output <> 'null'::jsonb`, run.ID) == 1
	})
	env.clock.Advance(20 * time.Second)
	time.Sleep(100 * time.Millisecond)
	if len(env.fc.callsFor("chat.abort")) != 0 {
		t.Fatal("aborted within the 20 s grace")
	}
	env.clock.Advance(time.Second)
	r := env.wait(done, run.ID)
	if r.Status != store.RunSucceeded || len(env.fc.callsFor("chat.abort")) != 1 {
		t.Fatalf("run = %s %v", r.Status, r.FailureReason)
	}
}

func TestGatewayOutageReattach(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			token := tokenIn(p.Message)
			env.fc.push("session.message", assistantMsg(p.SessionKey, p.IdempotencyKey,
				[]any{toolCallPart("call_fin", "yuno__finish", map[string]any{"run_token": token})}, "toolUse", 100, 10, 0))
			if err := env.e.AcceptFinish(ctx, token, json.RawMessage(`{"summary":"before outage"}`)); err != nil {
				t.Errorf("AcceptFinish: %v", err)
			}
			env.fc.setConnected(false)
			env.fc.push(openclaw.EventDisconnected, map[string]any{})
			// The turn ends while we are away: only chat.history will tell.
			final := assistantMsg(p.SessionKey, p.IdempotencyKey, []any{textPart("Finished.")}, "stop", 120, 4, 0)
			env.fc.mu.Lock()
			env.fc.history[p.SessionKey] = append(env.fc.history[p.SessionKey], mustJSON(final["message"]))
			env.fc.mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			env.fc.setConnected(true)
			env.fc.push(openclaw.EventReconnected, map[string]any{})
		}()
		return openclaw.ChatSendResult{RunID: p.IdempotencyKey, Status: "started"}, nil
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x"})
	r := env.wait(done, run.ID)
	if r.Status != store.RunSucceeded || r.StepCount != 2 {
		t.Fatalf("run = %s %v steps=%d", r.Status, r.FailureReason, r.StepCount)
	}
	if n := len(env.fc.sendParams()); n != 1 {
		t.Fatalf("chat.send calls = %d (duplicate turn)", n)
	}
	if n := len(env.fc.callsFor("sessions.messages.subscribe")); n != 2 {
		t.Fatalf("subscribe calls = %d", n)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// ---- wave 1 review fixes ---------------------------------------------------------------

func startedTurn(p openclaw.ChatSendParams) (any, error) {
	return openclaw.ChatSendResult{RunID: p.IdempotencyKey, Status: "started"}, nil
}

// Two runs of one agent started within the same minute share the timestamp prefix of
// their UUIDv7 ids; their sessions, transcripts and counters must stay apart.
func TestConcurrentRunsOfOneAgentUseSeparateSessions(t *testing.T) {
	env := newEnv(t)
	a := env.agent(nil)
	both := make(chan struct{})
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if n == 2 {
			close(both)
		}
		go func() {
			<-both // both visits listen before either turn produces events
			finishThenFinal(t, env, p, `{"summary":"`+p.IdempotencyKey+`"}`)
		}()
		return startedTurn(p)
	}
	runA, doneA := env.start(TriggerRequest{AgentID: a.ID, Task: "a"})
	runB, doneB := env.start(TriggerRequest{AgentID: a.ID, Task: "b"})
	ra, rb := env.wait(doneA, runA.ID), env.wait(doneB, runB.ID)
	if store.ShortID(runA.ID) != store.ShortID(runB.ID) {
		t.Logf("run ids no longer share their first 8 characters (%s, %s)", runA.ID, runB.ID)
	}
	sends := env.fc.sendParams()
	if len(sends) != 2 || sends[0].SessionKey == sends[1].SessionKey {
		t.Fatalf("session keys = %+v", sends)
	}
	for _, r := range []*store.Run{ra, rb} {
		if r.Status != store.RunSucceeded || r.StepCount != 2 || r.InputTokens != 220 {
			t.Fatalf("run %s = %s steps=%d input=%d (charged for the other run's session?)", r.ID, r.Status, r.StepCount, r.InputTokens)
		}
		tr, _ := store.GetTrace(context.Background(), env.pool, r.ID)
		var msgs []map[string]any
		_ = json.Unmarshal(tr.Nodes[0].Messages, &msgs)
		if len(msgs) != 2 || !strings.Contains(*tr.Nodes[0].SessionKey, r.ID) {
			t.Fatalf("run %s: %d messages in session %s", r.ID, len(msgs), *tr.Nodes[0].SessionKey)
		}
	}
}

// Credentials in transcript text, tool inputs, tool outputs, error spans and log lines
// are redacted before anything is stored or published.
func TestCredentialsAreRedactedBeforeStorage(t *testing.T) {
	env := newEnv(t)
	a := env.agent(nil)
	const apiKey = "sk-ant-api03-SECRETSECRETSECRETSECRET42"
	const botToken = "1234567890:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw_X"
	leak := apiKey + " and " + botToken
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if n == 1 {
			return nil, &openclaw.RPCError{Code: "UNAVAILABLE", Message: "upstream said " + leak}
		}
		go func() {
			k, r := p.SessionKey, p.IdempotencyKey
			env.fc.push("session.message", assistantMsg(k, r, []any{textPart("Found " + leak),
				toolCallPart("call_1", "exec", map[string]any{"command": "echo " + leak})}, "toolUse", 10, 5, 0))
			env.fc.push("session.approval", map[string]any{"sessionKey": k, "phase": "pending", "approval": map[string]any{
				"id": "ap-1", "status": "pending", "presentation": map[string]any{"kind": "exec", "commandText": "echo " + leak}}})
			env.fc.push("session.approval", map[string]any{"sessionKey": k, "phase": "terminal", "approval": map[string]any{
				"id": "ap-1", "status": "allowed", "decision": "allow-once"}})
			env.fc.push("agent", itemEvent(k, r, "call_1", "exec", "start", "running", ""))
			env.fc.push("agent", map[string]any{"runId": r, "sessionKey": k, "stream": "command_output",
				"data": map[string]any{"toolCallId": "call_1", "name": "exec", "phase": "end", "output": "printed " + leak}})
			env.fc.push("agent", itemEvent(k, r, "call_1", "exec", "end", "failed", "exit 1: "+leak))
			finishThenFinal(t, env, p, `{"summary":"done"}`)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "look"})
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %v", r.Status, r.FailureReason)
	}
	for _, secret := range []string{"SECRETSECRETSECRET", "AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw_X"} {
		for _, q := range []string{
			`SELECT count(*) FROM run_nodes WHERE messages::text LIKE '%' || $1 || '%'`,
			`SELECT count(*) FROM run_events WHERE data::text LIKE '%' || $1 || '%'`,
			`SELECT count(*) FROM spans WHERE attrs::text LIKE '%' || $1 || '%'`,
			`SELECT count(*) FROM tool_calls WHERE input::text LIKE '%' || $1 || '%' OR output::text LIKE '%' || $1 || '%'`,
			`SELECT count(*) FROM approvals WHERE message LIKE '%' || $1 || '%'`,
			`SELECT count(*) FROM runs WHERE coalesce(failure_reason, '') LIKE '%' || $1 || '%'`,
		} {
			if n := env.count(q, secret); n != 0 {
				t.Errorf("%s stored: %s", secret[:8], q)
			}
		}
	}
	for what, q := range map[string]string{
		"transcript":  `SELECT count(*) FROM run_nodes WHERE messages::text LIKE '%Found [redacted] and [redacted]%'`,
		"tool input":  `SELECT count(*) FROM tool_calls WHERE input::text LIKE '%echo [redacted]%'`,
		"tool output": `SELECT count(*) FROM tool_calls WHERE output::text LIKE '%printed [redacted]%'`,
		"error span":  `SELECT count(*) FROM spans WHERE attrs::text LIKE '%upstream said [redacted]%'`,
		"log line":    `SELECT count(*) FROM run_events WHERE event='log' AND data->>'message' LIKE '%upstream said [redacted]%'`,
	} {
		if env.count(q) == 0 {
			t.Errorf("%s: redacted text not stored", what)
		}
	}
}

// Setup RPCs answer to cancellation and the time limit, and a session created before
// the first turn is deleted on every exit.
func TestSetupRPCsAreBoundedAndCleanedUp(t *testing.T) {
	ctx := context.Background()
	t.Run("cancel interrupts a sessions.patch that never answers", func(t *testing.T) {
		env := newEnv(t)
		a := env.agent(nil)
		env.fc.block["sessions.patch"] = true
		run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x", Model: "scripted/scripted-1"})
		eventually(t, "sessions.patch sent", func() bool { return len(env.fc.callsFor("sessions.patch")) == 1 })
		if _, err := env.e.Cancel(ctx, run.ID, "tester"); err != nil {
			t.Fatal(err)
		}
		r := env.wait(done, run.ID)
		if r.Status != store.RunCancelled || *r.FailureReason != "cancelled by tester" || len(env.fc.sendParams()) != 0 {
			t.Fatalf("run = %s %v sends=%d", r.Status, r.FailureReason, len(env.fc.sendParams()))
		}
		if dels := env.fc.callsFor("sessions.delete"); len(dels) != 1 {
			t.Fatalf("session created by sessions.patch not deleted: %s", dels)
		}
	})
	t.Run("the time limit interrupts a subscribe that never answers", func(t *testing.T) {
		env := newEnv(t)
		a := env.agent(func(a *store.Agent) { a.Limits.MaxDurationSeconds = 1 })
		env.fc.block["sessions.messages.subscribe"] = true
		run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x"})
		eventually(t, "subscribe sent", func() bool { return len(env.fc.callsFor("sessions.messages.subscribe")) == 1 })
		env.clock.Advance(time.Second)
		r := env.wait(done, run.ID)
		if r.Status != store.RunTimedOut || *r.FailureReason != "time limit 1s exceeded" {
			t.Fatalf("run = %s %v", r.Status, r.FailureReason)
		}
	})
	t.Run("a session made by sessions.patch is deleted when subscribe fails", func(t *testing.T) {
		env := newEnv(t)
		a := env.agent(nil)
		env.fc.fail["sessions.messages.subscribe"] = &openclaw.RPCError{Code: "INVALID_REQUEST", Message: "no such session"}
		run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x", Model: "scripted/scripted-1"})
		r := env.wait(done, run.ID)
		if r.Status != store.RunFailed || !strings.HasPrefix(*r.FailureReason, "session subscribe failed") {
			t.Fatalf("run = %s %v", r.Status, r.FailureReason)
		}
		patched := env.fc.callsFor("sessions.patch")
		dels := env.fc.callsFor("sessions.delete")
		if len(patched) != 1 || len(dels) != 1 {
			t.Fatalf("sessions.patch = %s, sessions.delete = %s", patched, dels)
		}
	})
}

// A chat.send whose answer is lost may still have started the turn: when the visit
// ends before a send succeeded, that turn is aborted under its known run id.
func TestCancelWhileChatSendAnswerIsLost(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	var runID string
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		// The gateway starts the turn, then the answer is lost while the user cancels.
		if _, err := env.e.Cancel(ctx, runID, "tester"); err != nil {
			t.Errorf("cancel: %v", err)
		}
		return nil, fmt.Errorf("fake: chat.send: %w", context.DeadlineExceeded)
	}
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	runID = run.ID
	claimed, err := store.ClaimQueuedRun(ctx, env.pool)
	if err != nil {
		t.Fatal(err)
	}
	env.e.runRun(ctx, claimed)
	r, _ := store.GetRun(ctx, env.pool, run.ID)
	if r.Status != store.RunCancelled || len(env.fc.sendParams()) != 1 {
		t.Fatalf("run = %s %v sends=%d", r.Status, r.FailureReason, len(env.fc.sendParams()))
	}
	sent := env.fc.sendParams()[0]
	aborts := env.fc.callsFor("chat.abort")
	if len(aborts) != 1 || !strings.Contains(string(aborts[0]), `"runId":"`+sent.IdempotencyKey+`"`) {
		t.Fatalf("possibly started turn not aborted: %s", aborts)
	}
	if dels := env.fc.callsFor("sessions.delete"); len(dels) != 1 || !strings.Contains(string(dels[0]), sent.SessionKey) {
		t.Fatalf("sessions.delete = %s", dels)
	}
	tr, _ := store.GetTrace(ctx, env.pool, run.ID)
	if tr.Nodes[0].OpenClawRunID == nil || *tr.Nodes[0].OpenClawRunID != sent.IdempotencyKey {
		t.Fatalf("turn id not recorded before the send: %v", tr.Nodes[0].OpenClawRunID)
	}
}

// A worker shutting down aborts its turn; the replacement visit after recovery aborts
// the lost visit's recorded turn and deletes its session before starting its own.
func TestShutdownAndRecoveryStopTheLostTurn(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimQueuedRun(ctx, env.pool)
	if err != nil {
		t.Fatal(err)
	}
	wctx, shutdown := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.e.runRun(wctx, claimed)
	}()
	eventually(t, "turn started", func() bool { return len(env.fc.sendParams()) == 1 })
	first := env.fc.sendParams()[0]
	shutdown()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not stop")
	}
	aborts := env.fc.callsFor("chat.abort")
	if len(aborts) != 1 || !strings.Contains(string(aborts[0]), `"runId":"`+first.IdempotencyKey+`"`) {
		t.Fatalf("shutdown left the turn running: chat.abort = %s", aborts)
	}
	if r, _ := store.GetRun(ctx, env.pool, run.ID); r.Status != store.RunRunning || len(env.fc.callsFor("sessions.delete")) != 0 {
		t.Fatalf("after shutdown: status %s, deletes %d", r.Status, len(env.fc.callsFor("sessions.delete")))
	}

	if n, _, err := store.RequeueStaleRuns(ctx, env.pool, -time.Hour); err != nil || n != 1 {
		t.Fatalf("requeue: %d %v", n, err)
	}
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go finishThenFinal(t, env, p, `{"summary":"second visit"}`)
		return startedTurn(p)
	}
	again, err := store.ClaimQueuedRun(ctx, env.pool)
	if err != nil || again.Attempt != 2 {
		t.Fatalf("claim: %+v %v", again, err)
	}
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		env.e.runRun(ctx, again)
	}()
	if r := env.wait(done2, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("recovered run = %s %v", r.Status, r.FailureReason)
	}
	calls := env.fc.methods()
	abortAt, deleteAt, sendAt := -1, -1, -1
	seenSends := 0
	for i, m := range calls {
		switch m {
		case "chat.abort":
			abortAt = i
		case "sessions.delete":
			if deleteAt < 0 {
				deleteAt = i
			}
		case "chat.send":
			if seenSends++; seenSends == 2 {
				sendAt = i
			}
		}
	}
	if !(abortAt >= 0 && deleteAt > abortAt && sendAt > deleteAt) {
		t.Fatalf("recovery order (abort %d, delete %d, second send %d): %v", abortAt, deleteAt, sendAt, calls)
	}
	aborts = env.fc.callsFor("chat.abort")
	dels := env.fc.callsFor("sessions.delete")
	if len(aborts) != 2 || !strings.Contains(string(aborts[1]), first.IdempotencyKey) || !strings.Contains(string(dels[0]), first.SessionKey) {
		t.Fatalf("recovery did not stop the lost turn: aborts %s, deletes %s", aborts, dels)
	}
	if second := env.fc.sendParams()[1]; second.SessionKey == first.SessionKey {
		t.Fatal("replacement visit reused the lost visit's session")
	}
}

// Tool calls that ran while events were missing are rebuilt from chat.history, with
// their rows, terminal status, count and limit.
func TestToolCallsRebuiltFromHistory(t *testing.T) {
	tests := []struct {
		name     string
		limits   store.Limits
		status   string
		reason   string
		toolRows int
	}{
		{name: "counted and completed", status: store.RunSucceeded, toolRows: 2},
		{name: "counted against max_tool_calls", limits: store.Limits{MaxToolCalls: 2}, status: store.RunFailed,
			reason: "tool call limit 2 exceeded", toolRows: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			a := env.agent(nil)
			env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
				go func() {
					k, r := p.SessionKey, p.IdempotencyKey
					env.fc.push("session.message", assistantMsg(k, r, []any{
						toolCallPart("call_seen", "exec", map[string]any{"command": "ls"}),
						toolCallPart("call_lost", "read", map[string]any{"path": "a.txt"}),
					}, "toolUse", 100, 10, 0))
					env.fc.push("agent", itemEvent(k, r, "call_seen", "exec", "start", "running", "")) // its end never arrives
					env.fc.setConnected(false)
					env.fc.push(openclaw.EventDisconnected, map[string]any{})
					// Both tools ran and the turn ended while the client was away.
					env.fc.addHistory(k,
						toolResultEntry(r, "call_seen", "exec", "listing", false),
						toolResultEntry(r, "call_lost", "read", "contents", false),
						assistantMsg(k, r, []any{textPart(`Done: {"summary":"rebuilt"}`)}, "stop", 120, 5, 0)["message"])
					time.Sleep(50 * time.Millisecond)
					env.fc.setConnected(true)
					env.fc.push(openclaw.EventReconnected, map[string]any{})
				}()
				return startedTurn(p)
			}
			run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x", Limits: tc.limits})
			r := env.wait(done, run.ID)
			if r.Status != tc.status || deref(r.FailureReason) != tc.reason || r.ToolCallCount != 2 {
				t.Fatalf("run = %s %q tool calls %d", r.Status, deref(r.FailureReason), r.ToolCallCount)
			}
			tr, _ := store.GetTrace(context.Background(), env.pool, run.ID)
			if len(tr.ToolCalls) != tc.toolRows {
				t.Fatalf("tool rows = %d", len(tr.ToolCalls))
			}
			want := map[string]string{"call_seen": `"listing"`, "call_lost": `"contents"`}
			for _, c := range tr.ToolCalls {
				if c.Status != store.ToolExecuted || c.FinishedAt == nil || string(c.Output) != want[c.ToolUseID] {
					t.Errorf("tool %s = %s finished=%v output=%s", c.ToolUseID, c.Status, c.FinishedAt, c.Output)
				}
			}
			if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND kind='tool_call' AND ended_at IS NOT NULL`, run.ID); n != 2 {
				t.Fatalf("ended tool spans = %d", n)
			}
		})
	}
}

// A full event buffer signals a gap; the visit reconciles at once and enforces the
// limits the lost usage events would have triggered.
func TestDroppedEventsAreReconciledAtOnce(t *testing.T) {
	env := newEnv(t)
	a := env.agent(nil)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			k, r := p.SessionKey, p.IdempotencyKey
			env.fc.addHistory(k,
				assistantMsg(k, r, []any{toolCallPart("call_1", "ls", map[string]any{"path": "."})}, "toolUse", 122, 7, 0)["message"],
				assistantMsg(k, r, []any{toolCallPart("call_2", "ls", map[string]any{"path": "."})}, "toolUse", 122, 7, 0)["message"])
			env.fc.push(openclaw.EventGap, map[string]any{})
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x", Limits: store.Limits{MaxSteps: 2}})
	r := env.wait(done, run.ID) // well before the 30 s quiet catch-up
	if r.Status != store.RunFailed || *r.FailureReason != "step limit 2 exceeded" || r.StepCount != 2 {
		t.Fatalf("run = %s %v steps=%d", r.Status, r.FailureReason, r.StepCount)
	}
}

// A result taken from the final reply only wins once the turn's usage is reconciled
// and the limits still hold.
func TestFinalReplyResultWaitsForMissedUsage(t *testing.T) {
	env := newEnv(t)
	a := env.agent(nil)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			k, r := p.SessionKey, p.IdempotencyKey
			env.fc.push("session.message", assistantMsg(k, r, []any{toolCallPart("call_1", "ls", map[string]any{"path": "."})}, "toolUse", 122, 7, 0))
			// The second step's usage event is lost; the turn's final event arrives.
			final := assistantMsg(k, r, []any{textPart(`{"summary":"over the limit"}`)}, "stop", 122, 7, 0)["message"]
			env.fc.addHistory(k, final)
			env.fc.push("chat", chatEvent(k, r, "final", map[string]any{"stopReason": "stop", "message": final}))
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x", Limits: store.Limits{MaxSteps: 2}})
	r := env.wait(done, run.ID)
	if r.Status != store.RunFailed || *r.FailureReason != "step limit 2 exceeded" || r.StepCount != 2 {
		t.Fatalf("run = %s %v steps=%d", r.Status, r.FailureReason, r.StepCount)
	}
}

// A cancellation that takes effect when the finish grace runs out, or during the abort
// that follows it, wins over the accepted result.
func TestCancellationWinsOverFinishGrace(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, abortEmits bool) (*testEnv, *store.Run, <-chan struct{}) {
		env := newEnv(t)
		env.fc.abortEmits = abortEmits
		a := env.agent(nil)
		env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
			go func() {
				if err := env.e.AcceptFinish(ctx, tokenIn(p.Message), json.RawMessage(`{"summary":"early"}`)); err != nil {
					t.Errorf("AcceptFinish: %v", err)
				}
			}()
			return startedTurn(p)
		}
		run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x"})
		eventually(t, "finish accepted", func() bool {
			return env.count(`SELECT count(*) FROM run_nodes WHERE run_id=$1 AND output <> 'null'::jsonb`, run.ID) == 1
		})
		return env, run, done
	}
	t.Run("same tick", func(t *testing.T) {
		env, run, done := setup(t, true)
		if err := store.RequestCancel(ctx, env.pool, run.ID, "tester"); err != nil { // the flag only: the tick finds it
			t.Fatal(err)
		}
		env.clock.Advance(21 * time.Second)
		if r := env.wait(done, run.ID); r.Status != store.RunCancelled || *r.FailureReason != "cancelled by tester" {
			t.Fatalf("run = %s %v", r.Status, r.FailureReason)
		}
	})
	t.Run("during the abort wait", func(t *testing.T) {
		env, run, done := setup(t, false)
		env.clock.Advance(21 * time.Second)
		eventually(t, "grace abort", func() bool { return len(env.fc.callsFor("chat.abort")) == 1 })
		if err := store.RequestCancel(ctx, env.pool, run.ID, "tester"); err != nil {
			t.Fatal(err)
		}
		env.clock.Advance(10 * time.Second)
		if r := env.wait(done, run.ID); r.Status != store.RunCancelled || *r.FailureReason != "cancelled by tester" {
			t.Fatalf("run = %s %v", r.Status, r.FailureReason)
		}
	})
}

// Concurrent finish submissions store one result, a repeat does not restart the grace,
// and a submission after the visit closed is refused without touching the output.
func TestFinishFirstResultWins(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	var mu sync.Mutex
	token := ""
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		mu.Lock()
		token = tokenIn(p.Message)
		mu.Unlock()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x"})
	eventually(t, "turn started", func() bool { mu.Lock(); defer mu.Unlock(); return token != "" })
	env.e.mu.Lock()
	v := env.e.active[run.ID]
	env.e.mu.Unlock()

	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = env.e.AcceptFinish(ctx, token, json.RawMessage(fmt.Sprintf(`{"summary":"r%d"}`, i)))
		}()
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		var fe *FinishValidationError
		switch {
		case err == nil && winner < 0:
			winner = i
		case err == nil:
			t.Fatalf("two results accepted (%d and %d)", winner, i)
		case !errors.As(err, &fe) || !strings.Contains(fe.Message, "already accepted"):
			t.Fatalf("submission %d: %v", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no result accepted")
	}
	want := fmt.Sprintf(`{"summary":"r%d"}`, winner)
	env.clock.Advance(15 * time.Second)
	if err := env.e.AcceptFinish(ctx, token, json.RawMessage(want)); err != nil {
		t.Fatalf("repeat of the accepted result: %v", err)
	}
	env.clock.Advance(6 * time.Second) // 21 s after the first acceptance: the grace is over
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %v", r.Status, r.FailureReason)
	}
	// A submission that resolved the token just before the visit closed lands now.
	if err := v.acceptFinish(ctx, json.RawMessage(`{"summary":"late"}`)); !errors.Is(err, ErrInvalidRunToken) {
		t.Fatalf("late submission: %v", err)
	}
	tr, _ := store.GetTrace(ctx, env.pool, run.ID)
	var got, exp map[string]any
	_ = json.Unmarshal(tr.Nodes[0].Output, &got)
	_ = json.Unmarshal([]byte(want), &exp)
	if got["summary"] != exp["summary"] {
		t.Fatalf("node output = %s, want %s", tr.Nodes[0].Output, want)
	}
}

// Three refused finish submissions end the visit, MCP input schema refusals included.
func TestFinishFailsAfterThreeRejections(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	const schemaMsg = "invalid input: at '/result': got string, want object"
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			token := tokenIn(p.Message)
			for range 2 {
				var fe *FinishValidationError
				if err := env.e.AcceptFinish(ctx, token, json.RawMessage(`{"summary":42}`)); !errors.As(err, &fe) {
					t.Errorf("invalid finish: %v", err)
				}
			}
			env.e.RejectFinish(ctx, token, schemaMsg) // refused by the MCP input schema
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x"})
	r := env.wait(done, run.ID)
	if r.Status != store.RunFailed || *r.FailureReason != "finish result rejected 3 times: "+schemaMsg {
		t.Fatalf("run = %s %v", r.Status, r.FailureReason)
	}
	if len(env.fc.callsFor("chat.abort")) != 1 {
		t.Fatal("turn not aborted")
	}
}

// The run token stays valid through an approval pause longer than the run's time limit.
func TestRunTokenOutlivesApprovalPause(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(func(a *store.Agent) { a.Limits.MaxDurationSeconds = 60 })
	var mu sync.Mutex
	var sent openclaw.ChatSendParams
	approval := func(phase, status string) map[string]any {
		return map[string]any{"sessionKey": sent.SessionKey, "phase": phase, "approval": map[string]any{
			"id": "ap-1", "status": status, "decision": "allow-once", "presentation": map[string]any{"kind": "exec", "commandText": "make"}}}
	}
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		mu.Lock()
		sent = p
		mu.Unlock()
		go env.fc.push("session.approval", approval("pending", "pending"))
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x"})
	eventually(t, "waiting for approval", func() bool {
		r, _ := store.GetRun(ctx, env.pool, run.ID)
		return r.Status == store.RunWaitingApproval
	})
	env.clock.Advance(5 * time.Minute) // a person takes their time
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	p := sent
	mu.Unlock()
	if _, ok := env.e.ResolveRunToken(tokenIn(p.Message)); !ok {
		t.Fatal("run token expired during the approval pause")
	}
	env.fc.push("session.approval", approval("terminal", "allowed"))
	go finishThenFinal(t, env, p, `{"summary":"after approval"}`)
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %v", r.Status, r.FailureReason)
	}
}

// T5.5.3, the bound: a retryable provider error is retried as a new turn three times,
// then the node fails with the error kind.
func TestRetryableModelErrorGivesUp(t *testing.T) {
	env := newEnv(t)
	a := env.agent(nil)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go env.fc.push("chat", chatEvent(p.SessionKey, p.IdempotencyKey, "error", map[string]any{
			"errorKind": "rate_limited", "errorMessage": "slow down", "errorDetail": map[string]any{"httpStatus": 429}}))
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x"})
	r := env.wait(done, run.ID)
	if r.Status != store.RunFailed || deref(r.FailureReason) != "model error: rate_limited" || len(env.fc.sendParams()) != 4 {
		t.Fatalf("run = %s %q sends=%d", r.Status, deref(r.FailureReason), len(env.fc.sendParams()))
	}
	if !slices.Equal(env.clock.Sleeps, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}) {
		t.Fatalf("backoff = %v", env.clock.Sleeps)
	}
	if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND kind='model_call' AND attrs->>'status'='error'`, run.ID); n != 4 {
		t.Fatalf("failed attempt spans = %d", n)
	}
	if n := env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='log' AND data->>'level'='warn' AND data->>'message' LIKE 'model error (rate_limited)%'`, run.ID); n != 3 {
		t.Fatalf("retry warnings = %d", n)
	}
}
