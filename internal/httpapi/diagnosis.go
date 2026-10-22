package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"yuno/internal/liveness"
	"yuno/internal/obs"
	"yuno/internal/project"
	"yuno/internal/store"
)

// Liveness from evidence (DESIGN 5.12): what each live run is doing, with the timestamps
// that prove it, and a run's events a page at a time.

const (
	maxDiagnosedRuns  = 100 // GET /api/runs/diagnosis
	defaultEventsPage = 100
	maxEventsPage     = 500
)

// diagnosisThresholds is the thresholds table, in seconds: H, P, M, S and T.
type diagnosisThresholds struct {
	HeartbeatFreshS int64 `json:"heartbeat_fresh_s"`
	ProgressFreshS  int64 `json:"progress_fresh_s"`
	ModelStallS     int64 `json:"model_stall_s"`
	WorkerStaleS    int64 `json:"worker_stale_s"`
	ToolOverdueS    int64 `json:"tool_overdue_s"`
}

// runDiagnosis is RunDiagnosis.
type runDiagnosis struct {
	RunID      string              `json:"run_id"`
	Status     string              `json:"status"`
	Liveness   string              `json:"liveness"`
	Reason     string              `json:"reason"`
	CheckedAt  time.Time           `json:"checked_at"`
	Thresholds diagnosisThresholds `json:"thresholds"`
	Nodes      []nodeDiagnosis     `json:"nodes"`
	Pending    []pendingDiagnosis  `json:"pending"`
}

type nodeDiagnosis struct {
	NodeID         string          `json:"node_id"`
	Visit          int             `json:"visit"`
	BranchID       *string         `json:"branch_id"`
	Agent          string          `json:"agent"`
	Status         string          `json:"status"`
	Liveness       string          `json:"liveness"`
	Reason         string          `json:"reason"`
	HeartbeatAt    *time.Time      `json:"heartbeat_at"`
	ProgressAt     *time.Time      `json:"progress_at"`
	WaitingOn      string          `json:"waiting_on"`
	ModelWaitSince *time.Time      `json:"model_wait_since"`
	OpenCalls      []callDiagnosis `json:"open_calls"`
	LastEvent      *eventRef       `json:"last_event"`
}

type callDiagnosis struct {
	Name      string    `json:"name"`
	Summary   string    `json:"summary"`
	StartedAt time.Time `json:"started_at"`
	AgeS      int64     `json:"age_s"`
}

type eventRef struct {
	Seq   int64     `json:"seq"`
	Event string    `json:"event"`
	At    time.Time `json:"at"`
}

type pendingDiagnosis struct {
	ApprovalID  string    `json:"approval_id"`
	Kind        string    `json:"kind"`
	RequestedAt time.Time `json:"requested_at"`
	AgeS        int64     `json:"age_s"`
}

// getRunDiagnosis is GET /api/runs/{id}/diagnosis.
func (s *Server) getRunDiagnosis(w http.ResponseWriter, r *http.Request) {
	run, err := store.GetRun(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "run", err)
		return
	}
	out, err := s.diagnose(r.Context(), []store.Run{*run})
	if err != nil {
		s.internalError(w, r, "diagnose run", err)
		return
	}
	writeJSON(w, http.StatusOK, out[0])
}

