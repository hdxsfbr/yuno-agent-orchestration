package evals

import (
	"encoding/json"
	"reflect"
	"strings"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// platformPrefix is the server prefix OpenClaw puts on platform tool names. Our tables
// and API carry the plain name, but a trace row may still hold the prefixed one.
const platformPrefix = "yuno__"

// finishTool is the one platform tool every agent can call whatever its allowlist says
// (the materialiser always grants it).
const finishTool = "finish"

// sideEffectingTools are the platform tools of risk side_effecting (DESIGN 5.7). A row
// with an idempotency key is also treated as one, whatever its name.
var sideEffectingTools = map[string]bool{"deploy_release": true, "gitlab_create_mr": true}

// toolBase strips the platform prefix: "yuno__finish" is "finish".
func toolBase(name string) string { return strings.TrimPrefix(name, platformPrefix) }

func isSideEffecting(tc store.ToolCall) bool {
	if sideEffectingTools[toolBase(tc.ToolName)] {
		return true
	}
	return tc.IdempotencyKey != nil && *tc.IdempotencyKey != ""
}

// wasReplayed reports whether a tool call returned a stored side-effect result
// (output.replayed = true, DESIGN 5.4 rule 2).
func wasReplayed(tc store.ToolCall) bool {
	var out struct {
		Replayed bool `json:"replayed"`
	}
	if len(tc.Output) == 0 || json.Unmarshal(tc.Output, &out) != nil {
		return false
	}
	return out.Replayed
}

// canonicalJSON re-encodes a JSON document with sorted keys and no whitespace, so two
// inputs that differ only in key order or spacing compare equal.
func canonicalJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "null"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(out)
}

// agentResolver finds the case agent that ran a tool call: by run node, then workflow
// node, then agent name for a workflow case; the case's one agent for a quick run.
type agentResolver struct {
	c      Case
	def    *workflow.Definition
	byNode map[string]string // run node id -> workflow node id
}

func newAgentResolver(c Case, t store.Trace) agentResolver {
	r := agentResolver{c: c, byNode: map[string]string{}}
	if c.IsWorkflow() {
		r.def, _ = c.Definition()
	}
	for _, n := range t.Nodes {
		r.byNode[n.ID] = n.NodeID
	}
	return r
}

// forCall returns the agent's spec and label. ok is false when the call cannot be tied
// to an agent of the case.
func (r agentResolver) forCall(tc store.ToolCall) (spec AgentSpec, label string, ok bool) {
	if !r.c.IsWorkflow() {
		return r.c.Agent, "agent", true
	}
	if r.def == nil {
		return AgentSpec{}, "", false
	}
	nodeID, found := r.byNode[tc.RunNodeID]
	if !found {
		return AgentSpec{}, "", false
	}
	n, found := r.def.Node(nodeID)
	if !found || n.Kind != workflow.KindAgent {
		return AgentSpec{}, "", false
	}
	spec, ok = r.c.Agents[n.AgentName]
	return spec, n.AgentName, ok
}

// jsonEqual compares two values as JSON, so a YAML int equals a decoded float64.
func jsonEqual(a, b any) bool {
	na, okA := normalizeJSON(a)
	nb, okB := normalizeJSON(b)
	return okA && okB && reflect.DeepEqual(na, nb)
}

func normalizeJSON(v any) (any, bool) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false
	}
	return out, true
}

// compactJSON renders a value for a message.
func compactJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	return string(raw)
}
