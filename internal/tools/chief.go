package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/obs"
	"yuno/internal/store"
)

// The Chief of Staff's tools (DESIGN 5.15 rules 2 and 4 to 8): the roster, delegation
// as platform runs, and a run's status. They are chat-capable: an agent calls them with
// its chat token while it talks to a person, or with a run token inside a run.
const (
	ListAgentsName    = "list_agents"
	ListWorkflowsName = "list_workflows"
	DelegateTaskName  = "delegate_task"
	RunStatusName     = "run_status"
)

// ChatToolNames are the tools callable with a chat token (DESIGN 5.15 rule 2), sorted.
// The registry marks the same tools Chat; a test keeps the two in step.
var ChatToolNames = []string{DelegateTaskName, ListAgentsName, ListWorkflowsName, MemoryRecallName, MemorySaveName, RunStatusName}

// IsChatTool reports whether name (with or without the platform prefix) is
// chat-capable.
func IsChatTool(name string) bool {
	return slices.Contains(ChatToolNames, PlainName(name))
}

// delegate_task's rate limit (DESIGN 5.15 rule 6) and run_status's state cap (rule 8).
const (
	DelegationMax         = 10
	DelegationWindow      = 10 * time.Minute
	delegationLimitText   = "delegation limit reached; try later"
	runStatusStateMaxSize = 4 << 10
)

// tokenProperties are the identity fields of a chat-capable tool: exactly one of them is
// required, which the MCP gate enforces (a schema-level oneOf is not accepted at the top
// of a tool's input schema by every model provider).
const tokenProperties = `
    "run_token": {"type": "string", "description": "The run token from the task message, when you are working in a run."},
    "chat_token": {"type": "string", "description": "Your chat token from AGENTS.md, when you are chatting with a person and have no run token."},`

var listAgentsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {` + strings.TrimSuffix(tokenProperties, ",") + `
  },
  "additionalProperties": false
}`)

var listWorkflowsSchema = listAgentsSchema

var delegateTaskSchema = json.RawMessage(`{
  "type": "object",
  "properties": {` + tokenProperties + `
    "agent_name": {"type": "string", "description": "The name of the agent to hand the task to, as list_agents shows it. Give agent_name with task, or workflow_name with input, never both."},
    "task": {"type": "string", "description": "With agent_name: the complete, self-contained task. The agent sees nothing of your conversation, so include every detail it needs."},
    "workflow_name": {"type": "string", "description": "The name of the workflow to run, as list_workflows shows it."},
    "input": {"type": "object", "description": "With workflow_name: the workflow's input, matching the input_schema list_workflows shows for it."}
  },
  "additionalProperties": false
}`)

var runStatusSchema = json.RawMessage(`{
  "type": "object",
  "properties": {` + tokenProperties + `
    "run_id": {"type": "string", "minLength": 1, "description": "The run id delegate_task returned, or the one in a [platform] message."}
  },
  "required": ["run_id"],
  "additionalProperties": false
}`)

// Delegation is one delegate_task request as the engine's Trigger needs it: an agent
// with a task (a quick run) or a workflow with its input.
type Delegation struct {
	CallerAgentID string
	AgentID       string
	Task          string
	WorkflowID    string
	Input         json.RawMessage
	// TriggerRef is "conversation:<id>" for a call made in a conversation, "" otherwise
	// (DESIGN 5.15 rule 6).
	TriggerRef string
}

// Delegator queues a delegated run with Trigger = channel and default limits: the
// engine's Trigger behind an interface, since this package cannot import the engine.
// Failures the model should see (validation, a guardrail rate limit, limits) are
// *ModelError carrying the engine's message; any other error is internal.
type Delegator interface {
	Delegate(ctx context.Context, d Delegation) (*store.Run, error)
}

// ChiefDeps are what the Chief of Staff's tools need.
type ChiefDeps struct {
	Pool      *pgxpool.Pool
	Delegator Delegator
	// PublicBaseURL prefixes the run links the tools return; relative links without it.
	PublicBaseURL string
	// Limiter is delegate_task's rate limit; nil builds one on the real clock.
	Limiter *DelegationLimiter
}

// RunURL is where a person opens a run in the UI: PUBLIC_BASE_URL/runs/<id>, or the
// relative path when PUBLIC_BASE_URL is unset (DESIGN 5.15 rule 6).
func RunURL(publicBase, runID string) string {
	return strings.TrimRight(strings.TrimSpace(publicBase), "/") + "/runs/" + runID
}

// DelegationLimiter is delegate_task's rate limit (DESIGN 5.15 rule 6): at most Max
// delegations per agent in any sliding Window. It lives in memory, so an api restart
// resets it; the delegated runs' own limits and the agents' hourly run guardrails
// still apply.
type DelegationLimiter struct {
	Max    int
	Window time.Duration
	// Now is the clock; tests pass a fake one.
	Now func() time.Time

	mu   sync.Mutex
	hits map[string][]time.Time
}

// NewDelegationLimiter returns the 10-per-10-minutes limiter on clock now (time.Now
// when nil).
func NewDelegationLimiter(now func() time.Time) *DelegationLimiter {
	if now == nil {
		now = time.Now
	}
	return &DelegationLimiter{Max: DelegationMax, Window: DelegationWindow, Now: now}
}

// Reserve takes one of the agent's delegations in the current window. It answers false
// when the window is full; otherwise release gives the delegation back (the run could
// not be queued).
func (l *DelegationLimiter) Reserve(agentID string) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	now := l.Now()
	cutoff := now.Add(-l.Window)
	kept := l.hits[agentID][:0]
	for _, t := range l.hits[agentID] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	l.hits[agentID] = kept
	if len(kept) >= l.Max {
		return func() {}, false
	}
	l.hits[agentID] = append(kept, now)
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			list := l.hits[agentID]
			if i := slices.IndexFunc(list, func(t time.Time) bool { return t.Equal(now) }); i >= 0 {
				l.hits[agentID] = slices.Delete(list, i, i+1)
			}
		})
	}, true
}

// NewListAgents builds list_agents (DESIGN 5.15 rule 4): every agent but the caller,
// with its role, model, tools and whether a channel is bound to it.
func NewListAgents(pool *pgxpool.Pool) *Tool {
	return &Tool{
		Name: ListAgentsName,
		Description: "List the platform's other agents: name, role, model, the tools each may use, and whether it " +
			"answers a chat channel. Call it before delegating, to pick the agent whose role fits the task.",
		InputSchema: listAgentsSchema,
		Risk:        RiskReadOnly,
		Hits:        HitsPostgres,
		Timeout:     5 * time.Second,
		Chat:        true,
		Exec: func(ctx context.Context, call Call) (Result, error) {
			if pool == nil {
				return Result{}, errors.New("list_agents: no database")
			}
			agents, err := store.ListAgents(ctx, pool)
			if err != nil {
				return Result{}, err
			}
			bound, err := store.ListAgentChannels(ctx, pool)
			if err != nil {
				return Result{}, err
			}
			type agentView struct {
				Name           string   `json:"name"`
				Role           string   `json:"role"`
				Model          string   `json:"model"`
				Tools          []string `json:"tools"`
				BoundToChannel bool     `json:"bound_to_channel"`
			}
			out := []agentView{}
			for _, a := range agents {
				if a.ID == call.AgentID {
					continue
				}
				out = append(out, agentView{Name: a.Name, Role: a.Role, Model: a.Model, Tools: a.ToolAllowlist,
					BoundToChannel: len(bound[a.ID]) > 0})
			}
			return JSONResult(map[string]any{"agents": out}), nil
		},
	}
}

// NewListWorkflows builds list_workflows (DESIGN 5.15 rule 4): every workflow an
// operator can run, not the templates or the hidden quick-run wrappers, with the agents
// its nodes use and its input schema.
func NewListWorkflows(pool *pgxpool.Pool) *Tool {
	return &Tool{
		Name: ListWorkflowsName,
		Description: "List the workflows you can run with delegate_task: name, description, the agents its steps " +
			"use, and the input_schema its input must match.",
		InputSchema: listWorkflowsSchema,
		Risk:        RiskReadOnly,
		Hits:        HitsPostgres,
		Timeout:     5 * time.Second,
		Chat:        true,
		Exec: func(ctx context.Context, call Call) (Result, error) {
			if pool == nil {
				return Result{}, errors.New("list_workflows: no database")
			}
			workflows, err := store.ListWorkflows(ctx, pool)
			if err != nil {
				return Result{}, err
			}
			// Deleted agents keep their names for the workflows that still name them.
			agents, err := store.ListAgentsWithDeleted(ctx, pool)
			if err != nil {
				return Result{}, err
			}
			names := make(map[string]string, len(agents))
			for _, a := range agents {
				names[a.ID] = a.Name
			}
			type workflowView struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Agents      []string        `json:"agents"`
				InputSchema json.RawMessage `json:"input_schema"`
			}
			out := []workflowView{}
			for _, wf := range workflows {
				if wf.TemplateKey != nil {
					continue
				}
				var def struct {
					InputSchema json.RawMessage `json:"input_schema"`
					Nodes       []struct {
						Kind    string `json:"kind"`
						AgentID string `json:"agent_id"`
					} `json:"nodes"`
				}
				_ = json.Unmarshal(wf.Definition, &def)
				v := workflowView{Name: wf.Name, Description: wf.Description, Agents: []string{}, InputSchema: def.InputSchema}
				if len(bytes.TrimSpace(v.InputSchema)) == 0 {
					v.InputSchema = json.RawMessage(`null`)
				}
				for _, n := range def.Nodes {
					if n.Kind == "agent" && names[n.AgentID] != "" && !slices.Contains(v.Agents, names[n.AgentID]) {
						v.Agents = append(v.Agents, names[n.AgentID])
					}
				}
				out = append(out, v)
			}
			return JSONResult(map[string]any{"workflows": out}), nil
		},
	}
}

type delegateInput struct {
	AgentName    *string         `json:"agent_name"`
	Task         *string         `json:"task"`
	WorkflowName *string         `json:"workflow_name"`
	Input        json.RawMessage `json:"input"`
}

// errDelegateShape is the model's message for any input that is not exactly one of the
// two delegation forms (DESIGN 5.15 rule 5).
const errDelegateShape = "invalid input: give exactly one of agent_name (with a non-empty task) or workflow_name (with an input object)"

// NewDelegateTask builds delegate_task (DESIGN 5.15 rules 5 to 7): hand a task to an
// agent, or an input to a workflow, as a platform run triggered by the channel.
func NewDelegateTask(d ChiefDeps) *Tool {
	limiter := d.Limiter
	if limiter == nil {
		limiter = NewDelegationLimiter(nil)
	}
	return &Tool{
		Name: DelegateTaskName,
		Description: "Hand work to another agent or a workflow as a platform run, which the operator can follow in " +
			"the UI. Give agent_name with a complete, self-contained task, or workflow_name with an input that " +
			"matches its input_schema. Returns the run id, its status and a link; when the run ends, its outcome " +
			"arrives in your conversation as a message that starts with [platform].",
		InputSchema: delegateTaskSchema,
		Risk:        RiskComputational,
		Hits:        HitsPostgres,
		Timeout:     15 * time.Second,
		Chat:        true,
		Describe:    describeDelegation,
		Exec: func(ctx context.Context, call Call) (Result, error) {
			var in delegateInput
			if err := json.Unmarshal(call.Input, &in); err != nil {
				return Result{}, &ModelError{Message: "invalid input: " + err.Error()}
			}
			req, msg := delegationOf(in)
			if msg != "" {
				return Result{}, &ModelError{Message: msg}
			}
			if d.Pool == nil || d.Delegator == nil {
				return Result{}, errors.New("delegate_task: not configured")
			}
			if in.AgentName != nil {
				a, err := agentByName(ctx, d.Pool, *in.AgentName)
				if err != nil {
					return Result{}, err
				}
				if a.ID == call.AgentID {
					return Result{}, &ModelError{Message: "cannot delegate to yourself"}
				}
				req.AgentID = a.ID
			} else {
				wf, err := workflowByName(ctx, d.Pool, *in.WorkflowName)
				if err != nil {
					return Result{}, err
				}
				req.WorkflowID = wf.ID
			}
			release, ok := limiter.Reserve(call.AgentID)
			if !ok {
				return Result{IsError: true, Blocked: true, Text: delegationLimitText}, nil
			}
			req.CallerAgentID = call.AgentID
			if call.Chat {
				req.TriggerRef = store.ConversationRef(call.ConversationID)
			}
			run, err := d.Delegator.Delegate(ctx, req)
			if err != nil {
				release()
				return Result{}, err
			}
			return JSONResult(map[string]any{"run_id": run.ID, "status": run.Status, "url": RunURL(d.PublicBaseURL, run.ID)}), nil
		},
	}
}

// delegationOf checks the input is exactly one delegation form (DESIGN 5.15 rule 5) and
// returns its task or input; msg is the model's error otherwise.
func delegationOf(in delegateInput) (req Delegation, msg string) {
	hasAgent, hasWorkflow := in.AgentName != nil, in.WorkflowName != nil
	hasInput := len(bytes.TrimSpace(in.Input)) > 0
	switch {
	case hasAgent == hasWorkflow:
		return req, errDelegateShape
	case hasAgent:
		if strings.TrimSpace(*in.AgentName) == "" || in.Task == nil || strings.TrimSpace(*in.Task) == "" || hasInput {
			return req, errDelegateShape
		}
		req.Task = *in.Task
	default:
		if strings.TrimSpace(*in.WorkflowName) == "" || in.Task != nil || !hasInput || bytes.TrimSpace(in.Input)[0] != '{' {
			return req, errDelegateShape
		}
		req.Input = in.Input
	}
	return req, ""
}

// agentByName finds the live agent with exactly this name, ignoring case; the oldest
// wins when several share it. An unknown name is the model's error.
func agentByName(ctx context.Context, pool *pgxpool.Pool, name string) (*store.Agent, error) {
	agents, err := store.ListAgents(ctx, pool) // oldest first
	if err != nil {
		return nil, err
	}
	want := strings.TrimSpace(name)
	for i := range agents {
		if strings.EqualFold(strings.TrimSpace(agents[i].Name), want) {
			return &agents[i], nil
		}
	}
	return nil, &ModelError{Message: "no agent named " + want}
}

// workflowByName finds the operator workflow (not a template or a quick-run wrapper)
// with exactly this name, ignoring case; the oldest wins.
func workflowByName(ctx context.Context, pool *pgxpool.Pool, name string) (*store.Workflow, error) {
	workflows, err := store.ListWorkflows(ctx, pool) // oldest first
	if err != nil {
		return nil, err
	}
	want := strings.TrimSpace(name)
	for i := range workflows {
		if workflows[i].TemplateKey == nil && strings.EqualFold(strings.TrimSpace(workflows[i].Name), want) {
			return &workflows[i], nil
		}
	}
	return nil, &ModelError{Message: "no workflow named " + want}
}

// NewRunStatus builds run_status (DESIGN 5.15 rule 8).
func NewRunStatus(pool *pgxpool.Pool, publicBaseURL string) *Tool {
	return &Tool{
		Name: RunStatusName,
		Description: "Check on a platform run: its status, timing, steps, tool calls, cost, its final state when it " +
			"has ended, the failure reason if it failed, and the approval it waits for, if any.",
		InputSchema: runStatusSchema,
		Risk:        RiskReadOnly,
		Hits:        HitsPostgres,
		Timeout:     5 * time.Second,
		Chat:        true,
		Exec: func(ctx context.Context, call Call) (Result, error) {
			var in struct {
				RunID string `json:"run_id"`
			}
			if err := json.Unmarshal(call.Input, &in); err != nil {
				return Result{}, &ModelError{Message: "invalid input: " + err.Error()}
			}
			if pool == nil {
				return Result{}, errors.New("run_status: no database")
			}
			id := strings.TrimSpace(in.RunID)
			run, err := store.GetRun(ctx, pool, id)
			if errors.Is(err, store.ErrNotFound) {
				return Result{}, &ModelError{Message: "no run " + id}
			}
			if err != nil {
				return Result{}, err
			}
			out := map[string]any{
				"run_id": run.ID, "status": run.Status, "trigger": run.Trigger,
				"steps": run.StepCount, "tool_calls": run.ToolCallCount,
				"cost_usd": float64(run.CostUSDMicros) / 1e6, "url": RunURL(publicBaseURL, run.ID),
			}
			if run.StartedAt != nil {
				out["started_at"] = run.StartedAt.UTC().Format(time.RFC3339)
				end := time.Now()
				if run.FinishedAt != nil {
					end = *run.FinishedAt
					out["finished_at"] = run.FinishedAt.UTC().Format(time.RFC3339)
				}
				out["duration_seconds"] = int64(end.Sub(*run.StartedAt).Round(time.Second) / time.Second)
			}
			if store.IsTerminal(run.Status) {
				out["state"] = capJSON(run.State, runStatusStateMaxSize)
			}
			if run.FailureReason != nil && *run.FailureReason != "" {
				out["failure_reason"] = *run.FailureReason
			}
			if run.Status == store.RunWaitingApproval {
				pending, err := store.ListPendingApprovals(ctx, pool, run.ID)
				if err != nil {
					return Result{}, err
				}
				if len(pending) > 0 {
					out["pending_approval"] = map[string]string{"kind": pending[0].Kind, "message": pending[0].Message}
				}
			}
			return JSONResult(out), nil
		},
	}
}

// capJSON returns raw as JSON when it fits in max bytes, else its text cut to max with
// the truncation marker, as a string (DESIGN 5.15 rule 8: the state is capped at 4 KB).
func capJSON(raw json.RawMessage, max int) any {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return obs.Truncate(string(raw), max)
	}
	if buf.Len() <= max {
		return json.RawMessage(buf.Bytes())
	}
	return obs.Truncate(buf.String(), max)
}

// describeDelegation is the sentence a human reads when an operator gates
// delegate_task on an approval (DESIGN 5.15 rule 7, 5.3 rule 5).
func describeDelegation(input json.RawMessage) string {
	var in delegateInput
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	switch {
	case in.AgentName != nil && in.Task != nil:
		return fmt.Sprintf("Delegate to %s: %s", strings.TrimSpace(*in.AgentName), obs.Truncate(*in.Task, 300))
	case in.WorkflowName != nil:
		return "Run the workflow " + strings.TrimSpace(*in.WorkflowName)
	}
	return ""
}
