package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"

	"yuno/internal/engine"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// T5.22.8: the merge request delegate_task resolved reaches the engine's trigger request,
// and the run is still a channel run with the conversation as its reference (DESIGN 5.22
// rule 8, 5.15 rule 6).
func TestDelegatorPassesTheMergeRequest(t *testing.T) {
	fe := &fakeEngine{}
	s, _ := newTestServer(t, nil, fe, nil)
	_, err := delegator{s}.Delegate(context.Background(), tools.Delegation{AgentID: "ag-1", WorkflowID: "wf-1", Input: []byte(`{"task":"x"}`),
		ProjectID: "pr-1", MergeRequestID: "mr-1", TriggerRef: "conversation:c-1"})
	mustDo(t, err)
	if len(fe.triggers) != 1 {
		t.Fatalf("triggers = %+v", fe.triggers)
	}
	if got := fe.triggers[0]; got.MergeRequestID != "mr-1" || got.ProjectID != "pr-1" || got.WorkflowID != "wf-1" || got.Trigger != store.TriggerChannel ||
		got.TriggerRef != "conversation:c-1" {
		t.Fatalf("trigger = %+v", got)
	}
	// A delegation that names none leaves it empty.
	_, err = delegator{s}.Delegate(context.Background(), tools.Delegation{AgentID: "ag-1", Task: "fix", ProjectID: "pr-1"})
	mustDo(t, err)
	if fe.triggers[1].MergeRequestID != "" {
		t.Fatalf("trigger = %+v", fe.triggers[1])
	}
}

// T5.22.8: the engine's refusals of rule 1 (the queue-time checks of the named request)
// reach the model as "the run was not started: <text>", like every other refusal.
func TestDelegatorReportsTheMergeRequestRefusals(t *testing.T) {
	for _, text := range []string{
		"merge_request_id: merge request !12 is being updated by run run-1",
		"merge_request_id: pull request #4 is merged",
		"merge_request_id: merge request !12 belongs to project Other",
	} {
		fe := &fakeEngine{triggerErr: &engine.ValidationError{Messages: []string{text}}}
		s, _ := newTestServer(t, nil, fe, nil)
		_, err := delegator{s}.Delegate(context.Background(), tools.Delegation{AgentID: "ag-1", Task: "fix", ProjectID: "pr-1", MergeRequestID: "mr-1"})
		var me *tools.ModelError
		if !errors.As(err, &me) || me.Message != "the run was not started: "+text {
			t.Errorf("%q: err = %v", text, err)
		}
	}
}

// T5.22.11 (tools half): GET /api/tools lists read_merge_request, read-only, external,
// untrusted and chat-capable, with its input schema.
func TestListToolsHasReadMergeRequest(t *testing.T) {
	_, ts := newTestServer(t, nil, nil, nil)
	code, body := do(t, ts, http.MethodGet, "/api/tools", "")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	var got []toolView
	mustDo(t, json.Unmarshal(body, &got))
	i := slices.IndexFunc(got, func(tv toolView) bool { return tv.Name == tools.ReadMergeRequestName })
	if i < 0 {
		t.Fatalf("read_merge_request is not listed: %s", body)
	}
	if tv := got[i]; !tv.Chat || tv.Risk != tools.RiskReadOnly || tv.Hits != tools.HitsExternal || tv.TimeoutSeconds != 15 || !tv.UntrustedOutput ||
		tv.Source != "platform" || len(tv.InputSchema) == 0 {
		t.Fatalf("read_merge_request = %+v", tv)
	}
}
