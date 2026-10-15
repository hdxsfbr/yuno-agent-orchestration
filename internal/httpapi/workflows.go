package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

const (
	maxWorkflowBody = 1 << 20
	maxWorkflowName = 120
)

// workflowInput is WorkflowInput. The Workflow read-only fields are accepted and
// ignored so a client can PUT back what it fetched.
type workflowInput struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Definition  json.RawMessage `json:"definition"`

	ID          json.RawMessage `json:"id"`
	Version     json.RawMessage `json:"version"`
	TemplateKey json.RawMessage `json:"template_key"`
	CreatedAt   json.RawMessage `json:"created_at"`
	UpdatedAt   json.RawMessage `json:"updated_at"`
	DeletedAt   json.RawMessage `json:"deleted_at"`
}

type validateRequest struct {
	Definition json.RawMessage `json:"definition"`
}

func (s *Server) listWorkflows(w http.ResponseWriter, r *http.Request) {
	all, err := store.ListWorkflows(r.Context(), s.pool)
	if err != nil {
		s.internalError(w, r, "list workflows", err)
		return
	}
	// Templates (listed under /workflow-templates) and the hidden quick-run wrappers
	// carry a template_key; the list is the operator's own workflows.
	out := make([]store.Workflow, 0, len(all))
	for _, wf := range all {
		if wf.TemplateKey == nil {
			out = append(out, wf)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, err := store.GetWorkflow(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "workflow", err)
		return
	}
	writeJSON(w, http.StatusOK, wf)
}

func (s *Server) createWorkflow(w http.ResponseWriter, r *http.Request) {
	var in workflowInput
	if !decodeBody(w, r, &in, maxWorkflowBody) {
		return
	}
	fe, err := s.checkWorkflowInput(r.Context(), &in)
	if err != nil {
		s.internalError(w, r, "validate workflow", err)
		return
	}
	if len(fe) > 0 {
		writeValidation(w, "invalid workflow", fe)
		return
	}
	wf := &store.Workflow{Name: strings.TrimSpace(in.Name), Description: in.Description, Definition: in.Definition}
	if err := store.CreateWorkflow(r.Context(), s.pool, wf); err != nil {
		s.internalError(w, r, "create workflow", err)
		return
	}
	s.respondWorkflow(w, r, wf.ID, http.StatusCreated)
}

// updateWorkflow replaces name, description and definition and bumps the version;
// runs keep the version they started with.
func (s *Server) updateWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, ok := s.editableWorkflow(w, r)
	if !ok {
		return
	}
	var in workflowInput
	if !decodeBody(w, r, &in, maxWorkflowBody) {
		return
	}
	fe, err := s.checkWorkflowInput(r.Context(), &in)
	if err != nil {
		s.internalError(w, r, "validate workflow", err)
		return
	}
	if len(fe) > 0 {
		writeValidation(w, "invalid workflow", fe)
		return
	}
	wf.Name, wf.Description, wf.Definition = strings.TrimSpace(in.Name), in.Description, in.Definition
	if err := store.UpdateWorkflow(r.Context(), s.pool, wf); err != nil {
		s.storeError(w, r, "workflow", err)
		return
	}
	s.respondWorkflow(w, r, wf.ID, http.StatusOK)
}

// deleteWorkflow soft-deletes: runs keep their snapshot.
func (s *Server) deleteWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, ok := s.editableWorkflow(w, r)
	if !ok {
		return
	}
	if err := store.DeleteWorkflow(r.Context(), s.pool, wf.ID); err != nil {
		s.storeError(w, r, "workflow", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// editableWorkflow loads a workflow the operator may change: templates are files in
// the repository and quick-run wrappers belong to their agent, so both answer 409.
func (s *Server) editableWorkflow(w http.ResponseWriter, r *http.Request) (*store.Workflow, bool) {
	wf, err := store.GetWorkflow(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "workflow", err)
		return nil, false
	}
	if wf.TemplateKey != nil {
		writeError(w, http.StatusConflict, "read_only", "this workflow is managed by the platform; instantiate a template to get an editable copy", nil)
		return nil, false
	}
	return wf, true
}

func (s *Server) respondWorkflow(w http.ResponseWriter, r *http.Request, id string, status int) {
	wf, err := store.GetWorkflow(r.Context(), s.pool, id)
	if err != nil {
		s.storeError(w, r, "workflow", err)
		return
	}
	writeJSON(w, status, wf)
}

// validateWorkflow checks a definition without saving it: 200 {valid: true} or 422
// with one detail per problem (DESIGN 5.6 rules 1, 2, 5).
func (s *Server) validateWorkflow(w http.ResponseWriter, r *http.Request) {
	var in validateRequest
	if !decodeBody(w, r, &in, maxWorkflowBody) {
		return
	}
	fe, err := s.checkDefinition(r.Context(), in.Definition, definitionRules{agents: true})
	if err != nil {
		s.internalError(w, r, "validate workflow", err)
		return
	}
	if len(fe) > 0 {
		writeValidation(w, "invalid workflow", fe)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"valid": true})
}

