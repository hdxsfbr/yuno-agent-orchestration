package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"yuno/internal/store"
)

// answer_question (DESIGN 5.24 rule 9): the Chief of Staff relays the person's answer to
// a question a run asked. It is chat-only: a run token reaches the executor too, because
// the MCP gate resolves it, and is refused there.

const (
	answerMaxBytes = 4096
	answerText     = "answer: 1 to 4096 characters"
	// answerNotInARun is what a call made with a run token gets (rule 9).
	answerNotInARun = AnswerQuestionName + " is not available in a run"
	// answerNotSaid refuses an answer the person's newest message does not carry
	// (platform control A7).
	answerNotSaid = "relay only what the person said"
)

// saidIn reports whether answer appears in the person's message, ignoring case and how
// the whitespace runs.
func saidIn(message, answer string) bool {
	norm := func(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
	a := norm(answer)
	return a != "" && strings.Contains(norm(message), a)
}

var answerQuestionSchema = json.RawMessage(`{
  "type": "object",
  "properties": {` + tokenProperties + `
    "question_id": {"type": "string", "minLength": 1, "description": "The question id from the [platform] message that said the run asks a question (question <id>)."},
    "answer": {"type": "string", "description": "The person's answer, as they wrote it."}
  },
  "required": ["question_id", "answer"],
  "additionalProperties": false
}`)

type answerQuestionInput struct {
	QuestionID string `json:"question_id"`
	Answer     string `json:"answer"`
}

// NewAnswerQuestion builds answer_question (DESIGN 5.24 rule 9): computational, postgres,
// chat-capable and nothing else. The answer is recorded as decided by "chat:<agent name>".
func NewAnswerQuestion(d PlatformDeps) *Tool {
	return &Tool{
		Name: AnswerQuestionName,
		Description: "Pass the person's answer to a question a run asked. Use it when a [platform] message said a run " +
			"asks a question and the person has answered: give the question id from that message and their answer " +
			"as they wrote it. The platform accepts it only when the person's latest message contains that answer, " +
			"or the question's code (run_status shows it). Returns the run id and its link.",
		InputSchema: answerQuestionSchema,
		Risk:        RiskComputational,
		Hits:        HitsPostgres,
		Timeout:     10 * time.Second,
		Chat:        true,
		Exec: func(ctx context.Context, call Call) (Result, error) {
			return execAnswerQuestion(ctx, d, call)
		},
	}
}

func execAnswerQuestion(ctx context.Context, d PlatformDeps, call Call) (Result, error) {
	// A chat call has no run or node visit; a call that carries them came with a run
	// token (the registry has no chat-only flag, so the tool refuses it itself).
	if !call.Chat || call.RunID != "" {
		return Result{}, &ModelError{Message: answerNotInARun}
	}
	var in answerQuestionInput
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return Result{}, &ModelError{Message: "invalid input: " + err.Error()}
	}
	id := strings.TrimSpace(in.QuestionID)
	if id == "" {
		return Result{}, &ModelError{Message: "question_id: must not be empty"}
	}
	if !plainText(id) {
		// No row has such an id, and the database would refuse the lookup itself.
		return Result{}, &ModelError{Message: "no question " + strings.Map(func(r rune) rune {
			if r == 0 || r == utf8.RuneError {
				return -1
			}
			return r
		}, id)}
	}
	answer := strings.TrimSpace(in.Answer)
	if n := len(answer); n < 1 || n > answerMaxBytes || !plainText(answer) {
		return Result{}, &ModelError{Message: answerText}
	}
	if d.Pool == nil || d.Asker == nil {
		return Result{}, errors.New("answer_question: not configured")
	}

	// The row decides which refusal the model reads. The engine's own sentinels cannot be
	// matched here (it imports this package), so the same check runs on the row first
	// and again if the engine refuses: a question decided between the two is "already".
	asked, err := pendingQuestion(ctx, d, id)
	if err != nil {
		return Result{}, err
	}
	// Platform control A7: the evidence decide_approval has. The answer is the person's own
	// words in their newest message, or that message carries the question's code.
	latest := ""
	if call.ConversationID != "" {
		if latest, err = newestPersonMessage(ctx, d.Pool, call.ConversationID); err != nil {
			return Result{}, err
		}
	}
	if !saidIn(latest, answer) && !containsCode(latest, ApprovalCode(d.ApprovalCodeKey, id)) {
		return Result{}, &ModelError{Message: answerNotSaid}
	}
	agent, err := store.GetAgentForRun(ctx, d.Pool, call.AgentID)
	if err != nil {
		return Result{}, err
	}
	row, err := d.Asker.AnswerQuestion(ctx, id, answer, "chat:"+agent.Name)
	if err != nil {
		var me *ModelError
		if errors.As(err, &me) {
			return Result{}, err
		}
		if _, rerr := pendingQuestion(ctx, d, id); rerr != nil {
			return Result{}, rerr
		}
		return Result{}, err
	}
	runID := asked.RunID
	if runID == "" && row != nil {
		runID = row.RunID
	}
	return JSONResult(map[string]any{"status": "answered", "run_id": runID, "url": RunURL(d.PublicBaseURL, runID)}), nil
}

// pendingQuestion returns the row when id is a pending question; otherwise the
// *ModelError the model reads: no question <id>, <id> is not a question, question <id> is
// already <status> (rule 9).
func pendingQuestion(ctx context.Context, d PlatformDeps, id string) (*store.Approval, error) {
	a, err := store.GetApproval(ctx, d.Pool, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, &ModelError{Message: "no question " + id}
	case err != nil:
		return nil, err
	case a.Kind != store.ApprovalKindQuestion:
		return nil, &ModelError{Message: id + " is not a question"}
	case a.Status != store.ApprovalPending:
		return nil, &ModelError{Message: "question " + id + " is already " + a.Status}
	}
	return a, nil
}
