package guardrails

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

func agentWith(name string, g store.Guardrails) *store.Agent {
	return &store.Agent{ID: "id-" + name, Name: name, Guardrails: g, MemoryPolicy: store.MemoryPolicy{Enabled: true}}
}

// T5.11.1: the run's cost cap is the smallest of the layered limit and every
// participating agent's per-run cap, never above the platform maximum.
func TestEffectiveLimitsMinOf(t *testing.T) {
	coder := agentWith("Coder", store.Guardrails{MaxCostUSDPerRun: 1.50})
	reviewer := agentWith("Reviewer", store.Guardrails{MaxCostUSDPerRun: 0.75})
	uncapped := agentWith("Deployer", store.Guardrails{})
	base := store.Limits{MaxSteps: 40, MaxCostUSD: 2.00}
	for _, tc := range []struct {
		name   string
		limits store.Limits
		agents []*store.Agent
		max    float64
		want   float64
	}{
		{"two agents, the smaller cap wins", base, []*store.Agent{coder, reviewer}, 20, 0.75},
		{"order does not matter", base, []*store.Agent{reviewer, coder}, 20, 0.75},
		{"workflow limit below every cap", store.Limits{MaxCostUSD: 0.50}, []*store.Agent{coder, reviewer}, 20, 0.50},
		{"uncapped agent does not lower it", base, []*store.Agent{uncapped}, 20, 2.00},
		{"one capped among uncapped", base, []*store.Agent{uncapped, coder, nil}, 20, 1.50},
		{"no agents", base, nil, 20, 2.00},
		{"platform maximum clamps", store.Limits{MaxCostUSD: 50}, []*store.Agent{agentWith("Big", store.Guardrails{MaxCostUSDPerRun: 40})}, 20, 20},
	} {
		got := EffectiveLimits(tc.limits, tc.agents, tc.max)
		if got.MaxCostUSD != tc.want {
			t.Errorf("%s: max_cost_usd = %v, want %v", tc.name, got.MaxCostUSD, tc.want)
		}
		if got.MaxSteps != tc.limits.MaxSteps {
			t.Errorf("%s: other limits changed: %+v", tc.name, got)
		}
	}
}

func TestAgentIDs(t *testing.T) {
	def := &workflow.Definition{Nodes: []workflow.Node{
		{ID: "start", Kind: workflow.KindStart},
		{ID: "coder", Kind: workflow.KindAgent, AgentID: "a1"},
		{ID: "reviewer", Kind: workflow.KindAgent, AgentID: "a2"},
		{ID: "gate", Kind: workflow.KindApproval},
		{ID: "coder_again", Kind: workflow.KindAgent, AgentID: "a1"},
	}}
	if got := AgentIDs(def); !slices.Equal(got, []string{"a1", "a2"}) {
		t.Fatalf("AgentIDs = %v", got)
	}
}

// T5.11.3 (materialiser input) and T5.10.5: blocked tools and, with memory off, the
// memory tools are denied; finish never is.
func TestDeniedTools(t *testing.T) {
	a := &store.Agent{
		Guardrails:   store.Guardrails{BlockedTools: []string{"yuno__deploy_release", "http_fetch", "finish", "exec", "http_fetch"}},
		MemoryPolicy: store.MemoryPolicy{Enabled: false},
	}
	want := []string{"deploy_release", "exec", "http_fetch", "memory_recall", "memory_save"}
	if got := DeniedTools(a); !slices.Equal(got, want) {
		t.Fatalf("denied = %v, want %v", got, want)
	}
	a.MemoryPolicy.Enabled = true
	if got := DeniedTools(a); slices.Contains(got, "memory_save") || slices.Contains(got, "memory_recall") {
		t.Fatalf("memory enabled but denied: %v", got)
	}
	if got := DeniedTools(&store.Agent{MemoryPolicy: store.MemoryPolicy{Enabled: true}}); len(got) != 0 {
		t.Fatalf("nothing blocked, denied = %v", got)
	}
}

func TestToolBlocked(t *testing.T) {
	g := store.Guardrails{BlockedTools: []string{"deploy_release", "yuno__http_fetch", "finish"}}
	for name, want := range map[string]bool{
		"deploy_release": true, "yuno__deploy_release": true, "http_fetch": true,
		"finish": false, "memory_save": false, "gitlab_create_mr": false,
	} {
		if got := ToolBlocked(g, name); got != want {
			t.Errorf("ToolBlocked(%s) = %v, want %v", name, got, want)
		}
	}
}

