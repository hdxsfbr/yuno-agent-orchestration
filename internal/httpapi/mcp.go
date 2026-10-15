package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"yuno/internal/config"
	"yuno/internal/engine"
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

// mcpTool adapts a registry tool to an MCP handler. Tool failures are tool results
// with isError set, so the model sees them and can correct itself; internal failures
// are logged and reported to the model without detail.
func (s *Server) mcpTool(t *tools.Tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		start := time.Now()
		res, err := t.Run(ctx, tools.Call{Input: req.Params.Arguments}, s.cfg.ToolOutputMaxBytes)
		if err != nil {
			s.log.Error("mcp tool failed", "tool", t.Name, "error", err)
			res = tools.Result{IsError: true, Text: t.Name + " failed with an internal error; try the call again"}
		}
		s.log.Info("mcp tool call", "tool", t.Name, "is_error", res.IsError, "duration_ms", time.Since(start).Milliseconds())
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: res.Text}},
			IsError: res.IsError,
		}, nil
	}
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
