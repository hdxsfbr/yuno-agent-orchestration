package evals

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Triggers a case can expect its run to carry (store.Trigger*; eval is the runner's own).
var expectableTriggers = []string{"manual", "schedule", "channel"}

// validateRunActions checks the actions that change how the runner starts or ends a
// run: schedule, retrigger and cancel_when_waiting.
func (c Case) validateRunActions(add func(string, ...any)) {
	a := c.Actions
	if s := a.Schedule; s != nil {
		switch {
		case !c.IsWorkflow():
			add("actions.schedule needs a workflow case: a schedule triggers a workflow")
		case c.RunsModel():
			add("actions.schedule needs a workflow without agent nodes: a scheduled run cannot carry the scripted model, so its agents would call the real model")
		}
		if s.Kind != ScheduleKindDelay {
			add("actions.schedule.kind %q must be %q: only a one-shot schedule fires within a case", s.Kind, ScheduleKindDelay)
		}
		if s.RunInSeconds < 1 || s.RunInSeconds > maxScheduleRunIn {
			add("actions.schedule.run_in_seconds %d must be between 1 and %d", s.RunInSeconds, maxScheduleRunIn)
		}
		if s.WaitSeconds < 0 || s.WaitSeconds > maxScheduleWait {
			add("actions.schedule.wait_seconds %d must be between 0 and %d", s.WaitSeconds, maxScheduleWait)
		}
		if s.wait() <= s.RunInSeconds {
			add("actions.schedule.wait_seconds %d must exceed run_in_seconds %d, or the runner gives up before the schedule is due", s.wait(), s.RunInSeconds)
		}
		if a.Retrigger {
			add("actions.schedule and actions.retrigger cannot be combined")
		}
	}
	if a.Retrigger && a.Schedule == nil && c.Expect.TriggerStatus == 0 {
		add("actions.retrigger needs expect.trigger_status, or it asserts nothing")
	}
}

// validateObservedExpect checks the expectations that read what the runner observed.
func (c Case) validateObservedExpect(add func(string, ...any)) {
	e, a := c.Expect, c.Actions
	if e.Trigger != "" && !slices.Contains(expectableTriggers, e.Trigger) {
		add("expect.trigger %q must be one of %s", e.Trigger, strings.Join(expectableTriggers, ", "))
	}
	if e.Trigger == "schedule" && a.Schedule == nil {
		add("expect.trigger schedule needs actions.schedule: only the runner knows which schedule it created")
	}
	if a.Schedule != nil && e.Trigger != "schedule" {
		add("a case with actions.schedule must expect trigger schedule")
	}
	if e.ScheduleEnabled != nil && a.Schedule == nil {
		add("expect.schedule_enabled needs actions.schedule")
	}
	if e.TriggerStatus != 0 {
		if e.TriggerStatus < 100 || e.TriggerStatus > 599 {
			add("expect.trigger_status %d is not an HTTP status", e.TriggerStatus)
		}
		if !a.Retrigger {
			add("expect.trigger_status needs actions.retrigger: it is the status of the extra trigger")
		}
	}
	if e.TriggerErrorContains != "" && e.TriggerStatus == 0 {
		add("expect.trigger_error_contains needs expect.trigger_status")
	}

	for _, field := range []struct {
		name  string
		tools []string
	}{{"tools_present", e.ToolsPresent}, {"tools_absent", e.ToolsAbsent}} {
		for i, t := range field.tools {
			if strings.TrimSpace(t) == "" {
				add("expect.%s[%d] is empty", field.name, i)
			}
		}
	}
	for _, t := range e.ToolsPresent {
		if slices.Contains(e.ToolsAbsent, t) {
			add("expect.tools_present and expect.tools_absent both name %q", t)
		}
	}
	for _, tool := range sortedKeys(e.ToolOutputs) {
		if strings.TrimSpace(tool) == "" {
			add("expect.tool_outputs has an empty tool name")
		}
		if len(e.ToolOutputs[tool]) == 0 {
			add("expect.tool_outputs.%s is empty", tool)
		}
		for i, text := range e.ToolOutputs[tool] {
			if text == "" {
				add("expect.tool_outputs.%s[%d] is empty", tool, i)
			}
		}
	}
	if e.ApprovalStatuses != nil && len(e.ApprovalReasons) > len(*e.ApprovalStatuses) {
		add("expect.approval_reasons has %d entries but expect.approval_statuses lists %d approvals", len(e.ApprovalReasons), len(*e.ApprovalStatuses))
	}
}

// maxCriterionRunes bounds a criterion's text: it is a sentence for the grader and the
// report, not a prompt.
const maxCriterionRunes = 300

// validateRubric checks the rubric (DESIGN 5.13 rule 4): none, or three to six
// criteria, each with exactly one local check the heuristic mode can run.
func (c Case) validateRubric(add func(string, ...any)) {
	n := len(c.Rubric)
	if n == 0 {
		return
	}
	if n < MinRubricCriteria || n > MaxRubricCriteria {
		add("rubric has %d criteria, want %d to %d (or none)", n, MinRubricCriteria, MaxRubricCriteria)
	}
	for i, r := range c.Rubric {
		at := fmt.Sprintf("rubric[%d]", i)
		text := strings.TrimSpace(r.Criterion)
		if text == "" {
			add("%s.criterion is required", at)
		}
		if len([]rune(text)) > maxCriterionRunes {
			add("%s.criterion is longer than %d characters", at, maxCriterionRunes)
		}
		k := r.Check
		kinds := k.kinds()
		if len(kinds) != 1 {
			add("%s.check must set exactly one of contains, regex or json_path_eq, got %d", at, len(kinds))
			continue
		}
		if k.Path != "" && !strings.HasPrefix(k.Path, "/") {
			add("%s.check.path %q must be a JSON pointer (empty or starting with '/')", at, k.Path)
		}
		switch kinds[0] {
		case "regex":
			if _, err := regexp.Compile(k.Regex); err != nil {
				add("%s.check.regex is not a valid RE2 expression: %v", at, err)
			}
		case "json_path_eq":
			if k.Path == "" {
				add("%s.check.path is required with json_path_eq", at)
			}
			if _, err := k.expected(); err != nil {
				add("%s.check.json_path_eq: %v", at, err)
			}
		}
	}
}
