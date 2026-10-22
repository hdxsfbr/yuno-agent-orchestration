package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"yuno/internal/store"
)

// readSchedule is GET /api/schedules/{id}, and checks GET /api/schedules lists the same.
func readSchedule(t *testing.T, ts *httptest.Server, id string) store.Schedule {
	t.Helper()
	code, body := do(t, ts, http.MethodGet, "/api/schedules/"+id, "")
	if code != http.StatusOK {
		t.Fatalf("get: %d %s", code, body)
	}
	got := decodeSchedule(t, body)
	code, body = do(t, ts, http.MethodGet, "/api/schedules", "")
	var list []store.Schedule
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil {
		t.Fatalf("list: %d %s", code, body)
	}
	i := slices.IndexFunc(list, func(s store.Schedule) bool { return s.ID == id })
	if i < 0 || list[i].Enabled != got.Enabled || list[i].TZ != got.TZ || !sameTime(list[i].NextRunAt, got.NextRunAt) {
		t.Fatalf("the list disagrees with the schedule: %+v vs %+v", list[i], got)
	}
	return got
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func at(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

// DESIGN W4 rule 6: a schedule pauses and resumes through enabled, and exposes its time
// zone (tz) and next_run_at. Each change is read back through GET: paused, it has no next
// run; resumed, its next run is the next occurrence from now in its zone; a new zone moves
// the next run; a zone the parser does not know is refused and changes nothing.
func TestScheduleEnabledTimezoneNextRun(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	pinScheduleClock(t, "2099-10-15T08:00:00Z")
	wf := scheduleWorkflow(t)

	// 09:00 in Tokyo (UTC+9): 17:00 there now, so tomorrow, 00:00 UTC.
	code, body := do(t, ts, http.MethodPost, "/api/schedules",
		`{"workflow_id":"`+wf.ID+`","kind":"cron","cron_expr":"0 9 * * *","tz":"Asia/Tokyo","input":{"task":"t"}}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	id := decodeSchedule(t, body).ID
	t.Cleanup(func() { _ = store.DeleteSchedule(context.Background(), pool, id) })
	if got := readSchedule(t, ts, id); !got.Enabled || got.TZ != "Asia/Tokyo" || !sameTime(got.NextRunAt, at("2099-10-16T00:00:00Z")) {
		t.Fatalf("created: %+v next %v", got, got.NextRunAt)
	}

	// Pause: no next run, and a zone changed while paused keeps it paused.
	if code, body = do(t, ts, http.MethodPut, "/api/schedules/"+id, `{"enabled":false}`); code != http.StatusOK {
		t.Fatalf("pause: %d %s", code, body)
	}
	if got := readSchedule(t, ts, id); got.Enabled || got.NextRunAt != nil || got.TZ != "Asia/Tokyo" {
		t.Fatalf("paused: %+v next %v", got, got.NextRunAt)
	}
	if code, body = do(t, ts, http.MethodPut, "/api/schedules/"+id, `{"tz":"America/New_York"}`); code != http.StatusOK {
		t.Fatalf("zone while paused: %d %s", code, body)
	}
	if got := readSchedule(t, ts, id); got.Enabled || got.NextRunAt != nil || got.TZ != "America/New_York" {
		t.Fatalf("zone while paused: %+v next %v", got, got.NextRunAt)
	}

	// Resume later that day: 10:00 in New York (UTC-4), 09:00 has passed, so tomorrow 13:00 UTC.
	pinScheduleClock(t, "2099-10-15T14:00:00Z")
	if code, body = do(t, ts, http.MethodPut, "/api/schedules/"+id, `{"enabled":true}`); code != http.StatusOK {
		t.Fatalf("resume: %d %s", code, body)
	}
	if got := readSchedule(t, ts, id); !got.Enabled || !sameTime(got.NextRunAt, at("2099-10-16T13:00:00Z")) {
		t.Fatalf("resumed: %+v next %v", got, got.NextRunAt)
	}

	// A new zone while running moves the next run: 09:00 UTC has passed today.
	if code, body = do(t, ts, http.MethodPut, "/api/schedules/"+id, `{"tz":"UTC"}`); code != http.StatusOK {
		t.Fatalf("zone: %d %s", code, body)
	}
	if got := readSchedule(t, ts, id); !got.Enabled || got.TZ != "UTC" || !sameTime(got.NextRunAt, at("2099-10-16T09:00:00Z")) {
		t.Fatalf("new zone: %+v next %v", got, got.NextRunAt)
	}

	// After the clocks change in New York (1 November 2099, UTC-5), 09:00 there is 14:00 UTC.
	pinScheduleClock(t, "2099-11-03T12:00:00Z")
	if code, body = do(t, ts, http.MethodPut, "/api/schedules/"+id, `{"tz":"America/New_York"}`); code != http.StatusOK {
		t.Fatalf("zone after the change: %d %s", code, body)
	}
	if got := readSchedule(t, ts, id); !sameTime(got.NextRunAt, at("2099-11-03T14:00:00Z")) {
		t.Fatalf("after the clocks changed: next %v", got.NextRunAt)
	}

	// An unknown zone is refused with the parser's message and changes nothing.
	code, body = do(t, ts, http.MethodPut, "/api/schedules/"+id, `{"tz":"Mars/Olympus","enabled":false}`)
	if code != http.StatusUnprocessableEntity || !slices.Contains(detailPaths(t, body), "tz") {
		t.Fatalf("unknown zone: %d %s", code, body)
	}
	if got := readSchedule(t, ts, id); !got.Enabled || got.TZ != "America/New_York" || !sameTime(got.NextRunAt, at("2099-11-03T14:00:00Z")) {
		t.Fatalf("a refused change was stored: %+v next %v", got, got.NextRunAt)
	}
}
