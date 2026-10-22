package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// AdvanceWaveName is the Integrator's tool for a ship run (DESIGN 5.29 rule 14): once the
// wave's smoke test passed, it closes the wave and releases the next one.
const AdvanceWaveName = "advance_wave"

// AdvanceResult is what advancing a wave did: the plan, the wave that was closed, how many
// of its tickets were closed, how many tickets of the next wave were released, and whether
// this was the last wave. HeldWave is the next wave when a revision of the plan was running:
// it is released once the revision ends (DESIGN 5.29 rule 14, D-089), and Released is 0.
type AdvanceResult struct {
	PlanID   string `json:"plan_id"`
	Wave     int    `json:"wave"`
	Closed   int    `json:"closed"`
	Released int    `json:"released"`
	Last     bool   `json:"last"`
	HeldWave int    `json:"held_wave,omitempty"`
}

// AdvanceRefused is a refusal to advance (the project's waves are paused, the run is not a
// ship run, the wave is not the plan's current one). The tool returns it to the model as
// an error.
type AdvanceRefused struct{ Reason string }

func (e *AdvanceRefused) Error() string { return e.Reason }

// AdvanceWaveFunc closes the wave of the ship run and releases the next one (DESIGN 5.29
// rule 14); *AdvanceRefused when it may not.
type AdvanceWaveFunc func(ctx context.Context, runID string) (AdvanceResult, error)

// AdvanceWaveDeps are what the tool calls.
type AdvanceWaveDeps struct {
	AdvanceWave AdvanceWaveFunc
	// RunIDForToken resolves the run_token of the input to its run.
	RunIDForToken func(token string) (runID string, ok bool)
}

var advanceWaveSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_token": {"type": "string", "description": "The run token from the task message (run_token: r_...)."}
  },
  "required": ["run_token"],
  "additionalProperties": false
}`)

type advanceWaveInput struct {
	RunToken string `json:"run_token"`
}

// NewAdvanceWave builds advance_wave (DESIGN 5.29 rule 14): side-effecting, so it needs a
// human approval by default and runs once per call key; the key hashes the run, the visit
// and the input, and a ship run belongs to one plan and wave.
func NewAdvanceWave(d AdvanceWaveDeps) *Tool {
	return &Tool{
		Name: AdvanceWaveName,
		Description: "Close the current wave and release the next one. Call it exactly once in a ship run, after " +
			"the smoke test passed: it closes the wave's tickets on the tracker and releases the next wave's " +
			"tickets for building, or tells you this was the last wave. Takes only the run_token. Refuses when " +
			"the project's waves are paused or the wave is not the plan's current one. Returns plan_id, wave, " +
			"closed, released and last; held_wave names the next wave when a revision of the plan is running, " +
			"and the platform releases it once the revision ends, so there is nothing for you to do about it.",
		InputSchema: advanceWaveSchema,
		Risk:        RiskSideEffecting,
		Hits:        HitsExternal,
		Timeout:     2 * time.Minute,
		Exec: func(ctx context.Context, call Call) (Result, error) {
			return execAdvanceWave(ctx, d, call)
		},
	}
}

func execAdvanceWave(ctx context.Context, d AdvanceWaveDeps, call Call) (Result, error) {
	var in advanceWaveInput
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return Result{}, &ModelError{Message: "invalid input: " + err.Error()}
	}
	if strings.TrimSpace(in.RunToken) == "" {
		return Result{}, &ModelError{Message: "run_token: the run token from the task message"}
	}
	if call.Chat {
		return Result{}, &ModelError{Message: AdvanceWaveName + " is only available in a run"}
	}
	if d.AdvanceWave == nil {
		return Result{}, &ModelError{Message: AdvanceWaveName + " is not available"}
	}
	var runID string
	if d.RunIDForToken != nil {
		runID, _ = d.RunIDForToken(in.RunToken)
	}
	if runID == "" {
		return Result{}, &ModelError{Message: "run_token: not the token of a run"}
	}
	res, err := d.AdvanceWave(ctx, runID)
	if err != nil {
		var refused *AdvanceRefused
		if errors.As(err, &refused) {
			return Result{}, &ModelError{Message: refused.Reason}
		}
		return Result{}, err
	}
	return JSONResult(res), nil
}
