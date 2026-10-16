package evals

import (
	"fmt"
	"regexp"
	"strings"

	"yuno/internal/store"
)

// Guardrails mirrors store.Guardrails with YAML tags (DESIGN 5.11). Zero values mean
// "not set".
type Guardrails struct {
	MaxCostUSDPerRun     float64          `yaml:"max_cost_usd_per_run"`
	MaxRunsPerHour       int              `yaml:"max_runs_per_hour"`
	BlockedTools         []string         `yaml:"blocked_tools"`
	BlockedInputPatterns []BlockedPattern `yaml:"blocked_input_patterns"`
}

// BlockedPattern is one input guardrail: an RE2 regex tested against the canonical JSON
// of the named tool's input. A match blocks the call with "input blocked by guardrail
// <name>".
type BlockedPattern struct {
	Name  string `yaml:"name"`
	Tool  string `yaml:"tool"`
	Regex string `yaml:"regex"`
}

func (g Guardrails) isZero() bool {
	return g.MaxCostUSDPerRun == 0 && g.MaxRunsPerHour == 0 && len(g.BlockedTools) == 0 && len(g.BlockedInputPatterns) == 0
}

// Store converts the guardrails to the API's shape.
func (g Guardrails) Store() store.Guardrails {
	out := store.Guardrails{
		MaxCostUSDPerRun: g.MaxCostUSDPerRun,
		MaxRunsPerHour:   g.MaxRunsPerHour,
		BlockedTools:     g.BlockedTools,
	}
	for _, p := range g.BlockedInputPatterns {
		out.BlockedInputPatterns = append(out.BlockedInputPatterns, store.BlockedPattern{Name: p.Name, Tool: p.Tool, Regex: p.Regex})
	}
	return out
}

// MemoryPolicy mirrors store.MemoryPolicy with YAML tags (DESIGN 5.10).
type MemoryPolicy struct {
	Enabled             bool `yaml:"enabled"`
	MaxFacts            int  `yaml:"max_facts"`
	FactTTLDays         int  `yaml:"fact_ttl_days"`
	ConversationWindow  int  `yaml:"conversation_window"`
	ConversationTTLDays int  `yaml:"conversation_ttl_days"`
}

// Store converts the policy to the API's shape.
func (m MemoryPolicy) Store() store.MemoryPolicy {
	return store.MemoryPolicy{
		Enabled:             m.Enabled,
		MaxFacts:            m.MaxFacts,
		FactTTLDays:         m.FactTTLDays,
		ConversationWindow:  m.ConversationWindow,
		ConversationTTLDays: m.ConversationTTLDays,
	}
}

// checkPolicies validates an agent's guardrails and memory policy at load time, with the
// same rules the api applies at save time, so a case never reaches the api with a
// pattern it will refuse.
func checkPolicies(path string, a AgentSpec, add func(string, ...any)) {
	g := a.Guardrails
	if g.MaxCostUSDPerRun < 0 || g.MaxRunsPerHour < 0 {
		add("%s.guardrails: max_cost_usd_per_run and max_runs_per_hour must not be negative", path)
	}
	for i, t := range g.BlockedTools {
		if strings.TrimSpace(t) == "" {
			add("%s.guardrails.blocked_tools[%d] is empty", path, i)
		}
	}
	seen := map[string]bool{}
	for i, p := range g.BlockedInputPatterns {
		at := fmt.Sprintf("%s.guardrails.blocked_input_patterns[%d]", path, i)
		switch {
		case strings.TrimSpace(p.Name) == "":
			add("%s.name is required", at)
		case seen[p.Name]:
			add("%s.name %q is used twice", at, p.Name)
		}
		seen[p.Name] = true
		if strings.TrimSpace(p.Tool) == "" {
			add("%s.tool is required", at)
		}
		if _, err := regexp.Compile(p.Regex); err != nil || p.Regex == "" {
			add("%s.regex %q is not a valid RE2 expression", at, p.Regex)
		}
	}
	m := a.MemoryPolicy
	if m.MaxFacts < 0 || m.FactTTLDays < 0 || m.ConversationWindow < 0 || m.ConversationTTLDays < 0 {
		add("%s.memory_policy: counts and ttl days must not be negative", path)
	}
}

// blockedPatternFor returns the pattern of the agent that blocks input for the tool, or
// false. The simulator uses it to play the MCP gate's guardrail check.
func (g Guardrails) blockedPatternFor(tool, canonicalInput string) (BlockedPattern, bool) {
	for _, p := range g.BlockedInputPatterns {
		if toolBase(p.Tool) != tool {
			continue
		}
		if re, err := regexp.Compile(p.Regex); err == nil && re.MatchString(canonicalInput) {
			return p, true
		}
	}
	return BlockedPattern{}, false
}
