package httpapi

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"yuno/internal/engine"
	"yuno/internal/store"
)

const (
	maxDecisionBody       = 16 << 10
	maxDecisionReason     = 2000
	defaultApprovalsLimit = 100
	maxApprovalsLimit     = 500
	// decidedBy is the principal recorded for decisions made through the API: there
	// are no user accounts (DESIGN section 1 non-goals), only the API token.
	decidedBy = "api"
)

var approvalStatuses = []string{store.ApprovalPending, store.ApprovalApproved, store.ApprovalRejected, store.ApprovalExpired}

// approvalDecision is ApprovalDecision; always is only accepted on approve.
type approvalDecision struct {
	Reason string `json:"reason"`
	Always bool   `json:"always"`
}

type rejectDecision struct {
	Reason string `json:"reason"`
}

// listApprovals is the inbox: approvals newest first with their tool call joined.
func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var fe fieldErrors
	status := q.Get("status")
	if status != "" && !slices.Contains(approvalStatuses, status) {
		fe.add("status", "must be one of %s", strings.Join(approvalStatuses, ", "))
	}
	limit := defaultApprovalsLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxApprovalsLimit {
			fe.add("limit", "must be an integer from 1 to %d", maxApprovalsLimit)
		} else {
			limit = n
		}
	}
	if len(fe) > 0 {
		writeValidation(w, "invalid query", fe)
		return
	}
	items, err := store.ListApprovalItems(r.Context(), s.pool, status, q.Get("run_id"), limit)
	if err != nil {
		s.internalError(w, r, "list approvals", err)
		return
	}
	for i := range items {
		if tc := items[i].ToolCall; tc != nil {
			tc.Output = omitNull(tc.Output)
		}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) approveApproval(w http.ResponseWriter, r *http.Request) {
	var in approvalDecision
	if !decodeOptionalBody(w, r, &in) {
		return
	}
	decision := engine.DecisionApprove
	if in.Always {
		decision = engine.DecisionApproveAlways
	}
	s.decide(w, r, decision, in.Reason, in.Always)
}

func (s *Server) rejectApproval(w http.ResponseWriter, r *http.Request) {
	var in rejectDecision
	if !decodeOptionalBody(w, r, &in) {
		return
	}
	s.decide(w, r, engine.DecisionReject, in.Reason, false)
}

// decide applies a decision through the engine (DESIGN 5.3 rules 3, 5, 8): 404 for an
// unknown approval, 409 when it is no longer pending.
func (s *Server) decide(w http.ResponseWriter, r *http.Request, decision, reason string, always bool) {
	id := r.PathValue("id")
	reason = strings.TrimSpace(reason)
	var fe fieldErrors
	if utf8.RuneCountInString(reason) > maxDecisionReason {
		fe.add("reason", "must be at most %d characters", maxDecisionReason)
	}
	a, err := store.GetApproval(r.Context(), s.pool, id)
	if err != nil {
		s.storeError(w, r, "approval", err)
		return
	}
	if always && a.Kind != store.ApprovalKindExec {
		fe.add("always", "only exec approvals can be approved always")
	}
	if len(fe) > 0 {
		writeValidation(w, "invalid decision", fe)
		return
	}
	if a.Status != store.ApprovalPending {
		writeNotPending(w, a.Status)
		return
	}
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", errEngineUnavailable.Error(), nil)
		return
	}
	decided, err := s.engine.DecideApproval(r.Context(), id, decision, decidedBy, reason)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, decided)
	case errors.Is(err, engine.ErrNotPending):
		status := "decided"
		if cur, gerr := store.GetApproval(r.Context(), s.pool, id); gerr == nil {
			status = cur.Status
		}
		writeNotPending(w, status)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "approval not found", nil)
	case errors.Is(err, engine.ErrNotImplemented):
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "the run engine cannot decide approvals", nil)
	default:
		s.internalError(w, r, "decide approval", err)
	}
}

func writeNotPending(w http.ResponseWriter, status string) {
	writeError(w, http.StatusConflict, "not_pending", "approval is already "+status, nil)
}

// decodeOptionalBody is decodeBody for endpoints whose body may be empty.
func decodeOptionalBody(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxDecisionBody))
	if err != nil {
		writeValidation(w, "invalid request body", fieldErrors{bodyFieldError(err)})
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return decodeBody(w, r, v, maxDecisionBody)
}
