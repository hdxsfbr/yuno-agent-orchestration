package evals

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"yuno/internal/store"
)

// The fake api of runner_test.go learns workflows and approvals here. A case's plan says
// when its runs raise approvals; a raised approval keeps the run in waiting_approval
// until it is decided or expires, as the real engine does.

type fakeAPIError struct {
	status  int
	code    string
	message string
	details []map[string]string
}

type approvalPlan struct {
	raiseAt     []int         // run polls at which the next approval is raised
	expireAfter time.Duration // a pending approval expires after this long; 0 means never
	tool        string        // when set, the approvals gate this tool call rather than a node
}

type fakeApproval struct {
	store.Approval
	raisedAt time.Time
	tool     string
}

type fakeDecision struct {
	approvalID, decision, reason string
	at                           time.Time
}

func (f *fakeAPI) workflowRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/workflows", func(w http.ResponseWriter, r *http.Request) {
		var req WorkflowRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 422, "bad_request", err.Error())
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		caseID := suffixPattern.ReplaceAllString(strings.TrimPrefix(req.Name, "eval-"), "")
		if e := f.workflowErrors[caseID]; e != nil {
			body := map[string]any{"code": e.code, "message": e.message}
			if len(e.details) > 0 {
				body["details"] = e.details
			}
			writeJSON(w, e.status, map[string]any{"error": body})
			return
		}
		f.nextID++
		id := fmt.Sprintf("wf-%d", f.nextID)
		f.workflows[id] = req
		writeJSON(w, 201, store.Workflow{ID: id, Name: req.Name, Version: 1, Definition: req.Definition})
	})
	mux.HandleFunc("DELETE /api/workflows/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.deletedWorkflows = append(f.deletedWorkflows, r.PathValue("id"))
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /api/approvals", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.listErrors > 0 {
			f.listErrors--
			writeErr(w, 500, "internal", "database unavailable")
			return
		}
		out := []ApprovalItem{}
		for _, run := range f.runs {
			if run.id != r.URL.Query().Get("run_id") {
				continue
			}
			for _, a := range run.approvals {
				if s := r.URL.Query().Get("status"); s != "" && a.Status != s {
					continue
				}
				item := ApprovalItem{Approval: a.Approval}
				if a.tool != "" {
					item.ToolCall = &store.ToolCall{ID: *a.ToolCallID, ToolName: a.tool, Status: store.ToolAwaitingApproval}
				}
				out = append(out, item)
			}
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("POST /api/approvals/{id}/approve", f.decide(DecisionApprove))
	mux.HandleFunc("POST /api/approvals/{id}/reject", f.decide(DecisionReject))
}

func (f *fakeAPI) decide(decision string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body decisionRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.decideAttempts++
		var (
			a   *fakeApproval
			run *fakeRun
		)
		for _, candidate := range f.runs {
			for _, ap := range candidate.approvals {
				if ap.ID == r.PathValue("id") {
					a, run = ap, candidate
				}
			}
		}
		switch {
		case a == nil:
			writeErr(w, 404, "not_found", "no such approval")
			return
		case f.decideErrors > 0:
			f.decideErrors--
			writeErr(w, 500, "internal", "database unavailable")
			return
		case f.stickyConflicts > 0:
			f.stickyConflicts--
			writeErr(w, 409, "conflict", "approval is not pending")
			return
		case f.decideConflicts > 0:
			f.decideConflicts--
			a.Status = store.ApprovalExpired
			f.finishRun(run, store.RunFailed, "approval expired")
			writeErr(w, 409, "conflict", "approval is not pending")
			return
		case a.Status != store.ApprovalPending:
			writeErr(w, 409, "conflict", "approval is not pending")
			return
		}
		now := time.Now()
		a.DecidedAt, a.Reason = &now, &body.Reason
		f.decisions = append(f.decisions, fakeDecision{a.ID, decision, body.Reason, now})
		if decision == DecisionReject {
			a.Status = store.ApprovalRejected
			f.finishRun(run, store.RunFailed, "rejected at "+a.NodeID)
		} else {
			a.Status = store.ApprovalApproved
			run.status = store.RunRunning
		}
		writeJSON(w, 200, a.Approval)
	}
}

