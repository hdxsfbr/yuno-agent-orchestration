package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"yuno/internal/store"
	"yuno/internal/tools"
	"yuno/internal/workflow"
)

// templateKeyPattern keeps template keys apart from the quick-run wrappers'
// "quickrun:<agent>" keys, which share the template_key column.
var templateKeyPattern = regexp.MustCompile(`^[a-z0-9_]{1,60}$`)

// workflowTemplate is a templates/*.json file and the WorkflowTemplate schema
// (DESIGN 5.6 rule 9).
type workflowTemplate struct {
	Key         string          `json:"key"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Definition  json.RawMessage `json:"definition"`
}

type instantiateRequest struct {
	Name string `json:"name"`
}

// LoadTemplates upserts every templates/*.json into a workflows row keyed by its
// template_key: created when new, updated in place when the file changed, never
// deleted. Each file is logged; an invalid file is skipped.
func (s *Server) LoadTemplates(ctx context.Context) error {
	if s.templatesDir == "" || s.pool == nil {
		return nil
	}
	files, err := filepath.Glob(filepath.Join(s.templatesDir, "*.json"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		action, key, err := s.loadTemplate(ctx, f)
		if err != nil {
			s.log.Warn("workflow template skipped", "file", filepath.Base(f), "error", err)
			continue
		}
		s.log.Info("workflow template loaded", "key", key, "file", filepath.Base(f), "action", action)
	}
	return nil
}

func (s *Server) loadTemplate(ctx context.Context, file string) (action, key string, err error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", "", err
	}
	var t workflowTemplate
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return "", "", fmt.Errorf("decode: %w", err)
	}
	if !templateKeyPattern.MatchString(t.Key) {
		return "", t.Key, fmt.Errorf("key %q must match %s", t.Key, templateKeyPattern)
	}
	if strings.TrimSpace(t.Name) == "" {
		return "", t.Key, errors.New("name is required")
	}
	fe, err := s.checkDefinition(ctx, t.Definition, definitionRules{})
	if err != nil {
		return "", t.Key, err
	}
	if len(fe) > 0 {
		return "", t.Key, fmt.Errorf("definition: %s: %s", fe[0].Path, fe[0].Message)
	}
	existing, err := store.GetWorkflowByTemplateKey(ctx, s.pool, t.Key)
	switch {
	case errors.Is(err, store.ErrNotFound):
		key := t.Key
		wf := &store.Workflow{Name: t.Name, Description: t.Description, Definition: t.Definition, TemplateKey: &key}
		if err := store.CreateWorkflow(ctx, s.pool, wf); err != nil {
			return "", t.Key, err
		}
		return "created", t.Key, nil
	case err != nil:
		return "", t.Key, err
	}
	if existing.Name == t.Name && existing.Description == t.Description && sameJSON(existing.Definition, t.Definition) {
		return "unchanged", t.Key, nil
	}
	existing.Name, existing.Description, existing.Definition = t.Name, t.Description, t.Definition
	if err := store.UpdateWorkflow(ctx, s.pool, existing); err != nil {
		return "", t.Key, err
	}
	return "updated", t.Key, nil
}

// sameJSON compares two documents ignoring key order and whitespace.
func sameJSON(a, b json.RawMessage) bool {
	ca, errA := tools.CanonicalJSON(a)
	cb, errB := tools.CanonicalJSON(b)
	return errA == nil && errB == nil && bytes.Equal(ca, cb)
}

// listTemplates returns the loaded templates (not the quick-run wrappers).
func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	all, err := store.ListWorkflows(r.Context(), s.pool)
	if err != nil {
		s.internalError(w, r, "list templates", err)
		return
	}
	out := []workflowTemplate{}
	for _, wf := range all {
		if wf.TemplateKey != nil && templateKeyPattern.MatchString(*wf.TemplateKey) {
			out = append(out, workflowTemplate{Key: *wf.TemplateKey, Name: wf.Name, Description: wf.Description, Definition: wf.Definition})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	writeJSON(w, http.StatusOK, out)
}

// instantiateTemplate copies a template into a new editable workflow with every
// agent_name resolved, case-insensitively, to the id of the agent of that name
// (DESIGN 5.6 rule 9, D-018): 422 nodes[i].agent_name when no agent has the name.
func (s *Server) instantiateTemplate(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !templateKeyPattern.MatchString(key) {
		writeError(w, http.StatusNotFound, "not_found", "template not found", nil)
		return
	}
	tpl, err := store.GetWorkflowByTemplateKey(r.Context(), s.pool, key)
	if err != nil {
		s.storeError(w, r, "template", err)
		return
	}
	var in instantiateRequest
	if !decodeOptionalBody(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = tpl.Name
	}
	var fe fieldErrors
	if len([]rune(name)) > maxWorkflowName {
		fe.add("name", "must be at most %d characters", maxWorkflowName)
	}
	def, nameErrs, err := s.resolveAgentNames(r.Context(), tpl.Definition)
	if err != nil {
		s.internalError(w, r, "instantiate template", err)
		return
	}
	fe = append(fe, nameErrs...)
	if len(fe) == 0 {
		defErrs, err := s.checkDefinition(r.Context(), def, definitionRules{agents: true})
		if err != nil {
			s.internalError(w, r, "instantiate template", err)
			return
		}
		fe = append(fe, defErrs...)
	}
	if len(fe) > 0 {
		writeValidation(w, "the template cannot be instantiated", fe)
		return
	}
	wf := &store.Workflow{Name: name, Description: tpl.Description, Definition: def}
	if err := store.CreateWorkflow(r.Context(), s.pool, wf); err != nil {
		s.internalError(w, r, "instantiate template", err)
		return
	}
	s.log.Info("workflow template instantiated", "key", key, "workflow_id", wf.ID)
	s.respondWorkflow(w, r, wf.ID, http.StatusCreated)
}

// resolveAgentNames replaces agent_name with agent_id on every agent node, keeping
// everything else in the definition (including keys the platform does not know, such
// as the builder's layout). When several agents share a name the oldest wins.
func (s *Server) resolveAgentNames(ctx context.Context, raw json.RawMessage) (json.RawMessage, fieldErrors, error) {
	agents, err := store.ListAgents(ctx, s.pool)
	if err != nil {
		return nil, nil, err
	}
	// Oldest first, ids (UUIDv7, time-ordered) breaking ties, so the choice is stable.
	slices.SortStableFunc(agents, func(a, b store.Agent) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	byName := map[string]string{}
	for _, a := range agents {
		k := strings.ToLower(strings.TrimSpace(a.Name))
		if _, ok := byName[k]; !ok {
			byName[k] = a.ID
		}
	}
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, fmt.Errorf("template definition: %w", err)
	}
	var fe fieldErrors
	nodes, _ := doc["nodes"].([]any)
	for i, n := range nodes {
		node, ok := n.(map[string]any)
		if !ok || node["kind"] != workflow.KindAgent {
			continue
		}
		name, _ := node["agent_name"].(string)
		if name == "" {
			continue
		}
		id, ok := byName[strings.ToLower(strings.TrimSpace(name))]
		if !ok {
			fe.add(fmt.Sprintf("nodes[%d].agent_name", i), "no agent named '%s'", name)
			continue
		}
		node["agent_id"] = id
		delete(node, "agent_name")
	}
	out, err := json.Marshal(doc)
	return out, fe, err
}
