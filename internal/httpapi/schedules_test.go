package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"yuno/internal/store"
)

// pinScheduleClock fixes the clock schedule writes compute next_run_at from. The
// pinned instant is in the future so the scheduler package's tests, which tick with a
// clock in 2001, never find these schedules due.
func pinScheduleClock(t *testing.T, at string) time.Time {
	t.Helper()
	now, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatal(err)
	}
	prev := scheduleNow
	scheduleNow = func() time.Time { return now }
	t.Cleanup(func() { scheduleNow = prev })
	return now
}

func scheduleWorkflow(t *testing.T) *store.Workflow {
	t.Helper()
	wf := &store.Workflow{Name: "scheduled", Definition: json.RawMessage(validDefinition(testAgent(t, "sched-agent")))}
	mustDo(t, store.CreateWorkflow(context.Background(), testPool, wf))
	return wf
}

func decodeSchedule(t *testing.T, b []byte) store.Schedule {
	t.Helper()
	var s store.Schedule
	if err := json.Unmarshal(b, &s); err != nil || s.ID == "" {
		t.Fatalf("not a schedule: %s", b)
	}
	return s
}

func TestScheduleCRUD(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	pinScheduleClock(t, "2099-10-15T08:00:00Z")
	wf := scheduleWorkflow(t)

	// Create: next_run_at is the next occurrence in the schedule's zone (rule 1).
	code, body := do(t, ts, http.MethodPost, "/api/schedules",
		`{"workflow_id":"`+wf.ID+`","kind":"cron","cron_expr":"0 9 * * *","tz":"America/Sao_Paulo","input":{"task":"nightly report"}}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	s := decodeSchedule(t, body)
	if !s.Enabled || s.WorkflowName != "scheduled" || s.Kind != "cron" || s.TZ != "America/Sao_Paulo" ||
		s.NextRunAt == nil || !s.NextRunAt.Equal(time.Date(2099, 10, 15, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("created = %+v (next %v)", s, s.NextRunAt)
	}
	if !strings.Contains(string(s.Input), "nightly report") {
		t.Fatalf("input = %s", s.Input)
	}

	code, body = do(t, ts, http.MethodGet, "/api/schedules/"+s.ID, "")
	if code != http.StatusOK || decodeSchedule(t, body).ID != s.ID {
		t.Fatalf("get: %d %s", code, body)
	}
	code, body = do(t, ts, http.MethodGet, "/api/schedules", "")
	var list []store.Schedule
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil || !slices.ContainsFunc(list, func(x store.Schedule) bool { return x.ID == s.ID }) {
		t.Fatalf("list: %d %s", code, body)
	}

	// The enabled toggle: fields left out keep their value; disabled has no next run.
	code, body = do(t, ts, http.MethodPut, "/api/schedules/"+s.ID, `{"enabled":false}`)
	if got := decodeSchedule(t, body); code != http.StatusOK || got.Enabled || got.NextRunAt != nil || got.CronExpr == nil || *got.CronExpr != "0 9 * * *" {
		t.Fatalf("disable: %d %s", code, body)
	}
	pinScheduleClock(t, "2099-10-15T13:00:00Z")
	code, body = do(t, ts, http.MethodPut, "/api/schedules/"+s.ID, `{"enabled":true}`)
	if got := decodeSchedule(t, body); code != http.StatusOK || !got.Enabled || got.NextRunAt == nil ||
		!got.NextRunAt.Equal(time.Date(2099, 10, 16, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("enable recomputes from now: %d %s", code, body)
	}

	// PUT back what was fetched, switched to a delay: the cron expression goes.
	_, body = do(t, ts, http.MethodGet, "/api/schedules/"+s.ID, "")
	var back map[string]any
	mustDo(t, json.Unmarshal(body, &back))
	back["kind"], back["run_at"] = "delay", "2099-12-24T18:00:00-03:00"
	raw, _ := json.Marshal(back)
	code, body = do(t, ts, http.MethodPut, "/api/schedules/"+s.ID, string(raw))
	got := decodeSchedule(t, body)
	if code != http.StatusOK || got.Kind != "delay" || got.CronExpr != nil || got.RunAt == nil ||
		!got.NextRunAt.Equal(time.Date(2099, 12, 24, 21, 0, 0, 0, time.UTC)) {
		t.Fatalf("put back as delay: %d %s", code, body)
	}

	code, _ = do(t, ts, http.MethodDelete, "/api/schedules/"+s.ID, "")
	if code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		body := ""
		if m == http.MethodPut {
			body = `{"enabled":true}`
		}
		if code, _ := do(t, ts, m, "/api/schedules/"+s.ID, body); code != http.StatusNotFound {
			t.Fatalf("%s deleted: %d", m, code)
		}
	}
}

// T5.9.4: invalid cron or zone is a 422 carrying the parser's message, and every other
// field problem is path-qualified.
func TestScheduleValidation(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	pinScheduleClock(t, "2099-10-15T08:00:00Z")
	wf := scheduleWorkflow(t)
	tpl := "tpl_" + uniqueSuffix()
	template := &store.Workflow{Name: "template", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`), TemplateKey: &tpl}
	mustDo(t, store.CreateWorkflow(context.Background(), pool, template))
	base := `"workflow_id":"` + wf.ID + `","input":{"task":"t"}`
	for _, tc := range []struct {
		name, body string
		want       map[string]string
	}{
		{"bad cron", `{` + base + `,"kind":"cron","cron_expr":"61 * * * *"}`,
			map[string]string{"cron_expr": "end of range (61) above maximum (59): 61"}},
		{"too few fields", `{` + base + `,"kind":"cron","cron_expr":"* * *"}`,
			map[string]string{"cron_expr": "expected exactly 5 fields, found 3: [* * *]"}},
		{"sub-minute", `{` + base + `,"kind":"cron","cron_expr":"@every 10s"}`,
			map[string]string{"cron_expr": "must not fire more often than every 1m0s"}},
		{"no future occurrence", `{` + base + `,"kind":"cron","cron_expr":"0 0 31 2 *"}`,
			map[string]string{"cron_expr": "no future occurrence"}},
		{"paused, no future occurrence", `{` + base + `,"kind":"cron","cron_expr":"0 0 30 2 *","enabled":false}`,
			map[string]string{"cron_expr": "no future occurrence"}},
		{"unknown zone", `{` + base + `,"kind":"cron","cron_expr":"0 9 * * *","tz":"Mars/Olympus"}`,
			map[string]string{"tz": "unknown time zone Mars/Olympus"}},
		{"missing cron", `{` + base + `,"kind":"cron"}`, map[string]string{"cron_expr": "is required for a cron schedule"}},
		{"missing run_at", `{` + base + `,"kind":"delay"}`, map[string]string{"run_at": "is required for a delay schedule"}},
		{"bad run_at", `{` + base + `,"kind":"delay","run_at":"tomorrow"}`, map[string]string{"run_at": "must be an RFC 3339 timestamp such as 2026-10-15T09:00:00Z"}},
		{"kind", `{` + base + `,"kind":"hourly"}`, map[string]string{"kind": "must be one of cron, delay"}},
		{"no kind, no workflow", `{}`, map[string]string{"kind": "is required", "workflow_id": "is required"}},
		{"unknown workflow", `{"workflow_id":"nope","kind":"cron","cron_expr":"0 9 * * *"}`, map[string]string{"workflow_id": "no workflow with id 'nope'"}},
		{"template", `{"workflow_id":"` + template.ID + `","kind":"cron","cron_expr":"0 9 * * *"}`,
			map[string]string{"workflow_id": "is a template; create an editable copy and schedule that"}},
		{"input schema", `{"workflow_id":"` + wf.ID + `","kind":"cron","cron_expr":"0 9 * * *","input":{"task":5}}`,
			map[string]string{"input.task": "got number, want string"}},
		{"input required field", `{"workflow_id":"` + wf.ID + `","kind":"cron","cron_expr":"0 9 * * *"}`,
			map[string]string{"input": "missing property 'task'"}},
		{"input not an object", `{"workflow_id":"` + wf.ID + `","kind":"cron","cron_expr":"0 9 * * *","input":[1]}`,
			map[string]string{"input": "must be an object"}},
		{"unknown field", `{"workflow_id":"` + wf.ID + `","kind":"cron","cron_expr":"0 9 * * *","every":"day"}`,
			map[string]string{"every": "unknown field"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := do(t, ts, http.MethodPost, "/api/schedules", tc.body)
			if code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d: %s", code, body)
			}
			var fe []fieldError
			mustDo(t, json.Unmarshal(decodeError(t, body).Error.Details, &fe))
			got := map[string]string{}
			for _, f := range fe {
				got[f.Path] = f.Message
			}
			for path, msg := range tc.want {
				if got[path] != msg {
					t.Errorf("%s: %q, want %q (all: %v)", path, got[path], msg, got)
				}
			}
		})
	}
}

