package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
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
