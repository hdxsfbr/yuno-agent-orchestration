package deps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/tools"
)

var epoch = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

// fakeCI answers the CI of merge commits by sha and whether the default branch has CI; it
// records the refs and commits asked about.
type fakeCI struct {
	tools.RequestProvider
	mu         sync.Mutex
	states     map[string]string
	configured bool
	err        error
	asked      []string
	configAsks int
}

func (f *fakeCI) CommitCI(_ context.Context, ref, sha string) (tools.BranchStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, ref+"@"+sha)
	if f.err != nil {
		return tools.BranchStatus{}, f.err
	}
	st := f.states[sha]
	if st == "" {
		st = tools.BranchNone
	}
	return tools.BranchStatus{State: st, SHA: sha}, nil
}

func (f *fakeCI) CIConfigured(context.Context, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configAsks++
	return f.configured, nil
}

type env struct {
	t       *testing.T
	pool    store.Querier
	project *store.Project
	ci      *fakeCI
	now     time.Time
	gate    *Gate
	dep     *store.MergeRequest
	number  int64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := needDB(t)
	e := &env{t: t, pool: pool, ci: &fakeCI{states: map[string]string{}}, now: epoch, number: 100}
	e.project = &store.Project{ID: store.NewID(), Name: "p-" + store.NewID(), RepoURL: "https://gitlab.test/g/demo.git", Provider: "gitlab", DefaultBranch: "main"}
	must(t, store.CreateProject(context.Background(), pool, e.project, []byte("sealed")))
	e.gate = &Gate{Pool: pool, Now: func() time.Time { return e.now },
		Provider: func(context.Context, *store.MergeRequest) (*project.Config, tools.RequestProvider, error) {
			return &project.Config{Provider: project.ProviderGitLab, DefaultBranch: "main"}, e.ci, nil
		}}
	e.dep = e.request(store.MergeRequestOpen, "", time.Time{})
	return e
}

// request stores a record of its own run in state; a merged one merged at mergedAt with
// mergeSHA (none when "").
func (e *env) request(state, mergeSHA string, mergedAt time.Time) *store.MergeRequest {
	e.t.Helper()
	ctx := context.Background()
	wf := &store.Workflow{Name: "wf " + store.NewID(), Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	must(e.t, store.CreateWorkflow(ctx, e.pool, wf))
	settings := e.project.Settings()
	run := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: store.TriggerManual,
		ProjectID: &e.project.ID, Project: &settings}
	must(e.t, store.CreateRun(ctx, e.pool, run))
	e.number++
	mr := &store.MergeRequest{ProjectID: e.project.ID, RunID: run.ID, Provider: "gitlab", Number: e.number, URL: "u",
		SourceBranch: "b", TargetBranch: "main", Title: "t"}
	ok, err := store.InsertMergeRequest(ctx, e.pool, mr)
	must(e.t, err)
	if !ok {
		e.t.Fatal("record not inserted")
	}
	switch state {
	case store.MergeRequestMerged:
		var sha *string
		if mergeSHA != "" {
			sha = &mergeSHA
		}
		_, err = store.MarkMergeRequestMergedAs(ctx, e.pool, mr.ID, "sync", mergedAt, &mergedAt, sha)
	case store.MergeRequestClosed:
		_, err = e.pool.Exec(ctx, `UPDATE merge_requests SET state = 'closed', closed_at = $2 WHERE id = $1`, mr.ID, mergedAt)
	}
	must(e.t, err)
	got, err := store.GetMergeRequest(ctx, e.pool, mr.ID)
	must(e.t, err)
	return got
}

// edge makes the dependent wait on the prerequisite and returns the edge.
func (e *env) edge(kind, id string) store.Dependency {
	e.t.Helper()
	d := &store.Dependency{ProjectID: e.project.ID, DependentKind: store.DependencyMergeRequest, DependentID: e.dep.ID,
		PrerequisiteKind: kind, PrerequisiteID: id, Source: store.DependencyOperator}
	ok, err := store.AddDependency(context.Background(), e.pool, d)
	must(e.t, err)
	if !ok {
		e.t.Fatal("edge not added")
	}
	list, err := store.DependenciesOf(context.Background(), e.pool, store.DependencyMergeRequest, e.dep.ID)
	must(e.t, err)
	for _, x := range list {
		if x.ID == d.ID {
			return x
		}
	}
	e.t.Fatal("edge not found")
	return store.Dependency{}
}

