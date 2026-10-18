package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Wave 10 (DESIGN 5.24): the queries behind a question's count, its answer, the park of a
// visit on it, the re-queue its decision makes, and the continuation visit it creates.

// qFixture is a run with its coder step, and helpers to add visits and questions.
type qFixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	run  *Run
	at   time.Time
	n    int
}

func newQFixture(t *testing.T) *qFixture {
	t.Helper()
	pool := needDB(t)
	return &qFixture{t: t, pool: pool, run: w9MustRun(t, pool, w9Workflow(t, pool), nil), at: time.Now().UTC().Truncate(time.Microsecond)}
}

// tick returns a strictly later time on every call, so ordering by requested_at is exact.
func (f *qFixture) tick() time.Time {
	f.n++
	return f.at.Add(time.Duration(f.n) * time.Second)
}

func (f *qFixture) visit(node string, visit int, status string, parked bool, question *string) *RunNode {
	f.t.Helper()
	started := f.tick()
	n := &RunNode{RunID: f.run.ID, NodeID: node, Visit: visit, Status: status, StartedAt: &started, QuestionID: question}
	if parked {
		p := started
		n.ParkedAt = &p
	}
	mustDo(f.t, CreateRunNode(context.Background(), f.pool, n))
	return n
}

// question inserts a question of the visit with the status given; an approved one carries
// an answer.
func (f *qFixture) question(rn *RunNode, kind, status, text string) *Approval {
	f.t.Helper()
	at := f.tick()
	a := &Approval{RunID: f.run.ID, RunNodeID: rn.ID, Kind: kind, NodeID: rn.NodeID, Message: text, Status: status,
		RequestedAt: at, ExpiresAt: at.Add(time.Hour)}
	if status == ApprovalApproved {
		ans := "answer to " + text
		a.Answer = &ans
	}
	if status != ApprovalPending {
		decided, by := at.Add(time.Second), "api"
		a.DecidedAt, a.DecidedBy = &decided, &by
	}
	mustDo(f.t, InsertApproval(context.Background(), f.pool, a))
	return a
}

// T5.24.9 (the store's side): the step's count is every question of the run and node, any
// status; other kinds, other nodes and other runs do not count. The decided ones, oldest
// first, are what the answer paragraph lists.
func TestCountAndListStepQuestions(t *testing.T) {
	f := newQFixture(t)
	ctx := context.Background()
	one := f.visit("coder", 1, NodeRunning, false, nil)
	two := f.visit("coder", 2, NodeRunning, false, nil)
	other := f.visit("reviewer", 1, NodeRunning, false, nil)
	q1 := f.question(one, ApprovalKindQuestion, ApprovalApproved, "first")
	f.question(one, ApprovalKindTool, ApprovalApproved, "a tool approval")
	q2 := f.question(one, ApprovalKindQuestion, ApprovalExpired, "second")
	f.question(other, ApprovalKindQuestion, ApprovalPending, "another step's")
	q3 := f.question(two, ApprovalKindQuestion, ApprovalPending, "third")
	elsewhere := w9MustRun(t, f.pool, w9Workflow(t, f.pool), nil)
	rn := &RunNode{RunID: elsewhere.ID, NodeID: "coder", Status: NodeRunning}
	mustDo(t, CreateRunNode(ctx, f.pool, rn))
	mustDo(t, InsertApproval(ctx, f.pool, &Approval{RunID: elsewhere.ID, RunNodeID: rn.ID, Kind: ApprovalKindQuestion, NodeID: "coder",
		Message: "another run's", RequestedAt: f.at, ExpiresAt: f.at.Add(time.Hour)}))

	if n, err := CountStepQuestions(ctx, f.pool, f.run.ID, "coder"); err != nil || n != 3 {
		t.Fatalf("count = %d, %v; want 3 (any status, both visits)", n, err)
	}
	if n, err := CountStepQuestions(ctx, f.pool, f.run.ID, "reviewer"); err != nil || n != 1 {
		t.Fatalf("count of another step = %d, %v", n, err)
	}
	if n, err := CountStepQuestions(ctx, f.pool, f.run.ID, "nobody"); err != nil || n != 0 {
		t.Fatalf("count of a step that asked nothing = %d, %v", n, err)
	}

	listed, err := ListStepQuestions(ctx, f.pool, f.run.ID, "coder")
	if err != nil || len(listed) != 2 || listed[0].ID != q1.ID || listed[1].ID != q2.ID {
		t.Fatalf("decided questions = %+v, %v; want the answered and the expired one, oldest first (not %s)", listed, err, q3.ID)
	}
	if listed[0].Answer == nil || *listed[0].Answer != "answer to first" {
		t.Fatalf("the answer is not carried: %+v", listed[0])
	}
	if none, err := ListStepQuestions(ctx, f.pool, f.run.ID, "nobody"); err != nil || none == nil || len(none) != 0 {
		t.Fatalf("a step with no questions lists %#v, %v; want an empty list", none, err)
	}
}

