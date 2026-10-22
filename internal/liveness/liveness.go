// Package liveness says what a live run is doing from the evidence its visits record
// (DESIGN 5.12, liveness from evidence): executing, waiting, blocked, unknown or stalled,
// with the reason and the ages that prove it. A "running" status alone proves nothing, and
// missing telemetry never reads as executing (rules 1, 3 and 7).
//
// The package is pure: the caller reads the evidence and passes the time of the check.
package liveness

import (
	"fmt"
	"time"

	"yuno/internal/store"
)

// The liveness of a node or a run. A terminal run reports its status instead.
const (
	Executing = "executing"
	Waiting   = "waiting"
	Blocked   = "blocked"
	Unknown   = "unknown"
	Stalled   = "stalled"
)

// What a live visit waits on (run_nodes.waiting_on); "" is nothing the visit can name.
const (
	WaitModel = store.WaitingOnModel
	WaitTool  = store.WaitingOnTool
	WaitHuman = store.WaitingOnHuman
	WaitSetup = store.WaitingOnSetup
)

// The thresholds of DESIGN 5.12 that do not come from the configuration.
const (
	// HeartbeatEvery is the worker's tick: a live visit writes heartbeat_at this often, and
	// progress_at at most this often.
	HeartbeatEvery = 30 * time.Second
	// HeartbeatFresh (H) is three ticks: an older heartbeat proves nothing.
	HeartbeatFresh = 3 * HeartbeatEvery
	// ProgressFresh (P) is how recent progress must be to read as executing.
	ProgressFresh = 120 * time.Second
	// ToolOverdue (T) is the gateway's 1800 s exec timeout plus a minute: an open call older
	// than that is past any timeout that would have ended it.
	ToolOverdue = 1860 * time.Second
	// SummaryChars caps an open call's summary (the command or path), in characters.
	SummaryChars = 200
)

// Thresholds are the five limits the rules compare ages with. M (MODEL_STALL_AFTER) and S
// (ENGINE_STALE_AFTER) come from the configuration; zero or less means the platform does not
// act on that silence, and the rule that names it never says stalled.
type Thresholds struct {
	H, P, M, S, T time.Duration
}

// NewThresholds is the table of DESIGN 5.12 with the configured M and S.
func NewThresholds(modelStall, staleAfter time.Duration) Thresholds {
	return Thresholds{H: HeartbeatFresh, P: ProgressFresh, M: modelStall, S: staleAfter, T: ToolOverdue}
}

// OpenCall is a tool call of the turn with no result yet, as run_nodes.open_calls holds it.
type OpenCall struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	StartedAt time.Time `json:"started_at"`
	Summary   string    `json:"summary"`
}

// Pending is an approval or question nobody decided yet.
type Pending struct {
	ID          string
	Kind        string
	RequestedAt time.Time
}

// Node is the evidence of one live visit. Pending are the approvals and questions the visit
// raised that are still pending, oldest first; Parked says the visit parked on one of them
// and released its worker (DESIGN 5.24 rule 5).
type Node struct {
	NodeID string
	Parked bool
	// Paused says the visit parked because the operator paused it (DESIGN 5.33 rule 14).
	Paused         bool
	HeartbeatAt    *time.Time
	ProgressAt     *time.Time
	WaitingOn      string
	ModelWaitSince *time.Time
	OpenCalls      []OpenCall
	Pending        []Pending
}

// CallAge is an open call with its age at the check.
type CallAge struct {
	OpenCall
	AgeS int64
}

// Verdict is a liveness, its reason, and the evidence's ages at the check, in seconds (nil
// where the evidence is missing). NodeID is the visit's workflow node, for the run's reason.
type Verdict struct {
	NodeID        string
	Liveness      string
	Reason        string
	HeartbeatAgeS *int64
	ProgressAgeS  *int64
	ModelWaitAgeS *int64
	OpenCalls     []CallAge
}

