package httpapi

import (
	"context"
	"testing"
	"time"

	"yuno/internal/store"
)

// setGuardrails stores guardrails (and optionally enables memory) on the fixture's agent.
func (g *gateFixture) setGuardrails(t *testing.T, gr store.Guardrails, memoryOn bool) {
	t.Helper()
	ctx := context.Background()
	a, err := store.GetAgent(ctx, testPool, g.agent.ID)
	mustDo(t, err)
	a.Guardrails = gr
	a.MemoryPolicy.Enabled = memoryOn
	mustDo(t, store.UpdateAgent(ctx, testPool, a))
}

// T5.11.3 at the gate: a tool the agent's guardrails block is refused even when the
// allowlist and interaction rules permit it, recorded as blocked, and never reaches
// the approval or the effect.
func TestMCPGuardrailBlockedTool(t *testing.T) {
	g := newGateFixture(t, []string{"deploy_release", "memory_save"}, map[string]string{"deploy_release": "auto"}, 5*time.Second)
	g.setGuardrails(t, store.Guardrails{BlockedTools: []string{"yuno__deploy_release"}}, true)
	text, isErr := g.call(t, "deploy_release", g.deployArgs())
	if !isErr || text != "tool deploy_release is blocked for this agent" {
		t.Fatalf("result %v %q", isErr, text)
	}
	if g.posts.Load() != 0 || len(g.fe.approvalReqs) != 0 {
		t.Fatal("a guardrail-blocked tool ran or asked for approval")
	}
	rows := g.rows(t)
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	checkRow(t, rows[0], "deploy_release", store.ToolBlocked)
	// Other tools are unaffected.
	if text, isErr := g.call(t, "memory_save", map[string]any{"run_token": g.token, "key": "k", "value": "v"}); isErr {
		t.Fatalf("memory_save: %s", text)
	}
}

// T5.11.4 at the gate: a pattern tested on the canonical JSON input refuses the call
// with "input blocked by guardrail <name>", status blocked, before any approval;
// inputs it does not match, other tools and the run token are unaffected.
func TestMCPGuardrailInputPattern(t *testing.T) {
	g := newGateFixture(t, []string{"deploy_release", "memory_save"}, nil, 5*time.Second)
	g.setGuardrails(t, store.Guardrails{BlockedInputPatterns: []store.BlockedPattern{
		{Name: "no-prod-paths", Tool: "deploy_release", Regex: `"paths":\[[^\]]*"prod/`},
		{Name: "no-secrets", Tool: "memory_save", Regex: `(?i)password`},
		{Name: "token", Tool: "deploy_release", Regex: `r_gate`},
	}}, true)
	g.fe.waitOutcome = "approve"

	args := g.deployArgs()
	args["paths"] = []string{"app.py", "prod/settings.py"}
	text, isErr := g.call(t, "deploy_release", args)
	if !isErr || text != "input blocked by guardrail no-prod-paths" {
		t.Fatalf("blocked deploy: %v %q", isErr, text)
	}
	if g.posts.Load() != 0 || len(g.fe.approvalReqs) != 0 {
		t.Fatal("a blocked input reached the approval or the effect")
	}
	rows := g.rows(t)
	checkRow(t, rows[0], "deploy_release", store.ToolBlocked)
	if rows[0].IdempotencyKey != nil {
		t.Fatal("a blocked call got an idempotency key")
	}

	// The token pattern would match the run token if it were part of the input.
	if text, isErr := g.call(t, "deploy_release", g.deployArgs()); isErr {
		t.Fatalf("allowed deploy: %s", text)
	}
	if g.posts.Load() != 1 {
		t.Fatalf("allowed deploy posts = %d", g.posts.Load())
	}

	text, isErr = g.call(t, "memory_save", map[string]any{"run_token": g.token, "key": "db", "value": "PassWord is hunter2"})
	if !isErr || text != "input blocked by guardrail no-secrets" {
		t.Fatalf("blocked memory_save: %v %q", isErr, text)
	}
	run, _ := store.GetRun(context.Background(), testPool, g.run.ID)
	if run.ToolCallCount != 3 {
		t.Fatalf("tool_call_count = %d, want every call counted", run.ToolCallCount)
	}
}
