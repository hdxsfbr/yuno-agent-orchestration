package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"yuno/internal/plans"
	"yuno/internal/store"
)

// Plans and the wave pause (DESIGN 5.29 rule 9).

// wavesPauseReasonMax is WavesPauseInput.reason's cap in characters.
const wavesPauseReasonMax = 500

// planDetail is PlanDetail: the plan, its tickets by wave and its newest events.
type planDetail struct {
	Plan    *store.Plan        `json:"plan"`
	Tickets []store.PlanTicket `json:"tickets"`
	Events  []store.PlanEvent  `json:"events"`
}

// listPlans: GET /api/projects/{id}/plans, newest first, finished plans included.
func (s *Server) listPlans(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if _, err := store.GetProject(ctx, s.pool, id); err != nil {
		s.storeError(w, r, "project", err)
		return
	}
	list, err := store.ListPlans(ctx, s.pool, id)
	if err != nil {
		s.internalError(w, r, "list plans", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// getPlan: GET /api/plans/{id}.
func (s *Server) getPlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	plan, err := store.GetPlan(ctx, s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "plan", err)
		return
	}
	tickets, err := store.ListPlanTickets(ctx, s.pool, plan.ID)
	if err != nil {
		s.internalError(w, r, "list plan tickets", err)
		return
	}
	evs, err := store.ListPlanEvents(ctx, s.pool, plan.ID, store.DefaultPlanEventsLimit)
	if err != nil {
		s.internalError(w, r, "list plan events", err)
		return
	}
	writeJSON(w, http.StatusOK, planDetail{Plan: plan, Tickets: tickets, Events: evs})
}

// cancelPlan: POST /api/plans/{id}/cancel. An open plan becomes cancelled with the reason
// (a `cancelled` event); a plan that is done or cancelled is 409 plan_finished.
func (s *Server) cancelPlan(w http.ResponseWriter, r *http.Request) {
	var in wavesPauseInput
	if !decodeBody(w, r, &in, 1<<14) {
		return
	}
	reason := strings.TrimSpace(in.Reason)
	var fe fieldErrors
	if reason == "" {
		fe.add("reason", "is required")
	} else if utf8.RuneCountInString(reason) > wavesPauseReasonMax {
		fe.add("reason", "must be at most %d characters", wavesPauseReasonMax)
	}
	if len(fe) > 0 {
		writeValidation(w, "invalid cancel", fe)
		return
	}
	plan, err := s.plans.Cancel(r.Context(), r.PathValue("id"), reason)
	switch {
	case errors.Is(err, plans.ErrPlanFinished):
		writeError(w, http.StatusConflict, "plan_finished", "the plan is done or cancelled already", nil)
	case err != nil:
		s.storeError(w, r, "plan", err)
	default:
		writeJSON(w, http.StatusOK, plan)
	}
}

type wavesPauseInput struct {
	Reason string `json:"reason"`
}

// pauseWaves: POST /api/projects/{id}/waves/pause. Pausing a paused project replaces the
// reason.
func (s *Server) pauseWaves(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	var in wavesPauseInput
	if !decodeBody(w, r, &in, 1<<14) {
		return
	}
	reason := strings.TrimSpace(in.Reason)
	var fe fieldErrors
	if reason == "" {
		fe.add("reason", "is required")
	} else if utf8.RuneCountInString(reason) > wavesPauseReasonMax {
		fe.add("reason", "must be at most %d characters", wavesPauseReasonMax)
	}
	if len(fe) > 0 {
		writeValidation(w, "invalid pause", fe)
		return
	}
	if err := s.plans.Pause(ctx, id, reason); err != nil {
		s.storeError(w, r, "project", err)
		return
	}
	s.writeProject(w, r, id)
}

// resumeWaves: POST /api/projects/{id}/waves/resume. Resuming a project that is not paused
// succeeds.
func (s *Server) resumeWaves(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.plans.Resume(r.Context(), id); err != nil {
		s.storeError(w, r, "project", err)
		return
	}
	s.writeProject(w, r, id)
}

func (s *Server) writeProject(w http.ResponseWriter, r *http.Request, id string) {
	p, err := s.projectView(r.Context(), id)
	if err != nil {
		s.storeError(w, r, "project", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}
