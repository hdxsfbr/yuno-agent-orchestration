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
	Trigger    string // store.TriggerManual | TriggerSchedule | TriggerChannel | TriggerEval | TriggerIssue
	TriggerRef string
	// Model, when set, overrides the agent's model for this run as an OpenClaw model
	// ref, e.g. "scripted/scripted-1" (DESIGN 5.14 rule 9). The engine applies it to
	// the node session before the turn.
	Model string
	// ScriptID, when set, is written into the task message as "script_id: <id>" so the
	// scripted provider can find the registered script for this run.
	ScriptID string
	// ProjectID names the project the run works on (DESIGN 5.17 rule 4): required for a
	// workflow whose definition has `project: true`, refused for one without it, and
	// optional for a quick run, whose one-node definition then opts in.
	ProjectID string
	// MergeRequestID names the merge request record the run updates, from its source
	// branch instead of the default one (DESIGN 5.22 rule 1): the store's id of the
	// record, never the provider's number, "" for a run that names none. The engine
	// ignores it until the queue-time checks and the clone of that rule are built.
	MergeRequestID string
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

// ChatContext is what a chat token resolves to (DESIGN 5.15 rule 1): the agent that
// owns it, where its workspace is, and its most recent conversation, the one with the
// latest message ("" when it has none), which chat-time tool calls are recorded
// against (rule 3) and delegated runs report back to (rule 6).
type ChatContext struct {
	AgentID          string
	AgentName        string
	OpenClawAgentID  string
	Workspace        string
	ConversationID   string
	ToolAllowlist    []string
	InteractionRules map[string]string
}

// ErrInvalidRunToken is returned for unknown, expired or finished tokens.
var ErrInvalidRunToken = errors.New("invalid run token")

// ErrNotPending is returned by decisions on approvals that are not pending (409).
var ErrNotPending = errors.New("approval is not pending")

// ErrNotImplemented marks a contract method a later wave fills in.
var ErrNotImplemented = errors.New("not implemented in this wave")

// Decisions a human can make on an approval (DESIGN 5.3). ApproveAlways is only
// meaningful for exec approvals, where it also extends the agent's allowlist.
const (
	DecisionApprove       = "approve"
	DecisionApproveAlways = "approve_always"
	DecisionReject        = "reject"
)

// ApprovalRequest asks the engine to gate a platform side effect on a human decision
// (DESIGN 5.3 rule 5). Exec approvals come from gateway events and node approvals from
// the workflow, so callers only ever request kind store.ApprovalKindTool.
type ApprovalRequest struct {
	// Token is the run token of the calling visit: it identifies run, node visit and
	// agent, and the approval is refused with ErrInvalidRunToken when it is not live.
	Token string
	// ToolName and ToolCallID name the gated call. ToolCallID is the tool_calls row the
	// caller recorded (status awaiting_approval, with its idempotency key) so the
	// engine's fallback execution can find it.
	ToolName   string
	ToolCallID string
	// Message is what the human reads; Input is the full tool input for the inbox.
	Message string
	Input   json.RawMessage
}

// SideEffectExecutor executes an approved side-effecting tool call outside the MCP
// request that asked for it: the fallback of DESIGN 5.3 rule 5, used when the approval
// is granted after the MCP wait gave up. Implemented by the tools package; the engine
// records the outcome in runs.state.pending_effects[].
type SideEffectExecutor interface {
	// ExecuteApproved loads the tool_calls row, runs the effect under its idempotency
	// key and updates the row. It returns the result the model would have seen.
	ExecuteApproved(ctx context.Context, toolCallID string) (output json.RawMessage, isError bool, err error)
}

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
	// RemoveAgent takes a soft-deleted agent out of OpenClaw (agents.delete and its
	// workspace directory). Called after delete; the caller logs a failure, the
	// deletion stands and the start-up sync retries it.
	RemoveAgent(ctx context.Context, agentID string) error
	// Trigger validates and queues a run (422 on bad input, 404 on unknown ids, 429 on
	// guardrail rate limits are returned as typed errors by the implementation).
	Trigger(ctx context.Context, req TriggerRequest) (*store.Run, error)
	// Cancel flags a run for cancellation (DESIGN 5.2 rule 2).
	Cancel(ctx context.Context, runID, by string) (*store.Run, error)
	// Retry re-queues a failed, timed-out or cancelled run from the node it stopped at
	// (DESIGN 5.21 rules 1 to 3). Refusals are *RetryError (409), bad limits a
	// *ValidationError (422), an unknown run store.ErrNotFound.
	Retry(ctx context.Context, runID, by string, limits *store.Limits) (*store.Run, error)
	// ResolveRunToken maps a token to its run context; ok=false when invalid.
	ResolveRunToken(token string) (RunContext, bool)
	// ResolveChatToken maps a chat token to its agent and most recent conversation
	// (DESIGN 5.15 rule 1); ok=false for an unknown token or a deleted agent's.
	ResolveChatToken(token string) (ChatContext, bool)
	// AcceptFinish validates result against the node's output schema, stores it and
	// tells the engine the node is done. Returns *FinishValidationError on schema
	// failure (the caller returns the message to the model) or ErrInvalidRunToken.
	AcceptFinish(ctx context.Context, token string, result json.RawMessage) error

	// RequestApproval records a pending approval of kind tool for the token's visit,
	// registers it with the gateway as a plugin approval (external_id, two-phase), moves
	// the run and node to waiting_approval, publishes approval.requested and returns at
	// once. The approval expires at requested_at + approval_timeout_seconds.
	RequestApproval(ctx context.Context, req ApprovalRequest) (*store.Approval, error)
	// WaitApproval blocks until the approval is decided or ctx ends, and returns the
	// decided approval or ctx.Err(). While a caller waits, the decision is delivered to
	// it and the engine does not execute the effect itself; once no caller waits, a
	// later grant triggers Deps.Effects.ExecuteApproved (DESIGN 5.3 rule 5).
	WaitApproval(ctx context.Context, approvalID string) (*store.Approval, error)
	// DecideApproval applies a human decision (DecisionApprove, DecisionApproveAlways or
	// DecisionReject) by the principal `by` with an optional reason. Exec approvals are
	// resolved on the gateway (exec.approval.resolve), tool approvals on the gateway's
	// plugin approval too (plugin.approval.resolve), node approvals resume the run.
	// Returns ErrNotPending when the approval is not pending (409) and store.ErrNotFound
	// when it does not exist.
	DecideApproval(ctx context.Context, approvalID, decision, by, reason string) (*store.Approval, error)
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
	// Effects executes approved side effects outside the MCP request that asked for
	// them (DESIGN 5.3 rule 5). nil disables that fallback.
	Effects SideEffectExecutor
}