// T5.11.4: a pattern blocks rm -rf in an exec command, tested on the canonical JSON of
// the input.
func TestMatchInput(t *testing.T) {
	g := store.Guardrails{BlockedInputPatterns: []store.BlockedPattern{
		{Name: "no-rm-rf", Tool: "exec", Regex: `rm\s+-[a-z]*(rf|fr)`},
		{Name: "no-prod-paths", Tool: "deploy_release", Regex: `"paths":\[[^\]]*"prod/`},
		{Name: "token-shaped", Tool: "http_fetch", Regex: `r_[a-z2-7]{24}`},
	}}
	for _, tc := range []struct {
		name, tool, input, want string
	}{
		{"rm -rf blocked", "exec", `{"command":"rm -rf /workspace"}`, "no-rm-rf"},
		{"rm -fr with spacing", "exec", `{ "command" : "cd x && rm   -fr build" }`, "no-rm-rf"},
		{"plain rm allowed", "exec", `{"command":"rm build/out.txt"}`, ""},
		{"other tool not checked", "write", `{"path":"x.sh","content":"rm -rf /"}`, ""},
		{"canonical key order and spacing", "deploy_release", `{ "version": "1", "paths": [ "app.py", "prod/db.py" ], "notes": "" }`, "no-prod-paths"},
		{"prefixed tool name", "yuno__deploy_release", `{"paths":["prod/x"],"version":"1","notes":""}`, "no-prod-paths"},
		{"allowed deploy", "deploy_release", `{"paths":["app.py"],"version":"1","notes":""}`, ""},
		{"run token is not input", "http_fetch", `{"run_token":"r_abcdefghijklmnopqrstuvwx","url":"https://docs.python.org/"}`, ""},
	} {
		p, blocked := MatchInput(g, tc.tool, json.RawMessage(tc.input))
		if got := map[bool]string{true: p.Name}[blocked]; got != tc.want {
			t.Errorf("%s: matched %q, want %q", tc.name, got, tc.want)
		}
	}
	if p, ok := MatchCommand(g, "sudo rm -rf --no-preserve-root /"); !ok || p.Name != "no-rm-rf" {
		t.Errorf("MatchCommand: %v %v", p, ok)
	}
	if got := BlockedMessage(g.BlockedInputPatterns[0]); got != "input blocked by guardrail no-rm-rf" {
		t.Errorf("message = %q", got)
	}
	if got := BlockedMessage(store.BlockedPattern{Tool: "exec", Regex: "curl"}); got != "input blocked by guardrail curl" {
		t.Errorf("unnamed pattern message = %q", got)
	}
}

// A stored pattern that does not compile blocks the tool it names rather than letting
// every call through.
func TestMatchInputBadPatternFailsClosed(t *testing.T) {
	g := store.Guardrails{BlockedInputPatterns: []store.BlockedPattern{{Name: "broken", Tool: "http_fetch", Regex: `(unclosed`}}}
	if _, ok := MatchInput(g, "http_fetch", json.RawMessage(`{"url":"https://docs.python.org/"}`)); !ok {
		t.Fatal("broken pattern let the call through")
	}
	if _, ok := MatchInput(g, "memory_save", json.RawMessage(`{"key":"k","value":"v"}`)); ok {
		t.Fatal("broken pattern blocked a tool it does not name")
	}
}

