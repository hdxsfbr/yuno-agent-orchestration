package tools

import (
	"crypto/rand"
	"net/http"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Source says who executes a tool: platform tools are served by the api over MCP,
// openclaw tools are the runtime's built-ins.
type Source string

const (
	SourcePlatform Source = "platform"
	SourceOpenClaw Source = "openclaw"
)

// PlatformDeps are what the platform tools call: the engine's finish entry points, the
// database, and the external services of the DESIGN 5.7 table.
type PlatformDeps struct {
	// AcceptFinish is the engine's AcceptFinish with its model-facing failures
	// translated to *ModelError (see NewFinish).
	AcceptFinish AcceptFinishFunc
	// RejectFinish counts a finish call the input schema refused; nil skips counting.
	RejectFinish RejectFinishFunc
	// Pool backs the memory tools; nil leaves them unable to run.
	Pool   *pgxpool.Pool
	Deploy DeployConfig
	Fetch  FetchConfig
	// GitLab registers gitlab_create_mr when set (D-010).
	GitLab *GitLabConfig
	// Projects resolves a run's project for open_merge_request (DESIGN 5.17 rule 6) and
	// read_merge_request (5.22 rule 6, which also opens the token of the project a
	// conversation names); nil leaves both answering that no run has a project.
	Projects ProjectResolver
	// ProviderClient reaches the merge request providers (GitLab, GitHub) for
	// open_merge_request and read_merge_request; nil means ProviderClient(nil). It should
	// come from ProviderClient, so that no redirect is followed.
	ProviderClient *http.Client
	// MemorySaved runs after every memory_save; nil skips it.
	MemorySaved MemorySaved
	// MaxOutputBytes is the cap the MCP server applies to every result (0: none).
	MaxOutputBytes int
	// Delegator queues delegate_task's runs (DESIGN 5.15 rule 6); nil leaves the tool
	// unable to run.
	Delegator Delegator
	// PublicBaseURL prefixes the run links delegate_task and run_status return.
	PublicBaseURL string
	// DelegationLimiter is delegate_task's rate limit; nil builds the default one.
	DelegationLimiter *DelegationLimiter
	// Asker is the engine's door for ask_operator and answer_question (DESIGN 5.24); nil
	// leaves both tools unable to run.
	Asker Asker
	// MCPToolWait is how long ask_operator blocks for the operator's answer before it
	// reports the question pending (DESIGN 5.24 rule 4): the api's MCP_TOOL_WAIT.
	MCPToolWait time.Duration
	// Escalate ends a run with the agent's reason and, for a plan ticket, pauses the
	// project's waves (DESIGN 5.29 rule 6); nil leaves escalate unable to run.
	Escalate EscalateFunc
	// AdvanceWave closes the run's wave and releases the next one (DESIGN 5.29 rule 14);
	// late-bound like Escalate, nil leaves advance_wave unable to run.
	AdvanceWave AdvanceWaveFunc
	// DecideApproval is the engine's DecideApproval, late-bound, for decide_approval
	// (D-081); nil leaves the tool unable to run.
	DecideApproval DecideApprovalFunc
	// AddProject creates a project through the API's own code for add_project (D-081);
	// nil leaves the tool unable to run.
	AddProject AddProjectFunc
	// ApprovalCodeKey keys the codes decide_approval requires (D-081); empty gets a
	// random key for the life of the process.
	ApprovalCodeKey []byte
	// RunIDForToken resolves a run token for publish_tickets' approval sentence (5.29
	// rule 2); nil gives the generic sentence.
	RunIDForToken func(token string) (runID string, ok bool)
	// Platform sends platform_call's requests through the api (platform control A3); nil
	// leaves the tool unable to run.
	Platform PlatformDispatcher
}

// NewPlatformRegistry returns the platform tools served over MCP (DESIGN 5.7).
func NewPlatformRegistry(d PlatformDeps) *Registry {
	r := NewRegistry()
	codeKey := d.ApprovalCodeKey
	if len(codeKey) == 0 {
		codeKey = make([]byte, 32)
		_, _ = rand.Read(codeKey)
	}
	// answer_question accepts a question's code as the person's evidence (platform control A7).
	d.ApprovalCodeKey = codeKey
	r.Register(NewFinish(d.AcceptFinish, d.RejectFinish))
	r.Register(NewDeployRelease(d.Deploy))
	r.Register(NewHTTPFetch(d.Fetch))
	r.Register(NewMemorySave(d.Pool, d.MemorySaved))
	r.Register(NewMemoryRecall(d.Pool, d.MaxOutputBytes))
	r.Register(NewListAgents(d.Pool))
	r.Register(NewListWorkflows(d.Pool))
	r.Register(NewListProjects(d.Pool))
	r.Register(NewDelegateTask(ChiefDeps{Pool: d.Pool, Delegator: d.Delegator, PublicBaseURL: d.PublicBaseURL, Limiter: d.DelegationLimiter}))
	r.Register(NewRunStatus(d.Pool, d.PublicBaseURL, codeKey))
	if d.GitLab != nil {
		r.Register(NewGitLabCreateMR(*d.GitLab))
	}
	r.Register(NewOpenMergeRequest(MergeRequestConfig{Projects: d.Projects, Client: d.ProviderClient}))
	r.Register(NewReadMergeRequest(ReadMergeRequestConfig{Projects: d.Projects, Pool: d.Pool, Client: d.ProviderClient,
		MaxOutputBytes: d.MaxOutputBytes}))
	r.Register(NewAskOperator(d))
	r.Register(NewAnswerQuestion(d))
	r.Register(NewListApprovals(d.Pool, codeKey))
	r.Register(NewDecideApproval(d.Pool, d.DecideApproval, codeKey))
	r.Register(NewAddProject(d.Pool, d.AddProject))
	r.Register(NewPlatformOperations())
	r.Register(NewPlatformCall(PlatformCallDeps{Pool: d.Pool, Dispatcher: d.Platform}))
	pt := PublishTicketsConfig{Projects: d.Projects, Client: d.ProviderClient, RunIDForToken: d.RunIDForToken}
	if d.Pool != nil {
		pt.DB = d.Pool
	}
	r.Register(NewPublishTickets(pt))
	r.Register(NewEscalate(EscalateDeps{Escalate: d.Escalate}))
	r.Register(NewAdvanceWave(AdvanceWaveDeps{AdvanceWave: d.AdvanceWave, RunIDForToken: d.RunIDForToken}))
	return r
}

// openClawBuiltins are the runtime tools the templates use (DESIGN 5.7, profile
// "coding"). OpenClaw executes them inside the agent's sandbox, so they have no
// executor, schema or timeout here.
var openClawBuiltins = []Tool{
	{Name: "apply_patch", Description: "Apply a multi-file patch to the agent's workspace.", Risk: RiskComputational, Hits: HitsOpenClaw},
	{Name: "edit", Description: "Replace text in a file in the agent's workspace.", Risk: RiskComputational, Hits: HitsOpenClaw},
	{Name: "exec", Description: "Run a shell command in the agent's sandbox.", Risk: RiskComputational, Hits: HitsOpenClaw},
	{Name: "ls", Description: "List the files in a workspace directory.", Risk: RiskReadOnly, Hits: HitsOpenClaw},
	{Name: "process", Description: "Check on, send input to, or stop a command started in the background.", Risk: RiskComputational, Hits: HitsOpenClaw},
	{Name: "read", Description: "Read a file from the agent's workspace.", Risk: RiskReadOnly, Hits: HitsOpenClaw},
	{Name: "write", Description: "Create or overwrite a file in the agent's workspace.", Risk: RiskComputational, Hits: HitsOpenClaw},
}

// OpenClawBuiltins returns a copy of the built-in tool entries, sorted by name.
func OpenClawBuiltins() []Tool {
	out := make([]Tool, len(openClawBuiltins))
	copy(out, openClawBuiltins)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// IsOpenClawBuiltin reports whether name is one of the built-ins agents may allowlist.
func IsOpenClawBuiltin(name string) bool {
	for _, t := range openClawBuiltins {
		if t.Name == name {
			return true
		}
	}
	return false
}
