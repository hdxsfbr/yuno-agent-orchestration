package evals

import (
	"fmt"
	"slices"
	"strings"

	"yuno/internal/store"
)

// Observed is what the runner saw outside the run's own trace, for the expectations a
// trace cannot answer: the answer to an extra trigger, the tool list, the schedule the
// case created. It travels with the case result into the JSON report.
type Observed struct {
	// Retrigger is the answer to the extra trigger of actions.retrigger.
	Retrigger *TriggerAttempt `json:"retrigger,omitempty"`
	// Tools is GET /api/tools, listed when the case expects tools present or absent.
	Tools       []string `json:"tools,omitempty"`
	ToolsListed bool     `json:"tools_listed,omitempty"`
	// Schedule is the schedule the case created, read after the run ended.
	Schedule *ScheduleView `json:"schedule,omitempty"`
}

// TriggerAttempt is the api's answer to a trigger: the HTTP status and, for an error,
// its message.
type TriggerAttempt struct {
	Status  int    `json:"status"`
	Message string `json:"message,omitempty"`
}

// checkObserved compares the observation-backed expectations with what the runner saw.
func checkObserved(exp Expect, run *store.Run, o Observed) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if exp.Trigger != "" {
		switch {
		case run.Trigger != exp.Trigger:
			add("run trigger %q, want %q", run.Trigger, exp.Trigger)
		case exp.Trigger == store.TriggerSchedule:
			switch {
			case o.Schedule == nil:
				add("the runner created no schedule")
			case run.TriggerRef == nil || store.ScheduleIDOfRef(*run.TriggerRef) != o.Schedule.ID:
				add("run trigger_ref %s, want the schedule %s", derefOrNone(run.TriggerRef), o.Schedule.ID)
			}
		}
	}
	if exp.ScheduleEnabled != nil {
		switch {
		case o.Schedule == nil:
			add("the runner did not read the schedule back")
		case o.Schedule.Enabled != *exp.ScheduleEnabled:
			add("schedule enabled is %t after the run, want %t", o.Schedule.Enabled, *exp.ScheduleEnabled)
		}
	}
	if exp.TriggerStatus != 0 {
		switch {
		case o.Retrigger == nil:
			add("the runner made no extra trigger, want status %d", exp.TriggerStatus)
		case o.Retrigger.Status != exp.TriggerStatus:
			add("extra trigger answered %d (%s), want %d", o.Retrigger.Status, snippet(o.Retrigger.Message), exp.TriggerStatus)
		case exp.TriggerErrorContains != "" && !containsFold(o.Retrigger.Message, exp.TriggerErrorContains):
			add("extra trigger answered %q, which does not contain %q", o.Retrigger.Message, exp.TriggerErrorContains)
		}
	}
	if (len(exp.ToolsPresent) > 0 || len(exp.ToolsAbsent) > 0) && !o.ToolsListed {
		add("the runner did not list the tools")
	} else {
		for _, name := range exp.ToolsPresent {
			if !slices.Contains(o.Tools, name) {
				add("tool %s is not listed by GET /api/tools, want it listed", name)
			}
		}
		for _, name := range exp.ToolsAbsent {
			if slices.Contains(o.Tools, name) {
				add("tool %s is listed by GET /api/tools, want it absent", name)
			}
		}
	}
	return problems
}

func derefOrNone(s *string) string {
	if s == nil {
		return "none"
	}
	return *s
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
