package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"yuno/internal/forgefake"
	"yuno/internal/project"
	"yuno/internal/project/projecttest"
	"yuno/internal/store"
	"yuno/internal/tools"
)

const forgeTestToken = "forge-eval-test-token"

// projectAPI wraps the simulated api with the endpoints a project case uses: projects,
// plans, project runs and their approvals, the intake poll and workflows by name.
type projectAPI struct {
	t    *testing.T
	sim  *simAPI
	srv  *httptest.Server
	hook func(ProjectRequest) // called when the project is created

	mu        sync.Mutex
	projects  map[string]ProjectRequest
	deleted   []string
	polled    int
	approved  []string
	pending   bool // an approval of a ship run waits
	planState string
	planWave  bool                 // the plan has a wave_done event for wave 2
	bound     []scriptRegistration // scripts registered with a binding, in order
	pauses    []string             // pause and resume calls, in order
	shipRuns  []store.ShipRun      // the plan detail's ship runs
	tickets   []store.PlanTicket   // the plan detail's tickets
	planReads int
	cancelled []string // plans cancelled by the cleanup
}

func newProjectAPI(t *testing.T, sim *simAPI) *projectAPI {
	t.Helper()
	p := &projectAPI{t: t, sim: sim, projects: map[string]ProjectRequest{}, planState: store.PlanPublishing}
	inner := sim.srv.Config.Handler
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.serve(w, r) {
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *projectAPI) serve(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if r.Method == http.MethodPost && path == "/api/evals/scripts" {
		// Record a binding, then let the simulated api register the script as usual.
		raw, _ := io.ReadAll(r.Body)
		var reg scriptRegistration
		if json.Unmarshal(raw, &reg) == nil && reg.Bind != nil {
			p.mu.Lock()
			p.bound = append(p.bound, reg)
			p.mu.Unlock()
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		return false
	}
	own := strings.HasPrefix(path, "/api/projects") || strings.HasPrefix(path, "/api/plans/") ||
		(r.Method == http.MethodGet && (path == "/api/workflows" || path == "/api/runs")) ||
		(r.Method == http.MethodGet && path == "/api/approvals" && strings.HasPrefix(r.URL.Query().Get("run_id"), "prun-")) ||
		strings.HasPrefix(path, "/api/approvals/ap-")
	if !own {
		return false
	}
	if r.Header.Get("Authorization") != "Bearer "+fakeToken {
		writeErr(w, 401, "unauthorized", "missing or wrong bearer token")
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && path == "/api/projects":
		var req ProjectRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 422, "bad_request", err.Error())
			return true
		}
		id := "proj-" + req.Name
		p.projects[id] = req
		if p.hook != nil {
			p.mu.Unlock()
			p.hook(req)
			p.mu.Lock()
		}
		writeJSON(w, 201, store.Project{ID: id, Name: req.Name})
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/api/projects/"):
		p.deleted = append(p.deleted, strings.TrimPrefix(path, "/api/projects/"))
		w.WriteHeader(204)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/waves/pause"):
		var in struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		p.pauses = append(p.pauses, "pause: "+in.Reason)
		writeJSON(w, 200, store.Project{ID: "p"})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/waves/resume"):
		p.pauses = append(p.pauses, "resume")
		writeJSON(w, 200, store.Project{ID: "p"})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/intake/poll"):
		p.polled++
		w.WriteHeader(204)
	case r.Method == http.MethodGet && path == "/api/workflows":
		writeJSON(w, 200, []store.Workflow{{ID: "wf-feature", Name: "feature_pipeline"}, {ID: "wf-other", Name: "other"}})
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/plans"):
		// The plan moves along with each read, the way the platform's does with time.
		p.planReads++
		if p.planReads >= 3 {
			p.planState = store.PlanBuilding
		}
		writeJSON(w, 200, []store.Plan{{ID: "plan-1", Status: p.planState}})
	case r.Method == http.MethodGet && path == "/api/plans/plan-1":
		view := PlanView{Plan: store.Plan{ID: "plan-1", Status: p.planState}, Tickets: p.tickets, ShipRuns: p.shipRuns}
		w1, w2 := 1, 2
		view.Events = []store.PlanEvent{{Kind: store.PlanEventWaveDone, Wave: &w1}}
		if p.planWave {
			view.Events = append(view.Events, store.PlanEvent{Kind: store.PlanEventWaveDone, Wave: &w2})
		}
		writeJSON(w, 200, view)
	case r.Method == http.MethodPost && path == "/api/plans/plan-1/cancel":
		p.cancelled = append(p.cancelled, "plan-1")
		writeJSON(w, 200, store.Plan{ID: "plan-1"})
	case r.Method == http.MethodGet && path == "/api/runs":
		if r.URL.Query().Get("status") == store.RunWaitingApproval && p.pending {
			writeJSON(w, 200, []store.Run{{ID: "prun-1", Status: store.RunWaitingApproval}})
			return true
		}
		writeJSON(w, 200, []store.Run{})
	case r.Method == http.MethodGet && path == "/api/approvals":
		if !p.pending {
			writeJSON(w, 200, []ApprovalItem{})
			return true
		}
		other := store.Approval{ID: "ap-other", RunID: "prun-1", NodeID: "accept_feature", Status: store.ApprovalPending, RequestedAt: time.Now().Add(-time.Minute)}
		smoke := store.Approval{ID: "ap-smoke", RunID: "prun-1", NodeID: "smoke_test", Status: store.ApprovalPending, RequestedAt: time.Now()}
		writeJSON(w, 200, []ApprovalItem{{Approval: smoke}, {Approval: other}})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/approve"):
		p.approved = append(p.approved, strings.TrimSuffix(strings.TrimPrefix(path, "/api/approvals/"), "/approve"))
		p.pending = false
		writeJSON(w, 200, store.Approval{ID: "ap-smoke", Status: store.ApprovalApproved})
	default:
		writeErr(w, 404, "not_found", r.Method+" "+path)
	}
	return true
}

