package httpapi

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"yuno/internal/store"
)

func draftArgs(token string, draft any) map[string]any {
	args := map[string]any{"run_token": token, "title": "T1", "description": "d"}
	if draft != nil {
		args["draft"] = draft
	}
	return args
}

// readyMR records a merge request of the fixture's project that is open and not a draft.
func (f *autoFixture) readyMR(t *testing.T, runID string, number int64) *store.MergeRequest {
	t.Helper()
	mr := &store.MergeRequest{ProjectID: f.project.ID, RunID: runID, Provider: "gitlab", Number: number,
		URL: fmt.Sprintf("https://gitlab.test/group/demo/-/merge_requests/%d", number), SourceBranch: "yuno/ready",
		TargetBranch: "main", Title: "t"}
	ok, err := store.InsertMergeRequest(context.Background(), testPool, mr)
	if err != nil || !ok {
		t.Fatalf("insert the request record: %v %v", ok, err)
	}
	return mr
}

// D-107: opening a draft merge request needs no approval; the call says so in its policy
// and an approval row records that it went through as a draft.
func TestGateDraftOpensWithoutApproval(t *testing.T) {
	f := newAutoFixture(t, "")
	text, isErr := f.call(t, "open_merge_request", draftArgs(f.token, true))
	if isErr || !strings.Contains(text, `"policy":"auto:draft"`) || f.execs != 1 || len(f.fe.approvalReqs) != 0 {
		t.Fatalf("new draft: err %v %s (execs %d, approvals requested %d)", isErr, text, f.execs, len(f.fe.approvalReqs))
	}
	appr, err := store.ListApprovals(context.Background(), testPool, store.ApprovalApproved, f.run.ID, 10)
	mustDo(t, err)
	if len(appr) != 1 || appr[0].DecidedBy == nil || *appr[0].DecidedBy != draftApprover || appr[0].Kind != store.ApprovalKindTool {
		t.Fatalf("approval rows = %+v", appr)
	}

	// The run's own request, still a draft: new work goes onto it without an approval too.
	f = newAutoFixture(t, "")
	f.openMR(t)
	if text, _ := f.call(t, "open_merge_request", draftArgs(f.token, true)); !strings.Contains(text, `"policy":"auto:draft"`) || f.execs != 1 {
		t.Fatalf("own draft: %s (execs %d)", text, f.execs)
	}
}

// D-107: everything that is not a draft stays gated: draft false or left out, a push onto
// the run's own request once it is ready, and a run that names a request under review.
func TestGateDraftStaysGatedOtherwise(t *testing.T) {
	gated := func(t *testing.T, f *autoFixture, args map[string]any, what string) {
		t.Helper()
		text, _ := f.call(t, "open_merge_request", args)
		if !strings.Contains(text, "pending_approval") || f.execs != 0 || len(f.fe.approvalReqs) != 1 {
			t.Fatalf("%s: %s (execs %d, approvals requested %d)", what, text, f.execs, len(f.fe.approvalReqs))
		}
	}
	f := newAutoFixture(t, "")
	gated(t, f, draftArgs(f.token, false), "draft false")
	f = newAutoFixture(t, "")
	gated(t, f, draftArgs(f.token, nil), "draft left out")

	f = newAutoFixture(t, "")
	f.readyMR(t, f.run.ID, 2)
	gated(t, f, draftArgs(f.token, true), "the run's own request is ready")

	f = newAutoFixture(t, "")
	other := &store.Run{WorkflowID: f.run.WorkflowID, WorkflowVersion: 1, WorkflowSnapshot: f.run.WorkflowSnapshot,
		Trigger: store.TriggerManual, Status: store.RunSucceeded}
	mustDo(t, store.CreateRun(context.Background(), testPool, other))
	named := f.readyMR(t, other.ID, 3)
	_, err := testPool.Exec(context.Background(), `UPDATE runs SET merge_request_id = $2 WHERE id = $1`, f.run.ID, named.ID)
	mustDo(t, err)
	gated(t, f, draftArgs(f.token, true), "the run names a request under review")
}
