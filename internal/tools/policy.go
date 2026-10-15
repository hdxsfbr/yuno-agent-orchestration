package tools

import (
	"slices"
	"strings"
)

// Interaction rule values (DESIGN 5.3 rule 6, D-009).
const (
	PolicyAuto            = "auto"
	PolicyRequireApproval = "require_approval"
	PolicyBlocked         = "blocked"
)

// PlatformPrefix is how OpenClaw names this server's tools to the model (DESIGN 5.7
// rule 2); tables and the API use the plain name.
const PlatformPrefix = "yuno__"

// PlainName strips the platform prefix from a tool name.
func PlainName(name string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), PlatformPrefix)
}

// EffectivePolicy is the agent's interaction rule for the tool, else require_approval
// for side-effecting tools, else auto (DESIGN 5.3 rule 6). finish is the run protocol
// itself and is always auto.
func EffectivePolicy(t *Tool, rules map[string]string) string {
	if t.Name == FinishName {
		return PolicyAuto
	}
	for k, v := range rules {
		if PlainName(k) == t.Name && v != "" {
			return v
		}
	}
	if t.Risk == RiskSideEffecting {
		return PolicyRequireApproval
	}
	return PolicyAuto
}

// Permitted reports whether the agent's allowlist names the tool. finish needs no
// entry: the materialiser always allows it (openclaw.toolLists).
func Permitted(name string, allowlist []string) bool {
	if name == FinishName {
		return true
	}
	return slices.ContainsFunc(allowlist, func(a string) bool { return PlainName(a) == name })
}
