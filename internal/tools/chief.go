package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/obs"
	"yuno/internal/project"
	"yuno/internal/store"
)

// The Chief of Staff's tools (DESIGN 5.15 rules 2 and 4 to 8): the roster, delegation
// as platform runs, and a run's status. They are chat-capable: an agent calls them with
// its chat token while it talks to a person, or with a run token inside a run.
const (
	ListAgentsName    = "list_agents"
	ListWorkflowsName = "list_workflows"
	ListProjectsName  = "list_projects"
	DelegateTaskName  = "delegate_task"
	RunStatusName     = "run_status"
)

// ChatToolNames are the tools callable with a chat token (DESIGN 5.15 rule 2, 5.17 rule
// 8, 5.22 rule 6, 5.24 rule 9), sorted. The registry marks the same tools Chat; a test
// keeps the two in step.
var ChatToolNames = []string{AnswerQuestionName, DelegateTaskName, ListAgentsName, ListProjectsName, ListWorkflowsName, MemoryRecallName,
	MemorySaveName, ReadMergeRequestName, RunStatusName}

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
	// runStatusArtifactsMax is how many delivered files run_status names (DESIGN 5.25
	// rule 5); artifact_count says how many there are.
	runStatusArtifactsMax = 20
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
    "input": {"type": "object", "description": "With workflow_name: the workflow's input, matching the input_schema list_workflows shows for it."},
    "project": {"type": "string", "description": "The name of the project (a repository) the run works on, as list_projects shows it. Required for a workflow that works on a project repository, not accepted for one that does not; optional with agent_name."},
    "merge_request": {"type": "string", "description": "An existing merge request or pull request of the project to change, fix or continue instead of opening a new one: its number with or without ! or # (12, !12, #12) or its link. The run works on that request's branch. Needs project, the one the request belongs to."}
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
	// ProjectID is the project the run works on (DESIGN 5.17 rule 8); "" for none.
	ProjectID string
	// TriggerRef is "conversation:<id>" for a call made in a conversation, "" otherwise
	// (DESIGN 5.15 rule 6).
	TriggerRef string
	// MergeRequestID is the record of the open merge request the run updates (DESIGN 5.22
	// rule 8): the store's id, resolved from the number or link the person gave; "" for a
	// run that works from the default branch.
	MergeRequestID string
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
	Project      *string         `json:"project"`
	MergeRequest *string         `json:"merge_request"`
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
			"matches its input_schema. To change, fix or continue an existing merge request instead of opening a new " +
			"one, also give merge_request with the project it belongs to. Returns the run id, its status and a " +
			"link; when the run ends, its outcome arrives in your conversation as a message that starts with " +
			"[platform].",
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
				if req.ProjectID, err = projectForDelegation(ctx, d.Pool, in.Project, nil); err != nil {
					return Result{}, err
				}
			} else {
				wf, err := workflowByName(ctx, d.Pool, *in.WorkflowName)
				if err != nil {
					return Result{}, err
				}
				req.WorkflowID = wf.ID
				if req.ProjectID, err = projectForDelegation(ctx, d.Pool, in.Project, wf); err != nil {
					return Result{}, err
				}
			}
			if in.MergeRequest != nil && strings.TrimSpace(*in.MergeRequest) != "" {
				var err error
				if req.MergeRequestID, err = mergeRequestForDelegation(ctx, d.Pool, req.ProjectID, *in.MergeRequest); err != nil {
					return Result{}, err
				}
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

// projectForDelegation resolves delegate_task's project (DESIGN 5.17 rule 8): a
// workflow whose definition opts in needs one, one that does not refuses one, an agent
// takes one optionally; the name matches like agents and workflows. The model's errors
// list the live projects so it can choose.
func projectForDelegation(ctx context.Context, pool *pgxpool.Pool, name *string, wf *store.Workflow) (string, error) {
	given := name != nil && strings.TrimSpace(*name) != ""
	needs, wfName := false, ""
	if wf != nil {
		wfName = wf.Name
		var def struct {
			Project bool `json:"project"`
		}
		_ = json.Unmarshal(wf.Definition, &def)
		needs = def.Project
		if given && !needs {
			return "", &ModelError{Message: "workflow " + wf.Name + " does not use a project"}
		}
	}
	if !given && !needs {
		return "", nil
	}
	projects, err := store.ListProjects(ctx, pool)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(projects))
	for _, p := range projects {
		names = append(names, p.Name)
	}
	if !given {
		if len(names) == 0 {
			return "", &ModelError{Message: "workflow " + wfName + " needs a project and none exists yet; the operator creates one on the Projects page"}
		}
		return "", &ModelError{Message: "workflow " + wfName + " needs a project; choose one of: " + strings.Join(names, ", ")}
	}
	p, err := matchProject(projects, *name)
	if err != nil {
		return "", err
	}
	return p.ID, nil
}

