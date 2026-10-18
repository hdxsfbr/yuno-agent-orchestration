package engine

import (
	"context"
	"errors"

	"yuno/internal/store"
)

// RetryError is a refusal of Retry (DESIGN 5.21 rules 1 and 2): Code is the API's
// error code (run_not_retryable, retry_limit, limits_exhausted), Message the sentence
// for the operator, Details one "<limit>: <used> of <cap> used" line per exhausted
// limit for limits_exhausted.
type RetryError struct {
	Code    string
	Message string
	Details []string
}

func (e *RetryError) Error() string { return e.Message }

// Retry re-queues a failed, timed-out or cancelled run from the node it stopped at
// (DESIGN 5.21). Wave 8 agent C replaces this stub.
func (e *Engine) Retry(ctx context.Context, runID, by string, limits *store.Limits) (*store.Run, error) {
	return nil, errors.New("retry: not implemented")
}
