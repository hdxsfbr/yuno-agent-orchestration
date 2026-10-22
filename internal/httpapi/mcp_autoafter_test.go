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
	// approvalNode is the node decide adds visits of.
	approvalNode string
}

func newAutoFixture(t *testing.T, autoAfter string) *autoFixture {
	t.Helper()
	g := newGateFixture(t, []string{"open_merge_request", "deploy_release", "advance_wave"}, nil, 200*time.Millisecond)
	ctx := context.Background()
	f := &autoFixture{gateFixture: g, approvalNode: "manual_test"}
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
	rn := &store.RunNode{RunID: f.run.ID, NodeID: f.approvalNode, Visit: visit, Status: store.NodeSucceeded, StartedAt: &now}
	mustDo(t, store.CreateRunNode(ctx, testPool, rn))
	a := &store.Approval{RunID: f.run.ID, RunNodeID: rn.ID, Kind: store.ApprovalKindNode, NodeID: f.approvalNode, Message: "test it",
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

// mergedMR records the run's request as merged: the operator merged it before the ready
// call, which then opens a follow-up (DESIGN 5.29 rule 4).
func (f *autoFixture) mergedMR(t *testing.T) {
	t.Helper()
	ok, err := store.InsertMergeRequest(context.Background(), testPool, &store.MergeRequest{ProjectID: f.project.ID, RunID: f.run.ID,
		Provider: "gitlab", Number: 1, URL: "https://gitlab.test/group/demo/-/merge_requests/1", SourceBranch: "yuno/x",
		TargetBranch: "main", Title: "t", State: store.MergeRequestMerged})
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

// DESIGN 5.29 rules 4 and 5: when the operator merged the request before the ready call,
// the call opens a follow-up, a new request. auto_after lets it through as it lets the ready
// call through, only when it commits paths under docs/; one that would push code stays gated.
func TestGateAutoAfterCoversADocsFollowUp(t *testing.T) {
	f := newAutoFixture(t, "manual_test")
	f.decide(t, 1, store.ApprovalApproved)
	f.mergedMR(t)
	text, isErr := f.call(t, "open_merge_request", f.mrArgs())
	if isErr || !strings.Contains(text, `"policy":"auto_after:manual_test"`) || f.execs != 1 || len(f.fe.approvalReqs) != 0 {
		t.Fatalf("docs follow-up: err %v %s (execs %d, approvals requested %d)", isErr, text, f.execs, len(f.fe.approvalReqs))
	}

	f = newAutoFixture(t, "manual_test")
	f.decide(t, 1, store.ApprovalApproved)
	f.mergedMR(t)
	args := f.mrArgs()
	args["paths"] = []string{"docs/plans/x", "main.go"}
	text, _ = f.call(t, "open_merge_request", args)
	if !strings.Contains(text, "pending_approval") || len(f.fe.approvalReqs) != 1 || f.execs != 0 {
		t.Fatalf("code follow-up: %s (approvals requested %d, execs %d)", text, len(f.fe.approvalReqs), f.execs)
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

// newShipFixture is the gate fixture of a ship run: trigger plan, the ref the watcher
// writes, the smoke_test approval and an integrator node covered by it. advance_wave is
// stubbed and counted in execs.
func newShipFixture(t *testing.T, trigger, ref string) *autoFixture {
	t.Helper()
	f := newAutoFixture(t, "smoke_test")
	f.approvalNode = "smoke_test"
	def, _ := json.Marshal(map[string]any{
		"nodes": []map[string]any{{"id": "start", "kind": "start"}, {"id": "smoke_test", "kind": "approval"},
			agentNodeJSON(f.agent.ID, "smoke_test"), {"id": "end", "kind": "end"}},
		"edges": []map[string]any{{"from": "start", "to": "smoke_test"}, {"from": "smoke_test", "to": "integrator_ready"},
			{"from": "integrator_ready", "to": "end"}},
	})
	var refArg *string
	if ref != "" {
		refArg = &ref
	}
	_, err := testPool.Exec(context.Background(), `UPDATE runs SET workflow_snapshot = $2, trigger = $3, trigger_ref = $4 WHERE id = $1`,
		f.run.ID, def, trigger, refArg)
	mustDo(t, err)
	aw, ok := f.s.tools.Get(tools.AdvanceWaveName)
	if !ok {
		t.Fatal("advance_wave is not registered")
	}
	orig := aw.Exec
	aw.Exec = func(ctx context.Context, call tools.Call) (tools.Result, error) {
		f.execs++
		return tools.JSONResult(map[string]any{"plan_id": "p1", "wave": 1}), nil
	}
	t.Cleanup(func() { aw.Exec = orig })
	return f
}

// DESIGN 5.29 rule 14, review L6: advance_wave needs its own approval until the ship
// run's smoke test is approved, goes through after it, and never passes in a run that is
// not a ship run, whatever approval covers its node.
func TestGateAdvanceWave(t *testing.T) {
	args := func(f *autoFixture) map[string]any { return map[string]any{"run_token": f.token} }
	shipRef := func() string { return store.ShipRef("plan-"+uniqueSuffix(), 2) }
	gated := func(t *testing.T, f *autoFixture) {
		t.Helper()
		text, _ := f.call(t, "advance_wave", args(f))
		if !strings.Contains(text, "pending_approval") || len(f.fe.approvalReqs) != 1 || f.execs != 0 {
			t.Fatalf("advance_wave: %s (approvals requested %d, execs %d)", text, len(f.fe.approvalReqs), f.execs)
		}
	}
	t.Run("before the smoke test is approved", func(t *testing.T) {
		f := newShipFixture(t, store.TriggerPlan, shipRef())
		gated(t, f)
	})
	t.Run("smoke test pending", func(t *testing.T) {
		f := newShipFixture(t, store.TriggerPlan, shipRef())
		f.decide(t, 1, store.ApprovalPending)
		gated(t, f)
	})
	t.Run("smoke test approved", func(t *testing.T) {
		f := newShipFixture(t, store.TriggerPlan, shipRef())
		f.decide(t, 1, store.ApprovalApproved)
		text, isErr := f.call(t, "advance_wave", args(f))
		if isErr || !strings.Contains(text, `"policy":"auto_after:smoke_test"`) || f.execs != 1 || len(f.fe.approvalReqs) != 0 {
			t.Fatalf("advance_wave: err %v %s (execs %d, approvals requested %d)", isErr, text, f.execs, len(f.fe.approvalReqs))
		}
	})
	t.Run("ticket run after a manual_test approval", func(t *testing.T) {
		f := newAutoFixture(t, "manual_test")
		aw, _ := f.s.tools.Get(tools.AdvanceWaveName)
		orig := aw.Exec
		aw.Exec = func(ctx context.Context, call tools.Call) (tools.Result, error) {
			f.execs++
			return tools.JSONResult(map[string]any{}), nil
		}
		t.Cleanup(func() { aw.Exec = orig })
		f.decide(t, 1, store.ApprovalApproved)
		gated(t, f)
	})
	t.Run("approved smoke test in a run that is not a ship run", func(t *testing.T) {
		for name, tc := range map[string]struct{ trigger, ref string }{
			"manual":        {store.TriggerManual, ""},
			"schedule":      {store.TriggerSchedule, "sched-" + uniqueSuffix()},
			"plan, bad ref": {store.TriggerPlan, "plan:no-wave-" + uniqueSuffix()},
			"plan, no ref":  {store.TriggerPlan, ""},
		} {
			t.Run(name, func(t *testing.T) {
				f := newShipFixture(t, tc.trigger, tc.ref)
				f.decide(t, 1, store.ApprovalApproved)
				gated(t, f)
			})
		}
	})
}

// Review M3: the lessons request the ship template asks for goes through the MCP handler's
// schema validation. The call without a branch (the platform names it) reaches the
// operator's approval; one that passes a branch is refused by the schema.
func TestShipLessonsRequestCall(t *testing.T) {
	lessons := func(f *autoFixture, extra map[string]any) map[string]any {
		a := map[string]any{"run_token": f.token, "title": "Lessons from x, wave 1", "description": "Lessons of wave 1.",
			"paths": []string{"CLAUDE.md"}}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}
	f := newShipFixture(t, store.TriggerPlan, store.ShipRef("plan-"+uniqueSuffix(), 2))
	f.decide(t, 1, store.ApprovalApproved)
	text, _ := f.call(t, "open_merge_request", lessons(f, nil))
	if !strings.Contains(text, "pending_approval") || len(f.fe.approvalReqs) != 1 || f.execs != 0 {
		t.Fatalf("lessons request: %s (approvals requested %d, execs %d)", text, len(f.fe.approvalReqs), f.execs)
	}
	text, isErr := f.call(t, "open_merge_request", lessons(f, map[string]any{"branch": "lessons/x-wave-1"}))
	if !isErr || !strings.Contains(text, "branch") || len(f.fe.approvalReqs) != 1 || f.execs != 0 {
		t.Fatalf("with a branch: err %v %s", isErr, text)
	}
}
