package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"yuno/internal/engine"
	"yuno/internal/store"
)

// Wave 9 agent A1: the API half of iterating on a merge request (DESIGN 5.22 rules 7 and
// 11): run requests, the runs list, the schedule fields.

// iterRecord stores an open merge request record of project p, opened by a run that has
// ended (so a run may name it), and returns that run and the record.
func iterRecord(t *testing.T, p *store.Project, number int64) (*store.Run, *store.MergeRequest) {
	t.Helper()
	opener := projectRun(t, p)
	setRunStatus(t, opener.ID, store.RunSucceeded)
	mr := &store.MergeRequest{ProjectID: p.ID, RunID: opener.ID, Provider: "gitlab", Number: number,
		URL: "https://gitlab.test/group/demo/-/merge_requests/" + itoa(number), SourceBranch: "yuno/abc123", TargetBranch: "main", Title: "Add a thing"}
	ok, err := store.InsertMergeRequest(context.Background(), testPool, mr)
	mustDo(t, err)
	if !ok {
		t.Fatal("record not inserted")
	}
	return opener, mr
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func setRunStatus(t *testing.T, id, status string) {
	t.Helper()
	_, err := testPool.Exec(context.Background(), `UPDATE runs SET status=$2 WHERE id=$1`, id, status)
	mustDo(t, err)
}

// namingRun stores a run of project p that names the record, in the trigger and status
// given, with a checkout_sha when sha is not "".
func namingRun(t *testing.T, p *store.Project, mr *store.MergeRequest, trigger, status, sha string) *store.Run {
	t.Helper()
	run := projectRun(t, p)
	setRunStatus(t, run.ID, store.RunSucceeded) // projectRun's run names nothing: freed
	named := *run
	named.ID = ""
	named.Trigger, named.MergeRequestID, named.Status = trigger, &mr.ID, store.RunQueued
	if sha != "" {
		named.CheckoutSHA = &sha
	}
	mustDo(t, store.CreateRun(context.Background(), testPool, &named))
	if status != store.RunQueued {
		setRunStatus(t, named.ID, status)
	}
	return &named
}

// runIDs decodes a list of runs and returns their ids, in order.
func runIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var runs []store.Run
	mustDo(t, json.Unmarshal(body, &runs))
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = r.ID
	}
	return ids
}

// DESIGN 5.29 rule 12 (wave 3 review M2): GET /api/runs?trigger=plan lists the ship runs.
func TestRunsListFiltersOnPlanTrigger(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	p := seedProject(t, nil, "plan-list-"+uniqueSuffix(), "")
	ref := store.ShipRef(store.NewID(), 1)
	plain := projectRun(t, p)
	setRunStatus(t, plain.ID, store.RunSucceeded)
	ship := *plain
	ship.ID, ship.Trigger, ship.TriggerRef, ship.Status = "", store.TriggerPlan, &ref, store.RunQueued
	mustDo(t, store.CreateRun(context.Background(), testPool, &ship))

	code, body := do(t, ts, http.MethodGet, "/api/runs?trigger=plan&project_id="+p.ID, "")
	if got := runIDs(t, body); code != http.StatusOK || !slices.Equal(got, []string{ship.ID}) || slices.Contains(got, plain.ID) {
		t.Fatalf("GET /api/runs?trigger=plan: %d %v, want only %s", code, got, ship.ID)
	}
}