// projectCase is a quick-run case with a project and the steps of a wave: the issue the
// case opens, the merge request the platform opens, the default branch's pipeline, the
// plan and the manual test's approval.
func projectCase(t *testing.T) Case {
	t.Helper()
	c := shippedCase(t, "001-quick-run-happy")
	c.Project = &ProjectSpec{
		Intake:           &IntakeSpec{Enabled: true, Label: "yuno", Workflow: "feature_pipeline"},
		MaxCostUSDPerDay: 5,
		Issues:           []IssueSeed{{Title: "Seeded ticket", Body: "from the case", Labels: []string{"yuno"}}},
	}
	c.Steps = []CaseStep{
		{PollIntake: true},
		{Forge: &ForgeStep{CreateIssue: &IssueSeed{Title: "Second ticket", Labels: []string{"yuno", "wave:1"}}}},
		{Forge: &ForgeStep{ExpectIssues: []IssueExpect{
			{TitleContains: "seeded", Labels: []string{"yuno"}, State: "opened"},
			{TitleContains: "second ticket", Labels: []string{"wave:1"}},
		}, TimeoutSeconds: 5}},
		{Wait: &WaitStep{PlanStatus: store.PlanBuilding, TimeoutSeconds: 5}},
		{Forge: &ForgeStep{MergeOpen: &MergeOpenStep{Count: 1}, TimeoutSeconds: 5}},
		{Forge: &ForgeStep{Pipeline: "passed"}},
		{Wait: &WaitStep{PlanEvent: store.PlanEventWaveDone, Wave: 1, TimeoutSeconds: 5}},
		{Approve: &ApproveStep{Decision: DecisionApprove, Node: "smoke_test", TimeoutSeconds: 5}},
	}
	if err := c.Validate(c.Version); err != nil {
		t.Fatalf("the project case is invalid: %v", err)
	}
	return c
}

