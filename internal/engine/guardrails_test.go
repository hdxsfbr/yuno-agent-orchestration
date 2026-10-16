package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"yuno/internal/guardrails"
	"yuno/internal/openclaw"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// T5.11.1: the run's max_cost_usd is the smallest of the workflow's limit and every
// participating agent's max_cost_usd_per_run, for workflow and quick runs.
func TestTriggerAppliesAgentCostCaps(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	cheap := env.agent(func(a *store.Agent) { a.Name = "Cheap"; a.Guardrails.MaxCostUSDPerRun = 3 })
	pricey := env.agent(func(a *store.Agent) { a.Name = "Pricey"; a.Guardrails.MaxCostUSDPerRun = 5 })
	free := env.agent(func(a *store.Agent) { a.Name = "Uncapped" })
	tests := []struct {
		name  string
		nodes []workflow.Node
		limit float64
		want  float64
	}{
		{name: "two capped agents: the smaller cap", nodes: []workflow.Node{{ID: "a", AgentID: pricey.ID, Task: "x"}, {ID: "b", AgentID: cheap.ID, Task: "y"}}, limit: 10, want: 3},
		{name: "a workflow limit below every cap stands", nodes: []workflow.Node{{ID: "a", AgentID: pricey.ID, Task: "x"}}, limit: 2, want: 2},
		{name: "an uncapped agent does not lower it", nodes: []workflow.Node{{ID: "a", AgentID: free.ID, Task: "x"}}, limit: 7, want: 7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			def := line(tc.nodes...)
			def.Limits.MaxCostUSD = tc.limit
			wf := env.createWorkflow(def)
			run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID})
			if err != nil {
				t.Fatal(err)
			}
			if run.Limits.MaxCostUSD != tc.want {
				t.Fatalf("max_cost_usd = %v, want %v", run.Limits.MaxCostUSD, tc.want)
			}
		})
	}
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: cheap.ID, Task: "x", Limits: store.Limits{MaxCostUSD: 9}})
	if err != nil || run.Limits.MaxCostUSD != 3 {
		t.Fatalf("quick run: %+v %v", run, err)
	}
}

// T5.11.2: with max_runs_per_hour n, the trigger at n-1 runs in the hour is accepted
// and the one at n is refused with the rate-limit error, whatever workflow names the
// agent; another agent is not affected.
func TestTriggerRateLimit(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	limited := env.agent(func(a *store.Agent) { a.Name = "Limited"; a.Guardrails.MaxRunsPerHour = 2 })
	other := env.agent(func(a *store.Agent) { a.Name = "Other" })
	if _, err := env.e.Trigger(ctx, TriggerRequest{AgentID: limited.ID, Task: "1"}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	wf := env.createWorkflow(line(workflow.Node{ID: "o", AgentID: other.ID, Task: "x"}, workflow.Node{ID: "l", AgentID: limited.ID, Task: "y"}))
	if _, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID}); err != nil {
		t.Fatalf("run at n-1: %v", err)
	}
	_, err := env.e.Trigger(ctx, TriggerRequest{AgentID: limited.ID, Task: "3"})
	var rl *guardrails.RateLimitError
	if !errors.As(err, &rl) || rl.Error() != "rate limit: 2 runs/hour for agent Limited" {
		t.Fatalf("run at n: %v", err)
	}
	if _, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID}); !errors.As(err, &rl) {
		t.Fatalf("a workflow naming the agent is refused too: %v", err)
	}
	if n := env.count(`SELECT count(*) FROM runs`); n != 2 {
		t.Fatalf("runs = %d, want 2 (a refused trigger stores nothing)", n)
	}
	if _, err := env.e.Trigger(ctx, TriggerRequest{AgentID: other.ID, Task: "x"}); err != nil {
		t.Fatalf("another agent: %v", err)
	}
}

// DESIGN 5.9 rule 2: a schedule occurrence fired again finds its run and queues
// nothing, even when the agent's rate limit is used up by that very run; another
// occurrence of the same schedule is a new run (and here meets the limit).
func TestTriggerDuplicateScheduleOccurrence(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	limited := env.agent(func(a *store.Agent) { a.Name = "Hourly"; a.Guardrails.MaxRunsPerHour = 1 })
	wf := env.createWorkflow(line(workflow.Node{ID: "l", AgentID: limited.ID, Task: "y"}))
	req := TriggerRequest{WorkflowID: wf.ID, Trigger: store.TriggerSchedule, TriggerRef: "sch1@2026-10-16T09:00:00Z"}
	if _, err := env.e.Trigger(ctx, req); err != nil {
		t.Fatalf("first fire: %v", err)
	}
	if _, err := env.e.Trigger(ctx, req); !errors.Is(err, ErrDuplicateTrigger) {
		t.Fatalf("the same occurrence again: %v, want ErrDuplicateTrigger", err)
	}
	next := req
	next.TriggerRef = "sch1@2026-10-16T10:00:00Z"
	var rl *guardrails.RateLimitError
	if _, err := env.e.Trigger(ctx, next); !errors.As(err, &rl) {
		t.Fatalf("the next occurrence: %v, want the rate limit", err)
	}
	if n := env.count(`SELECT count(*) FROM runs`); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}

