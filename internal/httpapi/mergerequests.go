package httpapi

import "net/http"

// Merge requests (DESIGN 5.20 rules 3, 5 and 6). Wave 8 agent B replaces these stubs.

func (s *Server) listMergeRequests(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}

func (s *Server) getMergeRequest(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}

func (s *Server) syncMergeRequest(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}

func (s *Server) mergeMergeRequest(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}
