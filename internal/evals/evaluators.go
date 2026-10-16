package evals

import (
	"fmt"
	"strings"
	"time"

	"yuno/internal/store"
)

// Evaluator names, as they appear in reports.
const (
	EvalTermination    = "termination"
	EvalTraceIntegrity = "trace_integrity"
	EvalExpectation    = "expectation"
	EvalApprovalGate   = "approval_gate"
	EvalIdempotency    = "idempotency"
	EvalSchema         = "schema"
	EvalAllowlist      = "allowlist"
)

// EvalResult is the outcome of one deterministic evaluator on one case.
type EvalResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

type namedEvaluator struct {
	name string
	fn   func(Case, store.Trace, Observed) EvalResult
}

// fromTrace adapts an evaluator that reads the trace alone.
func fromTrace(fn func(Case, store.Trace) EvalResult) func(Case, store.Trace, Observed) EvalResult {
	return func(c Case, t store.Trace, _ Observed) EvalResult { return fn(c, t) }
}

// deterministic is the ordered set of evaluators every case is judged by.
var deterministic = []namedEvaluator{
	{EvalTermination, fromTrace(evalTermination)},
	{EvalTraceIntegrity, fromTrace(evalTraceIntegrity)},
	{EvalExpectation, evalExpectationObserved},
	{EvalApprovalGate, fromTrace(evalApprovalGate)},
	{EvalIdempotency, fromTrace(evalIdempotency)},
	{EvalSchema, fromTrace(evalSchema)},
	{EvalAllowlist, fromTrace(evalAllowlist)},
}

// EvaluatorNames lists the deterministic evaluators in report order.
func EvaluatorNames() []string {
	names := make([]string, len(deterministic))
	for i, e := range deterministic {
		names[i] = e.name
	}
	return names
}

// Evaluate runs every deterministic evaluator over the trace alone. A case whose
// expectations read what the runner observed (a refused trigger, the tool list, a
// schedule) needs EvaluateObserved.
func Evaluate(c Case, t store.Trace) []EvalResult {
	return EvaluateObserved(c, t, Observed{})
}

// EvaluateObserved runs every deterministic evaluator over the trace and what the runner
// observed. A panic inside an evaluator is reported as that evaluator failing; it never
// escapes.
func EvaluateObserved(c Case, t store.Trace, o Observed) []EvalResult {
	out := make([]EvalResult, 0, len(deterministic))
	for _, e := range deterministic {
		out = append(out, safeEvaluate(e, c, t, o))
	}
	return out
}

func safeEvaluate(e namedEvaluator, c Case, t store.Trace, o Observed) (res EvalResult) {
	defer func() {
		if p := recover(); p != nil {
			res = EvalResult{Name: e.name, Detail: fmt.Sprintf("evaluator panic: %v", p)}
		}
	}()
	return e.fn(c, t, o)
}

// failedResults marks every deterministic evaluator as failed for a case the runner
// could not evaluate (no trace), carrying the reason.
func failedResults(reason string) []EvalResult {
	out := make([]EvalResult, 0, len(deterministic))
	for _, e := range deterministic {
		out = append(out, EvalResult{Name: e.name, Detail: reason})
	}
	return out
}

// effectiveLimits resolves the limits a run is judged against: the run's own snapshot
// (already defaulted and clamped by the api) first, then the limits the case asked for
// (the agent's, or the workflow definition's), then the platform defaults, field by
// field.
func effectiveLimits(c Case, run *store.Run) store.Limits {
	caseLimits := c.RunLimits()
	pickInt := func(runV, caseV, defV int) int {
		switch {
		case runV > 0:
			return runV
		case caseV > 0:
			return caseV
		}
		return defV
	}
	return store.Limits{
		MaxSteps:           pickInt(run.Limits.MaxSteps, caseLimits.MaxSteps, store.DefaultLimits.MaxSteps),
		MaxDurationSeconds: pickInt(run.Limits.MaxDurationSeconds, caseLimits.MaxDurationSeconds, store.DefaultLimits.MaxDurationSeconds),
	}
}

