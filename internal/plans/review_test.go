package plans

import (
	"context"
	"errors"
	"testing"
	"time"

	"yuno/internal/store"
)

// Review M3: the ticket's move, the pause and the event are one transaction. A reason the
// event's JSON column refuses (a NUL) fails the last statement; nothing of the first two stays.
func TestEscalateIsOneTransaction(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	run := e.newRun()
	mustDo(t, e.svc.OnIssueTaken(ctx, e.project.ID, 11, run.ID))
	if ev, err := e.svc.Escalate(ctx, e.get("T1"), "other", "bad\x00reason", run.ID); err == nil || ev != nil {
		t.Fatalf("Escalate = %v %v, want an error", ev, err)
	}
	if tk := e.get("T1"); tk.Status != store.TicketBuilding {
		t.Fatalf("ticket = %s, want building", tk.Status)
	}
	if p := e.project2(); p.WavesPaused {
		t.Fatal("waves paused by a failed escalation")
	}
	if len(e.events()) != 0 {
		t.Fatalf("events = %+v", e.events())
	}
	// The run's end then escalates normally.
	e.svc.OnRunTerminal(ctx, ended(run, store.RunFailed, "escalated (design_wrong): no good"))
	e.svc.Wait()
	evs := e.events()
	if len(evs) != 1 || e.get("T1").Status != store.TicketFailed || !e.project2().WavesPaused {
		t.Fatalf("after the run ended: %+v %s", evs, e.get("T1").Status)
	}
}

// Review M4: the kind comes out of the reason an escalated run fails with.
func TestOnRunTerminalReadsTheEscalationKind(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	run := e.newRun()
	mustDo(t, e.svc.OnIssueTaken(ctx, e.project.ID, 11, run.ID))
	e.svc.OnRunTerminal(ctx, ended(run, store.RunFailed, "escalated (prd_gap): who may export?"))
	e.svc.Wait()
	evs := e.events()
	if len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}
	if k, rest, ok := parseEscalated("escalated (prd_gap): who may export?"); !ok || k != "prd_gap" || rest != "who may export?" {
		t.Fatalf("parse = %q %q %v", k, rest, ok)
	}
	if _, _, ok := parseEscalated("escalated (whim): x"); ok {
		t.Fatal("unknown kind parsed")
	}
	if p := e.project2(); p.WavesPausedReason == nil || *p.WavesPausedReason != "who may export?" {
		t.Fatalf("pause reason = %v", p.WavesPausedReason)
	}
}

func ageEvent(t *testing.T, id string, by time.Duration) {
	t.Helper()
	_, err := testPool.Exec(context.Background(), `UPDATE plan_events SET at = at - make_interval(secs => $2) WHERE id=$1`, id, by.Seconds())
	mustDo(t, err)
}

func notifiedAt(t *testing.T, id string) *time.Time {
	t.Helper()
	var at *time.Time
	mustDo(t, testPool.QueryRow(context.Background(), `SELECT notified_at FROM plan_events WHERE id=$1`, id).Scan(&at))
	return at
}

// Review M2: a notice that went out sets notified_at; one deferred, or sent with nowhere
// to go, stays null and the sweep sends it again under the same key, once.
func TestSweepResendsNoticesNotDelivered(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.bind()
	e.rep.mu.Lock()
	e.rep.deferred = true
	e.rep.mu.Unlock()
	run := e.newRun()
	mustDo(t, e.svc.OnIssueTaken(ctx, e.project.ID, 11, run.ID))
	ev, err := e.svc.Escalate(ctx, e.get("T1"), "loop_limit", "three rejections", run.ID)
	if err != nil || ev == nil {
		t.Fatalf("Escalate = %v %v", ev, err)
	}
	e.svc.Wait()
	if notifiedAt(t, ev.ID) != nil || len(e.rep.messages()) != 0 {
		t.Fatal("a deferred notice was marked notified")
	}
	// Too young: the first send may still finish.
	e.rep.mu.Lock()
	e.rep.deferred = false
	e.rep.mu.Unlock()
	e.svc.Sweep(ctx)
	e.svc.Wait()
	if len(e.rep.messages()) != 0 {
		t.Fatal("a young event was resent")
	}
	ageEvent(t, ev.ID, time.Minute)
	e.svc.Sweep(ctx)
	e.svc.Wait()
	msgs := e.rep.messages()
	if len(msgs) != 1 || msgs[0].key != NotifyKey(ev.ID) || !contains(msgs[0].text, "escalated (loop_limit)") || !contains(msgs[0].text, "three rejections") {
		t.Fatalf("messages = %+v", msgs)
	}
	if notifiedAt(t, ev.ID) == nil {
		t.Fatal("notified_at not set after the resend")
	}
	e.svc.Sweep(ctx)
	e.svc.Wait()
	if n := len(e.rep.messages()); n != 1 {
		t.Fatalf("%d messages after a second sweep", n)
	}
}

