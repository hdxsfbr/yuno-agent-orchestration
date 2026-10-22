package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"yuno/internal/engine"
	"yuno/internal/store"
)

// Recovery commands (DESIGN 5.33 rules 1 to 3, 14, 15 and 18): pause, resume and steer a run,
// and read a run's commands. Cancel and retry keep their own handlers (runs.go,
// runs_retry.go) and answer the same way: 202 with the run and its command, 409 with the
// refusal, 503 when the command could not be recorded.

// pausedBy, resumedBy and steeredBy are recorded as the commands' principal, as cancelledBy
// is for a cancellation (single operator, DESIGN section 10).
const (
	pausedBy  = "operator"
	resumedBy = "operator"
	steeredBy = "operator"
)

// runResume is RunResume: the optional body of a resume.
type runResume struct {
	Note string `json:"note"`
}

// runSteer is RunSteer.
type runSteer struct {
	NodeID  string `json:"node_id"`
	Message string `json:"message"`
}

// pauseRun pauses a run (rule 14): 202 with the run and the command (applied at once for a
// queued run, pending for the live run's worker), 404, 409 command_rejected or run_finished.
func (s *Server) pauseRun(w http.ResponseWriter, r *http.Request) {
	if !s.engineReady(w) {
		return
	}
	run, err := s.engine.Pause(r.Context(), r.PathValue("id"), pausedBy)
	s.commandAnswer(w, r, run, err)
}

// resumeRun re-queues a paused run (rule 15): 202 with the run and the applied command; 409
// command_rejected for a run that is not paused, cancel_unconfirmed while its last turn is not
// confirmed stopped (rule 13); 422 for a note over 2000 characters. The body is optional.
func (s *Server) resumeRun(w http.ResponseWriter, r *http.Request) {
	var in runResume
	if err := decodeJSON(r, &in, maxRunBody); err != nil && !errors.Is(err, io.EOF) {
		writeValidation(w, "invalid request body", fieldErrors{bodyFieldError(err)})
		return
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(in.Note)); n > engine.MaxResumeNoteChars || strings.ContainsRune(in.Note, 0) {
		var fe fieldErrors
		fe.add("note", "at most %d characters", engine.MaxResumeNoteChars)
		writeValidation(w, "invalid resume", fe)
		return
	}
	if !s.engineReady(w) {
		return
	}
	run, err := s.engine.Resume(r.Context(), r.PathValue("id"), resumedBy, in.Note)
	s.commandAnswer(w, r, run, err)
}

// steerRun sends the operator's message into a run's live step (rule 18): 202 with the run
// and the pending command; 409 command_rejected when nothing is running, the step is not
// named among several, or names one that is not running; 422 for a message that is not 1 to
// 4000 characters.
func (s *Server) steerRun(w http.ResponseWriter, r *http.Request) {
	var in runSteer
	if !decodeBody(w, r, &in, maxRunBody) {
		return
	}
	var fe fieldErrors
	if n := utf8.RuneCountInString(strings.TrimSpace(in.Message)); n < 1 || n > engine.MaxSteerChars || strings.ContainsRune(in.Message, 0) {
		fe.add("message", "1 to %d characters", engine.MaxSteerChars)
	}
	if hasControl(in.NodeID) {
		fe.add("node_id", "is not a step of the run")
	}
	if len(fe) > 0 {
		writeValidation(w, "invalid steer", fe)
		return
	}
	if !s.engineReady(w) {
		return
	}
	run, err := s.engine.Steer(r.Context(), r.PathValue("id"), steeredBy, in.NodeID, in.Message)
	s.commandAnswer(w, r, run, err)
}

// listRunCommands is GET /api/runs/{id}/commands: the run's commands newest first, at most
// 100 (rule 3).
func (s *Server) listRunCommands(w http.ResponseWriter, r *http.Request) {
	run, err := store.GetRun(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "run", err)
		return
	}
	list, err := store.ListRunCommands(r.Context(), s.pool, run.ID)
	if err != nil {
		s.internalError(w, r, "list run commands", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// getRunCommand is GET /api/runs/{id}/commands/{cid}.
func (s *Server) getRunCommand(w http.ResponseWriter, r *http.Request) {
	run, err := store.GetRun(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "run", err)
		return
	}
	cid := r.PathValue("cid")
	if hasControl(cid) {
		writeError(w, http.StatusNotFound, "not_found", "command not found", nil)
		return
	}
	c, err := store.GetRunCommand(r.Context(), s.pool, run.ID, cid)
	if err != nil {
		s.storeError(w, r, "command", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// engineReady answers 503 when there is no engine to take a command.
func (s *Server) engineReady(w http.ResponseWriter) bool {
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", errEngineUnavailable.Error(), nil)
		return false
	}
	return true
}

// commandAnswer answers a recovery command: 202 with the run (which carries the command),
// or the refusal.
func (s *Server) commandAnswer(w http.ResponseWriter, r *http.Request, run *store.Run, err error) {
	if err != nil {
		s.commandError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

// commandError maps a command's refusal onto the contract: 409 with the command's code and
// detail (and the recorded command in details), 503 for a command that could not be recorded,
// 422 for a bad note or message, 404 for an unknown run.
func (s *Server) commandError(w http.ResponseWriter, r *http.Request, err error) {
	var ce *engine.CommandError
	var ve *engine.ValidationError
	switch {
	case errors.As(err, &ce):
		var details any
		if ce.Command != nil {
			details = map[string]any{"command": ce.Command}
		}
		writeError(w, http.StatusConflict, ce.Code, ce.Message, details)
	case errors.Is(err, engine.ErrCommandNotTaken):
		writeError(w, http.StatusServiceUnavailable, "command_not_taken", err.Error(), nil)
	case errors.As(err, &ve):
		writeValidation(w, "invalid command", validationDetails(ve.Messages))
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "run not found", nil)
	default:
		s.engineError(w, r, err)
	}
}
