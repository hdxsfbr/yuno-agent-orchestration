package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"yuno/internal/engine"
	"yuno/internal/events"
	"yuno/internal/forgefake"
	"yuno/internal/liveness"
	"yuno/internal/project"
	"yuno/internal/project/projecttest"
	"yuno/internal/sched"
	"yuno/internal/store"
	"yuno/internal/tools"
	"yuno/internal/workflow"
)

// Chief platform control, DESIGN 5.34 rule 7, through the API and the real engine: a run
// that would work on a merge request its dependencies hold is created paused, no worker takes
// it, a resume or a retry is refused while the request is held, and the dependency watcher
// queues it once the request is let go (rule 8a).

// parkFixture is a server around the real engine (never started, so nothing claims a
// queued run), a project, an agent and a one-step workflow on the project.
type parkFixture struct {
	t       *testing.T
	srv     *Server
	ts      *httptest.Server
	eng     *engine.Engine
	project *store.Project
	agent   *store.Agent
	wf      *store.Workflow
	number  int64
}

func newParkFixture(t *testing.T, p *store.Project, d Deps) *parkFixture {
	t.Helper()
	pool := needDB(t)
	ctx := context.Background()
	cfg := testConfig()
	cfg.MaxSteps, cfg.MaxDurationSeconds, cfg.MaxNodeIterations, cfg.MaxLoopVisits = 200, 3600, 50, 10
	cfg.MaxCostUSD, cfg.MaxToolCalls, cfg.ApprovalTimeoutMaxSec = 20, 300, 604800
	rec := &events.Recorder{Hub: events.NewHub(), Append: func(ctx context.Context, runID, typ string, data json.RawMessage) (int64, error) {
		return store.AppendRunEvent(ctx, pool, runID, typ, data)
	}}
	eng := engine.New(engine.Deps{Pool: pool, Cfg: cfg, Recorder: rec})
	d.Cfg, d.Pool, d.Recorder = cfg, pool, rec
	srv, ts := newTestServerDeps(t, d, eng)
	f := &parkFixture{t: t, srv: srv, ts: ts, eng: eng, project: p, number: 100}
	f.agent = &store.Agent{Name: "Coder " + uniqueSuffix(), Role: "coder", SystemPrompt: "x", Model: "claude-sonnet-5-5", ToolAllowlist: []string{"exec"}}
	mustDo(t, store.CreateAgent(ctx, pool, f.agent))
	f.wf = f.workflow(nil)
	return f
}

// workflow stores a one-step workflow on the project for the fixture's agent; from names the
// template it is built from (nil: none).
func (f *parkFixture) workflow(from *string) *store.Workflow {
	f.t.Helper()
	def := workflow.SingleAgent(f.agent.ID, "{{.input.task}}", nil, store.Limits{})
	def.Project = true
	raw, err := json.Marshal(def)
	mustDo(f.t, err)
	wf := &store.Workflow{Name: "Dependent work " + uniqueSuffix(), Definition: raw, FromTemplate: from}
	mustDo(f.t, store.CreateWorkflow(context.Background(), testPool, wf))
	return wf
}

// ended stores a run of the project that has ended (the opener of a request).
func (f *parkFixture) ended() *store.Run {
	f.t.Helper()
	r := projectRun(f.t, f.project)
	_, err := testPool.Exec(context.Background(), `UPDATE runs SET status='succeeded', finished_at=now() WHERE id=$1`, r.ID)
	mustDo(f.t, err)
	return r
}

