package waves

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"yuno/internal/engine"
	"yuno/internal/plans"
	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// DESIGN 5.29 rules 10 to 15: the watcher, the advance and the ship run's endings.

// fakeIssues is an IssueProvider that records what it is asked.
type fakeIssues struct {
	mu      sync.Mutex
	open    []int64 // the open issues of the feature label
	hidden  []int64 // open issues the list leaves out (confidential on GitLab)
	stateEr error
	listErr error
	swapErr error
	closeEr error
	added   []int64 // issues given the intake label, in call order
	closed  []int64
	label   string
}

func (f *fakeIssues) ListLabelled(_ context.Context, _ string, _, _ int) ([]tools.Issue, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, false, f.listErr
	}
	var out []tools.Issue
	for _, n := range f.open {
		out = append(out, tools.Issue{Number: n})
	}
	return out, false, nil
}
func (f *fakeIssues) IssueState(_ context.Context, n int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stateEr != nil {
		return "", f.stateEr
	}
	for _, o := range append(append([]int64{}, f.open...), f.hidden...) {
		if o == n {
			return tools.IssueStateOpen, nil
		}
	}
	return tools.IssueStateClosed, nil
}
func (f *fakeIssues) EnsureLabels(context.Context, []tools.IssueLabel) error { return nil }
func (f *fakeIssues) SwapLabels(_ context.Context, n int64, add, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.swapErr != nil {
		return f.swapErr
	}
	f.label = add
	f.added = append(f.added, n)
	return nil
}
func (f *fakeIssues) Comment(context.Context, int64, string) error { return nil }
func (f *fakeIssues) Create(context.Context, string, string, []string) (tools.Issue, error) {
	return tools.Issue{}, errors.New("not used")
}
func (f *fakeIssues) ListAll(context.Context, string, int) ([]tools.Issue, error) { return nil, nil }
func (f *fakeIssues) Close(_ context.Context, n int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closeEr != nil {
		return f.closeEr
	}
	f.closed = append(f.closed, n)
	return nil
}

// fakeRequests answers only the default branch's status.
type fakeRequests struct {
	tools.RequestProvider
	mu     sync.Mutex
	status tools.BranchStatus
	err    error
}

func (f *fakeRequests) DefaultBranchStatus(context.Context) (tools.BranchStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, f.err
}

// fakeEngine creates the run as the engine would, through the store.
type fakeEngine struct {
	pool  store.Querier
	calls int
	err   error
	last  engine.TriggerRequest
}

func (f *fakeEngine) Trigger(ctx context.Context, req engine.TriggerRequest) (*store.Run, error) {
	f.calls++
	f.last = req
	if f.err != nil {
		return nil, f.err
	}
	wf, err := store.GetWorkflow(ctx, f.pool, req.WorkflowID)
	if err != nil {
		return nil, err
	}
	ref := req.TriggerRef
	settings := store.ProjectSettings{}
	run := &store.Run{WorkflowID: wf.ID, WorkflowVersion: wf.Version, WorkflowSnapshot: wf.Definition, Trigger: req.Trigger,
		TriggerRef: &ref, Input: req.Input, ProjectID: &req.ProjectID, Project: &settings}
	if err := store.CreateRun(ctx, f.pool, run); err != nil {
		return nil, err
	}
	return run, nil
}