// T5.22.11, DESIGN 5.22 rules 1 and 11: GET /api/runs filters on trigger and on the merge
// request the run names (newest first, with the older filters); an unknown trigger is a
// 422 detail; Run carries merge_request_id, checkout_sha and the named record as
// merge_request on lists, gets and traces.
func TestRunsListFiltersOnTriggerAndMergeRequest(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	sfx := uniqueSuffix()
	p := seedProject(t, nil, "iter-list-"+sfx, "")
	_, rec := iterRecord(t, p, 4)
	_, other := iterRecord(t, p, 5)
	sha := "0123456789abcdef0123456789abcdef01234567"
	first := namingRun(t, p, rec, store.TriggerManual, store.RunSucceeded, sha) // ended, frees the request
	second := namingRun(t, p, rec, store.TriggerSchedule, store.RunQueued, "")
	issue := namingRun(t, p, other, store.TriggerIssue, store.RunRunning, "")
	plain := projectRun(t, p)

	list := func(query string) []string {
		t.Helper()
		code, body := do(t, ts, http.MethodGet, "/api/runs?"+query, "")
		if code != http.StatusOK {
			t.Fatalf("GET /api/runs?%s: %d %s", query, code, body)
		}
		return runIDs(t, body)
	}
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"merge_request_id=" + rec.ID, []string{second.ID, first.ID}},
		{"merge_request_id=" + rec.ID + "&limit=1", []string{second.ID}},
		{"merge_request_id=" + rec.ID + "&status=succeeded", []string{first.ID}},
		{"merge_request_id=" + rec.ID + "&trigger=schedule", []string{second.ID}},
		{"merge_request_id=" + rec.ID + "&trigger=manual", []string{first.ID}},
		{"merge_request_id=" + rec.ID + "&trigger=issue", []string{}},
		{"merge_request_id=" + other.ID + "&trigger=issue&project_id=" + p.ID, []string{issue.ID}},
		{"merge_request_id=no-such-request", []string{}},
		{"merge_request_id=" + rec.ID + "&project_id=no-such-project", []string{}},
	} {
		if got := list(tc.query); !slices.Equal(got, tc.want) {
			t.Errorf("GET /api/runs?%s = %v, want %v", tc.query, got, tc.want)
		}
	}
	// The trigger filter on its own: the issue run is in, the others are not, and nothing
	// of another trigger comes with it.
	code, body := do(t, ts, http.MethodGet, "/api/runs?trigger=issue&limit=500", "")
	var runs []store.Run
	mustDo(t, json.Unmarshal(body, &runs))
	if code != http.StatusOK || !slices.ContainsFunc(runs, func(r store.Run) bool { return r.ID == issue.ID }) ||
		slices.ContainsFunc(runs, func(r store.Run) bool { return r.Trigger != store.TriggerIssue || r.ID == plain.ID || r.ID == first.ID }) {
		t.Errorf("GET /api/runs?trigger=issue: %d, %d runs, issue run listed: %v", code, len(runs), slices.ContainsFunc(runs, func(r store.Run) bool { return r.ID == issue.ID }))
	}

	// Another trigger is a 422 detail on trigger, whatever else the query says.
	for _, q := range []string{"trigger=webhook", "trigger=Manual&merge_request_id=" + rec.ID} {
		code, body = do(t, ts, http.MethodGet, "/api/runs?"+q, "")
		var fe []fieldError
		mustDo(t, json.Unmarshal(decodeError(t, body).Error.Details, &fe))
		if code != http.StatusUnprocessableEntity || len(fe) != 1 || fe[0].Path != "trigger" ||
			fe[0].Message != "must be one of manual, schedule, channel, eval, issue, plan" {
			t.Errorf("GET /api/runs?%s: %d %s", q, code, body)
		}
	}

	// The representation: the ids, the sha and the named record on a list, a get and a trace.
	code, body = do(t, ts, http.MethodGet, "/api/runs?merge_request_id="+rec.ID, "")
	mustDo(t, json.Unmarshal(body, &runs))
	byID := map[string]store.Run{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	got := byID[first.ID]
	if code != http.StatusOK || got.MergeRequestID == nil || *got.MergeRequestID != rec.ID || got.CheckoutSHA == nil || *got.CheckoutSHA != sha ||
		got.MergeRequest == nil || got.MergeRequest.ID != rec.ID || got.MergeRequest.Number != 4 {
		t.Fatalf("listed run = %+v (%s)", got, body)
	}
	if !strings.Contains(string(body), `"merge_request_id":"`+rec.ID+`"`) || !strings.Contains(string(body), `"checkout_sha":"`+sha+`"`) {
		t.Errorf("JSON names: %s", body)
	}
	for _, path := range []string{"/api/runs/" + first.ID, "/api/runs/" + first.ID + "/trace"} {
		code, body = do(t, ts, http.MethodGet, path, "")
		var shown store.Run
		if strings.HasSuffix(path, "/trace") {
			var tr store.Trace
			mustDo(t, json.Unmarshal(body, &tr))
			shown = *tr.Run
		} else {
			mustDo(t, json.Unmarshal(body, &shown))
		}
		if code != http.StatusOK || shown.MergeRequestID == nil || shown.MergeRequest == nil || shown.MergeRequest.ID != rec.ID || shown.CheckoutSHA == nil {
			t.Errorf("GET %s: %d %s", path, code, body)
		}
	}
	// A run that names none has none of the three.
	code, body = do(t, ts, http.MethodGet, "/api/runs/"+plain.ID, "")
	if code != http.StatusOK || strings.Contains(string(body), "merge_request") || strings.Contains(string(body), "checkout_sha") {
		t.Errorf("run without a request: %d %s", code, body)
	}
}

