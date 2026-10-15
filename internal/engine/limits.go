package engine

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"yuno/internal/config"
	"yuno/internal/store"
)

// transitions is the run state machine (DESIGN 5.1): every pair not listed is refused.
// running → queued is the stale-run re-queue; waiting_approval → running resumes a turn
// whose exec approval was decided while OpenClaw kept the turn open.
var transitions = map[string][]string{
	store.RunQueued:          {store.RunRunning, store.RunCancelled},
	store.RunRunning:         {store.RunQueued, store.RunWaitingApproval, store.RunSucceeded, store.RunFailed, store.RunCancelled, store.RunTimedOut},
	store.RunWaitingApproval: {store.RunQueued, store.RunRunning, store.RunFailed, store.RunCancelled, store.RunTimedOut},
}

// RunStatuses lists the seven run statuses in state-machine order.
var RunStatuses = []string{
	store.RunQueued, store.RunRunning, store.RunWaitingApproval,
	store.RunSucceeded, store.RunFailed, store.RunCancelled, store.RunTimedOut,
}

// TransitionError is a refused status change.
type TransitionError struct {
	From, To string
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("invalid transition %s→%s", e.From, e.To)
}

// ValidTransition returns nil when the run state machine allows from → to.
func ValidTransition(from, to string) error {
	for _, t := range transitions[from] {
		if t == to {
			return nil
		}
	}
	return &TransitionError{From: from, To: to}
}

// ValidationError lists path-qualified messages for a bad trigger request (422).
type ValidationError struct {
	Messages []string
}

func (e *ValidationError) Error() string {
	return "invalid request: " + strings.Join(e.Messages, "; ")
}

// resolveLimits fills zero limits from the request, then the agent's own limits, then
// store.DefaultLimits, and clamps the result to the platform maximums (DESIGN 5.2).
func resolveLimits(req, agent store.Limits, cfg config.Config) (store.Limits, error) {
	return resolveLayeredLimits("agent limits.", req, agent, cfg)
}

// resolveLayeredLimits is resolveLimits with the second layer named by prefix: the
// agent's limits for a quick run, the workflow definition's for a workflow run.
func resolveLayeredLimits(prefix string, req, agent store.Limits, cfg config.Config) (store.Limits, error) {
	msgs := negativeLimits("limits.", req)
	// The second layer is validated on write; one stored before that rule (or written
	// around the API) is refused here rather than silently replaced.
	msgs = append(msgs, negativeLimits(prefix, agent)...)
	if len(msgs) > 0 {
		return store.Limits{}, &ValidationError{Messages: msgs}
	}
	d := store.DefaultLimits
	l := store.Limits{
		MaxSteps:               firstInt(req.MaxSteps, agent.MaxSteps, d.MaxSteps),
		MaxDurationSeconds:     firstInt(req.MaxDurationSeconds, agent.MaxDurationSeconds, d.MaxDurationSeconds),
		MaxNodeIterations:      firstInt(req.MaxNodeIterations, agent.MaxNodeIterations, d.MaxNodeIterations),
		MaxLoopVisits:          firstInt(req.MaxLoopVisits, agent.MaxLoopVisits, d.MaxLoopVisits),
		MaxCostUSD:             firstFloat(req.MaxCostUSD, agent.MaxCostUSD, d.MaxCostUSD),
		MaxToolCalls:           firstInt(req.MaxToolCalls, agent.MaxToolCalls, d.MaxToolCalls),
		ApprovalTimeoutSeconds: firstInt(req.ApprovalTimeoutSeconds, agent.ApprovalTimeoutSeconds, d.ApprovalTimeoutSeconds),
	}
	l.MaxSteps = clampInt(l.MaxSteps, cfg.MaxSteps)
	l.MaxDurationSeconds = clampInt(l.MaxDurationSeconds, cfg.MaxDurationSeconds)
	l.MaxNodeIterations = clampInt(l.MaxNodeIterations, cfg.MaxNodeIterations)
	l.MaxLoopVisits = clampInt(l.MaxLoopVisits, cfg.MaxLoopVisits)
	if cfg.MaxCostUSD > 0 && l.MaxCostUSD > cfg.MaxCostUSD {
		l.MaxCostUSD = cfg.MaxCostUSD
	}
	l.MaxToolCalls = clampInt(l.MaxToolCalls, cfg.MaxToolCalls)
	l.ApprovalTimeoutSeconds = clampInt(l.ApprovalTimeoutSeconds, cfg.ApprovalTimeoutMaxSec)
	return l, nil
}

