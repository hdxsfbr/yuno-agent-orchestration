package evals

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"yuno/internal/store"
)

func TestCheckObserved(t *testing.T) {
	scheduled := &store.Run{Trigger: store.TriggerSchedule, TriggerRef: ptr("sch1@2026-10-16T10:00:00Z")}
	manual := &store.Run{Trigger: store.TriggerManual}
	sch := func(enabled bool) *ScheduleView { return &ScheduleView{ID: "sch1", Enabled: enabled} }

	tests := []struct {
		name string
		exp  Expect
		run  *store.Run
		obs  Observed
		want string // substring of the single problem, "" for none
	}{
		{"nothing expected", Expect{}, manual, Observed{}, ""},
		{"trigger matches", Expect{Trigger: "manual"}, manual, Observed{}, ""},
		{"trigger differs", Expect{Trigger: "manual"}, scheduled, Observed{}, `run trigger "schedule", want "manual"`},
		{"scheduled run carries the schedule", Expect{Trigger: "schedule"}, scheduled, Observed{Schedule: sch(false)}, ""},
		{"scheduled run carries another schedule", Expect{Trigger: "schedule"}, &store.Run{Trigger: "schedule", TriggerRef: ptr("sch9@2026-10-16T10:00:00Z")}, Observed{Schedule: sch(false)}, "want the schedule sch1"},
		{"a schedule whose id the reference only starts with", Expect{Trigger: "schedule"}, &store.Run{Trigger: "schedule", TriggerRef: ptr("sch10@2026-10-16T10:00:00Z")}, Observed{Schedule: sch(false)}, "want the schedule sch1"},
		{"scheduled run without a reference", Expect{Trigger: "schedule"}, &store.Run{Trigger: "schedule"}, Observed{Schedule: sch(false)}, "trigger_ref none"},
		{"no schedule was created", Expect{Trigger: "schedule"}, scheduled, Observed{}, "created no schedule"},
		{"schedule disabled as expected", Expect{ScheduleEnabled: ptr(false)}, manual, Observed{Schedule: sch(false)}, ""},
		{"schedule still enabled", Expect{ScheduleEnabled: ptr(false)}, manual, Observed{Schedule: sch(true)}, "enabled is true after the run, want false"},
		{"schedule not read back", Expect{ScheduleEnabled: ptr(false)}, manual, Observed{}, "did not read the schedule back"},
		{"refused as expected", Expect{TriggerStatus: 429, TriggerErrorContains: "RATE LIMIT"}, manual, Observed{Retrigger: &TriggerAttempt{Status: 429, Message: "rate limit: 1 runs/hour for agent a"}}, ""},
		{"accepted when refusal expected", Expect{TriggerStatus: 429}, manual, Observed{Retrigger: &TriggerAttempt{Status: 201, Message: "created"}}, "answered 201"},
		{"refused with another message", Expect{TriggerStatus: 429, TriggerErrorContains: "rate limit"}, manual, Observed{Retrigger: &TriggerAttempt{Status: 429, Message: "slow down"}}, `does not contain "rate limit"`},
		{"no extra trigger was made", Expect{TriggerStatus: 429}, manual, Observed{}, "made no extra trigger"},
		{"transport failure is not a status", Expect{TriggerStatus: 429}, manual, Observed{Retrigger: &TriggerAttempt{Message: "connection refused"}}, "answered 0"},
		{"tools as expected", Expect{ToolsPresent: []string{"finish"}, ToolsAbsent: []string{"gitlab_create_mr"}}, manual, Observed{Tools: []string{"finish", "exec"}, ToolsListed: true}, ""},
		{"an absent tool is listed", Expect{ToolsAbsent: []string{"gitlab_create_mr"}}, manual, Observed{Tools: []string{"gitlab_create_mr"}, ToolsListed: true}, "gitlab_create_mr is listed"},
		{"a present tool is not listed", Expect{ToolsPresent: []string{"finish"}}, manual, Observed{Tools: []string{"exec"}, ToolsListed: true}, "finish is not listed"},
		{"the tools were never listed", Expect{ToolsAbsent: []string{"x"}}, manual, Observed{}, "did not list the tools"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkObserved(tt.exp, tt.run, tt.obs)
			switch {
			case tt.want == "" && len(got) != 0:
				t.Fatalf("problems = %v, want none", got)
			case tt.want != "" && (len(got) != 1 || !strings.Contains(got[0], tt.want)):
				t.Fatalf("problems = %v, want one containing %q", got, tt.want)
			}
		})
	}
}

func TestExpectationUsesWhatTheRunnerObserved(t *testing.T) {
	c := baseCase()
	c.Expect.TriggerStatus = 429
	tr := goodTrace()
	if got := evalExpectation(c, tr); got.Passed {
		t.Fatal("without the runner's observation the extra trigger cannot have been refused")
	}
	got := evalExpectationObserved(c, tr, Observed{Retrigger: &TriggerAttempt{Status: 429}})
	if !got.Passed {
		t.Fatalf("got %+v", got)
	}
	// Evaluate keeps its two-argument form for traces with nothing observed.
	for _, r := range Evaluate(baseCase(), goodTrace()) {
		if r.Name == EvalExpectation && !r.Passed {
			t.Errorf("Evaluate: %+v", r)
		}
	}
	// And a panic-free path through every evaluator with observations.
	if n := len(EvaluateObserved(c, tr, Observed{})); n != len(EvaluatorNames()) {
		t.Errorf("%d results", n)
	}
}

func rowWithOutput(tool, status, output string) store.ToolCall {
	return store.ToolCall{ID: "t-" + tool, ToolName: tool, Status: status, Output: json.RawMessage(output), FinishedAt: ptr(t0)}
}

