package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"yuno/internal/model"
	"yuno/internal/store"
)

// maxResponseBytes bounds what the runner reads from one API response.
const maxResponseBytes = 32 << 20

// Client is the eval runner's view of the api: a plain HTTP client of the endpoints in
// api/openapi.yaml. It holds no state beyond the connection settings.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient returns a client for the api at base. When token is not empty it is sent
// as a bearer token on every request (the api requires it when API_TOKEN is set).
func NewClient(base, token string) *Client {
	return &Client{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

// APIError is a non-2xx answer from the api, decoded from the Error schema when the
// body has one.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Code != "" {
		return fmt.Sprintf("%s %s: %d %s: %s", e.Method, e.Path, e.Status, e.Code, msg)
	}
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.Path, e.Status, msg)
}

// statusOf returns the HTTP status carried by err, or 0 when err is not an *APIError.
func statusOf(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// AgentRequest is the body of POST /api/agents the runner sends.
type AgentRequest struct {
	Name          string       `json:"name"`
	Role          string       `json:"role,omitempty"`
	SystemPrompt  string       `json:"system_prompt,omitempty"`
	Model         string       `json:"model"`
	ToolAllowlist []string     `json:"tool_allowlist"`
	Limits        store.Limits `json:"limits"`
}

// RunRequest is the body of POST /api/runs the runner sends (a one-node quick run).
type RunRequest struct {
	AgentID  string       `json:"agent_id"`
	Task     string       `json:"task"`
	Model    string       `json:"model,omitempty"`
	ScriptID string       `json:"script_id,omitempty"`
	Limits   store.Limits `json:"limits"`
}

// scriptRegistration is the body of POST /api/evals/scripts.
type scriptRegistration struct {
	ScriptID string             `json:"script_id"`
	Steps    []model.ScriptStep `json:"steps"`
}

// WaitReady polls GET /readyz every interval until it answers 200 or ctx ends.
func (c *Client) WaitReady(ctx context.Context, interval time.Duration) error {
	var last error
	for {
		last = c.do(ctx, http.MethodGet, "/readyz", nil, nil)
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("api not ready: %w (last error: %v)", ctx.Err(), last)
		case <-time.After(interval):
		}
	}
}

// CreateAgent creates and materialises an agent.
func (c *Client) CreateAgent(ctx context.Context, req AgentRequest) (*store.Agent, error) {
	if req.ToolAllowlist == nil {
		req.ToolAllowlist = []string{}
	}
	var out store.Agent
	if err := c.do(ctx, http.MethodPost, "/api/agents", req, &out); err != nil {
		return nil, err
	}
	if out.ID == "" {
		return nil, errors.New("POST /api/agents: response has no id")
	}
	return &out, nil
}

// DeleteAgent soft-deletes an agent. An agent that is already gone is not an error.
func (c *Client) DeleteAgent(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/api/agents/"+url.PathEscape(id), nil, nil)
	if statusOf(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// RegisterScript registers (or replaces) a script for the scripted provider.
func (c *Client) RegisterScript(ctx context.Context, scriptID string, steps []model.ScriptStep) error {
	return c.do(ctx, http.MethodPost, "/api/evals/scripts", scriptRegistration{ScriptID: scriptID, Steps: steps}, nil)
}

// CreateRun triggers a quick run.
func (c *Client) CreateRun(ctx context.Context, req RunRequest) (*store.Run, error) {
	var out store.Run
	if err := c.do(ctx, http.MethodPost, "/api/runs", req, &out); err != nil {
		return nil, err
	}
	if out.ID == "" {
		return nil, errors.New("POST /api/runs: response has no id")
	}
	return &out, nil
}

// GetRun fetches a run.
func (c *Client) GetRun(ctx context.Context, id string) (*store.Run, error) {
	var out store.Run
	if err := c.do(ctx, http.MethodGet, "/api/runs/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelRun requests cancellation. A run that already finished (409) is not an error.
func (c *Client) CancelRun(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodPost, "/api/runs/"+url.PathEscape(id)+"/cancel", nil, nil)
	if statusOf(err) == http.StatusConflict {
		return nil
	}
	return err
}

// GetTrace fetches the correlated trace of a run.
func (c *Client) GetTrace(ctx context.Context, id string) (*store.Trace, error) {
	var out store.Trace
	if err := c.do(ctx, http.MethodGet, "/api/runs/"+url.PathEscape(id)+"/trace", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// do performs one request. in, when not nil, is sent as JSON; out, when not nil, receives
// the JSON response body.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("%s %s: encode request: %w", method, path, err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeAPIError(method, path, resp.StatusCode, raw)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

// decodeAPIError builds an APIError from the Error schema, falling back to the status.
func decodeAPIError(method, path string, status int, raw []byte) *APIError {
	ae := &APIError{Method: method, Path: path, Status: status}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &env) == nil {
		ae.Code, ae.Message = env.Error.Code, env.Error.Message
	}
	return ae
}
