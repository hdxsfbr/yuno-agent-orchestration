package engine

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/config"
	"yuno/internal/events"
	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// This file is the contract between the engine and its callers (HTTP API, MCP server,
// eval runner). The implementation is in engine.go, run.go and node.go.

// TriggerRequest starts a run. Either WorkflowID (with Input) or AgentID with Task for
// a one-node "quick run" (DESIGN section 9, wave 1).
type TriggerRequest struct {
	WorkflowID string
	Input      json.RawMessage
	AgentID    string
	Task       string
	Limits     store.Limits
	Trigger    string // store.TriggerManual | TriggerSchedule | TriggerChannel | TriggerEval
	TriggerRef string
	// Model, when set, overrides the agent's model for this run as an OpenClaw model
	// ref, e.g. "scripted/scripted-1" (DESIGN 5.14 rule 9). The engine applies it to
	// the node session before the turn.
	Model string
	// ScriptID, when set, is written into the task message as "script_id: <id>" so the
	// scripted provider can find the registered script for this run.
	ScriptID string
}

// RunContext is what a run token resolves to (DESIGN 5.7 rule 3).
type RunContext struct {
	RunID            string
	RunNodeID        string
	NodeID           string
	AgentID          string
	OpenClawAgentID  string
	OutputSchema     json.RawMessage
	ToolAllowlist    []string
	InteractionRules map[string]string
}

// ErrInvalidRunToken is returned for unknown, expired or finished tokens.
var ErrInvalidRunToken = errors.New("invalid run token")

// ErrNotPending is returned by decisions on approvals that are not pending (409).
var ErrNotPending = errors.New("approval is not pending")

// FinishValidationError carries the schema message the model should see as a tool
// result (DESIGN 5.6: the turn continues).
type FinishValidationError struct {
	Message string
}

func (e *FinishValidationError) Error() string { return "finish: " + e.Message }

// RuntimeView is what OpenClaw holds for an agent (GET /api/agents/{id}/runtime).
type RuntimeView struct {
	OpenClawAgentID string          `json:"openclaw_agent_id"`
	Materialized    bool            `json:"materialized"`
	InSync          bool            `json:"in_sync"`
	Workspace       string          `json:"workspace,omitempty"`
	ConfigEntry     json.RawMessage `json:"config_entry,omitempty"`
	ExecPolicy      json.RawMessage `json:"exec_policy,omitempty"`
	Files           []string        `json:"files"`
}

// Service is the engine as seen by the HTTP API, the MCP server and the eval runner.
type Service interface {
	// MaterializeAgent pushes the agent's current configuration to OpenClaw (DESIGN
	// 5.14 rule 4) and returns the runtime view. Called after create and update.
	MaterializeAgent(ctx context.Context, agentID string) (*RuntimeView, error)
	// AgentRuntime reports what OpenClaw currently holds without writing.
	AgentRuntime(ctx context.Context, agentID string) (*RuntimeView, error)
	// Trigger validates and queues a run (422 on bad input, 404 on unknown ids, 429 on
	// guardrail rate limits are returned as typed errors by the implementation).
	Trigger(ctx context.Context, req TriggerRequest) (*store.Run, error)
	// Cancel flags a run for cancellation (DESIGN 5.2 rule 2).
	Cancel(ctx context.Context, runID, by string) (*store.Run, error)
	// ResolveRunToken maps a token to its run context; ok=false when invalid.
	ResolveRunToken(token string) (RunContext, bool)
	// AcceptFinish validates result against the node's output schema, stores it and
	// tells the engine the node is done. Returns *FinishValidationError on schema
	// failure (the caller returns the message to the model) or ErrInvalidRunToken.
	AcceptFinish(ctx context.Context, token string, result json.RawMessage) error
}

// Deps is everything the engine needs. The HTTP layer builds it in httpapi.Run.
type Deps struct {
	Cfg      config.Config
	Log      *slog.Logger
	Pool     *pgxpool.Pool
	Gateway  openclaw.Client
	Hub      *events.Hub
	Recorder *events.Recorder
	Clock    Clock
	// StateDir is the gateway state directory as mounted in the api container
	// (workspace files are written under it).
	StateDir string
}
