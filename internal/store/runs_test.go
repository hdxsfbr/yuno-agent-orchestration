package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// DESIGN 5.9 rule 2: a schedule occurrence has at most one run, whoever inserts it
// second; other triggers may repeat a reference.
func TestCreateRunOneRunPerScheduleOccurrence(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := &Workflow{Name: "occurrences", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(t, CreateWorkflow(ctx, pool, wf))
	newRun := func(trigger, ref string) *Run {
		return &Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: trigger, TriggerRef: &ref}
	}
	ref := ScheduleOccurrenceRef("sch1", mustParse(t, "2026-10-16T09:00:00Z"))
	if ref != "sch1@2026-10-16T09:00:00Z" || ScheduleIDOfRef(ref) != "sch1" {
		t.Fatalf("ref = %q (schedule %q)", ref, ScheduleIDOfRef(ref))
	}
	first := newRun(TriggerSchedule, ref)
	mustDo(t, CreateRun(ctx, pool, first))
	if err := CreateRun(ctx, pool, newRun(TriggerSchedule, ref)); !errors.Is(err, ErrDuplicateTrigger) {
		t.Fatalf("second run of the occurrence: %v, want ErrDuplicateTrigger", err)
	}
	got, err := GetRunByTrigger(ctx, pool, TriggerSchedule, ref)
	if err != nil || got.ID != first.ID {
		t.Fatalf("run of the occurrence = %+v, %v", got, err)
	}
	if _, err := GetRunByTrigger(ctx, pool, TriggerSchedule, "sch1@2026-10-16T10:00:00Z"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another occurrence: %v, want ErrNotFound", err)
	}
	mustDo(t, CreateRun(ctx, pool, newRun(TriggerSchedule, "sch1@2026-10-16T10:00:00Z")))
	mustDo(t, CreateRun(ctx, pool, newRun(TriggerEval, "case-1")))
	mustDo(t, CreateRun(ctx, pool, newRun(TriggerEval, "case-1")))
}

// Migration 0007 on a database with runs from before it: a cron schedule's earlier runs
// all carry the bare schedule id, which the unique index would refuse. The migration
// rewrites them to "<schedule id>@<run id>" and then builds the index.
func TestMigration0007RewritesLegacyScheduleRefs(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	const index = `CREATE UNIQUE INDEX IF NOT EXISTS runs_schedule_occurrence_idx ON runs (trigger, trigger_ref) WHERE trigger = 'schedule'`
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `TRUNCATE runs CASCADE`)
		_, _ = pool.Exec(ctx, index)
	})
	mustDo(t, execSQL(ctx, pool, `DROP INDEX runs_schedule_occurrence_idx`))
	wf := &Workflow{Name: "legacy", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(t, CreateWorkflow(ctx, pool, wf))
	var legacy []*Run
	for _, tr := range []struct{ trigger, ref string }{{TriggerSchedule, "sch1"}, {TriggerSchedule, "sch1"}, {TriggerEval, "case-1"}} {
		ref := tr.ref
		r := &Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: tr.trigger, TriggerRef: &ref}
		mustDo(t, CreateRun(ctx, pool, r))
		legacy = append(legacy, r)
	}
	sql, err := migrationsFS.ReadFile("migrations/0007_schedule_occurrences.sql")
	mustDo(t, err)
	mustDo(t, execSQL(ctx, pool, string(sql)))
	for i, want := range []string{"sch1@" + legacy[0].ID, "sch1@" + legacy[1].ID, "case-1"} {
		got, err := GetRun(ctx, pool, legacy[i].ID)
		if err != nil || got.TriggerRef == nil || *got.TriggerRef != want {
			t.Fatalf("run %d trigger_ref = %v (%v), want %s", i, got.TriggerRef, err, want)
		}
	}
	if ScheduleIDOfRef("sch1@"+legacy[0].ID) != "sch1" {
		t.Fatal("a rewritten reference no longer names its schedule")
	}
	dup := "sch1@" + legacy[0].ID
	if err := CreateRun(ctx, pool, &Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition,
		Trigger: TriggerSchedule, TriggerRef: &dup}); !errors.Is(err, ErrDuplicateTrigger) {
		t.Fatalf("after the migration the index refuses a duplicate: %v", err)
	}
}

func execSQL(ctx context.Context, q Querier, sql string) error {
	_, err := q.Exec(ctx, sql)
	return err
}

// ---- retry from the failed step (DESIGN 5.21) -------------------------------------------