// DESIGN 5.10 rule 5 and implementation note: before each turn of an agent with memory
// on, MEMORY.md holds its newest live facts; with memory off it stays the bare header.
func TestMemoryRenderedBeforeTurn(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	remembers := env.agent(func(a *store.Agent) { a.Name = "Remembers"; a.MemoryPolicy = store.MemoryPolicy{Enabled: true} })
	forgets := env.agent(func(a *store.Agent) { a.Name = "Forgets" })
	now := env.clock.Now()
	for _, f := range []struct {
		agent, key, value string
		expires           time.Time
	}{
		{remembers.ID, "deploy-target", "staging", now.Add(time.Hour)},
		{remembers.ID, "old-host", "gone", now.Add(-time.Minute)},
		{forgets.ID, "secret-plan", "unused", now.Add(time.Hour)},
	} {
		if _, err := env.pool.Exec(ctx, `INSERT INTO memory_facts (id, scope, agent_id, key, value, created_at, updated_at, expires_at)
			VALUES ($1, 'agent', $2, $3, $4, $5, $5, $6)`, store.NewID(), f.agent, f.key, f.value, now.Add(-time.Hour), f.expires); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[string]string{}
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		b, err := os.ReadFile(filepath.Join(env.dir, "workspace-"+sessionAgent(p.SessionKey), "MEMORY.md"))
		mu.Lock()
		seen[sessionAgent(p.SessionKey)] = string(b)
		if err != nil {
			seen[sessionAgent(p.SessionKey)] = "missing: " + err.Error()
		}
		mu.Unlock()
		go finishThenFinal(t, env, p, `{"summary":"done"}`)
		return startedTurn(p)
	}
	for _, a := range []*store.Agent{remembers, forgets} {
		run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "work"})
		if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
			t.Fatalf("%s: run %s %q", a.Name, r.Status, deref(r.FailureReason))
		}
	}
	got, _ := store.GetAgent(ctx, env.pool, remembers.ID)
	mem := seen[*got.OpenClawAgentID]
	if !strings.Contains(mem, "- deploy-target: staging") || strings.Contains(mem, "old-host") {
		t.Fatalf("MEMORY.md of the agent with memory:\n%s", mem)
	}
	got, _ = store.GetAgent(ctx, env.pool, forgets.ID)
	if mem := seen[*got.OpenClawAgentID]; mem != openclaw.MemoryMD {
		t.Fatalf("MEMORY.md of the agent without memory = %q", mem)
	}
}

// DESIGN 5.11 rule 4 for exec: a command one of the agent's exec patterns blocks is
// denied on the gateway as soon as the approval is raised, with the pattern's message.
func TestBlockedCommandDeniedAtOnce(t *testing.T) {
	env := newEnv(t)
	a := env.agent(func(a *store.Agent) {
		a.Guardrails.BlockedInputPatterns = []store.BlockedPattern{{Name: "no force delete", Tool: "exec", Regex: `rm\s+-rf`}}
	})
	var mu sync.Mutex
	var sent openclaw.ChatSendParams
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		mu.Lock()
		sent = p
		mu.Unlock()
		go env.fc.push("session.approval", map[string]any{"sessionKey": p.SessionKey, "phase": "pending", "approval": map[string]any{
			"id": "exec-rm", "status": "pending", "presentation": map[string]any{"kind": "exec", "commandText": "rm -rf /workspace"}}})
		return startedTurn(p)
	}
	env.fc.onExecResolve = func(id, decision string) {
		mu.Lock()
		p := sent
		mu.Unlock()
		env.fc.push("session.approval", map[string]any{"sessionKey": p.SessionKey, "phase": "terminal", "approval": map[string]any{
			"id": id, "status": "denied", "decision": decision}})
		finishThenFinal(t, env, p, `{"summary":"refused"}`)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "clean up"})
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	res := env.fc.callsFor("exec.approval.resolve")
	if len(res) != 1 || !strings.Contains(string(res[0]), `"decision":"deny"`) {
		t.Fatalf("exec.approval.resolve = %s", res)
	}
	got := pendingOrDecided(t, env, run.ID)
	if got.Status != store.ApprovalRejected || deref(got.DecidedBy) != "guardrail" || !strings.Contains(deref(got.Reason), "no force delete") {
		t.Fatalf("approval = %+v reason %q", got, deref(got.Reason))
	}
}
