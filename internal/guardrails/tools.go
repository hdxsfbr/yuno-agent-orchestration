package guardrails

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"yuno/internal/store"
	"yuno/internal/tools"
)

// ExecTool is the OpenClaw built-in whose input patterns also shape the exec policy.
const ExecTool = "exec"

// ToolBlocked reports whether the agent's guardrails block a tool (rule 3). Names are
// compared without the platform prefix; finish, the run protocol itself, is never
// blocked.
func ToolBlocked(g store.Guardrails, name string) bool {
	name = tools.PlainName(name)
	if name == tools.FinishName {
		return false
	}
	return slices.ContainsFunc(g.BlockedTools, func(b string) bool { return tools.PlainName(b) == name })
}

// DeniedTools are the tools the agent must not be offered, by plain name, sorted and
// unique: its blocked_tools (rule 3) and, while its memory policy is disabled, the
// memory tools (DESIGN 5.10 edge case). The materialiser puts them in tools.deny and
// leaves them out of tools.allow; the MCP gate refuses a blocked one that is called
// anyway.
func DeniedTools(a *store.Agent) []string {
	set := map[string]bool{}
	for _, t := range a.Guardrails.BlockedTools {
		if t = tools.PlainName(t); t != "" && t != tools.FinishName {
			set[t] = true
		}
	}
	if !a.MemoryPolicy.Enabled {
		set[tools.MemorySaveName] = true
		set[tools.MemoryRecallName] = true
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// PatternName is how a pattern is named to the model and in logs: its name, else its
// expression.
func PatternName(p store.BlockedPattern) string {
	if n := strings.TrimSpace(p.Name); n != "" {
		return n
	}
	return p.Regex
}

// BlockedMessage is the tool result text for an input a pattern blocked (rule 4).
func BlockedMessage(p store.BlockedPattern) string {
	return "input blocked by guardrail " + PatternName(p)
}

// MatchInput applies rule 4 to one call: it returns the first of the agent's patterns
// for this tool whose expression matches the canonical JSON of the input (keys sorted,
// no spacing, run_token left out). A pattern that does not compile matches everything
// it applies to, so a broken guardrail fails closed; Validate keeps such patterns from
// being saved.
func MatchInput(g store.Guardrails, tool string, input json.RawMessage) (store.BlockedPattern, bool) {
	tool = tools.PlainName(tool)
	if !slices.ContainsFunc(g.BlockedInputPatterns, func(p store.BlockedPattern) bool { return tools.PlainName(p.Tool) == tool }) {
		return store.BlockedPattern{}, false
	}
	return matchText(g, tool, canonicalInput(input))
}

// matchText tests the patterns for tool against subject as it is.
func matchText(g store.Guardrails, tool, subject string) (store.BlockedPattern, bool) {
	for _, p := range g.BlockedInputPatterns {
		if tools.PlainName(p.Tool) != tool {
			continue
		}
		re, err := compile(p.Regex)
		if err != nil || re.MatchString(subject) {
			return p, true
		}
	}
	return store.BlockedPattern{}, false
}

// MatchCommand is MatchInput for an exec command line, as OpenClaw's exec tool
// receives it ({"command": ...}). The engine can use it to deny an exec approval the
// gateway raises for a blocked command without asking a human.
func MatchCommand(g store.Guardrails, command string) (store.BlockedPattern, bool) {
	raw, _ := json.Marshal(map[string]string{"command": command})
	return MatchInput(g, ExecTool, raw)
}

// ExecPolicy is the exec approvals policy the materialiser pushes for the agent
// (DESIGN 5.11 implementation note). Without exec patterns it is the agent's own. With
// any, every host command must reach the platform: security "allowlist" with an empty
// allowlist, ask "on-miss" and ask_fallback "deny", so the gateway raises an approval
// for each command, the engine denies the ones a pattern matches (MatchCommand) and the
// rest wait in the inbox. Pruning the allowlist instead is not enough: a broad entry
// such as "*" would let a blocked command run unasked. Security "deny" stays "deny",
// which is stricter still. OpenClaw applies this policy to elevated (host) exec only;
// commands in the agent's sandbox are not subject to exec approvals (D-028).
func ExecPolicy(a *store.Agent) store.ExecPolicy {
	pol := a.ExecPolicy
	if !slices.ContainsFunc(a.Guardrails.BlockedInputPatterns, func(p store.BlockedPattern) bool {
		return tools.PlainName(p.Tool) == ExecTool
	}) {
		return pol
	}
	if pol.Security == "deny" {
		return store.ExecPolicy{Security: "deny", Ask: pol.Ask, AskFallback: "deny"}
	}
	return store.ExecPolicy{Security: "allowlist", Ask: "on-miss", AskFallback: "deny"}
}

// canonicalInput is the text patterns are tested against. Input that is not a JSON
// document is tested as it is.
func canonicalInput(input json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return string(input)
	}
	if m, ok := v.(map[string]any); ok {
		// The identity fields bind the call to a run or a conversation; patterns are
		// about what the call asks for (DESIGN 5.15 rule 1 for chat_token).
		delete(m, tools.RunTokenField)
		delete(m, tools.ChatTokenField)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(input)
	}
	canon, err := tools.CanonicalJSON(b)
	if err != nil {
		return string(input)
	}
	return string(canon)
}

// compiled caches expressions: the MCP gate tests them on every platform tool call.
var compiled sync.Map // string -> compileResult

type compileResult struct {
	re  *regexp.Regexp
	err error
}

func compile(expr string) (*regexp.Regexp, error) {
	if c, ok := compiled.Load(expr); ok {
		r := c.(compileResult)
		return r.re, r.err
	}
	re, err := regexp.Compile(expr)
	compiled.Store(expr, compileResult{re: re, err: err})
	return re, err
}
