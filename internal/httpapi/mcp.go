package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"yuno/internal/config"
	"yuno/internal/engine"
	"yuno/internal/obs"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// mcpHandler serves the platform tool registry over MCP streamable HTTP behind the
// YUNO_MCP_TOKEN bearer (DESIGN 5.7 rule 1). The server is stateless: every POST
// carries its own request, so a restarted api needs no session recovery from the
// gateway's MCP client.
func (s *Server) mcpHandler() http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "yuno", Title: "Yuno platform tools", Version: config.Version}, nil)
	for _, t := range s.tools.List() {
		srv.AddTool(&mcp.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}, s.mcpTool(t))
	}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Stateless: true,
		Logger:    s.log,
	})
	return s.mcpAuth(h)
}

// mcpAuth requires Authorization: Bearer <YUNO_MCP_TOKEN>, compared in constant time.
// An unset token refuses every request.
func (s *Server) mcpAuth(next http.Handler) http.Handler {
	want := []byte(s.cfg.MCPToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if len(want) == 0 || !strings.EqualFold(scheme, "Bearer") ||
			subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="yuno-mcp"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// mcpTool adapts a registry tool to an MCP handler. Every outcome is a tool result, so
// the model sees refusals and failures and can correct itself; internal failures are
// logged and reported to the model without detail.
func (s *Server) mcpTool(t *tools.Tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		start := time.Now()
		text, isErr := s.callPlatformTool(ctx, t, req.Params.Arguments)
		s.log.Info("mcp tool call", "tool", t.Name, "is_error", isErr, "duration_ms", time.Since(start).Milliseconds())
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: isErr}, nil
	}
}

// Results the gate gives the model for a gated call (DESIGN 5.3 rule 5).
type gateRejected struct {
	Status string `json:"status"` // "rejected"
	Reason string `json:"reason"`
}

type gatePending struct {
	Status     string `json:"status"` // "pending_approval"
	ApprovalID string `json:"approval_id"`
}

// callPlatformTool is the gate every platform tool call goes through (DESIGN 5.3 rules
// 5–8, 5.4 rule 1, 5.7 rules 3–4): resolve the run token, refuse tools the agent may
// not use, validate the input, compute the idempotency key for side effects, record
// the authoritative tool_calls row, then execute directly (auto) or behind a human
// approval (require_approval). It returns the model's text and whether it is an error.
func (s *Server) callPlatformTool(ctx context.Context, t *tools.Tool, args json.RawMessage) (string, bool) {
	if len(strings.TrimSpace(string(args))) == 0 {
		args = json.RawMessage(`{}`)
	}
	token := runTokenOf(args)
	if token == "" {
		// Without a token there is no run to record against; the schema message names
		// the missing field.
		if err := t.ValidateInput(args); err != nil {
			return s.modelText(tools.Result{IsError: true, Text: err.Error()}), true
		}
		return "invalid run token", true
	}
	if s.engine == nil {
		return t.Name + " failed with an internal error; try the call again", true
	}
	rc, ok := s.engine.ResolveRunToken(token)
	if !ok {
		return "invalid run token", true
	}
	g := &gateCall{s: s, t: t, rc: rc, token: token, args: args, start: time.Now().UTC(),
		log: s.log.With("run_id", rc.RunID, "node_id", rc.NodeID, "tool", t.Name)}
	return g.run(ctx)
}

// gateCall is one platform tool call on its way through the gate.
type gateCall struct {
	s     *Server
	t     *tools.Tool
	rc    engine.RunContext
	token string
	args  json.RawMessage
	start time.Time
	log   *slog.Logger
	row   *store.ToolCall
	call  tools.Call
}

