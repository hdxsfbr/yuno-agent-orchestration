package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"yuno/internal/store"
)

const maxRestoreBody = 4 << 10

// restoreRequest is WorkflowRestore.
type restoreRequest struct {
	ExpectedVersion *int `json:"expected_version"`
}

// listWorkflowVersions returns the workflow's versions newest first, without their
// definitions (DESIGN 5.35 rule 2).
func (s *Server) listWorkflowVersions(w http.ResponseWriter, r *http.Request) {
	wf, err := store.GetWorkflow(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "workflow", err)
		return
	}
	list, err := store.ListWorkflowVersions(r.Context(), s.pool, wf.ID)
	if err != nil {
		s.internalError(w, r, "list workflow versions", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// getWorkflowVersion returns one version with its definition (DESIGN 5.35 rule 2).
func (s *Server) getWorkflowVersion(w http.ResponseWriter, r *http.Request) {
	wf, err := store.GetWorkflow(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "workflow", err)
		return
	}
	v, ok := s.workflowVersion(w, r, wf.ID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// workflowVersion loads the version the path names; a version that is not a positive
// integer, or that the workflow does not have, answers 404.
func (s *Server) workflowVersion(w http.ResponseWriter, r *http.Request, workflowID string) (*store.WorkflowVersion, bool) {
	n, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || n < 1 {
		writeError(w, http.StatusNotFound, "not_found", "workflow version not found", nil)
		return nil, false
	}
	v, err := store.GetWorkflowVersion(r.Context(), s.pool, workflowID, n)
	if err != nil {
		s.storeError(w, r, "workflow version", err)
		return nil, false
	}
	return v, true
}

// restoreWorkflowVersion writes an earlier version's name, description and definition as
// a new version, current + 1 (DESIGN 5.35 rule 3). The caller names the version it read in
// expected_version: any other current version answers 412 version_conflict. The restored
// definition is checked as a PUT checks one, against the agents and schemas of today: 422
// with the problems when it would not be accepted now. Runs keep the snapshot they
// started with.
func (s *Server) restoreWorkflowVersion(w http.ResponseWriter, r *http.Request) {
	wf, ok := s.editableWorkflow(w, r)
	if !ok {
		return
	}
	v, ok := s.workflowVersion(w, r, wf.ID)
	if !ok {
		return
	}
	var in restoreRequest
	if !decodeBody(w, r, &in, maxRestoreBody) {
		return
	}
	if in.ExpectedVersion == nil {
		writeValidation(w, "invalid request body", fieldErrors{{Path: "expected_version", Message: "is required"}})
		return
	}
	if *in.ExpectedVersion != wf.Version {
		writeVersionConflict(w, wf.Version)
		return
	}
	check := workflowInput{Name: v.Name, Description: v.Description, Definition: v.Definition}
	fe, err := s.checkWorkflowInput(r.Context(), &check)
	if err != nil {
		s.internalError(w, r, "validate workflow", err)
		return
	}
	if len(fe) > 0 {
		writeValidation(w, fmt.Sprintf("version %d cannot be restored: it is not a valid workflow now", v.Version), fe)
		return
	}
	wf.Name, wf.Description, wf.Definition = v.Name, v.Description, v.Definition
	err = store.UpdateWorkflowIfVersion(r.Context(), s.pool, wf, *in.ExpectedVersion)
	if errors.Is(err, store.ErrVersionConflict) {
		// Another write landed between the read above and this one.
		if now, gerr := store.GetWorkflow(r.Context(), s.pool, wf.ID); gerr == nil {
			writeVersionConflict(w, now.Version)
			return
		}
		writeError(w, http.StatusPreconditionFailed, "version_conflict", "the workflow changed since it was read; read it again", nil)
		return
	}
	if err != nil {
		s.storeError(w, r, "workflow", err)
		return
	}
	s.respondWorkflow(w, r, wf.ID, http.StatusOK)
}

// writeVersionConflict answers 412 version_conflict with the workflow's current version.
func writeVersionConflict(w http.ResponseWriter, current int) {
	writeError(w, http.StatusPreconditionFailed, "version_conflict",
		fmt.Sprintf("the workflow is at version %d now; read it again", current), map[string]int{"current_version": current})
}