// endedRun stores a run in the given status the way the engine leaves a run that ended
// there: stopped at a node, with counters, a reason, a finish time, a cancel flag, a
// pause, a report claim and a start time, all of which a retry has to clear or keep.
func endedRun(t *testing.T, status string) *Run {
	t.Helper()
	ctx := context.Background()
	pool := testPool
	wf := &Workflow{Name: "retry-" + status, Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(t, CreateWorkflow(ctx, pool, wf))
	r := &Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: TriggerManual,
		Limits: Limits{MaxSteps: 40, MaxCostUSD: 2}}
	mustDo(t, CreateRun(ctx, pool, r))
	mustDo(t, execSQL(ctx, pool, `UPDATE runs SET status='`+status+`', current_node='review',
		state='{"code":{"ok":true}}', step_count=40, tool_call_count=7, cost_usd_micros=1500000, attempt=2, retries=1,
		failure_reason='step limit 40 exceeded', finished_at=now(), cancel_requested=true, cancel_by='operator',
		resume_node_visit_id='rn-1', reported_at=now(), started_at=now() - interval '10 minutes', paused_at=now(),
		paused_total_ms=5000 WHERE id='`+r.ID+`'`))
	got, err := GetRun(ctx, pool, r.ID)
	mustDo(t, err)
	return got
}

// DESIGN 5.21 rule 3: the retry is one update, conditional on the status; it clears what
// ended the run, keeps what the run did, and counts.
func TestRetryRunClearsWhatEndedTheRunAndKeepsWhatItDid(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	at := mustParse(t, "2026-10-17T12:00:00Z")
	for _, status := range []string{RunFailed, RunTimedOut, RunCancelled} {
		t.Run(status, func(t *testing.T) {
			before := endedRun(t, status)
			got, err := RetryRun(ctx, pool, before.ID, nil, at)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != RunQueued || got.Retries != 2 || got.Attempt != 1 {
				t.Fatalf("status=%s retries=%d attempt=%d", got.Status, got.Retries, got.Attempt)
			}
			if got.FailureReason != nil || got.FinishedAt != nil || got.CancelRequested || got.CancelBy != nil ||
				got.ResumeNodeVisitID != nil || got.ReportedAt != nil {
				t.Fatalf("what ended the run is still there: %+v", got)
			}
			if got.StartedAt != nil || got.PausedAt != nil || got.PausedTotalMs != 0 {
				t.Fatalf("the time limit does not restart: started=%v paused=%v/%d", got.StartedAt, got.PausedAt, got.PausedTotalMs)
			}
			node := ""
			if got.CurrentNode != nil {
				node = *got.CurrentNode
			}
			if node != "review" || string(got.State) != `{"code": {"ok": true}}` {
				t.Fatalf("current_node=%q state=%s", node, got.State)
			}
			if got.StepCount != 40 || got.ToolCallCount != 7 || got.CostUSDMicros != 1500000 {
				t.Fatalf("counters = %d/%d/%d", got.StepCount, got.ToolCallCount, got.CostUSDMicros)
			}
			if got.Limits != before.Limits {
				t.Fatalf("limits changed without being given: %+v", got.Limits)
			}
			if !got.UpdatedAt.Equal(at) {
				t.Fatalf("updated_at = %v", got.UpdatedAt)
			}
			again, err := GetRun(ctx, pool, before.ID)
			if err != nil || again.Status != RunQueued || again.Retries != 2 {
				t.Fatalf("stored run = %+v, %v", again, err)
			}
		})
	}
}

