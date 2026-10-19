package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// DESIGN 5.29 rule 1 (migration 0017): plans, tickets, events and the pause flag.

func planFixture(t *testing.T, name string) (*Project, *Run) {
	t.Helper()
	return mrProject(t, testPool, name)
}

// T5.29.1: at most one open plan per project, one plan per feature, and a finished plan
// frees the slot.
func TestPlanConstraints(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, run := planFixture(t, "plans-one")
	other, otherRun := planFixture(t, "plans-other")

	if open, err := OpenPlanForProject(ctx, pool, p.ID); err != nil || open != nil {
		t.Fatalf("no plan yet: %v %v", open, err)
	}
	a := &Plan{ProjectID: p.ID, Feature: "search", PlanRunID: run.ID}
	mustDo(t, CreatePlan(ctx, pool, a))
	if a.Status != PlanPublishing || a.CurrentWave != 1 || a.CreatedAt.IsZero() {
		t.Fatalf("defaults: %+v", a)
	}
	if err := CreatePlan(ctx, pool, &Plan{ProjectID: p.ID, Feature: "export", PlanRunID: run.ID}); !errors.Is(err, ErrPlanOpen) {
		t.Fatalf("second open plan = %v, want ErrPlanOpen", err)
	}
	if err := CreatePlan(ctx, pool, &Plan{ProjectID: p.ID, Feature: "search", PlanRunID: run.ID}); !errors.Is(err, ErrPlanOpen) && !errors.Is(err, ErrPlanExists) {
		t.Fatalf("same feature = %v", err)
	}
	// Another project is independent.
	mustDo(t, CreatePlan(ctx, pool, &Plan{ProjectID: other.ID, Feature: "search", PlanRunID: otherRun.ID}))

	open, err := OpenPlanForProject(ctx, pool, p.ID)
	if err != nil || open == nil || open.ID != a.ID {
		t.Fatalf("open plan = %+v %v", open, err)
	}
	got, err := GetPlan(ctx, pool, a.ID)
	if err != nil || got.Feature != "search" || got.PlanMRID != nil {
		t.Fatalf("GetPlan = %+v %v", got, err)
	}
	if _, err := GetPlan(ctx, pool, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown plan = %v", err)
	}

	// A status outside the list is refused by the table.
	if err := SetPlanStatus(ctx, pool, a.ID, "weird"); err == nil {
		t.Fatal("bad status accepted")
	}
	mustDo(t, SetPlanStatus(ctx, pool, a.ID, PlanDone))
	if open, _ := OpenPlanForProject(ctx, pool, p.ID); open != nil {
		t.Fatalf("a done plan is not open: %+v", open)
	}
	// The feature name is free again once its plan is finished (review M9): the plan of the
	// same feature is refused only while the first is open.
	c := &Plan{ProjectID: p.ID, Feature: "search", PlanRunID: run.ID}
	mustDo(t, CreatePlan(ctx, pool, c))
	mustDo(t, CancelPlan(ctx, pool, c.ID))
	if err := CancelPlan(ctx, pool, c.ID); !errors.Is(err, ErrPlanFinished) {
		t.Fatalf("second cancel = %v", err)
	}
	if err := CancelPlan(ctx, pool, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown cancel = %v", err)
	}
	b := &Plan{ProjectID: p.ID, Feature: "export", PlanRunID: run.ID}
	mustDo(t, CreatePlan(ctx, pool, b))
	plans, err := ListPlans(ctx, pool, p.ID)
	if err != nil || len(plans) != 3 || plans[0].ID != b.ID || plans[1].ID != c.ID || plans[2].ID != a.ID {
		t.Fatalf("ListPlans = %+v %v", plans, err)
	}
	if none, err := ListPlans(ctx, pool, "nope"); err != nil || none == nil || len(none) != 0 {
		t.Fatalf("empty list = %#v %v", none, err)
	}
	openAll, err := ListOpenPlans(ctx, pool)
	if err != nil || len(openAll) != 2 {
		t.Fatalf("ListOpenPlans = %+v %v", openAll, err)
	}

	// Setters.
	mr := mrOf(p, run, 7)
	if ok, err := InsertMergeRequest(ctx, pool, mr); err != nil || !ok {
		t.Fatalf("insert mr: %v %v", ok, err)
	}
	mustDo(t, SetPlanMR(ctx, pool, b.ID, mr.ID))
	mustDo(t, SetCurrentWave(ctx, pool, b.ID, 3))
	mustDo(t, SetPlanStatus(ctx, pool, b.ID, PlanBuilding))
	got, _ = GetPlan(ctx, pool, b.ID)
	if got.PlanMRID == nil || *got.PlanMRID != mr.ID || got.CurrentWave != 3 || got.Status != PlanBuilding || !got.UpdatedAt.After(got.CreatedAt) {
		t.Fatalf("after setters: %+v", got)
	}
	for name, err := range map[string]error{
		"status": SetPlanStatus(ctx, pool, "nope", PlanDone),
		"mr":     SetPlanMR(ctx, pool, "nope", mr.ID),
		"wave":   SetCurrentWave(ctx, pool, "nope", 2),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s on an unknown plan = %v", name, err)
		}
	}
}

