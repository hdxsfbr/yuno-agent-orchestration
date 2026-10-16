package tools

import (
	"sort"

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
	// MemorySaved runs after every memory_save; nil skips it.
	MemorySaved MemorySaved
	// MaxOutputBytes is the cap the MCP server applies to every result (0: none).
	MaxOutputBytes int
}

// NewPlatformRegistry returns the platform tools served over MCP (DESIGN 5.7).
func NewPlatformRegistry(d PlatformDeps) *Registry {
	r := NewRegistry()
	r.Register(NewFinish(d.AcceptFinish, d.RejectFinish))
	r.Register(NewDeployRelease(d.Deploy))
	r.Register(NewHTTPFetch(d.Fetch))
	r.Register(NewMemorySave(d.Pool, d.MemorySaved))
	r.Register(NewMemoryRecall(d.Pool, d.MaxOutputBytes))
	if d.GitLab != nil {
		r.Register(NewGitLabCreateMR(*d.GitLab))
	}
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
