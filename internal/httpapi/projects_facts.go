package httpapi

import (
	"net/http"
	"time"

	"yuno/internal/memory"
	"yuno/internal/store"
)

// Project notes (DESIGN 5.18 rule 6): what agents saved for a project with memory_save
// and scope project, listed and deleted by the operator.

// listProjectFacts lists the project's live notes, newest first, each with the saving
// agent's name. Expired notes are left out even before the sweep removes them.
func (s *Server) listProjectFacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if _, err := store.GetProject(ctx, s.pool, id); err != nil {
		s.storeError(w, r, "project", err)
		return
	}
	facts, err := memory.ListProjectFacts(ctx, s.pool, id, time.Now().UTC())
	if err != nil {
		s.internalError(w, r, "list project notes", err)
		return
	}
	writeJSON(w, http.StatusOK, facts)
}

// deleteProjectFact removes one note: 204, 404 project not found for a missing or
// deleted project, 404 memory fact not found for a missing note.
func (s *Server) deleteProjectFact(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if _, err := store.GetProject(ctx, s.pool, id); err != nil {
		s.storeError(w, r, "project", err)
		return
	}
	if err := memory.DeleteProjectFact(ctx, s.pool, id, r.PathValue("factId")); err != nil {
		s.storeError(w, r, "memory fact", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