type env struct {
	t       *testing.T
	svc     *Service
	issues  *fakeIssues
	reqs    *fakeRequests
	eng     *fakeEngine
	plans   *plans.Service
	project *store.Project
	plan    *store.Plan
	now     time.Time
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// newEnv makes a project with a plan of two waves (T1, T2 in wave 1; T3 in wave 2) in the
// given status, a ship_pipeline workflow instantiated from the template, and the fakes.
func newEnv(t *testing.T, status string) *env {
	t.Helper()
	pool := needDB(t)
	ctx := context.Background()
	e := &env{t: t, issues: &fakeIssues{open: []int64{11, 12, 13}}, reqs: &fakeRequests{}, now: time.Now().UTC().Truncate(time.Second)}
	e.eng = &fakeEngine{pool: pool}
	e.plans = &plans.Service{Pool: pool}
	t.Cleanup(e.plans.Wait)
	e.svc = &Service{Pool: pool, Plans: e.plans, Engine: e.eng,
		Config: func(context.Context, *store.Project) (*project.Config, error) {
			return &project.Config{Provider: project.ProviderGitLab}, nil
		},
		Issues:   func(*project.Config) tools.IssueProvider { return e.issues },
		Requests: func(*project.Config) tools.RequestProvider { return e.reqs },
	}
	e.project = &store.Project{Name: "demo-" + store.NewID()[20:], RepoURL: "https://gitlab.test/g/demo.git", Provider: "gitlab", DefaultBranch: "main"}
	mustDo(t, store.CreateProject(ctx, pool, e.project, []byte("sealed")))
	from := ShipTemplateKey
	mustDo(t, store.CreateWorkflow(ctx, pool, &store.Workflow{Name: "ship " + store.NewID(), FromTemplate: &from,
		Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}))
	run := e.newRun(store.TriggerManual, "")
	e.plan = &store.Plan{ProjectID: e.project.ID, Feature: "search", PlanRunID: run.ID, Status: status}
	mustDo(t, store.CreatePlan(ctx, pool, e.plan))
	e.ticket("T1", 1, 11, store.TicketPending)
	e.ticket("T2", 1, 12, store.TicketPending)
	e.ticket("T3", 2, 13, store.TicketPending)
	return e
}

func (e *env) newRun(trigger, ref string) *store.Run {
	e.t.Helper()
	wf := &store.Workflow{Name: "wf " + store.NewID(), Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(e.t, store.CreateWorkflow(context.Background(), testPool, wf))
	settings := e.project.Settings()
	run := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: trigger,
		ProjectID: &e.project.ID, Project: &settings}
	if ref != "" {
		run.TriggerRef = &ref
	}
	mustDo(e.t, store.CreateRun(context.Background(), testPool, run))
	return run
}

func (e *env) ticket(id string, wave int, issue int64, status string) {
	e.t.Helper()
	n := issue
	tk := &store.PlanTicket{PlanID: e.plan.ID, TicketID: id, Wave: wave, Title: "Title " + id, IssueNumber: &n,
		IssueURL: "https://gitlab.test/g/demo/-/issues/" + id, Status: status}
	mustDo(e.t, store.UpsertPlanTicket(context.Background(), testPool, tk))
}

// mr gives the ticket a merge request record in the state, merged (if so) at mergedAt, and
// sets the ticket as the platform would.
func (e *env) mr(ticket string, number int64, state string, mergedAt time.Time) {
	e.t.Helper()
	ctx := context.Background()
	run := e.newRun(store.TriggerIssue, "")
	mr := &store.MergeRequest{ProjectID: e.project.ID, RunID: run.ID, Provider: "gitlab", Number: number, URL: "u", State: state,
		SourceBranch: "b", TargetBranch: "main"}
	if state != store.MergeRequestOpen {
		at := mergedAt
		mr.ClosedAt = &at
	}
	ok, err := store.InsertMergeRequest(ctx, testPool, mr)
	mustDo(e.t, err)
	if !ok {
		e.t.Fatal("merge request not inserted")
	}
	mustDo(e.t, store.SetTicketMR(ctx, testPool, e.plan.ID, ticket, mr.ID))
	to := store.TicketInReview
	switch state {
	case store.MergeRequestMerged:
		to = store.TicketMerged
	}
	mustDo(e.t, store.SetTicketStatus(ctx, testPool, e.plan.ID, ticket, to))
}

func (e *env) merged(ticket string, number int64, at time.Time) {
	e.t.Helper()
	e.mr(ticket, number, store.MergeRequestMerged, at)
}

func (e *env) setStatus(ticket, status string) {
	e.t.Helper()
	mustDo(e.t, store.SetTicketStatus(context.Background(), testPool, e.plan.ID, ticket, status))
}

func (e *env) pause(reason string) {
	e.t.Helper()
	mustDo(e.t, store.SetWavesPaused(context.Background(), testPool, e.project.ID, true, reason))
}

func (e *env) pass() { e.t.Helper(); e.svc.Pass(context.Background()); e.plans.Wait() }

func (e *env) reload() *store.Plan {
	e.t.Helper()
	p, err := store.GetPlan(context.Background(), testPool, e.plan.ID)
	mustDo(e.t, err)
	return p
}

func (e *env) status(ticket string) string {
	e.t.Helper()
	l, err := store.ListPlanTickets(context.Background(), testPool, e.plan.ID)
	mustDo(e.t, err)
	for _, tk := range l {
		if tk.TicketID == ticket {
			return tk.Status
		}
	}
	e.t.Fatalf("no ticket %s", ticket)
	return ""
}

func (e *env) kinds() map[string]int {
	e.t.Helper()
	l, err := store.ListPlanEvents(context.Background(), testPool, e.plan.ID, 0)
	mustDo(e.t, err)
	out := map[string]int{}
	for _, ev := range l {
		out[ev.Kind]++
	}
	return out
}

func (e *env) ships() []store.ShipRun {
	e.t.Helper()
	l, err := store.ListShipRuns(context.Background(), testPool, e.plan.ID)
	mustDo(e.t, err)
	return l
}

func (e *env) setMain(state string, at time.Time) {
	e.reqs.mu.Lock()
	defer e.reqs.mu.Unlock()
	e.reqs.status = tools.BranchStatus{State: state, SHA: "abc123", At: at, URL: "https://gitlab.test/g/demo/-/pipelines/9"}
	e.reqs.err = nil
}

// ---- T5.29.9: the watcher ---------------------------------------------------------------

func TestWatcherReleasesWaveOneAfterPlanMerge(t *testing.T) {
	e := newEnv(t, store.PlanWaitingPlanMerge)
	ctx := context.Background()
	// No plan merge request yet, then an open one: nothing is released.
	e.pass()
	run := e.newRun(store.TriggerManual, "")
	mr := &store.MergeRequest{ProjectID: e.project.ID, RunID: run.ID, Provider: "gitlab", Number: 1, URL: "u", SourceBranch: "plan", TargetBranch: "main"}
	if ok, err := store.InsertMergeRequest(ctx, testPool, mr); err != nil || !ok {
		t.Fatalf("insert: %v %v", ok, err)
	}
	mustDo(t, store.SetPlanMR(ctx, testPool, e.plan.ID, mr.ID))
	e.pass()
	if p := e.reload(); p.Status != store.PlanWaitingPlanMerge || len(e.issues.added) != 0 {
		t.Fatalf("an open plan merge request released something: %s %v", p.Status, e.issues.added)
	}
	// Merged but paused: held.
	won, err := store.MarkMergeRequestMerged(ctx, testPool, mr.ID, "sync", time.Now().UTC())
	if err != nil || !won {
		t.Fatalf("merge: %v %v", won, err)
	}
	e.pause("operator is looking")
	e.pass()
	if p := e.reload(); p.Status != store.PlanWaitingPlanMerge || len(e.issues.added) != 0 {
		t.Fatalf("a paused project released a wave: %s %v", p.Status, e.issues.added)
	}
	mustDo(t, store.SetWavesPaused(ctx, testPool, e.project.ID, false, ""))
	e.pass()
	p := e.reload()
	if p.Status != store.PlanBuilding || p.CurrentWave != 1 {
		t.Fatalf("plan = %s wave %d, want building wave 1", p.Status, p.CurrentWave)
	}
	if len(e.issues.added) != 2 || e.issues.label != "yuno" {
		t.Fatalf("labelled %v with %q, want the two wave 1 issues with yuno", e.issues.added, e.issues.label)
	}
	if e.status("T1") != store.TicketReleased || e.status("T2") != store.TicketReleased || e.status("T3") != store.TicketPending {
		t.Fatalf("ticket statuses: %s %s %s", e.status("T1"), e.status("T2"), e.status("T3"))
	}
	if k := e.kinds(); k[store.PlanEventWaveReleased] != 1 {
		t.Fatalf("events = %v", k)
	}
	e.pass() // the plan is building now: no second release
	if k := e.kinds(); k[store.PlanEventWaveReleased] != 1 || len(e.issues.added) != 2 {
		t.Fatalf("a second pass released again: %v %v", k, e.issues.added)
	}
}

func TestReleaseTrackerErrorLeavesThePlanWaiting(t *testing.T) {
	e := newEnv(t, store.PlanWaitingPlanMerge)
	run := e.newRun(store.TriggerManual, "")
	mr := &store.MergeRequest{ProjectID: e.project.ID, RunID: run.ID, Provider: "gitlab", Number: 1, URL: "u", State: store.MergeRequestMerged, SourceBranch: "plan", TargetBranch: "main"}
	if ok, err := store.InsertMergeRequest(context.Background(), testPool, mr); err != nil || !ok {
		t.Fatalf("insert: %v %v", ok, err)
	}
	mustDo(t, store.SetPlanMR(context.Background(), testPool, e.plan.ID, mr.ID))
	e.issues.swapErr = errors.New("gitlab: 502")
	e.pass()
	if p := e.reload(); p.Status != store.PlanWaitingPlanMerge || e.status("T1") != store.TicketPending {
		t.Fatalf("a failed label released the wave: %s %s", p.Status, e.status("T1"))
	}
	e.issues.swapErr = nil
	e.pass()
	if p := e.reload(); p.Status != store.PlanBuilding || e.status("T1") != store.TicketReleased {
		t.Fatalf("the retry did not release: %s %s", p.Status, e.status("T1"))
	}
}

func TestWatcherShipsAWaveOnlyWhenMergedAndGreenAfterTheMerges(t *testing.T) {
	e := newEnv(t, store.PlanBuilding)
	t0 := e.now.Add(-time.Hour)
	e.setStatus("T1", store.TicketReleased)
	e.setStatus("T2", store.TicketReleased)
	e.merged("T1", 21, t0)
	e.setMain("passed", t0.Add(time.Minute))

	e.pass() // T2 is not merged
	if len(e.ships()) != 0 {
		t.Fatal("a ship run started with an unmerged ticket")
	}
	m, err := store.GetPlanMain(context.Background(), testPool, e.plan.ID)
	if err != nil || m == nil || m.State != "passed" || m.SHA != "abc123" || m.At == nil {
		t.Fatalf("main = %+v, %v; the watcher records what it read", m, err)
	}

	e.merged("T2", 22, t0.Add(10*time.Minute))
	e.pass() // main's pipeline is older than the newest merge
	if len(e.ships()) != 0 {
		t.Fatal("a pipeline older than the newest merge shipped the wave")
	}
	e.setMain("failed", t0.Add(20*time.Minute))
	e.pass()
	e.setMain("running", t0.Add(20*time.Minute))
	e.pass()
	e.setMain("none", time.Time{})
	e.pass()
	if len(e.ships()) != 0 || e.eng.calls != 0 {
		t.Fatal("a red, running or absent pipeline shipped the wave")
	}

	e.setMain("passed", t0.Add(10*time.Minute)) // at the newest merge exactly
	e.pass()
	ships := e.ships()
	if len(ships) != 1 || ships[0].Wave != 1 {
		t.Fatalf("ship runs = %+v, want one for wave 1", ships)
	}
	p := e.reload()
	if p.Status != store.PlanShipping || p.CurrentWave != 1 {
		t.Fatalf("plan = %s wave %d", p.Status, p.CurrentWave)
	}
	var in map[string]any
	mustDo(t, json.Unmarshal(e.eng.last.Input, &in))
	if in["plan_id"] != e.plan.ID || in["feature"] != "search" || in["wave"] != float64(1) || in["last_wave"] != float64(2) {
		t.Fatalf("ship run input = %v", in)
	}
	if e.eng.last.Trigger != store.TriggerPlan || e.eng.last.TriggerRef != "plan:"+e.plan.ID+":wave:1" || e.eng.last.ProjectID != e.project.ID {
		t.Fatalf("trigger = %+v", e.eng.last)
	}
	evs, _ := store.ListPlanEvents(context.Background(), testPool, e.plan.ID, 0)
	found := false
	for _, ev := range evs {
		if ev.Kind == store.PlanEventSmokeTestWaiting {
			var d struct {
				RunID string `json:"run_id"`
			}
			mustDo(t, json.Unmarshal(ev.Detail, &d))
			found = d.RunID == ships[0].RunID && ev.Wave != nil && *ev.Wave == 1
		}
	}
	if !found {
		t.Fatalf("no smoke_test_waiting event naming the run: %+v", evs)
	}
	e.pass()
	e.pass()
	if len(e.ships()) != 1 || e.eng.calls != 1 {
		t.Fatalf("a shipping plan started another run: %d runs, %d triggers", len(e.ships()), e.eng.calls)
	}
}

// H1: the wave ships on the provider's clock. A merge the platform recorded with the
// provider's merged_at, whose pipeline was created a moment before the platform noted the
// merge, ships; so does a pipeline of the merge commit whatever its time says.
func TestWatcherShipsOnTheProvidersClockAndTheMergeCommit(t *testing.T) {
	ctx := context.Background()
	mk := func() *env {
		e := newEnv(t, store.PlanBuilding)
		e.setStatus("T1", store.TicketReleased)
		e.setStatus("T2", store.TicketReleased)
		return e
	}
	// The pipeline (10:00:00.4) is before the platform's note (10:00:00.9) but after the
	// provider's merged_at (10:00:00.0).
	e := mk()
	merge := e.now.Add(-time.Hour)
	e.merged("T1", 31, merge.Add(-time.Minute))
	e.merged("T2", 32, merge)
	e.setMain("passed", merge.Add(400*time.Millisecond))
	e.pass()
	if len(e.ships()) != 1 {
		t.Fatal("a pipeline created after the provider's merged_at did not ship the wave")
	}
	var in map[string]any
	mustDo(t, json.Unmarshal(e.eng.last.Input, &in))
	mrs, _ := in["merge_requests"].([]any)
	if len(mrs) != 2 || in["main_url"] != "https://gitlab.test/g/demo/-/pipelines/9" {
		t.Fatalf("ship run input = %v; want the merged requests and main's pipeline", in)
	}
	if m, _ := mrs[0].(map[string]any); m["url"] != "u" || m["title"] == nil {
		t.Fatalf("merge_requests[0] = %v", mrs[0])
	}

	// Older pipeline, but of the newest merge's commit.
	e = mk()
	e.merged("T1", 33, merge.Add(-time.Minute))
	e.merged("T2", 34, merge)
	_, err := testPool.Exec(ctx, `UPDATE merge_requests SET merge_sha='abc123' WHERE number=34 AND project_id=$1`, e.project.ID)
	mustDo(t, err)
	e.setMain("passed", merge.Add(-time.Second))
	e.pass()
	if len(e.ships()) != 1 {
		t.Fatal("the pipeline of the merge commit did not ship the wave")
	}

	// A commit that is not the newest merge's, older than it: no.
	e = mk()
	e.merged("T1", 35, merge.Add(-time.Minute))
	e.merged("T2", 36, merge)
	_, err = testPool.Exec(ctx, `UPDATE merge_requests SET merge_sha='ffff' WHERE number=36 AND project_id=$1`, e.project.ID)
	mustDo(t, err)
	e.setMain("passed", merge.Add(-time.Second))
	e.pass()
	if len(e.ships()) != 0 {
		t.Fatal("an older pipeline of another commit shipped the wave")
	}
}

func TestWatcherNeverStartsASecondShipRun(t *testing.T) {
	e := newEnv(t, store.PlanBuilding)
	t0 := e.now.Add(-time.Hour)
	for i, id := range []string{"T1", "T2"} {
		e.merged(id, int64(30+i), t0)
	}
	e.setMain("passed", t0.Add(time.Minute))
	// A run for the wave exists already (the process stopped before the plan moved): the
	// watcher records it instead of starting another.
	run := e.newRun(store.TriggerPlan, "plan:"+e.plan.ID+":wave:1")
	e.pass()
	if e.eng.calls != 0 {
		t.Fatalf("triggered %d times with a run in place", e.eng.calls)
	}
	if p := e.reload(); p.Status != store.PlanShipping {
		t.Fatalf("plan = %s, want shipping", p.Status)
	}
	if s := e.ships(); len(s) != 1 || s[0].RunID != run.ID {
		t.Fatalf("ship runs = %+v", s)
	}
	// The unique index refuses a second run for the plan and wave, whatever it is called.
	again := &store.Run{WorkflowID: run.WorkflowID, WorkflowVersion: 1, WorkflowSnapshot: run.WorkflowSnapshot, Trigger: store.TriggerPlan, TriggerRef: run.TriggerRef}
	if err := store.CreateRun(context.Background(), testPool, again); !errors.Is(err, store.ErrDuplicateTrigger) {
		t.Fatalf("second ship run: %v, want ErrDuplicateTrigger", err)
	}
}

func TestWatcherLosingTheStartRaceRecordsTheWinner(t *testing.T) {
	e := newEnv(t, store.PlanBuilding)
	t0 := e.now.Add(-time.Hour)
	e.merged("T1", 41, t0)
	e.merged("T2", 42, t0)
	e.setMain("passed", t0.Add(time.Minute))
	var winner *store.Run
	e.svc.Engine = triggerFunc(func(ctx context.Context, req engine.TriggerRequest) (*store.Run, error) {
		winner = e.newRun(store.TriggerPlan, req.TriggerRef) // another process got there first
		return nil, engine.ErrDuplicateTrigger
	})
	e.pass()
	if s := e.ships(); len(s) != 1 || s[0].RunID != winner.ID {
		t.Fatalf("ship runs = %+v", s)
	}
	if p := e.reload(); p.Status != store.PlanShipping {
		t.Fatalf("plan = %s", p.Status)
	}
}

type triggerFunc func(context.Context, engine.TriggerRequest) (*store.Run, error)

func (f triggerFunc) Trigger(ctx context.Context, req engine.TriggerRequest) (*store.Run, error) {
	return f(ctx, req)
}

func TestWatcherHoldsWhilePausedButRecordsMain(t *testing.T) {
	e := newEnv(t, store.PlanBuilding)
	t0 := e.now.Add(-time.Hour)
	e.merged("T1", 51, t0)
	e.merged("T2", 52, t0)
	e.setMain("passed", t0.Add(time.Minute))
	e.pause("looking at it")
	e.pass()
	if len(e.ships()) != 0 || e.eng.calls != 0 {
		t.Fatal("a paused project started a ship run")
	}
	if m, _ := store.GetPlanMain(context.Background(), testPool, e.plan.ID); m == nil || m.State != "passed" {
		t.Fatalf("main = %+v; the page still shows it while paused", m)
	}
	mustDo(t, store.SetWavesPaused(context.Background(), testPool, e.project.ID, false, ""))
	e.pass()
	if len(e.ships()) != 1 {
		t.Fatal("resuming did not start the ship run")
	}
}

func TestWatcherFailedStartIsRetriedNotEscalated(t *testing.T) {
	e := newEnv(t, store.PlanBuilding)
	t0 := e.now.Add(-time.Hour)
	e.merged("T1", 61, t0)
	e.merged("T2", 62, t0)
	e.setMain("passed", t0.Add(time.Minute))
	e.eng.err = errors.New("budget reached")
	e.pass()
	if p := e.reload(); p.Status != store.PlanBuilding || e.kinds()[store.PlanEventEscalated] != 0 {
		t.Fatalf("plan = %s, events %v", p.Status, e.kinds())
	}
	e.eng.err = nil
	e.pass()
	if len(e.ships()) != 1 {
		t.Fatal("the next pass did not start the run")
	}
}

func TestWatcherTrackerErrorsAreNotEscalated(t *testing.T) {
	e := newEnv(t, store.PlanBuilding)
	e.setStatus("T1", store.TicketBuilding)
	e.setStatus("T2", store.TicketBuilding)

	e.issues.listErr = errors.New("gitlab: 503")
	e.reqs.err = errors.New("gitlab: 503")
	e.pass()
	if e.status("T1") != store.TicketBuilding || e.status("T2") != store.TicketBuilding {
		t.Fatal("a tracker outage failed a ticket")
	}
	if k := e.kinds(); k[store.PlanEventEscalated] != 0 {
		t.Fatalf("events = %v", k)
	}
	if p, _ := store.GetProject(context.Background(), testPool, e.project.ID); p.WavesPaused {
		t.Fatal("a tracker outage paused the waves")
	}
	if len(e.ships()) != 0 {
		t.Fatal("a ship run started on an unreadable branch")
	}
}

func TestWatcherEscalatesAClosedUnmergedRequest(t *testing.T) {
	e := newEnv(t, store.PlanBuilding)
	e.setStatus("T2", store.TicketBuilding)
	e.mr("T1", 71, store.MergeRequestClosed, e.now)
	e.setMain("passed", e.now)
	e.issues.open = []int64{11, 12}
	e.pass()
	if e.status("T1") != store.TicketFailed {
		t.Fatalf("T1 = %s, want failed", e.status("T1"))
	}
	p, _ := store.GetProject(context.Background(), testPool, e.project.ID)
	if !p.WavesPaused || p.WavesPausedReason == nil || *p.WavesPausedReason == "" {
		t.Fatalf("waves paused = %v %v", p.WavesPaused, p.WavesPausedReason)
	}
	if k := e.kinds(); k[store.PlanEventEscalated] != 1 {
		t.Fatalf("events = %v", k)
	}
	e.pass()
	if k := e.kinds(); k[store.PlanEventEscalated] != 1 {
		t.Fatalf("a second pass escalated again: %v", k)
	}
}

func TestWatcherEscalatesAnIssueClosedWithoutAMergeRequest(t *testing.T) {
	e := newEnv(t, store.PlanBuilding)
	e.setStatus("T1", store.TicketReleased)
	e.setStatus("T2", store.TicketReleased)
	e.setMain("passed", e.now)
	e.issues.open = []int64{11} // #12 is closed
	e.pass()
	if e.status("T1") != store.TicketReleased {
		t.Fatalf("T1 = %s; its issue is open", e.status("T1"))
	}
	if e.status("T2") != store.TicketFailed {
		t.Fatalf("T2 = %s, want failed", e.status("T2"))
	}
	if k := e.kinds(); k[store.PlanEventEscalated] != 1 {
		t.Fatalf("events = %v", k)
	}
}

// L2: only a released ticket is judged by its issue, and the issue is read itself: a
// confidential issue (left out of the list) is open, a building ticket's closed issue is not
// the watcher's to judge, and a tracker error concludes nothing.
func TestWatcherJudgesReleasedTicketsByTheirOwnIssue(t *testing.T) {
	e := newEnv(t, store.PlanBuilding)
	e.setStatus("T1", store.TicketReleased)
	e.setStatus("T2", store.TicketBuilding)
	e.setMain("passed", e.now)
	e.issues.open = nil
	e.issues.hidden = []int64{11} // #11 confidential, #12 closed
	e.pass()
	if e.status("T1") != store.TicketReleased || e.status("T2") != store.TicketBuilding {
		t.Fatalf("T1 = %s, T2 = %s; a confidential issue is open and a building ticket is not judged", e.status("T1"), e.status("T2"))
	}
	if k := e.kinds(); k[store.PlanEventEscalated] != 0 {
		t.Fatalf("events = %v", k)
	}
	e.issues.hidden = nil
	e.issues.stateEr = errors.New("tracker down")
	e.pass()
	if e.status("T1") != store.TicketReleased || e.kinds()[store.PlanEventEscalated] != 0 {
		t.Fatal("a tracker error escalated")
	}
}

func TestWatcherLeavesAnIssueClosedByItsOwnMergeAlone(t *testing.T) {
	e := newEnv(t, store.PlanBuilding)
	e.setStatus("T1", store.TicketReleased)
	e.mr("T2", 81, store.MergeRequestOpen, time.Time{}) // in review: the provider may have closed the issue on merge
	e.setMain("passed", e.now)
	e.issues.open = []int64{11}
	e.pass()
	if e.status("T2") != store.TicketInReview {
		t.Fatalf("T2 = %s; a request still open is the sync's to settle", e.status("T2"))
	}
	if k := e.kinds(); k[store.PlanEventEscalated] != 0 {
		t.Fatalf("events = %v", k)
	}
}

func TestWatcherIgnoresFinishedPlansAndDeletedProjects(t *testing.T) {
	e := newEnv(t, store.PlanBuilding)
	t0 := e.now.Add(-time.Hour)
	e.merged("T1", 91, t0)
	e.merged("T2", 92, t0)
	e.setMain("passed", t0.Add(time.Minute))
	mustDo(t, store.CancelPlan(context.Background(), testPool, e.plan.ID))
	e.pass()
	if e.eng.calls != 0 {
		t.Fatal("a cancelled plan was shipped")
	}
}

// ---- T5.29.10: advance_wave ---------------------------------------------------------------

// shipping puts the plan in shipping at wave and returns its ship run.
func (e *env) shipping(wave int, tickets ...string) *store.Run {
	e.t.Helper()
	ctx := context.Background()
	mustDo(e.t, store.SetCurrentWave(ctx, testPool, e.plan.ID, wave))
	mustDo(e.t, store.SetPlanStatus(ctx, testPool, e.plan.ID, store.PlanShipping))
	for i, id := range tickets {
		e.merged(id, int64(100+wave*10+i), e.now.Add(-time.Hour))
	}
	return e.newRun(store.TriggerPlan, store.ShipRef(e.plan.ID, wave))
}

func TestAdvanceClosesTheWaveAndReleasesTheNext(t *testing.T) {
	e := newEnv(t, store.PlanBuilding)
	run := e.shipping(1, "T1", "T2")
	res, err := e.svc.Advance(context.Background(), run.ID)
	e.plans.Wait()
	mustDo(t, err)
	want := tools.AdvanceResult{PlanID: e.plan.ID, Wave: 1, Closed: 2, Released: 1, Last: false}
	if res != want {
		t.Fatalf("result = %+v, want %+v", res, want)
	}
	if len(e.issues.closed) != 2 || len(e.issues.added) != 1 || e.issues.added[0] != 13 || e.issues.label != "yuno" {
		t.Fatalf("closed %v, labelled %v with %q", e.issues.closed, e.issues.added, e.issues.label)
	}
	if e.status("T1") != store.TicketClosed || e.status("T2") != store.TicketClosed || e.status("T3") != store.TicketReleased {
		t.Fatalf("tickets: %s %s %s", e.status("T1"), e.status("T2"), e.status("T3"))
	}
	if p := e.reload(); p.Status != store.PlanBuilding || p.CurrentWave != 2 {
		t.Fatalf("plan = %s wave %d", p.Status, p.CurrentWave)
	}
	k := e.kinds()
	if k[store.PlanEventWaveDone] != 1 || k[store.PlanEventWaveReleased] != 1 || k[store.PlanEventAcceptWaiting] != 0 {
		t.Fatalf("events = %v", k)
	}

	// A second call returns the first result and changes nothing.
	closedBefore, addedBefore := len(e.issues.closed), len(e.issues.added)
	again, err := e.svc.Advance(context.Background(), run.ID)
	mustDo(t, err)
	if again != want {
		t.Fatalf("second result = %+v, want %+v", again, want)
	}
	if len(e.issues.closed) != closedBefore || len(e.issues.added) != addedBefore {
		t.Fatal("a second call touched the tracker")
	}
	if k2 := e.kinds(); k2[store.PlanEventWaveDone] != 1 || k2[store.PlanEventWaveReleased] != 1 {
		t.Fatalf("events after the second call = %v", k2)
	}
}

func TestAdvanceLastWaveSetsTheAcceptanceWaiting(t *testing.T) {
	e := newEnv(t, store.PlanBuilding)
	e.setStatus("T1", store.TicketClosed)
	e.setStatus("T2", store.TicketClosed)
	run := e.shipping(2, "T3")
	res, err := e.svc.Advance(context.Background(), run.ID)
	e.plans.Wait()
	mustDo(t, err)
	if !res.Last || res.Closed != 1 || res.Released != 0 || res.Wave != 2 {
		t.Fatalf("result = %+v", res)
	}
	if p := e.reload(); p.Status != store.PlanAccepting || p.CurrentWave != 2 {
		t.Fatalf("plan = %s wave %d", p.Status, p.CurrentWave)
	}
	k := e.kinds()
	if k[store.PlanEventWaveDone] != 1 || k[store.PlanEventAcceptWaiting] != 1 {
		t.Fatalf("events = %v", k)
	}
	if again, err := e.svc.Advance(context.Background(), run.ID); err != nil || again != res {
		t.Fatalf("second call = %+v, %v", again, err)
	}
	if k := e.kinds(); k[store.PlanEventAcceptWaiting] != 1 {
		t.Fatalf("events = %v", k)
	}
}

func refusal(t *testing.T, err error) string {
	t.Helper()
	var r *tools.AdvanceRefused
	if !errors.As(err, &r) {
		t.Fatalf("error = %v, want a refusal", err)
	}
	return r.Reason
}

func TestAdvanceRefuses(t *testing.T) {
	ctx := context.Background()
	t.Run("a run that is not a ship run", func(t *testing.T) {
		e := newEnv(t, store.PlanShipping)
		_, err := e.svc.Advance(ctx, e.newRun(store.TriggerIssue, "p#1").ID)
		refusal(t, err)
	})
	t.Run("paused", func(t *testing.T) {
		e := newEnv(t, store.PlanBuilding)
		run := e.shipping(1, "T1", "T2")
		e.pause("smoke test failed")
		_, err := e.svc.Advance(ctx, run.ID)
		if r := refusal(t, err); r == "" {
			t.Fatal("empty reason")
		}
		if len(e.issues.closed) != 0 || e.status("T1") != store.TicketMerged || e.reload().Status != store.PlanShipping {
			t.Fatal("a refused advance had an effect")
		}
	})
	t.Run("not the current wave", func(t *testing.T) {
		e := newEnv(t, store.PlanBuilding)
		e.shipping(1, "T1", "T2")
		run := e.newRun(store.TriggerPlan, store.ShipRef(e.plan.ID, 2))
		_, err := e.svc.Advance(ctx, run.ID)
		refusal(t, err)
		if len(e.issues.closed) != 0 {
			t.Fatal("a refused advance closed issues")
		}
	})
	t.Run("a plan that is not shipping", func(t *testing.T) {
		e := newEnv(t, store.PlanBuilding)
		run := e.newRun(store.TriggerPlan, store.ShipRef(e.plan.ID, 1))
		_, err := e.svc.Advance(ctx, run.ID)
		refusal(t, err)
	})
	t.Run("a finished plan", func(t *testing.T) {
		e := newEnv(t, store.PlanBuilding)
		run := e.shipping(1, "T1", "T2")
		mustDo(t, store.CancelPlan(ctx, testPool, e.plan.ID))
		_, err := e.svc.Advance(ctx, run.ID)
		refusal(t, err)
	})
}

func TestAdvanceTrackerErrorIsRetriedWhole(t *testing.T) {
	e := newEnv(t, store.PlanBuilding)
	run := e.shipping(1, "T1", "T2")
	e.issues.closeEr = errors.New("gitlab: 502")
	_, err := e.svc.Advance(context.Background(), run.ID)
	var r *tools.AdvanceRefused
	if err == nil || errors.As(err, &r) {
		t.Fatalf("error = %v, want a plain error naming the ticket", err)
	}
	if e.status("T1") != store.TicketMerged || e.reload().Status != store.PlanShipping || e.kinds()[store.PlanEventWaveDone] != 0 {
		t.Fatal("a failed close left a partial advance")
	}
	e.issues.closeEr = nil
	res, err := e.svc.Advance(context.Background(), run.ID)
	e.plans.Wait()
	mustDo(t, err)
	if res.Closed != 2 || res.Released != 1 {
		t.Fatalf("retry result = %+v", res)
	}
}

func TestAdvanceRunsOnce(t *testing.T) {
	e := newEnv(t, store.PlanBuilding)
	run := e.shipping(1, "T1", "T2")
	var wg sync.WaitGroup
	results := make([]tools.AdvanceResult, 4)
	errs := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = e.svc.Advance(context.Background(), run.ID)
		}()
	}
	wg.Wait()
	e.plans.Wait()
	for i := range results {
		if errs[i] != nil || results[i] != results[0] {
			t.Fatalf("call %d = %+v, %v; want %+v", i, results[i], errs[i], results[0])
		}
	}
	if k := e.kinds(); k[store.PlanEventWaveDone] != 1 {
		t.Fatalf("events = %v", k)
	}
}

