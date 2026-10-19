package workflow

import (
	"strings"
	"testing"
)

func agentNode(id, autoAfter string) Node {
	return Node{ID: id, Kind: KindAgent, AgentID: "a", Task: "t", AutoAfter: autoAfter}
}

// wavePipelineShape is builder -> draft -> manual_test -> ready -> end, with the manual
// test's rejection going back to the builder.
func wavePipelineShape(ready Node) Definition {
	return Definition{
		Nodes: []Node{{ID: "start", Kind: KindStart}, agentNode("builder", ""), agentNode("draft", ""),
			{ID: "manual_test", Kind: KindApproval, OnReject: "builder"}, ready, {ID: "end", Kind: KindEnd}},
		Edges: []Edge{{From: "start", To: "builder"}, {From: "builder", To: "draft"}, {From: "draft", To: "manual_test"},
			{From: "manual_test", To: "ready"}, {From: "ready", To: "end"}},
	}
}

// T5.29.4 (save half): auto_after names an approval node that every path to the node
// passes through.
func TestValidateAutoAfter(t *testing.T) {
	good := wavePipelineShape(agentNode("ready", "manual_test"))
	if err := good.ValidateStructure(); err != nil {
		t.Fatalf("valid auto_after: %v", err)
	}

	bypass := wavePipelineShape(agentNode("ready", "manual_test"))
	bypass.Edges = append(bypass.Edges, Edge{From: "draft", To: "ready"})

	failureBypass := wavePipelineShape(agentNode("ready", "manual_test"))
	failureBypass.Nodes[2].OnFailure = "ready"

	notApproval := wavePipelineShape(agentNode("ready", "draft"))
	self := wavePipelineShape(agentNode("ready", "ready"))
	unknown := wavePipelineShape(agentNode("ready", "nope"))
	after := wavePipelineShape(agentNode("ready", "manual_test"))
	after.Nodes[2].AutoAfter = "manual_test" // draft precedes the approval
	onApproval := wavePipelineShape(agentNode("ready", "manual_test"))
	onApproval.Nodes[3].AutoAfter = "manual_test"

	cases := []struct {
		name string
		def  Definition
		want string
	}{
		{"unknown node", unknown, "nodes[4].auto_after: unknown node 'nope'"},
		{"not an approval", notApproval, "nodes[4].auto_after: 'draft' is not an approval node"},
		{"itself", self, "nodes[4].auto_after: 'ready' is not an approval node"},
		{"a path around the approval", bypass, "nodes[4].auto_after: approval 'manual_test' must precede 'ready' on every path from start, but 'ready' can be reached without passing it"},
		{"an on_failure path around it", failureBypass, "nodes[4].auto_after: approval 'manual_test' must precede 'ready'"},
		{"the approval comes later", after, "nodes[2].auto_after: approval 'manual_test' must precede 'draft'"},
		{"on an approval node", onApproval, "nodes[3].auto_after: only agent nodes have one"},
	}
	for _, c := range cases {
		err := c.def.ValidateStructure()
		if err == nil {
			t.Errorf("%s: expected an error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %q, want substring %q", c.name, err.Error(), c.want)
		}
	}
}