func TestNotifySetsNotifiedAtAndOldEventsAreLeft(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.bind()
	run := e.newRun()
	mustDo(t, e.svc.OnIssueTaken(ctx, e.project.ID, 12, run.ID))
	ev, err := e.svc.Escalate(ctx, e.get("T2"), "other", "x", run.ID)
	if err != nil || ev == nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if notifiedAt(t, ev.ID) == nil {
		t.Fatal("notified_at not set by Notify")
	}
	// A paused event is not a notice; an event past the window is not resent.
	mustDo(t, e.svc.Pause(ctx, e.project.ID, "later"))
	old := &store.PlanEvent{PlanID: e.plan.ID, Kind: store.PlanEventWaveDone, At: time.Now().UTC().Add(-48 * time.Hour)}
	mustDo(t, store.AddPlanEvent(ctx, testPool, old))
	e.svc.Sweep(ctx)
	e.svc.Wait()
	if n := len(e.rep.messages()); n != 1 {
		t.Fatalf("%d messages", n)
	}
}

// Review M9.
func TestCancelPlan(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sub, cancel := e.hub.Subscribe("", 16)
	defer cancel()
	if _, err := e.svc.Cancel(ctx, "no-such-plan", "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown plan = %v", err)
	}
	p, err := e.svc.Cancel(ctx, e.plan.ID, "the feature was dropped")
	if err != nil || p.Status != store.PlanCancelled {
		t.Fatalf("Cancel = %+v %v", p, err)
	}
	evs := e.events()
	if len(evs) != 1 || evs[0].Kind != store.PlanEventCancelled || !contains(string(evs[0].Detail), "the feature was dropped") {
		t.Fatalf("events = %+v", evs)
	}
	select {
	case ev := <-sub:
		if ev.Type != EventPlanUpdated {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no plan.updated event")
	}
	if _, err := e.svc.Cancel(ctx, e.plan.ID, "again"); !errors.Is(err, ErrPlanFinished) {
		t.Fatalf("second cancel = %v", err)
	}
	if len(e.events()) != 1 {
		t.Fatal("a refused cancel wrote an event")
	}
	// The project and the feature name are free again.
	again := &store.Plan{ProjectID: e.project.ID, Feature: e.plan.Feature, PlanRunID: e.planRun.ID}
	mustDo(t, store.CreatePlan(ctx, testPool, again))
}