// listRunDiagnoses is GET /api/runs/diagnosis: every run that has not ended, newest first,
// at most 100, optionally only a project's.
func (s *Server) listRunDiagnoses(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	if hasControl(projectID) {
		// No project has such an id; Postgres would refuse a NUL with a 500.
		writeJSON(w, http.StatusOK, []runDiagnosis{})
		return
	}
	runs, err := store.ListLiveRuns(r.Context(), s.pool, projectID, maxDiagnosedRuns)
	if err != nil {
		s.internalError(w, r, "list live runs", err)
		return
	}
	out, err := s.diagnose(r.Context(), runs)
	if err != nil {
		s.internalError(w, r, "diagnose runs", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// diagnose classifies runs at one instant from their evidence (DESIGN 5.12 liveness rules
// and run liveness), with the configured M and S.
func (s *Server) diagnose(ctx context.Context, runs []store.Run) ([]runDiagnosis, error) {
	now := time.Now().UTC()
	th := liveness.NewThresholds(s.cfg.ModelStallAfter, s.cfg.EngineStaleAfter)
	var live []string
	for _, r := range runs {
		if !store.IsTerminal(r.Status) {
			live = append(live, r.ID)
		}
	}
	nodes, err := store.ListLiveNodes(ctx, s.pool, live)
	if err != nil {
		return nil, err
	}
	approvals, err := store.ListPendingApprovalsOfRuns(ctx, s.pool, live)
	if err != nil {
		return nil, err
	}
	blocked, err := s.blockedRuns(ctx, runs, now)
	if err != nil {
		return nil, err
	}
	nodesOf := map[string][]store.LiveNode{}
	for _, n := range nodes {
		nodesOf[n.RunID] = append(nodesOf[n.RunID], n)
	}
	pendingOfRun, pendingOfNode := map[string][]liveness.Pending{}, map[string][]liveness.Pending{}
	for _, a := range approvals {
		p := liveness.Pending{ID: a.ID, Kind: a.Kind, RequestedAt: a.RequestedAt}
		pendingOfRun[a.RunID] = append(pendingOfRun[a.RunID], p)
		if a.RunNodeID != "" {
			pendingOfNode[a.RunNodeID] = append(pendingOfNode[a.RunNodeID], p)
		}
	}
	thresholds := diagnosisThresholds{HeartbeatFreshS: seconds(th.H), ProgressFreshS: seconds(th.P), ModelStallS: seconds(th.M),
		WorkerStaleS: seconds(th.S), ToolOverdueS: seconds(th.T)}
	out := make([]runDiagnosis, 0, len(runs))
	for _, r := range runs {
		d := runDiagnosis{RunID: r.ID, Status: r.Status, CheckedAt: now, Thresholds: thresholds,
			Nodes: []nodeDiagnosis{}, Pending: []pendingDiagnosis{}}
		in := liveness.Run{Status: r.Status, UpdatedAt: r.UpdatedAt, PausedAt: r.PausedAt, Blocked: blocked[r.ID],
			Pending: pendingOfRun[r.ID]}
		if r.FailureReason != nil {
			in.FailureReason = *r.FailureReason
		}
		for _, n := range nodesOf[r.ID] {
			var calls []liveness.OpenCall
			if err := json.Unmarshal(n.OpenCalls, &calls); err != nil {
				calls = nil // unreadable evidence is missing evidence
			}
			v := liveness.ClassifyNode(liveness.Node{NodeID: n.NodeID, Parked: n.Parked, HeartbeatAt: n.HeartbeatAt,
				ProgressAt: n.ProgressAt, WaitingOn: n.WaitingOn, ModelWaitSince: n.ModelWaitSince, OpenCalls: calls,
				Pending: pendingOfNode[n.ID]}, th, now)
			in.Nodes = append(in.Nodes, v)
			d.Nodes = append(d.Nodes, nodeView(n, v))
		}
		d.Liveness, d.Reason = liveness.ClassifyRun(in, th, now)
		for _, p := range in.Pending {
			d.Pending = append(d.Pending, pendingDiagnosis{ApprovalID: p.ID, Kind: p.Kind, RequestedAt: p.RequestedAt.UTC(),
				AgeS: max(0, int64(now.Sub(p.RequestedAt)/time.Second))})
		}
		out = append(out, d)
	}
	return out, nil
}

// nodeView is a live visit as the diagnosis lists it.
func nodeView(n store.LiveNode, v liveness.Verdict) nodeDiagnosis {
	d := nodeDiagnosis{NodeID: n.NodeID, Visit: n.Visit, BranchID: n.BranchID, Agent: n.Agent, Status: n.Status,
		Liveness: v.Liveness, Reason: v.Reason, HeartbeatAt: utc(n.HeartbeatAt), ProgressAt: utc(n.ProgressAt),
		WaitingOn: n.WaitingOn, ModelWaitSince: utc(n.ModelWaitSince), OpenCalls: []callDiagnosis{}}
	for _, c := range v.OpenCalls {
		d.OpenCalls = append(d.OpenCalls, callDiagnosis{Name: c.Name, Summary: c.Summary, StartedAt: c.StartedAt.UTC(), AgeS: c.AgeS})
	}
	if n.LastEventSeq != nil && n.LastEvent != nil && n.LastEventAt != nil {
		d.LastEvent = &eventRef{Seq: *n.LastEventSeq, Event: *n.LastEvent, At: n.LastEventAt.UTC()}
	}
	return d
}

// blockedRuns says why each queued run of a project cannot go on: the project has reached a
// budget cap (the budget check's own refusal, DESIGN 5.19 rule 3), or its waves are paused
// (5.29 rule 9). A run of a deleted project, or of none, is not blocked.
func (s *Server) blockedRuns(ctx context.Context, runs []store.Run, now time.Time) (map[string]string, error) {
	out := map[string]string{}
	why := map[string]string{} // project id → reason, read once per project
	for _, r := range runs {
		if r.Status != store.RunQueued || r.ProjectID == nil {
			continue
		}
		reason, seen := why[*r.ProjectID]
		if !seen {
			p, err := store.GetProject(ctx, s.pool, *r.ProjectID)
			switch {
			case errors.Is(err, store.ErrNotFound):
			case err != nil:
				return nil, err
			default:
				var parts []string
				var be *project.BudgetError
				if err := project.CheckBudget(ctx, s.pool, p, now); errors.As(err, &be) {
					parts = append(parts, be.Error())
				} else if err != nil {
					return nil, err
				}
				if p.WavesPaused {
					paused := "the project's waves are paused"
					if p.WavesPausedReason != nil && strings.TrimSpace(*p.WavesPausedReason) != "" {
						paused += ": " + strings.TrimSpace(*p.WavesPausedReason)
					}
					parts = append(parts, paused)
				}
				reason = strings.Join(parts, "; ")
			}
			why[*r.ProjectID] = reason
		}
		if reason != "" {
			out[r.ID] = reason
		}
	}
	return out, nil
}

func seconds(d time.Duration) int64 { return int64(d / time.Second) }

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// runEventsPage is RunEventsPage.
type runEventsPage struct {
	Events    []pagedEvent `json:"events"`
	NextAfter int64        `json:"next_after"`
}

type pagedEvent struct {
	Seq   int64           `json:"seq"`
	Event string          `json:"event"`
	At    time.Time       `json:"at"`
	Data  json.RawMessage `json:"data"`
}

// listRunEventsPage is GET /api/runs/{id}/events/page: the run's durable events after a
// sequence number, oldest first, a page at a time, each redacted again on the way out.
func (s *Server) listRunEventsPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var fe fieldErrors
	var after int64
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			fe.add("after", "must be a non-negative integer")
		} else {
			after = n
		}
	}
	limit := defaultEventsPage
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxEventsPage {
			fe.add("limit", "must be an integer from 1 to %d", maxEventsPage)
		} else {
			limit = n
		}
	}
	var types []string
	for _, t := range strings.Split(q.Get("event"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			if hasControl(t) {
				fe.add("event", "must be event types separated by commas")
				break
			}
			types = append(types, t)
		}
	}
	if len(fe) > 0 {
		writeValidation(w, "invalid query", fe)
		return
	}
	run, err := store.GetRun(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "run", err)
		return
	}
	list, err := store.RunEventsPage(r.Context(), s.pool, run.ID, after, limit, types)
	if err != nil {
		s.internalError(w, r, "list run events", err)
		return
	}
	out := runEventsPage{Events: make([]pagedEvent, 0, len(list)), NextAfter: after}
	for _, e := range list {
		out.Events = append(out.Events, pagedEvent{Seq: e.Seq, Event: e.Type, At: e.At.UTC(), Data: obs.RedactJSON(e.Data)})
		out.NextAfter = e.Seq
	}
	writeJSON(w, http.StatusOK, out)
}