// judge judges one edge alone and returns the state.
func (e *env) judge(d store.Dependency) State {
	e.t.Helper()
	st, err := e.gate.Judge(context.Background(), []store.Dependency{d})
	must(e.t, err)
	return st
}

const mergeSHA = "1111111111111111111111111111111111111111"

// DESIGN 5.34 rules 4 and 5: a merge request prerequisite is satisfied when merged, its
// merge commit known, and the default branch's CI of exactly that commit passed; each
// clause on both sides, the reasons in rule 5's words, the 15-minute no-CI window at 14
// and 16 minutes, a ticket prerequisite through its merge request, and a satisfied edge
// that stays satisfied.
func TestPrerequisiteSatisfied(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	blocked := func(name string, st State, reason string) {
		t.Helper()
		if st.Satisfied || len(st.Blocked) != 1 || st.Blocked[0].Reason != reason {
			t.Fatalf("%s: %+v, want blocked %q", name, st, reason)
		}
	}

	// Not merged: open, then closed without merging (the operator is told of that one).
	open := e.request(store.MergeRequestOpen, "", time.Time{})
	blocked("open", e.judge(e.edge(store.DependencyMergeRequest, open.ID)), "!102 is not merged")
	closed := e.request(store.MergeRequestClosed, "", epoch.Add(-time.Hour))
	st := e.judge(e.edge(store.DependencyMergeRequest, closed.ID))
	blocked("closed", st, "prerequisite !103 was closed without merging")
	if len(st.Closed) != 1 {
		t.Fatalf("closed is not reported as closed: %+v", st)
	}

	// Merged without a known merge commit: no provider is asked.
	noSHA := e.request(store.MergeRequestMerged, "", epoch.Add(-time.Hour))
	blocked("no merge commit", e.judge(e.edge(store.DependencyMergeRequest, noSHA.ID)), "!104 merged; the merge commit is not known yet")
	if len(e.ci.asked) != 0 {
		t.Fatalf("a provider was asked: %v", e.ci.asked)
	}

	// Merged with its commit: the default branch's CI of exactly that commit decides.
	merged := e.request(store.MergeRequestMerged, mergeSHA, epoch.Add(-time.Hour))
	edge := e.edge(store.DependencyMergeRequest, merged.ID)
	for _, state := range []string{tools.BranchFailed, tools.BranchRunning, tools.BranchPending} {
		e.ci.states[mergeSHA] = state
		blocked(state, e.judge(edge), "!105 merged; CI on 1111111 is "+state)
	}
	if e.ci.asked[0] != "main@"+mergeSHA {
		t.Fatalf("asked %v, want the default branch at the merge commit", e.ci.asked)
	}
	e.ci.states[mergeSHA] = tools.BranchPassed
	if st := e.judge(edge); !st.Satisfied || len(st.Blocked) != 0 {
		t.Fatalf("passed: %+v", st)
	}
	stored, err := store.DependenciesOf(ctx, e.pool, store.DependencyMergeRequest, e.dep.ID)
	must(t, err)
	for _, d := range stored {
		if d.ID == edge.ID && (d.SatisfiedAt == nil || !d.SatisfiedAt.Equal(epoch) || d.Reason != nil) {
			t.Fatalf("satisfied edge = %+v", d)
		}
		if d.ID == edge.ID {
			edge = d
		}
	}
	// Satisfied stays satisfied: a later failure of the same commit is not asked about.
	e.ci.states[mergeSHA] = tools.BranchFailed
	asked := len(e.ci.asked)
	if st := e.judge(edge); !st.Satisfied || len(e.ci.asked) != asked {
		t.Fatalf("a satisfied prerequisite was judged again: %+v (%d asks)", st, len(e.ci.asked)-asked)
	}

	// No pipeline on the merge commit: pending inside the window, then the project's CI decides.
	const quiet = "2222222222222222222222222222222222222222"
	noCI := e.request(store.MergeRequestMerged, quiet, epoch)
	noCIEdge := e.edge(store.DependencyMergeRequest, noCI.ID)
	e.now = epoch.Add(14 * time.Minute)
	blocked("no CI at 14 minutes", e.judge(noCIEdge), "!106 merged; CI on 2222222 is pending")
	if e.ci.configAsks != 0 {
		t.Fatalf("CI configuration asked inside the window")
	}
	e.now = epoch.Add(16 * time.Minute)
	e.ci.configured = true
	blocked("CI configured at 16 minutes", e.judge(noCIEdge), "!106 merged; CI on 2222222 is pending")
	e.ci.configured = false
	st = e.judge(noCIEdge)
	if !st.Satisfied {
		t.Fatalf("no CI at 16 minutes: %+v", st)
	}
	stored, err = store.DependenciesOf(ctx, e.pool, store.DependencyMergeRequest, e.dep.ID)
	must(t, err)
	for _, d := range stored {
		if d.ID == noCIEdge.ID && (d.SatisfiedAt == nil || d.Reason == nil || *d.Reason != NoteNoCI) {
			t.Fatalf("no-CI edge = %+v", d)
		}
	}

	// A provider that fails: an error, the edge left unjudged.
	failing := e.request(store.MergeRequestMerged, mergeSHA, epoch)
	e.ci.err = errors.New("gitlab: HTTP 502")
	var pe *ProviderError
	if _, err := e.gate.Judge(ctx, []store.Dependency{e.edge(store.DependencyMergeRequest, failing.ID)}); !errors.As(err, &pe) {
		t.Fatalf("provider failure = %v", err)
	}
	e.ci.err = nil

	// Tickets: satisfied when their merge request is; never when closed without merging.
	run := e.dep.RunID
	plan := &store.Plan{ProjectID: e.project.ID, Feature: "search", PlanRunID: run, Status: store.PlanBuilding}
	must(t, store.CreatePlan(ctx, e.pool, plan))
	for _, tk := range []struct {
		id, status string
		mr         *store.MergeRequest
	}{{"T1", store.TicketMerged, merged}, {"T2", store.TicketClosed, nil}, {"T3", store.TicketReleased, nil}, {"T4", store.TicketInReview, open}} {
		must(t, store.UpsertPlanTicket(ctx, e.pool, &store.PlanTicket{PlanID: plan.ID, TicketID: tk.id, Wave: 1, Title: tk.id, Status: tk.status}))
		if tk.mr != nil {
			_, err := e.pool.Exec(ctx, `UPDATE plan_tickets SET merge_request_id = $3 WHERE plan_id = $1 AND ticket_id = $2`, plan.ID, tk.id, tk.mr.ID)
			must(t, err)
		}
	}
	e.ci.states[mergeSHA] = tools.BranchPassed
	if st := e.judge(e.edge(store.DependencyTicket, store.TicketNode(plan.ID, "T1"))); !st.Satisfied {
		t.Fatalf("ticket with a merged, green request: %+v", st)
	}
	st = e.judge(e.edge(store.DependencyTicket, store.TicketNode(plan.ID, "T2")))
	blocked("closed ticket", st, "prerequisite T2 was closed without merging")
	if len(st.Closed) != 1 {
		t.Fatalf("a closed ticket is not reported closed: %+v", st)
	}
	blocked("released ticket", e.judge(e.edge(store.DependencyTicket, store.TicketNode(plan.ID, "T3"))), "T3 is not merged")
	blocked("ticket in review", e.judge(e.edge(store.DependencyTicket, store.TicketNode(plan.ID, "T4"))), "!102 is not merged")

	// The whole merge request: its first blocker is the reason rule 7 will park a run with.
	ok, reason, err := e.gate.PrerequisitesSatisfied(ctx, e.dep.ID)
	if err != nil || ok || !strings.HasPrefix(reason, "!102 is not merged") {
		t.Fatalf("PrerequisitesSatisfied = %v %q %v", ok, reason, err)
	}
	// The stored state reads the same without asking anyone.
	edges, err := store.MergeRequestDependencies(ctx, e.pool, e.dep.ID)
	must(t, err)
	asked = len(e.ci.asked)
	sst, err := Stored(ctx, e.pool, edges)
	must(t, err)
	if sst.Satisfied || sst.Reason() != "!102 is not merged" || len(e.ci.asked) != asked {
		t.Fatalf("stored = %+v", sst)
	}
}
