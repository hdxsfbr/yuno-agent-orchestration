package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"yuno/internal/engine"
	"yuno/internal/store"
)

// Retry of a run from the step it stopped at (DESIGN 5.21 rule 1).

// retriedBy is recorded as the retry's `by` in the run event, as cancelledBy is for a
// cancellation (single operator, DESIGN section 10).
const retriedBy = "operator"

// runRetry is RunRetry: the optional body of a retry. Limits, when given, are resolved
// and clamped as at trigger time and replace the run's limits before the exhausted-limit
// check; FromNode names a step the run reached to retry from (DESIGN 5.33 rule 16).
type runRetry struct {
	FromNode string        `json:"from_node"`
	Limits   *store.Limits `json:"limits"`
}

// retryRun re-queues a failed, timed-out or cancelled run from its current node, or from
// from_node: 202 with the run and its command, 404 for an unknown run, 409 with the code
// and message of the refusal (run_not_retryable, retry_limit, limits_exhausted with one
// detail per exhausted limit, cancel_unconfirmed), 422 for limits that are not valid, 503
// when the command could not be recorded. The body is optional.
func (s *Server) retryRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := store.GetRun(r.Context(), s.pool, id); err != nil {
		s.storeError(w, r, "run", err)
		return
	}
	var in runRetry
	if err := decodeJSON(r, &in, maxRunBody); err != nil && !errors.Is(err, io.EOF) {
		writeValidation(w, "invalid request body", fieldErrors{bodyFieldError(err)})
		return
	}
	if hasControl(in.FromNode) {
		var fe fieldErrors
		fe.add("from_node", "must be a step this run reached")
		writeValidation(w, "invalid retry", fe)
		return
	}
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", errEngineUnavailable.Error(), nil)
		return
	}
	var run *store.Run
	var err error
	if strings.TrimSpace(in.FromNode) == "" {
		run, err = s.engine.Retry(r.Context(), id, retriedBy, in.Limits)
	} else {
		run, err = s.engine.RetryFrom(r.Context(), id, retriedBy, in.FromNode, in.Limits)
	}
	if err != nil {
		s.retryError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

// retryError maps the engine's answer onto the contract. What it does not know (a
// project at its budget, an engine failure) is engineError's.
func (s *Server) retryError(w http.ResponseWriter, r *http.Request, err error) {
	var re *engine.RetryError
	var ve *engine.ValidationError
	switch {
	case errors.As(err, &re):
		writeError(w, http.StatusConflict, re.Code, re.Message, retryDetails(re.Details))
	case errors.As(err, &ve):
		writeValidation(w, "invalid limits", validationDetails(ve.Messages))
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "run not found", nil)
	case errors.Is(err, engine.ErrCommandNotTaken):
		writeError(w, http.StatusServiceUnavailable, "command_not_taken", err.Error(), nil)
	default:
		s.engineError(w, r, err)
	}
}

// retryDetails turns the engine's "<limit>: <used> of <cap> used" lines into details:
// the path names the limit, the message is the line. nil (no details at all) for none.
func retryDetails(lines []string) any {
	if len(lines) == 0 {
		return nil
	}
	fe := make(fieldErrors, 0, len(lines))
	for _, l := range lines {
		path, _, _ := strings.Cut(l, ":")
		fe = append(fe, fieldError{Path: path, Message: l})
	}
	return fe
}