func (f *fakeAPI) finishRun(run *fakeRun, status, reason string) {
	now := time.Now()
	run.status, run.reason, run.finished = status, reason, &now
}

// advanceApprovals keeps a run with a pending approval waiting, or ends it when the
// approval has expired. It reports whether it handled the run.
func (f *fakeAPI) advanceApprovals(run *fakeRun) bool {
	for _, a := range run.approvals {
		if a.Status != store.ApprovalPending {
			continue
		}
		if p := f.plans[run.caseID]; p.expireAfter > 0 && time.Since(a.raisedAt) >= p.expireAfter {
			a.Status = store.ApprovalExpired
			f.finishRun(run, store.RunFailed, "approval expired")
			return true
		}
		run.status = store.RunWaitingApproval
		return true
	}
	return false
}

// raiseApproval raises the plan's next approval when the run has polled far enough.
func (f *fakeAPI) raiseApproval(run *fakeRun) bool {
	p := f.plans[run.caseID]
	if run.raised >= len(p.raiseAt) || run.polls < p.raiseAt[run.raised] {
		return false
	}
	f.approvalSeq++
	now := time.Now()
	a := &fakeApproval{raisedAt: now, tool: p.tool, Approval: store.Approval{
		ID: fmt.Sprintf("appr-%d", f.approvalSeq), RunID: run.id, Kind: store.ApprovalKindNode, NodeID: "gate",
		Status: store.ApprovalPending, RequestedAt: now.Add(time.Duration(f.approvalSeq) * time.Millisecond), ExpiresAt: now.Add(time.Hour),
	}}
	if p.tool != "" {
		a.Kind, a.NodeID = store.ApprovalKindTool, "agent"
		a.ToolCallID = ptr(fmt.Sprintf("tc-%d", f.approvalSeq))
	}
	run.approvals = append(run.approvals, a)
	run.raised++
	run.status = store.RunWaitingApproval
	return true
}

func (f *fakeAPI) decisionsSnapshot() []fakeDecision {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeDecision(nil), f.decisions...)
}

