package guardrails

import (
	"fmt"
	"regexp"
	"strings"

	"yuno/internal/store"
	"yuno/internal/tools"
)

// maxPatternLen bounds one expression; RE2 is linear in the input, not in the pattern.
const maxPatternLen = 1000

// Problem is one validation failure: the JSON path under the agent body and what is
// wrong, the shape of a 422 detail.
type Problem struct {
	Path    string
	Message string
}

// Validate checks an agent's guardrails when the agent is saved (DESIGN 5.11 rule 4:
// patterns are compiled at save time, a bad one is a 422). known reports whether a
// tool name exists; nil skips that check.
func Validate(g store.Guardrails, known func(name string) bool) []Problem {
	var ps []Problem
	add := func(path, format string, args ...any) {
		ps = append(ps, Problem{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	if g.MaxCostUSDPerRun < 0 {
		add("guardrails.max_cost_usd_per_run", "must not be negative")
	}
	if g.MaxRunsPerHour < 0 {
		add("guardrails.max_runs_per_hour", "must not be negative")
	}
	for i, t := range g.BlockedTools {
		path := fmt.Sprintf("guardrails.blocked_tools[%d]", i)
		name := tools.PlainName(t)
		switch {
		case name == "":
			add(path, "must not be empty")
		case name == tools.FinishName:
			add(path, "finish cannot be blocked: it is how a node reports its result")
		case known != nil && !known(name):
			add(path, "unknown tool %q", t)
		}
	}
	for i, p := range g.BlockedInputPatterns {
		base := fmt.Sprintf("guardrails.blocked_input_patterns[%d]", i)
		if strings.TrimSpace(p.Name) == "" {
			add(base+".name", "is required")
		}
		switch tool := tools.PlainName(p.Tool); {
		case tool == "":
			add(base+".tool", "is required")
		case known != nil && !known(tool):
			add(base+".tool", "unknown tool %q", p.Tool)
		}
		switch {
		case strings.TrimSpace(p.Regex) == "":
			add(base+".regex", "is required")
		case len(p.Regex) > maxPatternLen:
			add(base+".regex", "must be at most %d characters", maxPatternLen)
		default:
			if _, err := regexp.Compile(p.Regex); err != nil {
				add(base+".regex", "invalid regular expression: %s", strings.TrimPrefix(err.Error(), "error parsing regexp: "))
			}
		}
	}
	return ps
}
