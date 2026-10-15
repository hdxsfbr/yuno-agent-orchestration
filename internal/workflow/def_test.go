package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"yuno/internal/store"
)

// T5.6.1: structural rules produce path-qualified messages.
func TestValidateStructure(t *testing.T) {
	good := SingleAgent("a1", "do it", nil, store.Limits{})
	if err := good.ValidateStructure(); err != nil {
		t.Fatalf("single agent should validate: %v", err)
	}

	cases := []struct {
		name string
		def  Definition
		want string
	}{
		{"no start", Definition{Nodes: []Node{{ID: "end", Kind: KindEnd}}}, "exactly one start node"},
		{"bad id", Definition{Nodes: []Node{{ID: "Start!", Kind: KindStart}, {ID: "end", Kind: KindEnd}}, Edges: []Edge{{From: "Start!", To: "end"}}}, "nodes[0].id: must match"},
		{"unknown edge target", Definition{Nodes: []Node{{ID: "start", Kind: KindStart}, {ID: "end", Kind: KindEnd}}, Edges: []Edge{{From: "start", To: "endd"}}}, "edges[0].to: unknown node 'endd'"},
		{"no outgoing", Definition{Nodes: []Node{{ID: "start", Kind: KindStart}, {ID: "a", Kind: KindAgent, AgentID: "x", Task: "t"}, {ID: "end", Kind: KindEnd}}, Edges: []Edge{{From: "start", To: "end"}}}, "'a' has no outgoing edge"},
		{"unreachable", Definition{Nodes: []Node{{ID: "start", Kind: KindStart}, {ID: "a", Kind: KindAgent, AgentID: "x", Task: "t"}, {ID: "end", Kind: KindEnd}}, Edges: []Edge{{From: "start", To: "end"}, {From: "a", To: "end"}}}, "'a' is not reachable"},
		{"agent without task", Definition{Nodes: []Node{{ID: "start", Kind: KindStart}, {ID: "a", Kind: KindAgent, AgentID: "x"}, {ID: "end", Kind: KindEnd}}, Edges: []Edge{{From: "start", To: "a"}, {From: "a", To: "end"}}}, "nodes[1].task: required"},
		{"unknown kind", Definition{Nodes: []Node{{ID: "start", Kind: KindStart}, {ID: "z", Kind: "zz"}, {ID: "end", Kind: KindEnd}}, Edges: []Edge{{From: "start", To: "z"}, {From: "z", To: "end"}}}, "unknown kind 'zz'"},
	}
	for _, c := range cases {
		err := c.def.ValidateStructure()
		if err == nil {
			t.Errorf("%s: expected error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %q, want substring %q", c.name, err.Error(), c.want)
		}
	}
}

func TestParseRoundTrip(t *testing.T) {
	d := SingleAgent("a1", "task", nil, store.Limits{MaxSteps: 5})
	raw, _ := json.Marshal(d)
	back, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.Limits.MaxSteps != 5 || len(back.Nodes) != 3 || len(back.OutgoingEdges("start")) != 1 {
		t.Fatalf("round trip lost data: %+v", back)
	}
	if _, ok := back.Node("agent"); !ok {
		t.Fatal("node lookup failed")
	}
}
