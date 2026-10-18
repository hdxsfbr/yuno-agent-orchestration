package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"yuno/internal/guardrails"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// triggerWorkflow validates and queues a run of a stored workflow (DESIGN 5.6
// consistency: the definition is validated again at trigger time). Problems with the
// definition or the input are a *ValidationError with path-qualified messages; an
// unknown workflow wraps store.ErrNotFound.
func (e *Engine) triggerWorkflow(ctx context.Context, req TriggerRequest) (*store.Run, error) {
	wf, err := store.GetWorkflow(ctx, e.deps.Pool, req.WorkflowID)
	if err != nil {
		return nil, fmt.Errorf("workflow %s: %w", req.WorkflowID, err)
	}
	def, err := workflow.Parse(wf.Definition)
	if err != nil {
		return nil, &ValidationError{Messages: []string{"definition: " + err.Error()}}
	}
	var msgs []string
	var we *workflow.ValidationError
	if err := def.ValidateStructure(); errors.As(err, &we) {
		msgs = append(msgs, we.Messages...)
	} else if err != nil {
		return nil, err
	}
	agentMsgs, err := e.checkAgentNodes(ctx, def)
	if err != nil {
		return nil, err
	}
	msgs = append(msgs, agentMsgs...)
	// A workflow that opts in names its project on every request (DESIGN 5.17 rule 4).
	proj, projMsgs, err := e.projectForRequest(ctx, req.ProjectID, def.Project)
	if err != nil {
		return nil, err
	}
	msgs = append(msgs, projMsgs...)

	input := req.Input
	if t := bytes.TrimSpace(input); len(t) == 0 || string(t) == "null" {
		input = json.RawMessage(`{}`)
	}
	msgs = append(msgs, ValidateInput(def.InputSchema, input)...)
	if len(msgs) == 0 {
		// Templates render against the actual input and an empty state; only a
		// definition that is otherwise sound gets this far.
		msgs = append(msgs, def.ValidateTemplates(input)...)
	}
	if len(msgs) > 0 {
		return nil, &ValidationError{Messages: msgs}
	}
	limits, err := resolveLayeredLimits("workflow limits.", req.Limits, def.Limits, e.deps.Cfg)
	if err != nil {
		return nil, err
	}
	// Guardrails (DESIGN 5.11 rules 1 and 2): every participating agent's per-run cost
	// cap lowers the run's, and their hourly rates are checked as the run is stored.
	agents, err := guardrails.ParticipatingAgents(ctx, e.deps.Pool, def)
	if err != nil {
		return nil, err
	}
	def.Limits = guardrails.EffectiveLimits(limits, agents, e.deps.Cfg.MaxCostUSD)
	return e.queueRun(ctx, wf, def, input, req, agents, proj)
}

// checkAgentNodes requires every agent node to name an existing agent by id (a
// template that still names agents by agent_name must be instantiated first) and to
// carry an output schema that compiles.
func (e *Engine) checkAgentNodes(ctx context.Context, def *workflow.Definition) ([]string, error) {
	var msgs []string
	known := map[string]bool{}
	for i, n := range def.Nodes {
		if n.Kind != workflow.KindAgent {
			continue
		}
		if _, err := compileSchema(n.OutputSchema); err != nil {
			msgs = append(msgs, fmt.Sprintf("nodes[%d].output_schema: %v", i, strings.TrimPrefix(err.Error(), "output schema: ")))
		}
		if n.AgentID == "" {
			if n.AgentName != "" {
				msgs = append(msgs, fmt.Sprintf("nodes[%d].agent_id: template must be instantiated", i))
			}
			continue // ValidateStructure reported a node with neither
		}
		ok, seen := known[n.AgentID]
		if !seen {
			_, err := store.GetAgent(ctx, e.deps.Pool, n.AgentID)
			switch {
			case err == nil:
				ok = true
			case errors.Is(err, store.ErrNotFound):
			default:
				return nil, fmt.Errorf("agent %s: %w", n.AgentID, err)
			}
			known[n.AgentID] = ok
		}
		if !ok {
			msgs = append(msgs, fmt.Sprintf("nodes[%d].agent_id: no agent with id '%s'", i, n.AgentID))
		}
	}
	return msgs, nil
}

// ValidateInput checks the run input against the workflow's input_schema (DESIGN 5.6
// rule 11): one path-qualified message per violated leaf, e.g. "input.task: ...".
func ValidateInput(schema, input json.RawMessage) []string {
	if t := bytes.TrimSpace(schema); len(t) == 0 || string(t) == "null" {
		return nil
	}
	compiled, err := compileSchema(schema)
	if err != nil {
		return []string{"input_schema: " + strings.TrimPrefix(err.Error(), "output schema: ")}
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(input))
	if err != nil {
		return []string{"input: must be valid JSON"}
	}
	err = compiled.Validate(inst)
	if err == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []string{"input: " + err.Error()}
	}
	var msgs []string
	var walk func(v *jsonschema.ValidationError)
	walk = func(v *jsonschema.ValidationError) {
		if len(v.Causes) == 0 {
			path := "input"
			for _, p := range v.InstanceLocation {
				path += "." + p
			}
			msgs = append(msgs, path+": "+v.ErrorKind.LocalizedString(schemaPrinter))
			return
		}
		for _, c := range v.Causes {
			walk(c)
		}
	}
	walk(ve)
	return msgs
}
