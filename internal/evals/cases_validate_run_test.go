package evals

import (
	"strings"
	"testing"
)

// agentlessSchedule is a workflow case with no agent node, run through a schedule.
const agentlessSchedule = `id: sch-a
version: v1
split: public
description: test case
workflow:
  nodes:
    - { id: start, kind: start }
    - { id: end, kind: end }
  edges:
    - { from: start, to: end }
  limits: { max_steps: 5 }
input: { x: 1 }
actions:
  schedule: { kind: delay, run_in_seconds: 2 }
expect:
  status: succeeded
  trigger: schedule
`

// replace swaps one fragment of a case source, failing the test when it is not there.
func replace(t *testing.T, src, old, repl string) string {
	t.Helper()
	if !strings.Contains(src, old) {
		t.Fatalf("test source has no %q:\n%s", old, src)
	}
	return strings.Replace(src, old, repl, 1)
}

func TestAgentlessScheduleCaseLoads(t *testing.T) {
	c, err := loadOne(t, agentlessSchedule)
	if err != nil {
		t.Fatal(err)
	}
	if c.RunsModel() || c.Actions.Schedule == nil || c.Actions.Schedule.wait() != defaultScheduleWait {
		t.Fatalf("case = %+v", c.Actions)
	}
}

func TestRunsModel(t *testing.T) {
	quick, err := loadOne(t, caseSrc{}.String())
	if err != nil {
		t.Fatal(err)
	}
	wf, err := loadOne(t, minimalWorkflowCase)
	if err != nil {
		t.Fatal(err)
	}
	none, err := loadOne(t, agentlessSchedule)
	if err != nil {
		t.Fatal(err)
	}
	if !quick.RunsModel() || !wf.RunsModel() || none.RunsModel() {
		t.Errorf("quick %v, workflow %v, agentless %v; want true, true, false", quick.RunsModel(), wf.RunsModel(), none.RunsModel())
	}
}

