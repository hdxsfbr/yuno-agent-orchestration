package mergereq

import (
	"context"
	"testing"
	"time"

	"yuno/internal/plans"
	"yuno/internal/store"
)

// DESIGN 5.29 rule 10: the record of a ticket run's merge request puts the ticket in
// review (the store does it on insert); the sync's merged state, or a merge made through the
// platform, sets it merged; a request closed unmerged leaves it in review.
func TestMergedRequestSetsThePlanTicketMerged(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.sync.Plans = &plans.Service{Pool: e.pool}
	p := e.project(testToken)

	planRun := e.run(p, false)
	plan := &store.Plan{ProjectID: p.ID, Feature: "search", PlanRunID: planRun.ID, Status: store.PlanBuilding}
	must(t, store.CreatePlan(ctx, e.pool, plan))
	tickets := map[string]*store.Run{}
	for _, id := range []string{"T1", "T2", "T3"} {
		n := int64(len(tickets) + 1)
		must(t, store.UpsertPlanTicket(ctx, e.pool, &store.PlanTicket{PlanID: plan.ID, TicketID: id, Wave: 1, Title: id, IssueNumber: &n, Status: store.TicketReleased}))
		run := e.run(p, false)
		if moved, err := store.LinkTicketRun(ctx, e.pool, plan.ID, id, run.ID); err != nil || !moved {
			t.Fatalf("link %s: %v %v", id, moved, err)
		}
		tickets[id] = run
	}
	status := func(id string) string {
		l, err := store.ListPlanTickets(ctx, e.pool, plan.ID)
		must(t, err)
		for _, tk := range l {
			if tk.TicketID == id {
				return tk.Status
			}
		}
		t.Fatalf("no ticket %s", id)
		return ""
	}

	synced := e.record(p, tickets["T1"], 1, time.Hour)
	closed := e.record(p, tickets["T2"], 2, time.Hour)
	byHand := e.record(p, tickets["T3"], 3, time.Hour)
	for _, id := range []string{"T1", "T2", "T3"} {
		if got := status(id); got != store.TicketInReview {
			t.Fatalf("%s after its record = %s", id, got)
		}
	}

	e.fake.request(1, "merged", "sha", "success", 0)
	e.fake.request(2, "closed", "sha", "", 0)
	for _, id := range []string{synced.ID, closed.ID} {
		if _, err := e.sync.SyncOne(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if got := status("T1"); got != store.TicketMerged {
		t.Fatalf("T1 after a merged sync = %s", got)
	}
	if got := status("T2"); got != store.TicketInReview {
		t.Fatalf("T2 after a closed sync = %s", got)
	}
	if _, err := e.sync.MarkMerged(ctx, byHand.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	if got := status("T3"); got != store.TicketMerged {
		t.Fatalf("T3 after a merge through the platform = %s", got)
	}
}

// Review L5: a pass applies the merges a ticket missed (the record is merged, the ticket
// still in review).
func TestPassReappliesMissedMerges(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.project(testToken)
	planRun := e.run(p, false)
	plan := &store.Plan{ProjectID: p.ID, Feature: "search", PlanRunID: planRun.ID, Status: store.PlanBuilding}
	must(t, store.CreatePlan(ctx, e.pool, plan))
	n := int64(1)
	must(t, store.UpsertPlanTicket(ctx, e.pool, &store.PlanTicket{PlanID: plan.ID, TicketID: "T1", Wave: 1, Title: "T1", IssueNumber: &n, Status: store.TicketReleased}))
	run := e.run(p, false)
	if moved, err := store.LinkTicketRun(ctx, e.pool, plan.ID, "T1", run.ID); err != nil || !moved {
		t.Fatalf("link: %v %v", moved, err)
	}
	rec := e.record(p, run, 1, time.Hour)
	// The record merged with no plan hook listening: the ticket is not told.
	if won, err := store.MarkMergeRequestMerged(ctx, e.pool, rec.ID, "sync", time.Now().UTC()); err != nil || !won {
		t.Fatalf("mark: %v %v", won, err)
	}
	status := func() string {
		l, err := store.ListPlanTickets(ctx, e.pool, plan.ID)
		must(t, err)
		return l[0].Status
	}
	if status() != store.TicketInReview {
		t.Fatalf("setup: %s", status())
	}
	e.sync.Plans = &plans.Service{Pool: e.pool}
	e.sync.TickOnce(ctx)
	if status() != store.TicketMerged {
		t.Fatalf("after a pass: %s", status())
	}
}
