package evals

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"yuno/internal/store"
)

// interactionModes are the values of an agent's interaction rule (DESIGN 5.3 rule 6).
var interactionModes = []string{"auto", "require_approval", "blocked"}

// toolStatusNames are the statuses a tool call row can end in.
var toolStatusNames = []string{
	store.ToolExecuted, store.ToolAwaitingApproval, store.ToolApproved,
	store.ToolRejected, store.ToolBlocked, store.ToolExpired, store.ToolFailed,
}

// approvalStatusNames are the statuses an approval row can have.
var approvalStatusNames = []string{
	store.ApprovalPending, store.ApprovalApproved, store.ApprovalRejected, store.ApprovalExpired,
}

// agentKeyPattern is the label of an agent in a workflow case; it ends up in the name of
// the created agent.
var agentKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,23}$`)

// maxAgentNameLen is the api's limit for an agent name.
const maxAgentNameLen = 80

// runnerSuffixLen is the length of "-<6 hex>" the runner appends to created names.
const runnerSuffixLen = 7

// Validate checks the case against the format rules. dirVersion is the name of the
// version directory the file lives in, which must equal the case's version.
func (c Case) Validate(dirVersion string) error {
	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }

	if !caseIDPattern.MatchString(c.ID) {
		add("id %q must match %s", c.ID, caseIDPattern)
	}
	switch {
	case c.Version == "":
		add("version is required")
	case c.Version != dirVersion:
		add("version %q does not match its directory %q", c.Version, dirVersion)
	}
	if c.Split != SplitPublic && c.Split != SplitPrivate {
		add("split %q must be %q or %q", c.Split, SplitPublic, SplitPrivate)
	}

	if c.IsWorkflow() {
		c.validateWorkflowMode(add)
	} else {
		c.validateQuickMode(add)
	}
	c.validateScript(add)
	c.validateActions(add)
	c.validateProject(add)
	c.validateExpect(add)
	c.validateRubric(add)

	if len(p) == 0 {
		return nil
	}
	return errors.New(strings.Join(p, "; "))
}

func (c Case) validateQuickMode(add func(string, ...any)) {
	if strings.TrimSpace(c.Task) == "" {
		add("task is required")
	}
	if len(c.Agents) > 0 || len(c.Input) > 0 {
		add("agents and input belong to a workflow case; a quick run has agent and task")
	}
	checkAgentSpec("agent", c.Agent, add)
}

func (c Case) validateWorkflowMode(add func(string, ...any)) {
	if strings.TrimSpace(c.Task) != "" || !c.Agent.isZero() {
		add("agent and task belong to a quick run; a workflow case has workflow, input and agents")
	}
	if len(c.Agents) == 0 && c.RunsModel() {
		add("agents is required for a workflow case with agent nodes")
	}
	for _, key := range c.AgentKeys() {
		if !agentKeyPattern.MatchString(key) {
			add("agents key %q must match %s", key, agentKeyPattern)
			continue
		}
		if n := len("eval-") + len(c.ID) + 1 + len(key) + runnerSuffixLen; n > maxAgentNameLen {
			add("agents.%s: the created agent name would be %d characters, the api allows %d", key, n, maxAgentNameLen)
		}
		checkAgentSpec("agents."+key, c.Agents[key], add)
	}
	c.checkWorkflow(add)
	if def, err := c.Definition(); err == nil {
		checkLimits("workflow limits.", def.Limits, add)
	}
	if _, err := c.InputJSON(); err != nil {
		add("%v", err)
	}
}

func checkAgentSpec(path string, a AgentSpec, add func(string, ...any)) {
	for i, t := range a.ToolAllowlist {
		if strings.TrimSpace(t) == "" {
			add("%s.tool_allowlist[%d] is empty", path, i)
		}
	}
	tools := make([]string, 0, len(a.InteractionRules))
	for tool := range a.InteractionRules {
		tools = append(tools, tool)
	}
	slices.Sort(tools)
	for _, tool := range tools {
		if strings.TrimSpace(tool) == "" {
			add("%s.interaction_rules has an empty tool name", path)
		}
		if mode := a.InteractionRules[tool]; !slices.Contains(interactionModes, mode) {
			add("%s.interaction_rules.%s %q must be one of %s", path, tool, mode, strings.Join(interactionModes, ", "))
		}
	}
	checkLimits(path+".limits.", a.Limits.Store(), add)
	checkPolicies(path, a, add)
}

func checkLimits(prefix string, l store.Limits, add func(string, ...any)) {
	for _, f := range []struct {
		name string
		v    float64
	}{
		{"max_steps", float64(l.MaxSteps)},
		{"max_duration_seconds", float64(l.MaxDurationSeconds)},
		{"max_node_iterations", float64(l.MaxNodeIterations)},
		{"max_loop_visits", float64(l.MaxLoopVisits)},
		{"max_cost_usd", l.MaxCostUSD},
		{"max_tool_calls", float64(l.MaxToolCalls)},
		{"approval_timeout_seconds", float64(l.ApprovalTimeoutSeconds)},
	} {
		if f.v < 0 {
			add("%s%s must not be negative", prefix, f.name)
		}
	}
}

func (c Case) validateScript(add func(string, ...any)) {
	switch {
	case len(c.Script) == 0 && c.RunsModel():
		add("script is required and must have at least one step")
	case len(c.Script) > 0 && !c.RunsModel():
		add("script has steps but the workflow has no agent node to play them")
	}
	for i, s := range c.Script {
		checkScriptStep(fmt.Sprintf("script step %d", i+1), s, add)
	}
}

// checkScriptStep checks one scripted model response: exactly one of text or tool.
func checkScriptStep(at string, s ScriptStep, add func(string, ...any)) {
	hasText := strings.TrimSpace(s.Text) != ""
	switch {
	case s.Tool != nil && hasText:
		add("%s has both text and tool", at)
	case s.Tool == nil && !hasText:
		add("%s has neither text nor tool", at)
	case s.Tool != nil && strings.TrimSpace(s.Tool.Name) == "":
		add("%s: tool name is required", at)
	}
	if s.Tool != nil {
		if _, err := json.Marshal(s.Tool.Args); err != nil {
			add("%s: tool args are not JSON-encodable: %v", at, err)
		}
	}
	if s.Usage != nil && (s.Usage.Prompt < 0 || s.Usage.Completion < 0) {
		add("%s: usage must not be negative", at)
	}
}

func (c Case) validateActions(add func(string, ...any)) {
	a := c.Actions
	if a.CancelAfterSteps < 0 {
		add("actions.cancel_after_steps must not be negative")
	}
	if a.ApprovalWaitSeconds < 0 {
		add("actions.approval_wait_seconds must not be negative")
	}
	for i, act := range a.Approvals {
		if act.Decision != DecisionApprove && act.Decision != DecisionReject {
			add("actions.approvals[%d].decision %q must be %q or %q", i, act.Decision, DecisionApprove, DecisionReject)
		}
	}
	c.validateRunActions(add)
}

func (c Case) validateExpect(add func(string, ...any)) {
	e := c.Expect
	if !store.IsTerminal(e.Status) {
		add("expect.status %q must be a terminal run status (succeeded, failed, cancelled, timed_out)", e.Status)
	}
	if e.MinToolCalls < 0 || e.MinModelCalls < 0 || e.MinAgentMessages < 0 {
		add("expect.min_tool_calls, expect.min_model_calls and expect.min_agent_messages must not be negative")
	}
	for _, ptr := range sortedKeys(e.State) {
		if ptr != "" && !strings.HasPrefix(ptr, "/") {
			add("expect.state key %q must be a JSON pointer (empty or starting with '/')", ptr)
		}
		if _, err := json.Marshal(e.State[ptr]); err != nil {
			add("expect.state %q: value is not JSON-encodable: %v", ptr, err)
		}
	}
	for _, field := range []struct {
		name     string
		statuses map[string][]string
	}{{"tool_statuses", e.ToolStatuses}, {"never_tool_statuses", e.NeverToolStatuses}} {
		for _, tool := range sortedKeys(field.statuses) {
			for _, st := range field.statuses[tool] {
				if !slices.Contains(toolStatusNames, st) {
					add("expect.%s.%s: unknown status %q (want one of %s)", field.name, tool, st, strings.Join(toolStatusNames, ", "))
				}
			}
		}
	}
	if e.ApprovalStatuses != nil {
		for _, st := range *e.ApprovalStatuses {
			if !slices.Contains(approvalStatusNames, st) {
				add("expect.approval_statuses: unknown status %q (want one of %s)", st, strings.Join(approvalStatusNames, ", "))
			}
		}
	}
	c.validateObservedExpect(add)
}

// sortedKeys returns the keys of m in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
