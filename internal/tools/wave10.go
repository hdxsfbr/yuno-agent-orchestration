package tools

import (
	"context"

	"yuno/internal/store"
)

// The names of the wave 10 platform tools (DESIGN 5.24 rules 1 and 9). The model sees
// them as yuno__ask_operator and yuno__answer_question.
const (
	AskOperatorName    = "ask_operator"
	AnswerQuestionName = "answer_question"
)

// Asker is what ask_operator and answer_question call on the engine (DESIGN 5.24
// rules 2, 3 and 9); the engine satisfies it.
type Asker interface {
	// AskOperator records the question of the visit that holds the run token and pauses
	// the run on it (rule 2). The caller then waits for the decision with WaitQuestion.
	AskOperator(ctx context.Context, token, question string, options []string) (*store.Approval, error)
	// WaitQuestion blocks until the question is decided or ctx ends (rules 2 and 4), and
	// returns the decided row or ctx.Err().
	WaitQuestion(ctx context.Context, id string) (*store.Approval, error)
	// AnswerQuestion answers a pending question as the principal `by` (rules 3 and 9).
	AnswerQuestion(ctx context.Context, id, answer, by string) (*store.Approval, error)
}
