package evals

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// simAPI is a fake api whose runs end the way the simulator says a correct run of the
// case ends: it serves the simulated trace for whatever case a request belongs to. It
// knows the endpoints the wave 3 runner actions use (schedules, tools, the model proxy)
// and a rate limit written after DESIGN 5.11 rule 2, so the runner is tested against the
// requests it really sends, not against a canned body.
type simAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu   sync.Mutex
	next int
	sims map[string]*simulator // by case id

	agents    map[string]AgentRequest
	workflows map[string]*simWorkflow
	scripts   map[string]bool
	runs      map[string]*simRun
	schedules map[string]*simSchedule

	runRequests      []RunRequest
	scheduleRequests []ScheduleRequest
	deletedAgents    []string
	deletedWorkflows []string
	deletedSchedules []string
	cancels          map[string]int
	decisions        int

	// Knobs.
	noRateLimit    bool     // the api does not enforce max_runs_per_hour
	fireAfterPolls int      // reads of a schedule before it fires; default 2
	neverFire      bool     // the schedule never creates a run
	scheduleError  string   // the schedule records this error instead of firing
	scheduleStatus int      // POST /api/schedules answers this status when not 0
	tools          []string // GET /api/tools; default the simulator's list
	toolsStatus    int      // GET /api/tools answers this status when not 0
	messages       *fakeMessages
}

type simWorkflow struct {
	caseID   string
	name     string
	agentIDs []string
}

type simRun struct {
	id          string
	caseID      string
	req         RunRequest
	polls       int
	cancelled   bool
	trigger     string
	scheduleRef string
}

type simSchedule struct {
	view  ScheduleView
	reads int
	req   ScheduleRequest
}

func newSimAPI(t *testing.T, cases ...Case) *simAPI {
	t.Helper()
	f := &simAPI{
		t: t, sims: map[string]*simulator{}, agents: map[string]AgentRequest{}, workflows: map[string]*simWorkflow{},
		scripts: map[string]bool{}, runs: map[string]*simRun{}, schedules: map[string]*simSchedule{}, cancels: map[string]int{},
		fireAfterPolls: 2,
	}
	for _, c := range cases {
		f.sims[c.ID] = simulate(t, c)
	}
	f.srv = httptest.NewServer(f.handler())
	t.Cleanup(f.srv.Close)
	return f
}

func (f *simAPI) id(prefix string) string {
	f.next++
	return fmt.Sprintf("%s-%d", prefix, f.next)
}

func (f *simAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ready")) })
	mux.HandleFunc("POST /api/agents", f.createAgent)
	mux.HandleFunc("DELETE /api/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deletedAgents = append(f.deletedAgents, r.PathValue("id"))
		f.mu.Unlock()
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /api/workflows", f.createWorkflow)
	mux.HandleFunc("DELETE /api/workflows/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deletedWorkflows = append(f.deletedWorkflows, r.PathValue("id"))
		f.mu.Unlock()
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /api/evals/scripts", func(w http.ResponseWriter, r *http.Request) {
		var reg scriptRegistration
		if err := json.NewDecoder(r.Body).Decode(&reg); err != nil || reg.ScriptID == "" {
			writeErr(w, 422, "bad_request", "bad script")
			return
		}
		f.mu.Lock()
		f.scripts[reg.ScriptID] = true
		f.mu.Unlock()
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /api/runs", f.createRun)
	mux.HandleFunc("GET /api/runs/{id}", f.getRun)
	mux.HandleFunc("POST /api/runs/{id}/cancel", f.cancelRun)
	mux.HandleFunc("GET /api/runs/{id}/trace", f.getTrace)
	mux.HandleFunc("GET /api/approvals", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, []ApprovalItem{}) })
	mux.HandleFunc("POST /api/approvals/{id}/approve", func(w http.ResponseWriter, r *http.Request) { f.decided(w) })
	mux.HandleFunc("POST /api/approvals/{id}/reject", func(w http.ResponseWriter, r *http.Request) { f.decided(w) })
	mux.HandleFunc("POST /api/schedules", f.createSchedule)
	mux.HandleFunc("GET /api/schedules/{id}", f.getSchedule)
	mux.HandleFunc("DELETE /api/schedules/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deletedSchedules = append(f.deletedSchedules, r.PathValue("id"))
		f.mu.Unlock()
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /api/tools", f.listTools)
	mux.HandleFunc("POST /model-proxy/anthropic/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if f.messages == nil {
			writeErr(w, 404, "not_found", "no model proxy")
			return
		}
		f.messages.ServeHTTP(w, r)
	})
	return (&fakeAPI{}).auth(mux)
}