// negativeLimits returns a validation message per negative limit (DESIGN 5.2).
func negativeLimits(prefix string, l store.Limits) []string {
	var msgs []string
	neg := func(name string, v float64) {
		if v < 0 {
			msgs = append(msgs, prefix+name+": must not be negative")
		}
	}
	neg("max_steps", float64(l.MaxSteps))
	neg("max_duration_seconds", float64(l.MaxDurationSeconds))
	neg("max_node_iterations", float64(l.MaxNodeIterations))
	neg("max_loop_visits", float64(l.MaxLoopVisits))
	neg("max_cost_usd", l.MaxCostUSD)
	neg("max_tool_calls", float64(l.MaxToolCalls))
	neg("approval_timeout_seconds", float64(l.ApprovalTimeoutSeconds))
	return msgs
}

func firstInt(vs ...int) int {
	for _, v := range vs {
		if v > 0 {
			return v
		}
	}
	return 0
}

func firstFloat(vs ...float64) float64 {
	for _, v := range vs {
		if v > 0 {
			return v
		}
	}
	return 0
}

func clampInt(v, max int) int {
	if max > 0 && v > max {
		return max
	}
	return v
}

// violation is a limit breach: the run status it ends in and the reason text.
type violation struct {
	status string
	reason string
}

// limitState is what a limit check looks at.
type limitState struct {
	run            *store.Run // freshly read: counters, cancel flag, started_at, pauses
	limits         store.Limits
	now            time.Time
	nodeIterations int  // steps within the current node visit
	finishAccepted bool // the node already delivered its result
}

// elapsed is wall time since the engine started the run minus time spent waiting for
// approvals (DESIGN 5.2).
func (s limitState) elapsed() time.Duration {
	if s.run.StartedAt == nil {
		return 0
	}
	paused := time.Duration(s.run.PausedTotalMs) * time.Millisecond
	if s.run.PausedAt != nil && s.now.After(*s.run.PausedAt) {
		paused += s.now.Sub(*s.run.PausedAt)
	}
	return s.now.Sub(*s.run.StartedAt) - paused
}

// checkLimits applies DESIGN 5.2 in order: cancellation, time, then the counters. Once
// a node's finish was accepted only cancellation and time still apply; the counters no
// longer matter because the result is in.
func checkLimits(s limitState) *violation {
	r, l := s.run, s.limits
	if r.CancelRequested {
		by := "user"
		if r.CancelBy != nil && *r.CancelBy != "" {
			by = *r.CancelBy
		}
		return &violation{store.RunCancelled, "cancelled by " + by}
	}
	if l.MaxDurationSeconds > 0 && s.elapsed() >= time.Duration(l.MaxDurationSeconds)*time.Second {
		return &violation{store.RunTimedOut, fmt.Sprintf("time limit %ds exceeded", l.MaxDurationSeconds)}
	}
	if s.finishAccepted {
		return nil
	}
	if l.MaxSteps > 0 && r.StepCount >= l.MaxSteps {
		return &violation{store.RunFailed, fmt.Sprintf("step limit %d exceeded", l.MaxSteps)}
	}
	if l.MaxCostUSD > 0 && r.CostUSDMicros >= int64(l.MaxCostUSD*1e6+0.5) {
		return &violation{store.RunFailed, "cost limit $" + formatUSD(l.MaxCostUSD) + " exceeded"}
	}
	if l.MaxNodeIterations > 0 && s.nodeIterations >= l.MaxNodeIterations {
		return &violation{store.RunFailed, fmt.Sprintf("node iteration limit %d exceeded", l.MaxNodeIterations)}
	}
	if l.MaxToolCalls > 0 && r.ToolCallCount >= l.MaxToolCalls {
		return &violation{store.RunFailed, fmt.Sprintf("tool call limit %d exceeded", l.MaxToolCalls)}
	}
	return nil
}

// formatUSD prints dollars with two decimals, or more when the amount needs them.
func formatUSD(x float64) string {
	s := strconv.FormatFloat(x, 'f', -1, 64)
	if i := strings.IndexByte(s, '.'); i < 0 || len(s)-i-1 <= 2 {
		return fmt.Sprintf("%.2f", x)
	}
	return s
}

// retryBackoff is the transport retry schedule: 1 s doubling, capped at 30 s.
func retryBackoff(attempt int) time.Duration {
	d := time.Second
	for i := 1; i < attempt && d < 30*time.Second; i++ {
		d *= 2
	}
	return min(d, 30*time.Second)
}

// retryableHTTPStatus are the provider statuses DESIGN 5.5 rule 3 retries.
var retryableHTTPStatus = map[int]bool{408: true, 409: true, 429: true, 500: true, 502: true, 503: true, 529: true}

// transportLike reports whether an OpenClaw errorKind or message describes a
// transport failure rather than a model or request error.
func transportLike(s string) bool {
	s = strings.ToLower(s)
	for _, k := range []string{"timeout", "timed out", "network", "transport", "connection", "econnreset",
		"econnrefused", "socket", "fetch failed", "unavailable", "overloaded", "rate_limit", "rate limit"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}
