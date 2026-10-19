package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"yuno/internal/project"
	"yuno/internal/store"
)

// T5.29.11 (rule 16): publish_tickets in a run with revise_plan amends the plan, on a fake
// GitLab and a fake GitHub.

// reviseEnv is the ticket env plus the routes a revision calls, with a log of what it did to
// which issue ("2 update", "3 close", "2 +yuno").
type reviseEnv struct {
	*ticketEnv
	log    []string
	closed map[int]bool
	planID string
}

func newReviseEnv(t *testing.T, provider project.Provider) *reviseEnv {
	t.Helper()
	e := &reviseEnv{ticketEnv: newTicketEnv(t, provider, testPlanState), closed: map[int]bool{}}
	note := func(format string, args ...any) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.log = append(e.log, fmt.Sprintf(format, args...))
	}
	bodyKey := "description"
	if e.gh {
		bodyKey = "body"
		e.api.on("GET /labels/wave:3", 200, map[string]any{})
	}
	for n := 1; n <= 8; n++ {
		n := n
		issue := fmt.Sprintf("/issues/%d", n)
		edit := func(c issueCall) issueAnswer {
			e.mu.Lock()
			defer e.mu.Unlock()
			if n > len(e.issues) {
				return issueAnswer{404, map[string]string{"message": "404 Not Found"}}
			}
			if v, ok := c.Body["title"]; ok {
				e.issues[n-1]["title"] = v
				e.issues[n-1][bodyKey] = c.Body[bodyKey]
				e.log = append(e.log, fmt.Sprintf("%d update", n))
			}
			if c.Body["state_event"] == "close" || c.Body["state"] == "closed" {
				e.closed[n] = true
				e.log = append(e.log, fmt.Sprintf("%d close", n))
			}
			if c.Body["state_event"] == "reopen" || c.Body["state"] == "open" {
				e.closed[n] = false
				e.log = append(e.log, fmt.Sprintf("%d reopen", n))
			}
			for _, k := range []string{"add_labels", "remove_labels"} {
				if v, ok := c.Body[k]; ok {
					e.log = append(e.log, fmt.Sprintf("%d %s %v", n, k, v))
				}
			}
			return issueAnswer{200, map[string]any{}}
		}
		state := func(issueCall) issueAnswer {
			e.mu.Lock()
			defer e.mu.Unlock()
			open := !e.closed[n]
			s := map[bool]string{true: "open", false: "closed"}[open]
			if !e.gh && open {
				s = "opened"
			}
			return issueAnswer{200, map[string]any{"state": s}}
		}
		e.api.onFunc("GET "+issue, state)
		if e.gh {
			e.api.onFunc("PATCH "+issue, edit)
			e.api.onFunc("POST "+issue+"/comments", func(issueCall) issueAnswer { note("%d comment", n); return issueAnswer{201, map[string]any{}} })
			e.api.onFunc("POST "+issue+"/labels", func(c issueCall) issueAnswer {
				note("%d +%v", n, c.Body["labels"])
				return issueAnswer{200, []any{}}
			})
			for _, l := range []string{"yuno:failed", "yuno:running"} {
				l := l
				e.api.onFunc("DELETE "+issue+"/labels/"+l, func(issueCall) issueAnswer { note("%d -%s", n, l); return issueAnswer{200, []any{}} })
			}
		} else {
			e.api.onFunc("PUT "+issue, edit)
			e.api.onFunc("POST "+issue+"/notes", func(issueCall) issueAnswer { note("%d comment", n); return issueAnswer{201, map[string]any{}} })
		}
	}
	// The original plan: three tickets published, then built up to wave 1 released.
	res, err := e.call(e.run)
	if err != nil || res.IsError {
		t.Fatalf("publish = %+v, %v", res, err)
	}
	var out publishTicketsOutput
	must(t, json.Unmarshal(res.Output, &out))
	e.planID = out.PlanID
	e.log = nil
	return e
}

func (e *reviseEnv) tickets() map[string]store.PlanTicket {
	e.t.Helper()
	rows, err := store.ListPlanTickets(context.Background(), testPool, e.planID)
	must(e.t, err)
	out := map[string]store.PlanTicket{}
	for _, r := range rows {
		out[r.TicketID] = r
	}
	return out
}