// DESIGN 5.22 rule 11: RunCreate.merge_request_id reaches the engine (trimmed, with the
// project id), and the engine's refusals of rule 1 are 422 details on merge_request_id.
func TestRunCreatePassesTheMergeRequestAndMapsItsRefusals(t *testing.T) {
	pool := needDB(t)
	fe := &fakeEngine{pool: pool}
	_, ts := newTestServer(t, pool, fe, nil)

	code, body := do(t, ts, http.MethodPost, "/api/runs",
		`{"agent_id":"ag","task":"fix the review comment","project_id":" pr ","merge_request_id":"  mr-1 "}`)
	fe.mu.Lock()
	reqs := slices.Clone(fe.triggers)
	fe.mu.Unlock()
	if code != http.StatusCreated || len(reqs) != 1 || reqs[0].MergeRequestID != "mr-1" || reqs[0].ProjectID != "pr" || reqs[0].AgentID != "ag" {
		t.Fatalf("create: %d %s, trigger requests %+v", code, body, reqs)
	}
	// A request that names none sends none.
	do(t, ts, http.MethodPost, "/api/runs", `{"agent_id":"ag","task":"t"}`)
	fe.mu.Lock()
	last := fe.triggers[len(fe.triggers)-1]
	fe.mu.Unlock()
	if last.MergeRequestID != "" {
		t.Errorf("a request without merge_request_id sent %q", last.MergeRequestID)
	}

	for _, msg := range []string{
		"merge_request_id: the workflow does not use a project",
		"merge_request_id: no merge request with id mr-1",
		"merge_request_id: merge request !4 belongs to project Other",
		"merge_request_id: merge request !4 is merged",
		"merge_request_id: pull request #4 is closed",
		"merge_request_id: merge request !4 is being updated by run run-1",
	} {
		fe.mu.Lock()
		fe.triggerErr = &engine.ValidationError{Messages: []string{msg}}
		fe.mu.Unlock()
		code, body = do(t, ts, http.MethodPost, "/api/runs", `{"agent_id":"ag","task":"t","project_id":"pr","merge_request_id":"mr-1"}`)
		var fes []fieldError
		mustDo(t, json.Unmarshal(decodeError(t, body).Error.Details, &fes))
		path, text, _ := strings.Cut(msg, ": ")
		if code != http.StatusUnprocessableEntity || len(fes) != 1 || fes[0].Path != path || fes[0].Message != text {
			t.Errorf("%q: %d %s", msg, code, body)
		}
	}
}

// DESIGN 5.22 rule 11 against the contract: the refusal of a run request is an Error body
// the document describes, and the request with the new member is valid.
func TestRunCreateMergeRequestRefusalConformance(t *testing.T) {
	pool := needDB(t)
	router := contract(t)
	fe := &fakeEngine{pool: pool, triggerErr: &engine.ValidationError{Messages: []string{"merge_request_id: no merge request with id no-such-request"}}}
	_, ts := newTestServer(t, pool, fe, nil)
	code, body := checkConformance(t, router, ts.URL, http.MethodPost, "/api/runs",
		`{"agent_id":"ag","task":"t","project_id":"pr","merge_request_id":"no-such-request"}`, true)
	var fes []fieldError
	mustDo(t, json.Unmarshal(decodeError(t, body).Error.Details, &fes))
	if code != http.StatusUnprocessableEntity || len(fes) != 1 || fes[0].Path != "merge_request_id" || fes[0].Message != "no merge request with id no-such-request" {
		t.Fatalf("refusal: %d %s", code, body)
	}
}