func (f *simAPI) decided(w http.ResponseWriter) {
	f.mu.Lock()
	f.decisions++
	f.mu.Unlock()
	writeErr(w, 409, "conflict", "approval is not pending")
}

func (f *simAPI) createAgent(w http.ResponseWriter, r *http.Request) {
	var req AgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 422, "bad_request", err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.id("agent")
	f.agents[id] = req
	writeJSON(w, 201, store.Agent{ID: id, Name: req.Name, Model: req.Model})
}

func (f *simAPI) createWorkflow(w http.ResponseWriter, r *http.Request) {
	var req WorkflowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 422, "bad_request", err.Error())
		return
	}
	def, err := workflow.Parse(req.Definition)
	if err != nil {
		writeErr(w, 422, "validation_failed", err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := &simWorkflow{caseID: caseIDFromScript(strings.TrimPrefix(req.Name, "eval-")), name: req.Name}
	for _, n := range def.Nodes {
		if n.Kind == workflow.KindAgent {
			if _, ok := f.agents[n.AgentID]; !ok {
				writeErr(w, 422, "validation_failed", "nodes: unknown agent "+n.AgentID)
				return
			}
			rec.agentIDs = append(rec.agentIDs, n.AgentID)
		}
	}
	id := f.id("wf")
	f.workflows[id] = rec
	writeJSON(w, 201, store.Workflow{ID: id, Name: req.Name, Version: 1, Definition: req.Definition})
}

// runsOf counts the runs created for a workflow.
func (f *simAPI) runsOf(workflowID string) int {
	n := 0
	for _, r := range f.runs {
		if r.req.WorkflowID == workflowID {
			n++
		}
	}
	return n
}

func (f *simAPI) createRun(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 422, "bad_request", err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runRequests = append(f.runRequests, req)
	trigger := req.Trigger
	if trigger == "" {
		trigger = store.TriggerManual
	}
	run := &simRun{req: req, trigger: trigger}
	switch {
	case req.WorkflowID != "":
		wf, ok := f.workflows[req.WorkflowID]
		if !ok {
			writeErr(w, 404, "not_found", "no such workflow")
			return
		}
		run.caseID = wf.caseID
		if !f.noRateLimit {
			for _, aid := range wf.agentIDs {
				a := f.agents[aid]
				if max := a.Guardrails.MaxRunsPerHour; max > 0 && f.runsOf(req.WorkflowID) >= max {
					// The body the api sends (httpapi createRun): details is an object.
					writeJSON(w, 429, map[string]any{"error": map[string]any{"code": "rate_limited",
						"message": fmt.Sprintf("rate limit: %d runs/hour for agent %s", max, a.Name),
						"details": map[string]any{"agent_id": aid, "limit": max}}})
					return
				}
			}
		}
	case req.AgentID != "":
		a, ok := f.agents[req.AgentID]
		if !ok {
			writeErr(w, 404, "not_found", "no such agent")
			return
		}
		run.caseID = suffixPattern.ReplaceAllString(strings.TrimPrefix(a.Name, "eval-"), "")
	}
	if req.ScriptID != "" && req.Model != ScriptedModel {
		writeErr(w, 422, "validation_failed", "script_id needs the scripted model")
		return
	}
	run.id = f.id("run")
	f.runs[run.id] = run
	writeJSON(w, 201, f.runBody(run))
}

// runBody is the run as the api reports it now: running on the first read, then the
// simulated end. A case that waits for approval stays in waiting_approval until it is
// cancelled.
func (f *simAPI) runBody(run *simRun) store.Run {
	sim := f.sims[run.caseID]
	if sim == nil {
		f.t.Fatalf("the fake api has no simulation for case %q", run.caseID)
	}
	body := *sim.tr.Run
	body.ID, body.Trigger = run.id, run.trigger
	if run.trigger == store.TriggerSchedule {
		ref := run.scheduleRef
		body.TriggerRef = &ref
	}
	holds := sim.c.Actions.CancelWhenWaiting && !run.cancelled
	switch {
	case holds:
		body.Status, body.FinishedAt, body.FailureReason = store.RunWaitingApproval, nil, nil
	case run.polls == 0 && !run.cancelled:
		body.Status, body.FinishedAt, body.FailureReason = store.RunRunning, nil, nil
	}
	return body
}

func (f *simAPI) getRun(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, ok := f.runs[r.PathValue("id")]
	if !ok {
		writeErr(w, 404, "not_found", "no such run")
		return
	}
	body := f.runBody(run)
	run.polls++
	writeJSON(w, 200, body)
}

func (f *simAPI) cancelRun(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, ok := f.runs[r.PathValue("id")]
	if !ok {
		writeErr(w, 404, "not_found", "no such run")
		return
	}
	f.cancels[run.id]++
	if before := f.runBody(run); store.IsTerminal(before.Status) {
		writeErr(w, 409, "run_finished", "run is already "+before.Status)
		return
	}
	run.cancelled = true
	writeJSON(w, 202, f.runBody(run))
}

func (f *simAPI) getTrace(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, ok := f.runs[r.PathValue("id")]
	if !ok {
		writeErr(w, 404, "not_found", "no such run")
		return
	}
	tr := f.sims[run.caseID].tr
	final := *tr.Run
	final.ID, final.Trigger = run.id, run.trigger
	if run.trigger == store.TriggerSchedule {
		ref := run.scheduleRef
		final.TriggerRef = &ref
	}
	tr.Run = &final
	writeJSON(w, 200, tr)
}

func (f *simAPI) createSchedule(w http.ResponseWriter, r *http.Request) {
	var req ScheduleRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, 422, "validation_failed", err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scheduleRequests = append(f.scheduleRequests, req)
	if f.scheduleStatus != 0 {
		writeErr(w, f.scheduleStatus, "not_found", "no such route")
		return
	}
	if _, ok := f.workflows[req.WorkflowID]; !ok {
		writeErr(w, 422, "validation_failed", "workflow_id: unknown workflow")
		return
	}
	next := req.RunAt
	s := &simSchedule{req: req, view: ScheduleView{ID: f.id("sch"), WorkflowID: req.WorkflowID, Kind: req.Kind, Enabled: req.Enabled, NextRunAt: &next}}
	f.schedules[s.view.ID] = s
	writeJSON(w, 201, s.view)
}

// getSchedule fires the schedule on its Nth read: it creates the run, records it and, as
// DESIGN 5.9 rule 2 says of a delay schedule, disables itself.
func (f *simAPI) getSchedule(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.schedules[r.PathValue("id")]
	if !ok {
		writeErr(w, 404, "not_found", "no such schedule")
		return
	}
	s.reads++
	if s.reads >= f.fireAfterPolls && s.view.LastRunID == nil && s.view.LastError == nil && s.view.Enabled {
		switch {
		case f.neverFire:
		case f.scheduleError != "":
			msg := f.scheduleError
			s.view.LastError, s.view.Enabled = &msg, false
		default:
			wf := f.workflows[s.req.WorkflowID]
			run := &simRun{id: f.id("run"), caseID: wf.caseID, req: RunRequest{WorkflowID: s.req.WorkflowID, Input: s.req.Input},
				trigger: store.TriggerSchedule, scheduleRef: store.ScheduleOccurrenceRef(s.view.ID, time.Now())}
			f.runs[run.id] = run
			id, now := run.id, time.Now().UTC()
			s.view.LastRunID, s.view.LastRunAt, s.view.Enabled, s.view.NextRunAt = &id, &now, false, nil
		}
	}
	writeJSON(w, 200, s.view)
}

func (f *simAPI) listTools(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.toolsStatus != 0 {
		writeErr(w, f.toolsStatus, "internal", "tool registry unavailable")
		return
	}
	names := f.tools
	if names == nil {
		for _, sim := range f.sims {
			names = sim.toolList()
			break
		}
	}
	out := make([]map[string]any, len(names))
	for i, n := range names {
		out[i] = map[string]any{"name": n, "risk": "computational", "source": "platform"}
	}
	writeJSON(w, 200, out)
}
