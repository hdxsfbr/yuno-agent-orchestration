package httpapi

import "net/http"

// Retry of a run from the step it stopped at (DESIGN 5.21 rule 1). Wave 8 agent C
// replaces this stub.

func (s *Server) retryRun(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}