// workflowCase loads the minimal workflow case with its approval action changed.
func workflowCase(t *testing.T, actions string) Case {
	t.Helper()
	src := minimalWorkflowCase
	if actions != "" {
		src = change(t, src, "actions:\n  approvals: [ { decision: approve } ]\n", actions)
	}
	c, err := loadOne(t, src)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRunnerRunsAWorkflowCase(t *testing.T) {
	f, srv := startFake(t)
	f.plans["wf-a"] = approvalPlan{raiseAt: []int{2}}
	c := workflowCase(t, "actions:\n  approvals: [ { decision: approve, reason: go ahead } ]\n")

	res := fastRunner(testConfig(srv.URL)).runCase(context.Background(), c)
	if res.Error != "" || res.Status != store.RunSucceeded || !res.E2E {
		t.Fatalf("result = %+v", res)
	}
	if res.WorkflowID == "" || res.AgentID != "" || len(res.AgentIDs) != 1 || res.RunID == "" || res.ScriptID == "" {
		t.Errorf("ids = workflow %q agent %q agents %v", res.WorkflowID, res.AgentID, res.AgentIDs)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.agents) != 1 {
		t.Fatalf("created %d agents, want 1", len(f.agents))
	}
	for id, a := range f.agents {
		if !regexp.MustCompile(`^eval-wf-a-coder-[0-9a-f]{6}$`).MatchString(a.Name) {
			t.Errorf("agent name = %q, want eval-<case>-<key>-<suffix>", a.Name)
		}
		if a.Role != "coder" || a.SystemPrompt != "be brief" || strings.Join(a.ToolAllowlist, ",") != "write,finish" || a.Model != "claude-test" {
			t.Errorf("agent = %+v", a)
		}
		if len(f.workflows) != 1 {
			t.Fatalf("created %d workflows, want 1", len(f.workflows))
		}
		for wfID, wf := range f.workflows {
			if !regexp.MustCompile(`^eval-wf-a-[0-9a-f]{6}$`).MatchString(wf.Name) {
				t.Errorf("workflow name = %q", wf.Name)
			}
			var def struct {
				Nodes []map[string]any `json:"nodes"`
			}
			if err := json.Unmarshal(wf.Definition, &def); err != nil {
				t.Fatal(err)
			}
			if def.Nodes[1]["agent_id"] != id {
				t.Errorf("the coder node must name the created agent %s: %v", id, def.Nodes[1])
			}
			if _, has := def.Nodes[1]["agent_name"]; has {
				t.Errorf("agent_name reached the api: %v", def.Nodes[1])
			}
			run := onlyRunLocked(f)
			want := RunRequest{WorkflowID: wfID, Model: ScriptedModel, ScriptID: run.req.ScriptID, Limits: store.Limits{MaxSteps: 5, MaxLoopVisits: 2}}
			if run.req.AgentID != "" || run.req.Task != "" || run.req.WorkflowID != want.WorkflowID || run.req.Model != want.Model ||
				run.req.Limits != want.Limits || string(run.req.Input) != `{"task":"build it"}` {
				t.Errorf("run request = %+v", run.req)
			}
			if run.req.ScriptID != "wf-a-"+strings.TrimPrefix(wf.Name, "eval-wf-a-") {
				t.Errorf("script id %q does not share the suffix of %q", run.req.ScriptID, wf.Name)
			}
			if len(f.scripts[run.req.ScriptID]) != 2 {
				t.Errorf("registered %d script steps, want 2", len(f.scripts[run.req.ScriptID]))
			}
			if len(f.deletedWorkflows) != 1 || f.deletedWorkflows[0] != wfID {
				t.Errorf("deleted workflows = %v, want [%s]", f.deletedWorkflows, wfID)
			}
		}
		if len(f.deleted) != 1 || f.deleted[0] != id {
			t.Errorf("deleted agents = %v, want [%s]", f.deleted, id)
		}
	}
	if len(f.decisions) != 1 || f.decisions[0].decision != DecisionApprove || f.decisions[0].reason != "go ahead" {
		t.Errorf("decisions = %+v", f.decisions)
	}
}

func onlyRunLocked(f *fakeAPI) *fakeRun {
	for _, r := range f.runs {
		return r
	}
	return nil
}

func TestRunnerAppliesApprovalActionsInOrder(t *testing.T) {
	f, srv := startFake(t)
	f.plans["wf-a"] = approvalPlan{raiseAt: []int{1, 3}, tool: "deploy_release"}
	c := workflowCase(t, "actions:\n  approvals:\n    - { decision: approve, reason: first }\n    - { decision: reject, reason: second }\n    - { decision: approve, reason: never used }\n")

	res := fastRunner(testConfig(srv.URL)).runCase(context.Background(), c)
	if res.Error != "" || res.Status != store.RunFailed || res.FailureReason != "rejected at agent" {
		t.Fatalf("result = %+v", res)
	}
	got := f.decisionsSnapshot()
	if len(got) != 2 || got[0].decision != DecisionApprove || got[0].reason != "first" ||
		got[1].decision != DecisionReject || got[1].reason != "second" || got[0].approvalID == got[1].approvalID {
		t.Fatalf("decisions = %+v, want approve(first) then reject(second) on two approvals", got)
	}
	if res.E2E {
		t.Error("a run that failed when success was expected is not end-to-end success")
	}
}

func TestRunnerStopsWhenAnApprovalHasNoActionLeft(t *testing.T) {
	f, srv := startFake(t)
	f.plans["wf-a"] = approvalPlan{raiseAt: []int{1}, tool: "deploy_release"}
	c := workflowCase(t, "actions: {}\n")
	r := fastRunner(testConfig(srv.URL))
	r.caseTimeout = 30 * time.Second // the stall, not the deadline, must end this

	start := time.Now()
	res := r.runCase(context.Background(), c)
	if time.Since(start) > 5*time.Second {
		t.Errorf("the runner waited %s for an approval nobody will decide", time.Since(start))
	}
	for _, want := range []string{"is pending and the case has no action left for it", "tool deploy_release at node agent"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("error %q does not contain %q", res.Error, want)
		}
	}
	if res.Passed() || res.E2E || res.Status != store.RunWaitingApproval {
		t.Errorf("a stalled case fails with the run as it stood: %+v", res)
	}
	if len(res.Evaluators) != len(EvaluatorNames()) {
		t.Errorf("the trace is still evaluated: %+v", res.Evaluators)
	}
	if n := f.cancelsFor("wf-a"); n != 1 {
		t.Errorf("the abandoned run must be cancelled once, got %d", n)
	}
	if len(f.decisionsSnapshot()) != 0 {
		t.Error("nothing should have been decided")
	}
	if len(f.deletedWorkflows) != 1 || f.deletedCount() != 1 {
		t.Errorf("cleanup: workflows %v agents %d", f.deletedWorkflows, f.deletedCount())
	}
}

