package evals

import (
	"strings"
	"testing"
	"time"

	"yuno/internal/store"
)

func TestEvalIdempotency(t *testing.T) {
	key := "key-aaaaaaaaaaaaaaaa"
	tests := []struct {
		name   string
		mutate func(tr *store.Trace)
		pass   bool
		want   string
	}{
		{name: "one call, one side effect", pass: true, want: "1 side-effecting call(s) in 1 group(s): 1 effect(s) made, 0 replayed"},
		{name: "no side-effecting calls", pass: true, want: "no side-effecting tool calls", mutate: func(tr *store.Trace) {
			tr.ToolCalls, tr.Approvals, tr.SideEffects = tr.ToolCalls[:1], nil, nil
		}},
		{name: "the same call twice: one effect, the second replayed", pass: true, want: "2 side-effecting call(s) in 1 group(s): 1 effect(s) made, 1 replayed",
			mutate: func(tr *store.Trace) {
				tr.ToolCalls = append(tr.ToolCalls, replayedCall(tr.ToolCalls[1], "t4", 6*time.Second, 7*time.Second))
			}},
		{name: "three identical calls, two replays", pass: true, want: "1 effect(s) made, 2 replayed", mutate: func(tr *store.Trace) {
			tr.ToolCalls = append(tr.ToolCalls,
				replayedCall(tr.ToolCalls[1], "t4", 6*time.Second, 7*time.Second),
				replayedCall(tr.ToolCalls[1], "t5", 8*time.Second, 9*time.Second))
		}},
		{name: "the same input in another visit is a new effect", pass: true, want: "2 effect(s) made", mutate: func(tr *store.Trace) {
			second := deployCall("t4", "rn2", "key-bbbbbbbbbbbbbbbb", 7*time.Second, 8*time.Second)
			tr.ToolCalls = append(tr.ToolCalls, second)
			tr.SideEffects = append(tr.SideEffects, effectFor("key-bbbbbbbbbbbbbbbb", "t4"))
		}},
		{name: "another input in the same visit is a new effect", pass: true, want: "2 effect(s) made", mutate: func(tr *store.Trace) {
			second := deployCall("t4", "rn1", "key-bbbbbbbbbbbbbbbb", 7*time.Second, 8*time.Second)
			second.Input = raw(`{"run_token":"r_x","version":"2.0.0","paths":["a.py"]}`)
			tr.ToolCalls = append(tr.ToolCalls, second)
			tr.SideEffects = append(tr.SideEffects, effectFor("key-bbbbbbbbbbbbbbbb", "t4"))
		}},
		{name: "key order and spacing do not make an input different", pass: true, want: "1 effect(s) made, 1 replayed", mutate: func(tr *store.Trace) {
			again := replayedCall(tr.ToolCalls[1], "t4", 6*time.Second, 7*time.Second)
			again.Input = raw(`{ "paths": ["a.py"], "version": "1.0.0", "run_token": "r_x" }`)
			tr.ToolCalls = append(tr.ToolCalls, again)
		}},
		{name: "a refused retry carries no effect", pass: true, mutate: func(tr *store.Trace) {
			again := tr.ToolCalls[1]
			again.ID, again.Status, again.Output = "t4", store.ToolRejected, nil
			tr.ToolCalls = append(tr.ToolCalls, again)
		}},
		{name: "a call that never executed needs no effect", pass: true, mutate: func(tr *store.Trace) {
			tr.ToolCalls[1].Status = store.ToolAwaitingApproval
			tr.SideEffects = nil
		}},
		{name: "a blocked call without a key is fine", pass: true, mutate: func(tr *store.Trace) {
			c := deployCall("t4", "rn1", "", time.Second, 2*time.Second)
			c.Status, c.IdempotencyKey = store.ToolBlocked, nil
			c.Input = raw(`{"version":"3"}`)
			tr.ToolCalls = append(tr.ToolCalls, c)
		}},

		{name: "two identical calls that both made the effect", want: "2 executed calls with the same input each made the effect", mutate: func(tr *store.Trace) {
			second := tr.ToolCalls[1]
			second.ID, second.StartedAt, second.FinishedAt = "t4", ptr(t0.Add(6*time.Second)), ptr(t0.Add(7*time.Second))
			tr.ToolCalls = append(tr.ToolCalls, second)
		}},
		{name: "two identical calls with different keys", want: "calls with the same input carry different idempotency keys", mutate: func(tr *store.Trace) {
			second := replayedCall(tr.ToolCalls[1], "t4", 6*time.Second, 7*time.Second)
			second.IdempotencyKey = ptr("key-cccccccccccccccc")
			tr.ToolCalls = append(tr.ToolCalls, second)
		}},
		{name: "two side effect rows for one key", want: "has 2 side_effects rows", mutate: func(tr *store.Trace) {
			tr.SideEffects = append(tr.SideEffects, effectFor(key, "t2"))
		}},
		{name: "executed but no side effect on record", want: "executed but has no side_effects row", mutate: func(tr *store.Trace) {
			tr.SideEffects = nil
		}},
		{name: "executed without an idempotency key", want: "executed without an idempotency key", mutate: func(tr *store.Trace) {
			tr.ToolCalls[1].IdempotencyKey = nil
			tr.ToolCalls[1].ToolName = "deploy_release"
			tr.SideEffects = nil
		}},
		{name: "every executed call marked replayed", want: "every executed call is marked replayed", mutate: func(tr *store.Trace) {
			tr.ToolCalls[1] = replayedCall(tr.ToolCalls[1], "t2", 2*time.Second, 5*time.Second)
		}},
		{name: "the first call replayed and a later one made the effect", want: "the first executed call is marked replayed", mutate: func(tr *store.Trace) {
			first := replayedCall(tr.ToolCalls[1], "t0", time.Second, 2*time.Second)
			tr.ToolCalls = append([]store.ToolCall{tr.ToolCalls[0], first}, tr.ToolCalls[1:]...)
		}},
		{name: "a side effect with no executed call", want: "side effect key-bbbbbbbb has no executed tool call", mutate: func(tr *store.Trace) {
			tr.SideEffects = append(tr.SideEffects, effectFor("key-bbbbbbbbbbbbbbbb", "t9"))
		}},
		{name: "a side effect on record for a call that did not execute", want: "has no executed tool call", mutate: func(tr *store.Trace) {
			tr.ToolCalls[1].Status = store.ToolRejected
		}},
		{name: "two visits share a key", want: "share idempotency key", mutate: func(tr *store.Trace) {
			second := deployCall("t4", "rn2", key, 7*time.Second, 8*time.Second)
			second.Input = raw(`{"version":"other"}`)
			tr.ToolCalls = append(tr.ToolCalls, second)
		}},
		{name: "a side effect of another run", want: "belongs to run run-other, not run1", mutate: func(tr *store.Trace) {
			tr.SideEffects[0].RunID = "run-other"
		}},
		{name: "the effect was made by another call than the original", want: "was made by tool call t9 but the original executed call is t2", mutate: func(tr *store.Trace) {
			tr.SideEffects[0].ToolCallID = "t9"
		}},
		{name: "no run", want: "no run", mutate: func(tr *store.Trace) { tr.Run = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := deployTrace()
			if tt.mutate != nil {
				tt.mutate(&tr)
			}
			got := evalIdempotency(quickDeployCase(nil), tr)
			if got.Name != EvalIdempotency {
				t.Fatalf("name = %q", got.Name)
			}
			if got.Passed != tt.pass {
				t.Fatalf("passed = %v, want %v (detail: %s)", got.Passed, tt.pass, got.Detail)
			}
			if !strings.Contains(got.Detail, tt.want) {
				t.Fatalf("detail %q does not contain %q", got.Detail, tt.want)
			}
		})
	}
}