// platformOpensAMergeRequest plays the platform for a project: it pushes a branch to the
// project's repository and opens a merge request, as the ticket run's open_merge_request
// does, through the real git and provider code.
func platformOpensAMergeRequest(t *testing.T, req ProjectRequest) {
	t.Helper()
	pc := project.Config{RepoURL: req.RepoURL, Token: req.Token, DefaultBranch: req.DefaultBranch, Provider: project.ProviderGitLab}
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "co")
	if err := pc.Clone(ctx, dir); err != nil {
		t.Errorf("clone: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Error(err)
		return
	}
	if _, err := pc.Propose(ctx, dir, "yuno/ticket-1", "Ticket 1"); err != nil {
		t.Errorf("push: %v", err)
		return
	}
	if _, err := tools.ProviderFor(&pc, nil).Create(ctx, "yuno/ticket-1", req.DefaultBranch, "Ticket 1", "", false); err != nil {
		t.Errorf("open the merge request: %v", err)
	}
}

// onlyProject is the one entry of the map.
func onlyProject(m map[string]ProjectRequest) (string, ProjectRequest) {
	for id, req := range m {
		return id, req
	}
	return "", ProjectRequest{}
}

func projectRunner(t *testing.T, api *projectAPI) (*Runner, *httptest.Server, *forgefake.Server) {
	t.Helper()
	projecttest.NeedGit(t)
	fs, err := forgefake.New(forgeTestToken, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	forge := httptest.NewServer(fs)
	t.Cleanup(forge.Close)
	r := fastRunner(testConfig(api.srv.URL))
	r.forge, r.forgeURL = newForgeClient(forge.URL, forgeTestToken), forge.URL
	return r, forge, fs
}

func TestProjectCaseRunsItsStepsAgainstTheForge(t *testing.T) {
	c := projectCase(t)
	sim := newSimAPI(t, c)
	api := newProjectAPI(t, sim)
	api.hook = func(req ProjectRequest) { platformOpensAMergeRequest(t, req) }
	// The manual test waits from the start; the plan has finished wave 1 and 2 by the time
	// the steps look.
	api.pending, api.planWave = true, true
	r, forge, _ := projectRunner(t, api)

	res := r.runCase(context.Background(), c)
	if !res.Passed() || res.Error != "" {
		t.Fatalf("result = passed %v error %q", res.Passed(), res.Error)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.projects) != 1 {
		t.Fatalf("projects created: %v", api.projects)
	}
	id, req := onlyProject(api.projects)
	if !strings.HasPrefix(req.RepoURL, forge.URL+"/eval/001-quick-run-happy-") || !strings.HasSuffix(req.RepoURL, ".git") ||
		req.Provider != "gitlab" || req.Token != forgeTestToken || req.DefaultBranch != "main" ||
		req.MaxCostUSDPerDay == nil || *req.MaxCostUSDPerDay != 5 {
		t.Errorf("project request %+v", req)
	}
	if in := req.Intake; in == nil || !in.Enabled || in.Label != "yuno" || in.WorkflowID != "wf-feature" {
		t.Errorf("intake %+v", in)
	}
	if len(api.deleted) != 1 || api.deleted[0] != id {
		t.Errorf("deleted projects %v, want %s", api.deleted, id)
	}
	if len(api.cancelled) != 1 {
		t.Errorf("open plans cancelled at the end: %v", api.cancelled)
	}
	if api.polled != 1 {
		t.Errorf("intake polled %d times", api.polled)
	}
	if len(api.approved) != 1 || api.approved[0] != "ap-smoke" {
		t.Errorf("approved %v: only the smoke_test approval was asked for", api.approved)
	}
	sim.mu.Lock()
	defer sim.mu.Unlock()
	if len(sim.runRequests) != 1 || sim.runRequests[0].ProjectID != id {
		t.Errorf("run requests %+v, want the run on project %s", sim.runRequests, id)
	}

	// What the forge holds now: the seeded issues, and the merge request merged.
	f := forgeClient{base: forge.URL, token: forgeTestToken, http: http.DefaultClient}
	issues, err := f.Issues(context.Background(), strings.TrimSuffix(strings.TrimPrefix(req.RepoURL, forge.URL+"/"), ".git"))
	if err != nil || len(issues) != 2 {
		t.Fatalf("forge issues %+v %v", issues, err)
	}
}

func TestProjectStepFailureFailsTheCaseAndCancelsTheRun(t *testing.T) {
	c := projectCase(t)
	c.Steps = []CaseStep{{Forge: &ForgeStep{ExpectIssues: []IssueExpect{{TitleContains: "never opened", State: "closed"}}, TimeoutSeconds: 1}}}
	// The run would take long to finish; the failed step cancels it.
	sim := newSimAPI(t, c)
	sim.mu.Lock()
	sim.sims[c.ID].c.Actions.CancelWhenWaiting = true
	sim.mu.Unlock()
	api := newProjectAPI(t, sim)
	r, _, _ := projectRunner(t, api)

	res := r.runCase(context.Background(), c)
	if res.Passed() || !strings.Contains(res.Error, "step 1 (forge expect_issues): not met within 1s: no issue is titled like \"never opened\", closed") {
		t.Fatalf("result = passed %v error %q", res.Passed(), res.Error)
	}
	if !strings.Contains(res.Error, "#1 opened [yuno]") {
		t.Errorf("the error does not say what the forge holds: %q", res.Error)
	}
	sim.mu.Lock()
	defer sim.mu.Unlock()
	if len(sim.cancels) == 0 {
		t.Error("the run was not cancelled")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.deleted) != 1 {
		t.Errorf("deleted projects %v", api.deleted)
	}
}

func TestProjectCaseNeedsTheForge(t *testing.T) {
	c := projectCase(t)
	sim := newSimAPI(t, c)
	api := newProjectAPI(t, sim)
	r := fastRunner(testConfig(api.srv.URL))
	r.forge, r.forgeURL = nil, ""
	res := r.runCase(context.Background(), c)
	if res.Passed() || !strings.Contains(res.Error, "EVAL_FORGE_URL is not set") {
		t.Fatalf("result = passed %v error %q", res.Passed(), res.Error)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.projects) != 0 {
		t.Errorf("a project was created without a forge: %v", api.projects)
	}
}

func TestProjectApprovalStepsLeaveTheRunsOwnApprovalsToTheCase(t *testing.T) {
	// Without an approve step an approval nobody decides stalls the case; with one the
	// step decides it. The run's own approval actions are used up first, in order.
	c := Case{Project: &ProjectSpec{}, Steps: []CaseStep{{Approve: &ApproveStep{Decision: DecisionApprove}}}}
	if !c.hasApproveStep() {
		t.Fatal("approve step not seen")
	}
	if (Case{}).hasApproveStep() {
		t.Fatal("approve step seen in a case without steps")
	}
}

func TestProjectCaseValidation(t *testing.T) {
	base := func() Case { return projectCase(t) }
	cases := []struct {
		name   string
		mutate func(*Case)
		want   string // "" is valid
	}{
		{"valid", func(*Case) {}, ""},
		{"steps without a project", func(c *Case) { c.Project = nil }, "steps need a project block"},
		{"intake without a workflow", func(c *Case) { c.Project.Intake.Workflow = "" }, "project.intake.workflow is required"},
		{"intake without a cap", func(c *Case) { c.Project.MaxCostUSDPerDay = 0 }, "needs project.max_cost_usd_per_day"},
		{"intake off needs neither", func(c *Case) {
			c.Project.Intake = &IntakeSpec{}
			c.Project.MaxCostUSDPerDay = 0
		}, ""},
		{"a label with a comma", func(c *Case) { c.Project.Intake.Label = "a,b" }, "project.intake.label"},
		{"a seed issue without a title", func(c *Case) { c.Project.Issues[0].Title = " " }, "project.issues[0].title is required"},
		{"a bad branch", func(c *Case) { c.Project.DefaultBranch = "bad branch" }, "project.default_branch"},
		{"an empty step", func(c *Case) { c.Steps[0] = CaseStep{} }, "steps[0] must set exactly one"},
		{"two things in a step", func(c *Case) { c.Steps[0] = CaseStep{PollIntake: true, Wait: &WaitStep{PlanStatus: store.PlanDone}} }, "steps[0] must set exactly one"},
		{"an empty forge step", func(c *Case) { c.Steps[1] = CaseStep{Forge: &ForgeStep{}} }, "steps[1].forge must set exactly one"},
		{"two forge actions", func(c *Case) {
			c.Steps[1].Forge.Pipeline = "passed"
		}, "steps[1].forge must set exactly one"},
		{"a pipeline state", func(c *Case) { c.Steps[5].Forge.Pipeline = "green" }, "steps[5].forge.pipeline"},
		{"an expectation that expects nothing", func(c *Case) { c.Steps[2].Forge.ExpectIssues[0] = IssueExpect{} }, "sets none of"},
		{"an issue state", func(c *Case) { c.Steps[2].Forge.ExpectIssues[0].State = "open" }, "expect_issues[0].state"},
		{"a wait for nothing", func(c *Case) { c.Steps[3].Wait = &WaitStep{} }, "exactly one of plan_status, plan_event or ticket_status"},
		{"a wait for both", func(c *Case) { c.Steps[3].Wait.PlanEvent = store.PlanEventWaveDone }, "exactly one of plan_status, plan_event or ticket_status"},
		{"an unknown plan status", func(c *Case) { c.Steps[3].Wait.PlanStatus = "sleeping" }, "plan_status"},
		{"an unknown plan event", func(c *Case) { c.Steps[6].Wait.PlanEvent = "wave_over" }, "plan_event"},
		{"a wave on a status wait", func(c *Case) { c.Steps[3].Wait.Wave = 1 }, "belongs to plan_event"},
		{"an approve without a decision", func(c *Case) { c.Steps[7].Approve.Decision = "" }, "approve.decision"},
		{"a timeout too long", func(c *Case) { c.Steps[3].Wait.TimeoutSeconds = 99999 }, "timeout_seconds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			// Copy what the mutations touch, so the cases do not share it.
			p := *c.Project
			in := *p.Intake
			p.Intake = &in
			p.Issues = append([]IssueSeed(nil), p.Issues...)
			c.Project = &p
			steps := make([]CaseStep, len(c.Steps))
			for i, s := range c.Steps {
				if s.Forge != nil {
					f := *s.Forge
					f.ExpectIssues = append([]IssueExpect(nil), f.ExpectIssues...)
					s.Forge = &f
				}
				if s.Wait != nil {
					w := *s.Wait
					s.Wait = &w
				}
				if s.Approve != nil {
					a := *s.Approve
					s.Approve = &a
				}
				steps[i] = s
			}
			c.Steps = steps
			tc.mutate(&c)
			err := c.Validate(c.Version)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("error %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestProjectCaseYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `id: p-1
version: v1
split: public
description: a project case
agent:
  name: worker
  system_prompt: Reply done.
task: Say done.
project:
  default_branch: trunk
  max_cost_usd_per_day: 2
  intake: {enabled: true, label: yuno, workflow: feature_pipeline}
  issues:
    - {title: Build it, labels: [yuno]}
steps:
  - poll_intake: true
  - forge: {expect_issues: [{title_contains: build, state: opened}], timeout_seconds: 10}
  - wait: {plan_event: wave_done, wave: 1}
  - forge: {merge_open: {count: 2}}
  - forge: {pipeline: failed}
  - approve: {decision: reject, reason: no, node: smoke_test}
script:
  - text: done
expect:
  status: succeeded
`
	if err := os.WriteFile(filepath.Join(dir, "v1", "p-1.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cases, err := LoadCases(dir, SplitAll)
	if err != nil || len(cases) != 1 {
		t.Fatalf("load: %v %v", cases, err)
	}
	c := cases[0]
	if c.Project == nil || c.Project.DefaultBranch != "trunk" || len(c.Steps) != 6 || c.Steps[3].Forge.MergeOpen.Count != 2 || c.Steps[2].Wait.Wave != 1 {
		t.Fatalf("decoded %+v", c)
	}
	// A typo in a step is an unknown field, not a step that does nothing.
	bad := strings.Replace(body, "poll_intake: true", "poll_intakes: true", 1)
	if err := os.WriteFile(filepath.Join(dir, "v1", "p-1.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCases(dir, SplitAll); err == nil || !strings.Contains(err.Error(), "poll_intakes") {
		t.Fatalf("a misspelt step key: %v", err)
	}
}

// The steps of the wave loop (DESIGN 5.29 rule 17): scripts bound to the runs the platform
// starts, a pause that holds a ship run back, and the resume.
func TestProjectCasePausesHoldsAndResumes(t *testing.T) {
	c := projectCase(t)
	c.Project.PlatformRuns = []PlatformRun{
		{Trigger: "issue", Script: []ScriptStep{{Text: "ticket one"}}},
		{Trigger: "plan", Script: []ScriptStep{{Text: "ship one"}}},
		{Trigger: "issue", Script: []ScriptStep{{Text: "ticket two"}}},
	}
	c.Steps = []CaseStep{
		{PauseWaves: "Holding wave 2."},
		{Wait: &WaitStep{TicketStatus: store.TicketMerged, Wave: 2, TimeoutSeconds: 5}},
		{Hold: &HoldStep{NoShipRun: 2, Seconds: 1}},
		{ResumeWaves: true},
	}
	if err := c.Validate(c.Version); err != nil {
		t.Fatal(err)
	}
	sim := newSimAPI(t, c)
	api := newProjectAPI(t, sim)
	api.tickets = []store.PlanTicket{{Wave: 1, Status: store.TicketClosed}, {Wave: 2, Status: store.TicketMerged}}
	api.shipRuns = []store.ShipRun{{Wave: 1, RunID: "ship-1", Status: "succeeded"}}
	r, _, _ := projectRunner(t, api)

	res := r.runCase(context.Background(), c)
	if !res.Passed() || res.Error != "" {
		t.Fatalf("result = passed %v error %q", res.Passed(), res.Error)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.bound) != 3 {
		t.Fatalf("bound scripts: %+v", api.bound)
	}
	for i, want := range []string{"issue", "plan", "issue"} {
		b := api.bound[i]
		id, _ := onlyProject(api.projects)
		if b.Bind.Trigger != want || b.Bind.ProjectID != id || !strings.Contains(b.ScriptID, "-"+want+"-") || len(b.Steps) != 1 {
			t.Errorf("bound script %d: %+v", i, b)
		}
	}
	if strings.Join(api.pauses, "|") != "pause: Holding wave 2.|resume" {
		t.Errorf("pause calls: %v", api.pauses)
	}
}

// A ship run that starts for the held wave fails the case, and so does a wave whose tickets
// are not all in the status waited for.
func TestProjectHoldFailsWhenAShipRunStarts(t *testing.T) {
	c := projectCase(t)
	c.Steps = []CaseStep{{Hold: &HoldStep{NoShipRun: 2, Seconds: 5}}}
	sim := newSimAPI(t, c)
	api := newProjectAPI(t, sim)
	api.shipRuns = []store.ShipRun{{Wave: 2, RunID: "ship-2", Status: "running"}}
	r, _, _ := projectRunner(t, api)
	res := r.runCase(context.Background(), c)
	if res.Passed() || !strings.Contains(res.Error, "step 1 (hold no ship run for wave 2): a ship run (ship-2, running) started for wave 2") {
		t.Fatalf("result = passed %v error %q", res.Passed(), res.Error)
	}

	c.Steps = []CaseStep{{Wait: &WaitStep{TicketStatus: store.TicketMerged, Wave: 2, TimeoutSeconds: 1}}}
	api.tickets = []store.PlanTicket{{Wave: 2, Status: store.TicketMerged}, {Wave: 2, Status: store.TicketInReview}}
	res = r.runCase(context.Background(), c)
	if res.Passed() || !strings.Contains(res.Error, "1 of 2 tickets of wave 2 are merged") {
		t.Fatalf("result = passed %v error %q", res.Passed(), res.Error)
	}
}