func TestRunnerHoldsApprovalActionsForTheWait(t *testing.T) {
	f, srv := startFake(t)
	f.plans["wf-a"] = approvalPlan{raiseAt: []int{1}}
	c := workflowCase(t, "actions:\n  approval_wait_seconds: 1\n  approvals: [ { decision: approve } ]\n")

	res := fastRunner(testConfig(srv.URL)).runCase(context.Background(), c)
	if res.Error != "" || res.Status != store.RunSucceeded {
		t.Fatalf("result = %+v", res)
	}
	got := f.decisionsSnapshot()
	if len(got) != 1 {
		t.Fatalf("decisions = %+v", got)
	}
	f.mu.Lock()
	raisedAt := onlyRunLocked(f).approvals[0].raisedAt
	f.mu.Unlock()
	if held := got[0].at.Sub(raisedAt); held < 900*time.Millisecond {
		t.Errorf("the approval was decided %s after it was raised, the case asked for a 1 s wait", held)
	}
}

func TestRunnerLetsAnApprovalExpireDuringTheWait(t *testing.T) {
	f, srv := startFake(t)
	f.plans["wf-a"] = approvalPlan{raiseAt: []int{1}, expireAfter: 150 * time.Millisecond}
	c := workflowCase(t, "actions:\n  approval_wait_seconds: 20\n")
	c.Expect = Expect{Status: store.RunFailed, FailureReasonContains: "approval expired", ApprovalStatuses: &[]string{store.ApprovalExpired}}

	start := time.Now()
	res := fastRunner(testConfig(srv.URL)).runCase(context.Background(), c)
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the runner sat out the whole wait (%s) although the run had already ended", time.Since(start))
	}
	if res.Error != "" || res.Status != store.RunFailed || res.FailureReason != "approval expired" || !res.E2E {
		t.Fatalf("result = %+v", res)
	}
	if len(f.decisionsSnapshot()) != 0 {
		t.Error("an approval left alone must not be decided")
	}
	for _, e := range res.Evaluators {
		if e.Name == EvalExpectation && !e.Passed {
			t.Errorf("expectation: %s", e.Detail)
		}
	}
	if n := f.cancelsFor("wf-a"); n != 0 {
		t.Errorf("a run that ended on its own is not cancelled, got %d", n)
	}
}

func TestRunnerRetriesAFailedDecision(t *testing.T) {
	f, srv := startFake(t)
	f.plans["wf-a"] = approvalPlan{raiseAt: []int{1}}
	f.decideErrors = 2
	res := fastRunner(testConfig(srv.URL)).runCase(context.Background(), workflowCase(t, ""))
	if res.Error != "" || res.Status != store.RunSucceeded {
		t.Fatalf("result = %+v", res)
	}
	f.mu.Lock()
	attempts := f.decideAttempts
	f.mu.Unlock()
	if attempts != 3 || len(f.decisionsSnapshot()) != 1 {
		t.Errorf("attempts = %d, decisions = %+v; want 2 failures then 1 success", attempts, f.decisionsSnapshot())
	}
}

