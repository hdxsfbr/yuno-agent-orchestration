package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"yuno/internal/plans"
	"yuno/internal/store"
)

// T5.29.7: the plans endpoints, the pause and resume with their events and the SSE event.

func seedPlan(t *testing.T, p *store.Project) (*store.Plan, []store.PlanTicket) {
	t.Helper()
	ctx := context.Background()
	run := projectRun(t, p)
	plan := &store.Plan{ProjectID: p.ID, Feature: "search-" + uniqueSuffix(), PlanRunID: run.ID, Status: store.PlanBuilding}
	mustDo(t, store.CreatePlan(ctx, testPool, plan))
	var tickets []store.PlanTicket
	for i, id := range []string{"T1", "T2", "T3"} {
		n := int64(10 + i)
		tk := store.PlanTicket{PlanID: plan.ID, TicketID: id, Wave: 1 + i/2, Title: "Ticket " + id, IssueNumber: &n, IssueURL: "https://gitlab.test/g/d/-/issues/" + id}
		mustDo(t, store.UpsertPlanTicket(ctx, testPool, &tk))
		tickets = append(tickets, tk)
	}
	return plan, tickets
}

func TestPlansAPI(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	srv, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	router := contract(t)
	sfx := uniqueSuffix()
	p := seedProject(t, testCipher(t), "plans-"+sfx, acceptedTokenPrefix+sfx)
	empty := seedProject(t, testCipher(t), "plans-empty-"+sfx, acceptedTokenPrefix+sfx)
	call := func(method, path, body string, valid bool) (int, []byte) {
		t.Helper()
		return checkConformance(t, router, ts.URL, method, path, body, valid)
	}

	// Lists: [] for none, 404 for an unknown project.
	if code, body := call(http.MethodGet, "/api/projects/"+empty.ID+"/plans", "", true); code != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("empty list: %d %s", code, body)
	}
	if code, body := call(http.MethodGet, "/api/projects/no-such/plans", "", true); code != http.StatusNotFound {
		t.Fatalf("unknown project: %d %s", code, body)
	}

	plan, tickets := seedPlan(t, p)
	mustDo(t, store.SetPlanStatus(ctx, testPool, plan.ID, store.PlanDone))
	open, _ := seedPlan(t, p)
	code, body := call(http.MethodGet, "/api/projects/"+p.ID+"/plans", "", true)
	var list []store.Plan
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil || len(list) != 2 || list[0].ID != open.ID || list[1].ID != plan.ID {
		t.Fatalf("list newest first, finished included: %d %s", code, body)
	}

	// Detail: plan, tickets by wave, events newest first.
	older := time.Now().UTC().Add(-time.Hour)
	mustDo(t, store.AddPlanEvent(ctx, testPool, &store.PlanEvent{PlanID: open.ID, Kind: store.PlanEventPublished, At: older}))
	mustDo(t, store.AddPlanEvent(ctx, testPool, &store.PlanEvent{PlanID: open.ID, Kind: store.PlanEventWaveReleased, At: older.Add(time.Minute),
		Detail: json.RawMessage(`{"wave":1}`)}))
	code, body = call(http.MethodGet, "/api/plans/"+open.ID, "", true)
	var d struct {
		Plan    store.Plan         `json:"plan"`
		Tickets []store.PlanTicket `json:"tickets"`
		Events  []store.PlanEvent  `json:"events"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &d) != nil || d.Plan.ID != open.ID || len(d.Tickets) != 3 || len(d.Events) != 2 ||
		d.Events[0].Kind != store.PlanEventWaveReleased || d.Events[1].Kind != store.PlanEventPublished ||
		d.Tickets[0].TicketID != tickets[0].TicketID || d.Tickets[2].Wave != 2 {
		t.Fatalf("detail: %d %s", code, body)
	}
	if !strings.Contains(string(body), `"main":null`) || !strings.Contains(string(body), `"ship_runs":[]`) {
		t.Fatalf("a plan the watcher has not read has main null and no ship runs: %s", body)
	}
	// Rule 11 and 12: the watcher's last read of the default branch and the wave's ship run.
	at := time.Now().UTC().Truncate(time.Second)
	if _, err := store.SetPlanMain(ctx, testPool, open.ID, store.PlanMain{State: "passed", SHA: "abc", At: &at, URL: "https://ci.test/9"}); err != nil {
		t.Fatal(err)
	}
	shipWF := &store.Workflow{Name: "ship " + store.NewID(), Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(t, store.CreateWorkflow(ctx, testPool, shipWF))
	ref := store.ShipRef(open.ID, 1)
	ship := &store.Run{WorkflowID: shipWF.ID, WorkflowVersion: 1, WorkflowSnapshot: shipWF.Definition, Trigger: store.TriggerPlan, TriggerRef: &ref}
	mustDo(t, store.CreateRun(ctx, testPool, ship))
	code, body = call(http.MethodGet, "/api/plans/"+open.ID, "", true)
	var d2 struct {
		Main     *store.PlanMain `json:"main"`
		ShipRuns []store.ShipRun `json:"ship_runs"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &d2) != nil || d2.Main == nil || d2.Main.State != "passed" || d2.Main.SHA != "abc" ||
		d2.Main.At == nil || !d2.Main.At.Equal(at) || len(d2.ShipRuns) != 1 || d2.ShipRuns[0].Wave != 1 || d2.ShipRuns[0].RunID != ship.ID {
		t.Fatalf("detail with main and ship runs: %d %s", code, body)
	}
	if code, _ := call(http.MethodGet, "/api/plans/no-such", "", true); code != http.StatusNotFound {
		t.Fatalf("unknown plan: %d", code)
	}

	// Pause: validation, the project back with the flag, an event on the open plan, SSE.
	sub, cancel := srv.hub.Subscribe("", 16)
	defer cancel()
	for _, bad := range []string{``, `{}`, `{"reason":""}`, `{"reason":"   "}`, `{"reason":"` + strings.Repeat("x", 501) + `"}`, `{"reason":"r","extra":1}`, `{"reason":1}`} {
		if code, body := call(http.MethodPost, "/api/projects/"+p.ID+"/waves/pause", bad, false); code != http.StatusUnprocessableEntity {
			t.Fatalf("pause %q: %d %s", bad, code, body)
		}
	}
	if code, _ := call(http.MethodPost, "/api/projects/no-such/waves/pause", `{"reason":"r"}`, true); code != http.StatusNotFound {
		t.Fatalf("pause of unknown project: %d", code)
	}
	code, body = call(http.MethodPost, "/api/projects/"+p.ID+"/waves/pause", `{"reason":"hold for the demo"}`, true)
	got := decodeProject(t, body)
	if code != http.StatusOK || got.ID != p.ID || !got.WavesPaused || got.WavesPausedReason == nil || *got.WavesPausedReason != "hold for the demo" {
		t.Fatalf("pause: %d %s", code, body)
	}
	select {
	case ev := <-sub:
		if ev.Type != plans.EventPlanUpdated || !strings.Contains(string(ev.Data), open.ID) {
			t.Fatalf("event = %s %s", ev.Type, ev.Data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no plan.updated event")
	}
	code, body = call(http.MethodGet, "/api/projects/"+p.ID, "", true)
	if got = decodeProject(t, body); code != http.StatusOK || !got.WavesPaused {
		t.Fatalf("project GET: %d %s", code, body)
	}
	// A second pause replaces the reason; resume clears it; resuming again succeeds.
	call(http.MethodPost, "/api/projects/"+p.ID+"/waves/pause", `{"reason":"second"}`, true)
	code, body = call(http.MethodPost, "/api/projects/"+p.ID+"/waves/resume", "", true)
	if got = decodeProject(t, body); code != http.StatusOK || got.WavesPaused || got.WavesPausedReason != nil {
		t.Fatalf("resume: %d %s", code, body)
	}
	if code, body = call(http.MethodPost, "/api/projects/"+p.ID+"/waves/resume", "", true); code != http.StatusOK {
		t.Fatalf("resume again: %d %s", code, body)
	}
	if code, _ = call(http.MethodPost, "/api/projects/no-such/waves/resume", "", true); code != http.StatusNotFound {
		t.Fatalf("resume of unknown project: %d", code)
	}
	evs, err := store.ListPlanEvents(ctx, testPool, open.ID, 0)
	mustDo(t, err)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "resumed,resumed,paused,paused,wave_released,published" {
		t.Fatalf("events = %v", kinds)
	}
	// The finished plan got none, and the project without a plan changes its flag only.
	if old, _ := store.ListPlanEvents(ctx, testPool, plan.ID, 0); len(old) != 0 {
		t.Fatalf("finished plan events = %+v", old)
	}
	if code, _ := call(http.MethodPost, "/api/projects/"+empty.ID+"/waves/pause", `{"reason":"r"}`, true); code != http.StatusOK {
		t.Fatalf("pause without a plan: %d", code)
	}
}

// Review M9: POST /api/plans/{id}/cancel.
func TestCancelPlanAPI(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	router := contract(t)
	call := func(method, path, body string, valid bool) (int, []byte) {
		t.Helper()
		return checkConformance(t, router, ts.URL, method, path, body, valid)
	}
	sfx := uniqueSuffix()
	p := seedProject(t, testCipher(t), "plans-cancel-"+sfx, acceptedTokenPrefix+sfx)
	plan, _ := seedPlan(t, p)

	if code, _ := call(http.MethodPost, "/api/plans/no-such/cancel", `{"reason":"x"}`, true); code != http.StatusNotFound {
		t.Fatalf("unknown plan = %d", code)
	}
	if code, _ := call(http.MethodPost, "/api/plans/"+plan.ID+"/cancel", `{}`, false); code != http.StatusUnprocessableEntity {
		t.Fatalf("no reason = %d", code)
	}
	code, body := call(http.MethodPost, "/api/plans/"+plan.ID+"/cancel", `{"reason":"dropped"}`, true)
	var got store.Plan
	if code != http.StatusOK || json.Unmarshal(body, &got) != nil || got.Status != store.PlanCancelled {
		t.Fatalf("cancel = %d %s", code, body)
	}
	evs, err := store.ListPlanEvents(ctx, testPool, plan.ID, 0)
	if err != nil || len(evs) != 1 || evs[0].Kind != store.PlanEventCancelled {
		t.Fatalf("events = %+v %v", evs, err)
	}
	if code, body := call(http.MethodPost, "/api/plans/"+plan.ID+"/cancel", `{"reason":"again"}`, true); code != http.StatusConflict || !strings.Contains(string(body), "plan_finished") {
		t.Fatalf("second cancel = %d %s", code, body)
	}
}

// Changes requested: POST /api/plans/{id}/tickets/{ticket}/changes.
func TestRequestChangesAPI(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	eng := &fakeEngine{pool: pool}
	_, ts := newTestServer(t, pool, eng, nil)
	router := contract(t)
	call := func(path, body string, valid bool) (int, []byte) {
		t.Helper()
		return checkConformance(t, router, ts.URL, http.MethodPost, path, body, valid)
	}
	sfx := uniqueSuffix()
	p := seedProject(t, testCipher(t), "plans-changes-"+sfx, acceptedTokenPrefix+sfx)
	waveKey := "wave_pipeline"
	wf := &store.Workflow{Name: "Wave pipeline " + sfx, FromTemplate: &waveKey,
		Definition: json.RawMessage(`{"nodes":[{"id":"agent","type":"agent"}],"edges":[],"project":true}`)}
	mustDo(t, store.CreateWorkflow(ctx, testPool, wf))

	plan, tickets := seedPlan(t, p)
	finished := func() *store.Run {
		r := projectRun(t, p)
		mustDo(t, store.SetRunStatus(ctx, testPool, r.ID, store.RunSucceeded, nil))
		return r
	}
	opener := finished()
	// The run that built T1 kept the issue as its task.
	built, err := createTestRun(ctx, pool, store.TriggerIssue, json.RawMessage(`{"task":"Work on issue #10: Ticket T1 body"}`), store.Limits{})
	mustDo(t, err)
	inReview := func(tk store.PlanTicket, runID string) *store.MergeRequest {
		t.Helper()
		mr := &store.MergeRequest{ProjectID: p.ID, RunID: runID, Provider: "gitlab", Number: int64(tk.Wave*100 + int(tk.TicketID[1]-'0')),
			URL: "https://gitlab.test/g/d/-/merge_requests/" + tk.TicketID, SourceBranch: "yuno/" + tk.TicketID, TargetBranch: "main", Title: tk.Title}
		ok, err := store.InsertMergeRequest(ctx, testPool, mr)
		if err != nil || !ok {
			t.Fatalf("insert the request: %v %v", ok, err)
		}
		mustDo(t, store.SetTicketMR(ctx, testPool, plan.ID, tk.TicketID, mr.ID))
		mustDo(t, store.SetTicketStatus(ctx, testPool, plan.ID, tk.TicketID, store.TicketInReview))
		return mr
	}
	path := func(ticket string) string { return "/api/plans/" + plan.ID + "/tickets/" + ticket + "/changes" }

	if code, _ := call("/api/plans/no-such/tickets/T1/changes", `{"notes":"x"}`, true); code != http.StatusNotFound {
		t.Fatalf("unknown plan = %d", code)
	}
	if code, _ := call(path("T9"), `{"notes":"x"}`, true); code != http.StatusNotFound {
		t.Fatalf("unknown ticket = %d", code)
	}
	if code, _ := call(path("T1"), `{"notes":"  "}`, false); code != http.StatusUnprocessableEntity {
		t.Fatalf("no notes = %d", code)
	}
	// No merge request yet.
	if code, body := call(path("T1"), `{"notes":"x"}`, true); code != http.StatusConflict || !strings.Contains(string(body), "no_open_merge_request") {
		t.Fatalf("no merge request = %d %s", code, body)
	}

	// Happy path.
	mustDo(t, store.SetTicketRun(ctx, testPool, plan.ID, "T1", built.ID))
	mr := inReview(tickets[0], opener.ID)
	code, body := call(path("T1"), `{"notes":"Handle the empty query"}`, true)
	var run store.Run
	if code != http.StatusCreated || json.Unmarshal(body, &run) != nil || run.ID == "" {
		t.Fatalf("request changes = %d %s", code, body)
	}
	if len(eng.triggers) != 1 || eng.triggers[0].MergeRequestID != mr.ID || eng.triggers[0].WorkflowID == "" || eng.triggers[0].ProjectID != p.ID {
		t.Fatalf("trigger = %+v", eng.triggers)
	}
	var in struct{ Task string }
	task := string(eng.triggers[0].Input)
	if json.Unmarshal(eng.triggers[0].Input, &in) != nil || !strings.Contains(in.Task, "Ticket T1 body") || !strings.HasSuffix(in.Task, "Changes requested: Handle the empty query") {
		t.Fatalf("task = %s", task)
	}
	tks, err := store.ListPlanTickets(ctx, testPool, plan.ID)
	mustDo(t, err)
	if tks[0].Status != store.TicketBuilding || tks[0].RunID == nil || *tks[0].RunID != run.ID {
		t.Fatalf("ticket = %+v", tks[0])
	}
	evs, err := store.ListPlanEvents(ctx, testPool, plan.ID, 0)
	if err != nil || len(evs) != 1 || evs[0].Kind != "changes_requested" || evs[0].TicketID == nil || *evs[0].TicketID != "T1" ||
		!strings.Contains(string(evs[0].Detail), "Handle the empty query") {
		t.Fatalf("events = %+v %v", evs, err)
	}
	// The ticket is building now, so a second request has no merge request in review.
	if code, body := call(path("T1"), `{"notes":"again"}`, true); code != http.StatusConflict || !strings.Contains(string(body), "no_open_merge_request") {
		t.Fatalf("second request = %d %s", code, body)
	}
	// The iteration pushed onto the request (open_merge_request's update path): the ticket
	// is back in review and a second change request succeeds (review M1).
	if moved, err := store.ReturnTicketToReview(ctx, testPool, run.ID); err != nil || !moved {
		t.Fatalf("return to review: %v %v", moved, err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE runs SET status='succeeded' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if code, body := call(path("T1"), `{"notes":"once more"}`, true); code != http.StatusCreated {
		t.Fatalf("second request after the iteration = %d %s", code, body)
	}

	// A merge request that is merged is no open request.
	mrT2 := inReview(tickets[1], finished().ID)
	if ok, err := store.MarkMergeRequestMerged(ctx, testPool, mrT2.ID, "operator", time.Now()); err != nil || !ok {
		t.Fatalf("merge: %v %v", ok, err)
	}
	if code, body := call(path("T2"), `{"notes":"x"}`, true); code != http.StatusConflict || !strings.Contains(string(body), "no_open_merge_request") {
		t.Fatalf("merged = %d %s", code, body)
	}

	// A live run on the request.
	mrT3 := inReview(tickets[2], finished().ID)
	live := projectRun(t, p)
	_, err = testPool.Exec(ctx, `UPDATE runs SET merge_request_id=$2 WHERE id=$1`, live.ID, mrT3.ID)
	mustDo(t, err)
	if code, body := call(path("T3"), `{"notes":"x"}`, true); code != http.StatusConflict || !strings.Contains(string(body), "run_live") {
		t.Fatalf("live run = %d %s", code, body)
	}
	if len(eng.triggers) != 2 { // the two accepted requests only
		t.Fatalf("refusals started runs: %d", len(eng.triggers))
	}

	// A finished plan.
	mustDo(t, store.SetPlanStatus(ctx, testPool, plan.ID, store.PlanCancelled))
	if code, body := call(path("T3"), `{"notes":"x"}`, true); code != http.StatusConflict || !strings.Contains(string(body), "plan_finished") {
		t.Fatalf("finished plan = %d %s", code, body)
	}
}
