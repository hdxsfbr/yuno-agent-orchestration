package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// mrProject stores a project and a run that names it, optionally a channel run that
// reports to a conversation.
func mrProject(t *testing.T, pool *pgxpool.Pool, name string) (*Project, *Run) {
	t.Helper()
	ctx := context.Background()
	p := &Project{Name: name, RepoURL: "https://gitlab.test/group/" + name + ".git", Provider: "gitlab", DefaultBranch: "main"}
	mustDo(t, CreateProject(ctx, pool, p, []byte("sealed")))
	return p, mrRun(t, pool, p, nil)
}

// mrRun queues a run of the project; ref, when set, makes it a channel run delegated
// from the conversation with that trigger_ref.
func mrRun(t *testing.T, pool *pgxpool.Pool, p *Project, ref *string) *Run {
	t.Helper()
	ctx := context.Background()
	wf := &Workflow{Name: "wf " + NewID(), Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(t, CreateWorkflow(ctx, pool, wf))
	settings := p.Settings()
	run := &Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: TriggerManual,
		ProjectID: &p.ID, Project: &settings}
	if ref != nil {
		run.Trigger, run.TriggerRef = TriggerChannel, ref
	}
	mustDo(t, CreateRun(ctx, pool, run))
	return run
}

func mrOf(p *Project, run *Run, number int64) *MergeRequest {
	return &MergeRequest{ProjectID: p.ID, RunID: run.ID, Provider: "gitlab", Number: number,
		URL:          "https://gitlab.test/group/demo/-/merge_requests/" + strconv.FormatInt(number, 10),
		SourceBranch: "yuno/abc", TargetBranch: "main", Title: "Add a thing"}
}

// T5.20.1: the record is written in the side effect's transaction. A fn that inserts
// the record and then fails leaves neither the record nor the key; one that succeeds
// commits both with the result, and the key replays without calling fn again.
func TestRunSideEffectTxWritesTheRecordWithTheResult(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, run := mrProject(t, pool, "t1")
	// side_effects has no foreign key to the tables the helper truncates, so the keys
	// are unique per run of the test.
	keyFail, keyPlain := "mr-key-fail-"+NewID(), "mr-key-plain-"+NewID()

	failed := errors.New("the call failed after the insert")
	_, _, err := RunSideEffectTx(ctx, pool, keyFail, "open_merge_request", run.ID, "tc-1",
		func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
			if ok, err := InsertMergeRequest(ctx, tx, mrOf(p, run, 1)); err != nil || !ok {
				t.Fatalf("insert in the transaction: %v %v", ok, err)
			}
			if mr, _ := MergeRequestByRun(ctx, tx, run.ID); mr == nil {
				t.Fatal("the record is not visible inside the transaction")
			}
			return nil, failed
		})
	if !errors.Is(err, failed) {
		t.Fatalf("err = %v, want fn's error", err)
	}
	if _, err := GetSideEffect(ctx, pool, keyFail); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the key survived a failed call: %v", err)
	}
	if mr, err := MergeRequestByRun(ctx, pool, run.ID); err != nil || mr != nil {
		t.Fatalf("the record survived a failed call: %+v %v", mr, err)
	}

	// The same key can be tried again, and this time it commits both.
	calls := 0
	do := func() (json.RawMessage, bool, error) {
		return RunSideEffectTx(ctx, pool, keyFail, "open_merge_request", run.ID, "tc-2",
			func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
				calls++
				if _, err := InsertMergeRequest(ctx, tx, mrOf(p, run, 1)); err != nil {
					return nil, err
				}
				return json.RawMessage(`{"number":1}`), nil
			})
	}
	res, replayed, err := do()
	if err != nil || replayed || string(res) != `{"number":1}` {
		t.Fatalf("second try = %s %v %v", res, replayed, err)
	}
	if mr, _ := MergeRequestByRun(ctx, pool, run.ID); mr == nil || mr.Number != 1 || mr.State != MergeRequestOpen || mr.CIStatus != CIStatusNone || mr.SyncedAt != nil {
		t.Fatalf("record = %+v", mr)
	}
	if se, err := GetSideEffect(ctx, pool, keyFail); err != nil || se.ToolCallID != "tc-2" {
		t.Fatalf("side effect = %+v %v", se, err)
	}
	if _, replayed, err := do(); err != nil || !replayed || calls != 1 {
		t.Fatalf("third call: replayed %v, fn calls %d, %v", replayed, calls, err)
	}

	// RunSideEffect (no transaction for fn) still releases the key when fn fails.
	_, _, err = RunSideEffect(ctx, pool, keyPlain, "deploy_release", run.ID, "tc-3",
		func(context.Context) (json.RawMessage, error) { return nil, failed })
	if !errors.Is(err, failed) {
		t.Fatal(err)
	}
	if _, err := GetSideEffect(ctx, pool, keyPlain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("plain key survived: %v", err)
	}
}