func TestCheckToolOutputs(t *testing.T) {
	// How a plain-text result is stored: {"text": ...}, with the quotes and angle
	// brackets of the wrapper escaped as JSON. The check reads the decoded text.
	fetch := rowWithOutput("http_fetch", store.ToolExecuted,
		`{"text":"HTTP 200 from https://docs.python.org/3/\n<untrusted_content source=\"http_fetch\">\nbody\n</untrusted_content>"}`)
	fetchEscaped := rowWithOutput("http_fetch", store.ToolExecuted, `{"text":"<untrusted_content source=\"http_fetch\"> x"}`)
	recall := rowWithOutput("yuno__memory_recall", store.ToolExecuted, `{"facts":[{"key":"deploy_window","value":"Tuesdays at 14:00 UTC"}]}`)
	blocked := rowWithOutput("deploy_release", store.ToolBlocked, `{"error":"input blocked by guardrail no-secret-files"}`)

	tests := []struct {
		name  string
		want  map[string][]string
		calls []store.ToolCall
		bad   string // substring of the one expected problem, "" for none
	}{
		{"the wrapper is in a text output", map[string][]string{"http_fetch": {`<untrusted_content source="http_fetch">`, "</untrusted_content>"}}, []store.ToolCall{fetch}, ""},
		{"html escapes in the stored JSON are decoded", map[string][]string{"http_fetch": {`<untrusted_content source="http_fetch">`}}, []store.ToolCall{fetchEscaped}, ""},
		{"a value nested in a JSON output", map[string][]string{"memory_recall": {"tuesdays at 14:00 utc"}}, []store.ToolCall{recall}, ""},
		{"a refusal is output too", map[string][]string{"deploy_release": {"input blocked by guardrail no-secret-files"}}, []store.ToolCall{blocked}, ""},
		{"the text is missing", map[string][]string{"http_fetch": {"</untrusted_content>"}}, []store.ToolCall{fetchEscaped}, `no http_fetch output contains "</untrusted_content>"`},
		{"the tool was never called", map[string][]string{"memory_recall": {"x"}}, []store.ToolCall{fetch}, "no memory_recall output"},
		{"a row with no output", map[string][]string{"http_fetch": {"x"}}, []store.ToolCall{{ToolName: "http_fetch"}}, "no http_fetch output"},
		{"an output of another tool does not count", map[string][]string{"http_fetch": {"Tuesdays"}}, []store.ToolCall{recall, fetch}, "no http_fetch output"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkToolOutputs(tt.want, tt.calls)
			switch {
			case tt.bad == "" && len(got) != 0:
				t.Fatalf("problems = %v, want none", got)
			case tt.bad != "" && (len(got) != 1 || !strings.Contains(got[0], tt.bad)):
				t.Fatalf("problems = %v, want one containing %q", got, tt.bad)
			}
		})
	}
}

func TestCheckApprovalReasons(t *testing.T) {
	ap := func(at int, reason *string) store.Approval {
		return store.Approval{RequestedAt: t0.Add(time.Duration(at) * time.Second), Reason: reason}
	}
	cancelled := ptr("run cancelled")
	tests := []struct {
		name      string
		want      []string
		approvals []store.Approval
		bad       string
	}{
		{"nothing to check", nil, []store.Approval{ap(0, nil)}, ""},
		{"the reason contains the text", []string{"RUN CANCELLED"}, []store.Approval{ap(0, cancelled)}, ""},
		{"requests are compared oldest first", []string{"", "run cancelled"}, []store.Approval{ap(5, cancelled), ap(1, nil)}, ""},
		{"the first request is the one checked first", []string{"run cancelled"}, []store.Approval{ap(5, cancelled), ap(1, nil)}, `approval 1 reason "" does not contain`},
		{"an empty entry skips an approval", []string{"", "run cancelled"}, []store.Approval{ap(1, nil), ap(5, cancelled)}, ""},
		{"another reason", []string{"run cancelled"}, []store.Approval{ap(0, ptr("approval expired"))}, `does not contain "run cancelled"`},
		{"no reason at all", []string{"run cancelled"}, []store.Approval{ap(0, nil)}, `reason "" does not contain`},
		{"more reasons than approvals", []string{"a", "b"}, []store.Approval{ap(0, ptr("a"))}, "approval 2 has no reason to check"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkApprovalReasons(tt.want, tt.approvals)
			switch {
			case tt.bad == "" && len(got) != 0:
				t.Fatalf("problems = %v, want none", got)
			case tt.bad != "" && (len(got) != 1 || !strings.Contains(got[0], tt.bad)):
				t.Fatalf("problems = %v, want one containing %q", got, tt.bad)
			}
		})
	}
}

func TestExpectationAppliesToolOutputsAndApprovalReasons(t *testing.T) {
	c := baseCase()
	c.Expect.ToolOutputs = map[string][]string{"exec": {"hello"}}
	tr := goodTrace()
	if got := evalExpectation(c, tr); got.Passed || !strings.Contains(got.Detail, `no exec output contains "hello"`) {
		t.Fatalf("a tool without output: %+v", got)
	}
	tr.ToolCalls[0].Output = json.RawMessage(`{"text":"Say HELLO"}`)
	if got := evalExpectation(c, tr); !got.Passed {
		t.Fatalf("with the output: %+v", got)
	}
	c.Expect.ApprovalReasons = []string{"run cancelled"}
	if got := evalExpectation(c, tr); got.Passed || !strings.Contains(got.Detail, "approval 1 has no reason to check") {
		t.Fatalf("a reason without an approval: %+v", got)
	}
}
