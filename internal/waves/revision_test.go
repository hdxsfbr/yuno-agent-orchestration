package waves

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"yuno/internal/store"
	"yuno/internal/tools"
)

// DESIGN 5.29 rules 12 and 14 (D-089): while a revision of the plan runs, the watcher
// releases no wave and starts no ship run for it, and advance_wave closes the wave it ships
// but holds the next one back; when the revision ends, however it ends, the waves go on.

// revision starts a plan pipeline run that revises the plan (input revise_plan) and returns
// a function that ends it with status.
func (e *env) revision(planID string) func(status string) {
	e.t.Helper()
	run := e.newRun(store.TriggerManual, "")
	in, _ := json.Marshal(map[string]string{"task": "change the search box", "feature": "search", "revise_plan": planID})
	_, err := testPool.Exec(context.Background(), `UPDATE runs SET input=$2, status='running' WHERE id=$1`, run.ID, in)
	mustDo(e.t, err)
	return func(status string) {
		e.t.Helper()
		mustDo(e.t, store.SetRunStatus(context.Background(), testPool, run.ID, status, nil))
	}
}

// T5.29.15: the first wave waits for the plan's revision; a revision that fails holds
// nothing back once it ended.
func TestWatcherReleasesNoWaveDuringARevision(t *testing.T) {
	e := newEnv(t, store.PlanWaitingPlanMerge)
	ctx := context.Background()
	run := e.newRun(store.TriggerManual, "")
	mr := &store.MergeRequest{ProjectID: e.project.ID, RunID: run.ID, Provider: "gitlab", Number: 1, URL: "u", State: store.MergeRequestMerged,
		SourceBranch: "plan", TargetBranch: "main"}
	if ok, err := store.InsertMergeRequest(ctx, testPool, mr); err != nil || !ok {
		t.Fatalf("insert: %v %v", ok, err)
	}
	mustDo(t, store.SetPlanMR(ctx, testPool, e.plan.ID, mr.ID))
	end := e.revision(e.plan.ID)
	e.pass()
	if p := e.reload(); p.Status != store.PlanWaitingPlanMerge || len(e.issues.added) != 0 || e.status("T1") != store.TicketPending {
		t.Fatalf("a wave was released during the revision: %s %v", p.Status, e.issues.added)
	}
	end(store.RunFailed)
	e.pass()
	if p := e.reload(); p.Status != store.PlanBuilding || len(e.issues.added) != 2 || e.status("T1") != store.TicketReleased {
		t.Fatalf("after the revision failed: %s %v", p.Status, e.issues.added)
	}
}

// T5.29.15: a wave that is merged and green gets no ship run while the plan's revision runs,
// and gets it once the revision ended; a revision of another plan, or one that ended, holds
// nothing.
func TestWatcherStartsNoShipRunDuringARevision(t *testing.T) {
	e := newEnv(t, store.PlanBuilding)
	t0 := e.now.Add(-time.Hour)
	e.merged("T1", 61, t0)
	e.merged("T2", 62, t0)
	e.setMain("passed", t0.Add(time.Minute))
	e.revision(store.NewID())(store.RunRunning) // another plan's
	e.revision(e.plan.ID)(store.RunSucceeded)   // an earlier revision, ended
	end := e.revision(e.plan.ID)
	e.pass()
	if len(e.ships()) != 0 || e.eng.calls != 0 {
		t.Fatal("a ship run started during the revision")
	}
	if m, _ := store.GetPlanMain(context.Background(), testPool, e.plan.ID); m == nil || m.State != "passed" {
		t.Fatalf("main = %+v; the watcher still reads it during a revision", m)
	}
	end(store.RunSucceeded)
	e.pass()
	if s := e.ships(); len(s) != 1 || s[0].Wave != 1 {
		t.Fatalf("ship runs after the revision = %+v", s)
	}
	var in struct {
		Tickets []string `json:"tickets"`
	}
	mustDo(t, json.Unmarshal(e.eng.last.Input, &in))
	if len(in.Tickets) != 2 || in.Tickets[0] != "T1" || in.Tickets[1] != "T2" {
		t.Fatalf("ship run input tickets = %v, want the wave's", in.Tickets)
	}
}

// T5.29.15: advance_wave during a revision closes the wave, moves the plan to the next one
// and releases nothing; the watcher releases it once the revision ended, once.
func TestAdvanceDuringARevisionHoldsTheNextWave(t *testing.T) {
	e := newEnv(t, store.PlanBuilding)
	run := e.shipping(1, "T1", "T2")
	end := e.revision(e.plan.ID)
	res, err := e.svc.Advance(context.Background(), run.ID)
	e.plans.Wait()
	mustDo(t, err)
	want := tools.AdvanceResult{PlanID: e.plan.ID, Wave: 1, Closed: 2, Released: 0, Last: false, HeldWave: 2}
	if res != want {
		t.Fatalf("result = %+v, want %+v", res, want)
	}
	if len(e.issues.closed) != 2 || len(e.issues.added) != 0 || e.status("T1") != store.TicketClosed || e.status("T3") != store.TicketPending {
		t.Fatalf("closed %v, labelled %v, T1 %s, T3 %s", e.issues.closed, e.issues.added, e.status("T1"), e.status("T3"))
	}
	if p := e.reload(); p.Status != store.PlanBuilding || p.CurrentWave != 2 {
		t.Fatalf("plan = %s wave %d", p.Status, p.CurrentWave)
	}
	if k := e.kinds(); k[store.PlanEventWaveDone] != 1 || k[store.PlanEventWaveReleased] != 0 {
		t.Fatalf("events = %v", k)
	}
	if again, err := e.svc.Advance(context.Background(), run.ID); err != nil || again != want {
		t.Fatalf("second call = %+v %v", again, err)
	}

	e.pass() // the revision still runs
	if len(e.issues.added) != 0 || e.status("T3") != store.TicketPending {
		t.Fatalf("wave 2 was released during the revision: %v", e.issues.added)
	}
	end(store.RunCancelled)
	e.pause("looking at it")
	e.pass()
	if len(e.issues.added) != 0 {
		t.Fatal("a paused project released the held wave")
	}
	mustDo(t, store.SetWavesPaused(context.Background(), testPool, e.project.ID, false, ""))
	e.pass()
	if len(e.issues.added) != 1 || e.issues.added[0] != 13 || e.issues.label != "yuno" || e.status("T3") != store.TicketReleased {
		t.Fatalf("after the revision: labelled %v with %q, T3 %s", e.issues.added, e.issues.label, e.status("T3"))
	}
	e.pass()
	if k := e.kinds(); k[store.PlanEventWaveReleased] != 1 || len(e.issues.added) != 1 {
		t.Fatalf("the held wave was released twice: %v %v", k, e.issues.added)
	}
}