func TestPlanTickets(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, run := planFixture(t, "plans-tickets")
	plan := &Plan{ProjectID: p.ID, Feature: "search", PlanRunID: run.ID}
	mustDo(t, CreatePlan(ctx, pool, plan))

	n := int64(41)
	for _, tk := range []*PlanTicket{
		{PlanID: plan.ID, TicketID: "T10", Wave: 2, Title: "ten"},
		{PlanID: plan.ID, TicketID: "T2", Wave: 1, Title: "two"},
		{PlanID: plan.ID, TicketID: "T1", Wave: 1, Title: "one", IssueNumber: &n, IssueURL: "https://gitlab.test/x/-/issues/41"},
	} {
		mustDo(t, UpsertPlanTicket(ctx, pool, tk))
		if tk.Status != TicketPending || tk.UpdatedAt.IsZero() {
			t.Fatalf("defaults: %+v", tk)
		}
	}
	list, err := ListPlanTickets(ctx, pool, plan.ID)
	if err != nil || len(list) != 3 || list[0].TicketID != "T1" || list[1].TicketID != "T2" || list[2].TicketID != "T10" {
		t.Fatalf("order by wave then ticket: %+v %v", list, err)
	}
	if none, err := ListPlanTickets(ctx, pool, "nope"); err != nil || none == nil || len(none) != 0 {
		t.Fatalf("empty = %#v %v", none, err)
	}

	// Setters keep to their column; an upsert (a revision) keeps progress.
	mr := mrOf(p, run, 8)
	if ok, err := InsertMergeRequest(ctx, pool, mr); err != nil || !ok {
		t.Fatalf("insert mr: %v %v", ok, err)
	}
	mustDo(t, SetTicketRun(ctx, pool, plan.ID, "T2", run.ID))
	mustDo(t, SetTicketMR(ctx, pool, plan.ID, "T2", mr.ID))
	mustDo(t, SetTicketStatus(ctx, pool, plan.ID, "T2", TicketInReview))
	mustDo(t, SetTicketIssue(ctx, pool, plan.ID, "T2", 42, "https://gitlab.test/x/-/issues/42"))
	revised := &PlanTicket{PlanID: plan.ID, TicketID: "T2", Wave: 2, Title: "two, revised", IssueNumber: ptr64(42), IssueURL: "u"}
	mustDo(t, UpsertPlanTicket(ctx, pool, revised))
	if revised.Status != TicketInReview || revised.RunID == nil || *revised.RunID != run.ID ||
		revised.MergeRequestID == nil || *revised.MergeRequestID != mr.ID || revised.Wave != 2 || revised.Title != "two, revised" {
		t.Fatalf("upsert must keep status, run and mr: %+v", revised)
	}
	if err := SetTicketStatus(ctx, pool, plan.ID, "T2", "weird"); err == nil {
		t.Fatal("bad ticket status accepted")
	}
	for name, err := range map[string]error{
		"status": SetTicketStatus(ctx, pool, plan.ID, "T99", TicketFailed),
		"run":    SetTicketRun(ctx, pool, plan.ID, "T99", run.ID),
		"mr":     SetTicketMR(ctx, pool, plan.ID, "T99", mr.ID),
		"issue":  SetTicketIssue(ctx, pool, plan.ID, "T99", 1, "u"),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s on an unknown ticket = %v", name, err)
		}
	}

	// TicketByIssue: by project and number; the newest plan wins; unknown is ErrNotFound.
	got, err := TicketByIssue(ctx, pool, p.ID, 41)
	if err != nil || got.TicketID != "T1" || got.PlanID != plan.ID {
		t.Fatalf("TicketByIssue = %+v %v", got, err)
	}
	if _, err := TicketByIssue(ctx, pool, p.ID, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown issue = %v", err)
	}
	if _, err := TicketByIssue(ctx, pool, "other-project", 41); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other project = %v", err)
	}
	mustDo(t, SetPlanStatus(ctx, pool, plan.ID, PlanDone))
	plan2 := &Plan{ProjectID: p.ID, Feature: "export", PlanRunID: run.ID}
	mustDo(t, CreatePlan(ctx, pool, plan2))
	mustDo(t, UpsertPlanTicket(ctx, pool, &PlanTicket{PlanID: plan2.ID, TicketID: "T1", Wave: 1, Title: "again", IssueNumber: &n}))
	if got, err := TicketByIssue(ctx, pool, p.ID, 41); err != nil || got.PlanID != plan2.ID {
		t.Fatalf("newest plan wins: %+v %v", got, err)
	}
}

