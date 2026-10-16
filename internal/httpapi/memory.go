package httpapi

import (
	"context"
	"net/http"
	"time"

	"yuno/internal/memory"
	"yuno/internal/openclaw"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// listAgentMemory lists the agent's live facts, newest first (DESIGN 5.10 rule 7).
// Expired facts are left out even before the sweep removes them.
func (s *Server) listAgentMemory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := store.GetAgent(r.Context(), s.pool, id); err != nil {
		s.storeError(w, r, "agent", err)
		return
	}
	facts, err := memory.List(r.Context(), s.pool, id, time.Now().UTC())
	if err != nil {
		s.internalError(w, r, "list memory", err)
		return
	}
	writeJSON(w, http.StatusOK, facts)
}

// deleteAgentMemory forgets one fact of the agent and takes it out of MEMORY.md.
func (s *Server) deleteAgentMemory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := store.GetAgent(r.Context(), s.pool, id)
	if err != nil {
		s.storeError(w, r, "agent", err)
		return
	}
	if err := memory.Delete(r.Context(), s.pool, id, r.PathValue("factId")); err != nil {
		s.storeError(w, r, "memory fact", err)
		return
	}
	s.renderMemory(r.Context(), a)
	w.WriteHeader(http.StatusNoContent)
}

// renderMemory re-renders an agent's MEMORY.md after its facts changed outside a node
// visit, so a conversation with the agent reads them too. Failures are logged: the next
// node visit renders the file again.
func (s *Server) renderMemory(ctx context.Context, a *store.Agent) {
	if s.pool == nil {
		return
	}
	if err := openclaw.RenderMemory(ctx, s.pool, s.cfg.OpenClawStateDir, a, time.Now().UTC()); err != nil {
		s.log.Warn("render MEMORY.md failed", "agent_id", a.ID, "error", err)
	}
}

// memorySaved is memory_save's hook (tools.PlatformDeps.MemorySaved).
func (s *Server) memorySaved(ctx context.Context, call tools.Call) {
	a, err := store.GetAgent(ctx, s.pool, call.AgentID)
	if err != nil {
		s.log.Warn("render MEMORY.md after memory_save failed", "agent_id", call.AgentID, "error", err)
		return
	}
	s.renderMemory(ctx, a)
}