// A delay schedule's next run is its run_at, in UTC; an agent's quick-run workflow may
// be scheduled (only shipped templates are refused).
func TestScheduleDelayAndQuickRun(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	pinScheduleClock(t, "2099-10-15T08:00:00Z")
	key := "quickrun:" + uniqueSuffix()
	quick := &store.Workflow{Name: "quick run", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`), TemplateKey: &key}
	mustDo(t, store.CreateWorkflow(context.Background(), pool, quick))
	code, body := do(t, ts, http.MethodPost, "/api/schedules",
		`{"workflow_id":"`+quick.ID+`","kind":"delay","run_at":"2099-10-15T10:00:02+02:00","input":{"task":"once"}}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	s := decodeSchedule(t, body)
	if s.NextRunAt == nil || !s.NextRunAt.Equal(time.Date(2099, 10, 15, 8, 0, 2, 0, time.UTC)) || s.RunAt == nil || !s.RunAt.Equal(*s.NextRunAt) {
		t.Fatalf("delay: next %v run_at %v", s.NextRunAt, s.RunAt)
	}
	mustDo(t, store.DeleteSchedule(context.Background(), pool, s.ID))
}

// OpenAPI conformance for the schedules, skills and agent memory paths.
func TestOpenAPIConformanceWave3B(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	router := contract(t)
	fe := &fakeEngine{pool: pool}
	_, ts := newTestServer(t, pool, fe, nil)
	pinScheduleClock(t, "2099-10-15T08:00:00Z")
	wf := scheduleWorkflow(t)
	agentID := testAgent(t, "conformance-memory")
	now := time.Now().UTC()
	fact := saveFact(t, agentID, "conformance", now, now.Add(time.Hour))
	skillName := "conformance-" + uniqueSuffix()

	type step struct {
		method, path, body string
		want               int
	}
	run := func(st step) []byte {
		t.Helper()
		code, body := checkConformance(t, router, ts.URL, st.method, st.path, st.body, st.want < 300)
		if code != st.want {
			t.Fatalf("%s %s: status %d, want %d: %s", st.method, st.path, code, st.want, body)
		}
		return body
	}
	body := run(step{http.MethodPost, "/api/schedules", `{"workflow_id":"` + wf.ID + `","kind":"cron","cron_expr":"*/30 * * * *","tz":"UTC","input":{"task":"t"}}`, http.StatusCreated})
	sc := decodeSchedule(t, body)
	t.Cleanup(func() { _ = store.DeleteSchedule(ctx, pool, sc.ID) })
	body = run(step{http.MethodPost, "/api/skills", `{"name":"` + skillName + `","description":"d","body":"b"}`, http.StatusCreated})
	sk := decodeSkill(t, body)
	for _, st := range []step{
		{http.MethodPost, "/api/schedules", `{"workflow_id":"` + wf.ID + `","kind":"cron","cron_expr":"bad"}`, http.StatusUnprocessableEntity},
		{http.MethodGet, "/api/schedules", "", http.StatusOK},
		{http.MethodGet, "/api/schedules/" + sc.ID, "", http.StatusOK},
		{http.MethodGet, "/api/schedules/no-such-schedule", "", http.StatusNotFound},
		{http.MethodPut, "/api/schedules/" + sc.ID, `{"enabled":false}`, http.StatusOK},
		{http.MethodPut, "/api/schedules/" + sc.ID, `{"kind":"delay","run_at":"2099-11-01T00:00:00Z","enabled":true}`, http.StatusOK},
		{http.MethodPut, "/api/schedules/" + sc.ID, `{"tz":"Nowhere/Land"}`, http.StatusUnprocessableEntity},
		{http.MethodGet, "/api/skills", "", http.StatusOK},
		{http.MethodGet, "/api/skills/" + sk.ID, "", http.StatusOK},
		{http.MethodPut, "/api/skills/" + sk.ID, `{"name":"` + skillName + `","body":"new"}`, http.StatusOK},
		{http.MethodPut, "/api/skills/" + sk.ID, `{"name":"Not Valid"}`, http.StatusUnprocessableEntity},
		{http.MethodPost, "/api/skills", `{"name":"x_y"}`, http.StatusUnprocessableEntity},
		{http.MethodGet, "/api/skills/no-such-skill", "", http.StatusNotFound},
		{http.MethodGet, "/api/agents/" + agentID + "/memory", "", http.StatusOK},
		{http.MethodGet, "/api/agents/no-such-agent/memory", "", http.StatusNotFound},
		{http.MethodDelete, "/api/agents/" + agentID + "/memory/" + fact.ID, "", http.StatusNoContent},
		{http.MethodDelete, "/api/agents/" + agentID + "/memory/" + fact.ID, "", http.StatusNotFound},
		{http.MethodDelete, "/api/skills/" + sk.ID, "", http.StatusNoContent},
		{http.MethodDelete, "/api/schedules/" + sc.ID, "", http.StatusNoContent},
		{http.MethodDelete, "/api/schedules/" + sc.ID, "", http.StatusNotFound},
	} {
		run(st)
	}
}