// 5.24 rule 3: AnswerApprovalAt moves a pending question to approved with the answer, the
// decider and the time; anything else is ErrNotFound and changes nothing.
func TestAnswerApprovalAt(t *testing.T) {
	f := newQFixture(t)
	ctx := context.Background()
	rn := f.visit("coder", 1, NodeRunning, false, nil)
	pending := f.question(rn, ApprovalKindQuestion, ApprovalPending, "which?")
	tool := f.question(rn, ApprovalKindTool, ApprovalPending, "deploy")
	declined := f.question(rn, ApprovalKindQuestion, ApprovalRejected, "declined one")
	at := f.tick()

	got, err := AnswerApprovalAt(ctx, f.pool, pending.ID, "Portuguese", "chat:Chief of Staff", at)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ApprovalApproved || got.Answer == nil || *got.Answer != "Portuguese" || got.DecidedBy == nil ||
		*got.DecidedBy != "chat:Chief of Staff" || got.DecidedAt == nil || !got.DecidedAt.Equal(at) || got.Reason != nil {
		t.Fatalf("answered question = %+v", got)
	}
	if _, err := AnswerApprovalAt(ctx, f.pool, pending.ID, "second", "api", at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second answer: %v; want ErrNotFound", err)
	}
	if _, err := AnswerApprovalAt(ctx, f.pool, tool.ID, "yes", "api", at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("answer to a tool approval: %v; want ErrNotFound", err)
	}
	if _, err := AnswerApprovalAt(ctx, f.pool, declined.ID, "late", "api", at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("answer to a declined question: %v; want ErrNotFound", err)
	}
	if _, err := AnswerApprovalAt(ctx, f.pool, "no-such-id", "x", "api", at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("answer to an unknown id: %v; want ErrNotFound", err)
	}
	if cur, err := GetApproval(ctx, f.pool, tool.ID); err != nil || cur.Status != ApprovalPending || cur.Answer != nil {
		t.Fatalf("the refused tool approval changed: %+v, %v", cur, err)
	}
	if cur, err := GetApproval(ctx, f.pool, pending.ID); err != nil || *cur.Answer != "Portuguese" {
		t.Fatalf("the second answer replaced the first: %+v, %v", cur, err)
	}
}

