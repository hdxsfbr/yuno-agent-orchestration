package tools

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// FinishName is the tool a node calls with its typed result (DESIGN 5.6). The model
// sees it as yuno__finish.
const FinishName = "finish"

// AcceptFinishFunc stores a node's result for the run token. Failures the model should
// correct (a result that does not match the output schema, an invalid run token) are
// returned as *ModelError; any other error is an internal failure.
type AcceptFinishFunc func(ctx context.Context, runToken string, result json.RawMessage) error

// RejectFinishFunc records a finish call the input schema refused, so it counts toward
// the node's finish attempts (DESIGN 5.6). The model still sees the schema message.
type RejectFinishFunc func(ctx context.Context, runToken, message string)

var finishSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_token": {"type": "string", "description": "The run token from the task message (run_token: r_...)."},
    "result": {"type": "object", "description": "The node's result. It must match the output schema given in the task message."}
  },
  "required": ["run_token", "result"],
  "additionalProperties": false
}`)

// acceptedResult is the tool result for a stored finish (DESIGN 5.6).
var acceptedResult = json.RawMessage(`{"status":"accepted"}`)

type finishInput struct {
	RunToken string          `json:"run_token"`
	Result   json.RawMessage `json:"result"`
}

// NewFinish builds the finish tool (DESIGN 5.7 table: computational, postgres, 5 s).
// reject may be nil.
func NewFinish(accept AcceptFinishFunc, reject RejectFinishFunc) *Tool {
	t := &Tool{
		Name: FinishName,
		Description: "Submit this node's final result. Call it once when the task is done, with the run_token " +
			"from the task message and a result object that matches the requested output schema. If the " +
			"result is rejected, fix it and call finish again.",
		InputSchema: finishSchema,
		Risk:        RiskComputational,
		Hits:        HitsPostgres,
		Timeout:     5 * time.Second,
		Exec: func(ctx context.Context, call Call) (Result, error) {
			if accept == nil {
				return Result{}, errors.New("finish: no engine")
			}
			var in finishInput
			if err := json.Unmarshal(call.Input, &in); err != nil {
				return Result{}, &ModelError{Message: "invalid input: " + err.Error()}
			}
			if err := accept(ctx, in.RunToken, in.Result); err != nil {
				return Result{}, err
			}
			return Result{Output: acceptedResult}, nil
		},
	}
	if reject != nil {
		t.OnInvalidInput = func(ctx context.Context, call Call, message string) {
			var in struct {
				RunToken any `json:"run_token"`
			}
			_ = json.Unmarshal(call.Input, &in)
			if token, ok := in.RunToken.(string); ok && token != "" {
				reject(ctx, token, message)
			}
		}
	}
	return t
}