func (g *gateCall) run(ctx context.Context) (string, bool) {
	s, t, rc := g.s, g.t, g.rc
	g.call = tools.Call{
		RunID: rc.RunID, NodeVisitID: rc.RunNodeID, NodeID: rc.NodeID, AgentID: rc.AgentID,
		OpenClawAgentID: rc.OpenClawAgentID, Workspace: s.workspace(rc.OpenClawAgentID),
		Input: g.args, ToolCallID: store.NewID(),
	}
	g.call.ToolUseID = "mcp-" + store.NewID()
	g.row = &store.ToolCall{
		ID: g.call.ToolCallID, RunID: rc.RunID, RunNodeID: rc.RunNodeID, ToolUseID: g.call.ToolUseID,
		ToolName: t.Name, Input: obs.RedactJSON(g.args), StartedAt: &g.start,
	}

	policy := tools.EffectivePolicy(t, rc.InteractionRules)
	if !tools.Permitted(t.Name, rc.ToolAllowlist) || policy == tools.PolicyBlocked {
		g.log.Warn("platform tool refused: blocked for this agent", "agent_id", rc.AgentID)
		return g.finish(ctx, store.ToolBlocked, tools.Result{IsError: true, Text: fmt.Sprintf("tool %s is blocked for this agent", t.Name)}, 1)
	}
	if err := t.ValidateInput(g.args); err != nil {
		if t.OnInvalidInput != nil {
			t.OnInvalidInput(ctx, g.call, err.Error())
		}
		return g.finish(ctx, store.ToolFailed, tools.Result{IsError: true, Text: err.Error()}, 1)
	}
	if t.Risk == tools.RiskSideEffecting {
		key, err := tools.IdempotencyKey(rc.RunID, rc.RunNodeID, t.Name, g.args)
		if err != nil {
			return g.finish(ctx, store.ToolFailed, tools.Result{IsError: true, Text: "invalid input: " + err.Error()}, 1)
		}
		g.call.IdempotencyKey = key
		g.row.IdempotencyKey = &key
	}
	if policy == tools.PolicyRequireApproval {
		return g.gated(ctx)
	}
	out, err := s.effects.Execute(ctx, t, g.call)
	if err != nil {
		g.log.Error("platform tool failed", "error", err)
		return g.finish(ctx, store.ToolFailed, tools.Result{IsError: true, Text: t.Name + " failed with an internal error; try the call again"}, out.Attempts)
	}
	res := out.Result
	if t.Risk == tools.RiskSideEffecting && !res.IsError {
		// The audit row says the effect ran without a human decision (T5.3.6).
		res = tools.Result{Output: tools.WithFields(res.Output, map[string]any{"policy": tools.PolicyAuto})}
	}
	status := store.ToolExecuted
	if res.IsError {
		status = store.ToolFailed
	}
	return g.finish(ctx, status, res, out.Attempts)
}

// gated records the call as awaiting_approval, asks the engine for an approval and
// blocks up to MCP_TOOL_WAIT for the decision (DESIGN 5.3 rule 5, D-014).
func (g *gateCall) gated(ctx context.Context) (string, bool) {
	s, t := g.s, g.t
	g.row.Status = store.ToolAwaitingApproval
	if err := g.insert(ctx); err != nil {
		g.log.Error("record gated tool call failed", "error", err)
		return t.Name + " failed with an internal error; try the call again", true
	}
	appr, err := s.engine.RequestApproval(ctx, engine.ApprovalRequest{
		Token: g.token, ToolName: t.Name, ToolCallID: g.row.ID, Message: t.Message(g.args), Input: g.row.Input,
	})
	if err != nil {
		res := tools.Result{IsError: true, Text: t.Name + " needs a human approval, but the approval could not be requested; try the call again"}
		if errors.Is(err, engine.ErrInvalidRunToken) {
			res.Text = "invalid run token"
		} else {
			g.log.Error("request approval failed", "error", err)
		}
		return g.complete(ctx, store.ToolFailed, res, 1)
	}
	g.log.Info("platform tool waiting for approval", "approval_id", appr.ID)
	waitCtx, cancel := context.WithTimeout(ctx, s.cfg.MCPToolWait)
	decided, err := s.engine.WaitApproval(waitCtx, appr.ID)
	cancel()
	if err != nil || decided == nil || decided.Status == store.ApprovalPending {
		// The wait ended without a decision. The row stays awaiting_approval; once the
		// approval is granted the engine executes the effect under the same key.
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			g.log.Warn("wait for approval ended", "approval_id", appr.ID, "error", err)
		}
		return g.modelJSON(gatePending{Status: "pending_approval", ApprovalID: appr.ID}), false
	}
	// The decision belongs to this call now: record it even if the client went away.
	dctx := context.WithoutCancel(ctx)
	switch decided.Status {
	case store.ApprovalApproved:
		out, err := s.effects.Execute(dctx, t, g.call)
		if err != nil {
			g.log.Error("approved platform tool failed", "error", err)
			return g.complete(dctx, store.ToolFailed, tools.Result{IsError: true, Text: t.Name + " failed with an internal error"}, out.Attempts)
		}
		status := store.ToolExecuted
		if out.Result.IsError {
			status = store.ToolFailed
		}
		return g.complete(dctx, status, out.Result, out.Attempts)
	default:
		status, reason := store.ToolRejected, "rejected by the operator"
		if decided.Status == store.ApprovalExpired {
			status, reason = store.ToolExpired, "approval expired"
		}
		if r := deref(decided.Reason, ""); r != "" {
			reason = r
		}
		res, _ := json.Marshal(gateRejected{Status: "rejected", Reason: reason})
		return g.complete(dctx, status, tools.Result{Output: res}, 1)
	}
}

