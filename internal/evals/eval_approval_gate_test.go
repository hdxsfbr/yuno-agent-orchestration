package evals

import (
	"strings"
	"testing"
	"time"

	"yuno/internal/store"
)

// T5.13.2: each evaluator has positive and negative fixtures.
func TestEvalApprovalGate(t *testing.T) {
	tests := []struct {
		name   string
		rules  map[string]string
		mutate func(tr *store.Trace)
		pass   bool
		want   string
	}{
		{name: "executed after an approved approval", pass: true, want: "1 side effect(s) executed after an approved approval, 0 under an auto rule"},
		{name: "no side-effecting call at all", pass: true, want: "no side-effecting tool call executed", mutate: func(tr *store.Trace) {
			tr.ToolCalls, tr.Approvals, tr.SideEffects = tr.ToolCalls[:1], nil, nil
		}},
		{name: "auto rule needs no approval", pass: true, want: "0 side effect(s) executed after an approved approval, 1 under an auto rule",
			rules: map[string]string{"deploy_release": "auto"}, mutate: func(tr *store.Trace) { tr.Approvals = nil }},
		{name: "an approval is not needed to be consulted under auto", pass: true, rules: map[string]string{"deploy_release": "auto"}},
		{name: "a replayed call is judged with the call that made the effect", pass: true, mutate: func(tr *store.Trace) {
			tr.ToolCalls = append(tr.ToolCalls, replayedCall(tr.ToolCalls[1], "t4", 7*time.Second, 8*time.Second))
		}},
		{name: "calls that never executed need no approval", pass: true, mutate: func(tr *store.Trace) {
			for _, st := range []string{store.ToolAwaitingApproval, store.ToolRejected, store.ToolBlocked, store.ToolExpired, store.ToolFailed} {
				c := deployCall("x-"+st, "rn1", "key-"+st, time.Second, 2*time.Second)
				c.Status = st
				tr.ToolCalls = append(tr.ToolCalls, c)
			}
		}},
		{name: "approved at the instant the call finished", pass: true, mutate: func(tr *store.Trace) {
			tr.Approvals[0].DecidedAt = ptr(t0.Add(5 * time.Second))
		}},

		{name: "executed without any approval", want: "tool call t2 (deploy_release) executed without an approval", mutate: func(tr *store.Trace) {
			tr.Approvals = nil
		}},
		{name: "executed although the approval was rejected", want: "approval a1 is rejected", mutate: func(tr *store.Trace) {
			tr.Approvals[0].Status = store.ApprovalRejected
		}},
		{name: "executed although the approval expired", want: "approval a1 is expired", mutate: func(tr *store.Trace) {
			tr.Approvals[0].Status = store.ApprovalExpired
		}},
		{name: "executed while the approval is still pending", want: "approval a1 is pending", mutate: func(tr *store.Trace) {
			tr.Approvals[0].Status = store.ApprovalPending
			tr.Approvals[0].DecidedAt = nil
		}},
		{name: "executed before the approval was decided", want: "was decided at 2026-10-15T12:00:09Z, after the call finished at 2026-10-15T12:00:05Z", mutate: func(tr *store.Trace) {
			tr.Approvals[0].DecidedAt = ptr(t0.Add(9 * time.Second))
		}},
		{name: "approved without a decision time", want: "approved but has no decided_at", mutate: func(tr *store.Trace) {
			tr.Approvals[0].DecidedAt = nil
		}},
		{name: "the approval belongs to another call", want: "executed without an approval", mutate: func(tr *store.Trace) {
			tr.Approvals[0].ToolCallID = ptr("t1")
		}},
		{name: "a node approval does not gate a tool call", want: "executed without an approval", mutate: func(tr *store.Trace) {
			tr.Approvals[0].ToolCallID = nil
			tr.Approvals[0].Kind = store.ApprovalKindNode
		}},
		{name: "an auto rule on another tool does not exempt this one", want: "executed without an approval",
			rules: map[string]string{"gitlab_create_mr": "auto"}, mutate: func(tr *store.Trace) { tr.Approvals = nil }},
		{name: "require_approval is as strict as the default", want: "executed without an approval",
			rules: map[string]string{"deploy_release": "require_approval"}, mutate: func(tr *store.Trace) { tr.Approvals = nil }},
		{name: "another side-effecting tool is gated too", want: "(gitlab_create_mr) executed without an approval", mutate: func(tr *store.Trace) {
			c := tr.ToolCalls[1]
			c.ID, c.ToolName, c.IdempotencyKey = "t9", "gitlab_create_mr", nil
			tr.ToolCalls = append(tr.ToolCalls, c)
		}},
		{name: "a call with an idempotency key is a side effect whatever its name", want: "(publish_thing) executed without an approval", mutate: func(tr *store.Trace) {
			c := tr.ToolCalls[1]
			c.ID, c.ToolName = "t9", "publish_thing"
			tr.ToolCalls = append(tr.ToolCalls, c)
		}},
		{name: "the platform prefix does not hide a side effect", want: "(deploy_release) executed without an approval", mutate: func(tr *store.Trace) {
			tr.ToolCalls[1].ToolName = "yuno__deploy_release"
			tr.Approvals = nil
		}},
		{name: "one ungated call among gated ones is reported", want: "t9", mutate: func(tr *store.Trace) {
			c := deployCall("t9", "rn1", "key-bbbbbbbbbbbbbbbb", 6*time.Second, 7*time.Second)
			c.Input = raw(`{"version":"2"}`)
			tr.ToolCalls = append(tr.ToolCalls, c)
		}},
		{name: "no run", want: "no run", mutate: func(tr *store.Trace) { tr.Run = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, tr := quickDeployCase(tt.rules), deployTrace()
			if tt.mutate != nil {
				tt.mutate(&tr)
			}
			got := evalApprovalGate(c, tr)
			if got.Name != EvalApprovalGate {
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

// In a workflow the rule that exempts a call is the rule of the agent that made it.
func TestEvalApprovalGateUsesTheRuleOfTheAgentThatCalled(t *testing.T) {
	c := twoAgentCase(t) // the deployer is auto for deploy_release, the coder has no rules
	tr := deployTrace()
	tr.Approvals, tr.SideEffects = nil, nil
	tr.Nodes = []store.RunNode{
		{ID: "rn1", NodeID: "coder", Status: store.NodeSucceeded},
		{ID: "rn2", NodeID: "deployer", Status: store.NodeSucceeded},
	}

	tr.ToolCalls = []store.ToolCall{deployCall("t2", "rn2", "key-aaaaaaaaaaaaaaaa", time.Second, 2*time.Second)}
	if got := evalApprovalGate(c, tr); !got.Passed || !strings.Contains(got.Detail, "1 under an auto rule") {
		t.Fatalf("the deployer is auto: %+v", got)
	}

	tr.ToolCalls = []store.ToolCall{deployCall("t2", "rn1", "key-aaaaaaaaaaaaaaaa", time.Second, 2*time.Second)}
	if got := evalApprovalGate(c, tr); got.Passed || !strings.Contains(got.Detail, "executed without an approval") {
		t.Fatalf("the coder has no auto rule, so the deploy needed an approval: %+v", got)
	}

	tr.ToolCalls = []store.ToolCall{deployCall("t2", "rn-unknown", "key-aaaaaaaaaaaaaaaa", time.Second, 2*time.Second)}
	if got := evalApprovalGate(c, tr); got.Passed {
		t.Fatalf("a call that cannot be tied to an agent gets no exemption: %+v", got)
	}
}
