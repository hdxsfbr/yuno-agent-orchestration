package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

// Run statuses (DESIGN 5.1 rule 1).
const (
	RunQueued          = "queued"
	RunRunning         = "running"
	RunWaitingApproval = "waiting_approval"
	RunSucceeded       = "succeeded"
	RunFailed          = "failed"
	RunCancelled       = "cancelled"
	RunTimedOut        = "timed_out"
)

// Node statuses (DESIGN 5.1 rule 3).
const (
	NodePending         = "pending"
	NodeRunning         = "running"
	NodeWaitingApproval = "waiting_approval"
	NodeSucceeded       = "succeeded"
	NodeFailed          = "failed"
	NodeSkipped         = "skipped"
)

// Tool call statuses (DESIGN 5.3, 5.7).
const (
	ToolExecuted         = "executed"
	ToolAwaitingApproval = "awaiting_approval"
	ToolApproved         = "approved"
	ToolRejected         = "rejected"
	ToolBlocked          = "blocked"
	ToolExpired          = "expired"
	ToolFailed           = "failed"
)

// Triggers.
const (
	TriggerManual   = "manual"
	TriggerSchedule = "schedule"
	TriggerChannel  = "channel"
	TriggerEval     = "eval"
	// TriggerIssue marks a run the intake loop created for a labelled issue; its
	// trigger_ref is "<project id>#<n>" (DESIGN 5.23 units).
	TriggerIssue = "issue"
	// TriggerPlan marks a ship run the wave watcher started; its trigger_ref is
	// "plan:<plan id>:wave:<n>:<attempt>" (DESIGN 5.29 rule 12), unique by runs_plan_wave_idx.
	TriggerPlan = "plan"
)

// ErrMergeRequestBusy is CreateRun's and RetryRun's answer when another live run already
// names the same merge request record (runs_merge_request_live_idx, DESIGN 5.22 rule 1):
// one run at a time pushes onto a request's branch.
var ErrMergeRequestBusy = errors.New("another live run is updating this merge request")

// ErrIssueBusy is CreateRun's and RetryRun's answer when a live run already exists for
// the same issue (runs_issue_live_idx, DESIGN 5.23 rule 3): one live run per issue.
var ErrIssueBusy = errors.New("this issue already has a live run")

// IsTerminal reports whether a run status is final.
func IsTerminal(status string) bool {
	switch status {
	case RunSucceeded, RunFailed, RunCancelled, RunTimedOut:
		return true
	}
	return false
}

// Limits is the per-workflow (and per-run snapshot) operating limit set. Zero means
// "use the default"; values are clamped to platform maximums at trigger time
// (DESIGN 5.2).
type Limits struct {
	MaxSteps               int     `json:"max_steps,omitempty"`
	MaxDurationSeconds     int     `json:"max_duration_seconds,omitempty"`
	MaxNodeIterations      int     `json:"max_node_iterations,omitempty"`
	MaxLoopVisits          int     `json:"max_loop_visits,omitempty"`
	MaxCostUSD             float64 `json:"max_cost_usd,omitempty"`
	MaxToolCalls           int     `json:"max_tool_calls,omitempty"`
	ApprovalTimeoutSeconds int     `json:"approval_timeout_seconds,omitempty"`
}

// DefaultLimits are DESIGN 5.2's defaults.
var DefaultLimits = Limits{
	MaxSteps:               40,
	MaxDurationSeconds:     600,
	MaxNodeIterations:      12,
	MaxLoopVisits:          3,
	MaxCostUSD:             2.00,
	MaxToolCalls:           60,
	ApprovalTimeoutSeconds: 86400,
}

type BlockedPattern struct {
	Name  string `json:"name"`
	Tool  string `json:"tool"`
	Regex string `json:"regex"`
}

// Guardrails (DESIGN 5.11).
type Guardrails struct {
	MaxCostUSDPerRun     float64          `json:"max_cost_usd_per_run,omitempty"`
	MaxRunsPerHour       int              `json:"max_runs_per_hour,omitempty"`
	BlockedTools         []string         `json:"blocked_tools,omitempty"`
	BlockedInputPatterns []BlockedPattern `json:"blocked_input_patterns,omitempty"`
}

