package httpapi

import "net/http"

// Run artifacts (DESIGN 5.25 rule 5): the two routes are in the contract ahead of the
// store that serves them. Both answer 501 until it exists.

// listArtifacts is GET /api/runs/{id}/artifacts.
func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not_implemented", "planned for wave 10", nil)
}

// downloadArtifact is GET /api/runs/{id}/artifacts/{artifactId}.
func (s *Server) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not_implemented", "planned for wave 10", nil)
}
