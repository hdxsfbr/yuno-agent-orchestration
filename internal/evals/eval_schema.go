package evals

import (
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// traceDefinition is the workflow definition the run was judged against: the snapshot
// the api stored with the run when the trace carries one (it is what actually ran),
// else the case's inline workflow, else the one-node workflow behind a quick run.
func traceDefinition(c Case, t store.Trace) *workflow.Definition {
	if t.Run != nil && hasSchema(t.Run.WorkflowSnapshot) {
		if d, err := workflow.Parse(t.Run.WorkflowSnapshot); err == nil && len(d.Nodes) > 0 {
			return d
		}
	}
	if c.IsWorkflow() {
		if d, err := c.Definition(); err == nil {
			return d
		}
	}
	return workflow.SingleAgent("", "", nil, store.Limits{})
}

// evalSchema checks the typed state of the run: the output of every node that succeeded
// validates against that node's output_schema, and the final state validates against the
// workflow's state_schema when it has one (DESIGN 5.6 rules 6, 7 and 11).
func evalSchema(c Case, t store.Trace) EvalResult {
	res := EvalResult{Name: EvalSchema}
	if t.Run == nil {
		res.Detail = "trace has no run"
		return res
	}
	def := traceDefinition(c, t)

	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	compiled := map[string]*jsonschema.Schema{}
	outputs := 0
	for _, n := range t.Nodes {
		if n.Status != store.NodeSucceeded {
			continue
		}
		dn, ok := def.Node(n.NodeID)
		if !ok || dn.Kind != workflow.KindAgent || !hasSchema(dn.OutputSchema) {
			continue
		}
		s, ok := compiled[n.NodeID]
		if !ok {
			var err error
			if s, err = compileSchema(dn.OutputSchema); err != nil {
				add("node %s: output_schema does not compile: %v", n.NodeID, err)
				continue
			}
			compiled[n.NodeID] = s
		}
		if !hasSchema(n.Output) {
			add("node %s (visit %d) succeeded without an output", n.NodeID, n.Visit)
			continue
		}
		if msg := validateDoc(s, n.Output); msg != "" {
			add("node %s (visit %d) output does not match its output_schema: %s", n.NodeID, n.Visit, msg)
			continue
		}
		outputs++
	}

	stateChecked := false
	if hasSchema(def.StateSchema) {
		s, err := compileSchema(def.StateSchema)
		switch {
		case err != nil:
			add("state_schema does not compile: %v", err)
		default:
			state := t.Run.State
			if len(state) == 0 {
				state = []byte(`{}`)
			}
			if msg := validateDoc(s, state); msg != "" {
				add("final state does not match the state_schema: %s", msg)
			} else {
				stateChecked = true
			}
		}
	}

	if len(problems) > 0 {
		res.Detail = strings.Join(problems, "; ")
		return res
	}
	res.Passed = true
	res.Detail = fmt.Sprintf("%d node output(s) match their output_schema", outputs)
	if stateChecked {
		res.Detail += ", the final state matches the state_schema"
	}
	return res
}
