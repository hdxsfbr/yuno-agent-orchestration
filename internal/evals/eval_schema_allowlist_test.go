package evals

import (
	"strings"
	"testing"
	"time"

	"yuno/internal/store"
)

// schemaTrace is a successful two-node run of twoAgentCase: both outputs and the final
// state are valid.
func schemaTrace() store.Trace {
	tr := goodTrace()
	tr.Run.State = raw(`{"coder":{"summary":"done"},"deployer":{"status":"deployed"}}`)
	tr.Nodes = []store.RunNode{
		{ID: "rn1", NodeID: "coder", Visit: 1, Status: store.NodeSucceeded, Output: raw(`{"summary":"done"}`)},
		{ID: "rn2", NodeID: "deployer", Visit: 1, Status: store.NodeSucceeded, Output: raw(`{"status":"deployed"}`)},
	}
	return tr
}

func TestEvalSchema(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(tr *store.Trace)
		pass   bool
		want   string
	}{
		{name: "outputs and state match", pass: true, want: "2 node output(s) match their output_schema, the final state matches the state_schema"},
		{name: "a node that failed has no output to check", pass: true, want: "1 node output(s)", mutate: func(tr *store.Trace) {
			tr.Nodes[1].Status, tr.Nodes[1].Output = store.NodeFailed, nil
		}},
		{name: "a revisited node is checked on every visit", pass: true, want: "3 node output(s)", mutate: func(tr *store.Trace) {
			tr.Nodes = append(tr.Nodes, store.RunNode{ID: "rn3", NodeID: "coder", Visit: 2, Status: store.NodeSucceeded, Output: raw(`{"summary":"again"}`)})
		}},
		{name: "extra properties are allowed when the schema allows them", pass: true, mutate: func(tr *store.Trace) {
			tr.Nodes[0].Output = raw(`{"summary":"done","files":["a.py"]}`)
		}},
		{name: "a node the definition does not know is ignored", pass: true, want: "2 node output(s)", mutate: func(tr *store.Trace) {
			tr.Nodes = append(tr.Nodes, store.RunNode{ID: "rn9", NodeID: "mystery", Status: store.NodeSucceeded, Output: raw(`7`)})
		}},
		{name: "an empty final state is checked as an empty object", mutate: func(tr *store.Trace) {
			tr.Run.State = nil
		}, want: "final state does not match the state_schema"},

		{name: "a required property is missing", want: "node coder (visit 1) output does not match its output_schema", mutate: func(tr *store.Trace) {
			tr.Nodes[0].Output = raw(`{"note":"x"}`)
		}},
		{name: "a property has the wrong type", want: "at /status", mutate: func(tr *store.Trace) {
			tr.Nodes[1].Output = raw(`{"status":7}`)
		}},
		{name: "the second visit is invalid", want: "node coder (visit 2)", mutate: func(tr *store.Trace) {
			tr.Nodes = append(tr.Nodes, store.RunNode{ID: "rn3", NodeID: "coder", Visit: 2, Status: store.NodeSucceeded, Output: raw(`{}`)})
		}},
		{name: "a succeeded node with no output", want: "node deployer (visit 1) succeeded without an output", mutate: func(tr *store.Trace) {
			tr.Nodes[1].Output = nil
		}},
		{name: "a null output counts as none", want: "succeeded without an output", mutate: func(tr *store.Trace) {
			tr.Nodes[1].Output = raw(`null`)
		}},
		{name: "the final state breaks the state schema", want: "final state does not match the state_schema", mutate: func(tr *store.Trace) {
			tr.Run.State = raw(`{"deployer":{"status":"deployed"}}`)
		}},
		{name: "several problems are all reported", want: "node deployer", mutate: func(tr *store.Trace) {
			tr.Nodes[0].Output = raw(`{}`)
			tr.Nodes[1].Output = raw(`{}`)
			tr.Run.State = raw(`{}`)
		}},
		{name: "no run", want: "no run", mutate: func(tr *store.Trace) { tr.Run = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := schemaTrace()
			if tt.mutate != nil {
				tt.mutate(&tr)
			}
			got := evalSchema(twoAgentCase(t), tr)
			if got.Name != EvalSchema {
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

func TestEvalSchemaSeveralProblemsAreAllReported(t *testing.T) {
	tr := schemaTrace()
	tr.Nodes[0].Output = raw(`{}`)
	tr.Nodes[1].Output = raw(`{}`)
	tr.Run.State = raw(`{}`)
	got := evalSchema(twoAgentCase(t), tr)
	for _, want := range []string{"node coder", "node deployer", "final state"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail %q does not mention %q", got.Detail, want)
		}
	}
}

// A quick run has no definition in the case: the default single-agent schema applies.
func TestEvalSchemaForAQuickRun(t *testing.T) {
	good := goodTrace()
	good.Nodes = []store.RunNode{{ID: "rn1", NodeID: "agent", Status: store.NodeSucceeded, Output: raw(`{"summary":"done"}`)}}
	if got := evalSchema(baseCase(), good); !got.Passed || !strings.Contains(got.Detail, "1 node output(s)") {
		t.Fatalf("valid quick-run output: %+v", got)
	}
	bad := goodTrace()
	bad.Nodes = []store.RunNode{{ID: "rn1", NodeID: "agent", Status: store.NodeSucceeded, Output: raw(`{"details":"no summary"}`)}}
	if got := evalSchema(baseCase(), bad); got.Passed || !strings.Contains(got.Detail, "node agent") {
		t.Fatalf("a quick-run output without a summary must fail: %+v", got)
	}
}

// The snapshot stored with the run is what ran, so it wins over the case's definition.
func TestEvalSchemaPrefersTheRunSnapshot(t *testing.T) {
	tr := schemaTrace()
	tr.Run.WorkflowSnapshot = raw(`{"nodes":[
	  {"id":"start","kind":"start"},
	  {"id":"coder","kind":"agent","agent_id":"a1","task":"x","output_schema":{"type":"object","required":["summary","files"]}},
	  {"id":"end","kind":"end"}],"edges":[]}`)
	got := evalSchema(twoAgentCase(t), tr)
	if got.Passed || !strings.Contains(got.Detail, "node coder") {
		t.Fatalf("the snapshot requires files, the output has none: %+v", got)
	}
	tr.Nodes[0].Output = raw(`{"summary":"done","files":[]}`)
	if got := evalSchema(twoAgentCase(t), tr); !got.Passed {
		t.Fatalf("the snapshot's schema is satisfied: %+v", got)
	}
	// A snapshot that cannot be read falls back to the case.
	tr.Run.WorkflowSnapshot = raw(`{not json`)
	tr.Nodes[0].Output = raw(`{"summary":"done"}`)
	if got := evalSchema(twoAgentCase(t), tr); !got.Passed {
		t.Fatalf("an unreadable snapshot falls back to the case's definition: %+v", got)
	}
}

func TestEvalAllowlist(t *testing.T) {
	exec := func(id, rn, tool string) store.ToolCall {
		return store.ToolCall{ID: id, RunNodeID: rn, ToolName: tool, Status: store.ToolExecuted, FinishedAt: ptr(t0.Add(time.Second))}
	}
	tests := []struct {
		name   string
		calls  []store.ToolCall
		mutate func(tr *store.Trace)
		pass   bool
		want   string
	}{
		{name: "every call is on the list", pass: true, want: "3 executed tool call(s), all within their agents' allowlists",
			calls: []store.ToolCall{exec("1", "rn1", "write"), exec("2", "rn1", "read"), exec("3", "rn2", "deploy_release")}},
		{name: "the platform prefix is stripped", pass: true,
			calls: []store.ToolCall{exec("1", "rn2", "yuno__deploy_release"), exec("2", "rn2", "yuno__finish")}},
		{name: "finish is allowed whatever the list says", pass: true,
			calls: []store.ToolCall{exec("1", "rn1", "finish"), exec("2", "rn1", "yuno__finish")}},
		{name: "refused calls are not judged", pass: true, want: "0 executed tool call(s)", calls: []store.ToolCall{
			{ID: "1", RunNodeID: "rn1", ToolName: "deploy_release", Status: store.ToolBlocked},
			{ID: "2", RunNodeID: "rn1", ToolName: "exec", Status: store.ToolRejected},
			{ID: "3", RunNodeID: "rn1", ToolName: "exec", Status: store.ToolExpired},
			{ID: "4", RunNodeID: "rn1", ToolName: "exec", Status: store.ToolFailed},
		}},
		{name: "no calls", pass: true, calls: nil},

		{name: "a tool outside the list", want: "agent coder executed exec, which is not in its allowlist [read, write, finish]",
			calls: []store.ToolCall{exec("1", "rn1", "exec")}},
		{name: "a tool that is on another agent's list", want: "agent coder executed deploy_release",
			calls: []store.ToolCall{exec("1", "rn1", "deploy_release")}},
		{name: "the platform prefix does not bypass the list", want: "agent coder executed deploy_release",
			calls: []store.ToolCall{exec("1", "rn1", "yuno__deploy_release")}},
		{name: "a call that cannot be tied to an agent", want: "cannot be tied to an agent of the case",
			calls: []store.ToolCall{exec("1", "rn-unknown", "write")}},
		{name: "a call from a node that is not an agent", want: "cannot be tied to an agent",
			calls: []store.ToolCall{exec("1", "rn0", "write")}, mutate: func(tr *store.Trace) {
				tr.Nodes = append(tr.Nodes, store.RunNode{ID: "rn0", NodeID: "end"})
			}},
		{name: "no run", want: "no run", mutate: func(tr *store.Trace) { tr.Run = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := goodTrace()
			tr.ToolCalls = tt.calls
			tr.Nodes = []store.RunNode{{ID: "rn1", NodeID: "coder"}, {ID: "rn2", NodeID: "deployer"}}
			if tt.mutate != nil {
				tt.mutate(&tr)
			}
			got := evalAllowlist(twoAgentCase(t), tr)
			if got.Name != EvalAllowlist {
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

func TestEvalAllowlistForAQuickRunAndBlockedRules(t *testing.T) {
	c := quickDeployCase(map[string]string{"deploy_release": "blocked"})
	tr := goodTrace()
	tr.ToolCalls = []store.ToolCall{{ID: "1", ToolName: "write", Status: store.ToolExecuted}}
	if got := evalAllowlist(c, tr); !got.Passed {
		t.Fatalf("write is on the list: %+v", got)
	}
	tr.ToolCalls = []store.ToolCall{{ID: "1", ToolName: "exec", Status: store.ToolExecuted}}
	if got := evalAllowlist(c, tr); got.Passed || !strings.Contains(got.Detail, "agent agent executed exec") {
		t.Fatalf("exec is not on the list: %+v", got)
	}
	// Blocked in the rules means a call that executed is a platform bug, even though the
	// tool is on the allowlist.
	tr.ToolCalls = []store.ToolCall{{ID: "1", ToolName: "deploy_release", Status: store.ToolExecuted}}
	if got := evalAllowlist(c, tr); got.Passed || !strings.Contains(got.Detail, "although its rule is blocked") {
		t.Fatalf("a blocked tool must not execute: %+v", got)
	}
	tr.ToolCalls = []store.ToolCall{{ID: "1", ToolName: "deploy_release", Status: store.ToolBlocked}}
	if got := evalAllowlist(c, tr); !got.Passed {
		t.Fatalf("a refused call is the platform doing its job: %+v", got)
	}
}

// finish is the one tool every managed agent can call, whatever its allowlist says.
func TestEvalAllowlistAlwaysAllowsFinish(t *testing.T) {
	c := baseCase()
	c.Agent.ToolAllowlist = []string{"write"}
	tr := goodTrace()
	tr.ToolCalls = []store.ToolCall{
		{ID: "1", ToolName: "write", Status: store.ToolExecuted},
		{ID: "2", ToolName: "yuno__finish", Status: store.ToolExecuted},
		{ID: "3", ToolName: "finish", Status: store.ToolExecuted},
	}
	if got := evalAllowlist(c, tr); !got.Passed {
		t.Fatalf("finish is not on the allowlist but every agent can call it: %+v", got)
	}
	tr.ToolCalls = append(tr.ToolCalls, store.ToolCall{ID: "4", ToolName: "exec", Status: store.ToolExecuted})
	if got := evalAllowlist(c, tr); got.Passed {
		t.Fatalf("exec is not on the list: %+v", got)
	}
}