// timeoutEnforcementWindow is how long after max_duration_seconds a run the engine timed
// out may finish: one 2 s limit tick plus the 10 s wait for the abort (DESIGN 5.2).
const timeoutEnforcementWindow = 12 * time.Second

// evalTermination checks that the run ended and stayed within its limits: a terminal
// status with a finish time, step_count within max_steps, and active time (finished minus
// started minus time paused for approvals) within max_duration_seconds.
//
// A run that failed because it hit the step limit is allowed one step over: the step
// that trips the limit is recorded before the check can abort the turn (DESIGN 5.2). A
// run that timed out is allowed the engine's enforcement window past its time limit.
func evalTermination(c Case, t store.Trace) EvalResult {
	res := EvalResult{Name: EvalTermination}
	run := t.Run
	if run == nil {
		res.Detail = "trace has no run"
		return res
	}
	var problems []string

	terminal := store.IsTerminal(run.Status)
	if !terminal {
		problems = append(problems, fmt.Sprintf("status %q is not terminal", run.Status))
	}
	if terminal && run.FinishedAt == nil {
		problems = append(problems, "terminal run has no finished_at")
	}

	lim := effectiveLimits(c, run)
	stepBound := lim.MaxSteps
	if run.FailureReason != nil && strings.Contains(strings.ToLower(*run.FailureReason), "step limit") {
		stepBound++
	}
	if run.StepCount > stepBound {
		problems = append(problems, fmt.Sprintf("step_count %d exceeds max_steps %d", run.StepCount, lim.MaxSteps))
	}

	var active time.Duration
	if run.StartedAt != nil && run.FinishedAt != nil {
		active = run.FinishedAt.Sub(*run.StartedAt) - time.Duration(run.PausedTotalMs)*time.Millisecond
		limit := time.Duration(lim.MaxDurationSeconds) * time.Second
		if run.Status == store.RunTimedOut {
			// The engine notices the deadline on its limit tick and then waits for OpenClaw
			// to confirm the abort before it writes finished_at (DESIGN 5.2).
			limit += timeoutEnforcementWindow
		}
		if active > limit {
			problems = append(problems, fmt.Sprintf("active duration %s exceeds max_duration_seconds %d",
				active.Round(time.Millisecond), lim.MaxDurationSeconds))
		}
	}

	if len(problems) > 0 {
		res.Detail = strings.Join(problems, "; ")
		return res
	}
	res.Passed = true
	res.Detail = fmt.Sprintf("status %s, %d of %d steps, %s of %ds",
		run.Status, run.StepCount, lim.MaxSteps, active.Round(time.Millisecond), lim.MaxDurationSeconds)
	return res
}

// terminalToolStatuses are the tool call statuses that mean the call is settled.
var terminalToolStatuses = map[string]bool{
	store.ToolExecuted: true,
	store.ToolRejected: true,
	store.ToolBlocked:  true,
	store.ToolExpired:  true,
	store.ToolFailed:   true,
}

// maxIntegrityExamples bounds how many individual violations a detail lists.
const maxIntegrityExamples = 3