// DESIGN 5.21 rule 3: given limits replace the run's.
func TestRetryRunReplacesGivenLimits(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	before := endedRun(t, RunFailed)
	raised := Limits{MaxSteps: 80, MaxDurationSeconds: 900, MaxCostUSD: 4}
	got, err := RetryRun(ctx, pool, before.ID, &raised, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.Limits != raised {
		t.Fatalf("limits = %+v, want %+v", got.Limits, raised)
	}
}

// DESIGN 5.21 rule 3: WHERE status IN ('failed', 'timed_out', 'cancelled'). Any other
// status, and a run that does not exist, is ErrNotFound and nothing changes.
func TestRetryRunIsConditionalOnStatus(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	for _, status := range []string{RunQueued, RunRunning, RunWaitingApproval, RunSucceeded} {
		t.Run(status, func(t *testing.T) {
			before := endedRun(t, status)
			if _, err := RetryRun(ctx, pool, before.ID, &Limits{MaxSteps: 99}, time.Now()); !errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
			after, err := GetRun(ctx, pool, before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != status || after.Retries != 1 || after.Attempt != 2 || after.Limits != before.Limits ||
				after.FailureReason == nil || !after.CancelRequested {
				t.Fatalf("run changed: %+v", after)
			}
		})
	}
	if _, err := RetryRun(ctx, pool, "no-such-run", nil, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
}

// DESIGN 5.21 rule 3, T5.21.7 at the store: of two retries at once only one finds the run
// in a retryable status.
func TestRetryRunTwoAtOnceOneWins(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	run := endedRun(t, RunTimedOut)
	var wg sync.WaitGroup
	results := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = RetryRun(ctx, pool, run.ID, nil, time.Now())
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range results {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, ErrNotFound):
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if got, _ := GetRun(ctx, pool, run.ID); won != 1 || got.Retries != 2 {
		t.Fatalf("%d retries won; run retries = %d", won, got.Retries)
	}
}

// DESIGN 5.1 rule 8 and 5.21 rule 3: attempt goes back to 1, so the crash re-queue
// (attempt < 2) applies once per retry.
func TestRetryRunResetsTheCrashRequeueBudget(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	run := endedRun(t, RunFailed) // attempt 2: the stale sweeper would fail it on sight
	if _, err := RetryRun(ctx, pool, run.ID, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	claimed, err := ClaimQueuedRun(ctx, pool)
	if err != nil || claimed.ID != run.ID {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	mustDo(t, execSQL(ctx, pool, `UPDATE runs SET updated_at = now() - interval '1 hour' WHERE id='`+run.ID+`'`))
	requeued, failed, err := RequeueStaleRuns(ctx, pool, time.Minute)
	if err != nil || requeued != 1 || failed != 0 {
		t.Fatalf("stale sweep: requeued=%d failed=%d err=%v", requeued, failed, err)
	}
	if got, _ := GetRun(ctx, pool, run.ID); got.Status != RunQueued || got.Attempt != 2 || got.Retries != 2 {
		t.Fatalf("run = %s attempt=%d retries=%d", got.Status, got.Attempt, got.Retries)
	}
}

// DESIGN 5.21: the check and the update are one transaction with the row locked, so a
// second reader of the row waits for the first transaction to end.
func TestGetRunForUpdateLocksTheRow(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	run := endedRun(t, RunFailed)
	if _, err := GetRunForUpdate(ctx, pool, "no-such-run"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
	tx, err := pool.Begin(ctx)
	mustDo(t, err)
	defer tx.Rollback(ctx)
	first, err := GetRunForUpdate(ctx, tx, run.ID)
	if err != nil || first.ID != run.ID || first.Status != RunFailed {
		t.Fatalf("locked read = %+v, %v", first, err)
	}
	second := make(chan error, 1)
	go func() {
		tx2, err := pool.Begin(ctx)
		if err != nil {
			second <- err
			return
		}
		defer tx2.Rollback(ctx)
		_, err = GetRunForUpdate(ctx, tx2, run.ID)
		second <- err
	}()
	select {
	case err := <-second:
		t.Fatalf("a second locking read did not wait for the first transaction (%v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	mustDo(t, tx.Commit(ctx))
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second locking read never got the row")
	}
}

// DESIGN 5.21 rule 2: a first claim stamps started_at; a retried run's claim leaves it
// empty for the engine to stamp from its clock (runRun), and a retried run claimed again
// later (an approval, a crash re-queue) keeps the start it has.
func TestClaimQueuedRunStartedAtOfRetriedRuns(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := &Workflow{Name: "claims", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(t, CreateWorkflow(ctx, pool, wf))
	claim := func(retries int, startedAt string) *Run {
		t.Helper()
		mustDo(t, execSQL(ctx, pool, `TRUNCATE runs CASCADE`))
		r := &Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: TriggerManual}
		mustDo(t, CreateRun(ctx, pool, r))
		mustDo(t, execSQL(ctx, pool, `UPDATE runs SET retries=`+itoa(retries)+`, started_at=`+startedAt+` WHERE id='`+r.ID+`'`))
		got, err := ClaimQueuedRun(ctx, pool)
		mustDo(t, err)
		return got
	}
	if got := claim(0, "NULL"); got.Status != RunRunning || got.StartedAt == nil {
		t.Fatalf("first claim: status=%s started_at=%v", got.Status, got.StartedAt)
	}
	if got := claim(2, "NULL"); got.Status != RunRunning || got.StartedAt != nil {
		t.Fatalf("claim after a retry: status=%s started_at=%v, want it empty", got.Status, got.StartedAt)
	}
	if got := claim(2, "now() - interval '5 minutes'"); got.StartedAt == nil || time.Since(*got.StartedAt) < 4*time.Minute {
		t.Fatalf("claim after a retry, already started: started_at=%v", got.StartedAt)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
