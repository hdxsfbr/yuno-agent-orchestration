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

func TestValidateStructureWave2Rules(t *testing.T) {
	base := func() Definition {
		return Definition{
			Nodes: []Node{
				{ID: "start", Kind: KindStart},
				{ID: "a", Kind: KindAgent, AgentID: "x", Task: "t", InputFrom: []string{"start"}},
				{ID: "gate", Kind: KindApproval, Message: "ok?"},
				{ID: "fallback", Kind: KindAgent, AgentID: "x", Task: "t"},
				{ID: "end", Kind: KindEnd},
			},
			Edges: []Edge{{From: "start", To: "a"}, {From: "a", To: "gate"}, {From: "gate", To: "end"}, {From: "fallback", To: "end"}},
		}
	}
	// fallback is reachable only through on_reject / on_failure.
	ok := base()
	ok.Nodes[2].OnReject = "fallback"
	if err := ok.ValidateStructure(); err != nil {
		t.Fatalf("on_reject target counts as reachable: %v", err)
	}
	ok = base()
	ok.Nodes[1].OnFailure = "fallback"
	if err := ok.ValidateStructure(); err != nil {
		t.Fatalf("on_failure target counts as reachable: %v", err)
	}
	cases := []struct {
		name string
		mut  func(d *Definition)
		want string
	}{
		{"unknown on_reject", func(d *Definition) { d.Nodes[2].OnReject = "nope" }, "nodes[2].on_reject: unknown node 'nope'"},
		{"on_reject on an agent", func(d *Definition) { d.Nodes[1].OnReject = "fallback" }, "nodes[1].on_reject: only approval nodes have one"},
		{"unknown on_failure", func(d *Definition) { d.Nodes[1].OnFailure = "nope" }, "nodes[1].on_failure: unknown node 'nope'"},
		{"unknown input_from", func(d *Definition) { d.Nodes[1].InputFrom = []string{"start", "ghost"} }, "nodes[1].input_from[1]: unknown node 'ghost'"},
		{"reserved id", func(d *Definition) { d.Nodes[3].ID = "pending_effects"; d.Edges[3].From = "pending_effects" }, "nodes[3].id: 'pending_effects' is reserved"},
		{"edge into start", func(d *Definition) { d.Edges = append(d.Edges, Edge{From: "fallback", To: "start"}) }, "edges[4].to: the start node cannot be an edge target"},
		{"unreachable without on_reject", func(d *Definition) {}, "'fallback' is not reachable"},
	}
	for _, c := range cases {
		d := base()
		c.mut(&d)
		err := d.ValidateStructure()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
}
