package httpapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"yuno/internal/mergereq"
	"yuno/internal/obs"
	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// Merge requests (DESIGN 5.20 rules 3, 5 and 6): the records the platform keeps of the
// requests it opened, their refresh from the provider and the merge button.

const (
	// mergeBy is merged_by for a merge made through the API (single operator, DESIGN
	// section 10).
	mergeBy = "operator"
	// mergeTimeout bounds the provider's answer to a merge; a merge may take a while
	// (GitLab computes it), and past it the request is re-synced (rule 5).
	mergeTimeout = 30 * time.Second
)

var mergeRequestStates = []string{store.MergeRequestOpen, store.MergeRequestMerged, store.MergeRequestClosed}

// mergeSync is the sync the handlers refresh and report through: the server's project
// resolver and provider client, and the engine as reporter when it can send platform
// messages (the engine does; a server without one reports nothing).
func (s *Server) mergeSync() *mergereq.Sync {
	rep, _ := s.engine.(mergereq.Reporter)
	return &mergereq.Sync{Pool: s.pool, Projects: s.projects, Client: s.checkClient, Reporter: rep, Log: s.log}
}

func (s *Server) listMergeRequests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var fe fieldErrors
	state := q.Get("state")
	if state != "" && !slices.Contains(mergeRequestStates, state) {
		fe.add("state", "must be one of %s", strings.Join(mergeRequestStates, ", "))
	}
	limit := store.DefaultMergeRequestsLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > store.MaxMergeRequestsLimit {
			fe.add("limit", "must be an integer from 1 to %d", store.MaxMergeRequestsLimit)
		} else {
			limit = n
		}
	}
	if len(fe) > 0 {
		writeValidation(w, "invalid query", fe)
		return
	}
	list, err := store.ListMergeRequests(r.Context(), s.pool, store.MergeRequestFilter{
		ProjectID: q.Get("project_id"), RunID: q.Get("run_id"), State: state, Limit: limit})
	if err != nil {
		s.internalError(w, r, "list merge requests", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getMergeRequest(w http.ResponseWriter, r *http.Request) {
	mr, err := store.GetMergeRequest(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "merge request", err)
		return
	}
	writeJSON(w, http.StatusOK, mr)
}

// syncMergeRequest refreshes one record from its provider at once (rule 3). A provider
// error is on the record (last_sync_error), not an HTTP error.
func (s *Server) syncMergeRequest(w http.ResponseWriter, r *http.Request) {
	mr, err := s.mergeSync().SyncOne(r.Context(), r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "merge request", err)
		return
	}
	writeJSON(w, http.StatusOK, mr)
}

// mergeMergeRequest merges the request on the provider (rule 5). The record's state is
// the idempotency: there is no tool call and no side effect row, and a second POST finds
// the record merged. The merge is recorded once the provider reports the request merged
// (sync.ConfirmMerged), not on its 2xx alone.
func (s *Server) mergeMergeRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	mr, err := store.GetMergeRequest(ctx, s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "merge request", err)
		return
	}
	if mr.State != store.MergeRequestOpen {
		writeNotOpen(w, mr.State)
		return
	}
	if mr.CIStatus != store.CIStatusPassed && mr.CIStatus != store.CIStatusNone {
		writeError(w, http.StatusConflict, "merge_request_ci_not_passed",
			"the pipeline is "+mr.CIStatus+"; merge on the provider to override", nil)
		return
	}
	pc, err := s.projects.ForRunID(ctx, mr.RunID)
	switch {
	case errors.Is(err, project.ErrCannotRead):
		// The operator fixes these on the Projects page; the text carries no secret.
		writeError(w, http.StatusConflict, "merge_request_not_mergeable", sentence(project.ErrCannotRead.Error()), nil)
		return
	case errors.Is(err, project.ErrNoKey):
		writeError(w, http.StatusConflict, "merge_request_not_mergeable", "PROJECT_TOKEN_KEY is not set; the stored token cannot be read.", nil)
		return
	case errors.Is(err, project.ErrNoProject):
		writeError(w, http.StatusConflict, "merge_request_not_mergeable", sentence(project.ErrNoProject.Error()), nil)
		return
	case err != nil:
		s.internalError(w, r, "read the merge request's project", err)
		return
	}

	mctx, cancel := context.WithTimeout(ctx, mergeTimeout)
	err = tools.ProviderFor(pc, s.checkClient).Merge(mctx, mr.Number)
	cancel()
	sync := s.mergeSync()
	var notMergeable *tools.ErrNotMergeable
	switch {
	case err == nil:
		// A 2xx is not the merge itself: the provider is asked once more, and the record is
		// marked merged only when it says so. Otherwise the record stays open with the
		// provider's answer in last_sync_error, and the loop settles it.
		merged, err := sync.ConfirmMerged(ctx, mr, pc, mergeBy)
		if err != nil {
			s.internalError(w, r, "record the merge", err)
			return
		}
		writeJSON(w, http.StatusOK, merged)
	case errors.As(err, &notMergeable):
		// The provider's own words can echo the credential the request carried, as its 502
		// answer below can: the same redaction applies before they reach the operator.
		writeError(w, http.StatusConflict, "merge_request_not_mergeable", redactToken(notMergeable.Message, pc.Token), nil)
	case tools.TransientError(err) || errors.Is(err, tools.ErrRequestNotFound):
		// A timeout or a 5xx may hide a merge that landed, and a 404 a request that is
		// gone: look once more (rule 5) and answer what the provider says now.
		s.log.Warn("merge request merge failed", "merge_request_id", mr.ID, "run_id", mr.RunID, "error", redactToken(err.Error(), pc.Token))
		cur, serr := sync.SyncOne(ctx, mr.ID)
		switch {
		case serr != nil:
			s.internalError(w, r, "re-sync the merge request", serr)
		case cur.State == store.MergeRequestMerged:
			writeJSON(w, http.StatusOK, cur)
		case cur.State != store.MergeRequestOpen:
			writeNotOpen(w, cur.State)
		default:
			writeError(w, http.StatusBadGateway, "provider_unavailable",
				"the provider did not answer; the merge may not have happened (the request was synced again)", nil)
		}
	default:
		// The provider answered and refused for a reason of its own (a token without the
		// right to merge): permanent, its first line goes to the operator.
		s.log.Warn("merge request merge refused", "merge_request_id", mr.ID, "run_id", mr.RunID, "error", redactToken(err.Error(), pc.Token))
		writeError(w, http.StatusBadGateway, "provider_error", redactToken(err.Error(), pc.Token), nil)
	}
}

// redactToken removes the project's token, and anything shaped like one, from a text
// that is about to reach the operator or a log.
func redactToken(text, token string) string {
	if token != "" {
		text = strings.ReplaceAll(text, token, obs.Redacted)
	}
	return obs.Redact(text)
}

func writeNotOpen(w http.ResponseWriter, state string) {
	writeError(w, http.StatusConflict, "merge_request_not_open", "the request is "+state, nil)
}
