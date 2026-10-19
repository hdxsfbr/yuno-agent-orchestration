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