// finish records a call that never waited: one insert with its final status.
func (g *gateCall) finish(ctx context.Context, status string, res tools.Result, attempts int) (string, bool) {
	text, stored := tools.Render(res, g.s.cfg.ToolOutputMaxBytes)
	g.fill(status, res.IsError, stored, attempts)
	if err := g.insert(ctx); err != nil {
		g.log.Error("record tool call failed", "error", err)
	}
	return text, res.IsError
}

// complete finishes a gated row; the engine's fallback may have finished it first, in
// which case the model gets what the row says.
func (g *gateCall) complete(ctx context.Context, status string, res tools.Result, attempts int) (string, bool) {
	text, stored := tools.Render(res, g.s.cfg.ToolOutputMaxBytes)
	g.fill(status, res.IsError, stored, attempts)
	wctx := context.WithoutCancel(ctx)
	wrote, err := store.CompleteGatedToolCall(wctx, g.s.pool, g.row)
	if err != nil {
		g.log.Error("record tool call outcome failed", "error", err)
		return text, res.IsError
	}
	if !wrote {
		if cur, err := store.GetToolCall(wctx, g.s.pool, g.row.ID); err == nil {
			return string(cur.Output), cur.IsError
		}
	}
	g.s.emitToolCall(wctx, g.row, nil)
	return text, res.IsError
}

func (g *gateCall) fill(status string, isErr bool, stored json.RawMessage, attempts int) {
	now := time.Now().UTC()
	g.row.Status, g.row.IsError, g.row.Output = status, isErr, stored
	g.row.Attempt = max(attempts, 1)
	g.row.FinishedAt = &now
	g.row.DurationMs = now.Sub(g.start).Milliseconds()
}

// insert writes the row and counts it toward the run's tool calls (common.md: the MCP
// row is authoritative for platform tools).
func (g *gateCall) insert(ctx context.Context) error {
	wctx := context.WithoutCancel(ctx)
	if g.s.pool == nil {
		return errors.New("no database")
	}
	if err := store.InsertToolCall(wctx, g.s.pool, g.row); err != nil {
		return err
	}
	run, err := store.AddRunUsageReturning(wctx, g.s.pool, g.row.RunID, 0, 1, 0, 0, 0, 0, 0)
	if err != nil {
		g.log.Warn("count tool call failed", "error", err)
	}
	g.s.emitToolCall(wctx, g.row, run)
	return nil
}

func (g *gateCall) modelJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// modelText redacts and caps a result for the model.
func (s *Server) modelText(res tools.Result) string {
	text, _ := tools.Render(res, s.cfg.ToolOutputMaxBytes)
	return text
}

// emitToolCall publishes the row (and the run counters when they moved) the way the
// engine's own tool_call events look.
func (s *Server) emitToolCall(ctx context.Context, row *store.ToolCall, run *store.Run) {
	payload := map[string]any{"tool_call": row}
	if run != nil {
		payload["run"] = map[string]any{
			"id": run.ID, "step_count": run.StepCount, "tool_call_count": run.ToolCallCount,
			"input_tokens": run.InputTokens, "output_tokens": run.OutputTokens, "cache_read_tokens": run.CacheReadTokens,
			"cache_write_tokens": run.CacheWriteTokens, "cost_usd_micros": run.CostUSDMicros,
		}
	}
	s.emit(ctx, row.RunID, "tool_call", payload)
}

// runTokenOf returns the run_token string in a call's arguments, or "".
func runTokenOf(args json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(args, &in) != nil {
		return ""
	}
	tok, _ := in[tools.RunTokenField].(string)
	return strings.TrimSpace(tok)
}

func deref(p *string, def string) string {
	if p == nil || *p == "" {
		return def
	}
	return *p
}

// acceptFinish is the finish tool's view of the engine: schema failures and invalid
// tokens become model-visible errors (DESIGN 5.6, 5.7 rule 3).
func (s *Server) acceptFinish(ctx context.Context, token string, result json.RawMessage) error {
	if s.engine == nil {
		return errors.New("engine unavailable")
	}
	err := s.engine.AcceptFinish(ctx, token, result)
	var fve *engine.FinishValidationError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &fve):
		return &tools.ModelError{Message: fve.Message}
	case errors.Is(err, engine.ErrInvalidRunToken):
		return &tools.ModelError{Message: "invalid run token"}
	}
	return err
}

// finishRejecter is the engine's count of finish calls the MCP input schema refused
// (DESIGN 5.6: they count toward the three attempts). It is not part of engine.Service,
// so an engine without it simply does not count them.
type finishRejecter interface {
	RejectFinish(ctx context.Context, token, message string)
}

func (s *Server) rejectFinish(ctx context.Context, token, message string) {
	if r, ok := s.engine.(finishRejecter); ok {
		r.RejectFinish(ctx, token, message)
	}
}