func (s *Server) checkWorkflowInput(ctx context.Context, in *workflowInput) (fieldErrors, error) {
	var fe fieldErrors
	switch n := utf8.RuneCountInString(in.Name); {
	case strings.TrimSpace(in.Name) == "":
		fe.add("name", "is required")
	case n > maxWorkflowName:
		fe.add("name", "must be at most %d characters", maxWorkflowName)
	}
	defErrs, err := s.checkDefinition(ctx, in.Definition, definitionRules{agents: true})
	return append(fe, defErrs...), err
}

// definitionRules selects the checks beyond the structural ones.
type definitionRules struct {
	// agents: agent nodes must name an existing agent by agent_id; agent_name is only
	// for template rows (DESIGN 5.6 rule 9). Off for templates, which name agents.
	agents bool
}

// checkDefinition runs every save-time check (DESIGN 5.6 rules 1, 2 and 5): the
// structural rules, agents, templates rendered against an empty input and state, and
// that every schema compiles. Paths are relative to the definition (nodes[1].task).
func (s *Server) checkDefinition(ctx context.Context, raw json.RawMessage, rules definitionRules) (fieldErrors, error) {
	var fe fieldErrors
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		fe.add("definition", "must be an object")
		return fe, nil
	}
	def, err := workflow.Parse(raw)
	if err != nil {
		fe.add("definition", "%s", strings.TrimPrefix(err.Error(), "definition: "))
		return fe, nil
	}
	if err := def.ValidateStructure(); err != nil {
		var ve *workflow.ValidationError
		if !errors.As(err, &ve) {
			return nil, err
		}
		for _, m := range ve.Messages {
			fe = append(fe, splitMessage(m))
		}
	}
	checkSchema(&fe, "input_schema", def.InputSchema)
	checkSchema(&fe, "state_schema", def.StateSchema)
	for i, n := range def.Nodes {
		p := fmt.Sprintf("nodes[%d]", i)
		switch n.Kind {
		case workflow.KindAgent:
			if rules.agents {
				if n.AgentName != "" {
					fe.add(p+".agent_name", "only templates name agents; choose an agent")
				}
				if n.AgentID != "" {
					if _, err := store.GetAgent(ctx, s.pool, n.AgentID); errors.Is(err, store.ErrNotFound) {
						fe.add(p+".agent_id", "no agent with id '%s'", n.AgentID)
					} else if err != nil {
						return nil, err
					}
				}
			}
			checkSchema(&fe, p+".output_schema", n.OutputSchema)
		}
	}
	// Rendered with the runtime's own renderer against an empty input and state: a key
	// the run fills in later renders empty, so only real template errors surface.
	for _, m := range def.ValidateTemplates(nil) {
		fe = append(fe, templateDetail(m))
	}
	l := def.Limits
	for _, lim := range []struct {
		name string
		v    float64
	}{
		{"max_steps", float64(l.MaxSteps)}, {"max_duration_seconds", float64(l.MaxDurationSeconds)},
		{"max_node_iterations", float64(l.MaxNodeIterations)}, {"max_loop_visits", float64(l.MaxLoopVisits)},
		{"max_cost_usd", l.MaxCostUSD}, {"max_tool_calls", float64(l.MaxToolCalls)},
		{"approval_timeout_seconds", float64(l.ApprovalTimeoutSeconds)},
	} {
		if lim.v < 0 {
			fe.add("limits."+lim.name, "must not be negative")
		}
	}
	return fe, nil
}

// splitMessage turns a "path: message" line into a detail.
func splitMessage(m string) fieldError {
	path, msg, ok := strings.Cut(m, ": ")
	if !ok {
		return fieldError{Path: "", Message: m}
	}
	return fieldError{Path: path, Message: msg}
}

// templateErr picks the line and message out of a text/template error
// ("template: task:3:7: executing \"task\" at <...>: ...").
var templateErr = regexp.MustCompile(`^template: [^:]*:(\d+)(?::\d+)?: (.*)$`)

// templateDetail is a workflow.ValidateTemplates line as a detail with a readable
// message ("line 3: function \"x\" not defined").
func templateDetail(m string) fieldError {
	fe := splitMessage(m)
	if sub := templateErr.FindStringSubmatch(fe.Message); sub != nil {
		fe.Message = "line " + sub[1] + ": " + sub[2]
	}
	return fe
}

// checkSchema compiles a JSON Schema; absent or null is fine.
func checkSchema(fe *fieldErrors, path string, raw json.RawMessage) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(t))
	if err == nil {
		loc := "https://yuno.local/workflow/" + strings.NewReplacer("[", "_", "]", "_", ".", "_").Replace(path) + ".json"
		c := jsonschema.NewCompiler()
		if err = c.AddResource(loc, doc); err == nil {
			_, err = c.Compile(loc)
		}
	}
	if err != nil {
		fe.add(path, "not a valid JSON Schema: %s", err.Error())
	}
}
