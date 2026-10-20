package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// DESIGN 5.3 rule 5 (D-085), the store's side: a visit parked on a tool approval is
// continued, and its run re-queued, only once nothing of it waits any more: not while the
// approval is pending, nor while the approved call's effect still runs (its row still
// awaiting_approval). The continuation names the approval when the visit has no question to
// name, and a visit that names it leaves nothing to continue.
func TestToolParkContinuationAndRequeue(t *testing.T) {
	f := newQFixture(t)
	ctx := context.Background()
	rn := f.visit("coder", 1, NodeRunning, false, nil)
	mustDo(t, execSQL(ctx, f.pool, `UPDATE runs SET status='running' WHERE id='`+f.run.ID+`'`))
	started := f.tick()
	call := &ToolCall{RunID: f.run.ID, RunNodeID: rn.ID, ToolUseID: "mcp-1", ToolName: "open_merge_request",
		Input: json.RawMessage(`{"title":"Ticket 7"}`), Status: ToolAwaitingApproval, StartedAt: &started}
	mustDo(t, InsertToolCall(ctx, f.pool, call))
	a := f.question(rn, ApprovalKindTool, ApprovalPending, "Open merge request")
	mustDo(t, execSQL(ctx, f.pool, `UPDATE approvals SET tool_call_id='`+call.ID+`' WHERE id='`+a.ID+`'`))
	if _, err := ParkVisit(ctx, f.pool, f.run.ID, rn.ID, f.tick()); err != nil {
		t.Fatal(err)
	}

	waits := func(what string) {
		t.Helper()
		if q, err := ContinuationQuestion(ctx, f.pool, f.run.ID, "coder"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: continuation = %+v, %v; want ErrNotFound", what, q, err)
		}
		if _, err := RequeueParkedRun(ctx, f.pool, f.run.ID, rn.ID, f.tick()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: re-queue = %v; want ErrNotFound", what, err)
		}
	}
	waits("the approval is pending")
	mustDo(t, execSQL(ctx, f.pool, `UPDATE approvals SET status='approved', decided_at=now() WHERE id='`+a.ID+`'`))
	waits("the approved call's effect still runs")
	if listed, err := ListParkedToolApprovals(ctx, f.pool); err != nil || len(listed) != 1 || listed[0].ID != a.ID {
		t.Fatalf("parked tool approvals = %+v, %v; the sweep sees the grant whose effect is unrecorded", listed, err)
	}

	mustDo(t, execSQL(ctx, f.pool, `UPDATE tool_calls SET status='executed' WHERE id='`+call.ID+`'`))
	if got, err := ContinuationQuestion(ctx, f.pool, f.run.ID, "coder"); err != nil || got.ID != a.ID {
		t.Fatalf("continuation = %+v, %v; want the tool approval %s", got, err, a.ID)
	}
	run, err := RequeueParkedRun(ctx, f.pool, f.run.ID, rn.ID, f.tick())
	if err != nil || run.Status != RunQueued {
		t.Fatalf("re-queue once settled = %+v, %v", run, err)
	}
	if node, _ := GetRunNode(ctx, f.pool, rn.ID); node.Status != NodeSucceeded || node.FinishedAt == nil {
		t.Fatalf("parked visit after the re-queue = %+v", node)
	}
	if got, err := ContinuationQuestion(ctx, f.pool, f.run.ID, "coder"); err != nil || got.ID != a.ID {
		t.Fatalf("continuation after the re-queue = %+v, %v; the closed parked visit is still continued", got, err)
	}
	if rows, err := ListVisitToolApprovals(ctx, f.pool, rn.ID); err != nil || len(rows) != 1 || rows[0].ID != a.ID {
		t.Fatalf("the visit's tool approvals = %+v, %v", rows, err)
	}
	f.visit("coder", 2, NodeRunning, false, &a.ID)
	if q, err := ContinuationQuestion(ctx, f.pool, f.run.ID, "coder"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after visit 2 names it: continuation = %+v, %v", q, err)
	}
}

// A visit that went on past an answered question and then parked on a tool approval is
// continued only once the approval is decided; the continuation may name the question, and
// then nothing of the visit is continued again.
func TestToolParkAfterAnAnsweredQuestion(t *testing.T) {
	f := newQFixture(t)
	ctx := context.Background()
	rn := f.visit("reviewer", 1, NodeWaitingApproval, true, nil)
	q := f.question(rn, ApprovalKindQuestion, ApprovalApproved, "answered within the wait")
	a := f.question(rn, ApprovalKindTool, ApprovalPending, "Deploy release 1.0.0")
	if got, err := ContinuationQuestion(ctx, f.pool, f.run.ID, "reviewer"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("continuation while the approval is pending = %+v, %v", got, err)
	}
	mustDo(t, execSQL(ctx, f.pool, `UPDATE approvals SET status='rejected', decided_at=now() WHERE id='`+a.ID+`'`))
	got, err := ContinuationQuestion(ctx, f.pool, f.run.ID, "reviewer")
	if err != nil || got.ID != q.ID {
		t.Fatalf("continuation = %+v, %v; want the question %s", got, err, q.ID)
	}
	f.visit("reviewer", 2, NodeRunning, false, &q.ID)
	if got, err := ContinuationQuestion(ctx, f.pool, f.run.ID, "reviewer"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after the visit is continued: continuation = %+v, %v; the tool approval is not continued again", got, err)
	}
}