// sendSchedule posts or puts a schedule body and returns the status, the schedule when it
// was stored and the details of a 422 as path → message.
func sendSchedule(t *testing.T, ts *httptest.Server, method, path, body string) (int, store.Schedule, map[string]string) {
	t.Helper()
	code, b := do(t, ts, method, path, body)
	if code == http.StatusUnprocessableEntity {
		var fes []fieldError
		mustDo(t, json.Unmarshal(decodeError(t, b).Error.Details, &fes))
		details := map[string]string{}
		for _, f := range fes {
			details[f.Path] = f.Message
		}
		return code, store.Schedule{}, details
	}
	var sc store.Schedule
	if code < 300 {
		sc = decodeSchedule(t, b)
	}
	return code, sc, nil
}

// T5.22.9 (the API half), DESIGN 5.22 rule 7: a schedule names the request its runs
// update; the rule 1 checks are 422 details on merge_request_id; a schedule on a workflow
// that uses no project says so with the engine's text, and one on a workflow that wants a
// project but has none says it needs one; left out on update it keeps its value, and is
// not checked again; a change of workflow or of project drops it; an empty string clears
// it; the representation carries the id and the joined number.
func TestScheduleMergeRequest(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	pinScheduleClock(t, "2099-10-15T08:00:00Z")
	sfx := uniqueSuffix()
	plain, needs, needs2 := scheduleWorkflow(t), projectScheduleWorkflow(t), projectScheduleWorkflow(t)
	p, p2 := seedProject(t, nil, "Sched MR "+sfx, ""), seedProject(t, nil, "Sched MR two "+sfx, "")
	opener, rec := iterRecord(t, p, 4)
	opener2, rec2 := iterRecord(t, p, 5)
	_, foreign := iterRecord(t, p2, 7)
	call := func(method, path, body string) (int, []byte) { return do(t, ts, method, path, body) }
	send := func(method, path, body string) (int, store.Schedule, map[string]string) {
		t.Helper()
		return sendSchedule(t, ts, method, path, body)
	}
	rest := `,"kind":"cron","cron_expr":"0 9 * * *","input":{"task":"t"}}`
	setState := func(id, state string) {
		_, err := pool.Exec(ctx, `UPDATE merge_requests SET state=$2 WHERE id=$1`, id, state)
		mustDo(t, err)
	}

	// The refusals, each one detail on merge_request_id with the engine's text.
	for _, tc := range []struct {
		name, body, want string
	}{
		{"no project at all", `{"workflow_id":"` + plain.ID + `","merge_request_id":"` + rec.ID + `"` + rest, "the workflow does not use a project"},
		{"no project on a workflow that wants one", `{"workflow_id":"` + needs.ID + `","merge_request_id":"` + rec.ID + `"` + rest, "needs a project"},
		{"unknown", `{"workflow_id":"` + needs.ID + `","project_id":"` + p.ID + `","merge_request_id":"nope"` + rest, "no merge request with id nope"},
		{"another project's", `{"workflow_id":"` + needs.ID + `","project_id":"` + p.ID + `","merge_request_id":"` + foreign.ID + `"` + rest,
			"merge request !7 belongs to project " + p2.Name},
	} {
		code, _, got := send(http.MethodPost, "/api/schedules", tc.body)
		if code != http.StatusUnprocessableEntity || got["merge_request_id"] != tc.want {
			t.Errorf("%s: %d %v, want merge_request_id %q", tc.name, code, got, tc.want)
		}
	}
	create := `{"workflow_id":"` + needs.ID + `","project_id":"` + p.ID + `","merge_request_id":"` + rec.ID + `"` + rest
	for _, state := range []string{store.MergeRequestMerged, store.MergeRequestClosed} {
		setState(rec.ID, state)
		if code, _, got := send(http.MethodPost, "/api/schedules", create); code != http.StatusUnprocessableEntity ||
			got["merge_request_id"] != "merge request !4 is "+state {
			t.Errorf("%s: %d %v", state, code, got)
		}
	}
	setState(rec.ID, store.MergeRequestOpen)
	setRunStatus(t, opener.ID, store.RunRunning)
	if code, _, got := send(http.MethodPost, "/api/schedules", create); code != http.StatusUnprocessableEntity ||
		got["merge_request_id"] != "merge request !4 is being updated by run "+opener.ID {
		t.Errorf("opener live: %d %v", code, got)
	}
	setRunStatus(t, opener.ID, store.RunSucceeded)
	// A refused project is the only complaint: the request is not checked on top of it.
	if code, _, got := send(http.MethodPost, "/api/schedules", `{"workflow_id":"`+plain.ID+`","project_id":"`+p.ID+`","merge_request_id":"`+rec.ID+`"`+rest); code != http.StatusUnprocessableEntity ||
		got["project_id"] == "" || got["merge_request_id"] != "" {
		t.Errorf("refused project: %d %v", code, got)
	}

	// Created: the id is stored trimmed, the representation joins the number.
	code, s, _ := send(http.MethodPost, "/api/schedules", strings.Replace(create, `"merge_request_id":"`+rec.ID+`"`, `"merge_request_id":" `+rec.ID+` "`, 1))
	if code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	t.Cleanup(func() { _ = store.DeleteSchedule(ctx, pool, s.ID) })
	if s.MergeRequestID == nil || *s.MergeRequestID != rec.ID || s.MergeRequestNumber == nil || *s.MergeRequestNumber != 4 {
		t.Fatalf("created = %+v", s)
	}
	_, listBody := call(http.MethodGet, "/api/schedules", "")
	var list []store.Schedule
	mustDo(t, json.Unmarshal(listBody, &list))
	if i := slices.IndexFunc(list, func(x store.Schedule) bool { return x.ID == s.ID }); i < 0 || list[i].MergeRequestNumber == nil || *list[i].MergeRequestNumber != 4 {
		t.Fatalf("list: %s", listBody)
	}

	// Left out, it is kept: the toggle, and a PUT of what was fetched (the read-only
	// number is accepted).
	if code, got, _ := send(http.MethodPut, "/api/schedules/"+s.ID, `{"enabled":false}`); code != http.StatusOK ||
		got.MergeRequestID == nil || *got.MergeRequestID != rec.ID || got.Enabled {
		t.Fatalf("toggle: %d %+v", code, got)
	}
	_, fetched := call(http.MethodGet, "/api/schedules/"+s.ID, "")
	if code, got, _ := send(http.MethodPut, "/api/schedules/"+s.ID, string(fetched)); code != http.StatusOK ||
		got.MergeRequestID == nil || *got.MergeRequestID != rec.ID {
		t.Fatalf("put back: %d %+v", code, got)
	}
	// Named again, it is replaced and refused when the new one is not fit.
	if code, got, _ := send(http.MethodPut, "/api/schedules/"+s.ID, `{"merge_request_id":"`+rec2.ID+`"}`); code != http.StatusOK ||
		got.MergeRequestID == nil || *got.MergeRequestID != rec2.ID || got.MergeRequestNumber == nil || *got.MergeRequestNumber != 5 {
		t.Fatalf("replace: %d %+v", code, got)
	}
	if code, _, got := send(http.MethodPut, "/api/schedules/"+s.ID, `{"merge_request_id":"`+foreign.ID+`"}`); code != http.StatusUnprocessableEntity ||
		got["merge_request_id"] != "merge request !7 belongs to project "+p2.Name {
		t.Fatalf("replace with another project's: %d %v", code, got)
	}

	// A change of workflow drops it (the project named again, unchanged), a change of
	// project too; naming the request with the change keeps the named one.
	if code, got, _ := send(http.MethodPut, "/api/schedules/"+s.ID, `{"workflow_id":"`+needs2.ID+`","project_id":"`+p.ID+`"}`); code != http.StatusOK ||
		got.MergeRequestID != nil || got.MergeRequestNumber != nil || got.WorkflowID != needs2.ID {
		t.Fatalf("workflow change: %d %+v", code, got)
	}
	if code, got, _ := send(http.MethodPut, "/api/schedules/"+s.ID, `{"merge_request_id":"`+rec.ID+`"}`); code != http.StatusOK || got.MergeRequestID == nil {
		t.Fatalf("name again: %d %+v", code, got)
	}
	if code, got, _ := send(http.MethodPut, "/api/schedules/"+s.ID, `{"project_id":"`+p2.ID+`"}`); code != http.StatusOK ||
		got.MergeRequestID != nil || got.ProjectID == nil || *got.ProjectID != p2.ID {
		t.Fatalf("project change: %d %+v", code, got)
	}
	if code, got, _ := send(http.MethodPut, "/api/schedules/"+s.ID, `{"project_id":"`+p.ID+`","merge_request_id":"`+rec.ID+`"}`); code != http.StatusOK ||
		got.MergeRequestID == nil || *got.MergeRequestID != rec.ID || *got.ProjectID != p.ID {
		t.Fatalf("project change naming the request: %d %+v", code, got)
	}
	// The same project named again is no change.
	if code, got, _ := send(http.MethodPut, "/api/schedules/"+s.ID, `{"project_id":"`+p.ID+`"}`); code != http.StatusOK ||
		got.MergeRequestID == nil || *got.MergeRequestID != rec.ID {
		t.Fatalf("project named again: %d %+v", code, got)
	}

	// An empty string clears it; the body's other fields stand.
	if code, got, _ := send(http.MethodPut, "/api/schedules/"+s.ID, `{"merge_request_id":" "}`); code != http.StatusOK ||
		got.MergeRequestID != nil || got.MergeRequestNumber != nil || got.ProjectID == nil {
		t.Fatalf("clear: %d %+v", code, got)
	}
	if _, body := call(http.MethodGet, "/api/schedules/"+s.ID, ""); strings.Contains(string(body), "merge_request") {
		t.Errorf("a schedule without a request carries merge_request members: %s", body)
	}

	// A request that ended since does not stop the schedule from being disabled, enabled
	// or edited: it is checked only when the body names it or the workflow or project
	// changes (review M1). The tick disables the schedule when it next fires.
	if code, _, _ := send(http.MethodPut, "/api/schedules/"+s.ID, `{"merge_request_id":"`+rec2.ID+`"}`); code != http.StatusOK {
		t.Fatalf("name rec2: %d", code)
	}
	cur, err := store.GetSchedule(ctx, pool, s.ID)
	mustDo(t, err)
	home, away := cur.WorkflowID, needs.ID // the workflow the schedule is on, and another one
	if home == needs.ID {
		away = needs2.ID
	}
	for _, state := range []string{store.MergeRequestMerged, store.MergeRequestClosed} {
		setState(rec2.ID, state)
		want := "merge request !5 is " + state
		for name, body := range map[string]string{
			"disable":      `{"enabled":false}`,
			"enable":       `{"enabled":true}`,
			"edit input":   `{"input":{"task":"edited under ` + state + `"}}`,
			"edit cron":    `{"cron_expr":"30 9 * * *"}`,
			"name project": `{"project_id":"` + p.ID + `"}`, // unchanged: not a change
		} {
			code, got, details := send(http.MethodPut, "/api/schedules/"+s.ID, body)
			if code != http.StatusOK || got.MergeRequestID == nil || *got.MergeRequestID != rec2.ID {
				t.Fatalf("%s, request %s: %d %+v %v; want 200 with the request kept", name, state, code, got, details)
			}
		}
		// A PUT of what was fetched names the request, so the request is checked.
		_, fetched := call(http.MethodGet, "/api/schedules/"+s.ID, "")
		if code, _, got := send(http.MethodPut, "/api/schedules/"+s.ID, string(fetched)); code != http.StatusUnprocessableEntity || got["merge_request_id"] != want {
			t.Fatalf("put back, request %s: %d %v, want %q", state, code, got, want)
		}
		if got, err := store.GetSchedule(ctx, pool, s.ID); err != nil || got.MergeRequestNumber == nil || *got.MergeRequestNumber != 5 ||
			string(got.Input) == "" || !strings.Contains(string(got.Input), "edited under "+state) {
			t.Fatalf("the stored schedule after the edits: %+v %v", got, err)
		}
		// Naming it in the body, or changing the workflow or project, checks it again.
		if code, _, got := send(http.MethodPut, "/api/schedules/"+s.ID, `{"merge_request_id":"`+rec2.ID+`"}`); code != http.StatusUnprocessableEntity ||
			got["merge_request_id"] != want {
			t.Fatalf("naming a %s request: %d %v, want %q", state, code, got, want)
		}
		if code, _, got := send(http.MethodPut, "/api/schedules/"+s.ID, `{"workflow_id":"`+away+`","project_id":"`+p.ID+`","merge_request_id":"`+rec2.ID+`"}`); code != http.StatusUnprocessableEntity ||
			got["merge_request_id"] != want {
			t.Fatalf("workflow change naming a %s request: %d %v, want %q", state, code, got, want)
		}
		// A change that drops the kept request needs no check: it goes.
		if code, got, _ := send(http.MethodPut, "/api/schedules/"+s.ID, `{"workflow_id":"`+away+`","project_id":"`+p.ID+`"}`); code != http.StatusOK ||
			got.MergeRequestID != nil || got.WorkflowID != away {
			t.Fatalf("workflow change over a %s request: %d %+v", state, code, got)
		}
		if code, got, _ := send(http.MethodPut, "/api/schedules/"+s.ID, `{"workflow_id":"`+home+`","project_id":"`+p.ID+`"}`); code != http.StatusOK {
			t.Fatalf("back to the first workflow: %d %+v", code, got)
		}
		setState(rec2.ID, store.MergeRequestOpen)
		if code, got, _ := send(http.MethodPut, "/api/schedules/"+s.ID, `{"merge_request_id":"`+rec2.ID+`"}`); code != http.StatusOK || got.MergeRequestID == nil {
			t.Fatalf("name rec2 again: %d %+v", code, got)
		}
	}
	// An open request that a live run is updating is not the schedule's business either
	// until the body names it: the toggle goes through while the opener runs, naming the
	// request is refused.
	setRunStatus(t, opener2.ID, store.RunRunning)
	if code, got, _ := send(http.MethodPut, "/api/schedules/"+s.ID, `{"enabled":false}`); code != http.StatusOK || got.MergeRequestID == nil {
		t.Fatalf("toggle while the opener runs: %d %+v", code, got)
	}
	if code, _, got := send(http.MethodPut, "/api/schedules/"+s.ID, `{"merge_request_id":"`+rec2.ID+`"}`); code != http.StatusUnprocessableEntity ||
		got["merge_request_id"] != "merge request !5 is being updated by run "+opener2.ID {
		t.Fatalf("naming a request its opener is updating: %d %v", code, got)
	}
	setRunStatus(t, opener2.ID, store.RunSucceeded)
}

