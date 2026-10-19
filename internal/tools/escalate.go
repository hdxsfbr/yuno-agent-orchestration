package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// escalate (DESIGN 5.29 rule 6): an agent in a project run stops the run because the plan
// cannot be built as written. The engine ends the run failed with
// "escalated (<kind>): <reason>" at once, and a plan ticket's run also fails its ticket,
// pauses the waves and tells the operator.

// EscalateName is the tool's name; the model sees it as yuno__escalate.
const EscalateName = "escalate"

const (
	escalateReasonMax = 2000
	escalateReasonMsg = "reason: 1 to 2000 characters"
	escalateKindMsg   = "kind: one of prd_gap, design_wrong, loop_limit, other"
	// runEndedText is the text of engine.ErrRunEnded, which this package cannot import.
	runEndedText = "the run has ended already"
)

var escalateKinds = []string{"prd_gap", "design_wrong", "loop_limit", "other"}

// EscalateFunc ends the run failed as escalated; the engine's EscalateRun.
type EscalateFunc func(ctx context.Context, runID, kind, reason string) error

// EscalateDeps are what the tool calls.
type EscalateDeps struct {
	Escalate EscalateFunc
}

var escalateSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_token": {"type": "string", "description": "The run token from the task message (run_token: r_...)."},
    "reason": {"type": "string", "description": "What is wrong and what the operator must decide, in one to a few sentences."},
    "kind": {"type": "string", "enum": ["prd_gap", "design_wrong", "loop_limit", "other"], "description": "prd_gap: the PRD does not say what to build. design_wrong: the design cannot work. loop_limit: the work keeps failing the same review. other: anything else."}
  },
  "required": ["run_token", "reason", "kind"],
  "additionalProperties": false
}`)

type escalateInput struct {
	RunToken string `json:"run_token"`
	Reason   string `json:"reason"`
	Kind     string `json:"kind"`
}

// NewEscalate builds escalate (DESIGN 5.29 rule 6): computational, postgres, run tokens
// only. It does not wait for the run to end: the engine aborts the calling visit.
func NewEscalate(d EscalateDeps) *Tool {
	return &Tool{
		Name: EscalateName,
		Description: "Stop this run and hand the problem to the operator, when the work cannot go on as planned: the " +
			"PRD leaves out something you need (prd_gap), the design cannot work (design_wrong), or the same " +
			"review failure keeps coming back (loop_limit). Give the reason plainly. The run ends as failed at once " +
			"and the project's waves pause; do not call finish afterwards.",
		InputSchema: escalateSchema,
		Risk:        RiskComputational,
		Hits:        HitsPostgres,
		Timeout:     10 * time.Second,
		// Ending the run twice is harmless but pointless.
		MaxAttempts: 1,
		Exec: func(ctx context.Context, call Call) (Result, error) {
			return execEscalate(ctx, d, call)
		},
	}
}

func execEscalate(ctx context.Context, d EscalateDeps, call Call) (Result, error) {
	var in escalateInput
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return Result{}, &ModelError{Message: "invalid input: " + err.Error()}
	}
	reason := strings.TrimSpace(in.Reason)
	if n := utf8.RuneCountInString(reason); n < 1 || n > escalateReasonMax {
		return Result{}, &ModelError{Message: escalateReasonMsg}
	}
	known := false
	for _, k := range escalateKinds {
		known = known || k == in.Kind
	}
	if !known {
		return Result{}, &ModelError{Message: escalateKindMsg}
	}
	if call.Chat || call.RunID == "" {
		return Result{}, &ModelError{Message: EscalateName + " is only available in a run"}
	}
	if d.Escalate == nil {
		return Result{}, errors.New("escalate: not configured")
	}
	if err := d.Escalate(ctx, call.RunID, in.Kind, reason); err != nil {
		if errorHasText(err, runEndedText) {
			return Result{}, &ModelError{Message: "the run has ended already"}
		}
		return Result{}, err
	}
	return JSONResult(map[string]any{"status": "escalated", "kind": in.Kind}), nil
}
