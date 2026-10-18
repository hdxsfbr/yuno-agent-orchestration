package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

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
	// The request's merge request, after the project check (DESIGN 5.22 rule 1).
	mrMsgs, mrFinal, err := e.requestedMergeRequest(ctx, req.MergeRequestID, proj, len(projMsgs) > 0)
	if err != nil {
		return nil, err
	}
	msgs = append(msgs, mrMsgs...)

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
		return nil, newValidationError(msgs, mrFinal)
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

// MergeRequestStateError is the ValidationError of a run request whose only fault is that
// the merge request it names has ended, merged or closed (DESIGN 5.22 rule 1, the
// fourth check). It is the typed marker the scheduler matches on, not the text: no later
// occurrence of a schedule on such a request can succeed, so the tick disables the
// schedule with "<Label> is <State>" in last_error (rule 7, D-049). It unwraps to its
// *ValidationError, so the API and every other caller treat it as the 422 it is. A
// request that has other faults too (a bad input, say) is a plain *ValidationError.
type MergeRequestStateError struct {
	*ValidationError
	Label string // "merge request !12" or "pull request #12"
	State string // "merged" or "closed"
}

// Unwrap makes errors.As find the *ValidationError.
func (e *MergeRequestStateError) Unwrap() error { return e.ValidationError }

// newValidationError is the 422 for msgs; it is a *MergeRequestStateError when the one
// message is the final-state refusal of rule 1 (final non-nil), so a caller can tell
// that refusal apart without reading the text.
func newValidationError(msgs []string, final *MergeRequestStateError) error {
	if final != nil && len(msgs) == 1 && msgs[0] == final.Messages[0] {
		return final
	}
	return &ValidationError{Messages: msgs}
}

// requestedMergeRequest runs the merge request checks of DESIGN 5.22 rule 1 for a trigger
// on the engine's pool. A project check that already failed (projectFailed) leaves the
// merge request unchecked: "the workflow does not use a project" would only repeat it.
func (e *Engine) requestedMergeRequest(ctx context.Context, id string, proj *store.Project, projectFailed bool) ([]string, *MergeRequestStateError, error) {
	if projectFailed {
		return nil, nil, nil
	}
	_, msgs, final, err := checkMergeRequest(ctx, e.deps.Pool, id, proj)
	return msgs, final, err
}

// MergeRequestForRequest resolves the merge request a run request or a schedule names
// (DESIGN 5.22 rule 1): the record of the project the run works on, open, and not being
// updated. An empty id names none and answers nil. Otherwise the checks run in this order
// and the first that fails answers its message, path-qualified like ProjectForRequest's
// (the schedule handlers check a schedule's request with it when it is saved, so a
// schedule is refused as its run would be, rule 7):
//
//   - proj is nil: "merge_request_id: the workflow does not use a project";
//   - no record with that id: "merge_request_id: no merge request with id <id>";
//   - the record is another project's: "merge_request_id: <merge request !n|pull request
//     #n> belongs to project <name>";
//   - the record is not open: "merge_request_id: <label> is <state>";
//   - the run that opened the record is live: "merge_request_id: <label> is being
//     updated by run <id>".
//
// The record is returned only when every check passed. Another live run that names the
// record is not read here: runs_merge_request_live_idx refuses its insert and queueRun
// maps that to the same last text, so two concurrent requests cannot both pass.
func MergeRequestForRequest(ctx context.Context, q store.Querier, id string, proj *store.Project) (*store.MergeRequest, []string, error) {
	mr, msgs, _, err := checkMergeRequest(ctx, q, id, proj)
	return mr, msgs, err
}

// checkMergeRequest is MergeRequestForRequest, plus the typed marker for the final-state
// refusal (non-nil exactly when the one message is that refusal).
func checkMergeRequest(ctx context.Context, q store.Querier, id string, proj *store.Project) (*store.MergeRequest, []string, *MergeRequestStateError, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil, nil, nil
	}
	if proj == nil {
		return nil, []string{"merge_request_id: the workflow does not use a project"}, nil, nil
	}
	// An id with a control character is no record's, and Postgres refuses a NUL in a
	// parameter: it is an unknown id like any other (review L1).
	if strings.ContainsFunc(id, unicode.IsControl) {
		return nil, []string{"merge_request_id: no merge request with id " + id}, nil, nil
	}
	mr, err := store.GetMergeRequest(ctx, q, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, []string{"merge_request_id: no merge request with id " + id}, nil, nil
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("merge request %s: %w", id, err)
	}
	if mr.ProjectID != proj.ID {
		// The record's project may be deleted since: its name is read either way.
		name, err := store.ProjectNameAny(ctx, q, mr.ProjectID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("merge request %s: project %s: %w", id, mr.ProjectID, err)
		}
		return nil, []string{"merge_request_id: " + mr.Label() + " belongs to project " + name}, nil, nil
	}
	if mr.State != store.MergeRequestOpen {
		msg := "merge_request_id: " + mr.Label() + " is " + mr.State
		return nil, []string{msg}, &MergeRequestStateError{ValidationError: &ValidationError{Messages: []string{msg}},
			Label: mr.Label(), State: mr.State}, nil
	}
	// The run that opened the request must have ended (rule 1): until it does, it may
	// still push to the branch.
	opener, err := store.GetRun(ctx, q, mr.RunID)
	switch {
	case err == nil && !store.IsTerminal(opener.Status):
		return nil, []string{"merge_request_id: " + mr.Label() + " is being updated by run " + opener.ID}, nil, nil
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return nil, nil, nil, fmt.Errorf("merge request %s: run %s: %w", id, mr.RunID, err)
	}
	return mr, nil, nil, nil
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