// ClassifyNode applies DESIGN 5.12 liveness rules 1 to 7, in order, to one live visit at now.
func ClassifyNode(n Node, th Thresholds, now time.Time) Verdict {
	v := Verdict{NodeID: n.NodeID, HeartbeatAgeS: ageS(now, n.HeartbeatAt), ProgressAgeS: ageS(now, n.ProgressAt)}
	if n.WaitingOn == WaitModel {
		v.ModelWaitAgeS = ageS(now, n.ModelWaitSince)
	}
	for _, c := range n.OpenCalls {
		v.OpenCalls = append(v.OpenCalls, CallAge{OpenCall: c, AgeS: secs(age(now, c.StartedAt))})
	}
	// A parked visit has no worker and no live turn: it waits on its decision, and its run's
	// waiting_approval is what the run rule reads (DESIGN 5.12 edge cases).
	if n.Parked {
		v.Liveness, v.Reason = Waiting, "parked, "+waitingOn(n.Pending, now)
		if n.Paused {
			v.Reason = "paused by the operator; a resume continues the step"
		}
		return v
	}
	// Rules 1 to 3: the worker's heartbeat.
	if n.HeartbeatAt == nil {
		v.Liveness, v.Reason = Unknown, "no heartbeat recorded"
		return v
	}
	hb := age(now, *n.HeartbeatAt)
	if th.S > 0 && hb > th.S {
		v.Liveness, v.Reason = Stalled, fmt.Sprintf("worker silent %s; the sweeper re-queues after %s", text(hb), text(th.S))
		return v
	}
	if hb > th.H {
		v.Liveness, v.Reason = Unknown, fmt.Sprintf("heartbeat %s old", text(hb))
		return v
	}
	switch n.WaitingOn {
	case WaitHuman: // rule 4
		v.Liveness, v.Reason = Waiting, waitingOn(n.Pending, now)
	case WaitTool: // rule 5
		v.Liveness, v.Reason = toolRule(n.OpenCalls, th, now)
	case WaitModel: // rule 6
		v.Liveness, v.Reason = modelRule(n, th, now)
	case WaitSetup, "": // rule 7
		v.Liveness, v.Reason = activityRule(n, th, now)
	default:
		v.Liveness, v.Reason = Unknown, fmt.Sprintf("waiting on %q, which no rule names", n.WaitingOn)
	}
	return v
}

// waitingOn names the oldest pending decision and its age (rule 4).
func waitingOn(pending []Pending, now time.Time) string {
	if len(pending) == 0 {
		return "waiting on a decision"
	}
	p := pending[0]
	var what string
	switch p.Kind {
	case store.ApprovalKindQuestion:
		what = "question " + p.ID
	case "", store.ApprovalKindNode:
		what = "approval " + p.ID
	default:
		what = p.Kind + " approval " + p.ID
	}
	return fmt.Sprintf("waiting on %s for %s", what, text(age(now, p.RequestedAt)))
}

// toolRule is rule 5: the oldest open call decides. An open call without a start time is
// missing telemetry.
func toolRule(calls []OpenCall, th Thresholds, now time.Time) (string, string) {
	if len(calls) == 0 {
		return Unknown, "waiting on a tool call that was not recorded"
	}
	oldest := calls[0]
	for _, c := range calls[1:] {
		if c.StartedAt.IsZero() || c.StartedAt.Before(oldest.StartedAt) {
			oldest = c
		}
	}
	if oldest.StartedAt.IsZero() {
		return Unknown, fmt.Sprintf("%s running; its start was not recorded", oldest.Name)
	}
	a := age(now, oldest.StartedAt)
	if a > th.T {
		return Stalled, fmt.Sprintf("%s running %s, past its timeout", oldest.Name, text(a))
	}
	what := oldest.Name
	if oldest.Summary != "" {
		what += " " + oldest.Summary
	}
	return Executing, fmt.Sprintf("running %s for %s", what, text(a))
}

// modelRule is rule 6. The wait is counted from the later of model_wait_since and
// progress_at: a reply streaming in restarts it, as it restarts the engine's own stall
// window (DESIGN 5.5 rule 6), so the verdict says what the engine will do.
func modelRule(n Node, th Thresholds, now time.Time) (string, string) {
	since := n.ModelWaitSince
	if since == nil {
		return Unknown, "waiting on the model; the wait's start was not recorded"
	}
	if n.ProgressAt != nil && n.ProgressAt.After(*since) {
		since = n.ProgressAt
	}
	a := age(now, *since)
	switch {
	case a <= th.P:
		return Executing, "model replying"
	case th.M <= 0:
		return Waiting, fmt.Sprintf("waiting on the model %s; no restart is configured", text(a))
	case a <= th.M:
		return Waiting, fmt.Sprintf("waiting on the model %s; restarted at %s", text(a), text(th.M))
	}
	return Stalled, fmt.Sprintf("waiting on the model %s, past the %s restart", text(a), text(th.M))
}

