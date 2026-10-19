package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// DESIGN 5.29 rules 11 to 14: the store functions of the wave watcher.

func TestShipRef(t *testing.T) {
	ref := ShipRef("plan-1", 3)
	if ref != "plan:plan-1:wave:3" {
		t.Fatalf("ref = %q", ref)
	}
	if id, w, ok := ParseShipRef(ref); !ok || id != "plan-1" || w != 3 {
		t.Fatalf("parse = %q %d %v", id, w, ok)
	}
	for _, bad := range []string{"", "plan:x", "plan::wave:1", "plan:x:wave:0", "plan:x:wave:a", "issue:x:wave:1"} {
		if _, _, ok := ParseShipRef(bad); ok {
			t.Fatalf("%q parsed", bad)
		}
	}
}

func TestPlanMainAndShipRuns(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, run := planFixture(t, "waves-main")
	plan := &Plan{ProjectID: p.ID, Feature: "search", PlanRunID: run.ID, Status: PlanBuilding}
	mustDo(t, CreatePlan(ctx, pool, plan))

	if m, err := GetPlanMain(ctx, pool, plan.ID); err != nil || m != nil {
		t.Fatalf("before the first read: %+v %v", m, err)
	}
	if _, err := GetPlanMain(ctx, pool, "no-such"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown plan: %v", err)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	m := PlanMain{State: "passed", SHA: "abc", At: &at, URL: "u"}
	if changed, err := SetPlanMain(ctx, pool, plan.ID, m); err != nil || !changed {
		t.Fatalf("first set: %v %v", changed, err)
	}
	if changed, err := SetPlanMain(ctx, pool, plan.ID, m); err != nil || changed {
		t.Fatalf("same again: %v %v", changed, err)
	}
	got, err := GetPlanMain(ctx, pool, plan.ID)
	if err != nil || got == nil || got.State != "passed" || got.SHA != "abc" || got.URL != "u" || got.At == nil || !got.At.Equal(at) {
		t.Fatalf("main = %+v %v", got, err)
	}
	if changed, _ := SetPlanMain(ctx, pool, plan.ID, PlanMain{State: "none"}); !changed {
		t.Fatal("a new state is a change")
	}
	if got, _ := GetPlanMain(ctx, pool, plan.ID); got.At != nil || got.SHA != "" {
		t.Fatalf("none = %+v", got)
	}
	if _, err := SetPlanMain(ctx, pool, "no-such", m); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown plan: %v", err)
	}

	// Ship runs: unique per plan and wave, listed by wave, another plan's not included.
	wf := w9Workflow(t, pool)
	mk := func(planID string, wave int) (*Run, error) {
		return w9Run(pool, wf, func(r *Run) { ref := ShipRef(planID, wave); r.Trigger, r.TriggerRef = TriggerPlan, &ref })
	}
	r2, err := mk(plan.ID, 2)
	mustDo(t, err)
	r1, err := mk(plan.ID, 1)
	mustDo(t, err)
	if _, err := mk(plan.ID, 1); !errors.Is(err, ErrDuplicateTrigger) {
		t.Fatalf("second ship run of wave 1: %v, want ErrDuplicateTrigger", err)
	}
	// A manual run with the same ref text is not a ship run and is not refused.
	ref := ShipRef(plan.ID, 1)
	w9MustRun(t, pool, wf, func(r *Run) { r.TriggerRef = &ref })
	if _, err := mk("other-plan", 1); err != nil {
		t.Fatal(err)
	}
	list, err := ListShipRuns(ctx, pool, plan.ID)
	if err != nil || len(list) != 2 || list[0].RunID != r1.ID || list[0].Wave != 1 || list[1].RunID != r2.ID || list[1].Status != RunQueued {
		t.Fatalf("ship runs = %+v %v", list, err)
	}
	if got, err := GetRunByTrigger(ctx, pool, TriggerPlan, ShipRef(plan.ID, 2)); err != nil || got.ID != r2.ID {
		t.Fatalf("by trigger = %v %v", got, err)
	}
}

