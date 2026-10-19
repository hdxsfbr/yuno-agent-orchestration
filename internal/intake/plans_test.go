package intake

import (
	"context"
	"encoding/json"
	"testing"

	"yuno/internal/plans"
	"yuno/internal/store"
)

// T5.29.5 to T5.29.7, intake side (DESIGN 5.29 rule 10): the take of an issue that carries a
// plan ticket links the new run to the ticket and sets it building; other issues change
// nothing.
func TestTakeLinksTheRunToThePlanTicket(t *testing.T) {
	e := newEnv(t, envOpts{})
	ctx := context.Background()
	e.tr.addIssue("Add search", "x", "yuno")
	e.tr.addIssue("Export", "y", "yuno")

	planRun := &store.Run{WorkflowID: e.wf.ID, WorkflowVersion: 1, WorkflowSnapshot: json.RawMessage(`{"nodes":[],"edges":[]}`),
		Trigger: store.TriggerManual, ProjectID: &e.project.ID}
	must(t, store.CreateRun(ctx, e.pool, planRun))
	plan := &store.Plan{ProjectID: e.project.ID, Feature: "search", PlanRunID: planRun.ID, Status: store.PlanBuilding}
	must(t, store.CreatePlan(ctx, e.pool, plan))
	for id, n := range map[string]int64{"T1": 2, "T2": 7} {
		n := n
		must(t, store.UpsertPlanTicket(ctx, e.pool, &store.PlanTicket{PlanID: plan.ID, TicketID: id, Wave: 1, Title: id,
			IssueNumber: &n, Status: store.TicketReleased}))
	}
	e.loop.Plans = &plans.Service{Pool: e.pool}

	if res := e.poll(); res.Taken != 2 || res.Error != "" {
		t.Fatalf("result = %+v", res)
	}
	tickets, err := store.ListPlanTickets(ctx, e.pool, plan.ID)
	must(t, err)
	byID := map[string]store.PlanTicket{}
	for _, tk := range tickets {
		byID[tk.TicketID] = tk
	}
	run := e.oneRun(2)
	if tk := byID["T1"]; tk.Status != store.TicketBuilding || tk.RunID == nil || *tk.RunID != run.ID {
		t.Fatalf("T1 = %+v, want building with run %s", tk, run.ID)
	}
	if tk := byID["T2"]; tk.Status != store.TicketReleased || tk.RunID != nil {
		t.Fatalf("T2 (issue 7 does not exist) = %+v", tk)
	}

	// A second poll takes nothing new and leaves the ticket as it is.
	e.poll()
	tickets, _ = store.ListPlanTickets(ctx, e.pool, plan.ID)
	if tickets[0].Status != store.TicketBuilding {
		t.Fatalf("after a second poll: %+v", tickets[0])
	}
}
