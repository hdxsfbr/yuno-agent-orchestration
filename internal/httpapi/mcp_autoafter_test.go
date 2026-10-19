package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"yuno/internal/store"
	"yuno/internal/tools"
)

// autoFixture is the gate fixture whose calling node "integrator_ready" has auto_after
// "manual_test", with an approval visit of manual_test and a stubbed open_merge_request.
type autoFixture struct {
	*gateFixture
	execs   int
	project *store.Project
}

func newAutoFixture(t *testing.T, autoAfter string) *autoFixture {
	t.Helper()
	g := newGateFixture(t, []string{"open_merge_request", "deploy_release"}, nil, 200*time.Millisecond)
	ctx := context.Background()
	f := &autoFixture{gateFixture: g}
	def, _ := json.Marshal(map[string]any{
		"nodes": []map[string]any{{"id": "start", "kind": "start"},
			{"id": "manual_test", "kind": "approval"},
			agentNodeJSON(g.agent.ID, autoAfter),
			{"id": "end", "kind": "end"}},
		"edges": []map[string]any{{"from": "start", "to": "manual_test"}, {"from": "manual_test", "to": "integrator_ready"},
			{"from": "integrator_ready", "to": "end"}},
	})
	_, err := testPool.Exec(ctx, `UPDATE runs SET workflow_snapshot = $2 WHERE id = $1`, g.run.ID, def)
	mustDo(t, err)
	_, err = testPool.Exec(ctx, `UPDATE run_nodes SET node_id = 'integrator_ready' WHERE id = $1`, g.visit.ID)
	mustDo(t, err)
	rc := g.fe.tokens[g.token]
	rc.NodeID = "integrator_ready"
	g.fe.tokens[g.token] = rc

	mr, ok := g.s.tools.Get(tools.OpenMergeRequestName)
	if !ok {
		t.Fatal("open_merge_request is not registered")
	}
	orig := mr.Exec
	mr.Exec = func(ctx context.Context, call tools.Call) (tools.Result, error) {
		f.execs++
		return tools.JSONResult(map[string]any{"number": 1, "updated": true}), nil
	}
	t.Cleanup(func() { mr.Exec = orig })

	f.project = &store.Project{ID: store.NewID(), Name: "auto-" + uniqueSuffix(), RepoURL: "https://gitlab.test/group/demo.git",
		Provider: "gitlab", DefaultBranch: "main"}
	mustDo(t, store.CreateProject(ctx, testPool, f.project, nil))
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM merge_requests WHERE project_id = $1`, f.project.ID)
		_ = store.DeleteProject(context.Background(), testPool, f.project.ID)
	})
	return f
}

// decide adds a visit of manual_test with an approval in the given status.
func (f *autoFixture) decide(t *testing.T, visit int, status string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	rn := &store.RunNode{RunID: f.run.ID, NodeID: "manual_test", Visit: visit, Status: store.NodeSucceeded, StartedAt: &now}
	mustDo(t, store.CreateRunNode(ctx, testPool, rn))
	a := &store.Approval{RunID: f.run.ID, RunNodeID: rn.ID, Kind: store.ApprovalKindNode, NodeID: "manual_test", Message: "test it",
		Status: status, RequestedAt: now.Add(time.Duration(visit) * time.Second), ExpiresAt: now.Add(time.Hour)}
	if status != store.ApprovalPending {
		by := "operator"
		a.DecidedBy, a.DecidedAt = &by, &now
	}
	mustDo(t, store.InsertApproval(ctx, testPool, a))
}

func (f *autoFixture) openMR(t *testing.T) {
	t.Helper()
	ok, err := store.InsertMergeRequest(context.Background(), testPool, &store.MergeRequest{ProjectID: f.project.ID, RunID: f.run.ID,
		Provider: "gitlab", Number: 1, URL: "https://gitlab.test/group/demo/-/merge_requests/1", SourceBranch: "yuno/x",
		TargetBranch: "main", Title: "t", Draft: true})
	if err != nil || !ok {
		t.Fatalf("insert the request record: %v %v", ok, err)
	}
}

func (f *autoFixture) mrArgs() map[string]any {
	return map[string]any{"run_token": f.token, "title": "Ready", "description": "d", "draft": false,
		"paths": []string{"docs/plans/checkout"}}
}

// T5.29.4 (gate half): after the approved manual_test, the ready update goes through
// without its own approval and is recorded as covered; nothing else is.
func TestGateAutoAfterLetsTheReadyUpdateThrough(t *testing.T) {
	f := newAutoFixture(t, "manual_test")
	f.decide(t, 1, store.ApprovalApproved)
	f.openMR(t)

	text, isErr := f.call(t, "open_merge_request", f.mrArgs())
	if isErr || !strings.Contains(text, `"policy":"auto_after:manual_test"`) || f.execs != 1 || len(f.fe.approvalReqs) != 0 {
		t.Fatalf("call: err %v %s (execs %d, approvals requested %d)", isErr, text, f.execs, len(f.fe.approvalReqs))
	}
	rows := f.rows(t)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	checkRow(t, rows[0], "open_merge_request", store.ToolExecuted)
	appr, err := store.ListApprovals(context.Background(), testPool, store.ApprovalApproved, f.run.ID, 10)
	mustDo(t, err)
	found := false
	for _, a := range appr {
		if a.Kind == store.ApprovalKindTool {
			found = true
			if a.DecidedBy == nil || *a.DecidedBy != "auto_after:manual_test" || a.ToolCallID == nil || *a.ToolCallID != rows[0].ID || a.NodeID != "integrator_ready" {
				t.Fatalf("auto approval = %+v", a)
			}
		}
	}
	if !found {
		t.Fatal("no approval row records the auto-approval")
	}
}

func TestGateAutoAfterStaysGatedOtherwise(t *testing.T) {
	gated := func(t *testing.T, f *autoFixture, tool string, args map[string]any) {
		t.Helper()
		text, _ := f.call(t, tool, args)
		if !strings.Contains(text, "pending_approval") || len(f.fe.approvalReqs) != 1 || f.execs != 0 {
			t.Fatalf("%s: %s (approvals requested %d, execs %d)", tool, text, len(f.fe.approvalReqs), f.execs)
		}
	}
	t.Run("rejected", func(t *testing.T) {
		f := newAutoFixture(t, "manual_test")
		f.decide(t, 1, store.ApprovalRejected)
		f.openMR(t)
		gated(t, f, "open_merge_request", f.mrArgs())
	})
	t.Run("newest visit still pending", func(t *testing.T) {
		f := newAutoFixture(t, "manual_test")
		f.decide(t, 1, store.ApprovalApproved)
		f.decide(t, 2, store.ApprovalPending)
		f.openMR(t)
		gated(t, f, "open_merge_request", f.mrArgs())
	})
	t.Run("never asked", func(t *testing.T) {
		f := newAutoFixture(t, "manual_test")
		f.openMR(t)
		gated(t, f, "open_merge_request", f.mrArgs())
	})
	t.Run("no request yet: a create", func(t *testing.T) {
		f := newAutoFixture(t, "manual_test")
		f.decide(t, 1, store.ApprovalApproved)
		gated(t, f, "open_merge_request", f.mrArgs())
	})
	for name, paths := range map[string]any{"no paths": nil, "code path": []string{"docs/plans/x", "main.go"},
		"outside docs": []string{"docs/../main.go"}, "absolute": []string{"/docs/x"}, "docs prefix only": []string{"docsx/a"}} {
		t.Run("paths: "+name, func(t *testing.T) {
			f := newAutoFixture(t, "manual_test")
			f.decide(t, 1, store.ApprovalApproved)
			f.openMR(t)
			args := f.mrArgs()
			if paths == nil {
				delete(args, "paths")
			} else {
				args["paths"] = paths
			}
			gated(t, f, "open_merge_request", args)
		})
	}
	t.Run("node without auto_after", func(t *testing.T) {
		f := newAutoFixture(t, "")
		f.decide(t, 1, store.ApprovalApproved)
		f.openMR(t)
		gated(t, f, "open_merge_request", f.mrArgs())
	})
	t.Run("another tool", func(t *testing.T) {
		f := newAutoFixture(t, "manual_test")
		f.decide(t, 1, store.ApprovalApproved)
		f.openMR(t)
		gated(t, f, "deploy_release", f.deployArgs())
	})
}

func agentNodeJSON(agentID, autoAfter string) map[string]any {
	n := map[string]any{"id": "integrator_ready", "kind": "agent", "agent_id": agentID, "task": "t"}
	if autoAfter != "" {
		n["auto_after"] = autoAfter
	}
	return n
}
