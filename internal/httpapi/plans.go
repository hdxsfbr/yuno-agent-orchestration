package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"yuno/internal/engine"
	"yuno/internal/plans"
	"yuno/internal/store"
)

// Plans and the wave pause (DESIGN 5.29 rule 9).

// wavesPauseReasonMax is WavesPauseInput.reason's cap in characters.
const wavesPauseReasonMax = 500

// planDetail is PlanDetail: the plan, its tickets by wave, its newest events, the wave
// watcher's last read of the default branch (null before the first) and the ship runs of
// its waves (DESIGN 5.29 rules 11 and 12).
type planDetail struct {
	Plan     *store.Plan        `json:"plan"`
	Tickets  []store.PlanTicket `json:"tickets"`
	Events   []store.PlanEvent  `json:"events"`
	Main     *store.PlanMain    `json:"main"`
	ShipRuns []store.ShipRun    `json:"ship_runs"`
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
	main, err := store.GetPlanMain(ctx, s.pool, plan.ID)
	if err != nil {
		s.internalError(w, r, "read the plan's main status", err)
		return
	}
	ships, err := store.ListShipRuns(ctx, s.pool, plan.ID)
	if err != nil {
		s.internalError(w, r, "list the plan's ship runs", err)
		return
	}
	writeJSON(w, http.StatusOK, planDetail{Plan: plan, Tickets: tickets, Events: evs, Main: main, ShipRuns: ships})
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

// changesNotesMax is ChangesRequestInput.notes' cap in characters.
const changesNotesMax = 4000

// waveTemplateKey is the template of the wave pipeline, which builds a ticket (DESIGN 5.29).
const waveTemplateKey = "wave_pipeline"

type changesRequestInput struct {
	Notes string `json:"notes"`
}

// requestChanges: POST /api/plans/{id}/tickets/{ticket}/changes. It starts a run of the
// project's wave pipeline on the ticket's open merge request (DESIGN 5.22 iteration), with
// the ticket's issue and the notes as its task, links the run to the ticket, moves the
// ticket back to building and records a `changes_requested` event. 409 when the plan is
// finished, the ticket has no open merge request or a run on that request is live.
func (s *Server) requestChanges(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in changesRequestInput
	if !decodeBody(w, r, &in, 1<<15) {
		return
	}
	notes := strings.TrimSpace(strings.ToValidUTF8(strings.ReplaceAll(in.Notes, "\x00", ""), ""))
	var fe fieldErrors
	if notes == "" {
		fe.add("notes", "is required")
	} else if utf8.RuneCountInString(notes) > changesNotesMax {
		fe.add("notes", "must be at most %d characters", changesNotesMax)
	}
	if len(fe) > 0 {
		writeValidation(w, "invalid changes request", fe)
		return
	}
	plan, err := store.GetPlan(ctx, s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "plan", err)
		return
	}
	var ticket *store.PlanTicket
	tickets, err := store.ListPlanTickets(ctx, s.pool, plan.ID)
	if err != nil {
		s.internalError(w, r, "list plan tickets", err)
		return
	}
	for i := range tickets {
		if tickets[i].TicketID == r.PathValue("ticket") {
			ticket = &tickets[i]
		}
	}
	if ticket == nil {
		writeError(w, http.StatusNotFound, "not_found", "ticket not found", nil)
		return
	}
	if plan.Status == store.PlanDone || plan.Status == store.PlanCancelled {
		writeError(w, http.StatusConflict, "plan_finished", "the plan is done or cancelled already", nil)
		return
	}
	if ticket.MergeRequestID == nil || ticket.Status != store.TicketInReview {
		writeError(w, http.StatusConflict, "no_open_merge_request", "the ticket has no merge request open for review", nil)
		return
	}
	mr, err := store.GetMergeRequest(ctx, s.pool, *ticket.MergeRequestID)
	if err != nil {
		s.storeError(w, r, "merge request", err)
		return
	}
	if mr.State != store.MergeRequestOpen {
		writeError(w, http.StatusConflict, "no_open_merge_request", "the ticket's merge request is "+mr.State, nil)
		return
	}
	live, err := store.LiveRunOnMergeRequest(ctx, s.pool, mr.ID, mr.RunID)
	if err != nil {
		s.internalError(w, r, "look for a live run on the merge request", err)
		return
	}
	if live != "" {
		writeError(w, http.StatusConflict, "run_live", "run "+live+" is working on the merge request", map[string]any{"run_id": live})
		return
	}
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", errEngineUnavailable.Error(), nil)
		return
	}
	wf, err := store.GetWorkflowByFromTemplate(ctx, s.pool, waveTemplateKey)
	if errors.Is(err, store.ErrNotFound) {
		wf, err = store.GetWorkflowByTemplateKey(ctx, s.pool, waveTemplateKey)
	}
	if err != nil {
		s.storeError(w, r, "wave pipeline workflow", err)
		return
	}
	task := s.changesTask(r, ticket, notes)
	input, _ := json.Marshal(map[string]string{"task": task})
	run, err := s.engine.Trigger(ctx, engine.TriggerRequest{WorkflowID: wf.ID, ProjectID: plan.ProjectID,
		Trigger: store.TriggerManual, Input: input, MergeRequestID: mr.ID})
	if err != nil {
		s.engineError(w, r, err)
		return
	}
	moved, err := store.LinkTicketIteration(ctx, s.pool, plan.ID, ticket.TicketID, run.ID)
	if err != nil {
		s.internalError(w, r, "link the iteration run to the ticket", err)
		return
	}
	if moved {
		detail, _ := json.Marshal(map[string]string{"notes": notes, "run_id": run.ID, "merge_request_id": mr.ID})
		tid := ticket.TicketID
		ev := &store.PlanEvent{PlanID: plan.ID, Kind: store.PlanEventChangesRequested, Wave: &ticket.Wave, TicketID: &tid, Detail: detail}
		if err := store.AddPlanEvent(ctx, s.pool, ev); err != nil {
			s.internalError(w, r, "record the changes request", err)
			return
		}
		s.plans.Publish(plan.ID, plan.ProjectID)
	}
	writeJSON(w, http.StatusCreated, run)
}

// changesTask is the iteration run's task: the issue the ticket was built from, as the
// ticket's last run took it, and the notes. A ticket whose run did not keep the issue is
// described by its title.
func (s *Server) changesTask(r *http.Request, t *store.PlanTicket, notes string) string {
	base := "Ticket " + t.TicketID + ": " + t.Title
	if t.RunID != nil {
		if prev, err := store.GetRun(r.Context(), s.pool, *t.RunID); err == nil {
			var in struct {
				Task string `json:"task"`
			}
			if json.Unmarshal(prev.Input, &in) == nil && strings.TrimSpace(in.Task) != "" {
				base = in.Task
			}
		}
	}
	return base + "\n\nChanges requested: " + notes
}
