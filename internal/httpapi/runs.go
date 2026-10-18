package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"yuno/internal/engine"
	"yuno/internal/guardrails"
	"yuno/internal/project"
	"yuno/internal/store"
)

const (
	maxRunBody       = 1 << 20
	defaultRunsLimit = 100
	maxRunsLimit     = 500
	// cancelledBy is recorded as runs.cancel_by for cancellations through the API
	// (single operator, DESIGN section 10).
	cancelledBy = "operator"
)

var runStatuses = []string{
	store.RunQueued, store.RunRunning, store.RunWaitingApproval,
	store.RunSucceeded, store.RunFailed, store.RunCancelled, store.RunTimedOut,
}

// runCreate is RunCreate.
type runCreate struct {
	WorkflowID string          `json:"workflow_id"`
	Input      json.RawMessage `json:"input"`
	AgentID    string          `json:"agent_id"`
	Task       string          `json:"task"`
	Limits     store.Limits    `json:"limits"`
	Model      string          `json:"model"`
	ScriptID   string          `json:"script_id"`
	Trigger    string          `json:"trigger"`
	// ProjectID names the project the run works on (DESIGN 5.17 rule 4): required for
	// a workflow that opts in, refused for one that does not, optional for a quick run.
	// The engine checks that against the workflow.
	ProjectID string `json:"project_id"`
}

// apiTriggers are the triggers a client may name; schedules and channels start their
// runs themselves.
var apiTriggers = []string{store.TriggerManual, store.TriggerEval}

func (in *runCreate) validate() fieldErrors {
	var fe fieldErrors
	switch {
	case in.WorkflowID == "" && in.AgentID == "":
		fe.add("", "set workflow_id (with input) or agent_id (with task)")
	case in.WorkflowID != "" && in.AgentID != "":
		fe.add("agent_id", "set either workflow_id or agent_id, not both")
	case in.AgentID != "" && strings.TrimSpace(in.Task) == "":
		fe.add("task", "is required with agent_id")
	}
	if raw := bytes.TrimSpace(in.Input); len(raw) > 0 && string(raw) != "null" && raw[0] != '{' {
		fe.add("input", "must be an object")
	}
	if in.ScriptID != "" && !validScriptID(in.ScriptID) {
		fe.add("script_id", "must match %s", scriptIDPattern)
	}
	if in.Trigger != "" && !slices.Contains(apiTriggers, in.Trigger) {
		fe.add("trigger", "must be one of %s", strings.Join(apiTriggers, ", "))
	}
	return fe
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var fe fieldErrors
	status := q.Get("status")
	if status != "" && !slices.Contains(runStatuses, status) {
		fe.add("status", "must be one of %s", strings.Join(runStatuses, ", "))
	}
	limit := defaultRunsLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxRunsLimit {
			fe.add("limit", "must be an integer from 1 to %d", maxRunsLimit)
		} else {
			limit = n
		}
	}
	if len(fe) > 0 {
		writeValidation(w, "invalid query", fe)
		return
	}
	runs, err := store.ListRuns(r.Context(), s.pool, status, strings.TrimSpace(q.Get("project_id")), limit)
	if err != nil {
		s.internalError(w, r, "list runs", err)
		return
	}
	if err := store.FillRunParticipants(r.Context(), s.pool, runs); err != nil {
		s.internalError(w, r, "list run participants", err)
		return
	}
	if err := store.FillRunMergeRequests(r.Context(), s.pool, runs); err != nil {
		s.internalError(w, r, "list run merge requests", err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

// createRun starts a run for an operator (trigger manual, the default) or for the eval
// runner (trigger eval); RunCreate maps one to one onto TriggerRequest.
func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	var in runCreate
	if !decodeBody(w, r, &in, maxRunBody) {
		return
	}
	if fe := in.validate(); len(fe) > 0 {
		writeValidation(w, "invalid run", fe)
		return
	}
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", errEngineUnavailable.Error(), nil)
		return
	}
	input := in.Input
	if string(bytes.TrimSpace(input)) == "null" {
		input = nil
	}
	trigger := in.Trigger
	if trigger == "" {
		trigger = store.TriggerManual
	}
	run, err := s.engine.Trigger(r.Context(), engine.TriggerRequest{
		WorkflowID: in.WorkflowID,
		Input:      input,
		AgentID:    in.AgentID,
		Task:       in.Task,
		Limits:     in.Limits,
		Trigger:    trigger,
		Model:      in.Model,
		ScriptID:   in.ScriptID,
		ProjectID:  strings.TrimSpace(in.ProjectID),
	})
	if err != nil {
		s.engineError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := store.GetRun(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "run", err)
		return
	}
	// The request the run opened rides on it (DESIGN 5.20 rule 6), like the lists' agent names.
	one := []store.Run{*run}
	if err := store.FillRunMergeRequests(r.Context(), s.pool, one); err != nil {
		s.internalError(w, r, "read the run's merge request", err)
		return
	}
	writeJSON(w, http.StatusOK, &one[0])
}

