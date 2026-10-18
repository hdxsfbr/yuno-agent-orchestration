package httpapi

import "net/http"

// Project notes (DESIGN 5.18 rule 6). Wave 8 agent A replaces these stubs.

func (s *Server) listProjectFacts(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}

func (s *Server) deleteProjectFact(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}

// notImplemented answers a route the contract documents ahead of its wave.
func notImplemented(w http.ResponseWriter) {
	writeError(w, http.StatusNotImplemented, "not_implemented", "this endpoint is not available yet", nil)
}