func (e *reviseEnv) logged(sub string) int {
	n := 0
	for _, l := range e.log {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// building puts the plan in `building` at wave and records the wave released.
func (e *reviseEnv) building(wave int, statuses map[string]string) {
	ctx := context.Background()
	must(e.t, store.SetPlanStatus(ctx, testPool, e.planID, store.PlanBuilding))
	must(e.t, store.SetCurrentWave(ctx, testPool, e.planID, wave))
	w := wave
	must(e.t, store.AddPlanEvent(ctx, testPool, &store.PlanEvent{PlanID: e.planID, Kind: store.PlanEventWaveReleased, Wave: &w}))
	for id, st := range statuses {
		must(e.t, store.SetTicketStatus(ctx, testPool, e.planID, id, st))
	}
}

func (e *reviseEnv) reviseRun(feature, state string) *store.Run {
	return newRunOf(e.t, e.ticketEnv, fmt.Sprintf(`{"feature":%q,"revise_plan":%q}`, feature, e.planID), state)
}

const revisedState = `{
  "slicer": {"tickets": [
    {"id": "T1", "title": "Walking skeleton", "slice": "End to end.", "files": ["a.go", "a_test.go"], "tests": ["TestA", "TestB"]},
    {"id": "T2", "title": "Second, reworked", "slice": "More, differently.", "files": ["b.go"], "tests": ["TestC"]},
    {"id": "T4", "title": "Fourth", "slice": "New.", "files": ["d.go"], "tests": ["TestE"]}
  ]},
  "waves": {"waves": [{"n": 1, "tickets": ["T1", "T2", "T4"]}]}
}`

func TestPublishTicketsRevisesAPlan(t *testing.T) {
	for _, provider := range []project.Provider{project.ProviderGitLab, project.ProviderGitHub} {
		t.Run(string(provider), func(t *testing.T) {
			ctx := context.Background()
			e := newReviseEnv(t, provider)
			// Wave 1 was released: T1 merged, T2 failed (escalated, the waves paused); T3 waits.
			e.building(1, map[string]string{"T1": store.TicketMerged, "T2": store.TicketFailed})
			must(t, store.SetWavesPaused(ctx, testPool, e.project.ID, true, "escalated"))
			planBefore := len(e.issues)

			run := e.reviseRun("checkout", revisedState)
			res, err := e.call(run)
			if err != nil || res.IsError {
				t.Fatalf("revise = %+v, %v", res, err)
			}
			var out publishTicketsOutput
			must(t, json.Unmarshal(res.Output, &out))
			if !out.Revised || out.PlanID != e.planID || out.Added != 1 || out.Updated != 1 || out.Dropped != 1 || out.Reset != 1 || out.Released != 2 || len(out.Issues) != 3 {
				t.Fatalf("output = %+v", out)
			}
			if len(e.issues) != planBefore+1 {
				t.Fatalf("issues = %d, want %d", len(e.issues), planBefore+1)
			}
			if got := e.issues[3]["title"]; got != "[checkout T4] Fourth" {
				t.Errorf("new issue = %v", got)
			}
			if !strings.Contains(fmt.Sprint(e.issues[3]["labels"]), "wave:1") {
				t.Errorf("new issue labels = %v", e.issues[3]["labels"])
			}
			// T2's issue was updated; T1's (merged) and the new one's were not.
			if e.logged("2 update") != 1 || e.logged("1 update") != 0 || e.logged("4 update") != 0 {
				t.Errorf("log = %v", e.log)
			}
			if !strings.Contains(fmt.Sprint(e.issues[1]["title"]), "Second, reworked") {
				t.Errorf("T2 title = %v", e.issues[1]["title"])
			}
			// T3 was dropped: commented, closed, set closed.
			if e.logged("3 comment") != 1 || e.logged("3 close") != 1 {
				t.Errorf("log = %v", e.log)
			}
			// T2 and T4 carry the intake label again; T2's failed label comes off.
			if e.logged("2 +") != 1 && e.logged("2 add_labels") != 1 || e.logged("4 +") != 1 && e.logged("4 add_labels") != 1 {
				t.Errorf("log = %v", e.log)
			}
			if e.gh && e.logged("2 -yuno:failed") != 1 {
				t.Errorf("log = %v", e.log)
			}
			tk := e.tickets()
			if tk["T1"].Status != store.TicketMerged || tk["T2"].Status != store.TicketReleased || tk["T3"].Status != store.TicketClosed ||
				tk["T4"].Status != store.TicketReleased || tk["T2"].RunID != nil || tk["T4"].IssueNumber == nil {
				t.Fatalf("tickets = %+v", tk)
			}
			paused, err := store.LockProjectPause(ctx, testPool, e.project.ID)
			must(t, err)
			if paused.WavesPaused {
				t.Error("the waves are still paused")
			}
			events, err := store.ListPlanEvents(ctx, testPool, e.planID, 0)
			must(t, err)
			kinds := map[string]int{}
			for _, ev := range events {
				kinds[ev.Kind]++
			}
			if kinds[store.PlanEventRevised] != 1 || kinds[store.PlanEventResumed] != 1 || kinds[store.PlanEventPublished] != 1 {
				t.Errorf("events = %v", kinds)
			}
			plan, err := store.GetPlan(ctx, testPool, e.planID)
			must(t, err)
			if plan.Status != store.PlanBuilding || plan.CurrentWave != 1 {
				t.Errorf("plan = %+v", plan)
			}

			// A retried visit creates no duplicates and updates nothing again.
			logged := len(e.log)
			res, err = e.call(run)
			if err != nil || res.IsError {
				t.Fatalf("retry = %+v, %v", res, err)
			}
			var again publishTicketsOutput
			must(t, json.Unmarshal(res.Output, &again))
			if len(e.issues) != planBefore+1 || len(e.log) != logged || again.Added != 0 || again.Updated != 0 || again.Dropped != 0 || len(again.Issues) != 3 {
				t.Errorf("retry: issues %d, log %v, output %+v", len(e.issues), e.log[logged:], again)
			}
		})
	}
}

// A revision after a rolled-back first attempt finds the issue it created by its marker.
func TestPublishTicketsReviseRetryAfterRollbackReusesNewIssue(t *testing.T) {
	e := newReviseEnv(t, project.ProviderGitLab)
	e.building(1, map[string]string{"T1": store.TicketMerged})
	run := e.reviseRun("checkout", revisedState)
	if res, err := e.call(run); err != nil || res.IsError {
		t.Fatalf("revise = %+v, %v", res, err)
	}
	// The transaction rolled back: the new ticket's row is gone, its issue stays.
	_, err := testPool.Exec(context.Background(), `DELETE FROM plan_tickets WHERE plan_id=$1 AND ticket_id='T4'`, e.planID)
	must(t, err)
	n := len(e.issues)
	res, err := e.call(run)
	if err != nil || res.IsError {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	if len(e.issues) != n || e.tickets()["T4"].IssueNumber == nil {
		t.Fatalf("issues %d, want %d; T4 %+v", len(e.issues), n, e.tickets()["T4"])
	}
}

// Tickets of finished waves are left as they are, and nothing may be placed in them.
func TestPublishTicketsReviseLeavesFinishedWaves(t *testing.T) {
	ctx := context.Background()
	e := newReviseEnv(t, project.ProviderGitHub)
	e.building(2, map[string]string{"T1": store.TicketClosed, "T2": store.TicketClosed})
	e.closed[1], e.closed[2] = true, true
	state := `{
	  "slicer": {"tickets": [
	    {"id": "T1", "title": "Rewritten", "slice": "Other.", "files": ["a.go"], "tests": ["TestA"]},
	    {"id": "T3", "title": "Third, changed", "slice": "Later, changed.", "files": ["c.go"], "tests": ["TestD"]},
	    {"id": "T4", "title": "Fourth", "slice": "New.", "files": ["d.go"], "tests": ["TestE"]}
	  ]},
	  "waves": {"waves": [{"n": 1, "tickets": ["T1"]}, {"n": 2, "tickets": ["T3"]}, {"n": 3, "tickets": ["T4"]}]}
	}`
	before := len(e.issues)
	res, err := e.call(e.reviseRun("checkout", state))
	if err != nil || res.IsError {
		t.Fatalf("revise = %+v, %v", res, err)
	}
	// T1 and T2 (wave 1) are untouched: no update, no close, T2 not dropped although it is
	// left out of the revision.
	if e.logged("1 update") != 0 || e.logged("2 close") != 0 || e.logged("2 comment") != 0 || e.logged("3 update") != 1 {
		t.Errorf("log = %v", e.log)
	}
	if len(e.issues) != before+1 || !strings.Contains(fmt.Sprint(e.issues[3]["labels"]), "wave:3") {
		t.Errorf("issues = %d, labels %v", len(e.issues), e.issues[len(e.issues)-1]["labels"])
	}
	tk := e.tickets()
	if tk["T1"].Title != "Walking skeleton" || tk["T1"].Status != store.TicketClosed || tk["T2"].Status != store.TicketClosed {
		t.Errorf("finished tickets = %+v %+v", tk["T1"], tk["T2"])
	}
	// T3 is still the current wave's ticket; wave 3's ticket is pending until released.
	if tk["T3"].Title != "Third, changed" || tk["T4"].Wave != 3 || tk["T4"].Status != store.TicketPending {
		t.Errorf("open tickets = %+v %+v", tk["T3"], tk["T4"])
	}
	plan, err := store.GetPlan(ctx, testPool, e.planID)
	must(t, err)
	if plan.CurrentWave != 2 {
		t.Errorf("current wave = %d", plan.CurrentWave)
	}

	// A new ticket in a finished wave is refused before any effect.
	bad := `{
	  "slicer": {"tickets": [{"id": "T7", "title": "Late", "slice": "x", "files": ["z.go"], "tests": ["t"]},
	    {"id": "T3", "title": "Third", "slice": "Later.", "files": ["c.go"], "tests": ["TestD"]}]},
	  "waves": {"waves": [{"n": 1, "tickets": ["T7"]}, {"n": 2, "tickets": ["T3"]}]}
	}`
	n := len(e.issues)
	_, err = e.call(e.reviseRun("checkout", bad))
	var me *ModelError
	if !asModelError(err, &me) || !strings.Contains(me.Message, "finished") || len(e.issues) != n {
		t.Fatalf("late ticket: %v (issues %d)", err, len(e.issues))
	}
}

func TestPublishTicketsReviseRefusals(t *testing.T) {
	ctx := context.Background()
	e := newReviseEnv(t, project.ProviderGitLab)
	e.building(1, nil)
	var me *ModelError
	n := len(e.issues)

	// A feature that does not match the plan's.
	_, err := e.call(e.reviseRun("search", testPlanState))
	if !asModelError(err, &me) || !strings.Contains(me.Message, `"checkout"`) || !strings.Contains(me.Message, `"search"`) {
		t.Fatalf("feature mismatch: %v", err)
	}
	// An unknown plan, and something that is no plan id.
	for _, id := range []string{"7d3c4f9e-0000-4000-8000-000000000001", "not-a-plan"} {
		r := newRunOf(t, e.ticketEnv, fmt.Sprintf(`{"feature":"checkout","revise_plan":%q}`, id), testPlanState)
		if _, err := e.call(r); !asModelError(err, &me) {
			t.Errorf("plan %q: %v", id, err)
		}
	}
	// A finished plan.
	must(t, store.SetPlanStatus(ctx, testPool, e.planID, store.PlanDone))
	if _, err := e.call(e.reviseRun("checkout", testPlanState)); !asModelError(err, &me) || !strings.Contains(me.Message, "cannot be revised") {
		t.Fatalf("done plan: %v", err)
	}
	// A plan of another project.
	must(t, store.SetPlanStatus(ctx, testPool, e.planID, store.PlanBuilding))
	other := newReviseEnv(t, project.ProviderGitLab)
	r := newRunOf(t, other.ticketEnv, fmt.Sprintf(`{"feature":"checkout","revise_plan":%q}`, e.planID), testPlanState)
	if _, err := other.call(r); !asModelError(err, &me) || !strings.Contains(me.Message, "another project") {
		t.Fatalf("other project: %v", err)
	}
	if len(e.issues) != n || len(other.log) != 0 {
		t.Errorf("effects: issues %d, log %v", len(e.issues), other.log)
	}
}

func TestPublishTicketsReviseDescribesItself(t *testing.T) {
	e := newReviseEnv(t, project.ProviderGitLab)
	e.building(2, map[string]string{"T1": store.TicketClosed, "T2": store.TicketClosed})
	run := e.reviseRun("checkout", testPlanState)
	e.run = run // RunIDForToken answers with the revise run
	got := e.tool.Describe(json.RawMessage(`{"run_token":"tok"}`))
	for _, want := range []string{"Revise the plan", "wave 1: 2", "Waves before wave 2 are finished"} {
		if !strings.Contains(got, want) {
			t.Errorf("description %q lacks %q", got, want)
		}
	}
}

// shipRun inserts a ship run of the plan's wave with the status, as the wave watcher starts it.
func (e *reviseEnv) shipRun(wave, attempt int, status string) *store.Run {
	e.t.Helper()
	ref := store.ShipAttemptRef(e.planID, wave, attempt)
	wf := &store.Workflow{Name: "ship " + store.NewID(), Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	must(e.t, store.CreateWorkflow(context.Background(), testPool, wf))
	settings := e.project.Settings()
	run := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: store.TriggerPlan, TriggerRef: &ref,
		Status: status, ProjectID: &e.project.ID, Project: &settings, Input: json.RawMessage(`{}`), State: json.RawMessage(`{}`)}
	must(e.t, store.CreateRun(context.Background(), testPool, run))
	dropRunAfter(e.t, run.ID)
	return run
}

func (e *reviseEnv) status() string {
	e.t.Helper()
	p, err := store.GetPlan(context.Background(), testPool, e.planID)
	must(e.t, err)
	return p.Status
}

// H1: a wave under a live ship run is not changed; once the run has ended the revision
// moves the plan back to building so the watcher can start the next attempt.
func TestPublishTicketsReviseHonoursTheShipRun(t *testing.T) {
	ctx := context.Background()
	e := newReviseEnv(t, project.ProviderGitLab)
	e.building(1, map[string]string{"T1": store.TicketMerged, "T2": store.TicketMerged})
	must(t, store.SetPlanStatus(ctx, testPool, e.planID, store.PlanShipping))
	ship := e.shipRun(1, 1, store.RunWaitingApproval)
	n := len(e.issues)

	var me *ModelError
	_, err := e.call(e.reviseRun("checkout", revisedState))
	if !asModelError(err, &me) || !strings.Contains(me.Message, "being shipped") || len(e.issues) != n || e.status() != store.PlanShipping {
		t.Fatalf("live ship run: %v (issues %d, plan %s)", err, len(e.issues), e.status())
	}

	_, err = testPool.Exec(ctx, `UPDATE runs SET status='failed' WHERE id=$1`, ship.ID)
	must(t, err)
	res, err := e.call(e.reviseRun("checkout", revisedState))
	if err != nil || res.IsError {
		t.Fatalf("revise after the run ended = %+v, %v", res, err)
	}
	if e.status() != store.PlanBuilding {
		t.Errorf("plan = %s, want building", e.status())
	}
}

// L6: tickets added only to a later wave leave a shipping plan alone.
func TestPublishTicketsReviseLaterWaveKeepsShipping(t *testing.T) {
	e := newReviseEnv(t, project.ProviderGitLab)
	e.building(1, map[string]string{"T1": store.TicketMerged, "T2": store.TicketMerged})
	must(t, store.SetPlanStatus(context.Background(), testPool, e.planID, store.PlanShipping))
	e.shipRun(1, 1, store.RunWaitingApproval)
	state := `{
	  "slicer": {"tickets": [
	    {"id": "T1", "title": "Walking skeleton", "slice": "End to end.", "files": ["a.go", "a_test.go"], "tests": ["TestA", "TestB"]},
	    {"id": "T2", "title": "Second", "slice": "More.", "files": ["b.go"], "tests": ["TestC"]},
	    {"id": "T3", "title": "Third", "slice": "Later.", "files": ["c.go"], "tests": ["TestD"]},
	    {"id": "T4", "title": "Fourth", "slice": "New.", "files": ["d.go"], "tests": ["TestE"]}
	  ]},
	  "waves": {"waves": [{"n": 1, "tickets": ["T1", "T2"]}, {"n": 2, "tickets": ["T3", "T4"]}]}
	}`
	res, err := e.call(e.reviseRun("checkout", state))
	if err != nil || res.IsError {
		t.Fatalf("revise = %+v, %v", res, err)
	}
	if e.status() != store.PlanShipping || e.tickets()["T4"].Wave != 2 {
		t.Fatalf("plan = %s, T4 = %+v", e.status(), e.tickets()["T4"])
	}
}

// M2: a failed ticket whose issue was closed gets the issue back open.
func TestPublishTicketsReviseReopensAClosedIssue(t *testing.T) {
	for _, provider := range []project.Provider{project.ProviderGitLab, project.ProviderGitHub} {
		t.Run(string(provider), func(t *testing.T) {
			e := newReviseEnv(t, provider)
			e.building(1, map[string]string{"T1": store.TicketMerged, "T2": store.TicketFailed})
			e.closed[2] = true
			res, err := e.call(e.reviseRun("checkout", revisedState))
			if err != nil || res.IsError {
				t.Fatalf("revise = %+v, %v", res, err)
			}
			if e.logged("2 reopen") != 1 || e.closed[2] {
				t.Errorf("log = %v, closed = %v", e.log, e.closed)
			}
			if e.logged("4 reopen") != 0 || e.logged("1 reopen") != 0 {
				t.Errorf("log = %v", e.log)
			}
		})
	}
}

// M5: a released ticket moved to a later wave loses the intake label and waits; a ticket
// with a run is not moved.
func TestPublishTicketsReviseMovesAReleasedTicketLater(t *testing.T) {
	e := newReviseEnv(t, project.ProviderGitLab)
	e.building(1, map[string]string{"T1": store.TicketReleased, "T2": store.TicketReleased})
	moved := `{
	  "slicer": {"tickets": [
	    {"id": "T1", "title": "Walking skeleton", "slice": "End to end.", "files": ["a.go", "a_test.go"], "tests": ["TestA", "TestB"]},
	    {"id": "T2", "title": "Second", "slice": "More.", "files": ["b.go"], "tests": ["TestC"]},
	    {"id": "T3", "title": "Third", "slice": "Later.", "files": ["c.go"], "tests": ["TestD"]}
	  ]},
	  "waves": {"waves": [{"n": 1, "tickets": ["T1"]}, {"n": 2, "tickets": ["T2", "T3"]}]}
	}`
	res, err := e.call(e.reviseRun("checkout", moved))
	if err != nil || res.IsError {
		t.Fatalf("revise = %+v, %v", res, err)
	}
	tk := e.tickets()
	if tk["T2"].Wave != 2 || tk["T2"].Status != store.TicketPending || tk["T1"].Status != store.TicketReleased {
		t.Fatalf("tickets = T1 %s, T2 %s wave %d", tk["T1"].Status, tk["T2"].Status, tk["T2"].Wave)
	}
	if e.logged("2 remove_labels") == 0 || !strings.Contains(strings.Join(e.log, "|"), "yuno") {
		t.Errorf("log = %v", e.log)
	}

	// A ticket that is being built stays where it is: the move is refused before any effect.
	e2 := newReviseEnv(t, project.ProviderGitLab)
	e2.building(1, map[string]string{"T2": store.TicketBuilding})
	n := len(e2.log)
	var me *ModelError
	if _, err := e2.call(e2.reviseRun("checkout", moved)); !asModelError(err, &me) || !strings.Contains(me.Message, "cannot be moved") || len(e2.log) != n {
		t.Fatalf("building ticket moved: %v (log %v)", err, e2.log)
	}
}

// L3: a revision that leaves nothing to build is refused.
func TestPublishTicketsReviseRefusesNoWork(t *testing.T) {
	e := newReviseEnv(t, project.ProviderGitLab)
	e.building(2, map[string]string{"T1": store.TicketClosed, "T2": store.TicketClosed})
	only := `{
	  "slicer": {"tickets": [{"id": "T1", "title": "Walking skeleton", "slice": "End to end.", "files": ["a.go"], "tests": ["TestA"]}]},
	  "waves": {"waves": [{"n": 1, "tickets": ["T1"]}]}
	}`
	var me *ModelError
	if _, err := e.call(e.reviseRun("checkout", only)); !asModelError(err, &me) || !strings.Contains(me.Message, "no ticket to build") {
		t.Fatalf("empty revision: %v", err)
	}
	if e.logged("close") != 0 || e.status() != store.PlanBuilding {
		t.Errorf("log %v, plan %s", e.log, e.status())
	}
}

// L2 and L4: the approval text names the in-flight tickets whose text changes and the pause
// the revision lifts.
func TestPublishTicketsReviseDescribesInFlightAndPause(t *testing.T) {
	ctx := context.Background()
	e := newReviseEnv(t, project.ProviderGitLab)
	e.building(1, map[string]string{"T1": store.TicketMerged, "T2": store.TicketInReview})
	must(t, store.SetWavesPaused(ctx, testPool, e.project.ID, true, "freeze until Monday"))
	e.run = e.reviseRun("checkout", revisedState)
	got := e.tool.Describe(json.RawMessage(`{"run_token":"tok"}`))
	for _, want := range []string{"The text of T2 changes", "request changes on its merge request", "paused: freeze until Monday"} {
		if !strings.Contains(got, want) {
			t.Errorf("description %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "T1") {
		t.Errorf("description names a merged ticket: %q", got)
	}
	res, err := e.call(e.run)
	if err != nil || res.IsError {
		t.Fatalf("revise = %+v, %v", res, err)
	}
	var out publishTicketsOutput
	must(t, json.Unmarshal(res.Output, &out))
	if len(out.ChangedInFlight) != 1 || out.ChangedInFlight[0] != "T2" {
		t.Errorf("changed in flight = %v", out.ChangedInFlight)
	}
	// Without a change or a pause the description says neither.
	e2 := newReviseEnv(t, project.ProviderGitLab)
	e2.building(1, map[string]string{"T2": store.TicketInReview})
	e2.run = e2.reviseRun("checkout", testPlanState)
	if got := e2.tool.Describe(json.RawMessage(`{"run_token":"tok"}`)); strings.Contains(got, "request changes") || strings.Contains(got, "paused") {
		t.Errorf("description = %q", got)
	}
}

// L5: the issues keep the plan's first merge request link, so a revision with its own
// request rewrites only the tickets that changed.
func TestPublishTicketsReviseKeepsThePlanMRLink(t *testing.T) {
	ctx := context.Background()
	e := newReviseEnv(t, project.ProviderGitLab)
	if ok, err := store.InsertMergeRequest(ctx, testPool, &store.MergeRequest{ProjectID: e.project.ID, RunID: e.run.ID, Provider: "gitlab",
		Number: 1, URL: "https://gitlab.test/group/demo/-/merge_requests/1", SourceBranch: "yuno/plan", TargetBranch: "main"}); err != nil || !ok {
		t.Fatalf("plan MR: %v %v", ok, err)
	}
	mr, err := store.MergeRequestByRun(ctx, testPool, e.run.ID)
	must(t, err)
	must(t, store.SetPlanMR(ctx, testPool, e.planID, mr.ID))
	e.building(1, nil)
	// A first revision writes the plan's link into the issues.
	if res, err := e.call(e.reviseRun("checkout", testPlanState)); err != nil || res.IsError {
		t.Fatalf("first revision = %+v, %v", res, err)
	}
	if !strings.Contains(e.body(0), "merge_requests/1") {
		t.Fatalf("issue body lacks the plan's link: %s", e.body(0))
	}
	// A second one has a merge request of its own and changes nothing.
	run := e.reviseRun("checkout", testPlanState)
	if ok, err := store.InsertMergeRequest(ctx, testPool, &store.MergeRequest{ProjectID: e.project.ID, RunID: run.ID, Provider: "gitlab",
		Number: 2, URL: "https://gitlab.test/group/demo/-/merge_requests/2", SourceBranch: "yuno/revision", TargetBranch: "main"}); err != nil || !ok {
		t.Fatalf("revision MR: %v %v", ok, err)
	}
	e.log = nil
	res, err := e.call(run)
	if err != nil || res.IsError {
		t.Fatalf("second revision = %+v, %v", res, err)
	}
	var out publishTicketsOutput
	must(t, json.Unmarshal(res.Output, &out))
	if out.Updated != 0 || e.logged("update") != 0 {
		t.Errorf("updated %d, log %v", out.Updated, e.log)
	}
}