// Review L6: a ticket that went into review is published by the sweep (the link is made in
// the tool's transaction, which has no hub).
func TestSweepPublishesTicketsInReview(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.svc.Sweep(ctx) // the first sweep sets the mark
	sub, cancel := e.hub.Subscribe("", 16)
	defer cancel()
	run := e.newRun()
	mustDo(t, e.svc.OnIssueTaken(ctx, e.project.ID, 11, run.ID))
	for len(sub) > 0 { // OnIssueTaken's own publish
		<-sub
	}
	mr := &store.MergeRequest{ProjectID: e.project.ID, RunID: run.ID, Provider: "gitlab", Number: 5, URL: "https://gitlab.test/g/demo/-/merge_requests/5",
		SourceBranch: "yuno/t1", TargetBranch: "main", Title: "T1"}
	if ok, err := store.InsertMergeRequest(ctx, testPool, mr); err != nil || !ok {
		t.Fatalf("insert: %v %v", ok, err)
	}
	e.svc.Sweep(ctx)
	select {
	case ev := <-sub:
		if ev.Type != EventPlanUpdated || !contains(string(ev.Data), e.plan.ID) {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no plan.updated after the ticket went into review")
	}
}

// Review L5.
func TestReapplyMerged(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	run := e.newRun()
	mustDo(t, e.svc.OnIssueTaken(ctx, e.project.ID, 11, run.ID))
	mr := &store.MergeRequest{ProjectID: e.project.ID, RunID: run.ID, Provider: "gitlab", Number: 5, URL: "https://gitlab.test/g/demo/-/merge_requests/5",
		SourceBranch: "yuno/t1", TargetBranch: "main", Title: "T1"}
	if ok, err := store.InsertMergeRequest(ctx, testPool, mr); err != nil || !ok {
		t.Fatalf("insert: %v %v", ok, err)
	}
	if n, err := e.svc.ReapplyMerged(ctx); err != nil || n != 0 {
		t.Fatalf("open record: %d %v", n, err)
	}
	// The record merged; the ticket was not told.
	won, err := store.MarkMergeRequestMerged(ctx, testPool, mr.ID, "sync", time.Now().UTC())
	if err != nil || !won {
		t.Fatalf("mark: %v %v", won, err)
	}
	if e.get("T1").Status != store.TicketInReview {
		t.Fatal("setup: the ticket moved")
	}
	if n, err := e.svc.ReapplyMerged(ctx); err != nil || n != 1 {
		t.Fatalf("ReapplyMerged = %d %v", n, err)
	}
	if e.get("T1").Status != store.TicketMerged {
		t.Fatalf("T1 = %s", e.get("T1").Status)
	}
	if n, _ := e.svc.ReapplyMerged(ctx); n != 0 {
		t.Fatalf("second pass moved %d", n)
	}
}

// Review H2, store side: a failed ticket goes back to building, or to in_review with a
// merge request, with a `retried` event; any other status is left alone.
func TestRetryTicketOfRun(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	run := e.newRun()
	mustDo(t, e.svc.OnIssueTaken(ctx, e.project.ID, 11, run.ID))
	if pid, _, err := store.RetryTicketOfRun(ctx, testPool, run.ID); err != nil || pid != "" {
		t.Fatalf("building ticket: %q %v", pid, err)
	}
	mustDo(t, store.SetTicketStatus(ctx, testPool, e.plan.ID, "T1", store.TicketFailed))
	pid, proj, err := store.RetryTicketOfRun(ctx, testPool, run.ID)
	if err != nil || pid != e.plan.ID || proj != e.project.ID {
		t.Fatalf("RetryTicketOfRun = %q %q %v", pid, proj, err)
	}
	if e.get("T1").Status != store.TicketBuilding {
		t.Fatalf("T1 = %s", e.get("T1").Status)
	}
	evs := e.events()
	if len(evs) != 1 || evs[0].Kind != store.PlanEventRetried || *evs[0].TicketID != "T1" {
		t.Fatalf("events = %+v", evs)
	}
	mr := &store.MergeRequest{ProjectID: e.project.ID, RunID: run.ID, Provider: "gitlab", Number: 6, URL: "https://gitlab.test/g/demo/-/merge_requests/6",
		SourceBranch: "yuno/t1", TargetBranch: "main", Title: "T1"}
	if ok, err := store.InsertMergeRequest(ctx, testPool, mr); err != nil || !ok {
		t.Fatalf("insert: %v %v", ok, err)
	}
	mustDo(t, store.SetTicketStatus(ctx, testPool, e.plan.ID, "T1", store.TicketFailed))
	if pid, _, err := store.RetryTicketOfRun(ctx, testPool, run.ID); err != nil || pid == "" {
		t.Fatalf("with a request: %q %v", pid, err)
	}
	if e.get("T1").Status != store.TicketInReview {
		t.Fatalf("T1 = %s", e.get("T1").Status)
	}
}