// Review L1: a merge_request_id with a control character is an unknown id, not a 500 from
// the database: a 422 detail on the three writes that take one, and an empty list for
// GET /api/runs. Left out, the stored request is untouched.
func TestMergeRequestIDWithAControlCharacterIsUnknownL1(t *testing.T) {
	pool := needDB(t)
	fe := &fakeEngine{pool: pool}
	_, ts := newTestServer(t, pool, fe, nil)
	pinScheduleClock(t, "2099-10-15T08:00:00Z")
	sfx := uniqueSuffix()
	needs := projectScheduleWorkflow(t)
	p := seedProject(t, nil, "Control MR "+sfx, "")
	_, rec := iterRecord(t, p, 4)
	rest := `,"kind":"cron","cron_expr":"0 9 * * *","input":{"task":"t"}}`

	for what, id := range map[string]string{"a NUL": "a\u0000b", "a bell": "a\u0007b", "a line break inside": "a\nb", "only a NUL": "\u0000", "a DEL": "a\u007fb"} {
		// POST /api/runs: refused before the engine is asked.
		before := len(fe.triggers)
		code, body := do(t, ts, http.MethodPost, "/api/runs", `{"agent_id":"ag","task":"t","project_id":"`+p.ID+`","merge_request_id":`+jsonText(id)+`}`)
		fes := fieldMessages(t, body)
		if code != http.StatusUnprocessableEntity || len(fes) != 1 || fes[0] != "merge_request_id: no merge request with id "+id || len(fe.triggers) != before {
			t.Errorf("%s, POST /api/runs: %d %s", what, code, body)
		}
		// POST /api/schedules.
		code, _, got := sendSchedule(t, ts, http.MethodPost, "/api/schedules",
			`{"workflow_id":"`+needs.ID+`","project_id":"`+p.ID+`","merge_request_id":`+jsonText(id)+rest)
		if code != http.StatusUnprocessableEntity || got["merge_request_id"] != "no merge request with id "+id {
			t.Errorf("%s, POST /api/schedules: %d %v", what, code, got)
		}
		// GET /api/runs: no run names it.
		code, body = do(t, ts, http.MethodGet, "/api/runs?merge_request_id="+url.QueryEscape(id), "")
		if code != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
			t.Errorf("%s, GET /api/runs: %d %s, want 200 []", what, code, body)
		}
	}

	// PUT /api/schedules/{id}: the control character is refused, the request kept.
	code, s, _ := sendSchedule(t, ts, http.MethodPost, "/api/schedules",
		`{"workflow_id":"`+needs.ID+`","project_id":"`+p.ID+`","merge_request_id":"`+rec.ID+`"`+rest)
	if code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	t.Cleanup(func() { _ = store.DeleteSchedule(context.Background(), pool, s.ID) })
	if code, _, got := sendSchedule(t, ts, http.MethodPut, "/api/schedules/"+s.ID, `{"merge_request_id":"x\u0000y"}`); code != http.StatusUnprocessableEntity ||
		got["merge_request_id"] != "no merge request with id x\x00y" {
		t.Errorf("PUT with a NUL: %d %v", code, got)
	}
	if stored, err := store.GetSchedule(context.Background(), pool, s.ID); err != nil || stored.MergeRequestID == nil || *stored.MergeRequestID != rec.ID {
		t.Errorf("a refused PUT changed the schedule: %+v %v", stored, err)
	}
}

