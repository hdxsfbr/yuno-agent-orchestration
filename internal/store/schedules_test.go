package store

import (
	"bytes"
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

// DESIGN 5.9 rule 6: a schedule names the project its runs work on. The project's
// name is joined for the list and survives the project's deletion; an update may
// clear it; the tick's locked rows carry it.
func TestScheduleProject(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := &Workflow{Name: "project workflow", Definition: json.RawMessage(`{"nodes":[],"edges":[],"project":true}`)}
	mustDo(t, CreateWorkflow(ctx, pool, wf))
	p := &Project{Name: "Playground", RepoURL: "https://gitlab.example/g/play.git", Provider: "gitlab", DefaultBranch: "main"}
	mustDo(t, CreateProject(ctx, pool, p, []byte("sealed")))
	expr := "0 9 * * *"
	due := mustParse(t, "2001-01-01T09:00:00Z")
	s := &Schedule{WorkflowID: wf.ID, ProjectID: &p.ID, Kind: ScheduleCron, CronExpr: &expr, TZ: "UTC", Enabled: true, NextRunAt: &due}
	mustDo(t, CreateSchedule(ctx, pool, s))

	got, err := GetSchedule(ctx, pool, s.ID)
	if err != nil || got.ProjectID == nil || *got.ProjectID != p.ID || got.ProjectName == nil || *got.ProjectName != "Playground" {
		t.Fatalf("get = %+v %v", got, err)
	}
	list, err := ListSchedules(ctx, pool)
	if err != nil || len(list) != 1 || list[0].ProjectName == nil || *list[0].ProjectName != "Playground" {
		t.Fatalf("list = %+v %v", list, err)
	}
	if b, _ := json.Marshal(got); !bytes.Contains(b, []byte(`"project_id":"`+p.ID+`"`)) || !bytes.Contains(b, []byte(`"project_name":"Playground"`)) {
		t.Fatalf("json = %s", b)
	}

	// The tick's rows carry the project id.
	tx, err := pool.Begin(ctx)
	mustDo(t, err)
	locked, err := LockDueSchedules(ctx, tx, due, 10)
	mustDo(t, err)
	mustDo(t, tx.Rollback(ctx))
	if len(locked) != 1 || locked[0].ProjectID == nil || *locked[0].ProjectID != p.ID || locked[0].WorkflowGone {
		t.Fatalf("locked = %+v", locked)
	}

	// A deleted project keeps its name on the schedule.
	mustDo(t, DeleteProject(ctx, pool, p.ID))
	if got, _ = GetSchedule(ctx, pool, s.ID); got.ProjectName == nil || *got.ProjectName != "Playground" {
		t.Fatalf("after the project's deletion: %+v", got)
	}

	// An update clears it; the JSON then has neither member.
	got.ProjectID = nil
	mustDo(t, UpdateSchedule(ctx, pool, got))
	got, err = GetSchedule(ctx, pool, s.ID)
	if err != nil || got.ProjectID != nil || got.ProjectName != nil {
		t.Fatalf("after clearing = %+v %v", got, err)
	}
	if b, _ := json.Marshal(got); bytes.Contains(b, []byte("project_id")) || bytes.Contains(b, []byte("project_name")) {
		t.Fatalf("json after clearing = %s", b)
	}
}
