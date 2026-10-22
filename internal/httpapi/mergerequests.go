package httpapi

import (
	"context"
	"errors"
	"fmt"
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
	return &mergereq.Sync{Pool: s.pool, Projects: s.projects, Client: s.checkClient, Reporter: rep, Events: s.rec, Log: s.log}
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
	// What holds the request, when something does (Chief platform control, DESIGN wave 3
	// rule 5), so the operator and the Chief see why it waits.
	edges, err := store.MergeRequestDependencies(r.Context(), s.pool, mr.ID)
	if err != nil {
		s.internalError(w, r, "read the merge request's dependencies", err)
		return
	}
	blocked, err := blockedBy(r.Context(), s.pool, mr, edges)
	if err != nil {
		s.internalError(w, r, "read the merge request's dependencies", err)
		return
	}
	writeJSON(w, http.StatusOK, mergeRequestRead{MergeRequest: mr, BlockedBy: blocked})
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
//
// The merge guard (Chief platform control, DESIGN wave 3 rule 15): the record's check must
// have passed on exactly its head (ci_status passed with ci_sha = head_sha), or found no
// pipeline on that head in a project with no CI configured (rule 4's test: no pipeline or
// check on the default branch's last 20 commits). The provider is sent the head as the
// sha the merge must find, so a head that moved since the check is refused there: 409
// merge_request_head_moved, after which the record is synced so it shows the new head.
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
	if s.projectRefused(w, r, err, "merge_request_not_mergeable") {
		return
	}
	head := ""
	if mr.HeadSHA != nil {
		head = *mr.HeadSHA
	}
	switch {
	case head == "":
		writeError(w, http.StatusConflict, "merge_request_ci_not_passed",
			"the head commit of the request is not known yet; sync the request, then merge", nil)
		return
	case mr.CISHA == nil || *mr.CISHA != head:
		writeError(w, http.StatusConflict, "merge_request_ci_not_passed",
			"the pipeline result is not for the head commit "+short(head)+"; sync the request, then merge", nil)
		return
	}
	provider := tools.ProviderFor(pc, s.checkClient)
	if mr.CIStatus == store.CIStatusNone {
		// None is accepted only for a project with no CI configured (rule 4's test).
		cctx, cancel := context.WithTimeout(ctx, mergeTimeout)
		configured, err := provider.CIConfigured(cctx, pc.DefaultBranch)
		cancel()
		switch {
		case err != nil:
			s.log.Warn("merge request CI check failed", "merge_request_id", mr.ID, "run_id", mr.RunID, "error", redactToken(err.Error(), pc.Token))
			code := "provider_error"
			if tools.TransientError(err) {
				code = "provider_unavailable"
			}
			writeError(w, http.StatusBadGateway, code, "whether the project has CI could not be read: "+redactToken(err.Error(), pc.Token), nil)
			return
		case configured:
			writeError(w, http.StatusConflict, "merge_request_ci_not_passed",
				"no pipeline has run on "+short(head)+" yet; merge on the provider to override", nil)
			return
		}
	}

	mctx, cancel := context.WithTimeout(ctx, mergeTimeout)
	err = provider.Merge(mctx, mr.Number, head)
	cancel()
	sync := s.mergeSync()
	var notMergeable *tools.ErrNotMergeable
	var moved *tools.ErrHeadMoved
	switch {
	case errors.As(err, &moved):
		// The branch moved since its check: the record is synced, so it shows the new head
		// and that head's own check.
		var details any
		if cur, serr := sync.SyncOne(ctx, mr.ID); serr == nil && cur.HeadSHA != nil && *cur.HeadSHA != head {
			details = map[string]string{"current": *cur.HeadSHA}
		}
		writeError(w, http.StatusConflict, "merge_request_head_moved",
			"the request's head is no longer "+short(head)+", the commit that was checked; nothing was merged", details)
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

// projectRefused answers the project resolver's refusals, the ones the operator fixes on
// the Projects page, as 409 code with a text that carries no secret, and any other error
// as a 500. It reports whether it answered; a nil err is not answered.
func (s *Server) projectRefused(w http.ResponseWriter, r *http.Request, err error, code string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, project.ErrCannotRead):
		writeError(w, http.StatusConflict, code, sentence(project.ErrCannotRead.Error()), nil)
	case errors.Is(err, project.ErrNoKey):
		writeError(w, http.StatusConflict, code, "PROJECT_TOKEN_KEY is not set; the stored token cannot be read.", nil)
	case errors.Is(err, project.ErrNoProject):
		writeError(w, http.StatusConflict, code, sentence(project.ErrNoProject.Error()), nil)
	default:
		s.internalError(w, r, "read the merge request's project", err)
	}
	return true
}