func TestNewActionAndExpectFieldsAreValidated(t *testing.T) {
	wfSchedule := replace(t, minimalWorkflowCase, "actions:\n  approvals: [ { decision: approve } ]\n",
		"actions:\n  approvals: [ { decision: approve } ]\n  schedule: { kind: delay, run_in_seconds: 2 }\n")
	quickWith := func(extra, expect string) string {
		return caseSrc{Extra: extra, Expect: expect}.String()
	}
	retrigger := "actions:\n  retrigger: true\n"
	status := "expect:\n  status: succeeded\n"

	tests := []struct {
		name string
		src  string
		want string
	}{
		{"a script for a workflow with no agent node", replace(t, agentlessSchedule, "input:", "script:\n  - text: hi\ninput:"), "no agent node to play them"},
		{"a schedule on a quick run", quickWith("actions:\n  schedule: { kind: delay, run_in_seconds: 2 }\n", status+"  trigger: schedule\n"), "needs a workflow case"},
		{"a schedule on a workflow with agents", replace(t, wfSchedule, "  status: succeeded\n", "  status: succeeded\n  trigger: schedule\n"), "without agent nodes"},
		{"a cron schedule", replace(t, agentlessSchedule, "kind: delay", "kind: cron"), `must be "delay"`},
		{"no delay", replace(t, agentlessSchedule, "run_in_seconds: 2", "run_in_seconds: 0"), "between 1 and 30"},
		{"too long a delay", replace(t, agentlessSchedule, "run_in_seconds: 2", "run_in_seconds: 31"), "between 1 and 30"},
		{"a wait that ends before the schedule is due", replace(t, agentlessSchedule, "run_in_seconds: 2", "run_in_seconds: 2, wait_seconds: 2"), "must exceed run_in_seconds"},
		{"a negative wait", replace(t, agentlessSchedule, "run_in_seconds: 2", "run_in_seconds: 2, wait_seconds: -1"), "between 0 and 120"},
		{"too long a wait", replace(t, agentlessSchedule, "run_in_seconds: 2", "run_in_seconds: 2, wait_seconds: 121"), "between 0 and 120"},
		{"a schedule and a retrigger", replace(t, agentlessSchedule, "actions:\n", "actions:\n  retrigger: true\n"), "cannot be combined"},
		{"trigger schedule without the action", replace(t, minimalWorkflowCase, "  status: succeeded\n", "  status: succeeded\n  trigger: schedule\n"), "needs actions.schedule"},
		{"the action without the expectation", replace(t, agentlessSchedule, "  trigger: schedule\n", ""), "must expect trigger schedule"},
		{"an unknown trigger", replace(t, agentlessSchedule, "trigger: schedule", "trigger: telepathy"), "expect.trigger"},
		{"schedule_enabled without a schedule", replace(t, minimalWorkflowCase, "  status: succeeded\n", "  status: succeeded\n  schedule_enabled: false\n"), "needs actions.schedule"},
		{"a retrigger that asserts nothing", quickWith(retrigger, status), "needs expect.trigger_status"},
		{"a status without a retrigger", quickWith("", status+"  trigger_status: 429\n"), "needs actions.retrigger"},
		{"a status that is not HTTP", quickWith(retrigger, status+"  trigger_status: 99\n"), "not an HTTP status"},
		{"an error text without a status", quickWith("", status+"  trigger_error_contains: rate\n"), "needs expect.trigger_status"},
		{"an empty tools_absent name", quickWith("", status+"  tools_absent: [\"\"]\n"), "tools_absent[0] is empty"},
		{"a tool both present and absent", quickWith("", status+"  tools_present: [a]\n  tools_absent: [a]\n"), "both name"},
		{"tool_outputs with no texts", quickWith("", status+"  tool_outputs:\n    http_fetch: []\n"), "tool_outputs.http_fetch is empty"},
		{"tool_outputs with an empty text", quickWith("", status+"  tool_outputs:\n    http_fetch: [\"\"]\n"), "tool_outputs.http_fetch[0] is empty"},
		{"more reasons than approvals", quickWith("", status+"  approval_statuses: [expired]\n  approval_reasons: [a, b]\n"), "approval_reasons has 2 entries"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadOne(t, tt.src)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestPoliciesAreValidated(t *testing.T) {
	agent := func(extra string) string {
		return strings.Replace(caseSrc{}.String(), "  limits: { max_steps: 5 }\n", "  limits: { max_steps: 5 }\n"+extra, 1)
	}
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"a negative run rate", agent("  guardrails: { max_runs_per_hour: -1 }\n"), "must not be negative"},
		{"a pattern that is not RE2", agent("  guardrails:\n    blocked_input_patterns:\n      - { name: p, tool: exec, regex: '(?<=a)b' }\n"), "not a valid RE2"},
		{"a pattern with no expression", agent("  guardrails:\n    blocked_input_patterns:\n      - { name: p, tool: exec }\n"), "not a valid RE2"},
		{"a pattern with no name", agent("  guardrails:\n    blocked_input_patterns:\n      - { tool: exec, regex: a }\n"), "name is required"},
		{"a pattern with no tool", agent("  guardrails:\n    blocked_input_patterns:\n      - { name: p, regex: a }\n"), "tool is required"},
		{"two patterns with one name", agent("  guardrails:\n    blocked_input_patterns:\n      - { name: p, tool: exec, regex: a }\n      - { name: p, tool: exec, regex: b }\n"), "used twice"},
		{"an empty blocked tool", agent("  guardrails: { blocked_tools: [\"\"] }\n"), "blocked_tools[0] is empty"},
		{"a negative memory count", agent("  memory_policy: { enabled: true, max_facts: -1 }\n"), "must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadOne(t, tt.src)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}

	c, err := loadOne(t, agent("  guardrails:\n    max_runs_per_hour: 2\n    blocked_tools: [gitlab_create_mr]\n    blocked_input_patterns:\n      - { name: p, tool: exec, regex: 'rm -rf' }\n  memory_policy: { enabled: true, max_facts: 5, fact_ttl_days: 3 }\n"))
	if err != nil {
		t.Fatal(err)
	}
	g := c.Agent.Guardrails.Store()
	if g.MaxRunsPerHour != 2 || len(g.BlockedTools) != 1 || len(g.BlockedInputPatterns) != 1 ||
		g.BlockedInputPatterns[0].Name != "p" || g.BlockedInputPatterns[0].Regex != "rm -rf" || g.BlockedInputPatterns[0].Tool != "exec" {
		t.Errorf("guardrails = %+v", g)
	}
	if m := c.Agent.MemoryPolicy.Store(); !m.Enabled || m.MaxFacts != 5 || m.FactTTLDays != 3 {
		t.Errorf("memory policy = %+v", m)
	}
}

func TestRubricIsValidated(t *testing.T) {
	crit := func(n int, check string) string {
		var b strings.Builder
		b.WriteString("rubric:\n")
		for i := 0; i < n; i++ {
			b.WriteString("  - criterion: Something holds.\n    check: " + check + "\n")
		}
		return b.String()
	}
	withRubric := func(rubric string) string { return replace(t, caseSrc{}.String(), "rubric: []\n", rubric) }
	ok := "{ path: /agent/summary, contains: x }"

	tests := []struct {
		name string
		src  string
		want string
	}{
		{"two criteria", withRubric(crit(2, ok)), "2 criteria, want 3 to 6"},
		{"seven criteria", withRubric(crit(7, ok)), "7 criteria, want 3 to 6"},
		{"no criterion text", withRubric("rubric:\n  - check: " + ok + "\n  - criterion: b\n    check: " + ok + "\n  - criterion: c\n    check: " + ok + "\n"), "criterion is required"},
		{"a criterion that is too long", withRubric(strings.Replace(crit(3, ok), "Something holds.", strings.Repeat("x", 301), 1)), "longer than"},
		{"no check", withRubric(crit(3, "{}")), "exactly one of contains, regex or json_path_eq, got 0"},
		{"two checks", withRubric(crit(3, "{ contains: a, regex: b }")), "got 2"},
		{"an invalid regex", withRubric(crit(3, "{ regex: '(' }")), "not a valid RE2"},
		{"a path that is not a pointer", withRubric(crit(3, "{ path: agent/summary, contains: x }")), "JSON pointer"},
		{"json_path_eq without a path", withRubric(crit(3, "{ json_path_eq: x }")), "path is required with json_path_eq"},
		{"an unknown check", withRubric(crit(3, "{ equals: x }")), "field equals not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadOne(t, tt.src)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}

	c, err := loadOne(t, withRubric(crit(3, ok)+""))
	if err != nil || len(c.Rubric) != 3 {
		t.Fatalf("three criteria must load: %v %+v", err, c.Rubric)
	}
	for _, check := range []string{
		"{ path: /agent/summary, json_path_eq: x }", "{ path: /agent/n, json_path_eq: 3 }", "{ path: /agent/ok, json_path_eq: false }",
		"{ path: /agent/o, json_path_eq: null }", "{ path: /agent/o, json_path_eq: { a: [1, 2] } }", "{ regex: 'a+b' }", "{ contains: x }",
	} {
		if _, err := loadOne(t, withRubric(crit(6, check))); err != nil {
			t.Errorf("check %s: %v", check, err)
		}
	}
}
