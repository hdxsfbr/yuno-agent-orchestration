package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"yuno/internal/engine"
	"yuno/internal/guardrails"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// The Chief of Staff's side of the API (DESIGN 5.15): delegate_task's view of the
// engine, and the conversation's chat-time tool calls for the Channels page.

// delegator is delegate_task's tools.Delegator: the engine's Trigger with Trigger =
// channel and default limits (rule 6), its refusals turned into messages the model can
// act on (failure handling: returned as tool errors, nothing queued).
type delegator struct{ s *Server }

func (d delegator) Delegate(ctx context.Context, req tools.Delegation) (*store.Run, error) {
	s := d.s
	if s.engine == nil {
		return nil, errEngineUnavailable
	}
	run, err := s.engine.Trigger(ctx, engine.TriggerRequest{
		AgentID: req.AgentID, Task: req.Task, WorkflowID: req.WorkflowID, Input: req.Input,
		Trigger: store.TriggerChannel, TriggerRef: req.TriggerRef,
	})
	var ve *engine.ValidationError
	var rl *guardrails.RateLimitError
	switch {
	case errors.As(err, &ve):
		return nil, &tools.ModelError{Message: "the run was not started: " + strings.Join(ve.Messages, "; ")}
	case errors.As(err, &rl):
		return nil, &tools.ModelError{Message: "the run was not started: " + rl.Error()}
	case errors.Is(err, store.ErrNotFound):
		return nil, &tools.ModelError{Message: "the run was not started: the agent or workflow no longer exists"}
	case err != nil:
		return nil, err
	}
	s.log.Info("delegated run", "run_id", run.ID, "agent_id", req.CallerAgentID, "to_agent_id", req.AgentID,
		"workflow_id", req.WorkflowID, "trigger_ref", req.TriggerRef)
	return run, nil
}

// listConversationToolCalls returns the chat-time platform tool calls recorded against
// a conversation, oldest first (DESIGN 5.15 rule 3).
func (s *Server) listConversationToolCalls(w http.ResponseWriter, r *http.Request) {
	limit, ok := conversationsLimit(w, r, defaultMessages, maxMessages)
	if !ok {
		return
	}
	ctx := r.Context()
	c, err := store.GetConversation(ctx, s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "conversation", err)
		return
	}
	out, err := store.ListToolCallsByConversation(ctx, s.pool, c.ID, limit)
	if err != nil {
		s.internalError(w, r, "list conversation tool calls", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