// short is a commit id as a message names it: its first seven characters.
func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// readFailed answers the errors the merge request reads and the update share (Chief
// platform control, DESIGN wave 3 rules 9 and 14): the project's refusals as 409 code, a
// request or job the provider does not have as 404, a provider failure as 502 with its
// cleaned text, anything else as a 500.
func (s *Server) readFailed(w http.ResponseWriter, r *http.Request, err error, code string) {
	var pe *mergereq.ProviderError
	switch {
	case errors.Is(err, project.ErrCannotRead), errors.Is(err, project.ErrNoKey), errors.Is(err, project.ErrNoProject):
		s.projectRefused(w, r, err, code)
	case errors.Is(err, tools.ErrRequestNotFound):
		writeError(w, http.StatusNotFound, "not_found", "the request is not on the provider", nil)
	case errors.Is(err, tools.ErrJobNotFound):
		writeError(w, http.StatusNotFound, "not_found", "the provider has no log for this job", nil)
	case errors.As(err, &pe):
		status, c := http.StatusBadGateway, "provider_error"
		if pe.Transient {
			c = "provider_unavailable"
		}
		writeError(w, status, c, pe.Message, nil)
	default:
		s.internalError(w, r, "read the merge request from its provider", err)
	}
}

// updateMergeRequestFromDefault merges the project's default branch into the request's
// branch and pushes it (Chief platform control, DESIGN wave 3 rules 9 to 11). The body
// names the head the caller saw; a branch that is elsewhere is 409 merge_request_head_moved
// with {current}. Conflicts are an outcome, recorded on the record, not an error.
func (s *Server) updateMergeRequestFromDefault(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ExpectedHeadSHA string `json:"expected_head_sha"`
	}
	if !decodeBody(w, r, &in, 4<<10) {
		return
	}
	if !project.IsCommitID(in.ExpectedHeadSHA) {
		writeValidation(w, "invalid request body", fieldErrors{{Path: "expected_head_sha", Message: "must be a full 40-character commit id"}})
		return
	}
	rec, res, err := s.mergeSync().UpdateFromDefault(r.Context(), r.PathValue("id"), in.ExpectedHeadSHA)
	var notOpen *mergereq.ErrNotOpen
	var moved *project.ErrHeadMoved
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, struct {
			mergereq.UpdateResult
			MergeRequest *store.MergeRequest `json:"merge_request"`
		}{res, rec})
	case errors.Is(err, store.ErrNotFound):
		s.storeError(w, r, "merge request", err)
	case errors.As(err, &notOpen):
		writeNotOpen(w, notOpen.State)
	case errors.As(err, &moved):
		var details any
		if moved.Current != "" {
			details = map[string]string{"current": moved.Current}
		}
		writeError(w, http.StatusConflict, "merge_request_head_moved", "the branch is not at "+short(in.ExpectedHeadSHA)+" any more; nothing was pushed", details)
	case errors.Is(err, project.ErrNoMergeBase):
		writeError(w, http.StatusConflict, "merge_request_not_updatable", sentence(project.ErrNoMergeBase.Error())+" Nothing was pushed.", nil)
	default:
		s.readFailed(w, r, err, "merge_request_not_updatable")
	}
}

// getMergeRequestDiff answers the request's files and patches from the provider (DESIGN
// wave 3 rule 14), redacted and capped at 256 KiB.
func (s *Server) getMergeRequestDiff(w http.ResponseWriter, r *http.Request) {
	rec, err := store.GetMergeRequest(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "merge request", err)
		return
	}
	d, err := s.mergeSync().Diff(r.Context(), rec)
	if err != nil {
		s.readFailed(w, r, err, "merge_request_unreadable")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// getMergeRequestChecks answers the pipeline or check runs of exactly the record's head
// with their jobs (rule 14).
func (s *Server) getMergeRequestChecks(w http.ResponseWriter, r *http.Request) {
	rec, err := store.GetMergeRequest(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "merge request", err)
		return
	}
	c, err := s.mergeSync().Checks(r.Context(), rec)
	if errors.Is(err, mergereq.ErrHeadUnknown) {
		writeError(w, http.StatusConflict, "merge_request_head_unknown", mergereq.ErrHeadUnknown.Error(), nil)
		return
	}
	if err != nil {
		s.readFailed(w, r, err, "merge_request_unreadable")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// getMergeRequestJobLog answers the tail of a job's log (rule 14): tail_bytes, default
// 64 KiB, at most 1 MiB, redacted.
func (s *Server) getMergeRequestJobLog(w http.ResponseWriter, r *http.Request) {
	tail := mergereq.DefaultLogTail
	if v := r.URL.Query().Get("tail_bytes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > mergereq.MaxLogTail {
			writeValidation(w, "invalid query", fieldErrors{{Path: "tail_bytes", Message: fmt.Sprintf("must be an integer from 1 to %d", mergereq.MaxLogTail)}})
			return
		}
		tail = n
	}
	rec, err := store.GetMergeRequest(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "merge request", err)
		return
	}
	lg, err := s.mergeSync().JobLog(r.Context(), rec, r.PathValue("job"), tail)
	if err != nil {
		s.readFailed(w, r, err, "merge_request_unreadable")
		return
	}
	writeJSON(w, http.StatusOK, lg)
}