// request stores an open merge request record of the project, opened by a run that ended.
func (f *parkFixture) request() *store.MergeRequest {
	f.t.Helper()
	f.number++
	n := strconv.FormatInt(f.number, 10)
	mr := &store.MergeRequest{ProjectID: f.project.ID, RunID: f.ended().ID, Provider: "gitlab", Number: f.number,
		URL: "https://gitlab.test/g/d/-/merge_requests/" + n, SourceBranch: "yuno/b" + n, TargetBranch: "main", Title: "Change " + n}
	ok, err := store.InsertMergeRequest(context.Background(), testPool, mr)
	if err != nil || !ok {
		f.t.Fatalf("insert the request: %v %v", ok, err)
	}
	f.t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM merge_requests WHERE id=$1`, mr.ID) })
	return mr
}

// held is a request that depends on pre, the edge judged "!<pre> is not merged", as the
// watcher records it; it returns the reason.
func (f *parkFixture) held(pre *store.MergeRequest) (*store.MergeRequest, string) {
	f.t.Helper()
	ctx := context.Background()
	dep := f.request()
	_, err := store.ReplaceMergeRequestPrerequisites(ctx, testPool, f.project.ID, dep.ID, []string{pre.ID}, store.DependencyOperator,
		json.RawMessage(`{"kind":"operator"}`))
	mustDo(f.t, err)
	edges, err := store.DependenciesOf(ctx, testPool, store.DependencyMergeRequest, dep.ID)
	mustDo(f.t, err)
	reason := store.RequestLabel(pre.Provider, pre.Number) + " is not merged"
	mustDo(f.t, store.SetDependencyEvaluation(ctx, testPool, edges[0].ID, false, &reason, time.Now()))
	return dep, reason
}

func (f *parkFixture) call(method, path, body string, valid bool) (int, []byte) {
	f.t.Helper()
	return checkConformance(f.t, contract(f.t), f.ts.URL, method, path, body, valid)
}

// create is POST /api/runs of the fixture's workflow on the request mrID.
func (f *parkFixture) create(mrID string) store.Run {
	f.t.Helper()
	body, _ := json.Marshal(map[string]any{"workflow_id": f.wf.ID, "project_id": f.project.ID, "merge_request_id": mrID,
		"input": map[string]string{"task": "address the review"}})
	code, out := f.call(http.MethodPost, "/api/runs", string(body), true)
	var run store.Run
	if code != http.StatusCreated || json.Unmarshal(out, &run) != nil {
		f.t.Fatalf("create run = %d %s", code, out)
	}
	return run
}

// parked checks that run waits paused for its request's dependencies with reason, and that
// no worker took it.
func parked(t *testing.T, what string, run *store.Run, reason string) {
	t.Helper()
	got, err := store.GetRun(context.Background(), testPool, run.ID)
	mustDo(t, err)
	if got.Status != store.RunPaused || got.ParkReason != store.RunParkDependency || got.ParkDetail == nil || *got.ParkDetail != reason ||
		got.StartedAt != nil || got.LeaseEpoch != 0 {
		t.Fatalf("%s: run %s status %s park %q %v started %v epoch %d, want paused for %q", what, got.ID, got.Status, got.ParkReason,
			got.ParkDetail, got.StartedAt, got.LeaseEpoch, reason)
	}
}

func (f *parkFixture) diagnosis(runID string) runDiagnosis {
	f.t.Helper()
	code, body := f.call(http.MethodGet, "/api/runs/"+runID+"/diagnosis", "", true)
	var d runDiagnosis
	if code != http.StatusOK || json.Unmarshal(body, &d) != nil {
		f.t.Fatalf("diagnosis = %d %s", code, body)
	}
	return d
}

// TestRunOnDependentParks (DESIGN 5.34 rule 7): every path that creates a run on a merge
// request (POST /api/runs, the Chief's delegate_task, changes requested on a plan ticket, a
// schedule) creates it paused with park reason dependency and the reason while an edge holds
// the request; the diagnosis says blocked with that reason. A request nothing holds gets a
// queued run.
func TestRunOnDependentParks(t *testing.T) {
	ctx := context.Background()
	sfx := uniqueSuffix()
	p := seedProject(t, testCipher(t), "parks-"+sfx, acceptedTokenPrefix+sfx)
	f := newParkFixture(t, p, Deps{})
	pre := f.request()

	// A request no edge holds: queued as before.
	free := f.create(f.request().ID)
	if free.Status != store.RunQueued || free.ParkReason != "" || free.ParkDetail != nil {
		t.Fatalf("run on a free request = %+v", free)
	}

	// POST /api/runs.
	dep, reason := f.held(pre)
	if reason != "!101 is not merged" {
		t.Fatalf("reason = %q", reason)
	}
	viaAPI := f.create(dep.ID)
	parked(t, "POST /api/runs", &viaAPI, reason)
	d := f.diagnosis(viaAPI.ID)
	if d.Liveness != liveness.Blocked || d.Reason != reason || d.Status != store.RunPaused {
		t.Fatalf("diagnosis = %+v", d)
	}

	// The Chief's delegate_task (the delegator behind it), an agent quick run on the request.
	dep, reason = f.held(pre)
	viaChief, err := delegator{f.srv}.Delegate(ctx, tools.Delegation{AgentID: f.agent.ID, Task: "fix the review comments", ProjectID: p.ID,
		MergeRequestID: dep.ID})
	mustDo(t, err)
	parked(t, "delegate_task", viaChief, reason)

	// Changes requested on a plan ticket in review (the wave pipeline workflow, the oldest of
	// its template, is the fixture's for this test).
	dep, reason = f.held(pre)
	waveKey := waveTemplateKey
	wave := f.workflow(&waveKey)
	_, err = testPool.Exec(ctx, `UPDATE workflows SET created_at = '2000-01-01T00:00:00Z' WHERE id=$1`, wave.ID)
	mustDo(t, err)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `UPDATE workflows SET deleted_at = now() WHERE id=$1`, wave.ID)
	})
	plan, _ := seedPlan(t, p)
	mustDo(t, store.SetTicketMR(ctx, testPool, plan.ID, "T1", dep.ID))
	mustDo(t, store.SetTicketStatus(ctx, testPool, plan.ID, "T1", store.TicketInReview))
	code, body := f.call(http.MethodPost, "/api/plans/"+plan.ID+"/tickets/T1/changes", `{"notes":"Handle the empty query"}`, true)
	var viaPlan store.Run
	if code != http.StatusCreated || json.Unmarshal(body, &viaPlan) != nil {
		t.Fatalf("request changes = %d %s", code, body)
	}
	parked(t, "changes requested", &viaPlan, reason)

	// A schedule on the request, fired by the scheduler at an instant only its occurrence is
	// due.
	dep, reason = f.held(pre)
	due := time.Date(2001, 3, 1, 9, 0, 0, 0, time.UTC)
	s := &store.Schedule{WorkflowID: f.wf.ID, ProjectID: &p.ID, MergeRequestID: &dep.ID, Kind: store.ScheduleDelay, RunAt: &due,
		TZ: "UTC", Enabled: true, NextRunAt: &due, Input: json.RawMessage(`{"task":"address the review"}`)}
	mustDo(t, store.CreateSchedule(ctx, testPool, s))
	t.Cleanup(func() { _ = store.DeleteSchedule(context.Background(), testPool, s.ID) })
	fired, err := (&sched.Scheduler{Pool: testPool, Engine: f.eng, Clock: engine.NewFakeClock(due.Add(time.Minute))}).TickOnce(ctx)
	mustDo(t, err)
	if len(fired) != 1 || fired[0].Run == nil {
		t.Fatalf("fired = %+v", fired)
	}
	parked(t, "schedule", fired[0].Run, reason)
}

// TestResumeRefusedWhileBlocked (DESIGN 5.34 rule 7): while the request is held, the
// operator's (or the Chief's) resume of the parked run and a retry of an ended run on the
// request, or of the run that opened it, are refused with 409 dependency_blocked, "blocked by
// <reason>; remove the dependency to override", each a command recorded rejected. Once the
// dependency is removed (PUT .../dependencies), the resume and the retry go through.
func TestResumeRefusedWhileBlocked(t *testing.T) {
	ctx := context.Background()
	sfx := uniqueSuffix()
	p := seedProject(t, testCipher(t), "blocked-"+sfx, acceptedTokenPrefix+sfx)
	f := newParkFixture(t, p, Deps{})
	pre := f.request()
	dep, reason := f.held(pre)
	want := "blocked by " + reason + "; remove the dependency to override"
	run := f.create(dep.ID)
	parked(t, "created", &run, reason)

	refused := func(what, path string) {
		t.Helper()
		code, body := f.call(http.MethodPost, path, `{}`, true)
		e := decodeError(t, body)
		if code != http.StatusConflict || e.Error.Code != engine.CodeDependencyBlocked || e.Error.Message != want {
			t.Fatalf("%s = %d %s, want 409 %s %q", what, code, body, engine.CodeDependencyBlocked, want)
		}
		runID := strings.Split(path, "/")[3]
		list, err := store.ListRunCommands(ctx, testPool, runID)
		mustDo(t, err)
		if len(list) == 0 || list[0].Outcome != store.CommandRejected || list[0].Detail != want {
			t.Fatalf("%s: commands = %+v", what, list)
		}
	}
	refused("resume", "/api/runs/"+run.ID+"/resume")
	parked(t, "after the refused resume", &run, reason)

	// An ended run on another held request, retried.
	other, _ := f.held(pre)
	ended := projectRun(t, p)
	_, err := testPool.Exec(ctx, `UPDATE runs SET status='failed', finished_at=now(), current_node='agent', merge_request_id=$2 WHERE id=$1`,
		ended.ID, other.ID)
	mustDo(t, err)
	refused("retry", "/api/runs/"+ended.ID+"/retry")
	if got, err := store.GetRun(ctx, testPool, ended.ID); err != nil || got.Status != store.RunFailed || got.Retries != 0 {
		t.Fatalf("retried run after the refusal = %+v %v", got, err)
	}
	// The run that opened a held request names none, and its next push goes to the request.
	opened, _ := f.held(pre)
	_, err = testPool.Exec(ctx, `UPDATE runs SET status='failed', current_node='agent' WHERE id=$1`, opened.RunID)
	mustDo(t, err)
	refused("retry of the opener", "/api/runs/"+opened.RunID+"/retry")

	// The dependency removed: the resume and the retry go through.
	for _, id := range []string{dep.ID, other.ID} {
		if code, body := f.call(http.MethodPut, "/api/merge-requests/"+id+"/dependencies", `{"prerequisites":[]}`, true); code != http.StatusOK {
			t.Fatalf("remove the dependency = %d %s", code, body)
		}
	}
	code, body := f.call(http.MethodPost, "/api/runs/"+run.ID+"/resume", `{}`, true)
	var resumed store.Run
	if code != http.StatusAccepted || json.Unmarshal(body, &resumed) != nil || resumed.Status != store.RunQueued || resumed.ParkReason != "" ||
		resumed.ParkDetail != nil || resumed.Command == nil || resumed.Command.Outcome != store.CommandApplied {
		t.Fatalf("resume once nothing holds the request = %d %s", code, body)
	}
	code, body = f.call(http.MethodPost, "/api/runs/"+ended.ID+"/retry", `{}`, true)
	var retried store.Run
	if code != http.StatusAccepted || json.Unmarshal(body, &retried) != nil || retried.Status != store.RunQueued || retried.Retries != 1 {
		t.Fatalf("retry once nothing holds the request = %d %s", code, body)
	}
}

// Acceptance, DESIGN 5.34 (TestDependencyAcceptanceResumesRun), against the fake forge and
// the real engine: !2 (the dependent, "!24") depends on !1 (the prerequisite, "!17"); a run
// on !2 parks; !1 merges and main's pipeline passes on its merge commit; the watcher updates
// !2 from main (a push); CI passes on the new head; the parked run is queued once, lease
// epoch + 1, with the note for its first step.
func TestDependencyAcceptanceResumesRun(t *testing.T) {
	pool := needDB(t)
	projecttest.NeedGit(t)
	ctx := context.Background()
	e := &forgeEnv{t: t, pool: pool, router: contract(t), token: acceptedTokenPrefix + uniqueSuffix()}
	fk, err := forgefake.New(e.token, t.TempDir())
	mustDo(t, err)
	e.forge = httptest.NewServer(fk)
	t.Cleanup(e.forge.Close)
	e.forgeCall(http.MethodPost, "/_forge/repos", map[string]any{"path": "acme/app", "default_branch": "main"}, http.StatusCreated)
	e.pc = project.Config{RepoURL: e.forge.URL + "/acme/app.git", Token: e.token, DefaultBranch: "main", Provider: project.ProviderGitLab}
	e.project = &store.Project{ID: store.NewID(), Name: "forge-" + uniqueSuffix(), RepoURL: e.pc.RepoURL, Provider: "gitlab", DefaultBranch: "main"}
	blob, err := testCipher(t).Seal(e.project.ID, e.token)
	mustDo(t, err)
	mustDo(t, store.CreateProject(ctx, pool, e.project, blob))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM merge_requests WHERE project_id = $1`, e.project.ID)
		_ = store.DeleteProject(context.Background(), pool, e.project.ID)
	})
	f := newParkFixture(t, e.project, Deps{})
	e.ts = f.ts
	open := func(branch string, files map[string]string) *store.MergeRequest {
		t.Helper()
		e.push("main", branch, files)
		ref, err := tools.ProviderFor(&e.pc, nil).Create(ctx, branch, "main", "Change on "+branch, "", false)
		mustDo(t, err)
		mr := &store.MergeRequest{ProjectID: e.project.ID, RunID: f.ended().ID, Provider: "gitlab", Number: ref.Number, URL: ref.URL,
			SourceBranch: branch, TargetBranch: "main", Title: "Change on " + branch}
		if ok, err := store.InsertMergeRequest(ctx, pool, mr); err != nil || !ok {
			t.Fatalf("insert the request: %v %v", ok, err)
		}
		return mr
	}
	var mine []string
	// tick runs one sync pass and checks this test's own requests synced: other packages'
	// tests leave open requests in the shared database whose providers do not answer, so
	// the pass's own failure count is not this test's.
	tick := func() {
		t.Helper()
		f.srv.mergeSync().TickOnce(ctx)
		for _, id := range mine {
			rec, err := store.GetMergeRequest(ctx, pool, id)
			if err != nil {
				t.Fatal(err)
			}
			if rec.LastSyncError != nil && *rec.LastSyncError != "" {
				t.Fatalf("syncing %s failed: %s", rec.SourceBranch, *rec.LastSyncError)
			}
		}
	}

	pre := open("yuno/pre", map[string]string{"pre.txt": "prerequisite\n"})
	dep := open("yuno/dep", map[string]string{"dep.txt": "dependent\n"})
	mine = []string{pre.ID, dep.ID}
	if code, body := e.call(http.MethodPut, "/api/merge-requests/"+dep.ID+"/dependencies", `{"prerequisites":["`+pre.ID+`"]}`, true); code != http.StatusOK {
		t.Fatalf("set the dependency = %d %s", code, body)
	}
	e.pipeline("passed", "main", nil)
	e.pipeline("passed", "yuno/dep", nil)
	h1 := e.tip("yuno/dep")
	tick()

	// A run on !2 parks with the watcher's reason.
	run := f.create(dep.ID)
	parked(t, "run on !2", &run, "!1 is not merged")
	if d := f.diagnosis(run.ID); d.Liveness != liveness.Blocked || d.Reason != "!1 is not merged" {
		t.Fatalf("diagnosis while !1 is open = %+v", d)
	}

	// !1 merges; a sync of it looks at its dependents at once. Main has not passed on the
	// merge commit yet.
	e.forgeCall(http.MethodPost, "/_forge/repos/acme/app/merge-open", map[string]any{"iid": pre.Number}, http.StatusOK)
	merged := e.sync(pre.ID)
	if merged.State != store.MergeRequestMerged || merged.MergeSHA == nil {
		t.Fatalf("prerequisite after the merge = %+v", merged)
	}
	pending := "!1 merged; CI on " + (*merged.MergeSHA)[:7] + " is pending"
	if d := f.diagnosis(run.ID); d.Liveness != liveness.Blocked || d.Reason != pending {
		t.Fatalf("diagnosis before main's pipeline = %+v, want %q", d, pending)
	}

	// Main passes on exactly the merge commit: the watcher updates !2 from main with a push.
	e.pipeline("passed", "main", nil)
	tick()
	h2 := e.tip("yuno/dep")
	if got := e.get(dep.ID); h2 == h1 || got.HeadSHA == nil || *got.HeadSHA != h2 || got.CIStatus != store.CIStatusPending {
		t.Fatalf("after the update: tip %s (was %s), record %+v", h2, h1, got)
	}
	tick()
	parked(t, "while CI runs on the new head", &run, "!1 is not merged")
	if d := f.diagnosis(run.ID); d.Liveness != liveness.Blocked || d.Reason != "waiting for CI on "+h2[:7] {
		t.Fatalf("diagnosis while CI runs on %s = %+v", h2[:7], d)
	}

	// CI passes on the new head: the run is queued, once, with the note.
	e.pipeline("passed", "yuno/dep", nil)
	tick()
	tick()
	tick()
	got, err := store.GetRun(ctx, pool, run.ID)
	mustDo(t, err)
	note := "Its prerequisite !1 is merged; the platform merged main into this branch (" + h2[:7] + ") and CI passed."
	if got.Status != store.RunQueued || got.LeaseEpoch != 1 || got.ParkReason != "" || got.ParkDetail != nil || got.ResumeNote == nil ||
		*got.ResumeNote != note {
		t.Fatalf("run after the release: status %s epoch %d park %q %v note %v, want queued, epoch 1, %q", got.Status, got.LeaseEpoch,
			got.ParkReason, got.ParkDetail, got.ResumeNote, note)
	}
	var queuedLogs int
	mustDo(t, pool.QueryRow(ctx, `SELECT count(*) FROM run_events WHERE run_id=$1 AND event='log' AND data->>'message' LIKE '%The run is queued.'`,
		run.ID).Scan(&queuedLogs))
	if queuedLogs != 1 {
		t.Fatalf("queued log events = %d, want 1", queuedLogs)
	}
}
