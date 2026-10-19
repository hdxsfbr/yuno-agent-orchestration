package httpapi

import (
	"context"
	"encoding/json"
	"path"
	"strings"
	"time"

	"yuno/internal/store"
	"yuno/internal/tools"
	"yuno/internal/workflow"
)

// autoAfterTools is the whole set of tools a node's auto_after can let through without
// an approval of their own (DESIGN 5.29 rules 5 and 14). Nothing else is ever covered.
var autoAfterTools = []string{tools.OpenMergeRequestName, tools.AdvanceWaveName}

// shipSmokeTestNode is the approval node of the ship pipeline that covers advance_wave.
const shipSmokeTestNode = "smoke_test"

// autoApprover is the decided_by and policy text of a call an earlier approval covered.
func autoApprover(approvalNode string) string { return "auto_after:" + approvalNode }

// autoAfter reports which approval node covers this side-effecting call ("" when none):
// the calling node names one with auto_after, the tool is in autoAfterTools, the run's
// newest visit of that approval node was approved (a pending, rejected or expired newest
// decision does not count), and, for open_merge_request, the run already has a request to
// update (a call that would create one stays gated) and the call names paths, every one
// under docs/ (a call that would push the whole checkout, or code, stays gated; review
// M1). Any doubt, including a database
// error, leaves the call gated. advance_wave passes only in a ship run (trigger plan, a
// trigger_ref of the form plan:<id>:wave:<n>) whose calling node is covered by the
// smoke_test approval (review L6).
func (g *gateCall) autoAfter(ctx context.Context) string {
	if g.s.pool == nil || g.rc.RunID == "" || g.t.Risk != tools.RiskSideEffecting {
		return ""
	}
	listed := false
	for _, n := range autoAfterTools {
		listed = listed || n == g.t.Name
	}
	if !listed {
		return ""
	}
	run, err := store.GetRun(ctx, g.s.pool, g.rc.RunID)
	if err != nil || len(run.WorkflowSnapshot) == 0 {
		return ""
	}
	var def workflow.Definition
	if json.Unmarshal(run.WorkflowSnapshot, &def) != nil {
		return ""
	}
	node, ok := def.Node(g.rc.NodeID)
	if !ok || node.AutoAfter == "" {
		return ""
	}
	if a, ok := def.Node(node.AutoAfter); !ok || a.Kind != workflow.KindApproval {
		return ""
	}
	visit, err := store.LatestNodeVisit(ctx, g.s.pool, g.rc.RunID, node.AutoAfter)
	if err != nil {
		return ""
	}
	appr, err := store.GetApprovalForRunNode(ctx, g.s.pool, visit.ID, store.ApprovalKindNode)
	if err != nil || appr.Status != store.ApprovalApproved {
		return ""
	}
	if g.t.Name == tools.AdvanceWaveName {
		if node.AutoAfter != shipSmokeTestNode || run.Trigger != store.TriggerPlan || run.TriggerRef == nil {
			return ""
		}
		if _, _, ok := store.ParseShipRef(*run.TriggerRef); !ok {
			return ""
		}
	}
	if g.t.Name == tools.OpenMergeRequestName {
		if !docsOnlyPaths(g.args) {
			return ""
		}
		if run.MergeRequestID != nil && *run.MergeRequestID != "" {
			if _, err := store.GetMergeRequest(ctx, g.s.pool, *run.MergeRequestID); err == nil {
				return node.AutoAfter
			}
		}
		if rec, err := store.MergeRequestByRun(ctx, g.s.pool, g.rc.RunID); err != nil || rec == nil {
			return ""
		}
	}
	return node.AutoAfter
}

// docsOnlyPaths reports whether an open_merge_request call names paths and every one is a
// clean relative path under docs/ (no "..", no absolute path). Without paths the call would
// commit the whole working tree.
func docsOnlyPaths(args json.RawMessage) bool {
	var in struct {
		Paths []string `json:"paths"`
	}
	if json.Unmarshal(args, &in) != nil || len(in.Paths) == 0 {
		return false
	}
	for _, raw := range in.Paths {
		p := path.Clean(strings.ReplaceAll(raw, "\\", "/"))
		if path.IsAbs(p) || !strings.HasPrefix(p, "docs/") || strings.Contains(p, "..") {
			return false
		}
	}
	return true
}

// recordAutoApproval writes the approval row that shows the call went through on an
// earlier decision: kind tool, approved, decided_by auto_after:<node>, linked to the
// call's row so the run trace lists it with the others. The row's output carries the
// same policy, so a failure here is logged and costs nothing else.
func (g *gateCall) recordAutoApproval(ctx context.Context, approvalNode string) {
	now := time.Now().UTC()
	by := autoApprover(approvalNode)
	reason := "covered by the approved " + approvalNode + " step"
	callID := g.row.ID
	a := &store.Approval{
		RunID: g.rc.RunID, RunNodeID: g.rc.RunNodeID, ToolCallID: &callID, Kind: store.ApprovalKindTool, NodeID: g.rc.NodeID,
		Message: g.t.Message(g.args), Status: store.ApprovalApproved, RequestedAt: g.start, DecidedAt: &now, DecidedBy: &by,
		Reason: &reason, ExpiresAt: now,
	}
	if err := store.InsertApproval(context.WithoutCancel(ctx), g.s.pool, a); err != nil {
		g.log.Error("record auto-approval failed", "error", err)
	}
}
