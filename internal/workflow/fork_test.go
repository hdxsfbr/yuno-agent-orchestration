package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

// when is a leaf condition for the fork tests.
func when(path, op, value string) *Condition {
	return &Condition{Path: path, Op: op, Value: json.RawMessage(value)}
}

// agent is a shorthand for an agent node in the fork tests.
func agent(id, agentID string, inputFrom ...string) Node {
	return Node{ID: id, Kind: KindAgent, AgentID: agentID, Task: "t", InputFrom: inputFrom}
}

// panel is the review panel's shape (DESIGN 5.26 rule 14) with agent ids: start → coder →
// fork(a, b, c) → join → lead → (coder | gate) → deployer → end.
func panel() Definition {
	return Definition{
		Project: true,
		Nodes: []Node{
			{ID: "start", Kind: KindStart},
			agent("coder", "A1", "start", "lead"),
			{ID: "panel", Kind: KindFork, Join: "merge"},
			agent("a", "A2", "coder"), agent("b", "A3", "coder"), agent("c", "A4", "coder"),
			{ID: "merge", Kind: KindJoin},
			agent("lead", "A5", "a", "b", "c"),
			{ID: "gate", Kind: KindApproval, Message: "ship?"},
			agent("deployer", "A6", "coder", "lead"),
			{ID: "end", Kind: KindEnd},
		},
		Edges: []Edge{
			{From: "start", To: "coder"}, {From: "coder", To: "panel"},
			{From: "panel", To: "a"}, {From: "panel", To: "b"}, {From: "panel", To: "c"},
			{From: "a", To: "merge"}, {From: "b", To: "merge"}, {From: "c", To: "merge"},
			{From: "merge", To: "lead"},
			{From: "lead", To: "coder", When: when("/lead/verdict", "eq", `"changes_requested"`)},
			{From: "lead", To: "gate"},
			{From: "gate", To: "deployer"}, {From: "deployer", To: "end"},
		},
	}
}

// T5.26.1 (valid shapes): the review panel, a loop inside a branch, a loop around the
// pair, the same agent before the fork and inside a branch, and the region helpers.
func TestForkValidShapes(t *testing.T) {
	d := panel()
	if err := d.ValidateStructure(); err != nil {
		t.Fatalf("panel: %v", err)
	}
	r, err := d.Region("panel")
	if err != nil {
		t.Fatal(err)
	}
	if r.Join != "merge" || strings.Join(r.Heads, ",") != "a,b,c" || len(r.Members) != 3 || r.Members["b"] != "b" || len(r.EdgeIndexes) != 3 || r.EdgeIndexes[0] != 2 {
		t.Fatalf("region = %+v", r)
	}
	if f, ok := d.ForkOf("merge"); !ok || f != "panel" {
		t.Fatalf("ForkOf = %q %v", f, ok)
	}
	if _, ok := d.ForkOf("lead"); ok {
		t.Fatal("lead is nobody's join")
	}

	// A loop inside branch a (a → a2 → a) and a checkout on the join.
	loop := panel()
	loop.Nodes = append(loop.Nodes, agent("a2", "A7", "a"))
	loop.Nodes[6].Checkout = "a"
	loop.Edges = append(loop.Edges,
		Edge{From: "a", To: "a2", When: when("/a/again", "eq", `true`)},
		Edge{From: "a2", To: "a"})
	// a's edge to merge stays, after the loop edge in evaluation order is fine either way.
	if err := loop.ValidateStructure(); err != nil {
		t.Fatalf("loop inside a branch: %v", err)
	}
	r, err = loop.Region("panel")
	if err != nil || r.Members["a2"] != "a" || r.Checkout != "a" {
		t.Fatalf("region with a loop = %+v %v", r, err)
	}

	// The same agent before the fork (coder, A1) and inside branch c.
	same := panel()
	same.Nodes[5].AgentID = "A1"
	if err := same.ValidateStructure(); err != nil {
		t.Fatalf("same agent before and inside: %v", err)
	}

	// A template compares names; distinct names pass.
	tpl := panel()
	for i := range tpl.Nodes {
		if tpl.Nodes[i].Kind == KindAgent {
			tpl.Nodes[i].AgentName, tpl.Nodes[i].AgentID = "Agent "+tpl.Nodes[i].AgentID, ""
		}
	}
	if err := tpl.ValidateStructure(); err != nil {
		t.Fatalf("template: %v", err)
	}

	// Two forks in sequence, each with its own join.
	two := panel()
	two.Nodes = append(two.Nodes, Node{ID: "f2", Kind: KindFork, Join: "j2"}, agent("x", "B1", "lead"), agent("y", "B2", "lead"), Node{ID: "j2", Kind: KindJoin})
	two.Edges[10] = Edge{From: "lead", To: "f2"} // lead → gate becomes lead → f2 → (x, y) → j2 → gate
	two.Edges = append(two.Edges, Edge{From: "f2", To: "x"}, Edge{From: "f2", To: "y"}, Edge{From: "x", To: "j2"}, Edge{From: "y", To: "j2"}, Edge{From: "j2", To: "gate"})
	if err := two.ValidateStructure(); err != nil {
		t.Fatalf("two forks in sequence: %v", err)
	}
}

