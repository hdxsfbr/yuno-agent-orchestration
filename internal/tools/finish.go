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

// finishSchema takes the result as JSON text, or as an object. The text form is the one the
// task message asks for (D-104): on OpenAI's native route the gateway makes every tool
// schema strict, closing the open result object so that any field in it is refused
// ("schema is false"), and only `{}` gets through; a string passes. Claude agents and
// scripted runs may still send the object.
var finishSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_token": {"type": "string", "description": "The run token from the task message (run_token: r_...)."},
    "result": {
      "description": "The node's result as JSON text: an object matching the output schema given in the task message, encoded as a string.",
      "anyOf": [{"type": "string"}, {"type": "object"}]
    }
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
			result, err := resultObject(in.Result)
			if err != nil {
				if reject != nil && in.RunToken != "" {
					reject(ctx, in.RunToken, err.Error()) // a refused result counts as an attempt
				}
				return Result{}, err
			}
			if err := accept(ctx, in.RunToken, result); err != nil {
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

// resultObject is the finish result as an object: the object itself, or the object a JSON
// string holds (D-104). Anything else is the model's to correct.
func resultObject(raw json.RawMessage) (json.RawMessage, error) {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return raw, nil // an object: the input schema allows no other kind
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &obj); err != nil || obj == nil {
		return nil, &ModelError{Message: "result: a string result must be the JSON text of an object matching the output schema"}
	}
	return json.RawMessage(text), nil
}
