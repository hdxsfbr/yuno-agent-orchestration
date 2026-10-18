package httpapi

import "net/http"

// Issue intake (DESIGN 5.23 rule 2): the poll-now route is in the contract ahead of the
// loop that serves it. pollIntake answers 501 until the intake replaces it.

func (s *Server) pollIntake(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not_implemented", "planned for wave 9", nil)
}