// matchProject finds the project called name among the live projects (trimmed, ignoring
// case); the model's error when there is none lists the live ones (DESIGN 5.17 rule 8).
func matchProject(projects []store.Project, name string) (*store.Project, error) {
	want := strings.TrimSpace(name)
	for i := range projects {
		if strings.EqualFold(strings.TrimSpace(projects[i].Name), want) {
			return &projects[i], nil
		}
	}
	msg := "no project named " + want
	if len(projects) > 0 {
		names := make([]string, 0, len(projects))
		for _, p := range projects {
			names = append(names, p.Name)
		}
		msg += "; choose one of: " + strings.Join(names, ", ")
	} else {
		msg += "; none exists yet"
	}
	return nil, &ModelError{Message: msg}
}

// projectByName is matchProject over the live projects of the database: the project
// delegate_task and read_merge_request name (DESIGN 5.17 rule 8, 5.22 rule 6).
func projectByName(ctx context.Context, pool *pgxpool.Pool, name string) (*store.Project, error) {
	projects, err := store.ListProjects(ctx, pool)
	if err != nil {
		return nil, err
	}
	return matchProject(projects, name)
}

// mergeRequestForDelegation resolves delegate_task's merge_request (DESIGN 5.22 rule 8):
// a number or a link, against the records of the named project by number. It answers the
// record's id. The model's errors: no project to resolve against, a form that is neither
// number nor link, no record with that number (the provider's own noun and sigil), and a
// record that is merged or closed (D-044 refuses at queue time; the engine's refusals of
// rule 1 follow at the trigger).
func mergeRequestForDelegation(ctx context.Context, pool *pgxpool.Pool, projectID, raw string) (string, error) {
	if projectID == "" {
		return "", &ModelError{Message: "merge_request needs a project"}
	}
	number, _, ok := parseRequestNumber(raw)
	if !ok {
		return "", &ModelError{Message: "merge_request must be a merge request number or URL"}
	}
	rec, err := store.MergeRequestByNumber(ctx, pool, projectID, number)
	if err != nil {
		return "", err
	}
	if rec == nil {
		p, err := store.GetProject(ctx, pool, projectID)
		if err != nil {
			return "", err
		}
		return "", &ModelError{Message: "no " + requestLabel(p.Provider, number) + " on project " + p.Name}
	}
	if rec.State != store.MergeRequestOpen {
		return "", &ModelError{Message: rec.Label() + " is " + rec.State}
	}
	return rec.ID, nil
}

