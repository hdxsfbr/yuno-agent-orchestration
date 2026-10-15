package evals

import (
	"encoding/json"
	"strings"
	"testing"
)

// minimalWorkflowCase is the smallest valid workflow case; tests change one thing in it.
const minimalWorkflowCase = `id: wf-a
version: v1
split: public
description: test case
workflow:
  nodes:
    - { id: start, kind: start }
    - { id: coder, kind: agent, agent_name: coder, task: "Do {{.input.task}}", output_schema: { type: object, properties: { summary: { type: string } }, required: [summary] } }
    - { id: gate, kind: approval, message: "Ship it?" }
    - { id: end, kind: end }
  edges:
    - { from: start, to: coder }
    - { from: coder, to: gate }
    - { from: gate, to: end }
  limits: { max_steps: 5, max_loop_visits: 2 }
input: { task: build it }
agents:
  coder: { system_prompt: be brief, tool_allowlist: [write, finish] }
script:
  - tool: { name: yuno__finish, args: { run_token: "$RUN_TOKEN", result: { summary: ok } } }
  - text: done
actions:
  approvals: [ { decision: approve } ]
expect:
  status: succeeded
`

// change replaces the first occurrence of old in src and fails the test when there is
// none, so a stale substitution cannot silently test nothing.
func change(t *testing.T, src, old, repl string) string {
	t.Helper()
	if !strings.Contains(src, old) {
		t.Fatalf("test setup: %q not found in the case source", old)
	}
	return strings.Replace(src, old, repl, 1)
}

func loadOne(t *testing.T, src string) (Case, error) {
	t.Helper()
	root := t.TempDir()
	writeCase(t, root, "v1/x.yaml", src)
	cases, err := LoadCases(root, "all")
	if err != nil {
		return Case{}, err
	}
	return cases[0], nil
}

func TestWorkflowCaseLoads(t *testing.T) {
	c, err := loadOne(t, minimalWorkflowCase)
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsWorkflow() {
		t.Fatal("a case with a workflow is a workflow case")
	}
	def, err := c.Definition()
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Nodes) != 4 || def.Edges[1].To != "gate" || def.Limits.MaxSteps != 5 {
		t.Fatalf("definition = %+v", def)
	}
	if got := c.RunLimits(); got.MaxSteps != 5 || got.MaxLoopVisits != 2 {
		t.Errorf("a workflow case asks for the definition's limits, got %+v", got)
	}
	if got := c.AgentKeys(); len(got) != 1 || got[0] != "coder" {
		t.Errorf("agent keys = %v", got)
	}
	in, err := c.InputJSON()
	if err != nil || string(in) != `{"task":"build it"}` {
		t.Errorf("input = %s, %v", in, err)
	}
	if c.Actions.Approvals[0].Decision != DecisionApprove {
		t.Errorf("actions = %+v", c.Actions)
	}
}

func TestWorkflowBodySubstitutesAgentIDs(t *testing.T) {
	c, err := loadOne(t, minimalWorkflowCase)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := c.WorkflowBody(map[string]string{"coder": "agent-77"})
	if err != nil {
		t.Fatal(err)
	}
	var def struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &def); err != nil {
		t.Fatal(err)
	}
	coder := def.Nodes[1]
	if coder["agent_id"] != "agent-77" {
		t.Errorf("agent node = %v, want agent_id agent-77", coder)
	}
	if _, has := coder["agent_name"]; has {
		t.Errorf("agent_name must not reach the api (only templates carry it): %v", coder)
	}
	if def.Nodes[2]["kind"] != "approval" || def.Nodes[2]["message"] != "Ship it?" {
		t.Errorf("approval node changed: %v", def.Nodes[2])
	}
	if _, err := c.WorkflowBody(map[string]string{}); err == nil || !strings.Contains(err.Error(), `no agent was created for "coder"`) {
		t.Errorf("a missing id must be an error, got %v", err)
	}
}

func TestQuickCaseHasNoDefinition(t *testing.T) {
	if _, err := (Case{}).Definition(); err == nil {
		t.Error("a quick run has no workflow definition")
	}
	if got := (Case{Agent: AgentSpec{Limits: Limits{MaxSteps: 3}}}).RunLimits(); got.MaxSteps != 3 {
		t.Errorf("a quick run asks for its agent's limits, got %+v", got)
	}
}