// 5.24 rule 5 and Consistency: the park is one transaction; RequeueParkedRun re-queues a
// run only when it waits on a visit that parked, adds the paused interval, closes the
// parked visit's row in the same statement, and is idempotent. A visit that waits in its
// turn (not parked) is never re-queued.
func TestParkVisitAndRequeueParkedRun(t *testing.T) {
	f := newQFixture(t)
	ctx := context.Background()
	rn := f.visit("coder", 1, NodeRunning, false, nil)
	mustDo(t, execSQL(ctx, f.pool, `UPDATE runs SET status='running' WHERE id='`+f.run.ID+`'`))

	// Paused in its turn (the wait of rule 2): waiting_approval at the visit, but not parked.
	paused := f.tick()
	if _, err := PauseRunAt(ctx, f.pool, f.run.ID, rn.ID, paused); err != nil {
		t.Fatal(err)
	}
	if parked, err := IsParkedOn(ctx, f.pool, f.run.ID, rn.ID); err != nil || parked {
		t.Fatalf("IsParkedOn while waiting in the turn = %v, %v", parked, err)
	}
	if _, err := RequeueParkedRun(ctx, f.pool, f.run.ID, rn.ID, f.tick()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("re-queue of a visit that has not parked: %v; want ErrNotFound", err)
	}
	if _, err := ResumeRunAt(ctx, f.pool, f.run.ID, f.tick()); err != nil {
		t.Fatal(err)
	}

	// The park.
	parkedAt := f.tick()
	run, err := ParkVisit(ctx, f.pool, f.run.ID, rn.ID, parkedAt)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunWaitingApproval || run.ResumeNodeVisitID == nil || *run.ResumeNodeVisitID != rn.ID ||
		run.PausedAt == nil || !run.PausedAt.Equal(parkedAt) {
		t.Fatalf("parked run = %+v", run)
	}
	node, err := GetRunNode(ctx, f.pool, rn.ID)
	mustDo(t, err)
	if node.Status != NodeWaitingApproval || node.ParkedAt == nil || !node.ParkedAt.Equal(parkedAt) || node.FinishedAt != nil {
		t.Fatalf("parked visit = %+v", node)
	}
	if parked, err := IsParkedOn(ctx, f.pool, f.run.ID, rn.ID); err != nil || !parked {
		t.Fatalf("IsParkedOn after the park = %v, %v", parked, err)
	}
	if parked, _ := IsParkedOn(ctx, f.pool, f.run.ID, "another-visit"); parked {
		t.Fatal("IsParkedOn is true for a visit the run does not wait on")
	}

	// A save of the visit's in-memory state does not undo what the decision did.
	before, err := GetRun(ctx, f.pool, f.run.ID)
	mustDo(t, err)
	requeuedAt := parkedAt.Add(7 * time.Second)
	requeued, err := RequeueParkedRun(ctx, f.pool, f.run.ID, rn.ID, requeuedAt)
	if err != nil {
		t.Fatal(err)
	}
	if requeued.Status != RunQueued || requeued.PausedAt != nil || requeued.PausedTotalMs != before.PausedTotalMs+7000 {
		t.Fatalf("re-queued run = %s paused_at %v paused_total %d (was %d); want queued, +7000 ms", requeued.Status,
			requeued.PausedAt, requeued.PausedTotalMs, before.PausedTotalMs)
	}
	node, err = GetRunNode(ctx, f.pool, rn.ID)
	mustDo(t, err)
	if node.Status != NodeSucceeded || node.FinishedAt == nil || !node.FinishedAt.Equal(requeuedAt) || node.ParkedAt == nil {
		t.Fatalf("parked visit after the decision = %+v; want succeeded, finished at the re-queue, still marked parked", node)
	}
	if string(node.Output) != "null" && len(node.Output) != 0 {
		t.Fatalf("the parked visit has an output: %s", node.Output)
	}
	if _, err := RequeueParkedRun(ctx, f.pool, f.run.ID, rn.ID, requeuedAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second re-queue: %v; want ErrNotFound (idempotent)", err)
	}

	// The visit goroutine that parked it may still save what it holds: the closed row stays closed.
	stale := *node
	stale.Status, stale.FinishedAt = NodeWaitingApproval, nil
	mustDo(t, UpdateRunNodeProgress(ctx, f.pool, &stale))
	if node, err = GetRunNode(ctx, f.pool, rn.ID); err != nil || node.Status != NodeSucceeded || node.FinishedAt == nil {
		t.Fatalf("a late save reopened the parked visit: %+v, %v", node, err)
	}
	// An ordinary visit still saves its status.
	plain := f.visit("reviewer", 1, NodeRunning, false, nil)
	plain.Status = NodeWaitingApproval
	mustDo(t, UpdateRunNodeProgress(ctx, f.pool, plain))
	if got, _ := GetRunNode(ctx, f.pool, plain.ID); got.Status != NodeWaitingApproval {
		t.Fatalf("a visit that never parked did not save its status: %+v", got)
	}
}