// iterationConformanceSteps exercises the wave 9 parameters and fields of DESIGN 5.22
// rule 11 against the contract: the runs list filters, a run that names a request (its
// merge_request_id, checkout_sha and merge_request members), a run request with the new
// member, and the schedule fields with their 422 refusals.
func iterationConformanceSteps(t *testing.T) []conformanceStep {
	t.Helper()
	ctx := context.Background()
	sfx := uniqueSuffix()
	p := seedProject(t, nil, "conf-iter-"+sfx, "")
	_, rec := iterRecord(t, p, 4)
	_, spare := iterRecord(t, p, 5)
	named := namingRun(t, p, rec, store.TriggerManual, store.RunSucceeded, "0123456789abcdef0123456789abcdef01234567")
	wf := projectScheduleWorkflow(t)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM schedules WHERE workflow_id = $1`, wf.ID)
	})
	next := scheduleNow().Add(24 * time.Hour)
	expr := "0 9 * * *"
	sc := &store.Schedule{WorkflowID: wf.ID, ProjectID: &p.ID, MergeRequestID: &rec.ID, Kind: store.ScheduleCron, CronExpr: &expr, TZ: "UTC",
		Enabled: true, NextRunAt: &next, Input: json.RawMessage(`{"task":"t"}`)}
	mustDo(t, store.CreateSchedule(ctx, testPool, sc))
	body := func(extra string) string {
		return `{"workflow_id":"` + wf.ID + `","project_id":"` + p.ID + `"` + extra + `,"kind":"cron","cron_expr":"0 9 * * *","input":{"task":"t"}}`
	}
	return []conformanceStep{
		{http.MethodGet, "/api/runs?merge_request_id=" + rec.ID, "", http.StatusOK},
		{http.MethodGet, "/api/runs?merge_request_id=" + rec.ID + "&trigger=manual&status=succeeded&limit=5", "", http.StatusOK},
		{http.MethodGet, "/api/runs?merge_request_id=no-such-request", "", http.StatusOK},
		{http.MethodGet, "/api/runs?trigger=issue&limit=5", "", http.StatusOK},
		{http.MethodGet, "/api/runs?trigger=webhook", "", http.StatusUnprocessableEntity},
		{http.MethodGet, "/api/runs?merge_request_id=a%00b", "", http.StatusOK},
		{http.MethodGet, "/api/runs/" + named.ID, "", http.StatusOK},
		{http.MethodGet, "/api/runs/" + named.ID + "/trace", "", http.StatusOK},
		{http.MethodPost, "/api/runs", `{"agent_id":"ag","task":"fix the review comment","project_id":"` + p.ID + `","merge_request_id":"` + rec.ID + `"}`, http.StatusCreated},
		{http.MethodGet, "/api/schedules/" + sc.ID, "", http.StatusOK},
		{http.MethodPost, "/api/schedules", body(`,"merge_request_id":"` + spare.ID + `"`), http.StatusCreated},
		{http.MethodPost, "/api/schedules", body(`,"merge_request_id":"no-such-request"`), http.StatusUnprocessableEntity},
		{http.MethodPost, "/api/schedules", `{"workflow_id":"` + wf.ID + `","merge_request_id":"` + rec.ID + `","kind":"cron","cron_expr":"0 9 * * *"}`, http.StatusUnprocessableEntity},
		{http.MethodPut, "/api/schedules/" + sc.ID, `{"merge_request_id":"no-such-request"}`, http.StatusUnprocessableEntity},
		{http.MethodPut, "/api/schedules/" + sc.ID, `{"merge_request_id":""}`, http.StatusOK},
	}
}