// T5.11.4 through the materialiser input: an agent with any exec pattern gets a policy
// that asks about every host command (empty allowlist, on-miss, fallback deny), so a
// broad allowlist entry cannot let a blocked command run unasked; the engine then
// denies the matches. An agent without exec patterns keeps its own policy.
func TestExecPolicy(t *testing.T) {
	asks := store.ExecPolicy{Security: "allowlist", Ask: "on-miss", AskFallback: "deny"}
	rmrf := []store.BlockedPattern{{Name: "no-rm-rf", Tool: "exec", Regex: `rm\s+-rf`}}
	for _, tc := range []struct {
		name     string
		patterns []store.BlockedPattern
		own      store.ExecPolicy
		want     store.ExecPolicy
	}{
		{"wildcard entry", rmrf, store.ExecPolicy{Security: "allowlist", Ask: "off", AskFallback: "full", Allowlist: []string{"*"}}, asks},
		{"entry the pattern does not match", rmrf, store.ExecPolicy{Security: "allowlist", Ask: "on-miss", Allowlist: []string{"rm *"}}, asks},
		{"full security", rmrf, store.ExecPolicy{Security: "full", Ask: "off", Allowlist: []string{"python3 -m pytest", "ls"}}, asks},
		{"defaults", rmrf, store.ExecPolicy{}, asks},
		{"prefixed tool name", []store.BlockedPattern{{Name: "p", Tool: "yuno__exec", Regex: "curl"}}, store.ExecPolicy{Security: "full", Allowlist: []string{"*"}}, asks},
		{"deny stays deny", rmrf, store.ExecPolicy{Security: "deny", Ask: "off", Allowlist: []string{"*"}}, store.ExecPolicy{Security: "deny", Ask: "off", AskFallback: "deny"}},
		{"no patterns", nil, store.ExecPolicy{Security: "full", Ask: "off", Allowlist: []string{"*", "rm *"}}, store.ExecPolicy{Security: "full", Ask: "off", Allowlist: []string{"*", "rm *"}}},
		{"patterns for other tools only", []store.BlockedPattern{{Name: "p", Tool: "http_fetch", Regex: "rm"}}, store.ExecPolicy{Security: "full", Allowlist: []string{"rm -rf x"}}, store.ExecPolicy{Security: "full", Allowlist: []string{"rm -rf x"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &store.Agent{ExecPolicy: tc.own, Guardrails: store.Guardrails{BlockedInputPatterns: tc.patterns}}
			own := slices.Clone(tc.own.Allowlist)
			got := ExecPolicy(a)
			if got.Security != tc.want.Security || got.Ask != tc.want.Ask || got.AskFallback != tc.want.AskFallback ||
				!slices.Equal(got.Allowlist, tc.want.Allowlist) {
				t.Fatalf("policy = %+v, want %+v", got, tc.want)
			}
			if !slices.Equal(a.ExecPolicy.Allowlist, own) {
				t.Fatal("ExecPolicy changed the agent's own policy")
			}
		})
	}
}

// T5.11.5: a regex that does not compile is refused at save time with a
// path-qualified message, with the other guardrail checks.
func TestValidate(t *testing.T) {
	known := func(n string) bool {
		return slices.Contains([]string{"exec", "deploy_release", "http_fetch", "finish"}, n)
	}
	g := store.Guardrails{
		MaxCostUSDPerRun: -1,
		MaxRunsPerHour:   -2,
		BlockedTools:     []string{"deploy_release", "finish", "teleport", ""},
		BlockedInputPatterns: []store.BlockedPattern{
			{Name: "ok", Tool: "exec", Regex: `rm\s+-rf`},
			{Name: "bad", Tool: "exec", Regex: `rm (-rf`},
			{Name: "", Tool: "", Regex: ""},
			{Name: "unknown tool", Tool: "teleport", Regex: "x"},
			{Name: "huge", Tool: "exec", Regex: strings.Repeat("a", 1001)},
		},
	}
	got := map[string]string{}
	for _, p := range Validate(g, known) {
		got[p.Path] = p.Message
	}
	want := map[string]string{
		"guardrails.max_cost_usd_per_run":            "must not be negative",
		"guardrails.max_runs_per_hour":               "must not be negative",
		"guardrails.blocked_tools[1]":                "finish cannot be blocked: it is how a node reports its result",
		"guardrails.blocked_tools[2]":                `unknown tool "teleport"`,
		"guardrails.blocked_tools[3]":                "must not be empty",
		"guardrails.blocked_input_patterns[1].regex": "invalid regular expression: missing closing ): `rm (-rf`",
		"guardrails.blocked_input_patterns[2].name":  "is required",
		"guardrails.blocked_input_patterns[2].tool":  "is required",
		"guardrails.blocked_input_patterns[2].regex": "is required",
		"guardrails.blocked_input_patterns[3].tool":  `unknown tool "teleport"`,
		"guardrails.blocked_input_patterns[4].regex": "must be at most 1000 characters",
	}
	for path, msg := range want {
		if got[path] != msg {
			t.Errorf("%s: %q, want %q", path, got[path], msg)
		}
	}
	if len(got) != len(want) {
		t.Errorf("problems = %v", got)
	}
	if ps := Validate(store.Guardrails{MaxRunsPerHour: 3, BlockedInputPatterns: []store.BlockedPattern{{Name: "n", Tool: "exec", Regex: "x"}}}, nil); len(ps) != 0 {
		t.Errorf("valid guardrails: %v", ps)
	}
}
