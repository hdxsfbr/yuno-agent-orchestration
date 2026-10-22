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
	"yuno/internal/project"
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
	// record, never the provider's number, "" for a run that names none; checked at
	// queue time (MergeRequestForRequest) and cloned from by the first visit (rule 2).
	MergeRequestID string
}

// RunContext is what a run token resolves to (DESIGN 5.7 rule 3).
type RunContext struct {
	RunID           string
	RunNodeID       string
	NodeID          string
	AgentID         string
	OpenClawAgentID string
	// Workspace is the visit's private workspace (DESIGN 5.30), where its run directory
	// and checkout are; "" when the visit has none.
	Workspace        string
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
	// Cancel cancels a run (DESIGN 5.2 rule 2, 5.33 rules 10 and 11). The run comes back with
	// its command; a finished run is a *CommandError (409 run_finished), a command whose
	// receipt could not be written ErrCommandNotTaken (503).
	Cancel(ctx context.Context, runID, by string) (*store.Run, error)
	// Retry re-queues a failed, timed-out or cancelled run from the node it stopped at
	// (DESIGN 5.21 rules 1 to 3). Refusals are *RetryError (409), bad limits a
	// *ValidationError (422), an unknown run store.ErrNotFound.
	Retry(ctx context.Context, runID, by string, limits *store.Limits) (*store.Run, error)
	// RetryFrom is Retry from a step the run reached (DESIGN 5.33 rules 16 and 17); an empty
	// fromNode is Retry.
	RetryFrom(ctx context.Context, runID, by, fromNode string, limits *store.Limits) (*store.Run, error)
	// Pause, Resume and Steer are the recovery commands of DESIGN 5.33 rules 14, 15 and 18.
	// Each answers the run with its command; a refusal is a *CommandError (409), a bad
	// note or message a *ValidationError (422).
	Pause(ctx context.Context, runID, by string) (*store.Run, error)
	Resume(ctx context.Context, runID, by, note string) (*store.Run, error)
	Steer(ctx context.Context, runID, by, nodeID, message string) (*store.Run, error)
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

	// AskOperator records a question of the visit that holds the run token (DESIGN 5.24
	// rule 2) as an approval of kind question, pauses the run and the node on it,
	// publishes approval.requested and returns the pending row at once;
	// ErrInvalidRunToken for an unknown or finished token. WaitQuestion then blocks for
	// the decision. A question is never registered with the gateway.
	AskOperator(ctx context.Context, token, question string, options []string) (*store.Approval, error)
	// WaitQuestion blocks until the question is decided or ctx ends, and returns the
	// decided row or ctx.Err() (rules 2 and 4); a decision while a caller waits goes to
	// that caller first (rule 5 (a)).
	WaitQuestion(ctx context.Context, id string) (*store.Approval, error)
	// AnswerQuestion answers a pending question as the principal by (rule 3: "api" for the
	// HTTP API, "chat:<agent name>" for answer_question, rule 9) and delivers the decision
	// as rule 5 says. Returns ErrNotPending when the row is not pending (409) and
	// store.ErrNotFound when it does not exist.
	AnswerQuestion(ctx context.Context, id, answer, by string) (*store.Approval, error)
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
	// Artifacts collects the files an agent visit left under runs/<run id>/out/ (DESIGN
	// 5.25 rule 2) when the visit ends; nil disables collection.
	Artifacts ArtifactCollector
	// Plans keeps plan tickets true to their runs (DESIGN 5.29 rules 6 and 7); nil skips it.
	Plans PlanTracker
	// Scripts, when set, gives a run the platform starts itself (trigger issue or plan) the
	// script an eval bound to its project, so those runs can be scripted too; nil leaves
	// every such run as it is.
	Scripts ScriptBinder
}

// ScriptBinder is model.ScriptBinder: TakeBound removes and returns the script id queued
// for the next run of the trigger on the project.
type ScriptBinder interface {
	TakeBound(projectID, trigger string) (scriptID string, ok bool)
}

// ScriptedRunModel is the model override of a run that takes a bound script.
const ScriptedRunModel = "scripted/scripted-1"

// PlanTracker is the part of the plans service the engine calls: OnRunTerminal for every
// run that reaches a terminal status (an escalated run included, 5.29 rule 6), and OnRunRetried.
type PlanTracker interface {
	OnRunTerminal(ctx context.Context, run *store.Run)
	// OnRunRetried publishes the change of a ticket an operator's retry moved out of failed.
	OnRunRetried(planID, projectID string)
}

// ArtifactCollector collects the files a visit left under runs/<run id>/out/ (DESIGN
// 5.25 rule 2); nil disables collection.
type ArtifactCollector interface {
	Collect(ctx context.Context, in ArtifactInput)
}

// ArtifactInput is one visit's end: the visit's private workspace (DESIGN 5.30), the run
// and the visit, the project configuration when the run has a checkout (nil otherwise),
// and the branch the checkout is on (the request's source branch for a 5.22 run, else
// the default branch).
type ArtifactInput struct {
	Workspace string
	Run       *store.Run
	Node      *store.RunNode
	Project   *project.Config
	Branch    string
	// SkipDiff is set for a visit inside a parallel branch the join does not name as its
	// checkout (DESIGN 5.26 rule 11): the generated diff.patch is not produced, so the
	// run's one patch never flips between branches. BranchHead names the branch for the
	// log line.
	SkipDiff   bool
	BranchHead string
}