// Rule 1 and 2: a run has one record and a provider number is recorded once per
// project; a conflicting insert writes nothing and does not abort the transaction.
func TestMergeRequestInsertAndLookups(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, run := mrProject(t, pool, "t2")

	mr := mrOf(p, run, 7)
	if ok, err := InsertMergeRequest(ctx, pool, mr); err != nil || !ok || mr.ID == "" {
		t.Fatalf("insert = %v %v", ok, err)
	}
	if ok, err := InsertMergeRequest(ctx, pool, mrOf(p, run, 8)); err != nil || ok {
		t.Fatalf("a second record for the run = %v %v", ok, err)
	}
	other := mrRun(t, pool, p, nil)
	if ok, err := InsertMergeRequest(ctx, pool, mrOf(p, other, 7)); err != nil || ok {
		t.Fatalf("a second record for the number = %v %v", ok, err)
	}
	// Inside a transaction a refused insert leaves it usable.
	tx, err := pool.Begin(ctx)
	mustDo(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck
	if ok, err := InsertMergeRequest(ctx, tx, mrOf(p, other, 7)); err != nil || ok {
		t.Fatalf("conflict in a transaction = %v %v", ok, err)
	}
	if ok, err := InsertMergeRequest(ctx, tx, mrOf(p, other, 9)); err != nil || !ok {
		t.Fatalf("insert after a conflict = %v %v", ok, err)
	}
	mustDo(t, LockRunMergeRequest(ctx, tx, other.ID))
	mustDo(t, tx.Commit(ctx))

	got, err := GetMergeRequest(ctx, pool, mr.ID)
	if err != nil || got.RunID != run.ID || got.Number != 7 || got.SourceBranch != "yuno/abc" || got.UnresolvedThreads != nil || got.HeadSHA != nil {
		t.Fatalf("get = %+v %v", got, err)
	}
	if _, err := GetMergeRequest(ctx, pool, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if by, err := MergeRequestByRun(ctx, pool, other.ID); err != nil || by == nil || by.Number != 9 {
		t.Fatalf("by run = %+v %v", by, err)
	}
	if by, err := MergeRequestByRun(ctx, pool, "no-run"); err != nil || by != nil {
		t.Fatalf("by an unknown run = %+v %v", by, err)
	}
}

// Rule 6: newest first, filters, 50 by default and 200 at most; empty is [] not null.
func TestListMergeRequests(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p1, run1 := mrProject(t, pool, "t3a")
	p2, run2 := mrProject(t, pool, "t3b")
	base := mustParse(t, "2026-10-17T10:00:00Z")
	a, b, c := mrOf(p1, run1, 1), mrOf(p2, run2, 1), mrOf(p1, mrRun(t, pool, p1, nil), 2)
	a.CreatedAt, b.CreatedAt, c.CreatedAt = base, base.Add(time.Minute), base.Add(2*time.Minute)
	b.State, c.State = MergeRequestMerged, MergeRequestClosed
	for _, mr := range []*MergeRequest{a, b, c} {
		if ok, err := InsertMergeRequest(ctx, pool, mr); err != nil || !ok {
			t.Fatalf("insert: %v %v", ok, err)
		}
	}
	ids := func(f MergeRequestFilter) []string {
		list, err := ListMergeRequests(ctx, pool, f)
		mustDo(t, err)
		out := make([]string, len(list))
		for i, mr := range list {
			out[i] = mr.ID
		}
		return out
	}
	eq := func(got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}
	eq(ids(MergeRequestFilter{}), c.ID, b.ID, a.ID)
	eq(ids(MergeRequestFilter{ProjectID: p1.ID}), c.ID, a.ID)
	eq(ids(MergeRequestFilter{RunID: run2.ID}), b.ID)
	eq(ids(MergeRequestFilter{State: MergeRequestMerged}), b.ID)
	eq(ids(MergeRequestFilter{ProjectID: p1.ID, State: MergeRequestOpen}), a.ID)
	eq(ids(MergeRequestFilter{Limit: 1}), c.ID)
	eq(ids(MergeRequestFilter{Limit: 100000}), c.ID, b.ID, a.ID)
	if list, err := ListMergeRequests(ctx, pool, MergeRequestFilter{State: "bogus"}); err != nil || list == nil || len(list) != 0 {
		t.Fatalf("no match = %#v %v", list, err)
	}
}

// Rule 3: the sync list is the open records created after the window, never-synced
// first, then the longest unsynced; the update is conditional on state = open and the
// merge wins once.
func TestMergeRequestSyncUpdates(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, _ := mrProject(t, pool, "t4")
	now := mustParse(t, "2026-10-17T12:00:00Z")
	mk := func(n int64, created time.Time, synced *time.Time, state string) *MergeRequest {
		mr := mrOf(p, mrRun(t, pool, p, nil), n)
		mr.CreatedAt, mr.SyncedAt, mr.State = created, synced, state
		if ok, err := InsertMergeRequest(ctx, pool, mr); err != nil || !ok {
			t.Fatalf("insert %d: %v %v", n, ok, err)
		}
		return mr
	}
	hour := func(h int) *time.Time { v := now.Add(-time.Duration(h) * time.Hour); return &v }
	never := mk(1, now.Add(-time.Hour), nil, MergeRequestOpen)
	older := mk(2, now.Add(-2*time.Hour), hour(5), MergeRequestOpen)
	newer := mk(3, now.Add(-3*time.Hour), hour(1), MergeRequestOpen)
	oldNever := mk(4, now.Add(-5*time.Hour), nil, MergeRequestOpen)
	mk(5, now.Add(-time.Hour), nil, MergeRequestMerged)
	mk(6, now.Add(-15*24*time.Hour), nil, MergeRequestOpen) // outside the 14 days

	list, err := ListMergeRequestsToSync(ctx, pool, now.Add(-14*24*time.Hour), 10)
	mustDo(t, err)
	var got []int64
	for _, mr := range list {
		got = append(got, mr.Number)
	}
	if want := []int64{4, 1, 2, 3}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Fatalf("to sync = %v, want %v", got, want)
	}
	if list, _ := ListMergeRequestsToSync(ctx, pool, now.Add(-14*24*time.Hour), 2); len(list) != 2 {
		t.Fatalf("limit: %d", len(list))
	}
	_, _ = newer, oldNever

	// An update writes the provider's view and moves updated_at only when it changed.
	sha, threads := "abc123", 2
	at := now.Add(time.Minute)
	ok, err := UpdateMergeRequestSync(ctx, pool, never.ID, MergeRequestSync{State: MergeRequestOpen, CIStatus: CIStatusPending,
		HeadSHA: &sha, UnresolvedThreads: &threads, SyncedAt: at})
	if err != nil || !ok {
		t.Fatalf("update = %v %v", ok, err)
	}
	got1, _ := GetMergeRequest(ctx, pool, never.ID)
	if got1.CIStatus != CIStatusPending || *got1.HeadSHA != sha || *got1.UnresolvedThreads != 2 || !got1.SyncedAt.Equal(at) || !got1.UpdatedAt.Equal(at) || got1.ClosedAt != nil {
		t.Fatalf("after update = %+v", got1)
	}
	again := at.Add(time.Minute)
	msg := "github graphql: refused"
	if ok, err := UpdateMergeRequestSync(ctx, pool, never.ID, MergeRequestSync{State: MergeRequestOpen, CIStatus: CIStatusPending,
		HeadSHA: &sha, UnresolvedThreads: &threads, LastSyncError: &msg, SyncedAt: again}); err != nil || !ok {
		t.Fatalf("unchanged update = %v %v", ok, err)
	}
	got2, _ := GetMergeRequest(ctx, pool, never.ID)
	if !got2.SyncedAt.Equal(again) || !got2.UpdatedAt.Equal(at) || got2.LastSyncError == nil || *got2.LastSyncError != msg {
		t.Fatalf("unchanged update moved updated_at or lost the error: %+v", got2)
	}
	// A failed refresh writes the text and the time and nothing else.
	failAt := again.Add(time.Minute)
	if ok, err := UpdateMergeRequestSyncError(ctx, pool, never.ID, "gitlab: HTTP 500", failAt); err != nil || !ok {
		t.Fatalf("error update = %v %v", ok, err)
	}
	got3, _ := GetMergeRequest(ctx, pool, never.ID)
	if got3.CIStatus != CIStatusPending || *got3.LastSyncError != "gitlab: HTTP 500" || !got3.SyncedAt.Equal(failAt) {
		t.Fatalf("after a failed refresh = %+v", got3)
	}

	// The merge and the sync race: the first conditional update wins, the other changes nothing.
	won, err := MarkMergeRequestMerged(ctx, pool, never.ID, "operator", failAt.Add(time.Minute))
	if err != nil || !won {
		t.Fatalf("merge = %v %v", won, err)
	}
	if ok, err := UpdateMergeRequestSync(ctx, pool, never.ID, MergeRequestSync{State: MergeRequestClosed, CIStatus: CIStatusFailed, SyncedAt: now}); err != nil || ok {
		t.Fatalf("a sync after the merge wrote: %v %v", ok, err)
	}
	if ok, err := UpdateMergeRequestSyncError(ctx, pool, never.ID, "late", now); err != nil || ok {
		t.Fatalf("an error after the merge wrote: %v %v", ok, err)
	}
	if ok, err := MarkMergeRequestMerged(ctx, pool, never.ID, "operator", now); err != nil || ok {
		t.Fatalf("a second merge wrote: %v %v", ok, err)
	}
	merged, _ := GetMergeRequest(ctx, pool, never.ID)
	if merged.State != MergeRequestMerged || merged.MergedBy == nil || *merged.MergedBy != "operator" || merged.ClosedAt == nil ||
		!merged.ClosedAt.Equal(failAt.Add(time.Minute)) || merged.LastSyncError != nil || merged.CIStatus != CIStatusPending {
		t.Fatalf("merged = %+v", merged)
	}

	// A refresh that finds the request closed stamps closed_at: the provider's, or the sync's own time.
	closedAt := now.Add(-30 * time.Minute)
	if ok, err := UpdateMergeRequestSync(ctx, pool, older.ID, MergeRequestSync{State: MergeRequestClosed, CIStatus: CIStatusNone, SyncedAt: now, ClosedAt: &closedAt}); err != nil || !ok {
		t.Fatalf("close = %v %v", ok, err)
	}
	if c, _ := GetMergeRequest(ctx, pool, older.ID); c.State != MergeRequestClosed || !c.ClosedAt.Equal(closedAt) {
		t.Fatalf("closed = %+v", c)
	}
	if ok, err := UpdateMergeRequestSync(ctx, pool, newer.ID, MergeRequestSync{State: MergeRequestMerged, CIStatus: CIStatusPassed, SyncedAt: now}); err != nil || !ok {
		t.Fatalf("merge by sync = %v %v", ok, err)
	}
	if c, _ := GetMergeRequest(ctx, pool, newer.ID); c.State != MergeRequestMerged || !c.ClosedAt.Equal(now) || c.MergedBy != nil {
		t.Fatalf("merged by sync = %+v", c)
	}
}

// Rule 4: reported_state is written once and only for the record's own final state; the
// report list holds final, unreported records of runs that report to a conversation.
func TestMergeRequestReported(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, plain := mrProject(t, pool, "t5")
	ref := ConversationRef("conv-1")
	chat := mrRun(t, pool, p, &ref)
	now := mustParse(t, "2026-10-17T12:00:00Z")

	mkFinal := func(run *Run, n int64, state string) *MergeRequest {
		mr := mrOf(p, run, n)
		mr.State, mr.ClosedAt = state, &now
		if ok, err := InsertMergeRequest(ctx, pool, mr); err != nil || !ok {
			t.Fatalf("insert: %v %v", ok, err)
		}
		return mr
	}
	onPlain := mkFinal(plain, 1, MergeRequestMerged)
	onChat := mkFinal(chat, 2, MergeRequestClosed)

	list, err := ListMergeRequestsToReport(ctx, pool, now.Add(-time.Hour), 10)
	mustDo(t, err)
	if len(list) != 1 || list[0].ID != onChat.ID {
		t.Fatalf("to report = %+v", list)
	}
	if list, _ := ListMergeRequestsToReport(ctx, pool, now.Add(time.Hour), 10); len(list) != 0 {
		t.Fatalf("a final state outside the window: %+v", list)
	}
	if ok, err := MarkMergeRequestReported(ctx, pool, onChat.ID, MergeRequestMerged); err != nil || ok {
		t.Fatalf("reported a state the record is not in: %v %v", ok, err)
	}
	if ok, err := MarkMergeRequestReported(ctx, pool, onChat.ID, MergeRequestClosed); err != nil || !ok {
		t.Fatalf("report = %v %v", ok, err)
	}
	if ok, err := MarkMergeRequestReported(ctx, pool, onChat.ID, MergeRequestClosed); err != nil || ok {
		t.Fatalf("a second report = %v %v", ok, err)
	}
	if got, _ := GetMergeRequest(ctx, pool, onChat.ID); got.ReportedState == nil || *got.ReportedState != MergeRequestClosed {
		t.Fatalf("reported_state = %+v", got)
	}
	if list, _ := ListMergeRequestsToReport(ctx, pool, now.Add(-time.Hour), 10); len(list) != 0 {
		t.Fatalf("a reported record is listed: %+v", list)
	}
	_ = onPlain
}

// Rule 6: FillRunMergeRequests sets the record on the runs that have one.
func TestFillRunMergeRequests(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, with := mrProject(t, pool, "t6")
	without := mrRun(t, pool, p, nil)
	if ok, err := InsertMergeRequest(ctx, pool, mrOf(p, with, 3)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	runs := []Run{*with, *without}
	mustDo(t, FillRunMergeRequests(ctx, pool, runs))
	if runs[0].MergeRequest == nil || runs[0].MergeRequest.Number != 3 || runs[1].MergeRequest != nil {
		t.Fatalf("filled = %+v / %+v", runs[0].MergeRequest, runs[1].MergeRequest)
	}
	mustDo(t, FillRunMergeRequests(ctx, pool, nil))
}

// DESIGN 5.29 rule 4: a follow-up is a second record of the run that names the record it
// follows. The run's own record stays the one MergeRequestByRun and FillRunMergeRequests
// answer, a record has at most one follow-up, a run still has one record of its own, and the
// run's listing shows both.
func TestFollowUpMergeRequests(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, run := mrProject(t, pool, "t-follow-up")
	own := mrOf(p, run, 21)
	if ok, err := InsertMergeRequest(ctx, pool, own); err != nil || !ok {
		t.Fatalf("own record: %v %v", ok, err)
	}
	if fu, err := FollowUpMergeRequest(ctx, pool, own.ID); err != nil || fu != nil {
		t.Fatalf("follow-up before one exists = %+v %v", fu, err)
	}
	fu := mrOf(p, run, 22)
	fu.SourceBranch, fu.Title = "yuno/def", "Follow-up: Add a thing"
	if ok, err := InsertFollowUpMergeRequest(ctx, pool, fu, own.ID); err != nil || !ok {
		t.Fatalf("follow-up: %v %v", ok, err)
	}
	if ok, err := InsertFollowUpMergeRequest(ctx, pool, mrOf(p, run, 23), own.ID); err != nil || ok {
		t.Fatalf("a second follow-up of the record = %v %v", ok, err)
	}
	if ok, err := InsertMergeRequest(ctx, pool, mrOf(p, run, 24)); err != nil || ok {
		t.Fatalf("a second record of the run's own = %v %v", ok, err)
	}
	if got, err := FollowUpMergeRequest(ctx, pool, own.ID); err != nil || got == nil || got.ID != fu.ID || got.Number != 22 || got.RunID != run.ID {
		t.Fatalf("follow-up = %+v %v", got, err)
	}
	if by, err := MergeRequestByRun(ctx, pool, run.ID); err != nil || by == nil || by.ID != own.ID {
		t.Fatalf("by run = %+v %v", by, err)
	}
	runs := []Run{*run}
	mustDo(t, FillRunMergeRequests(ctx, pool, runs))
	if runs[0].MergeRequest == nil || runs[0].MergeRequest.ID != own.ID {
		t.Fatalf("filled = %+v", runs[0].MergeRequest)
	}
	// A run that names the follow-up (an iteration on it, DESIGN 5.22) gets the follow-up.
	other := mrRun(t, pool, p, nil)
	other.MergeRequestID = &fu.ID
	runs = []Run{*other, *run}
	mustDo(t, FillRunMergeRequests(ctx, pool, runs))
	if runs[0].MergeRequest == nil || runs[0].MergeRequest.ID != fu.ID || runs[1].MergeRequest == nil || runs[1].MergeRequest.ID != own.ID {
		t.Fatalf("filled = %+v / %+v", runs[0].MergeRequest, runs[1].MergeRequest)
	}
	list, err := ListMergeRequests(ctx, pool, MergeRequestFilter{RunID: run.ID})
	if err != nil || len(list) != 2 {
		t.Fatalf("the run's merge requests = %+v %v", list, err)
	}
}

// T5.20.8: the backfill makes a record of the newest executed call of each run that has a
// project, skips runs without one, failed calls and calls it cannot read, and is
// idempotent.
func TestBackfillMergeRequests(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, run := mrProject(t, pool, "t8")
	older := mustParse(t, "2026-10-15T10:00:00Z")
	newest := mustParse(t, "2026-10-16T10:00:00Z")

	call := func(r *Run, useID, tool, status string, isErr bool, input, output string, at time.Time) {
		t.Helper()
		mustDo(t, InsertToolCall(ctx, pool, &ToolCall{RunID: r.ID, ToolUseID: useID, ToolName: tool, Status: status, IsError: isErr,
			Input: json.RawMessage(input), Output: json.RawMessage(output), StartedAt: &at, FinishedAt: &at}))
	}
	call(run, "a", "open_merge_request", ToolExecuted, false, `{"title":"First try"}`,
		`{"number":1,"url":"https://gitlab.test/group/t8/-/merge_requests/1","branch":"yuno/aaa","provider":"gitlab"}`, older)
	call(run, "b", "yuno__open_merge_request", ToolExecuted, false, `{"title":"Second try"}`,
		`{"number":2,"url":"https://gitlab.test/group/t8/-/merge_requests/2","branch":"yuno/bbb","provider":"gitlab","replayed":true}`, newest)
	call(run, "c", "open_merge_request", ToolFailed, true, `{"title":"Failed"}`, `{"text":"refused"}`, newest.Add(time.Hour))

	// A run without a project: not a record.
	wf := &Workflow{Name: "noproj", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(t, CreateWorkflow(ctx, pool, wf))
	loose := &Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: TriggerManual}
	mustDo(t, CreateRun(ctx, pool, loose))
	call(loose, "d", "open_merge_request", ToolExecuted, false, `{"title":"x"}`,
		`{"number":9,"url":"https://gitlab.test/group/x/-/merge_requests/9","branch":"yuno/ddd","provider":"gitlab"}`, newest)

	// A GitHub run whose output has no number: it comes from the URL; the provider from the output.
	ghRun := mrRun(t, pool, p, nil)
	call(ghRun, "e", "open_merge_request", ToolExecuted, false, `{"title":"Gh"}`,
		`{"url":"https://github.test/owner/demo/pull/12","branch":"yuno/eee","provider":"github"}`, newest)
	// Outputs that cannot make a record: no branch, no number anywhere.
	noBranch := mrRun(t, pool, p, nil)
	call(noBranch, "f", "open_merge_request", ToolExecuted, false, `{}`, `{"number":5,"url":"https://gitlab.test/x/-/merge_requests/5"}`, newest)
	noNumber := mrRun(t, pool, p, nil)
	call(noNumber, "g", "open_merge_request", ToolExecuted, false, `{}`, `{"url":"https://gitlab.test/x/-/merge_requests/","branch":"yuno/ggg"}`, newest)

	n, err := BackfillMergeRequests(ctx, pool)
	if err != nil || n != 2 {
		t.Fatalf("backfill = %d %v, want 2", n, err)
	}
	mr, err := MergeRequestByRun(ctx, pool, run.ID)
	if err != nil || mr == nil {
		t.Fatalf("no record for the run: %v", err)
	}
	if mr.Number != 2 || mr.SourceBranch != "yuno/bbb" || mr.Title != "Second try" || mr.State != MergeRequestOpen || mr.SyncedAt != nil ||
		mr.ProjectID != p.ID || mr.TargetBranch != "main" || mr.Provider != "gitlab" || !mr.CreatedAt.Equal(newest) || mr.CIStatus != CIStatusNone {
		t.Fatalf("record = %+v", mr)
	}
	gh, _ := MergeRequestByRun(ctx, pool, ghRun.ID)
	if gh == nil || gh.Number != 12 || gh.Provider != "github" || gh.URL != "https://github.test/owner/demo/pull/12" {
		t.Fatalf("github record = %+v", gh)
	}
	if got, _ := MergeRequestByRun(ctx, pool, loose.ID); got != nil {
		t.Fatalf("a run without a project got a record: %+v", got)
	}
	for _, r := range []*Run{noBranch, noNumber} {
		if got, _ := MergeRequestByRun(ctx, pool, r.ID); got != nil {
			t.Fatalf("an unreadable call made a record: %+v", got)
		}
	}
	if n, err := BackfillMergeRequests(ctx, pool); err != nil || n != 0 {
		t.Fatalf("second backfill = %d %v, want 0", n, err)
	}
	// A record the sync already settled is left alone by a later start.
	mustDo(t, func() error { _, err := MarkMergeRequestMerged(ctx, pool, mr.ID, "operator", newest); return err }())
	if n, err := BackfillMergeRequests(ctx, pool); err != nil || n != 0 {
		t.Fatalf("backfill over a merged record = %d %v", n, err)
	}
	if got, _ := MergeRequestByRun(ctx, pool, run.ID); got.State != MergeRequestMerged {
		t.Fatalf("the backfill touched a record: %+v", got)
	}
}
