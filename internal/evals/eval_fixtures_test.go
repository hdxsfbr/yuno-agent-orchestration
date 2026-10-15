package evals

import (
	"encoding/json"
	"testing"
	"time"

	"yuno/internal/store"
)

// twoAgentCaseYAML is a workflow case with a coder and a deployer. The deployer's rule
// for deploy_release is auto; the coder has no rules.
const twoAgentCaseYAML = `id: wf-b
version: v1
split: public
description: fixture case
workflow:
  state_schema: { type: object, properties: { coder: { type: object, required: [summary] } }, required: [coder] }
  nodes:
    - { id: start, kind: start }
    - { id: coder, kind: agent, agent_name: coder, task: "Do it", output_schema: { type: object, properties: { summary: { type: string } }, required: [summary] } }
    - { id: deployer, kind: agent, agent_name: deployer, task: "Ship it", output_schema: { type: object, properties: { status: { type: string } }, required: [status] } }
    - { id: end, kind: end }
  edges:
    - { from: start, to: coder }
    - { from: coder, to: deployer }
    - { from: deployer, to: end }
agents:
  coder: { system_prompt: x, tool_allowlist: [read, write, finish] }
  deployer: { system_prompt: x, tool_allowlist: [deploy_release, finish], interaction_rules: { deploy_release: auto } }
script:
  - text: done
expect:
  status: succeeded
`

func twoAgentCase(t *testing.T) Case {
	t.Helper()
	c, err := loadOne(t, twoAgentCaseYAML)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// quickDeployCase is a quick run whose agent may deploy; rules sets its interaction rules.
func quickDeployCase(rules map[string]string) Case {
	return Case{
		ID: "q", Version: "v1", Split: SplitPublic,
		Agent:  AgentSpec{ToolAllowlist: []string{"write", "deploy_release", "finish"}, InteractionRules: rules},
		Expect: Expect{Status: store.RunSucceeded},
	}
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

// deployCall is an executed deploy_release call in run node rn.
func deployCall(id, rn, key string, start, end time.Duration) store.ToolCall {
	return store.ToolCall{
		ID: id, RunID: "run1", RunNodeID: rn, ToolName: "deploy_release",
		Input: raw(`{"run_token":"r_x","version":"1.0.0","paths":["a.py"]}`), Output: raw(`{"release_id":"rel-1"}`),
		Status: store.ToolExecuted, IdempotencyKey: ptr(key),
		StartedAt: ptr(t0.Add(start)), FinishedAt: ptr(t0.Add(end)),
	}
}

func replayedCall(c store.ToolCall, id string, start, end time.Duration) store.ToolCall {
	c.ID, c.Output = id, raw(`{"release_id":"rel-1","replayed":true}`)
	c.StartedAt, c.FinishedAt = ptr(t0.Add(start)), ptr(t0.Add(end))
	return c
}

func approvedFor(id, call string, requested, decided time.Duration) store.Approval {
	return store.Approval{
		ID: id, RunID: "run1", Kind: store.ApprovalKindTool, ToolCallID: ptr(call), Status: store.ApprovalApproved,
		RequestedAt: t0.Add(requested), DecidedAt: ptr(t0.Add(decided)),
	}
}

func effectFor(key, call string) store.SideEffect {
	return store.SideEffect{IdempotencyKey: key, ToolName: "deploy_release", RunID: "run1", ToolCallID: call, Result: raw(`{"release_id":"rel-1"}`), CreatedAt: t0}
}

// deployTrace is a trace of one gated deploy that was approved at 3 s and finished at 5 s.
func deployTrace() store.Trace {
	tr := goodTrace()
	tr.Run.ID = "run1"
	tr.ToolCalls = []store.ToolCall{
		{ID: "t1", RunID: "run1", RunNodeID: "rn1", ToolName: "write", Status: store.ToolExecuted, FinishedAt: ptr(t0.Add(time.Second))},
		deployCall("t2", "rn1", "key-aaaaaaaaaaaaaaaa", 2*time.Second, 5*time.Second),
		{ID: "t3", RunID: "run1", RunNodeID: "rn1", ToolName: "yuno__finish", Status: store.ToolExecuted, FinishedAt: ptr(t0.Add(6 * time.Second))},
	}
	tr.Approvals = []store.Approval{approvedFor("a1", "t2", 2*time.Second, 3*time.Second)}
	tr.SideEffects = []store.SideEffect{effectFor("key-aaaaaaaaaaaaaaaa", "t2")}
	tr.Nodes = []store.RunNode{{ID: "rn1", RunID: "run1", NodeID: "agent", Status: store.NodeSucceeded, Output: raw(`{"summary":"ok"}`)}}
	return tr
}
