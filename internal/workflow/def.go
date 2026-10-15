// Package workflow holds the graph definition (DESIGN 5.6): nodes, edges, conditions,
// typed state, structural validation, condition evaluation and task rendering.
package workflow

import (
	"encoding/json"
	"fmt"
	"regexp"

	"yuno/internal/store"
)

const (
	KindStart    = "start"
	KindAgent    = "agent"
	KindApproval = "approval"
	KindEnd      = "end"
)

var nodeIDPattern = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// StatePendingEffects is the runs.state key where the engine records side effects
// executed after their approval outlived the tool call (DESIGN 5.3 rule 5). Node
// outputs live next to it under their node ids, so no node may take this id.
const StatePendingEffects = "pending_effects"

type Node struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	AgentID string `json:"agent_id,omitempty"`
	// AgentName is how a template names its agent before it is instantiated; the
	// instantiate endpoint resolves it to AgentID (DESIGN 5.6 rule 9). A runnable
	// workflow always has AgentID.
	AgentName    string          `json:"agent_name,omitempty"`
	Task         string          `json:"task,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	InputFrom    []string        `json:"input_from,omitempty"`
	Message      string          `json:"message,omitempty"`   // approval nodes
	OnReject     string          `json:"on_reject,omitempty"` // approval nodes: edge target
	OnFailure    string          `json:"on_failure,omitempty"`
}

// Condition is {path, op, value} or {all: []} / {any: []} (DESIGN 5.6 rule 3).
type Condition struct {
	Path  string          `json:"path,omitempty"`
	Op    string          `json:"op,omitempty"` // eq | neq | in | exists | gt | lt
	Value json.RawMessage `json:"value,omitempty"`
	All   []Condition     `json:"all,omitempty"`
	Any   []Condition     `json:"any,omitempty"`
}

type Edge struct {
	From string     `json:"from"`
	To   string     `json:"to"`
	When *Condition `json:"when,omitempty"`
}

type Definition struct {
	StateSchema json.RawMessage `json:"state_schema,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Nodes       []Node          `json:"nodes"`
	Edges       []Edge          `json:"edges"`
	Limits      store.Limits    `json:"limits"`
}

// Parse decodes a definition and runs the structural checks that need no database.
func Parse(raw json.RawMessage) (*Definition, error) {
	var d Definition
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("definition: %w", err)
	}
	return &d, nil
}

// Node returns the node with the given id.
func (d *Definition) Node(id string) (*Node, bool) {
	for i := range d.Nodes {
		if d.Nodes[i].ID == id {
			return &d.Nodes[i], true
		}
	}
	return nil, false
}

// Start returns the single start node id.
func (d *Definition) Start() (string, bool) {
	for _, n := range d.Nodes {
		if n.Kind == KindStart {
			return n.ID, true
		}
	}
	return "", false
}

// OutgoingEdges returns the edges from a node in definition order (DESIGN 5.6 rule 4).
func (d *Definition) OutgoingEdges(from string) []Edge {
	var out []Edge
	for _, e := range d.Edges {
		if e.From == from {
			out = append(out, e)
		}
	}
	return out
}

// ValidationError lists path-qualified messages (DESIGN 5.6 rule 2).
type ValidationError struct {
	Messages []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%d validation error(s): %v", len(e.Messages), e.Messages)
}

