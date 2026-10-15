package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"yuno/internal/engine"
	"yuno/internal/store"
)

// seedApprovals creates a run with a pending tool approval (joined to its tool call),
// a pending exec approval and a decided node approval.
func seedApprovals(t *testing.T) (run *store.Run, tool, exec, node *store.Approval) {
	t.Helper()
	ctx := context.Background()
	run, err := createTestRun(ctx, testPool, store.TriggerManual, nil, store.Limits{})
	mustDo(t, err)
	now := time.Now().UTC()
	visit := &store.RunNode{RunID: run.ID, NodeID: "deployer", Status: store.NodeWaitingApproval, StartedAt: &now}
	mustDo(t, store.CreateRunNode(ctx, testPool, visit))
	key := strings.Repeat("a", 64)
	tc := &store.ToolCall{RunID: run.ID, RunNodeID: visit.ID, ToolUseID: "mcp-1", ToolName: "deploy_release",
		Input: json.RawMessage(`{"version":"1.0.0","paths":["app.py"],"run_token":"[redacted]"}`), Status: store.ToolAwaitingApproval,
		IdempotencyKey: &key, StartedAt: &now}
	mustDo(t, store.InsertToolCall(ctx, testPool, tc))
	mk := func(kind, status string, toolCallID *string, at time.Time) *store.Approval {
		a := &store.Approval{RunID: run.ID, RunNodeID: visit.ID, ToolCallID: toolCallID, Kind: kind, NodeID: "deployer",
			Message: kind + " approval", Status: status, RequestedAt: at, ExpiresAt: at.Add(time.Hour)}
		mustDo(t, store.InsertApproval(ctx, testPool, a))
		return a
	}
	node = mk(store.ApprovalKindNode, store.ApprovalApproved, nil, now.Add(-2*time.Second))
	exec = mk(store.ApprovalKindExec, store.ApprovalPending, nil, now.Add(-time.Second))
	tool = mk(store.ApprovalKindTool, store.ApprovalPending, &tc.ID, now)
	return run, tool, exec, node
}

func TestListApprovals(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	run, tool, exec, _ := seedApprovals(t)

	code, body := do(t, ts, http.MethodGet, "/api/approvals?run_id="+run.ID, "")
	var items []store.ApprovalItem
	if code != http.StatusOK || json.Unmarshal(body, &items) != nil || len(items) != 3 {
		t.Fatalf("status %d: %s", code, body)
	}
	if items[0].ID != tool.ID || items[0].ToolCall == nil || items[0].ToolCall.ToolName != "deploy_release" ||
		!strings.Contains(string(items[0].ToolCall.Input), `"paths"`) {
		t.Fatalf("newest item is not the tool approval with its call: %s", body)
	}
	if items[1].ID != exec.ID || items[1].ToolCall != nil || strings.Contains(string(body), `"tool_call":null`) {
		t.Fatalf("exec approval: %s", body)
	}

	code, body = do(t, ts, http.MethodGet, "/api/approvals?status=pending&run_id="+run.ID+"&limit=1", "")
	if code != http.StatusOK || json.Unmarshal(body, &items) != nil || len(items) != 1 || items[0].ID != tool.ID {
		t.Fatalf("filtered: %d %s", code, body)
	}
	for _, q := range []string{"status=maybe", "limit=0", "limit=x"} {
		if code, body := do(t, ts, http.MethodGet, "/api/approvals?"+q, ""); code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: %d %s", q, code, body)
		}
	}
}

func TestDecideApprovals(t *testing.T) {
	pool := needDB(t)
	fe := &fakeEngine{pool: pool}
	_, ts := newTestServer(t, pool, fe, nil)
	_, tool, exec, node := seedApprovals(t)

	code, body := do(t, ts, http.MethodPost, "/api/approvals/"+tool.ID+"/approve", `{"reason":"looks right"}`)
	var a store.Approval
	if code != http.StatusOK || json.Unmarshal(body, &a) != nil || a.Status != store.ApprovalApproved ||
		a.DecidedBy == nil || *a.DecidedBy != "api" || a.Reason == nil || *a.Reason != "looks right" {
		t.Fatalf("approve: %d %s", code, body)
	}
	if fe.decisions[0] != tool.ID+"/approve/api/looks right" {
		t.Fatalf("engine got %q", fe.decisions[0])
	}

	// T5.3.8: a decision on a decided approval is 409.
	if code, body := do(t, ts, http.MethodPost, "/api/approvals/"+tool.ID+"/reject", ""); code != http.StatusConflict ||
		decodeError(t, body).Error.Code != "not_pending" {
		t.Fatalf("second decision: %d %s", code, body)
	}
	if code, _ := do(t, ts, http.MethodPost, "/api/approvals/"+node.ID+"/approve", `{}`); code != http.StatusConflict {
		t.Fatalf("decided node approval: %d", code)
	}
	if code, _ := do(t, ts, http.MethodPost, "/api/approvals/no-such-approval/approve", ""); code != http.StatusNotFound {
		t.Fatalf("unknown approval: %d", code)
	}

	// always is for exec approvals only; on one it is the approve_always decision.
	_, tool2, _, _ := seedApprovals(t)
	if code, body := do(t, ts, http.MethodPost, "/api/approvals/"+tool2.ID+"/approve", `{"always":true}`); code != http.StatusUnprocessableEntity ||
		!strings.Contains(string(body), `"always"`) {
		t.Fatalf("always on a tool approval: %d %s", code, body)
	}
	if code, body := do(t, ts, http.MethodPost, "/api/approvals/"+exec.ID+"/approve", `{"always":true}`); code != http.StatusOK {
		t.Fatalf("always on exec: %d %s", code, body)
	}
	if last := fe.decisions[len(fe.decisions)-1]; last != exec.ID+"/approve_always/api/" {
		t.Fatalf("engine got %q", last)
	}

	code, body = do(t, ts, http.MethodPost, "/api/approvals/"+tool2.ID+"/reject", `{"reason":"wrong version"}`)
	if code != http.StatusOK || json.Unmarshal(body, &a) != nil || a.Status != store.ApprovalRejected {
		t.Fatalf("reject: %d %s", code, body)
	}

	for _, tc := range []struct{ path, body string }{
		{"/approve", `{"reason":` + jsonQuote(strings.Repeat("r", 2001)) + `}`},
		{"/reject", `{"always":true}`},
		{"/approve", `{"reason":5}`},
	} {
		_, fresh, _, _ := seedApprovals(t)
		if code, body := do(t, ts, http.MethodPost, "/api/approvals/"+fresh.ID+tc.path, tc.body); code != http.StatusUnprocessableEntity {
			t.Fatalf("%s %s: %d %s", tc.path, tc.body, code, body)
		}
	}
}

// The engine's own answers map onto the contract: ErrNotPending (a decision raced
// this one) 409, unknown 404, not implemented 503.
func TestDecideApprovalEngineErrors(t *testing.T) {
	pool := needDB(t)
	tests := []struct {
		err  error
		want int
	}{
		{engine.ErrNotPending, http.StatusConflict},
		{store.ErrNotFound, http.StatusNotFound},
		{engine.ErrNotImplemented, http.StatusServiceUnavailable},
	}
	for _, tc := range tests {
		fe := &fakeEngine{pool: pool, decideErr: tc.err}
		_, ts := newTestServer(t, pool, fe, nil)
		_, tool, _, _ := seedApprovals(t)
		if code, body := do(t, ts, http.MethodPost, "/api/approvals/"+tool.ID+"/approve", ""); code != tc.want {
			t.Fatalf("%v: %d %s", tc.err, code, body)
		}
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
