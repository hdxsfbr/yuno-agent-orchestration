package tools

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"yuno/internal/store"
)

// ask_operator (DESIGN 5.24 rules 1, 2 and 4): an agent in a run asks the operator one
// question and waits for the answer. The tool records the question through the engine's
// Asker, blocks for the decision up to MCP_TOOL_WAIT, and turns every outcome into a
// result, never an error: the agent goes on either way.

// The limits of rule 1, in bytes after trimming, and the question cap per step the
// engine enforces (rule 2).
const (
	askQuestionMaxBytes = 1024
	askOptionsMin       = 2
	askOptionsMax       = 5
	askOptionMaxBytes   = 100
)

// The model-facing texts of rule 1.
const (
	askQuestionText = "question: 1 to 1024 characters"
	askOptionsText  = "options: 2 to 5 distinct options of at most 100 characters"
)

// askOperatorDefaultWait is how long the tool blocks when PlatformDeps.MCPToolWait is
// unset (MCP_TOOL_WAIT's default, DESIGN 5.3 rule 5), and askOperatorSlack is what the
// tool's timeout adds to the wait (rule 1: the wait plus 30 s).
const (
	askOperatorDefaultWait = 540 * time.Second
	askOperatorSlack       = 30 * time.Second
	// askParkTimeout bounds the call that tells the engine the wait ended.
	askParkTimeout = 10 * time.Second
)

// declinedReason is what the model reads when the operator declined without giving one
// (rule 4).
const declinedReason = "the operator declined to answer"

// invalidRunTokenText is the text of engine.ErrInvalidRunToken, which this package cannot
// import (the engine imports tools).
const invalidRunTokenText = "invalid run token"

// QuestionParker is the optional second interface of the engine behind Asker: when the
// wait ends with no decision, QuestionPending marks the visit parked on the question
// (id) and returns the run and the node to running, so the agent's turn goes on (DESIGN
// 5.24 rule 4). An Asker that does not implement it leaves the question pending only.
type QuestionParker interface {
	QuestionPending(ctx context.Context, token, id string) error
}

var askOperatorSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_token": {"type": "string", "description": "The run token from the task message (run_token: r_...)."},
    "question": {"type": "string", "description": "The question, short and self-contained: the operator reads it without your context."},
    "options": {"type": ["array", "null"], "items": {"type": "string"}, "description": "Optional suggested answers when a few are likely: 2 to 5 distinct options of at most 100 characters. The operator may answer something else."}
  },
  "required": ["run_token", "question"],
  "additionalProperties": false
}`)

type askOperatorInput struct {
	RunToken string    `json:"run_token"`
	Question string    `json:"question"`
	Options  *[]string `json:"options"`
}

// NewAskOperator builds ask_operator (DESIGN 5.24 rule 1): computational, postgres, run
// tokens only, a timeout of the wait plus 30 s. d.Asker is the engine; d.MCPToolWait is
// how long the call blocks for the answer (0: 540 s).
func NewAskOperator(d PlatformDeps) *Tool {
	wait := d.MCPToolWait
	if wait <= 0 {
		wait = askOperatorDefaultWait
	}
	return &Tool{
		Name: AskOperatorName,
		Description: "Ask the operator one short question when a fact you cannot find out yourself decides the work, " +
			"with options when a few answers are likely; at most 3 per step. The answer usually comes back in this " +
			"result; if it says \"pending\", save your progress in files under runs/<run id>/ and end your turn " +
			"without calling yuno__finish: the platform runs this step again with the answer added to your task.",
		InputSchema: askOperatorSchema,
		Risk:        RiskComputational,
		Hits:        HitsPostgres,
		Timeout:     wait + askOperatorSlack,
		// A question is never asked twice by a retry.
		MaxAttempts: 1,
		Exec: func(ctx context.Context, call Call) (Result, error) {
			return execAskOperator(ctx, d.Asker, wait, call)
		},
	}
}

func execAskOperator(ctx context.Context, asker Asker, wait time.Duration, call Call) (Result, error) {
	var in askOperatorInput
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return Result{}, &ModelError{Message: "invalid input: " + err.Error()}
	}
	question := strings.TrimSpace(in.Question)
	if n := len(question); n < 1 || n > askQuestionMaxBytes || !plainText(question) {
		return Result{}, &ModelError{Message: askQuestionText}
	}
	var options []string
	if in.Options != nil {
		var ok bool
		if options, ok = cleanOptions(*in.Options); !ok {
			return Result{}, &ModelError{Message: askOptionsText}
		}
	}
	if asker == nil {
		return Result{}, errors.New("ask_operator: not configured")
	}

	row, err := asker.AskOperator(ctx, in.RunToken, question, options)
	if err != nil {
		// A *ModelError from the engine (one question at a time, ask fewer questions)
		// reaches the model as it is; a token that ended since the gate resolved it is
		// the model's error too. Anything else is an internal failure.
		if errorHasText(err, invalidRunTokenText) {
			return Result{}, &ModelError{Message: invalidRunTokenText}
		}
		return Result{}, err
	}
	if row == nil {
		return Result{}, errors.New("ask_operator: the engine recorded no question")
	}

	// From here on the question exists: whatever happens, the model gets a result.
	wctx, cancel := context.WithTimeout(ctx, wait)
	decided, werr := asker.WaitQuestion(wctx, row.ID)
	cancel()
	if werr == nil && decided != nil {
		if res, ok := decisionResult(row.ID, decided); ok {
			return res, nil
		}
	}
	// The wait ended first. The engine marks the visit parked on the question and lets
	// its turn go on; the row stays pending. The call is made even when the request's
	// own context is gone, or the run would stay paused on a call nobody awaits.
	if p, ok := asker.(QuestionParker); ok {
		pctx, stop := context.WithTimeout(context.WithoutCancel(ctx), askParkTimeout)
		if err := p.QuestionPending(pctx, in.RunToken, row.ID); err != nil {
			slog.Warn("ask_operator: mark the question pending failed", "run_id", call.RunID, "question_id", row.ID, "error", err)
		}
		stop()
	}
	return JSONResult(map[string]any{"status": "pending", "question_id": row.ID}), nil
}

// decisionResult is rule 4's result for a decided row; ok is false for a row that is
// still pending.
func decisionResult(id string, a *store.Approval) (Result, bool) {
	switch a.Status {
	case store.ApprovalApproved:
		answer := ""
		if a.Answer != nil {
			answer = *a.Answer
		}
		return JSONResult(map[string]any{"status": "answered", "question_id": id, "answer": answer}), true
	case store.ApprovalRejected:
		reason := declinedReason
		if a.Reason != nil && strings.TrimSpace(*a.Reason) != "" {
			reason = *a.Reason
		}
		return JSONResult(map[string]any{"status": "declined", "question_id": id, "reason": reason}), true
	case store.ApprovalExpired:
		return JSONResult(map[string]any{"status": "unanswered", "question_id": id}), true
	}
	return Result{}, false
}

// cleanOptions checks rule 1's option list and returns it trimmed: 2 to 5 distinct
// strings of 1 to 100 bytes without control characters.
func cleanOptions(in []string) ([]string, bool) {
	if len(in) < askOptionsMin || len(in) > askOptionsMax {
		return nil, false
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, o := range in {
		o = strings.TrimSpace(o)
		if n := len(o); n < 1 || n > askOptionMaxBytes || !plainText(o) || seen[o] {
			return nil, false
		}
		if strings.IndexFunc(o, unicode.IsControl) >= 0 {
			return nil, false
		}
		seen[o] = true
		out = append(out, o)
	}
	return out, true
}

// plainText reports whether s can be stored as a question, an option or an answer: valid
// UTF-8 without a NUL, which the database refuses with an error the model and the operator
// cannot act on (review L4).
func plainText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// errorHasText reports whether err, or any error it wraps or joins, says text exactly.
func errorHasText(err error, text string) bool {
	if err == nil {
		return false
	}
	if err.Error() == text {
		return true
	}
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		return errorHasText(u.Unwrap(), text)
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if errorHasText(e, text) {
				return true
			}
		}
	}
	return false
}