// evalTraceIntegrity checks that the stored trace is complete and sane: every tool call
// has a terminal status and a finish time, every model call has a model and non-negative
// token counts, and the run has at least one span.
func evalTraceIntegrity(_ Case, t store.Trace) EvalResult {
	res := EvalResult{Name: EvalTraceIntegrity}
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if t.Run == nil {
		add("trace has no run")
	}
	for _, tc := range t.ToolCalls {
		if !terminalToolStatuses[tc.Status] {
			add("tool call %s (%s) has non-terminal status %q", tc.ID, tc.ToolName, tc.Status)
		}
		if tc.FinishedAt == nil {
			add("tool call %s (%s) has no finished_at", tc.ID, tc.ToolName)
		}
	}
	for _, mc := range t.ModelCalls {
		if strings.TrimSpace(mc.Model) == "" {
			add("model call %s has no model", mc.ID)
		}
		if mc.InputTokens < 0 || mc.OutputTokens < 0 || mc.CacheReadTokens < 0 || mc.CacheWriteTokens < 0 {
			add("model call %s has negative tokens", mc.ID)
		}
	}
	if len(t.Spans) == 0 {
		add("run has no spans")
	}

	if len(problems) > 0 {
		shown := problems
		if len(shown) > maxIntegrityExamples {
			shown = shown[:maxIntegrityExamples]
		}
		res.Detail = strings.Join(shown, "; ")
		if extra := len(problems) - len(shown); extra > 0 {
			res.Detail += fmt.Sprintf("; and %d more", extra)
		}
		return res
	}
	res.Passed = true
	res.Detail = fmt.Sprintf("%d model calls, %d tool calls, %d spans", len(t.ModelCalls), len(t.ToolCalls), len(t.Spans))
	return res
}

// evalExpectation checks the trace against the case's expect block: the final status,
// an optional case-insensitive substring of the failure reason, and minimum call counts.
func evalExpectation(c Case, t store.Trace) EvalResult {
	return evalExpectationObserved(c, t, Observed{})
}

// evalExpectationObserved is evalExpectation with what the runner observed outside the
// trace: the extra trigger's answer, the tool list and the schedule.
func evalExpectationObserved(c Case, t store.Trace, o Observed) EvalResult {
	res := EvalResult{Name: EvalExpectation}
	if t.Run == nil {
		res.Detail = "trace has no run"
		return res
	}
	var problems []string
	exp := c.Expect

	if t.Run.Status != exp.Status {
		problems = append(problems, fmt.Sprintf("status %q, want %q", t.Run.Status, exp.Status))
	}
	if exp.FailureReasonContains != "" {
		reason := ""
		if t.Run.FailureReason != nil {
			reason = *t.Run.FailureReason
		}
		if !strings.Contains(strings.ToLower(reason), strings.ToLower(exp.FailureReasonContains)) {
			problems = append(problems, fmt.Sprintf("failure reason %q does not contain %q", reason, exp.FailureReasonContains))
		}
	}
	if exp.MinToolCalls > 0 && len(t.ToolCalls) < exp.MinToolCalls {
		problems = append(problems, fmt.Sprintf("%d tool calls, want at least %d", len(t.ToolCalls), exp.MinToolCalls))
	}
	if exp.MinModelCalls > 0 && len(t.ModelCalls) < exp.MinModelCalls {
		problems = append(problems, fmt.Sprintf("%d model calls, want at least %d", len(t.ModelCalls), exp.MinModelCalls))
	}
	if exp.MinAgentMessages > 0 && len(t.AgentMessages) < exp.MinAgentMessages {
		problems = append(problems, fmt.Sprintf("%d agent messages, want at least %d", len(t.AgentMessages), exp.MinAgentMessages))
	}
	problems = append(problems, checkFinalState(exp.State, t.Run.State)...)
	problems = append(problems, checkToolStatuses(exp.ToolStatuses, t.ToolCalls)...)
	problems = append(problems, checkNeverToolStatuses(exp.NeverToolStatuses, t.ToolCalls)...)
	problems = append(problems, checkApprovalStatuses(exp.ApprovalStatuses, t.Approvals)...)
	problems = append(problems, checkApprovalReasons(exp.ApprovalReasons, t.Approvals)...)
	problems = append(problems, checkToolOutputs(exp.ToolOutputs, t.ToolCalls)...)
	problems = append(problems, checkObserved(exp, t.Run, o)...)

	if len(problems) > 0 {
		res.Detail = strings.Join(problems, "; ")
		return res
	}
	res.Passed = true
	res.Detail = fmt.Sprintf("status %s as expected", t.Run.Status)
	return res
}
