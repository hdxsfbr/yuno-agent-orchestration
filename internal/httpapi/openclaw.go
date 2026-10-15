package httpapi

import (
	"context"
	"net/http"
	"slices"
	"time"

	"yuno/internal/openclaw"
)

// adminScope is the operator scope materialisation and config patches need.
const adminScope = "operator.admin"

type openclawStatusResponse struct {
	Connected bool     `json:"connected"`
	Version   string   `json:"version,omitempty"`
	ConnID    string   `json:"conn_id,omitempty"`
	Scopes    []string `json:"scopes"`
	Agents    *int     `json:"agents,omitempty"`
}

// gatewayReady reports whether the gateway connection is up and scoped for admin
// work, and why not otherwise.
func (s *Server) gatewayReady() (bool, string) {
	if s.gateway == nil {
		return false, "gateway not connected"
	}
	info, ok := s.gateway.Info()
	if !ok {
		return false, "gateway not connected"
	}
	if !slices.Contains(info.Scopes, adminScope) {
		return false, "gateway connection lacks " + adminScope
	}
	return true, ""
}

// openclawStatus reports the gateway connection and, best effort, how many agents the
// gateway knows.
func (s *Server) openclawStatus(w http.ResponseWriter, r *http.Request) {
	resp := openclawStatusResponse{Scopes: []string{}}
	if s.gateway != nil {
		info, ok := s.gateway.Info()
		resp.Connected = ok
		resp.Version, resp.ConnID = info.Version, info.ConnID
		if info.Scopes != nil {
			resp.Scopes = info.Scopes
		}
		if ok {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			if list, err := openclaw.AgentsList(ctx, s.gateway); err == nil {
				n := len(list.Agents)
				resp.Agents = &n
			} else {
				s.log.Warn("openclaw status: agents.list failed", "error", err)
			}
			cancel()
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