// ParkVisit refuses a run that is not running or waiting, and a visit that has ended, and
// stores nothing then.
func TestParkVisitRefusals(t *testing.T) {
	f := newQFixture(t)
	ctx := context.Background()
	rn := f.visit("coder", 1, NodeRunning, false, nil)
	for _, status := range []string{RunQueued, RunFailed, RunCancelled} {
		mustDo(t, execSQL(ctx, f.pool, `UPDATE runs SET status='`+status+`' WHERE id='`+f.run.ID+`'`))
		if _, err := ParkVisit(ctx, f.pool, f.run.ID, rn.ID, f.tick()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("park of a %s run: %v; want ErrNotFound", status, err)
		}
		node, err := GetRunNode(ctx, f.pool, rn.ID)
		mustDo(t, err)
		if node.ParkedAt != nil || node.Status != NodeRunning {
			t.Fatalf("the failed park (%s run) left the visit changed: %+v", status, node)
		}
	}
	mustDo(t, execSQL(ctx, f.pool, `UPDATE runs SET status='running' WHERE id='`+f.run.ID+`'`))
	done := f.visit("coder", 2, NodeFailed, false, nil)
	if _, err := ParkVisit(ctx, f.pool, f.run.ID, done.ID, f.tick()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("park of a finished visit: %v; want ErrNotFound", err)
	}
	if _, err := ParkVisit(ctx, f.pool, f.run.ID, rn.ID, f.tick()); err != nil {
		t.Fatalf("park of a running visit of a running run: %v", err)
	}
}

// 5.24 rule 5: the continuation question is the newest answered or declined question the
// parked visit stopped on that no visit names; never an expired one, one the visit went on
// past, or one a visit already continues.
func TestContinuationQuestion(t *testing.T) {
	f := newQFixture(t)
	ctx := context.Background()
	none := func(what string) {
		t.Helper()
		if q, err := ContinuationQuestion(ctx, f.pool, f.run.ID, "coder"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: continuation = %+v, %v; want ErrNotFound", what, q, err)
		}
	}
	want := func(what string, q *Approval) {
		t.Helper()
		got, err := ContinuationQuestion(ctx, f.pool, f.run.ID, "coder")
		if err != nil || got.ID != q.ID {
			t.Fatalf("%s: continuation = %+v, %v; want %s", what, got, err, q.ID)
		}
	}
	none("a step that asked nothing")

	// Visit 1 asked q0 and was answered within the wait, then parked on q1.
	one := f.visit("coder", 1, NodeWaitingApproval, true, nil)
	q0 := f.question(one, ApprovalKindQuestion, ApprovalApproved, "within the wait")
	q1 := f.question(one, ApprovalKindQuestion, ApprovalPending, "parked on")
	none("a question still pending")
	mustDo(t, execSQL(ctx, f.pool, `UPDATE approvals SET status='expired' WHERE id='`+q1.ID+`'`))
	none("an expired question (a retry after rule 8 continues nothing)")
	mustDo(t, execSQL(ctx, f.pool, `UPDATE approvals SET status='approved', answer='B' WHERE id='`+q1.ID+`'`))
	want("the answered question the visit parked on, not the one it went on past (q0)", q1)

	// Declined counts the same (D-059).
	mustDo(t, execSQL(ctx, f.pool, `UPDATE approvals SET status='rejected', answer=NULL WHERE id='`+q1.ID+`'`))
	want("a declined question", q1)

	// Visit 2 continues it: nothing is left to continue, for a loop revisit or a retry.
	f.visit("coder", 2, NodeSucceeded, false, &q1.ID)
	none("the question is named by visit 2")

	// A question of a visit that never parked is not a continuation (the restart case of
	// rule 5 (d): the new visit carries the paragraph but no question_id).
	three := f.visit("coder", 3, NodeRunning, false, nil)
	f.question(three, ApprovalKindQuestion, ApprovalApproved, "never parked")
	none("a question of a visit that never parked")

	// Another step and another run have their own.
	other := f.visit("reviewer", 1, NodeWaitingApproval, true, nil)
	rq := f.question(other, ApprovalKindQuestion, ApprovalApproved, "reviewer's")
	if got, err := ContinuationQuestion(ctx, f.pool, f.run.ID, "reviewer"); err != nil || got.ID != rq.ID {
		t.Fatalf("reviewer's continuation = %+v, %v", got, err)
	}
	if _, err := ContinuationQuestion(ctx, f.pool, "another-run", "coder"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another run: %v", err)
	}
	_ = q0
}