// MemoryPolicy (DESIGN 5.10).
type MemoryPolicy struct {
	Enabled             bool `json:"enabled"`
	MaxFacts            int  `json:"max_facts,omitempty"`
	FactTTLDays         int  `json:"fact_ttl_days,omitempty"`
	ConversationWindow  int  `json:"conversation_window,omitempty"`
	ConversationTTLDays int  `json:"conversation_ttl_days,omitempty"`
}

// ExecPolicy is the agent's OpenClaw exec approvals policy (DESIGN 5.3 rule 1).
type ExecPolicy struct {
	Security    string   `json:"security,omitempty"`     // deny | allowlist | full
	Ask         string   `json:"ask,omitempty"`          // off | on-miss | always
	AskFallback string   `json:"ask_fallback,omitempty"` // deny | allowlist | full
	Allowlist   []string `json:"allowlist,omitempty"`    // command patterns
}

type Agent struct {
	ID               string            `db:"id" json:"id"`
	Name             string            `db:"name" json:"name"`
	Role             string            `db:"role" json:"role"`
	SystemPrompt     string            `db:"system_prompt" json:"system_prompt"`
	Model            string            `db:"model" json:"model"`
	Effort           string            `db:"effort" json:"effort"`
	MaxTokens        int               `db:"max_tokens" json:"max_tokens"`
	ToolAllowlist    []string          `db:"tool_allowlist" json:"tool_allowlist"`
	InteractionRules map[string]string `db:"interaction_rules" json:"interaction_rules"`
	Limits           Limits            `db:"limits" json:"limits"`
	Guardrails       Guardrails        `db:"guardrails" json:"guardrails"`
	MemoryPolicy     MemoryPolicy      `db:"memory_policy" json:"memory_policy"`
	Channels         []string          `db:"channels" json:"channels"`
	OpenClawAgentID  *string           `db:"openclaw_agent_id" json:"openclaw_agent_id,omitempty"`
	Skills           []string          `db:"skills" json:"skills"`
	Thinking         string            `db:"thinking" json:"thinking"`
	ExecPolicy       ExecPolicy        `db:"exec_policy" json:"exec_policy"`
	MaterializedHash string            `db:"materialized_hash" json:"materialized_hash"`
	// SeededHash is the sha256 of the role, prompt and allowlist the seed last wrote to a
	// shipped agent (nil for agents made before it was recorded and for other agents).
	SeededHash *string `db:"seeded_hash" json:"-"`
	// ChatToken identifies the agent's chat-time platform tool calls (DESIGN 5.15 rule
	// 1). It is never serialised: the API, events and logs do not carry it.
	ChatToken string     `db:"chat_token" json:"-"`
	CreatedAt time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt time.Time  `db:"updated_at" json:"updated_at"`
	DeletedAt *time.Time `db:"deleted_at" json:"deleted_at,omitempty"`
}

