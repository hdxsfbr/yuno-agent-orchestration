package httpapi

import (
	"net/http"
	"time"

	"yuno/internal/memory"
	"yuno/internal/store"
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

// deleteAgentMemory forgets one fact of the agent.
func (s *Server) deleteAgentMemory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := store.GetAgent(r.Context(), s.pool, id); err != nil {
		s.storeError(w, r, "agent", err)
		return
	}
	if err := memory.Delete(r.Context(), s.pool, id, r.PathValue("factId")); err != nil {
		s.storeError(w, r, "memory fact", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
