package engine

import (
	"context"
	"encoding/json"
	"errors"

	"yuno/internal/store"
)

// Engine runs workflows. This file is a compile-time stub; wave 1 replaces it.
type Engine struct {
	deps Deps
}

var errNotImplemented = errors.New("engine: not implemented yet")

// New builds an engine from its dependencies. It does not start workers.
func New(d Deps) *Engine {
	if d.Clock == nil {
		d.Clock = RealClock{}
	}
	return &Engine{deps: d}
}

// Start runs the worker pool and the stale-run sweeper until ctx is done.
func (e *Engine) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (e *Engine) Trigger(ctx context.Context, req TriggerRequest) (*store.Run, error) {
	return nil, errNotImplemented
}

func (e *Engine) Cancel(ctx context.Context, runID, by string) (*store.Run, error) {
	return nil, errNotImplemented
}

func (e *Engine) ResolveRunToken(token string) (RunContext, bool) {
	return RunContext{}, false
}

func (e *Engine) AcceptFinish(ctx context.Context, token string, result json.RawMessage) error {
	return ErrInvalidRunToken
}

var _ Service = (*Engine)(nil)