// NewListProjects builds list_projects (DESIGN 5.17 rule 8): every live project with
// its last run, so the Chief can match the person's words to a name. A project with a
// budget cap also shows its spend against it (DESIGN 5.19 rule 8), so the Chief can say
// why a delegation was refused.
func NewListProjects(pool *pgxpool.Pool) *Tool {
	return &Tool{
		Name: ListProjectsName,
		Description: "List the projects (git repositories) the platform can work on: name, repository, provider, " +
			"default branch, whether the sandbox has network access, and the last run on each; a project with a " +
			"budget cap also shows the cap and what it has spent today and this month, since new runs on it are " +
			"refused once a cap is reached; intake says whether labelled issues become runs on it and which label. " +
			"Call it to find the project a person names, then pass its name as project to delegate_task; a " +
			"workflow that works on a project repository needs one.",
		InputSchema: listAgentsSchema,
		Risk:        RiskReadOnly,
		Hits:        HitsPostgres,
		Timeout:     5 * time.Second,
		Chat:        true,
		Exec: func(ctx context.Context, call Call) (Result, error) {
			if pool == nil {
				return Result{}, errors.New("list_projects: no database")
			}
			projects, err := store.ListProjects(ctx, pool)
			if err != nil {
				return Result{}, err
			}
			latest, err := store.LatestRunsByProject(ctx, pool)
			if err != nil {
				return Result{}, err
			}
			if slices.ContainsFunc(projects, hasBudgetCap) {
				if err := store.FillProjectSpend(ctx, pool, projects, time.Now().UTC()); err != nil {
					return Result{}, err
				}
			}
			type lastRun struct {
				RunID      string  `json:"run_id"`
				Status     string  `json:"status"`
				Toolchain  *string `json:"toolchain,omitempty"`
				FinishedAt *string `json:"finished_at,omitempty"`
			}
			// DESIGN 5.23 rule 1: whether labelled issues become runs on the project.
			type intakeView struct {
				Enabled bool   `json:"enabled"`
				Label   string `json:"label"`
			}
			type projectView struct {
				Name           string      `json:"name"`
				Repository     string      `json:"repository"`
				Provider       string      `json:"provider"`
				DefaultBranch  string      `json:"default_branch"`
				SandboxNetwork bool        `json:"sandbox_network"`
				LastRun        *lastRun    `json:"last_run"`
				Budget         *budgetView `json:"budget,omitempty"`
				Intake         intakeView  `json:"intake"`
			}
			out := []projectView{}
			for _, p := range projects {
				v := projectView{Name: p.Name, Repository: p.RepoURL, Provider: p.Provider, DefaultBranch: p.DefaultBranch, SandboxNetwork: p.SandboxNetwork,
					Budget: budgetOf(&p), Intake: intakeView{Enabled: p.IntakeEnabled, Label: p.IntakeLabel}}
				if r, ok := latest[p.ID]; ok {
					v.LastRun = &lastRun{RunID: r.ID, Status: r.Status, Toolchain: r.Toolchain}
					if r.FinishedAt != nil {
						at := r.FinishedAt.UTC().Format(time.RFC3339)
						v.LastRun.FinishedAt = &at
					}
				}
				out = append(out, v)
			}
			return JSONResult(map[string]any{"projects": out}), nil
		},
	}
}

// budgetView is list_projects' budget of a project with a cap (DESIGN 5.19 rule 8):
// the cap and what the project has spent in the current UTC window, in USD, for each
// window that has a cap.
type budgetView struct {
	Day   *budgetWindowView `json:"day,omitempty"`
	Month *budgetWindowView `json:"month,omitempty"`
}

type budgetWindowView struct {
	MaxUSD   float64 `json:"max_usd"`
	SpentUSD float64 `json:"spent_usd"`
}

func hasBudgetCap(p store.Project) bool {
	return p.BudgetDayUSDMicros != nil || p.BudgetMonthUSDMicros != nil
}

// budgetOf is the project's budget view, nil without a cap. Spend must be filled in.
func budgetOf(p *store.Project) *budgetView {
	if !hasBudgetCap(*p) || p.Spend == nil {
		return nil
	}
	b := &budgetView{}
	if p.BudgetDayUSDMicros != nil {
		b.Day = &budgetWindowView{MaxUSD: p.BudgetDayUSDMicros.USD(), SpentUSD: p.Spend.DayUSDMicros.USD()}
	}
	if p.BudgetMonthUSDMicros != nil {
		b.Month = &budgetWindowView{MaxUSD: p.BudgetMonthUSDMicros.USD(), SpentUSD: p.Spend.MonthUSDMicros.USD()}
	}
	return b
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
			"has ended, the failure reason if it failed, the approval it waits for, if any, the question it asks " +
			"the operator while it waits for an answer, the files it delivered, the merge request it opened or " +
			"updates, and the issue it came from.",
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
			// DESIGN 5.17 rule 8: the project and the request the run opened.
			if run.Project != nil {
				out["project"] = run.Project.Name
			}
			// DESIGN 5.20 rule 6: the run's merge request record, the one it names (role
			// "updates", 5.22 rule 8) else the one it opened (role "opened"); 5.21 rule 7:
			// its retries.
			if mr, role, err := runRequest(ctx, pool, run); err != nil {
				return Result{}, err
			} else if mr != nil {
				out["merge_request_url"] = mr.URL
				out["merge_request"] = map[string]any{"number": mr.Number, "url": mr.URL, "state": mr.State,
					"ci_status": mr.CIStatus, "unresolved_threads": mr.UnresolvedThreads, "role": role}
			}
			// DESIGN 5.23 rule 8: the issue an intake run came from.
			if issue := issueOf(run); issue != nil {
				out["issue"] = issue
			}
			out["retries"] = run.Retries
			if run.Status == store.RunWaitingApproval {
				pending, err := store.ListPendingApprovals(ctx, pool, run.ID)
				if err != nil {
					return Result{}, err
				}
				if len(pending) > 0 {
					out["pending_approval"] = map[string]string{"kind": pending[0].Kind, "message": pending[0].Message}
				}
				// DESIGN 5.24 rule 11: the question the run waits on, next to the entry above.
				for i := range pending {
					if pending[i].Kind == store.ApprovalKindQuestion {
						out["question"] = questionView(&pending[i])
						break
					}
				}
			}
			// DESIGN 5.25 rule 5: the files the run delivered, the first 20 by path.
			artifacts, err := store.ListRunArtifacts(ctx, pool, run.ID)
			if err != nil {
				return Result{}, err
			}
			if len(artifacts) > 0 {
				shown := make([]map[string]any, 0, min(len(artifacts), runStatusArtifactsMax))
				for _, a := range artifacts[:min(len(artifacts), runStatusArtifactsMax)] {
					shown = append(shown, map[string]any{"path": a.Path, "size_bytes": a.Size})
				}
				out["artifacts"] = shown
				out["artifact_count"] = len(artifacts)
			}
			return JSONResult(out), nil
		},
	}
}

