package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/obs"
	"yuno/internal/store"
)

// Service executes platform tool calls the way DESIGN 5.4 and 5.5 rule 4 require:
// side-effecting tools at most once per idempotency key, every tool retried on the
// errors it classifies as transient. It is also the engine's SideEffectExecutor
// (ExecuteApproved, DESIGN 5.3 rule 5).
type Service struct {
	Pool     *pgxpool.Pool
	Registry *Registry
	Log      *slog.Logger
	// Workspace maps an OpenClaw agent id to its workspace directory.
	Workspace func(openclawAgentID string) string
	// MaxOutputBytes caps the text the model sees and the stored output.
	MaxOutputBytes int
	// Emit publishes an SSE event for a run; nil skips it.
	Emit func(ctx context.Context, runID, typ string, v any)
	// Sleep waits between attempts; nil means a real, cancellable sleep.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Outcome is an executed call: the result, whether it replayed a stored effect, and how
// many attempts it took.
type Outcome struct {
	Result   Result
	Replayed bool
	Attempts int
}

// failedEffect carries an error result out of RunSideEffect so the key is released and
// the model still sees what went wrong.
type failedEffect struct {
	res      Result
	attempts int
}

func (f *failedEffect) Error() string { return f.res.Text }

// Execute runs a validated call. Side-effecting tools need call.IdempotencyKey and run
// inside store.RunSideEffect: a key with a stored result replays it with
// output.replayed = true and no external call. err is an internal failure.
func (s *Service) Execute(ctx context.Context, t *Tool, call Call) (Outcome, error) {
	if t.Risk != RiskSideEffecting {
		res, attempts, err := s.attempt(ctx, t, call)
		return Outcome{Result: res, Attempts: attempts}, err
	}
	if s.Pool == nil {
		return Outcome{}, errors.New("side effects need a database")
	}
	if call.IdempotencyKey == "" {
		return Outcome{}, fmt.Errorf("tool %s: side effect without an idempotency key", t.Name)
	}
	var attempts int
	stored, replayed, err := store.RunSideEffect(ctx, s.Pool, call.IdempotencyKey, t.Name, call.RunID, call.ToolCallID,
		func(ctx context.Context) (json.RawMessage, error) {
			res, n, err := s.attempt(ctx, t, call)
			attempts = n
			if err != nil {
				return nil, err
			}
			if res.IsError {
				return nil, &failedEffect{res: res, attempts: n}
			}
			if len(res.Output) > 0 {
				return res.Output, nil
			}
			return json.Marshal(map[string]string{"text": res.Text})
		})
	var fe *failedEffect
	switch {
	case errors.As(err, &fe):
		return Outcome{Result: fe.res, Attempts: fe.attempts}, nil
	case errors.Is(err, store.ErrEffectNotRecorded):
		s.log().Warn("side effect result not recorded", "run_id", call.RunID, "tool", t.Name, "error", err)
	case err != nil:
		return Outcome{Attempts: attempts}, err
	}
	out := stored
	if replayed {
		out = WithFields(stored, map[string]any{"replayed": true})
		s.log().Info("side effect replayed", "run_id", call.RunID, "tool", t.Name, "idempotency_key", call.IdempotencyKey)
	}
	return Outcome{Result: Result{Output: out}, Replayed: replayed, Attempts: attempts}, nil
}

// attempt invokes the tool up to MaxAttempts times while it fails transiently, with
// the backoff of DESIGN 5.2 (1, 2, 4 … s, capped at 30 s). Every attempt of a call that
// needed more than one is a span and every retry a warn line (DESIGN 5.5 rule 5).
func (s *Service) attempt(ctx context.Context, t *Tool, call Call) (Result, int, error) {
	maxAttempts := max(t.MaxAttempts, 1)
	type try struct {
		start, end time.Time
		errText    string
	}
	var tries []try
	defer func() {
		if len(tries) > 1 {
			for i, tr := range tries {
				s.span(ctx, call, t.Name, i+1, tr.start, tr.end, tr.errText)
			}
		}
	}()
	for n := 1; ; n++ {
		start := time.Now()
		res, err := t.Invoke(ctx, call)
		tr := try{start: start, end: time.Now()}
		if err != nil {
			tr.errText = err.Error()
		} else if res.IsError {
			tr.errText = res.Text
		}
		tries = append(tries, tr)
		if err != nil || !res.IsError || !res.Transient || n >= maxAttempts || ctx.Err() != nil {
			return res, n, err
		}
		wait := Backoff(n)
		s.log().Warn("platform tool attempt failed, retrying", "run_id", call.RunID, "node_id", call.NodeID,
			"tool", t.Name, "attempt", n, "retry_in", wait.String(), "error", obs.Redact(res.Text))
		if err := s.sleep(ctx, wait); err != nil {
			return res, n, nil
		}
	}
}

// Backoff is the wait after attempt n: 1, 2, 4, 8 … s capped at 30 s, no jitter
// (DESIGN 5.2).
func Backoff(n int) time.Duration {
	d := time.Second
	for i := 1; i < n && d < 30*time.Second; i++ {
		d *= 2
	}
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

func (s *Service) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Service) span(ctx context.Context, call Call, tool string, attempt int, start, end time.Time, errText string) {
	if s.Pool == nil || call.RunID == "" {
		return
	}
	attrs, _ := json.Marshal(map[string]any{"tool": tool, "attempt": attempt, "tool_call_id": call.ToolCallID, "error": errText})
	sp := &store.Span{RunID: call.RunID, Kind: "tool_call", Name: fmt.Sprintf("%s attempt %d", tool, attempt),
		StartedAt: start.UTC(), EndedAt: ptr(end.UTC()), Attrs: obs.RedactJSON(attrs)}
	if err := store.InsertSpan(context.WithoutCancel(ctx), s.Pool, sp); err != nil {
		s.log().Warn("insert attempt span failed", "run_id", call.RunID, "error", err)
	}
}

// ExecuteApproved runs a gated call the engine approved after the MCP handler stopped
// waiting (DESIGN 5.3 rule 5): it loads the tool_calls row, rebuilds the call context
// from the node visit and its agent, executes under the row's idempotency key and
// finishes the row. A row that is no longer awaiting approval is returned as it is, so
// a repeated call executes nothing.
func (s *Service) ExecuteApproved(ctx context.Context, toolCallID string) (json.RawMessage, bool, error) {
	if s.Pool == nil {
		return nil, false, errors.New("execute approved: no database")
	}
	row, err := store.GetToolCall(ctx, s.Pool, toolCallID)
	if err != nil {
		return nil, false, fmt.Errorf("tool call %s: %w", toolCallID, err)
	}
	if row.Status != store.ToolAwaitingApproval {
		return row.Output, row.IsError, nil
	}
	start := time.Now().UTC()
	t, ok := s.Registry.Get(row.ToolName)
	if !ok {
		return s.finishRow(ctx, row, Outcome{Result: Result{IsError: true, Text: "tool " + row.ToolName + " is not available"}}, start)
	}
	va, err := store.GetVisitAgent(ctx, s.Pool, row.RunNodeID)
	if err != nil {
		return nil, false, fmt.Errorf("tool call %s: node visit: %w", toolCallID, err)
	}
	call := Call{
		RunID: row.RunID, NodeVisitID: row.RunNodeID, NodeID: va.NodeID, AgentID: va.AgentID,
		ToolUseID: row.ToolUseID, ToolCallID: row.ID, Input: row.Input, OpenClawAgentID: va.OpenClawAgentID,
	}
	if row.IdempotencyKey != nil {
		call.IdempotencyKey = *row.IdempotencyKey
	}
	if s.Workspace != nil && va.OpenClawAgentID != "" {
		call.Workspace = s.Workspace(va.OpenClawAgentID)
	}
	out, err := s.Execute(ctx, t, call)
	if err != nil {
		s.log().Error("approved tool call failed", "run_id", row.RunID, "tool", row.ToolName, "error", err)
		out = Outcome{Result: Result{IsError: true, Text: row.ToolName + " failed with an internal error"}}
	}
	return s.finishRow(ctx, row, out, start)
}

// finishRow writes an executed outcome onto a gated row (first writer wins) and
// returns what the model would have seen.
func (s *Service) finishRow(ctx context.Context, row *store.ToolCall, out Outcome, start time.Time) (json.RawMessage, bool, error) {
	_, stored := Render(out.Result, s.MaxOutputBytes)
	now := time.Now().UTC()
	row.Output, row.IsError = stored, out.Result.IsError
	row.Status = store.ToolExecuted
	if out.Result.IsError {
		row.Status = store.ToolFailed
	}
	row.Attempt = max(out.Attempts, 1)
	row.DurationMs = now.Sub(start).Milliseconds()
	row.FinishedAt = &now
	wctx := context.WithoutCancel(ctx)
	wrote, err := store.CompleteGatedToolCall(wctx, s.Pool, row)
	if err != nil {
		return nil, false, fmt.Errorf("tool call %s: record outcome: %w", row.ID, err)
	}
	if !wrote {
		cur, err := store.GetToolCall(wctx, s.Pool, row.ID)
		if err != nil {
			return nil, false, err
		}
		return cur.Output, cur.IsError, nil
	}
	if s.Emit != nil {
		s.Emit(wctx, row.RunID, "tool_call", map[string]any{"tool_call": row})
	}
	return row.Output, row.IsError, nil
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

func ptr[T any](v T) *T { return &v }
