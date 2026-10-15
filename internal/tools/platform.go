package tools

import "sort"

// Source says who executes a tool: platform tools are served by the api over MCP,
// openclaw tools are the runtime's built-ins.
type Source string

const (
	SourcePlatform Source = "platform"
	SourceOpenClaw Source = "openclaw"
)

// PlatformDeps are the engine entry points the platform tools call.
type PlatformDeps struct {
	// AcceptFinish is the engine's AcceptFinish with its model-facing failures
	// translated to *ModelError (see NewFinish).
	AcceptFinish AcceptFinishFunc
	// RejectFinish counts a finish call the input schema refused; nil skips counting.
	RejectFinish RejectFinishFunc
}

// NewPlatformRegistry returns the platform tools served over MCP (DESIGN 5.7). This
// wave has only finish; approval gating arrives with the side-effecting tools.
func NewPlatformRegistry(d PlatformDeps) *Registry {
	r := NewRegistry()
	r.Register(NewFinish(d.AcceptFinish, d.RejectFinish))
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
