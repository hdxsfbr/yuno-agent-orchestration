package plans

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"yuno/internal/store"
)

// DESIGN 5.29 rules 14 and 15: how a ship run's ending moves the plan.

// shipRun makes the ship run of wave and the approvals it decided: node → status/reason.
func (e *env) shipRun(wave int, status string, reason string, decisions map[string][2]string) *store.Run {
	e.t.Helper()
	ctx := context.Background()
	run := e.newRun()
	ref := store.ShipRef(e.plan.ID, wave)
	_, err := testPool.Exec(ctx, `UPDATE runs SET trigger=$2, trigger_ref=$3 WHERE id=$1`, run.ID, store.TriggerPlan, ref)
	mustDo(e.t, err)
	node := &store.RunNode{RunID: run.ID, NodeID: "gate", Status: store.NodeSucceeded}
	mustDo(e.t, store.CreateRunNode(ctx, testPool, node))
	now := time.Now().UTC()
	for nodeID, d := range decisions {
		a := &store.Approval{RunID: run.ID, RunNodeID: node.ID, Kind: store.ApprovalKindNode, NodeID: nodeID, Message: "m",
			Status: d[0], RequestedAt: now, ExpiresAt: now.Add(time.Hour)}
		if d[1] != "" {
			r := d[1]
			a.Reason = &r
		}
		mustDo(e.t, store.InsertApproval(ctx, testPool, a))
	}
	got, err := store.GetRun(ctx, testPool, run.ID)
	mustDo(e.t, err)
	return ended(got, status, reason)
}

func (e *env) eventOf(kind string) *store.PlanEvent {
	e.t.Helper()
	for _, ev := range e.events() {
		if ev.Kind == kind {
			return &ev
		}
	}
	return nil
}

func TestShipRunRejectionsPauseWithTheReason(t *testing.T) {
	for _, tc := range []struct {
		name, node, reason, want string
	}{
		{"smoke test", "smoke_test", "login page is blank", "smoke test rejected: login page is blank"},
		{"smoke test without a reason", "smoke_test", "", "smoke test rejected: no reason given"},
		{"acceptance", "accept_feature", "search ignores filters", "feature not accepted: search ignores filters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			mustDo(t, store.SetPlanStatus(context.Background(), testPool, e.plan.ID, store.PlanShipping))
			conv := e.bind()
			run := e.shipRun(1, store.RunFailed, "rejected at "+tc.node, map[string][2]string{tc.node: {store.ApprovalRejected, tc.reason}})
			e.svc.OnRunTerminal(context.Background(), run)
			e.svc.Wait()
			p := e.project2()
			if !p.WavesPaused || p.WavesPausedReason == nil || *p.WavesPausedReason != tc.want {
				t.Fatalf("paused = %v %v, want %q", p.WavesPaused, p.WavesPausedReason, tc.want)
			}
			ev := e.eventOf(store.PlanEventEscalated)
			if ev == nil || ev.TicketID != nil || ev.Wave == nil || *ev.Wave != 1 {
				t.Fatalf("event = %+v", ev)
			}
			var d struct {
				Reason string `json:"reason"`
				RunID  string `json:"run_id"`
			}
			mustDo(t, json.Unmarshal(ev.Detail, &d))
			if d.Reason != tc.want || d.RunID != run.ID {
				t.Fatalf("detail = %s", ev.Detail)
			}
			msgs := e.rep.messages()
			if len(msgs) != 1 || msgs[0].conversation != conv || !strings.Contains(msgs[0].text, tc.want) || !strings.HasPrefix(msgs[0].text, "[platform]") {
				t.Fatalf("messages = %+v", msgs)
			}
			// Told twice, recorded once.
			e.svc.OnRunTerminal(context.Background(), run)
			e.svc.Wait()
			n := 0
			for _, ev := range e.events() {
				if ev.Kind == store.PlanEventEscalated {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("%d escalated events after a second report", n)
			}
			// The resend sweep words the notice from the record.
			if text := e.svc.noticeText(context.Background(), &store.Plan{Feature: "search"}, ev); !strings.Contains(text, tc.want) {
				t.Fatalf("notice = %q", text)
			}
		})
	}
}

