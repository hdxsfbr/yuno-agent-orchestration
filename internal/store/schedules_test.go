package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// NextScheduleDue is the earliest next run of the enabled schedules (the Status page's
// "next due"), nil when none is enabled.
func TestNextScheduleDue(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	if next, err := NextScheduleDue(ctx, pool); err != nil || next != nil {
		t.Fatalf("no schedules: %v, %v", next, err)
	}
	wf := &Workflow{Name: "due", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(t, CreateWorkflow(ctx, pool, wf))
	add := func(at string, enabled bool) {
		t.Helper()
		next := mustParse(t, at)
		expr := "0 9 * * *"
		mustDo(t, CreateSchedule(ctx, pool, &Schedule{WorkflowID: wf.ID, Kind: ScheduleCron, CronExpr: &expr, TZ: "UTC",
			Enabled: enabled, NextRunAt: &next}))
	}
	add("2026-10-17T09:00:00Z", true)
	add("2026-10-16T09:00:00Z", true)
	add("2026-10-15T09:00:00Z", false) // paused: not due
	next, err := NextScheduleDue(ctx, pool)
	if err != nil || next == nil || !next.Equal(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("next due = %v, %v", next, err)
	}
}
