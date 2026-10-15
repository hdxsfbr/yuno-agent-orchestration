package evals

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"yuno/internal/workflow"
)

// errNotWorkflow is returned by Definition for a quick-run case.
var errNotWorkflow = errors.New("case has no workflow")

// Definition decodes the case's inline workflow. Unknown fields are errors, so a typo in
// a case fails at load time instead of being silently ignored by the api.
func (c Case) Definition() (*workflow.Definition, error) {
	if c.Workflow == nil {
		return nil, errNotWorkflow
	}
	raw, err := json.Marshal(c.Workflow)
	if err != nil {
		return nil, fmt.Errorf("workflow is not JSON-encodable: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var d workflow.Definition
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("workflow: %w", err)
	}
	return &d, nil
}

// WorkflowBody returns the definition as the api takes it: every agent node's agent_name
// replaced by the id of the agent created for that name. A name without an id is an
// error, so the runner never posts a half-resolved definition.
func (c Case) WorkflowBody(agentIDs map[string]string) (json.RawMessage, error) {
	def, err := c.Definition()
	if err != nil {
		return nil, err
	}
	for i := range def.Nodes {
		n := &def.Nodes[i]
		if n.Kind != workflow.KindAgent {
			continue
		}
		id, ok := agentIDs[n.AgentName]
		if !ok || id == "" {
			return nil, fmt.Errorf("nodes[%d] (%s): no agent was created for %q", i, n.ID, n.AgentName)
		}
		n.AgentID, n.AgentName = id, ""
	}
	return json.Marshal(def)
}

// InputJSON returns the workflow input as a JSON object ("{}" when the case has none).
func (c Case) InputJSON() (json.RawMessage, error) {
	if len(c.Input) == 0 {
		return json.RawMessage(`{}`), nil
	}
	raw, err := json.Marshal(c.Input)
	if err != nil {
		return nil, fmt.Errorf("input is not JSON-encodable: %w", err)
	}
	return raw, nil
}

// AgentKeys returns the keys of the agents map in a stable order.
func (c Case) AgentKeys() []string {
	keys := make([]string, 0, len(c.Agents))
	for k := range c.Agents {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// checkWorkflow validates the inline definition and everything that ties it to the
// case: agent names, schemas, templates and the input.
func (c Case) checkWorkflow(add func(string, ...any)) {
	def, err := c.Definition()
	if err != nil {
		add("%v", err)
		return
	}
	var ve *workflow.ValidationError
	if err := def.ValidateStructure(); errors.As(err, &ve) {
		for _, m := range ve.Messages {
			add("workflow %s", m)
		}
	}

	used := map[string]bool{}
	for i, n := range def.Nodes {
		if n.Kind != workflow.KindAgent {
			continue
		}
		if n.AgentID != "" {
			add("workflow nodes[%d].agent_id: a case names its agents with agent_name", i)
		}
		if _, ok := c.Agents[n.AgentName]; !ok && n.AgentName != "" {
			add("workflow nodes[%d].agent_name: no agent %q in agents", i, n.AgentName)
		}
		used[n.AgentName] = true
		if hasSchema(n.OutputSchema) {
			if _, err := compileSchema(n.OutputSchema); err != nil {
				add("workflow nodes[%d].output_schema: %v", i, err)
			}
		}
	}
	// The api renders every task and message at save time with the engine's own
	// renderer; so does the case, against its real input.
	if in, err := c.InputJSON(); err == nil {
		for _, m := range def.ValidateTemplates(in) {
			add("workflow %s", m)
		}
	}
	for _, key := range c.AgentKeys() {
		if !used[key] {
			add("agents.%s is not used by any workflow node", key)
		}
	}

	if hasSchema(def.StateSchema) {
		if _, err := compileSchema(def.StateSchema); err != nil {
			add("workflow state_schema: %v", err)
		}
	}
	if hasSchema(def.InputSchema) {
		s, err := compileSchema(def.InputSchema)
		switch {
		case err != nil:
			add("workflow input_schema: %v", err)
		default:
			in, jerr := c.InputJSON()
			if jerr != nil {
				add("%v", jerr)
			} else if msg := validateDoc(s, in); msg != "" {
				add("input does not match the workflow's input_schema: %s", msg)
			}
		}
	}
}
