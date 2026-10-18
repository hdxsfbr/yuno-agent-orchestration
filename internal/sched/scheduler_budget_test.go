package sched

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"yuno/internal/engine"
	"yuno/internal/store"
)

// T5.19.3 (schedule): a firing whose project has spent its cap is refused by the real
// engine's budget check; the schedule records the text in last_error and advances, as it
// does for a rate limit, and fires normally once the window has moved on.
func TestTickRefusedByProjectBudget(t *testing.T) {
	f := newFixture(t, at(t, "2001-12-02T09:00:10Z"))
	f.s.Engine = engine.New(engine.Deps{Pool: f.pool, Cfg: engineConfig(), Clock: f.clock})
	ctx := context.Background()
	spendWf := f.wf // the fixture's plain workflow carries the run that spent the budget
	f.wf = f.devPipeline()
	p := f.newProject()
	day := store.USDMicros(1_000_000)
	p.BudgetDayUSDMicros = &day
	if err := store.UpdateProject(ctx, f.pool, p); err != nil {
		t.Fatal(err)
	}
	f.project, f.input = &p.ID, json.RawMessage(`{"task":"add a greeting endpoint"}`)
	s := f.schedule(store.ScheduleCron, "0 9 * * *", "UTC", at(t, "2001-12-02T09:00:00Z"))

	// A run created earlier in the same UTC day spent the whole day cap.
	settings := p.Settings()
	spent := &store.Run{WorkflowID: spendWf.ID, WorkflowVersion: 1, WorkflowSnapshot: spendWf.Definition, Trigger: store.TriggerManual,
		ProjectID: &p.ID, Project: &settings}
	if err := store.CreateRun(ctx, f.pool, spent); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE runs SET created_at=$2, cost_usd_micros=$3 WHERE id=$1`, spent.ID, at(t, "2001-12-02T03:00:00Z"), 1_000_000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(ctx, `DELETE FROM runs WHERE id = $1 OR workflow_id = $2`, spent.ID, f.wf.ID)
	})

	if fired := f.tick(); len(fired) != 0 {
		t.Fatalf("fired on a project at its cap: %+v", fired)
	}
	got := f.get(s.ID)
	want := "project " + p.Name + " has reached its daily budget ($1.00); new runs are refused until 2001-12-03T00:00:00Z"
	if got.LastError == nil || *got.LastError != want {
		t.Fatalf("last_error = %v, want %q", got.LastError, want)
	}
	if !got.Enabled || got.NextRunAt == nil || !got.NextRunAt.Equal(at(t, "2001-12-03T09:00:00Z")) || got.LastRunID != nil {
		t.Fatalf("enabled %v next %v last run %v: the schedule must advance without a run", got.Enabled, got.NextRunAt, got.LastRunID)
	}
	var runs int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE workflow_id = $1`, f.wf.ID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 || len(f.events) != 0 {
		t.Fatalf("runs = %d, events = %d for a refused firing", runs, len(f.events))
	}

	// The next occurrence is in a new UTC day, whose spend is nothing: it fires and
	// clears the error.
	f.clock.Advance(24 * time.Hour)
	if fired := f.tick(); len(fired) != 1 {
		t.Fatalf("next day fired %d, want 1", len(fired))
	}
	if got := f.get(s.ID); got.LastError != nil || got.LastRunID == nil {
		t.Fatalf("after the window moved on: error %v last run %v", got.LastError, got.LastRunID)
	}
}