// ValidateStructure checks the rules of DESIGN 5.6 rules 1 and 2 that do not need the
// database (agent existence is checked by the caller).
func (d *Definition) ValidateStructure() error {
	var msgs []string
	ids := map[string]bool{}
	starts, ends := 0, 0
	for i, n := range d.Nodes {
		if !nodeIDPattern.MatchString(n.ID) {
			msgs = append(msgs, fmt.Sprintf("nodes[%d].id: must match %s", i, nodeIDPattern.String()))
		}
		if ids[n.ID] {
			msgs = append(msgs, fmt.Sprintf("nodes[%d].id: duplicate '%s'", i, n.ID))
		}
		if n.ID == StatePendingEffects {
			msgs = append(msgs, fmt.Sprintf("nodes[%d].id: '%s' is reserved", i, n.ID))
		}
		ids[n.ID] = true
		switch n.Kind {
		case KindStart:
			starts++
		case KindEnd:
			ends++
		case KindAgent:
			if n.AgentID == "" && n.AgentName == "" {
				msgs = append(msgs, fmt.Sprintf("nodes[%d].agent_id: required for agent nodes", i))
			}
			if n.Task == "" {
				msgs = append(msgs, fmt.Sprintf("nodes[%d].task: required for agent nodes", i))
			}
		case KindApproval:
		default:
			msgs = append(msgs, fmt.Sprintf("nodes[%d].kind: unknown kind '%s'", i, n.Kind))
		}
	}
	if starts != 1 {
		msgs = append(msgs, fmt.Sprintf("nodes: exactly one start node required, found %d", starts))
	}
	if ends < 1 {
		msgs = append(msgs, "nodes: at least one end node required")
	}
	outgoing := map[string]int{}
	for i, e := range d.Edges {
		if !ids[e.From] {
			msgs = append(msgs, fmt.Sprintf("edges[%d].from: unknown node '%s'", i, e.From))
		}
		if !ids[e.To] {
			msgs = append(msgs, fmt.Sprintf("edges[%d].to: unknown node '%s'", i, e.To))
		}
		if n, ok := d.Node(e.To); ok && n.Kind == KindStart {
			msgs = append(msgs, fmt.Sprintf("edges[%d].to: the start node cannot be an edge target", i))
		}
		outgoing[e.From]++
		msgs = append(msgs, validateCondition(fmt.Sprintf("edges[%d].when", i), e.When)...)
	}
	for i, n := range d.Nodes {
		if n.Kind != KindEnd && outgoing[n.ID] == 0 {
			msgs = append(msgs, fmt.Sprintf("nodes[%d]: '%s' has no outgoing edge", i, n.ID))
		}
		if n.OnReject != "" && !ids[n.OnReject] {
			msgs = append(msgs, fmt.Sprintf("nodes[%d].on_reject: unknown node '%s'", i, n.OnReject))
		}
		if n.OnReject != "" && n.Kind != KindApproval {
			msgs = append(msgs, fmt.Sprintf("nodes[%d].on_reject: only approval nodes have one", i))
		}
		if n.OnFailure != "" && !ids[n.OnFailure] {
			msgs = append(msgs, fmt.Sprintf("nodes[%d].on_failure: unknown node '%s'", i, n.OnFailure))
		}
		for j, from := range n.InputFrom {
			if !ids[from] {
				msgs = append(msgs, fmt.Sprintf("nodes[%d].input_from[%d]: unknown node '%s'", i, j, from))
			}
		}
	}
	// Reachability from start, through edges and the on_reject / on_failure targets.
	if start, ok := d.Start(); ok {
		seen := map[string]bool{start: true}
		stack := []string{start}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			next := []string{}
			for _, e := range d.OutgoingEdges(cur) {
				next = append(next, e.To)
			}
			if n, ok := d.Node(cur); ok {
				next = append(next, n.OnReject, n.OnFailure)
			}
			for _, to := range next {
				if to != "" && !seen[to] {
					seen[to] = true
					stack = append(stack, to)
				}
			}
		}
		for i, n := range d.Nodes {
			if !seen[n.ID] {
				msgs = append(msgs, fmt.Sprintf("nodes[%d]: '%s' is not reachable from start", i, n.ID))
			}
		}
	}
	if len(msgs) > 0 {
		return &ValidationError{Messages: msgs}
	}
	return nil
}

// SingleAgent builds the one-node workflow behind "quick run" on the agent page
// (DESIGN section 9, wave 1) and behind conversation runs. Its task template already
// carries the input, so it has no input_from.
func SingleAgent(agentID, task string, outputSchema json.RawMessage, limits store.Limits) *Definition {
	if len(outputSchema) == 0 {
		outputSchema = json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"],"additionalProperties":true}`)
	}
	return &Definition{
		Nodes: []Node{
			{ID: "start", Kind: KindStart},
			{ID: "agent", Kind: KindAgent, AgentID: agentID, Task: task, OutputSchema: outputSchema},
			{ID: "end", Kind: KindEnd},
		},
		Edges:  []Edge{{From: "start", To: "agent"}, {From: "agent", To: "end"}},
		Limits: limits,
	}
}