func TestShipRunFailureWithoutARejectionPauses(t *testing.T) {
	e := newEnv(t)
	mustDo(t, store.SetPlanStatus(context.Background(), testPool, e.plan.ID, store.PlanShipping))
	run := e.shipRun(1, store.RunFailed, "advance_wave refused", nil)
	e.svc.OnRunTerminal(context.Background(), run)
	p := e.project2()
	if !p.WavesPaused || p.WavesPausedReason == nil || *p.WavesPausedReason != "advance_wave refused" {
		t.Fatalf("paused = %v %v", p.WavesPaused, p.WavesPausedReason)
	}
	if e.eventOf(store.PlanEventEscalated) == nil {
		t.Fatal("no escalated event")
	}
}

func TestShipRunAcceptedFinishesThePlan(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	mustDo(t, store.SetPlanStatus(ctx, testPool, e.plan.ID, store.PlanAccepting))
	e.bind()
	run := e.shipRun(1, store.RunSucceeded, "", map[string][2]string{"smoke_test": {store.ApprovalApproved, ""}, "accept_feature": {store.ApprovalApproved, ""}})
	e.svc.OnRunTerminal(ctx, run)
	e.svc.Wait()
	plan, err := store.GetPlan(ctx, testPool, e.plan.ID)
	mustDo(t, err)
	if plan.Status != store.PlanDone {
		t.Fatalf("plan = %s, want done", plan.Status)
	}
	if e.eventOf(store.PlanEventFeatureDone) == nil {
		t.Fatalf("events = %+v", e.events())
	}
	if msgs := e.rep.messages(); len(msgs) != 1 || !strings.Contains(msgs[0].text, "is done") {
		t.Fatalf("messages = %+v", msgs)
	}
	e.svc.OnRunTerminal(ctx, run) // a second report: the plan is finished
	n := 0
	for _, ev := range e.events() {
		if ev.Kind == store.PlanEventFeatureDone {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d feature_done events", n)
	}
}

func (e *env) waveDone(wave int) {
	e.t.Helper()
	w := wave
	mustDo(e.t, store.AddPlanEvent(context.Background(), testPool, &store.PlanEvent{PlanID: e.plan.ID, Kind: store.PlanEventWaveDone,
		Wave: &w, Detail: json.RawMessage(`{}`)}))
}

func TestShipRunSucceededOfAMiddleWaveChangesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	mustDo(t, store.SetPlanStatus(ctx, testPool, e.plan.ID, store.PlanBuilding))
	e.waveDone(1) // advance_wave moved the plan on
	run := e.shipRun(1, store.RunSucceeded, "", map[string][2]string{"smoke_test": {store.ApprovalApproved, ""}})
	e.svc.OnRunTerminal(ctx, run)
	if plan, _ := store.GetPlan(ctx, testPool, e.plan.ID); plan.Status != store.PlanBuilding || e.project2().WavesPaused || len(e.events()) != 1 {
		t.Fatalf("plan = %s paused = %v events = %+v", plan.Status, e.project2().WavesPaused, e.events())
	}
}

