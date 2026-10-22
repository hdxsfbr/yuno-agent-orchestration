package httpapi

import "net/http"

// platformGate is the platform gate's place in the chain (DESIGN 5.31 rule 6); it passes every request through.
func (s *Server) platformGate(next http.Handler) http.Handler { return next }