// 5.24 rule 5: the loop limit counts the visits that continue no question; the visit number
// counts them all.
func TestCountNodeLoopVisits(t *testing.T) {
	f := newQFixture(t)
	ctx := context.Background()
	one := f.visit("coder", 1, NodeSucceeded, true, nil)
	q := f.question(one, ApprovalKindQuestion, ApprovalApproved, "which?")
	f.visit("coder", 2, NodeSucceeded, false, &q.ID)
	f.visit("coder", 3, NodeRunning, false, nil)
	f.visit("reviewer", 1, NodeSucceeded, false, nil)

	loop, err := CountNodeLoopVisits(ctx, f.pool, f.run.ID, "coder")
	if err != nil || loop != 2 {
		t.Fatalf("loop visits = %d, %v; want 2 (the continuation is free)", loop, err)
	}
	all, err := CountNodeVisits(ctx, f.pool, f.run.ID, "coder")
	if err != nil || all != 3 {
		t.Fatalf("visits = %d, %v; want 3", all, err)
	}
	if n, err := CountNodeLoopVisits(ctx, f.pool, f.run.ID, "reviewer"); err != nil || n != 1 {
		t.Fatalf("reviewer = %d, %v", n, err)
	}
}

// The sweep's list: the newest question of the visit a run waits on, when it is decided.
// A pending question, a run that waits on something else, and a visit that never parked
// are not listed.
func TestListParkedQuestions(t *testing.T) {
	f := newQFixture(t)
	ctx := context.Background()
	rn := f.visit("coder", 1, NodeRunning, false, nil)
	mustDo(t, execSQL(ctx, f.pool, `UPDATE runs SET status='running' WHERE id='`+f.run.ID+`'`))
	older := f.question(rn, ApprovalKindQuestion, ApprovalApproved, "answered within the wait")
	newest := f.question(rn, ApprovalKindQuestion, ApprovalPending, "parked on")

	listed := func() []Approval {
		t.Helper()
		got, err := ListParkedQuestions(ctx, f.pool)
		mustDo(t, err)
		return got
	}
	if got := listed(); len(got) != 0 {
		t.Fatalf("a run that is not waiting lists %+v", got)
	}
	if _, err := ParkVisit(ctx, f.pool, f.run.ID, rn.ID, f.tick()); err != nil {
		t.Fatal(err)
	}
	if got := listed(); len(got) != 0 {
		t.Fatalf("a parked visit with a pending question lists %+v (the older answered one is not its newest)", got)
	}
	mustDo(t, execSQL(ctx, f.pool, `UPDATE approvals SET status='approved', answer='A', decided_at=now() WHERE id='`+newest.ID+`'`))
	if got := listed(); len(got) != 1 || got[0].ID != newest.ID || got[0].Answer == nil {
		t.Fatalf("listed = %+v; want the newest question %s, not %s", got, newest.ID, older.ID)
	}
	mustDo(t, execSQL(ctx, f.pool, `UPDATE approvals SET status='expired', answer=NULL WHERE id='`+newest.ID+`'`))
	if got := listed(); len(got) != 1 || got[0].Status != ApprovalExpired {
		t.Fatalf("an expired question is listed so the sweep can fail the run: %+v", got)
	}
	if _, err := RequeueParkedRun(ctx, f.pool, f.run.ID, rn.ID, f.tick()); err != nil {
		t.Fatal(err)
	}
	if got := listed(); len(got) != 0 {
		t.Fatalf("a re-queued run lists %+v", got)
	}
}

// The options of a question reach the engine as the JSON array the row holds.
func TestQuestionOptionsSurviveTheQueries(t *testing.T) {
	f := newQFixture(t)
	ctx := context.Background()
	rn := f.visit("coder", 1, NodeRunning, false, nil)
	at := f.tick()
	q := &Approval{RunID: f.run.ID, RunNodeID: rn.ID, Kind: ApprovalKindQuestion, NodeID: "coder", Message: "Which?",
		Options: json.RawMessage(`["a","b"]`), RequestedAt: at, ExpiresAt: at.Add(time.Hour)}
	mustDo(t, InsertApproval(ctx, f.pool, q))
	if _, err := AnswerApprovalAt(ctx, f.pool, q.ID, "c", "api", f.tick()); err != nil {
		t.Fatal(err)
	}
	listed, err := ListStepQuestions(ctx, f.pool, f.run.ID, "coder")
	if err != nil || len(listed) != 1 || string(listed[0].Options) != `["a", "b"]` && string(listed[0].Options) != `["a","b"]` {
		t.Fatalf("listed = %+v, %v", listed, err)
	}
}