// H2: a ship run that succeeded without closing its wave, or on the last wave without the
// acceptance approved, leaves the plan stranded; the operator is told and the waves pause.
func TestShipRunSucceededWithoutClosingTheWaveEscalates(t *testing.T) {
	for _, tc := range []struct {
		name, status, want string
		done               bool
	}{
		{"no wave_done", store.PlanShipping, "ship run for wave 1 finished without closing the wave", false},
		{"accepting, not accepted", store.PlanAccepting, "ship run for wave 1 finished without the feature being accepted", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			mustDo(t, store.SetPlanStatus(ctx, testPool, e.plan.ID, tc.status))
			if tc.done {
				e.waveDone(1)
			}
			e.bind()
			run := e.shipRun(1, store.RunSucceeded, "", map[string][2]string{"smoke_test": {store.ApprovalApproved, ""}})
			e.svc.OnRunTerminal(ctx, run)
			e.svc.Wait()
			p := e.project2()
			if !p.WavesPaused || p.WavesPausedReason == nil || *p.WavesPausedReason != tc.want {
				t.Fatalf("paused = %v %v, want %q", p.WavesPaused, p.WavesPausedReason, tc.want)
			}
			if e.eventOf(store.PlanEventEscalated) == nil {
				t.Fatal("no escalated event")
			}
			if msgs := e.rep.messages(); len(msgs) != 1 || !strings.Contains(msgs[0].text, tc.want) {
				t.Fatalf("messages = %+v", msgs)
			}
			if plan, _ := store.GetPlan(ctx, testPool, e.plan.ID); plan.Status != tc.status {
				t.Fatalf("plan = %s, want it left as it was (%s)", plan.Status, tc.status)
			}
		})
	}
}

// M1: a ship run retried after an escalation that ends badly again escalates again; the
// same ending reported twice is recorded once.
func TestShipRunRetriedAndFailedAgainEscalatesAgain(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	mustDo(t, store.SetPlanStatus(ctx, testPool, e.plan.ID, store.PlanShipping))
	e.bind()
	run := e.shipRun(1, store.RunFailed, "rejected at smoke_test: blank", map[string][2]string{"smoke_test": {store.ApprovalRejected, "blank"}})
	e.svc.OnRunTerminal(ctx, run)
	e.svc.OnRunTerminal(ctx, run)
	e.svc.Wait()
	count := func() int {
		n := 0
		for _, ev := range e.events() {
			if ev.Kind == store.PlanEventEscalated {
				n++
			}
		}
		return n
	}
	if count() != 1 {
		t.Fatalf("%d escalations after the first ending", count())
	}
	mustDo(t, store.SetWavesPaused(ctx, testPool, e.plan.ProjectID, false, "")) // the operator resumes, then retries
	run.Retries = 1
	e.svc.OnRunTerminal(ctx, run)
	e.svc.Wait()
	if count() != 2 || !e.project2().WavesPaused {
		t.Fatalf("%d escalations, paused %v; the second ending was silent", count(), e.project2().WavesPaused)
	}
	e.svc.OnRunTerminal(ctx, run)
	e.svc.Wait()
	if count() != 2 {
		t.Fatalf("%d escalations after the second ending was reported twice", count())
	}
}

func TestShipRunOfAFinishedPlanIsIgnored(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	mustDo(t, store.CancelPlan(ctx, testPool, e.plan.ID))
	run := e.shipRun(1, store.RunFailed, "x", map[string][2]string{"smoke_test": {store.ApprovalRejected, "no"}})
	e.svc.OnRunTerminal(ctx, run)
	if e.project2().WavesPaused {
		t.Fatal("a cancelled plan paused the waves")
	}
}

// The engine ends a rejected approval node as a failed run whose reason names the node; the
// reason is enough when the approval rows cannot be read.
func TestShipRunRejectionReadFromTheFailureReason(t *testing.T) {
	for reason, want := range map[string]string{
		"rejected at smoke_test: blank page":      "smoke test rejected: blank page",
		"rejected at accept_feature: wrong thing": "feature not accepted: wrong thing",
		"escalated (design_wrong): no API":        "escalated (design_wrong): no API",
	} {
		e := newEnv(t)
		mustDo(t, store.SetPlanStatus(context.Background(), testPool, e.plan.ID, store.PlanShipping))
		e.svc.OnRunTerminal(context.Background(), e.shipRun(1, store.RunFailed, reason, nil))
		e.svc.Wait() // the notice in flight ends before the next case empties the tables
		if p := e.project2(); p.WavesPausedReason == nil || *p.WavesPausedReason != want {
			t.Fatalf("%q paused with %v, want %q", reason, p.WavesPausedReason, want)
		}
	}
}
