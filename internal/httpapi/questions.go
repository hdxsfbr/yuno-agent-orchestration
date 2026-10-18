package httpapi

import (
	"context"

	"yuno/internal/store"
	"yuno/internal/tools"
)

// asker is the engine behind ask_operator and answer_question (DESIGN 5.24 rules 2, 3
// and 9). Run builds the tool registry before the engine exists and sets s.engine
// afterwards (as delegate_task's delegator relies on), so the tools reach the engine
// through this value at call time, never through the engine the registry saw when it was
// built: the first deploy of wave 10 answered every ask_operator with "not configured"
// for that reason.
type asker struct{ s *Server }

var _ tools.Asker = asker{}
var _ tools.QuestionParker = asker{}

func (a asker) AskOperator(ctx context.Context, token, question string, options []string) (*store.Approval, error) {
	if a.s == nil || a.s.engine == nil {
		return nil, errEngineUnavailable
	}
	return a.s.engine.AskOperator(ctx, token, question, options)
}

func (a asker) WaitQuestion(ctx context.Context, id string) (*store.Approval, error) {
	if a.s == nil || a.s.engine == nil {
		return nil, errEngineUnavailable
	}
	return a.s.engine.WaitQuestion(ctx, id)
}

func (a asker) AnswerQuestion(ctx context.Context, id, answer, by string) (*store.Approval, error) {
	if a.s == nil || a.s.engine == nil {
		return nil, errEngineUnavailable
	}
	return a.s.engine.AnswerQuestion(ctx, id, answer, by)
}

// QuestionPending parks the visit on its question once the wait ended (rule 4), when the
// engine behind the server does that; an engine that does not leaves the tool's result
// at "pending" without a park, as the tool does for any Asker that is no QuestionParker.
func (a asker) QuestionPending(ctx context.Context, token, id string) error {
	if a.s == nil || a.s.engine == nil {
		return errEngineUnavailable
	}
	p, ok := a.s.engine.(tools.QuestionParker)
	if !ok {
		return nil
	}
	return p.QuestionPending(ctx, token, id)
}