// questionView is run_status's question (DESIGN 5.24 rule 11): its id, text, options when
// it has any, and when it was asked.
func questionView(a *store.Approval) map[string]any {
	q := map[string]any{"id": a.ID, "text": a.Message, "asked_at": a.RequestedAt.UTC().Format(time.RFC3339)}
	var options []string
	if len(a.Options) > 0 && json.Unmarshal(a.Options, &options) == nil && len(options) > 0 {
		q["options"] = options
	}
	return q
}

// runRequest is the merge request record run_status shows for a run (DESIGN 5.22 rule 8):
// the record the run names (role "updates"), else the one it opened (role "opened"); nil
// when it has neither.
func runRequest(ctx context.Context, pool *pgxpool.Pool, run *store.Run) (mr *store.MergeRequest, role string, err error) {
	if run.MergeRequestID != nil && *run.MergeRequestID != "" {
		mr, err = store.GetMergeRequest(ctx, pool, *run.MergeRequestID)
		if err == nil {
			return mr, "updates", nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, "", err
		}
	}
	mr, err = store.MergeRequestByRun(ctx, pool, run.ID)
	return mr, "opened", err
}

// issueOf is run_status's issue of an intake run (DESIGN 5.23 rule 8): its number, from
// the run's trigger_ref "<project id>#<n>", and its link from the project's snapshot; nil
// for any other run. A snapshot the run lacks leaves the number alone.
func issueOf(run *store.Run) map[string]any {
	if run.Trigger != store.TriggerIssue || run.TriggerRef == nil {
		return nil
	}
	i := strings.LastIndexByte(*run.TriggerRef, '#')
	if i < 0 {
		return nil
	}
	n, err := strconv.ParseInt((*run.TriggerRef)[i+1:], 10, 64)
	if err != nil || n <= 0 {
		return nil
	}
	issue := map[string]any{"number": n}
	if run.Project != nil {
		issue["url"] = project.IssueURL(*run.Project, n)
	}
	return issue
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
	on := ""
	if in.Project != nil && strings.TrimSpace(*in.Project) != "" {
		on = " on project " + strings.TrimSpace(*in.Project)
	}
	var sentence string
	switch {
	case in.AgentName != nil && in.Task != nil:
		sentence = fmt.Sprintf("Delegate to %s%s: %s", strings.TrimSpace(*in.AgentName), on, obs.Truncate(*in.Task, 300))
	case in.WorkflowName != nil:
		sentence = "Run the workflow " + strings.TrimSpace(*in.WorkflowName) + on
	default:
		return ""
	}
	// DESIGN 5.22 rule 8: the request the run updates. The sentence is built from the
	// input alone, so the sigil is the one the person wrote (or the link's): "!" for a
	// merge request, "#" for a pull request, "!" for a bare number.
	if in.MergeRequest != nil {
		if n, sigil, ok := parseRequestNumber(*in.MergeRequest); ok {
			sentence += ", updating " + inputRequestLabel(n, sigil)
		}
	}
	return sentence
}

// inputRequestLabel names request n from the sigil its input carried: "pull request #n"
// for "#", "merge request !n" otherwise.
func inputRequestLabel(n int64, sigil string) string {
	if sigil == "#" {
		return requestLabel("github", n)
	}
	return requestLabel("gitlab", n)
}