func TestWorkflowCaseValidation(t *testing.T) {
	tests := []struct {
		name    string
		old, to string
		want    string
	}{
		{"agent node without a name", "agent_name: coder, ", "", "nodes[1].agent_id: required for agent nodes"},
		{"agent name that is not in agents", "agent_name: coder", "agent_name: nobody", `no agent "nobody" in agents`},
		{"agent id in a case", "agent_name: coder,", "agent_name: coder, agent_id: abc,", "names its agents with agent_name"},
		{"unknown field in the definition", "kind: approval,", "kind: approval, mesage: x,", `unknown field "mesage"`},
		{"unreachable node", "- { from: gate, to: end }", "- { from: gate, to: gate }", "'end' is not reachable from start"},
		{"edge to an unknown node", "to: gate }\n    - { from: gate", "to: gat }\n    - { from: gate", "edges[1].to: unknown node 'gat'"},
		{"task template with a syntax error", `task: "Do {{.input.task}}"`, `task: "Do {{.input.task"`, "nodes[1].task"},
		{"approval message with a syntax error", `message: "Ship it?"`, `message: "{{if}}"`, "nodes[2].message"},
		{"output schema that does not compile", "output_schema: { type: object,", "output_schema: { type: 12,", "output_schema"},
		{"input that breaks the input schema", "  limits: {", "  input_schema: { type: object, required: [other] }\n  limits: {", "input does not match the workflow's input_schema"},
		{"state schema that does not compile", "  limits: {", "  state_schema: { type: 12 }\n  limits: {", "state_schema"},
		{"negative workflow limit", "max_steps: 5", "max_steps: -5", "workflow limits.max_steps must not be negative"},
		{"unused agent", "  coder: { system_prompt: be brief, tool_allowlist: [write, finish] }\n", "  coder: { system_prompt: be brief, tool_allowlist: [write, finish] }\n  extra: { system_prompt: x }\n", "agents.extra is not used by any workflow node"},
		{"agent key that is not a label", "  coder: { system_prompt", "  Coder: { system_prompt", `agents key "Coder"`},
		{"unknown interaction rule", "tool_allowlist: [write, finish] }", "tool_allowlist: [write, finish], interaction_rules: { deploy_release: sometimes } }", "interaction_rules.deploy_release"},
		{"negative agent limit", "tool_allowlist: [write, finish] }", "tool_allowlist: [write, finish], limits: { max_steps: -1 } }", "agents.coder.limits.max_steps must not be negative"},
		{"task beside a workflow", "input: { task", "task: do it\ninput: { task", "belong to a quick run"},
		{"agent beside a workflow", "input: { task", "agent: { name: A }\ninput: { task", "belong to a quick run"},
		{"approval decision that is not one", "decision: approve", "decision: maybe", `actions.approvals[0].decision "maybe"`},
		{"negative approval wait", "actions:\n", "actions:\n  approval_wait_seconds: -1\n", "approval_wait_seconds must not be negative"},
		{"state key that is not a pointer", "expect:\n  status: succeeded\n", "expect:\n  status: succeeded\n  state: { coder/summary: x }\n", "must be a JSON pointer"},
		{"unknown tool status", "expect:\n  status: succeeded\n", "expect:\n  status: succeeded\n  tool_statuses: { deploy_release: [done] }\n", `unknown status "done"`},
		{"unknown ruled-out tool status", "expect:\n  status: succeeded\n", "expect:\n  status: succeeded\n  never_tool_statuses: { deploy_release: [done] }\n", `expect.never_tool_statuses.deploy_release: unknown status "done"`},
		{"unknown approval status", "expect:\n  status: succeeded\n", "expect:\n  status: succeeded\n  approval_statuses: [granted]\n", `unknown status "granted"`},
		{"negative agent message minimum", "expect:\n  status: succeeded\n", "expect:\n  status: succeeded\n  min_agent_messages: -1\n", "must not be negative"},
		{"agents missing", "agents:\n  coder: { system_prompt: be brief, tool_allowlist: [write, finish] }\n", "", "agents is required for a workflow case"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadOne(t, change(t, minimalWorkflowCase, tt.old, tt.to))
			if err == nil {
				t.Fatalf("expected an error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestCreatedAgentNamesStayWithinTheAPILimit(t *testing.T) {
	long := strings.Repeat("a", 58)
	src := change(t, minimalWorkflowCase, "id: wf-a", "id: "+long)
	src = change(t, src, "  coder: { system_prompt", "  reviewer-with-a-long-key: { system_prompt")
	src = change(t, src, "agent_name: coder", "agent_name: reviewer-with-a-long-key")
	_, err := loadOne(t, src)
	if err == nil || !strings.Contains(err.Error(), "the created agent name would be") {
		t.Fatalf("error = %v", err)
	}
}

func TestQuickCaseRejectsWorkflowFields(t *testing.T) {
	_, err := loadOne(t, caseSrc{Extra: "agents: { a: { system_prompt: x } }\n"}.String())
	if err == nil || !strings.Contains(err.Error(), "agents and input belong to a workflow case") {
		t.Fatalf("error = %v", err)
	}
	_, err = loadOne(t, caseSrc{Extra: "input: { a: 1 }\n"}.String())
	if err == nil || !strings.Contains(err.Error(), "agents and input belong to a workflow case") {
		t.Fatalf("error = %v", err)
	}
	_, err = loadOne(t, caseSrc{Extra: "agent_extra: x\n"}.String())
	if err == nil {
		t.Fatal("unknown top-level fields stay errors")
	}
}

func TestExpectDecodesTheWaveTwoFields(t *testing.T) {
	src := change(t, minimalWorkflowCase, "expect:\n  status: succeeded\n", `expect:
  status: failed
  failure_reason_contains: rejected at gate
  min_agent_messages: 2
  state:
    "/coder/summary": ok
    "/coder/attempts": 2
  tool_statuses:
    deploy_release: []
    finish: [failed, executed]
  approval_statuses: []
`)
	c, err := loadOne(t, src)
	if err != nil {
		t.Fatal(err)
	}
	e := c.Expect
	if e.MinAgentMessages != 2 || e.State["/coder/summary"] != "ok" || e.State["/coder/attempts"] != 2 {
		t.Errorf("expect = %+v", e)
	}
	if got, ok := e.ToolStatuses["deploy_release"]; !ok || len(got) != 0 {
		t.Errorf("an empty status list must survive decoding as a present, empty entry: %+v", e.ToolStatuses)
	}
	if len(e.ToolStatuses["finish"]) != 2 {
		t.Errorf("tool statuses = %+v", e.ToolStatuses)
	}
	if e.ApprovalStatuses == nil || len(*e.ApprovalStatuses) != 0 {
		t.Errorf("approval_statuses: [] must mean 'no approvals', not 'unchecked': %+v", e.ApprovalStatuses)
	}

	// Absent means unchecked.
	c, err = loadOne(t, minimalWorkflowCase)
	if err != nil {
		t.Fatal(err)
	}
	if c.Expect.ApprovalStatuses != nil || c.Expect.ToolStatuses != nil || c.Expect.State != nil {
		t.Errorf("absent expectations must stay nil: %+v", c.Expect)
	}
}

func TestQuickCaseAgentRulesAreValidated(t *testing.T) {
	src := strings.Replace(caseSrc{}.String(), "tool_allowlist: [exec]", "tool_allowlist: [exec]\n  interaction_rules: { deploy_release: sometimes }", 1)
	if _, err := loadOne(t, src); err == nil || !strings.Contains(err.Error(), "agent.interaction_rules.deploy_release") {
		t.Fatalf("error = %v", err)
	}
	ok := strings.Replace(caseSrc{}.String(), "tool_allowlist: [exec]", "tool_allowlist: [exec]\n  interaction_rules: { deploy_release: auto }", 1)
	c, err := loadOne(t, ok)
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent.InteractionRules["deploy_release"] != "auto" {
		t.Errorf("rules = %v", c.Agent.InteractionRules)
	}
}