func TestPlanMovesAndEvents(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, run := planFixture(t, "waves-move")
	plan := &Plan{ProjectID: p.ID, Feature: "search", PlanRunID: run.ID, Status: PlanBuilding}
	mustDo(t, CreatePlan(ctx, pool, plan))
	if moved, err := MovePlan(ctx, pool, plan.ID, PlanShipping, 1, PlanWaitingPlanMerge); err != nil || moved {
		t.Fatalf("move from the wrong status: %v %v", moved, err)
	}
	if moved, err := MovePlan(ctx, pool, plan.ID, PlanShipping, 2, PlanBuilding); err != nil || !moved {
		t.Fatalf("move: %v %v", moved, err)
	}
	if got, _ := GetPlan(ctx, pool, plan.ID); got.Status != PlanShipping || got.CurrentWave != 2 {
		t.Fatalf("plan = %+v", got)
	}
	tx, err := pool.Begin(ctx)
	mustDo(t, err)
	if l, err := LockPlan(ctx, tx, plan.ID); err != nil || l.ID != plan.ID {
		t.Fatalf("lock: %v %v", l, err)
	}
	mustDo(t, tx.Rollback(ctx))
	if _, err := LockPlan(ctx, pool, "no-such"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lock unknown: %v", err)
	}

	w := 2
	mustDo(t, AddPlanEvent(ctx, pool, &PlanEvent{PlanID: plan.ID, Kind: PlanEventWaveDone, Wave: &w, Detail: []byte(`{"run_id":"r1"}`)}))
	for _, c := range []struct {
		wave  int
		run   string
		found bool
	}{{0, "", true}, {2, "", true}, {1, "", false}, {2, "r1", true}, {0, "r2", false}} {
		_, err := FindPlanEvent(ctx, pool, plan.ID, PlanEventWaveDone, c.wave, c.run)
		if (err == nil) != c.found || (err != nil && !errors.Is(err, ErrNotFound)) {
			t.Fatalf("find wave %d run %q: %v, want found=%v", c.wave, c.run, err, c.found)
		}
	}

	// Wave tickets with their merge request records.
	a := &PlanTicket{PlanID: plan.ID, TicketID: "T1", Wave: 2, Title: "a"}
	b := &PlanTicket{PlanID: plan.ID, TicketID: "T2", Wave: 2, Title: "b"}
	mustDo(t, UpsertPlanTicket(ctx, pool, a))
	mustDo(t, UpsertPlanTicket(ctx, pool, b))
	mustDo(t, UpsertPlanTicket(ctx, pool, &PlanTicket{PlanID: plan.ID, TicketID: "T3", Wave: 3, Title: "c"}))
	wf := w9Workflow(t, pool)
	mrRun := w9MustRun(t, pool, wf, nil)
	closed := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	mr := &MergeRequest{ProjectID: p.ID, RunID: mrRun.ID, Provider: "gitlab", Number: 7, URL: "u", SourceBranch: "b", TargetBranch: "main",
		State: MergeRequestMerged, ClosedAt: &closed}
	if ok, err := InsertMergeRequest(ctx, pool, mr); err != nil || !ok {
		t.Fatalf("insert: %v %v", ok, err)
	}
	mustDo(t, SetTicketMR(ctx, pool, plan.ID, "T1", mr.ID))
	list, err := ListWaveTickets(ctx, pool, plan.ID, 2)
	if err != nil || len(list) != 2 {
		t.Fatalf("wave 2 = %+v %v", list, err)
	}
	if list[0].TicketID != "T1" || list[0].MRState != MergeRequestMerged || list[0].MRNumber != 7 || list[0].MergedAt == nil || !list[0].MergedAt.Equal(closed) {
		t.Fatalf("T1 = %+v", list[0])
	}
	if list[1].TicketID != "T2" || list[1].MRState != "" || list[1].MergedAt != nil {
		t.Fatalf("T2 = %+v", list[1])
	}
}

func TestWorkflowByFromTemplate(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	if _, err := GetWorkflowByFromTemplate(ctx, pool, "ship_pipeline"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("none yet: %v", err)
	}
	key := "ship_pipeline"
	first := &Workflow{Name: "one " + NewID(), FromTemplate: &key, Definition: []byte(`{"nodes":[],"edges":[]}`)}
	mustDo(t, CreateWorkflow(ctx, pool, first))
	mustDo(t, CreateWorkflow(ctx, pool, &Workflow{Name: "two " + NewID(), FromTemplate: &key, Definition: []byte(`{"nodes":[],"edges":[]}`)}))
	if got, err := GetWorkflowByFromTemplate(ctx, pool, key); err != nil || got.ID != first.ID {
		t.Fatalf("oldest copy = %v %v", got, err)
	}
}