func ptr64(n int64) *int64 { return &n }

func TestPlanEvents(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, run := planFixture(t, "plans-events")
	plan := &Plan{ProjectID: p.ID, Feature: "search", PlanRunID: run.ID}
	mustDo(t, CreatePlan(ctx, pool, plan))

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	w, tid := 1, "T1"
	mustDo(t, AddPlanEvent(ctx, pool, &PlanEvent{PlanID: plan.ID, Kind: PlanEventPublished, At: base}))
	mustDo(t, AddPlanEvent(ctx, pool, &PlanEvent{PlanID: plan.ID, Kind: PlanEventWaveReleased, Wave: &w, At: base.Add(time.Minute),
		Detail: json.RawMessage(`{"tickets":2}`)}))
	e := &PlanEvent{PlanID: plan.ID, Kind: PlanEventEscalated, Wave: &w, TicketID: &tid, At: base.Add(2 * time.Minute),
		Detail: json.RawMessage(`{"reason":"prd gap"}`)}
	mustDo(t, AddPlanEvent(ctx, pool, e))
	if e.ID == "" {
		t.Fatal("no id")
	}

	got, err := ListPlanEvents(ctx, pool, plan.ID, 0)
	if err != nil || len(got) != 3 || got[0].Kind != PlanEventEscalated || got[2].Kind != PlanEventPublished {
		t.Fatalf("newest first: %+v %v", got, err)
	}
	if got[0].TicketID == nil || *got[0].TicketID != "T1" || got[0].Wave == nil || *got[0].Wave != 1 {
		t.Fatalf("wave/ticket: %+v", got[0])
	}
	var d struct{ Reason string }
	if json.Unmarshal(got[0].Detail, &d); d.Reason != "prd gap" {
		t.Fatalf("detail = %s", got[0].Detail)
	}
	if string(got[2].Detail) != "{}" || got[2].Wave != nil || got[2].TicketID != nil {
		t.Fatalf("plain event: %+v", got[2])
	}
	if lim, _ := ListPlanEvents(ctx, pool, plan.ID, 2); len(lim) != 2 || lim[0].Kind != PlanEventEscalated {
		t.Fatalf("limit: %+v", lim)
	}
	if none, err := ListPlanEvents(ctx, pool, "nope", 10); err != nil || none == nil || len(none) != 0 {
		t.Fatalf("empty = %#v %v", none, err)
	}
}

func TestWavesPausedAndDraft(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, run := planFixture(t, "plans-pause")
	got, _ := GetProject(ctx, pool, p.ID)
	if got.WavesPaused || got.WavesPausedReason != nil {
		t.Fatalf("default: %+v", got)
	}
	mustDo(t, SetWavesPaused(ctx, pool, p.ID, true, "ticket T3 escalated"))
	got, _ = GetProject(ctx, pool, p.ID)
	if !got.WavesPaused || got.WavesPausedReason == nil || *got.WavesPausedReason != "ticket T3 escalated" {
		t.Fatalf("paused: %+v", got)
	}
	list, _ := ListProjects(ctx, pool)
	if len(list) == 0 || !list[0].WavesPaused {
		t.Fatalf("ListProjects: %+v", list)
	}
	mustDo(t, SetWavesPaused(ctx, pool, p.ID, false, "ignored"))
	got, _ = GetProject(ctx, pool, p.ID)
	if got.WavesPaused || got.WavesPausedReason != nil {
		t.Fatalf("resumed: %+v", got)
	}
	if err := SetWavesPaused(ctx, pool, "nope", true, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project = %v", err)
	}

	// merge_requests.draft round-trips and defaults to false.
	mr := mrOf(p, run, 9)
	mr.Draft = true
	if ok, err := InsertMergeRequest(ctx, pool, mr); err != nil || !ok {
		t.Fatalf("insert: %v %v", ok, err)
	}
	rec, err := GetMergeRequest(ctx, pool, mr.ID)
	if err != nil || !rec.Draft {
		t.Fatalf("draft = %+v %v", rec, err)
	}
	run2 := mrRun(t, pool, p, nil)
	plain := mrOf(p, run2, 10)
	if ok, err := InsertMergeRequest(ctx, pool, plain); err != nil || !ok {
		t.Fatalf("insert: %v %v", ok, err)
	}
	if rec, _ := GetMergeRequest(ctx, pool, plain.ID); rec.Draft {
		t.Fatal("draft must default to false")
	}
}