func TestRunnerCountsAConflictAsTheActionUsed(t *testing.T) {
	f, srv := startFake(t)
	f.plans["wf-a"] = approvalPlan{raiseAt: []int{1}}
	f.decideConflicts = 1 // the approval expired just before the decision arrived
	res := fastRunner(testConfig(srv.URL)).runCase(context.Background(), workflowCase(t, ""))
	if res.Status != store.RunFailed || res.FailureReason != "approval expired" || res.Error != "" {
		t.Fatalf("result = %+v", res)
	}
	f.mu.Lock()
	attempts := f.decideAttempts
	f.mu.Unlock()
	if attempts != 1 {
		t.Errorf("a 409 must not be retried, got %d attempts", attempts)
	}
}

func TestRunnerToleratesAFailingApprovalList(t *testing.T) {
	f, srv := startFake(t)
	f.plans["wf-a"] = approvalPlan{raiseAt: []int{1}}
	f.listErrors = 3
	res := fastRunner(testConfig(srv.URL)).runCase(context.Background(), workflowCase(t, ""))
	if res.Error != "" || res.Status != store.RunSucceeded {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunnerReportsWhyTheWorkflowWasRefused(t *testing.T) {
	f, srv := startFake(t)
	f.workflowErrors["wf-a"] = &fakeAPIError{status: 422, code: "validation_failed", message: "invalid workflow",
		details: []map[string]string{{"path": "nodes[1].agent_id", "message": "no agent with that id"}, {"path": "edges[0].to", "message": "unknown node 'x'"}}}
	res := fastRunner(testConfig(srv.URL)).runCase(context.Background(), workflowCase(t, ""))
	for _, want := range []string{"create workflow", "422", "validation_failed", "invalid workflow", "nodes[1].agent_id: no agent with that id", "edges[0].to: unknown node 'x'"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("error %q does not contain %q", res.Error, want)
		}
	}
	if res.Passed() || res.RunID != "" {
		t.Errorf("no run may start: %+v", res)
	}
	if len(res.Evaluators) != len(EvaluatorNames()) || res.Evaluators[0].Passed || !strings.Contains(res.Evaluators[0].Detail, "create workflow") {
		t.Errorf("evaluators = %+v", res.Evaluators)
	}
	if f.deletedCount() != 1 || len(f.deletedWorkflows) != 0 {
		t.Errorf("the agent made before the failure is deleted and there is no workflow to delete: agents %d workflows %v", f.deletedCount(), f.deletedWorkflows)
	}
}

func TestRunnerDeletesTheAgentsAlreadyMadeWhenALaterOneFails(t *testing.T) {
	f, srv := startFake(t)
	f.failAgentMatch = "-deployer-"
	c, err := loadOne(t, twoAgentCaseYAML)
	if err != nil {
		t.Fatal(err)
	}
	res := fastRunner(testConfig(srv.URL)).runCase(context.Background(), c)
	if !strings.Contains(res.Error, "create agent deployer") {
		t.Fatalf("error = %q", res.Error)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.agents) != 1 || len(f.deleted) != 1 || len(f.workflows) != 0 || len(f.runs) != 0 {
		t.Errorf("agents %d deleted %d workflows %d runs %d", len(f.agents), len(f.deleted), len(f.workflows), len(f.runs))
	}
}

func TestRunnerCreatesEveryAgentOfAWorkflowCaseWithItsOwnRules(t *testing.T) {
	f, srv := startFake(t)
	c, err := loadOne(t, twoAgentCaseYAML)
	if err != nil {
		t.Fatal(err)
	}
	res := fastRunner(testConfig(srv.URL)).runCase(context.Background(), c)
	if res.Error != "" || len(res.AgentIDs) != 2 {
		t.Fatalf("result = %+v", res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var coder, deployer *AgentRequest
	for id := range f.agents {
		a := f.agents[id]
		switch {
		case strings.Contains(a.Name, "-coder-"):
			coder = &a
		case strings.Contains(a.Name, "-deployer-"):
			deployer = &a
		}
	}
	if coder == nil || deployer == nil {
		t.Fatalf("agents = %+v", f.agents)
	}
	if len(coder.InteractionRules) != 0 || deployer.InteractionRules["deploy_release"] != "auto" {
		t.Errorf("interaction rules must reach the api: coder %v deployer %v", coder.InteractionRules, deployer.InteractionRules)
	}
	// Both agents are in the posted definition, each node naming its own.
	var def struct {
		Nodes []map[string]any `json:"nodes"`
	}
	for _, wf := range f.workflows {
		if err := json.Unmarshal(wf.Definition, &def); err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]string{}
	for id, a := range f.agents {
		ids[a.Name[:strings.LastIndex(a.Name, "-")]] = id
	}
	if def.Nodes[1]["agent_id"] != ids["eval-wf-b-coder"] || def.Nodes[2]["agent_id"] != ids["eval-wf-b-deployer"] {
		t.Errorf("nodes %v %v, agents %v", def.Nodes[1], def.Nodes[2], ids)
	}
	if len(f.deleted) != 2 || len(f.deletedWorkflows) != 1 {
		t.Errorf("deleted agents %v workflows %v", f.deleted, f.deletedWorkflows)
	}
}

func TestRunnerCleansUpAStuckWorkflowRun(t *testing.T) {
	f, srv := startFake(t)
	f.stuck["wf-a"] = true
	r := fastRunner(testConfig(srv.URL))
	r.caseTimeout = 150 * time.Millisecond
	res := r.runCase(context.Background(), workflowCase(t, ""))
	if res.Error != "runner timeout" || res.Passed() {
		t.Fatalf("result = %+v", res)
	}
	if n := f.cancelsFor("wf-a"); n != 1 {
		t.Errorf("cancels = %d, want 1", n)
	}
	if len(f.deletedWorkflows) != 1 || f.deletedCount() != 1 {
		t.Errorf("cleanup: workflows %v agents %d", f.deletedWorkflows, f.deletedCount())
	}
}

func TestQuickCaseSendsItsInteractionRules(t *testing.T) {
	f, srv := startFake(t)
	c := quickDeployCase(map[string]string{"deploy_release": "blocked"})
	c.ID, c.Task = "q-rules", "Do it."
	c.Script = []ScriptStep{{Text: "done"}}
	res := fastRunner(testConfig(srv.URL)).runCase(context.Background(), c)
	if res.Error != "" {
		t.Fatalf("error = %s", res.Error)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.agents {
		if a.InteractionRules["deploy_release"] != "blocked" {
			t.Errorf("agent = %+v", a)
		}
	}
	if len(f.workflows) != 0 {
		t.Error("a quick run creates no workflow")
	}
	for _, r := range f.runs {
		if r.req.WorkflowID != "" || r.req.AgentID == "" || r.req.Task != "Do it." || r.req.Input != nil {
			t.Errorf("run request = %+v", r.req)
		}
	}
}

func TestClientApprovalsAndWorkflows(t *testing.T) {
	f, srv := startFake(t)
	ctx := context.Background()
	c := NewClient(srv.URL, fakeToken)

	wf, err := c.CreateWorkflow(ctx, WorkflowRequest{Name: "eval-x-abc123", Definition: json.RawMessage(`{"nodes":[]}`)})
	if err != nil || wf.ID == "" {
		t.Fatalf("CreateWorkflow = %+v, %v", wf, err)
	}
	if err := c.DeleteWorkflow(ctx, wf.ID); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	run := &fakeRun{id: "run-x", caseID: "x", status: store.RunWaitingApproval}
	run.approvals = []*fakeApproval{
		{tool: "deploy_release", Approval: store.Approval{ID: "ap-1", RunID: "run-x", Kind: store.ApprovalKindTool, NodeID: "agent", Status: store.ApprovalPending, ToolCallID: ptr("tc-1")}},
		{Approval: store.Approval{ID: "ap-2", RunID: "run-x", Kind: store.ApprovalKindNode, NodeID: "gate", Status: store.ApprovalApproved}},
	}
	f.runs[run.id] = run
	f.mu.Unlock()

	pending, err := c.ListApprovals(ctx, "run-x", store.ApprovalPending)
	if err != nil || len(pending) != 1 || pending[0].ID != "ap-1" {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	if pending[0].ToolCall == nil || pending[0].ToolCall.ToolName != "deploy_release" || pending[0].Kind != store.ApprovalKindTool {
		t.Errorf("the joined tool call must decode: %+v", pending[0])
	}
	all, err := c.ListApprovals(ctx, "run-x", "")
	if err != nil || len(all) != 2 {
		t.Fatalf("all = %+v, %v", all, err)
	}
	if other, _ := c.ListApprovals(ctx, "run-other", ""); len(other) != 0 {
		t.Errorf("approvals of another run: %+v", other)
	}

	if _, err := c.DecideApproval(ctx, "ap-1", "maybe", ""); err == nil {
		t.Error("an unknown decision is refused before any request")
	}
	if _, err := c.DecideApproval(ctx, "ap-1", DecisionReject, "no"); err != nil {
		t.Fatal(err)
	}
	if d := f.decisionsSnapshot(); len(d) != 1 || d[0].decision != DecisionReject || d[0].reason != "no" {
		t.Errorf("decisions = %+v", d)
	}
	if _, err := c.DecideApproval(ctx, "ap-1", DecisionApprove, ""); statusOf(err) != 409 {
		t.Errorf("a second decision is a 409, got %v", err)
	}
	if _, err := c.DecideApproval(ctx, "nope", DecisionApprove, ""); statusOf(err) != 404 {
		t.Errorf("an unknown approval is a 404, got %v", err)
	}
}

func TestAPIErrorShowsDetails(t *testing.T) {
	e := decodeAPIError("POST", "/api/workflows", 422, []byte(`{"error":{"code":"validation_failed","message":"invalid workflow","details":[{"path":"nodes[0].id","message":"duplicate"},{"message":"no path"}]}}`))
	want := "POST /api/workflows: 422 validation_failed: invalid workflow (nodes[0].id: duplicate; no path)"
	if e.Error() != want {
		t.Errorf("error = %q\nwant    %q", e.Error(), want)
	}
	if plain := decodeAPIError("GET", "/x", 502, []byte("<html>")); plain.Error() != "GET /x: 502 Bad Gateway" {
		t.Errorf("plain = %q", plain.Error())
	}
}

// firstTool is the tool the run's script calls first, which is what the fake traces
// record as the run's tool calls; "exec" when the script calls none.
func (f *fakeAPI) firstTool(run *fakeRun) string {
	for _, s := range f.scripts[run.req.ScriptID] {
		if s.Tool != nil {
			return toolBase(s.Tool.Name)
		}
	}
	return "exec"
}

// An action answered with 409 is used up even if the approval is still listed as
// pending: the next action belongs to the next approval, never to this one again.
func TestRunnerDoesNotDecideTheSameApprovalTwice(t *testing.T) {
	f, srv := startFake(t)
	f.plans["wf-a"] = approvalPlan{raiseAt: []int{1}}
	f.stickyConflicts = 5
	c := workflowCase(t, "actions:\n  approvals:\n    - { decision: approve }\n    - { decision: approve }\n")
	r := fastRunner(testConfig(srv.URL))
	r.caseTimeout = 400 * time.Millisecond

	res := r.runCase(context.Background(), c)
	if res.Error != "runner timeout" {
		t.Fatalf("result = %+v", res)
	}
	f.mu.Lock()
	attempts := f.decideAttempts
	f.mu.Unlock()
	if attempts != 1 {
		t.Errorf("the approval was decided %d times, want one attempt", attempts)
	}
}
