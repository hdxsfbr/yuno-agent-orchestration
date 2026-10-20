package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// T5.28.4: the waves_partition check.
func TestWavesPartitionCheck(t *testing.T) {
	state := map[string]json.RawMessage{"slicer": json.RawMessage(`{"tickets":[
		{"id":"T1.1","files":["api/a.go","./api/shared.go"]},
		{"id":"T1.2","files":["web/c.ts","api/shared.go"]},
		{"id":"T2.1","files":["README.md"]},
		{"id":"T2.2"}]}`)}
	node := &Node{ID: "waves", Kind: KindAgent, InputFrom: []string{"start", "slicer"}, Checks: []string{CheckWavesPartition}}
	tests := []struct{ name, result, want string }{
		{"valid", `{"waves":[{"n":2,"tickets":["T1.2","T2.2"]},{"n":1,"tickets":["T1.1","T2.1"]}]}`, ""},
		{"one wave, no shared file", `{"waves":[{"n":1,"tickets":["T1.1","T2.1","T2.2"]},{"n":2,"tickets":["T1.2"]}]}`, ""},
		{"ticket in two waves", `{"waves":[{"n":1,"tickets":["T1.1","T2.1","T2.2"]},{"n":2,"tickets":["T1.2","T2.1"]}]}`, "T2.1 is in wave 1 and wave 2"},
		{"ticket twice in a wave", `{"waves":[{"n":1,"tickets":["T1.1","T1.1","T2.1","T2.2"]},{"n":2,"tickets":["T1.2"]}]}`, "T1.1 is in wave 1 and wave 1"},
		{"ticket missing", `{"waves":[{"n":1,"tickets":["T1.1","T2.1"]},{"n":2,"tickets":["T1.2"]}]}`, "not placed in any wave: T2.2"},
		{"unknown ticket", `{"waves":[{"n":1,"tickets":["T1.1","T2.1","T2.2","T9.9"]},{"n":2,"tickets":["T1.2"]}]}`, "T9.9, which is not in the ticket list"},
		{"gap in numbers", `{"waves":[{"n":1,"tickets":["T1.1","T2.1","T2.2"]},{"n":3,"tickets":["T1.2"]}]}`, "numbered 1 to 2 without gaps"},
		{"numbering from 0", `{"waves":[{"n":0,"tickets":["T1.1","T2.1","T2.2"]},{"n":1,"tickets":["T1.2"]}]}`, "numbered 1 to 2 without gaps"},
		{"repeated number", `{"waves":[{"n":1,"tickets":["T1.1","T2.1","T2.2"]},{"n":1,"tickets":["T1.2"]}]}`, "numbered 1 to 2 without gaps"},
		{"shared file (cleaned path)", `{"waves":[{"n":1,"tickets":["T1.1","T1.2"]},{"n":2,"tickets":["T2.1","T2.2"]}]}`, "T1.1 and T1.2 are both in wave 1 and both list api/shared.go"},
		{"not a list", `{"waves":"x"}`, "waves must be a list"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := node.RunCheck(CheckWavesPartition, state, json.RawMessage(tc.result))
			if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	if got := node.RunCheck(CheckWavesPartition, map[string]json.RawMessage{}, json.RawMessage(`{"waves":[]}`)); !strings.Contains(got, "no tickets") {
		t.Fatalf("without tickets: %q", got)
	}
}

// Checks are validated at save: unknown names, non-agent nodes and a waves_partition
// without input_from are refused.
func TestChecksValidatedAtSave(t *testing.T) {
	mk := func(n Node) error {
		d := &Definition{Nodes: []Node{{ID: "start", Kind: KindStart}, n, {ID: "end", Kind: KindEnd}},
			Edges: []Edge{{From: "start", To: n.ID}, {From: n.ID, To: "end"}}}
		return d.ValidateStructure()
	}
	agent := Node{ID: "waves", Kind: KindAgent, AgentName: "Planner", Task: "t", InputFrom: []string{"start"}}
	ok := agent
	ok.Checks = []string{CheckWavesPartition}
	if err := mk(ok); err != nil {
		t.Fatalf("valid: %v", err)
	}
	for name, tc := range map[string]struct {
		mut  func(*Node)
		want string
	}{
		"unknown":       {func(n *Node) { n.Checks = []string{"nope"} }, "nodes[1].checks[0]: unknown check 'nope'"},
		"no input_from": {func(n *Node) { n.Checks = []string{CheckWavesPartition}; n.InputFrom = nil }, "needs input_from"},
		"approval node": {func(n *Node) { n.Kind = KindApproval; n.Checks = []string{CheckWavesPartition} }, "only agent nodes have checks"},
	} {
		n := agent
		tc.mut(&n)
		if err := mk(n); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// Review L8 and L3: two tickets with one id are refused, and ids need no wave prefix.
func TestWavesPartitionCheckTicketIDs(t *testing.T) {
	node := &Node{ID: "waves", Kind: KindAgent, InputFrom: []string{"slicer"}, Checks: []string{CheckWavesPartition}}
	dup := map[string]json.RawMessage{"slicer": json.RawMessage(`{"tickets":[{"id":"T1"},{"id":"T2"},{"id":"T1"}]}`)}
	got := node.RunCheck(CheckWavesPartition, dup, json.RawMessage(`{"waves":[{"n":1,"tickets":["T1","T2"]}]}`))
	if !strings.Contains(got, "ticket id T1 is used by more than one ticket") {
		t.Fatalf("duplicate ids: %q", got)
	}
	plain := map[string]json.RawMessage{"slicer": json.RawMessage(`{"tickets":[{"id":"T1","files":["a"]},{"id":"T2","files":["a"]},{"id":"T3"}]}`)}
	if got := node.RunCheck(CheckWavesPartition, plain, json.RawMessage(`{"waves":[{"n":1,"tickets":["T1","T3"]},{"n":2,"tickets":["T2"]}]}`)); got != "" {
		t.Fatalf("ids without a wave prefix: %q", got)
	}
}

// The plan caps are applied before the plan is approved: a plan over them is sent back to
// the waves agent, so publish_tickets never refuses an approved plan.
func TestWavesPartitionCheckCaps(t *testing.T) {
	node := &Node{ID: "waves", Kind: KindAgent, InputFrom: []string{"slicer"}, Checks: []string{CheckWavesPartition}}
	plan := func(tickets, waves int) (map[string]json.RawMessage, json.RawMessage) {
		var ts, ws []string
		for i := 1; i <= tickets; i++ {
			ts = append(ts, fmt.Sprintf(`{"id":"T%d"}`, i))
		}
		per := map[int][]string{}
		for i := 1; i <= tickets; i++ {
			per[(i-1)%waves+1] = append(per[(i-1)%waves+1], fmt.Sprintf(`"T%d"`, i))
		}
		for n := 1; n <= waves; n++ {
			ws = append(ws, fmt.Sprintf(`{"n":%d,"tickets":[%s]}`, n, strings.Join(per[n], ",")))
		}
		return map[string]json.RawMessage{"slicer": json.RawMessage(`{"tickets":[` + strings.Join(ts, ",") + `]}`)},
			json.RawMessage(`{"waves":[` + strings.Join(ws, ",") + `]}`)
	}
	state, result := plan(28, 15)
	if got := node.RunCheck(CheckWavesPartition, state, result); got != "" {
		t.Fatalf("28 tickets in 15 waves: %q", got)
	}
	state, result = plan(MaxPlanWaves+1, MaxPlanWaves+1)
	if got := node.RunCheck(CheckWavesPartition, state, result); !strings.Contains(got, "at most 30") {
		t.Fatalf("too many waves: %q", got)
	}
	state, result = plan(MaxPlanTickets+1, 5)
	if got := node.RunCheck(CheckWavesPartition, state, result); !strings.Contains(got, "at most 100") {
		t.Fatalf("too many tickets: %q", got)
	}
}