// activityRule is rule 7: setup, or nothing the visit can name, reads as executing only
// on recent progress.
func activityRule(n Node, th Thresholds, now time.Time) (string, string) {
	if n.ProgressAt == nil {
		return Unknown, "no activity recorded"
	}
	a := age(now, *n.ProgressAt)
	if a > th.P {
		return Unknown, fmt.Sprintf("no activity for %s", text(a))
	}
	if n.WaitingOn == WaitSetup {
		return Executing, fmt.Sprintf("setting up the step; last activity %s ago", text(a))
	}
	return Executing, fmt.Sprintf("last activity %s ago", text(a))
}

// Run is what the run rule reads. Blocked is why a queued run cannot start, or why a run
// parked for its merge request's dependencies waits ("" when nothing blocks it); Pending are
// the run's pending approvals and questions, oldest first; Nodes are the verdicts of its live
// visits.
type Run struct {
	Status        string
	FailureReason string
	UpdatedAt     time.Time
	PausedAt      *time.Time
	Blocked       string
	Pending       []Pending
	Nodes         []Verdict
}

// The order of DESIGN 5.12 run liveness, worst first.
var severity = map[string]int{Stalled: 5, Unknown: 4, Blocked: 3, Waiting: 2, Executing: 1}

// ClassifyRun applies the run liveness rules of DESIGN 5.12 at now.
func ClassifyRun(r Run, th Thresholds, now time.Time) (string, string) {
	switch r.Status {
	case store.RunQueued:
		if r.Blocked != "" {
			return Blocked, r.Blocked
		}
		return Waiting, "queued " + text(age(now, r.UpdatedAt))
	case store.RunWaitingApproval:
		// A parked visit is the same: its run waits on the decision.
		if len(r.Pending) == 0 && r.PausedAt != nil {
			return Waiting, "paused " + text(age(now, *r.PausedAt)) + ", waiting on a decision"
		}
		return Waiting, waitingOn(r.Pending, now)
	case store.RunPaused:
		// Chief platform control, DESIGN 5.34 rule 7: held by its merge request's
		// dependencies until the watcher re-queues it.
		if r.Blocked != "" {
			return Blocked, r.Blocked
		}
		// DESIGN 5.33 rule 14: no worker holds it until the operator resumes it.
		if r.PausedAt != nil {
			return Waiting, "paused by the operator " + text(age(now, *r.PausedAt)) + " ago; waiting for a resume"
		}
		return Waiting, "paused by the operator; waiting for a resume"
	case store.RunRunning:
		if len(r.Nodes) == 0 {
			// Between steps: the run row's own write time is the only evidence.
			a := age(now, r.UpdatedAt)
			switch {
			case a <= th.H:
				return Executing, fmt.Sprintf("between steps; the run was updated %s ago", text(a))
			case th.S <= 0 || a <= th.S:
				return Unknown, fmt.Sprintf("no step running; the run was updated %s ago", text(a))
			}
			return Stalled, fmt.Sprintf("no step running and the run silent for %s", text(a))
		}
		worst := 0
		for i, n := range r.Nodes {
			if severity[n.Liveness] > severity[r.Nodes[worst].Liveness] {
				worst = i
			}
		}
		n := r.Nodes[worst]
		if n.NodeID == "" {
			return n.Liveness, n.Reason
		}
		return n.Liveness, n.NodeID + ": " + n.Reason
	}
	// A terminal run reports its status.
	return r.Status, r.FailureReason
}

func age(now, t time.Time) time.Duration {
	if d := now.Sub(t); d > 0 {
		return d
	}
	return 0
}

func ageS(now time.Time, t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	s := secs(age(now, *t))
	return &s
}

func secs(d time.Duration) int64 { return int64(d / time.Second) }

// text writes an age to the second as Go writes durations (95 s is "1m35s").
func text(d time.Duration) string { return d.Truncate(time.Second).String() }