// L1: a pause that lands after the pass or the call began, before its transaction, stops
// the release, and the tracker is not written.
func TestPauseLandingMidwayStopsTheRelease(t *testing.T) {
	ctx := context.Background()
	t.Run("advance", func(t *testing.T) {
		e := newEnv(t, store.PlanBuilding)
		run := e.shipping(1, "T1", "T2")
		inner := e.svc.Config
		e.svc.Config = func(c context.Context, p *store.Project) (*project.Config, error) {
			e.pause("operator paused meanwhile")
			return inner(c, p)
		}
		_, err := e.svc.Advance(ctx, run.ID)
		refusal(t, err)
		if len(e.issues.closed) != 0 || len(e.issues.added) != 0 {
			t.Fatalf("tracker written: closed %v, labelled %v", e.issues.closed, e.issues.added)
		}
		if p := e.reload(); p.Status != store.PlanShipping || e.kinds()[store.PlanEventWaveDone] != 0 {
			t.Fatalf("plan = %s, events %v", p.Status, e.kinds())
		}
	})
	t.Run("first release", func(t *testing.T) {
		e := newEnv(t, store.PlanWaitingPlanMerge)
		run := e.newRun(store.TriggerManual, "")
		mr := &store.MergeRequest{ProjectID: e.project.ID, RunID: run.ID, Provider: "gitlab", Number: 1, URL: "u", SourceBranch: "plan", TargetBranch: "main"}
		if ok, err := store.InsertMergeRequest(ctx, testPool, mr); err != nil || !ok {
			t.Fatalf("insert: %v %v", ok, err)
		}
		mustDo(t, store.SetPlanMR(ctx, testPool, e.plan.ID, mr.ID))
		if won, err := store.MarkMergeRequestMerged(ctx, testPool, mr.ID, "sync", time.Now().UTC()); err != nil || !won {
			t.Fatalf("merge: %v %v", won, err)
		}
		inner := e.svc.Config
		e.svc.Config = func(c context.Context, p *store.Project) (*project.Config, error) {
			e.pause("operator paused meanwhile")
			return inner(c, p)
		}
		e.pass()
		if p := e.reload(); p.Status != store.PlanWaitingPlanMerge || len(e.issues.added) != 0 {
			t.Fatalf("plan = %s, labelled %v", p.Status, e.issues.added)
		}
	})
}