// T5.26.1 (each message of rule 2): a definition that violates one rule reports it,
// path-qualified.
func TestForkValidationMessages(t *testing.T) {
	cases := []struct {
		name string
		mut  func(d *Definition)
		want string
	}{
		{"join required", func(d *Definition) { d.Nodes[2].Join = "" }, "nodes[2].join: required for fork nodes"},
		{"join unknown", func(d *Definition) { d.Nodes[2].Join = "nope" }, "nodes[2].join: unknown node 'nope'"},
		{"join not a join", func(d *Definition) { d.Nodes[2].Join = "lead" }, "nodes[2].join: 'lead' is not a join node"},
		{"join taken", func(d *Definition) {
			d.Nodes = append(d.Nodes, Node{ID: "f2", Kind: KindFork, Join: "merge"}, agent("x", "B1"), agent("y", "B2"))
			d.Edges = append(d.Edges, Edge{From: "start", To: "f2"}, Edge{From: "f2", To: "x"}, Edge{From: "f2", To: "y"}, Edge{From: "x", To: "merge"}, Edge{From: "y", To: "merge"})
		}, "nodes[11].join: 'merge' is already the join of 'panel'"},
		{"one edge", func(d *Definition) {
			d.Edges = append(d.Edges[:3], d.Edges[5:]...) // drop panel→b, panel→c
			d.Nodes = append(d.Nodes[:4], d.Nodes[6:]...) // drop b, c
			d.Edges = append(d.Edges[:5], d.Edges[7:]...) // drop b→merge, c→merge
			d.Nodes[5].InputFrom = []string{"a"}
		}, "nodes[2]: a fork needs at least two outgoing edges"},
		{"six edges", func(d *Definition) {
			for _, id := range []string{"d", "e", "f"} {
				d.Nodes = append(d.Nodes, agent(id, "X"+id))
				d.Edges = append(d.Edges, Edge{From: "panel", To: id}, Edge{From: id, To: "merge"})
			}
		}, "nodes[2]: a fork may have at most 5 outgoing edges"},
		{"branch at join", func(d *Definition) { d.Edges = append(d.Edges, Edge{From: "panel", To: "merge"}) }, "edges[13].to: a branch of 'panel' cannot start at its join"},
		{"duplicate branch", func(d *Definition) { d.Edges = append(d.Edges, Edge{From: "panel", To: "a"}) }, "edges[13].to: duplicate branch of 'panel'"},
		{"fork on_failure", func(d *Definition) { d.Nodes[2].OnFailure = "end" }, "nodes[2].on_failure: a fork has none; a failed branch fails the run"},
		{"join on_failure", func(d *Definition) { d.Nodes[6].OnFailure = "end" }, "nodes[6].on_failure: a join has none"},
		{"join nobody's", func(d *Definition) {
			d.Nodes = append(d.Nodes, Node{ID: "j2", Kind: KindJoin})
			d.Edges = append(d.Edges, Edge{From: "j2", To: "end"})
		}, "nodes[11]: join 'j2' is not named by any fork"},
		{"checkout unknown branch", func(d *Definition) { d.Nodes[6].Checkout = "lead" }, "nodes[6].checkout: unknown branch 'lead'"},
		{"checkout without project", func(d *Definition) { d.Nodes[6].Checkout = "a"; d.Project = false }, "nodes[6].checkout: the workflow does not work on a project"},
		{"checkout on a fork", func(d *Definition) { d.Nodes[2].Checkout = "a" }, "nodes[2].checkout: only join nodes have one"},
		{"checkout on an agent", func(d *Definition) { d.Nodes[1].Checkout = "a" }, "nodes[1].checkout: only join nodes have one"},
		{"join on an agent", func(d *Definition) { d.Nodes[1].Join = "merge" }, "nodes[1].join: only fork nodes have one"},
		{"approval inside", func(d *Definition) {
			d.Nodes = append(d.Nodes, Node{ID: "ask", Kind: KindApproval, Message: "ok?"})
			d.Edges[5] = Edge{From: "a", To: "ask"}
			d.Edges = append(d.Edges, Edge{From: "ask", To: "merge"})
		}, "nodes[11]: an approval node cannot be inside a branch of 'panel'; put the gate after its join 'merge'"},
		{"fork inside", func(d *Definition) {
			d.Nodes = append(d.Nodes, Node{ID: "f2", Kind: KindFork, Join: "j2"}, agent("x", "B1"), agent("y", "B2"), Node{ID: "j2", Kind: KindJoin})
			d.Edges[5] = Edge{From: "a", To: "f2"}
			d.Edges = append(d.Edges, Edge{From: "f2", To: "x"}, Edge{From: "f2", To: "y"}, Edge{From: "x", To: "j2"}, Edge{From: "y", To: "j2"}, Edge{From: "j2", To: "merge"})
		}, "nodes[11]: a fork cannot be inside a branch of 'panel' (forks do not nest)"},
		{"to the shared end", func(d *Definition) { d.Edges[5] = Edge{From: "a", To: "end"} }, "edges[5].to: leaves the branch of 'panel' without reaching its join 'merge'"},
		{"end inside", func(d *Definition) {
			d.Nodes = append(d.Nodes, Node{ID: "end2", Kind: KindEnd})
			d.Edges[5] = Edge{From: "a", To: "end2"}
		}, "nodes[11]: an end node cannot be inside a branch of 'panel'"},
		{"leaves to before", func(d *Definition) {
			d.Edges = append(d.Edges, Edge{From: "a", To: "coder", When: when("/a/x", OpExists, ``)})
		}, "edges[13].to: leaves the branch of 'panel' without reaching its join 'merge'"},
		{"leaves to after", func(d *Definition) {
			d.Edges = append(d.Edges, Edge{From: "b", To: "lead", When: when("/b/x", OpExists, ``)})
		}, "edges[13].to: leaves the branch of 'panel' without reaching its join 'merge'"},
		{"on_failure leaves", func(d *Definition) { d.Nodes[3].OnFailure = "deployer" }, "nodes[3].on_failure: leaves the branch of 'panel' without reaching its join 'merge'"},
		{"enters from outside", func(d *Definition) {
			d.Edges = append(d.Edges, Edge{From: "coder", To: "b", When: when("/coder/x", OpExists, ``)})
		}, "edges[13].to: enters a branch of 'panel' from outside"},
		{"on_failure enters", func(d *Definition) { d.Nodes[1].OnFailure = "c" }, "nodes[1].on_failure: enters a branch of 'panel' from outside"},
		{"join from outside", func(d *Definition) {
			d.Edges = append(d.Edges, Edge{From: "coder", To: "merge", When: when("/coder/x", OpExists, ``)})
		}, "edges[13].to: join 'merge' is reached only through the branches of 'panel'"},
		{"two branches meet", func(d *Definition) {
			d.Nodes = append(d.Nodes, agent("shared", "S1"))
			d.Edges[5] = Edge{From: "a", To: "shared"}
			d.Edges[6] = Edge{From: "b", To: "shared"}
			d.Edges = append(d.Edges, Edge{From: "shared", To: "merge"})
		}, "nodes[11]: 'shared' is reachable from two branches of 'panel' ('a' and 'b'); branches meet only at the join 'merge'"},
		{"input_from across", func(d *Definition) { d.Nodes[4].InputFrom = []string{"coder", "a"} }, "nodes[4].input_from[1]: 'a' is in another branch of 'panel'"},
		{"same agent twice", func(d *Definition) { d.Nodes[4].AgentID = "A2" }, "nodes[4].agent_id: agent 'A2' is used in two branches of 'panel' ('a' and 'b'); branches run at once and share one workspace per agent"},
		{"same agent name twice", func(d *Definition) {
			d.Nodes[3].AgentID, d.Nodes[3].AgentName = "", "Reviewer"
			d.Nodes[5].AgentID, d.Nodes[5].AgentName = "", "reviewer"
		}, "nodes[5].agent_name: agent 'reviewer' is used in two branches of 'panel' ('a' and 'c'); branches run at once and share one workspace per agent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := panel()
			c.mut(&d)
			err := d.ValidateStructure()
			if err == nil {
				t.Fatalf("expected %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v\nwant substring %q", err, c.want)
			}
		})
	}
}

// A definition without forks reports no fork message and Region refuses a non-fork.
func TestForkValidationWithoutForks(t *testing.T) {
	d := SingleAgent("a1", "do it", nil, panel().Limits)
	if err := d.ValidateStructure(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Region("agent"); err == nil {
		t.Fatal("Region of an agent node should fail")
	}
}