// cancelRun flags the run for cancellation: 202 with the run, 409 when it has already
// finished.
func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := store.GetRun(r.Context(), s.pool, id)
	if err != nil {
		s.storeError(w, r, "run", err)
		return
	}
	if store.IsTerminal(run.Status) {
		writeError(w, http.StatusConflict, "run_finished", "run is already "+run.Status, nil)
		return
	}
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", errEngineUnavailable.Error(), nil)
		return
	}
	run, err = s.engine.Cancel(r.Context(), id, cancelledBy)
	if err != nil {
		// The run may have finished between the read and the cancel.
		if cur, gerr := store.GetRun(r.Context(), s.pool, id); gerr == nil && store.IsTerminal(cur.Status) {
			writeError(w, http.StatusConflict, "run_finished", "run is already "+cur.Status, nil)
			return
		}
		s.engineError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) getTrace(w http.ResponseWriter, r *http.Request) {
	t, err := store.GetTrace(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "run", err)
		return
	}
	// A node or tool call without output is sent without the field rather than as
	// null (the contract's output is optional and untyped).
	for i := range t.Nodes {
		t.Nodes[i].Output = omitNull(t.Nodes[i].Output)
		t.Nodes[i].PendingToolResults = omitNull(t.Nodes[i].PendingToolResults)
	}
	for i := range t.ToolCalls {
		t.ToolCalls[i].Output = omitNull(t.ToolCalls[i].Output)
	}
	for i := range t.Nodes {
		t.Nodes[i].Messages = stripReasoning(t.Nodes[i].Messages)
	}
	if t.SideEffects, err = store.ListSideEffects(r.Context(), s.pool, t.Run.ID); err != nil {
		s.internalError(w, r, "list side effects", err)
		return
	}
	// The request the run opened rides on the trace's run as it does on a get (DESIGN 5.20
	// rule 6), so the page needs no second fetch.
	one := []store.Run{*t.Run}
	if err := store.FillRunMergeRequests(r.Context(), s.pool, one); err != nil {
		s.internalError(w, r, "read the run's merge request", err)
		return
	}
	t.Run.MergeRequest = one[0].MergeRequest
	writeJSON(w, http.StatusOK, t)
}

// reasoningParts are transcript part types that carry a model's reasoning.
var reasoningParts = map[string]bool{"thinking": true, "redacted_thinking": true, "reasoning": true}

// stripReasoning drops reasoning parts from a node's transcript. The engine never
// stores them (DESIGN 5.14 rule 11); the trace does not serve them either, whatever a
// row holds.
func stripReasoning(messages json.RawMessage) json.RawMessage {
	var list []map[string]json.RawMessage
	if json.Unmarshal(messages, &list) != nil {
		return messages
	}
	changed := false
	kept := list[:0]
	for _, m := range list {
		var role string
		_ = json.Unmarshal(m["role"], &role)
		if reasoningParts[role] {
			changed = true
			continue
		}
		var parts []map[string]json.RawMessage
		if json.Unmarshal(m["content"], &parts) == nil {
			out := parts[:0]
			for _, p := range parts {
				var typ string
				_ = json.Unmarshal(p["type"], &typ)
				if !reasoningParts[typ] {
					out = append(out, p)
				}
			}
			if len(out) != len(parts) {
				changed = true
				b, _ := json.Marshal(out)
				m["content"] = b
			}
		}
		kept = append(kept, m)
	}
	if !changed {
		return messages
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return messages
	}
	return b
}

// validationDetails turns the engine's "path: message" lines into details; a line
// without a path is a detail on the body.
func validationDetails(msgs []string) fieldErrors {
	var fe fieldErrors
	for _, m := range msgs {
		path, msg, ok := strings.Cut(m, ": ")
		if !ok {
			path, msg = "", m
		}
		fe = append(fe, fieldError{Path: path, Message: msg})
	}
	return fe
}

// omitNull turns a JSON null into an absent value for omitempty fields.
func omitNull(raw json.RawMessage) json.RawMessage {
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	return raw
}

// engineError maps engine failures onto the contract: unknown ids 404, invalid
// requests 422 (the engine's "path: message" lines become details), anything else
// 500. The engine has no rate-limit error yet; guardrails arrive in a later wave.
func (s *Server) engineError(w http.ResponseWriter, r *http.Request, err error) {
	var ve *engine.ValidationError
	var rl *guardrails.RateLimitError
	var be *project.BudgetError
	switch {
	case errors.As(err, &ve):
		writeValidation(w, "invalid run", validationDetails(ve.Messages))
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error(), nil)
	case errors.As(err, &rl):
		writeError(w, http.StatusTooManyRequests, "rate_limited", rl.Error(), map[string]any{"agent_id": rl.AgentID, "limit": rl.Limit})
	case errors.As(err, &be):
		// DESIGN 5.19 rule 3: the project is at its daily or monthly cap.
		budgetRefusal(w, be)
	default:
		s.internalError(w, r, "engine", err)
	}
}
