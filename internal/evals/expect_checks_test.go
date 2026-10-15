package evals

import (
	"strings"
	"testing"
	"time"

	"yuno/internal/store"
)

func TestEvalExpectationWaveTwoChecks(t *testing.T) {
	withState := func(tr *store.Trace) {
		tr.Run.State = raw(`{"coder":{"summary":"done","tests_passed":true,"files":["a.py","b.py"],"attempts":2},"reviewer":{"verdict":"approved"}}`)
	}
	tests := []struct {
		name   string
		mutate func(c *Case, tr *store.Trace)
		pass   bool
		want   string
	}{
		{name: "state entries match", pass: true, mutate: func(c *Case, tr *store.Trace) {
			withState(tr)
			c.Expect.State = map[string]any{
				"/reviewer/verdict":   "approved",
				"/coder/tests_passed": true,
				"/coder/attempts":     2, // a YAML int against a decoded float64
				"/coder/files/1":      "b.py",
				"/coder/files":        []any{"a.py", "b.py"},
				"/coder":              map[string]any{"summary": "done", "tests_passed": true, "files": []any{"a.py", "b.py"}, "attempts": 2},
			}
		}},
		{name: "the whole state can be named with an empty pointer", pass: true, mutate: func(c *Case, tr *store.Trace) {
			tr.Run.State = raw(`{"a":1}`)
			c.Expect.State = map[string]any{"": map[string]any{"a": 1}}
		}},
		{name: "a state value differs", want: `state /reviewer/verdict is "changes_requested", want "approved"`, mutate: func(c *Case, tr *store.Trace) {
			withState(tr)
			tr.Run.State = raw(`{"reviewer":{"verdict":"changes_requested"}}`)
			c.Expect.State = map[string]any{"/reviewer/verdict": "approved"}
		}},
		{name: "a state path is missing", want: "state /deployer/status is missing", mutate: func(c *Case, tr *store.Trace) {
			withState(tr)
			c.Expect.State = map[string]any{"/deployer/status": "deployed"}
		}},
		{name: "there is no state at all", want: "state /a is missing", mutate: func(c *Case, tr *store.Trace) {
			tr.Run.State = nil
			c.Expect.State = map[string]any{"/a": 1}
		}},
		{name: "a state type differs", want: `state /coder/attempts is 2, want "2"`, mutate: func(c *Case, tr *store.Trace) {
			withState(tr)
			c.Expect.State = map[string]any{"/coder/attempts": "2"}
		}},
		{name: "an array index out of range is missing", want: "state /coder/files/2 is missing", mutate: func(c *Case, tr *store.Trace) {
			withState(tr)
			c.Expect.State = map[string]any{"/coder/files/2": "x"}
		}},
		{name: "every state entry is reported", want: "state /b is missing", mutate: func(c *Case, tr *store.Trace) {
			tr.Run.State = raw(`{}`)
			c.Expect.State = map[string]any{"/a": 1, "/b": 2}
		}},
		{name: "state that is not JSON", want: "run state is not valid JSON", mutate: func(c *Case, tr *store.Trace) {
			tr.Run.State = raw(`{broken`)
			c.Expect.State = map[string]any{"/a": 1}
		}},

		{name: "tool statuses match in order", pass: true, mutate: func(c *Case, tr *store.Trace) {
			tr.ToolCalls = append(tr.ToolCalls,
				store.ToolCall{ID: "d1", ToolName: "deploy_release", Status: store.ToolAwaitingApproval},
				store.ToolCall{ID: "d2", ToolName: "yuno__deploy_release", Status: store.ToolExecuted})
			c.Expect.ToolStatuses = map[string][]string{"deploy_release": {store.ToolAwaitingApproval, store.ToolExecuted}, "exec": {store.ToolExecuted}}
		}},
		{name: "the wanted statuses may have rows in between", pass: true, mutate: func(c *Case, tr *store.Trace) {
			tr.ToolCalls = append(tr.ToolCalls,
				store.ToolCall{ID: "d1", ToolName: "deploy_release", Status: store.ToolRejected},
				store.ToolCall{ID: "d2", ToolName: "deploy_release", Status: store.ToolBlocked},
				store.ToolCall{ID: "d3", ToolName: "deploy_release", Status: store.ToolExecuted})
			c.Expect.ToolStatuses = map[string][]string{"deploy_release": {store.ToolRejected, store.ToolExecuted}}
		}},
		{name: "an empty list means no call", pass: true, mutate: func(c *Case, tr *store.Trace) {
			c.Expect.ToolStatuses = map[string][]string{"deploy_release": {}}
		}},
		{name: "a tool that was called when none was wanted", want: "tool deploy_release was called (executed), want no call", mutate: func(c *Case, tr *store.Trace) {
			tr.ToolCalls = append(tr.ToolCalls, store.ToolCall{ID: "d1", ToolName: "yuno__deploy_release", Status: store.ToolExecuted})
			c.Expect.ToolStatuses = map[string][]string{"deploy_release": {}}
		}},
		{name: "a status is missing", want: "tool deploy_release statuses [blocked], want [executed]", mutate: func(c *Case, tr *store.Trace) {
			tr.ToolCalls = append(tr.ToolCalls, store.ToolCall{ID: "d1", ToolName: "deploy_release", Status: store.ToolBlocked})
			c.Expect.ToolStatuses = map[string][]string{"deploy_release": {store.ToolExecuted}}
		}},
		{name: "statuses in the wrong order", want: "statuses [executed, awaiting_approval], want [awaiting_approval, executed]", mutate: func(c *Case, tr *store.Trace) {
			tr.ToolCalls = append(tr.ToolCalls,
				store.ToolCall{ID: "d1", ToolName: "deploy_release", Status: store.ToolExecuted},
				store.ToolCall{ID: "d2", ToolName: "deploy_release", Status: store.ToolAwaitingApproval})
			c.Expect.ToolStatuses = map[string][]string{"deploy_release": {store.ToolAwaitingApproval, store.ToolExecuted}}
		}},
		{name: "one row cannot match two wanted statuses", want: "want [executed, executed]", mutate: func(c *Case, tr *store.Trace) {
			tr.ToolCalls = append(tr.ToolCalls, store.ToolCall{ID: "d1", ToolName: "deploy_release", Status: store.ToolExecuted})
			c.Expect.ToolStatuses = map[string][]string{"deploy_release": {store.ToolExecuted, store.ToolExecuted}}
		}},
		{name: "a tool with no rows has no statuses", want: "tool finish statuses [], want [executed]", mutate: func(c *Case, tr *store.Trace) {
			c.Expect.ToolStatuses = map[string][]string{"finish": {store.ToolExecuted}}
		}},

		{name: "a ruled-out status that never happened", pass: true, mutate: func(c *Case, tr *store.Trace) {
			tr.ToolCalls = append(tr.ToolCalls, store.ToolCall{ID: "d1", ToolName: "deploy_release", Status: store.ToolBlocked})
			c.Expect.NeverToolStatuses = map[string][]string{"deploy_release": {store.ToolExecuted, store.ToolAwaitingApproval}}
		}},
		{name: "a ruled-out status needs no row at all", pass: true, mutate: func(c *Case, tr *store.Trace) {
			c.Expect.NeverToolStatuses = map[string][]string{"deploy_release": {store.ToolExecuted}}
		}},
		{name: "a ruled-out status that happened", want: "tool deploy_release has a row with status executed (d2), which the case rules out", mutate: func(c *Case, tr *store.Trace) {
			tr.ToolCalls = append(tr.ToolCalls,
				store.ToolCall{ID: "d1", ToolName: "deploy_release", Status: store.ToolBlocked},
				store.ToolCall{ID: "d2", ToolName: "yuno__deploy_release", Status: store.ToolExecuted})
			c.Expect.NeverToolStatuses = map[string][]string{"deploy_release": {store.ToolExecuted}}
		}},
		{name: "a ruled-out status of another tool is not this tool's", pass: true, mutate: func(c *Case, tr *store.Trace) {
			c.Expect.NeverToolStatuses = map[string][]string{"deploy_release": {store.ToolExecuted}}
		}},

		{name: "approval statuses match in request order", pass: true, mutate: func(c *Case, tr *store.Trace) {
			tr.Approvals = []store.Approval{
				{ID: "a2", Status: store.ApprovalApproved, RequestedAt: t0.Add(2 * time.Second)},
				{ID: "a1", Status: store.ApprovalRejected, RequestedAt: t0.Add(time.Second)},
			}
			c.Expect.ApprovalStatuses = &[]string{store.ApprovalRejected, store.ApprovalApproved}
		}},
		{name: "none wanted and none raised", pass: true, mutate: func(c *Case, tr *store.Trace) {
			c.Expect.ApprovalStatuses = &[]string{}
		}},
		{name: "unchecked when not set", pass: true, mutate: func(c *Case, tr *store.Trace) {
			tr.Approvals = []store.Approval{{ID: "a1", Status: store.ApprovalExpired}}
		}},
		{name: "approvals raised when none were wanted", want: "approval statuses [approved], want []", mutate: func(c *Case, tr *store.Trace) {
			tr.Approvals = []store.Approval{{ID: "a1", Status: store.ApprovalApproved}}
			c.Expect.ApprovalStatuses = &[]string{}
		}},
		{name: "an approval ended differently", want: "approval statuses [expired], want [rejected]", mutate: func(c *Case, tr *store.Trace) {
			tr.Approvals = []store.Approval{{ID: "a1", Status: store.ApprovalExpired}}
			c.Expect.ApprovalStatuses = &[]string{store.ApprovalRejected}
		}},
		{name: "an approval is missing", want: "approval statuses [approved], want [approved, approved]", mutate: func(c *Case, tr *store.Trace) {
			tr.Approvals = []store.Approval{{ID: "a1", Status: store.ApprovalApproved}}
			c.Expect.ApprovalStatuses = &[]string{store.ApprovalApproved, store.ApprovalApproved}
		}},

		{name: "enough agent messages", pass: true, mutate: func(c *Case, tr *store.Trace) {
			tr.AgentMessages = make([]store.AgentMessage, 3)
			c.Expect.MinAgentMessages = 3
		}},
		{name: "too few agent messages", want: "1 agent messages, want at least 2", mutate: func(c *Case, tr *store.Trace) {
			tr.AgentMessages = make([]store.AgentMessage, 1)
			c.Expect.MinAgentMessages = 2
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, tr := baseCase(), goodTrace()
			tt.mutate(&c, &tr)
			got := evalExpectation(c, tr)
			if got.Passed != tt.pass {
				t.Fatalf("passed = %v, want %v (detail: %s)", got.Passed, tt.pass, got.Detail)
			}
			if !strings.Contains(got.Detail, tt.want) {
				t.Fatalf("detail %q does not contain %q", got.Detail, tt.want)
			}
		})
	}
}
