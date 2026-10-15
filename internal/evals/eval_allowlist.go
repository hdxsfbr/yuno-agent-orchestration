package evals

import (
	"fmt"
	"strings"

	"yuno/internal/store"
)

// evalAllowlist checks that every tool call that executed was one its agent may make:
// the name (without the platform prefix) is in the agent's tool_allowlist, or is finish,
// which every managed agent can call, and the agent's interaction rule for it is not
// "blocked". Calls that were refused (blocked, rejected, expired) do not count: refusing
// them is the platform doing its job.
func evalAllowlist(c Case, t store.Trace) EvalResult {
	res := EvalResult{Name: EvalAllowlist}
	if t.Run == nil {
		res.Detail = "trace has no run"
		return res
	}
	resolver := newAgentResolver(c, t)

	var problems []string
	executed := 0
	for _, tc := range t.ToolCalls {
		if tc.Status != store.ToolExecuted {
			continue
		}
		executed++
		tool := toolBase(tc.ToolName)
		spec, label, ok := resolver.forCall(tc)
		if !ok {
			problems = append(problems, fmt.Sprintf("tool call %s (%s) cannot be tied to an agent of the case", tc.ID, tool))
			continue
		}
		if spec.InteractionRules[tool] == "blocked" {
			problems = append(problems, fmt.Sprintf("agent %s executed %s although its rule is blocked", label, tool))
			continue
		}
		if tool != finishTool && !allowed(spec.ToolAllowlist, tool) {
			problems = append(problems, fmt.Sprintf("agent %s executed %s, which is not in its allowlist [%s]",
				label, tool, strings.Join(spec.ToolAllowlist, ", ")))
		}
	}

	if len(problems) > 0 {
		res.Detail = strings.Join(problems, "; ")
		return res
	}
	res.Passed = true
	res.Detail = fmt.Sprintf("%d executed tool call(s), all within their agents' allowlists", executed)
	return res
}

func allowed(allowlist []string, tool string) bool {
	for _, a := range allowlist {
		if toolBase(a) == tool {
			return true
		}
	}
	return false
}
