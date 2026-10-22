package waves

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// ciRequests adds the CI of merge commits (Chief platform control, DESIGN 5.34 rule 4)
// to fakeRequests: one state for every commit.
type ciRequests struct {
	*fakeRequests
	mu    sync.Mutex
	state string
	asked []string
}

func (c *ciRequests) CommitCI(_ context.Context, ref, sha string) (tools.BranchStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, ref+"@"+sha)
	return tools.BranchStatus{State: c.state, SHA: sha}, nil
}

func (c *ciRequests) set(state string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = state
}

// DESIGN 5.34 rule 6: a released wave's ticket whose prerequisite is not satisfied is not
// labelled for intake and stays pending; the watcher labels it once the prerequisite merged
// and the default branch passed on its merge commit (rule 8b's ReleaseSatisfied).
func TestTicketNotReleasedUntilSatisfied(t *testing.T) {
	e := newEnv(t, store.PlanWaitingPlanMerge)
	ctx := context.Background()
	ci := &ciRequests{fakeRequests: e.reqs, state: tools.BranchPending}
	e.svc.Requests = func(*project.Config) tools.RequestProvider { return ci }
	// The Planner's edges: T2 on T1 in the same wave, T3 on T1 in an earlier one.
	mustDo(t, store.ReplacePlannerDependencies(ctx, testPool, e.project.ID, e.plan.ID, [][2]string{{"T2", "T1"}, {"T3", "T1"}}))

	run := e.newRun(store.TriggerManual, "")
	planMR := &store.MergeRequest{ProjectID: e.project.ID, RunID: run.ID, Provider: "gitlab", Number: 1, URL: "u", SourceBranch: "plan", TargetBranch: "main"}
	if ok, err := store.InsertMergeRequest(ctx, testPool, planMR); err != nil || !ok {
		t.Fatalf("insert: %v %v", ok, err)
	}
	mustDo(t, store.SetPlanMR(ctx, testPool, e.plan.ID, planMR.ID))
	if won, err := store.MarkMergeRequestMerged(ctx, testPool, planMR.ID, "sync", time.Now().UTC()); err != nil || !won {
		t.Fatalf("merge: %v %v", won, err)
	}

	// Wave 1 is released: T1 labelled, T2 held back and pending.
	e.pass()
	if p := e.reload(); p.Status != store.PlanBuilding || p.CurrentWave != 1 {
		t.Fatalf("plan = %s wave %d", p.Status, p.CurrentWave)
	}
	if !slices.Equal(e.issues.added, []int64{11}) || e.status("T1") != store.TicketReleased || e.status("T2") != store.TicketPending {
		t.Fatalf("labelled %v; T1 %s, T2 %s", e.issues.added, e.status("T1"), e.status("T2"))
	}
	if k := e.kinds(); k[store.PlanEventWaveReleased] != 1 {
		t.Fatalf("events = %v", k)
	}
	reason := func() string {
		t.Helper()
		list, err := store.DependenciesOf(ctx, testPool, store.DependencyTicket, store.TicketNode(e.plan.ID, "T2"))
		mustDo(t, err)
		if len(list) != 1 || list[0].Reason == nil {
			return ""
		}
		return *list[0].Reason
	}
	if r := reason(); r != "T1 is not merged" {
		t.Fatalf("reason = %q", r)
	}

	// T1 merges; main has not passed on its merge commit yet: T2 still waits, pass after pass.
	e.merged("T1", 21, time.Now().UTC())
	const sha = "abcdef1234567890abcdef1234567890abcdef12"
	_, err := testPool.Exec(ctx, `UPDATE merge_requests SET merge_sha = $2 WHERE id = (SELECT merge_request_id FROM plan_tickets
		WHERE plan_id = $1 AND ticket_id = 'T1')`, e.plan.ID, sha)
	mustDo(t, err)
	e.pass()
	e.pass()
	if e.status("T2") != store.TicketPending || len(e.issues.added) != 1 {
		t.Fatalf("released before main passed: T2 %s, labelled %v", e.status("T2"), e.issues.added)
	}
	if r := reason(); r != "!21 merged; CI on abcdef1 is pending" {
		t.Fatalf("reason = %q", r)
	}
	if len(ci.asked) == 0 || ci.asked[0] != "main@"+sha {
		t.Fatalf("asked %v, want main at the merge commit", ci.asked)
	}

	// Main passes on it: the dependency watcher's call labels T2 and releases it, once.
	ci.set(tools.BranchPassed)
	mustDo(t, e.svc.ReleaseSatisfied(ctx))
	e.plans.Wait()
	if !slices.Equal(e.issues.added, []int64{11, 12}) || e.issues.label != "yuno" || e.status("T2") != store.TicketReleased {
		t.Fatalf("after main passed: labelled %v with %q, T2 %s", e.issues.added, e.issues.label, e.status("T2"))
	}
	list, err := store.DependenciesOf(ctx, testPool, store.DependencyTicket, store.TicketNode(e.plan.ID, "T2"))
	mustDo(t, err)
	if len(list) != 1 || list[0].ClearedAt == nil {
		t.Fatalf("T2's edge = %+v", list)
	}
	e.pass()
	mustDo(t, e.svc.ReleaseSatisfied(ctx))
	if len(e.issues.added) != 2 {
		t.Fatalf("labelled again: %v", e.issues.added)
	}
}