type Skill struct {
	ID          string `db:"id" json:"id"`
	Name        string `db:"name" json:"name"`
	Description string `db:"description" json:"description"`
	Body        string `db:"body" json:"body"`
	// Source is user | library | git | upload (DESIGN 5.27 rule 10); SourceURL, SourceRef
	// (the resolved commit for git) and License are empty for a user skill. FilesCount is
	// the number of bundle files besides SKILL.md; it is read, never written.
	Source     string    `db:"source" json:"source"`
	SourceURL  string    `db:"source_url" json:"source_url"`
	SourceRef  string    `db:"source_ref" json:"source_ref"`
	License    string    `db:"license" json:"license"`
	FilesCount int       `db:"files_count" json:"files_count"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
	UpdatedAt  time.Time `db:"updated_at" json:"updated_at"`
}

// Skill sources (DESIGN 5.27 rule 10).
const (
	SkillSourceUser    = "user"
	SkillSourceLibrary = "library"
	SkillSourceGit     = "git"
	SkillSourceUpload  = "upload"
)

// SkillFile is one bundle file of a skill besides SKILL.md (DESIGN 5.27 rule 5). Mode is
// 0644 (420) or 0755 (493). Content is empty in metadata-only reads.
type SkillFile struct {
	Path    string `json:"path"`
	Content []byte `json:"-"`
	Mode    int    `json:"mode"`
	Size    int    `json:"size"`
	// SHA256 is the digest of the content, set on write and read with or without it.
	SHA256 []byte `json:"-"`
}

// AgentFileEdit is the operator's stored text for one workspace file of an agent (DESIGN
// 5.27 rules 2 and 3): Addition for a generated file, Override for a runtime file.
type AgentFileEdit struct {
	AgentID   string    `json:"agent_id"`
	Name      string    `json:"name"`
	Addition  *string   `json:"addition"`
	Override  *string   `json:"override"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Approval struct {
	ID          string          `db:"id" json:"id"`
	RunID       string          `db:"run_id" json:"run_id"`
	RunNodeID   string          `db:"run_node_id" json:"run_node_id"`
	ToolCallID  *string         `db:"tool_call_id" json:"tool_call_id,omitempty"`
	Kind        string          `db:"kind" json:"kind"` // exec | tool | node | question
	NodeID      string          `db:"node_id" json:"node_id"`
	Message     string          `db:"message" json:"message"`
	Options     json.RawMessage `db:"options" json:"options,omitempty"` // a question's 2 to 5 suggested answers (DESIGN 5.24 rule 2)
	Status      string          `db:"status" json:"status"`             // pending | approved | rejected | expired
	ExternalID  *string         `db:"external_id" json:"external_id,omitempty"`
	RequestedAt time.Time       `db:"requested_at" json:"requested_at"`
	DecidedAt   *time.Time      `db:"decided_at" json:"decided_at,omitempty"`
	DecidedBy   *string         `db:"decided_by" json:"decided_by,omitempty"`
	Reason      *string         `db:"reason" json:"reason,omitempty"`
	Answer      *string         `db:"answer" json:"answer,omitempty"` // an answered question's answer (DESIGN 5.24 rule 3)
	ExpiresAt   time.Time       `db:"expires_at" json:"expires_at"`
	// ConversationID is set, and RunID empty, for an approval raised in a conversation
	// (DESIGN 5.8 rule 5).
	ConversationID *string `db:"conversation_id" json:"conversation_id,omitempty"`
	// ReportedAt is when the approval was announced in the conversation that delegated
	// its run (DESIGN 5.15 rule 10); nil when it was not.
	ReportedAt *time.Time `db:"reported_at" json:"reported_at,omitempty"`
}

type Workflow struct {
	ID          string          `db:"id" json:"id"`
	Name        string          `db:"name" json:"name"`
	Description string          `db:"description" json:"description"`
	Definition  json.RawMessage `db:"definition" json:"definition"`
	Version     int             `db:"version" json:"version"`
	TemplateKey *string         `db:"template_key" json:"template_key,omitempty"`
	// FromTemplate is the key of the template the workflow was instantiated from
	// (DESIGN 5.15 rule 13); nil for workflows built from scratch.
	FromTemplate *string `db:"from_template" json:"from_template,omitempty"`
	// SeededHash is the sha256 of the definition the seed last wrote to a shipped copy
	// (nil for copies made before it was recorded and for workflows the seed never wrote).
	SeededHash *string    `db:"seeded_hash" json:"-"`
	CreatedAt  time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt  time.Time  `db:"updated_at" json:"updated_at"`
	DeletedAt  *time.Time `db:"deleted_at" json:"deleted_at,omitempty"`
}

type Run struct {
	ID                string          `db:"id" json:"id"`
	WorkflowID        string          `db:"workflow_id" json:"workflow_id"`
	WorkflowVersion   int             `db:"workflow_version" json:"workflow_version"`
	WorkflowSnapshot  json.RawMessage `db:"workflow_snapshot" json:"workflow_snapshot,omitempty"`
	Trigger           string          `db:"trigger" json:"trigger"`
	TriggerRef        *string         `db:"trigger_ref" json:"trigger_ref,omitempty"`
	Status            string          `db:"status" json:"status"`
	Input             json.RawMessage `db:"input" json:"input"`
	State             json.RawMessage `db:"state" json:"state"`
	Limits            Limits          `db:"limits" json:"limits"`
	CurrentNode       *string         `db:"current_node" json:"current_node,omitempty"`
	ResumeNodeVisitID *string         `db:"resume_node_visit_id" json:"resume_node_visit_id,omitempty"`
	StepCount         int             `db:"step_count" json:"step_count"`
	ToolCallCount     int             `db:"tool_call_count" json:"tool_call_count"`
	Attempt           int             `db:"attempt" json:"attempt"`
	CancelRequested   bool            `db:"cancel_requested" json:"cancel_requested"`
	CancelBy          *string         `db:"cancel_by" json:"cancel_by,omitempty"`
	FailureReason     *string         `db:"failure_reason" json:"failure_reason,omitempty"`
	CostUSDMicros     int64           `db:"cost_usd_micros" json:"cost_usd_micros"`
	InputTokens       int64           `db:"input_tokens" json:"input_tokens"`
	OutputTokens      int64           `db:"output_tokens" json:"output_tokens"`
	CacheReadTokens   int64           `db:"cache_read_tokens" json:"cache_read_tokens"`
	CacheWriteTokens  int64           `db:"cache_write_tokens" json:"cache_write_tokens"`
	PausedAt          *time.Time      `db:"paused_at" json:"paused_at,omitempty"`
	PausedTotalMs     int64           `db:"paused_total_ms" json:"paused_total_ms"`
	CreatedAt         time.Time       `db:"created_at" json:"created_at"`
	StartedAt         *time.Time      `db:"started_at" json:"started_at,omitempty"`
	FinishedAt        *time.Time      `db:"finished_at" json:"finished_at,omitempty"`
	UpdatedAt         time.Time       `db:"updated_at" json:"updated_at"`
	// ReportedAt is when a channel-triggered run's outcome was sent back into its
	// conversation (DESIGN 5.15 rule 9); nil when it was not.
	ReportedAt *time.Time `db:"reported_at" json:"reported_at,omitempty"`
	// Toolchain is what the project checkout's manifest said (DESIGN 5.16 rule 5): go,
	// node, python or default; nil for a run without a checkout.
	Toolchain *string `db:"toolchain" json:"toolchain,omitempty"`
	// ProjectID and Project are the project the run works on and the snapshot of its
	// settings taken when the run was queued (DESIGN 5.17 rule 4); nil without one.
	ProjectID *string          `db:"project_id" json:"project_id,omitempty"`
	Project   *ProjectSettings `db:"project_snapshot" json:"project,omitempty"`
	// Retries counts the operator's retries from the step the run stopped at (DESIGN
	// 5.21 rule 3). MergeRequest is the request the run opened (DESIGN 5.20 rule 6),
	// not a column: FillRunMergeRequests sets it for the API's views, nil when none.
	Retries      int           `db:"retries" json:"retries"`
	MergeRequest *MergeRequest `db:"-" json:"merge_request,omitempty"`
	// MergeRequestID is the merge request record the run names and updates (DESIGN 5.22
	// rule 1), checked and written when the run is queued; nil for a run that works from
	// the default branch. The record a run opened is merge_requests.run_id, not this.
	MergeRequestID *string `db:"merge_request_id" json:"merge_request_id,omitempty"`
	// CheckoutSHA is the commit the project checkout was at: HEAD right after the clone,
	// and the pushed commit after open_merge_request pushed (DESIGN 5.22 rules 2 and 4),
	// the base the branch guard compares against. A full 40-hex id, nil before the clone
	// and for a run cloned before migration 0013.
	CheckoutSHA *string `db:"checkout_sha" json:"checkout_sha,omitempty"`
	// MergedSHA is the default branch commit the platform merged into the checkout of a run
	// on a merge request before its first turn (DESIGN 5.22 rule 13): the second parent of the
	// run's next push, which clears it. Nil when nothing was merged.
	MergedSHA *string `db:"merged_sha" json:"-"`
	// AgentNames and Models are not columns; FillRunParticipants sets them for list views.
	AgentNames []string `db:"-" json:"agent_names,omitempty"`
	Models     []string `db:"-" json:"models,omitempty"`
}

type RunNode struct {
	ID                 string          `db:"id" json:"id"`
	RunID              string          `db:"run_id" json:"run_id"`
	NodeID             string          `db:"node_id" json:"node_id"`
	AgentID            *string         `db:"agent_id" json:"agent_id,omitempty"`
	Visit              int             `db:"visit" json:"visit"`
	Iteration          int             `db:"iteration" json:"iteration"`
	Status             string          `db:"status" json:"status"`
	Messages           json.RawMessage `db:"messages" json:"messages"`
	PendingToolResults json.RawMessage `db:"pending_tool_results" json:"pending_tool_results,omitempty"`
	Output             json.RawMessage `db:"output" json:"output,omitempty"`
	FailureReason      *string         `db:"failure_reason" json:"failure_reason,omitempty"`
	SessionKey         *string         `db:"session_key" json:"session_key,omitempty"`
	OpenClawRunID      *string         `db:"openclaw_run_id" json:"openclaw_run_id,omitempty"`
	StartedAt          *time.Time      `db:"started_at" json:"started_at,omitempty"`
	FinishedAt         *time.Time      `db:"finished_at" json:"finished_at,omitempty"`
	// QuestionID names the question a continuation visit was created for (DESIGN 5.24
	// rule 5); nil for every other visit.
	QuestionID *string `db:"question_id" json:"question_id,omitempty"`
	// ParkedAt is when the visit parked on its pending question and released its worker
	// (DESIGN 5.24 rule 5); nil for a visit that never parked. Not part of the contract.
	ParkedAt *time.Time `db:"parked_at" json:"-"`
	// BranchID is the branch of a fork visit this visit was made in (DESIGN 5.26 rule 4);
	// nil for a visit outside any fork.
	BranchID *string `db:"branch_id" json:"branch_id,omitempty"`
	// WorkspaceDir is the visit's private workspace (DESIGN 5.30 rule 2) and RepoSHA the
	// run repository commit its checkout ended at (rule 4); nil until the visit has them.
	// Not part of the contract.
	WorkspaceDir *string `db:"workspace_dir" json:"-"`
	RepoSHA      *string `db:"repo_sha" json:"-"`
}

// RunBranch is one branch of a fork visit (DESIGN 5.26 rule 3): the part of the run that
// started at one outgoing edge of the fork and runs at the same time as its siblings
// until the join. Head, the fork edge's target, names it; CurrentNode is where it is.
type RunBranch struct {
	ID            string     `db:"id" json:"id"`
	RunID         string     `db:"run_id" json:"run_id"`
	ForkVisitID   string     `db:"fork_visit_id" json:"fork_visit_id"`
	ForkID        string     `db:"fork_id" json:"fork_id"`
	Head          string     `db:"head" json:"head"`
	Status        string     `db:"status" json:"status"`
	CurrentNode   string     `db:"current_node" json:"current_node"`
	FailureReason *string    `db:"failure_reason" json:"failure_reason,omitempty"`
	StartedAt     time.Time  `db:"started_at" json:"started_at"`
	FinishedAt    *time.Time `db:"finished_at" json:"finished_at,omitempty"`
}

// Branch statuses (DESIGN 5.26 rule 12).
const (
	BranchRunning         = "running"
	BranchWaitingApproval = "waiting_approval" // the branch's newest visit parked on a question
	BranchSucceeded       = "succeeded"
	BranchFailed          = "failed"
	BranchCancelled       = "cancelled"
)

type ModelCall struct {
	ID               string     `db:"id" json:"id"`
	RunID            string     `db:"run_id" json:"run_id"`
	RunNodeID        string     `db:"run_node_id" json:"run_node_id"`
	Provider         string     `db:"provider" json:"provider"`
	Model            string     `db:"model" json:"model"`
	RequestID        *string    `db:"request_id" json:"request_id,omitempty"`
	StopReason       *string    `db:"stop_reason" json:"stop_reason,omitempty"`
	InputTokens      int64      `db:"input_tokens" json:"input_tokens"`
	OutputTokens     int64      `db:"output_tokens" json:"output_tokens"`
	CacheReadTokens  int64      `db:"cache_read_tokens" json:"cache_read_tokens"`
	CacheWriteTokens int64      `db:"cache_write_tokens" json:"cache_write_tokens"`
	CostUSDMicros    int64      `db:"cost_usd_micros" json:"cost_usd_micros"`
	DurationMs       int64      `db:"duration_ms" json:"duration_ms"`
	Attempt          int        `db:"attempt" json:"attempt"`
	Error            *string    `db:"error" json:"error,omitempty"`
	StartedAt        time.Time  `db:"started_at" json:"started_at"`
	FinishedAt       *time.Time `db:"finished_at" json:"finished_at,omitempty"`
}

// ToolCall is one recorded tool call. A chat-time platform tool call (DESIGN 5.15 rule
// 3) has no run: RunID and RunNodeID read as "" and ConversationID names the calling
// agent's most recent conversation, or is nil when it has none.
type ToolCall struct {
	ID             string          `db:"id" json:"id"`
	RunID          string          `db:"run_id" json:"run_id"`
	RunNodeID      string          `db:"run_node_id" json:"run_node_id"`
	ConversationID *string         `db:"conversation_id" json:"conversation_id,omitempty"`
	ModelCallID    *string         `db:"model_call_id" json:"model_call_id,omitempty"`
	ToolUseID      string          `db:"tool_use_id" json:"tool_use_id"`
	ToolName       string          `db:"tool_name" json:"tool_name"`
	Input          json.RawMessage `db:"input" json:"input"`
	Output         json.RawMessage `db:"output" json:"output,omitempty"`
	IsError        bool            `db:"is_error" json:"is_error"`
	Status         string          `db:"status" json:"status"`
	IdempotencyKey *string         `db:"idempotency_key" json:"idempotency_key,omitempty"`
	Attempt        int             `db:"attempt" json:"attempt"`
	DurationMs     int64           `db:"duration_ms" json:"duration_ms"`
	StartedAt      *time.Time      `db:"started_at" json:"started_at,omitempty"`
	FinishedAt     *time.Time      `db:"finished_at" json:"finished_at,omitempty"`
}

type Span struct {
	ID        string          `db:"id" json:"id"`
	RunID     string          `db:"run_id" json:"run_id"`
	ParentID  *string         `db:"parent_id" json:"parent_id,omitempty"`
	Kind      string          `db:"kind" json:"kind"`
	Name      string          `db:"name" json:"name"`
	StartedAt time.Time       `db:"started_at" json:"started_at"`
	EndedAt   *time.Time      `db:"ended_at" json:"ended_at,omitempty"`
	Attrs     json.RawMessage `db:"attrs" json:"attrs"`
}

type AgentMessage struct {
	ID        string          `db:"id" json:"id"`
	RunID     string          `db:"run_id" json:"run_id"`
	FromNode  string          `db:"from_node" json:"from_node"`
	ToNode    string          `db:"to_node" json:"to_node"`
	Payload   json.RawMessage `db:"payload" json:"payload"`
	CreatedAt time.Time       `db:"created_at" json:"created_at"`
}

// Trace is everything the run detail page shows (DESIGN section 6, /runs/{id}/trace).
type Trace struct {
	Run           *Run           `json:"run"`
	Nodes         []RunNode      `json:"nodes"`
	ModelCalls    []ModelCall    `json:"model_calls"`
	ToolCalls     []ToolCall     `json:"tool_calls"`
	Spans         []Span         `json:"spans"`
	AgentMessages []AgentMessage `json:"agent_messages"`
	Approvals     []Approval     `json:"approvals"`
	// SideEffects are the run's idempotency records (DESIGN 5.4), filled from wave 2.
	SideEffects []SideEffect `json:"side_effects,omitempty"`
	// Branches are the rows of every fork visit of the run (DESIGN 5.26 rule 12), by
	// started_at; empty, not null, for a run without forks.
	Branches []RunBranch `json:"branches"`
}

// Approval kinds (DESIGN 5.3, section 4) and statuses.
const (
	ApprovalKindExec = "exec" // an OpenClaw exec approval, external_id = the gateway's id
	ApprovalKindTool = "tool" // a platform side-effecting tool call gated over MCP
	ApprovalKindNode = "node" // a workflow approval node
	// ApprovalKindQuestion is a question an agent put to the operator (DESIGN 5.24).
	ApprovalKindQuestion = "question"

	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
	ApprovalExpired  = "expired"
)

// SideEffect is one executed side effect keyed by its idempotency key (DESIGN 5.4).
type SideEffect struct {
	IdempotencyKey string          `db:"idempotency_key" json:"idempotency_key"`
	ToolName       string          `db:"tool_name" json:"tool_name"`
	RunID          string          `db:"run_id" json:"run_id"`
	ToolCallID     string          `db:"tool_call_id" json:"tool_call_id"`
	Result         json.RawMessage `db:"result" json:"result"`
	CreatedAt      time.Time       `db:"created_at" json:"created_at"`
}

// RunArtifact is one file a run delivered (DESIGN 5.25 rule 4): Path is relative to the
// run's out/ directory, Visit and NodeID name the visit that last collected it, and
// Generated marks the platform's own diff.patch. The file is stored under ARTIFACT_DIR as
// <run id>/<id>.
type RunArtifact struct {
	ID          string    `db:"id" json:"id"`
	RunID       string    `db:"run_id" json:"run_id"`
	NodeID      string    `db:"node_id" json:"node_id"`
	Visit       int       `db:"visit" json:"visit"`
	Path        string    `db:"path" json:"path"`
	Size        int64     `db:"size" json:"size"`
	SHA256      string    `db:"sha256" json:"sha256"`
	ContentType string    `db:"content_type" json:"content_type"`
	Generated   bool      `db:"generated" json:"generated"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

// USDMicros is money stored as micro-USD and shown as USD in JSON (DESIGN 5.19 units):
// 1500000 marshals as 1.5 and 1.5 unmarshals as 1500000, rounded to the micro.
type USDMicros int64

// USD is the amount as a float, for display and for the API.
func (m USDMicros) USD() float64 { return float64(m) / 1e6 }

// MarshalJSON writes the amount as USD.
func (m USDMicros) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.USD())
}

// UnmarshalJSON reads USD (a JSON number) into micro-USD, rounding to the nearest micro.
func (m *USDMicros) UnmarshalJSON(b []byte) error {
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*m = USDMicros(math.Round(f * 1e6))
	return nil
}

// Merge request states and pipeline statuses (DESIGN 5.20 units).
const (
	MergeRequestOpen   = "open"
	MergeRequestMerged = "merged"
	MergeRequestClosed = "closed"

	CIStatusNone    = "none"
	CIStatusPending = "pending"
	CIStatusPassed  = "passed"
	CIStatusFailed  = "failed"
)

// MergeRequest is a merge request (GitLab) or pull request (GitHub) the platform opened
// for a run, one per run, refreshed from the provider (DESIGN 5.20 rule 1).
// UnresolvedThreads is nil when the provider could not tell.
type MergeRequest struct {
	ID                string     `db:"id" json:"id"`
	ProjectID         string     `db:"project_id" json:"project_id"`
	RunID             string     `db:"run_id" json:"run_id"`
	Provider          string     `db:"provider" json:"provider"`
	Number            int64      `db:"number" json:"number"`
	URL               string     `db:"url" json:"url"`
	SourceBranch      string     `db:"source_branch" json:"source_branch"`
	TargetBranch      string     `db:"target_branch" json:"target_branch"`
	Title             string     `db:"title" json:"title"`
	State             string     `db:"state" json:"state"`
	CIStatus          string     `db:"ci_status" json:"ci_status"`
	HeadSHA           *string    `db:"head_sha" json:"head_sha,omitempty"`
	UnresolvedThreads *int       `db:"unresolved_threads" json:"unresolved_threads"`
	LastSyncError     *string    `db:"last_sync_error" json:"last_sync_error,omitempty"`
	SyncedAt          *time.Time `db:"synced_at" json:"synced_at,omitempty"`
	ReportedState     *string    `db:"reported_state" json:"reported_state,omitempty"`
	MergedBy          *string    `db:"merged_by" json:"merged_by,omitempty"`
	CreatedAt         time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt         time.Time  `db:"updated_at" json:"updated_at"`
	ClosedAt          *time.Time `db:"closed_at" json:"closed_at,omitempty"`
	// Draft is true while the request is a draft (DESIGN 5.29 rule 4).
	Draft bool `db:"draft" json:"draft"`
}

// Label is how the provider names this request in the texts the platform writes (every
// refusal of DESIGN 5.22 and 5.23): "merge request !12" on GitLab, "pull request #12"
// on GitHub.
func (m *MergeRequest) Label() string {
	if m.Provider == "github" {
		return fmt.Sprintf("pull request #%d", m.Number)
	}
	return fmt.Sprintf("merge request !%d", m.Number)
}
