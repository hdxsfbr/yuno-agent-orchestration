package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"yuno/internal/openclaw"
	"yuno/internal/project"
	"yuno/internal/project/projecttest"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// Wave 9 agent A1: a run that iterates on an open merge request, the engine side (DESIGN
// 5.22 rules 1, 2, 3 and 5, and the retry of 5.21 rule 3 amended). The remote is a local
// bare repository, as in project_test.go; the tests are named after the rule they pin.

// iterBranch is the source branch of the request the tests iterate on.
const iterBranch = "yuno/abc123def456"

// pushFiles pushes a commit with files onto branch of the bare repository, creating the
// branch from the default branch when it does not exist yet.
func pushFiles(t *testing.T, bare, branch string, files map[string]string) {
	t.Helper()
	base := t.TempDir()
	projecttest.Git(t, base, "clone", "--quiet", bare, "work")
	work := filepath.Join(base, "work")
	if !slices.Contains(projecttest.Branches(t, bare), branch) {
		projecttest.Git(t, work, "checkout", "--quiet", "-b", branch)
	} else {
		projecttest.Git(t, work, "checkout", "--quiet", branch)
	}
	for name, content := range files {
		writeFile(t, filepath.Join(work, name), content)
	}
	projecttest.Git(t, work, "add", "--all")
	projecttest.Git(t, work, "commit", "--quiet", "-m", "work on "+branch)
	projecttest.Git(t, work, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
}

// iterFixture is a project whose remote has the request's branch (feature.txt on top of
// main) and a later commit on main (later.txt), the record of the request, the run that
// opened it (ended) and a one-node project workflow.
type iterFixture struct {
	env    *testEnv
	bare   string
	proj   *store.Project
	opener *store.Run
	rec    *store.MergeRequest
	agent  *store.Agent
	wf     *store.Workflow
}

func newIterFixture(t *testing.T) *iterFixture {
	t.Helper()
	env, bare, proj := projectEnv(t, map[string]string{"README.md": "# demo\n", "go.mod": "module demo\n"}, true)
	pushFiles(t, bare, iterBranch, map[string]string{"feature.txt": "the branch's work\n"})
	pushFiles(t, bare, "main", map[string]string{"later.txt": "main moved on\n"})
	f := &iterFixture{env: env, bare: bare, proj: proj, agent: env.agent(nil)}
	f.opener = env.endedRun(store.RunSucceeded, "", "")
	f.rec = f.record(proj, f.opener, 4, "gitlab")
	f.wf = projectWorkflow(env, f.agent)
	return f
}

// record stores an open merge request record of project p opened by run, on iterBranch.
func (f *iterFixture) record(p *store.Project, run *store.Run, number int64, provider string) *store.MergeRequest {
	f.env.t.Helper()
	url := "https://gitlab.test/g/p/-/merge_requests/4"
	if provider == "github" {
		url = "https://github.test/g/p/pull/4"
	}
	mr := &store.MergeRequest{ProjectID: p.ID, RunID: run.ID, Provider: provider, Number: number, URL: url,
		SourceBranch: iterBranch, TargetBranch: "main", Title: "Add the feature"}
	if ok, err := store.InsertMergeRequest(context.Background(), f.env.pool, mr); err != nil || !ok {
		f.env.t.Fatalf("insert record: %v %v", ok, err)
	}
	return mr
}

// request is the trigger of a run of the project workflow that names the record.
func (f *iterFixture) request() TriggerRequest {
	return TriggerRequest{WorkflowID: f.wf.ID, ProjectID: f.proj.ID, MergeRequestID: f.rec.ID}
}

func (f *iterFixture) setState(state string) {
	f.env.t.Helper()
	if _, err := f.env.pool.Exec(context.Background(), `UPDATE merge_requests SET state=$2 WHERE id=$1`, f.rec.ID, state); err != nil {
		f.env.t.Fatal(err)
	}
}

func (f *iterFixture) setRunStatus(id, status string) {
	f.env.t.Helper()
	if _, err := f.env.pool.Exec(context.Background(), `UPDATE runs SET status=$2 WHERE id=$1`, id, status); err != nil {
		f.env.t.Fatal(err)
	}
}

// liveNaming stores a queued run that names the record, as another request would have.
func (f *iterFixture) liveNaming() *store.Run {
	f.env.t.Helper()
	snap, _ := json.Marshal(runSnapshot{Definition: workflow.Definition{}})
	run := &store.Run{WorkflowID: f.wf.ID, WorkflowVersion: 1, WorkflowSnapshot: snap, Trigger: store.TriggerManual,
		Limits: store.DefaultLimits, ProjectID: &f.proj.ID, MergeRequestID: &f.rec.ID}
	if err := store.CreateRun(context.Background(), f.env.pool, run); err != nil {
		f.env.t.Fatalf("live run: %v", err)
	}
	return run
}

// refusal is the messages of the *ValidationError err carries, failing when it is none.
func refusal(t *testing.T, err error) []string {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a *ValidationError", err)
	}
	return ve.Messages
}

func countRuns(t *testing.T, env *testEnv) int {
	t.Helper()
	return env.count(`SELECT count(*) FROM runs`)
}

// T5.22.1, DESIGN 5.22 rule 1: each text of the queue-time checks, in order, and the
// happy path writing runs.merge_request_id. Each refusal queues nothing.
func TestIterationRule1QueueTimeChecks(t *testing.T) {
	f := newIterFixture(t)
	env, ctx := f.env, context.Background()
	other := env.project(f.bare, false)
	otherOpener := env.endedRun(store.RunSucceeded, "", "")
	otherRec := f.record(other, otherOpener, 7, "gitlab")
	plain := env.createWorkflow(line(workflow.Node{ID: "one", AgentID: f.agent.ID, Task: "write"}))
	gone := env.project(f.bare, false)
	goneOpener := env.endedRun(store.RunSucceeded, "", "")
	goneRec := f.record(gone, goneOpener, 9, "github")
	if err := store.DeleteProject(ctx, env.pool, gone.ID); err != nil {
		t.Fatal(err)
	}
	before := countRuns(t, env)

	trigger := func(req TriggerRequest) []string {
		t.Helper()
		_, err := env.e.Trigger(ctx, req)
		return refusal(t, err)
	}
	want := func(what string, got []string, msgs ...string) {
		t.Helper()
		if !slices.Equal(got, msgs) {
			t.Errorf("%s: messages = %q, want %q", what, got, msgs)
		}
	}

	// 1. The request names no project: a workflow without the flag, a quick run without one.
	want("workflow without a project", trigger(TriggerRequest{WorkflowID: plain.ID, MergeRequestID: f.rec.ID}),
		"merge_request_id: the workflow does not use a project")
	want("quick run without a project", trigger(TriggerRequest{AgentID: f.agent.ID, Task: "t", MergeRequestID: f.rec.ID}),
		"merge_request_id: the workflow does not use a project")
	// A project check that failed is the only complaint: the request is not checked on top.
	want("unknown project", trigger(TriggerRequest{WorkflowID: f.wf.ID, ProjectID: "nope", MergeRequestID: f.rec.ID}),
		"project_id: no project with id nope")

	// 2. No record with that id.
	req := f.request()
	req.MergeRequestID = "nope"
	want("unknown id", trigger(req), "merge_request_id: no merge request with id nope")

	// 3. The record is another project's, deleted or not; its name is read either way.
	req.MergeRequestID = otherRec.ID
	want("another project", trigger(req), "merge_request_id: merge request !7 belongs to project "+other.Name)
	req.MergeRequestID = goneRec.ID
	want("a deleted project's record", trigger(req), "merge_request_id: pull request #9 belongs to project "+gone.Name)
	// ... and that comes before the state: a closed record of another project.
	if _, err := env.pool.Exec(ctx, `UPDATE merge_requests SET state='closed' WHERE id=$1`, otherRec.ID); err != nil {
		t.Fatal(err)
	}
	req.MergeRequestID = otherRec.ID
	want("another project's closed record", trigger(req), "merge_request_id: merge request !7 belongs to project "+other.Name)

	// 4. The record is not open, whichever the provider calls it.
	for _, state := range []string{store.MergeRequestMerged, store.MergeRequestClosed} {
		f.setState(state)
		want(state, trigger(f.request()), "merge_request_id: merge request !4 is "+state)
	}
	// ... and that comes before the live run of the opener.
	f.setRunStatus(f.opener.ID, store.RunRunning)
	want("closed with the opener live", trigger(f.request()), "merge_request_id: merge request !4 is closed")
	f.setState(store.MergeRequestOpen)

	// 5. The run that opened the record is live, in each live status; once it ends the
	// request is free.
	for _, status := range []string{store.RunQueued, store.RunRunning, store.RunWaitingApproval} {
		f.setRunStatus(f.opener.ID, status)
		want("opener "+status, trigger(f.request()), "merge_request_id: merge request !4 is being updated by run "+f.opener.ID)
		want("quick run, opener "+status, trigger(TriggerRequest{AgentID: f.agent.ID, Task: "t", ProjectID: f.proj.ID, MergeRequestID: f.rec.ID}),
			"merge_request_id: merge request !4 is being updated by run "+f.opener.ID)
	}
	f.setRunStatus(f.opener.ID, store.RunSucceeded)

	// 6. Another live run names the record: the partial unique index refuses the insert and
	// the engine says which run holds the request.
	live := f.liveNaming()
	want("named by a live run", trigger(f.request()), "merge_request_id: merge request !4 is being updated by run "+live.ID)
	want("quick run, named by a live run", trigger(TriggerRequest{AgentID: f.agent.ID, Task: "t", ProjectID: f.proj.ID, MergeRequestID: f.rec.ID}),
		"merge_request_id: merge request !4 is being updated by run "+live.ID)
	if got := countRuns(t, env); got != before+1 { // the live run stored above, nothing else
		t.Errorf("runs after the refusals = %d, want %d: a refused request queued something", got, before+1)
	}
	f.setRunStatus(live.ID, store.RunFailed)

	// 7. The happy path writes the record's id on the run, trimmed, for a workflow and a
	// quick run alike; the run reads back with it.
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: f.wf.ID, ProjectID: f.proj.ID, MergeRequestID: " " + f.rec.ID + " "})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.GetRun(ctx, env.pool, run.ID); err != nil || got.MergeRequestID == nil || *got.MergeRequestID != f.rec.ID ||
		got.Status != store.RunQueued || got.ProjectID == nil || *got.ProjectID != f.proj.ID {
		t.Fatalf("queued run = %+v %v", got, err)
	}
	// The queued run is live now: a quick run on the same request is refused with its id.
	want("second request on the same record", trigger(TriggerRequest{AgentID: f.agent.ID, Task: "t", ProjectID: f.proj.ID, MergeRequestID: f.rec.ID}),
		"merge_request_id: merge request !4 is being updated by run "+run.ID)
	f.setRunStatus(run.ID, store.RunSucceeded)
	quick, err := env.e.Trigger(ctx, TriggerRequest{AgentID: f.agent.ID, Task: "t", ProjectID: f.proj.ID, MergeRequestID: f.rec.ID})
	if err != nil || quick.MergeRequestID == nil || *quick.MergeRequestID != f.rec.ID {
		t.Fatalf("quick run = %+v %v", quick, err)
	}
	// A run that names none is untouched by any of this.
	none, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: f.wf.ID, ProjectID: f.proj.ID})
	if err != nil || none.MergeRequestID != nil {
		t.Fatalf("run without a request = %+v %v", none, err)
	}
}

// DESIGN 5.22 rule 1, rule 7: the final-state refusal is typed so the scheduler can match
// it without reading the text; a busy refusal, an unknown id and a closed request with
// another fault are plain validation errors.
func TestIterationRule1FinalStateIsTyped(t *testing.T) {
	f := newIterFixture(t)
	env, ctx := f.env, context.Background()
	f.setState(store.MergeRequestClosed)
	_, err := env.e.Trigger(ctx, f.request())
	var final *MergeRequestStateError
	var ve *ValidationError
	if !errors.As(err, &final) || final.Label != "merge request !4" || final.State != "closed" ||
		!slices.Equal(final.Messages, []string{"merge_request_id: merge request !4 is closed"}) {
		t.Fatalf("closed: %v (%+v)", err, final)
	}
	if !errors.As(err, &ve) || ve != final.ValidationError {
		t.Fatalf("the typed refusal does not unwrap to its *ValidationError: %v", err)
	}
	f.setState(store.MergeRequestMerged)
	if _, err := env.e.Trigger(ctx, f.request()); !errors.As(err, &final) || final.State != "merged" {
		t.Fatalf("merged: %v", err)
	}

	// Another fault next to the state: a workflow whose input schema wants a field.
	def := line(workflow.Node{ID: "one", AgentID: f.agent.ID, Task: "write"})
	def.Project = true
	def.InputSchema = json.RawMessage(`{"type":"object","required":["task"]}`)
	strict := env.createWorkflow(def)
	_, err = env.e.Trigger(ctx, TriggerRequest{WorkflowID: strict.ID, ProjectID: f.proj.ID, MergeRequestID: f.rec.ID})
	if errors.As(err, &final) || !errors.As(err, &ve) || len(ve.Messages) != 2 {
		t.Fatalf("closed with a bad input: %v", err)
	}

	f.setState(store.MergeRequestOpen)
	f.liveNaming()
	if _, err = env.e.Trigger(ctx, f.request()); errors.As(err, &final) || !errors.As(err, &ve) {
		t.Fatalf("busy: %v", err)
	}
	req := f.request()
	req.MergeRequestID = "nope"
	if _, err = env.e.Trigger(ctx, req); errors.As(err, &final) || !errors.As(err, &ve) {
		t.Fatalf("unknown: %v", err)
	}
}

// DESIGN 5.22 rule 1: the exported check the schedule handlers reuse answers the record
// only when every check passed, and nothing for an empty id.
func TestIterationRule1MergeRequestForRequest(t *testing.T) {
	f := newIterFixture(t)
	ctx := context.Background()
	if mr, msgs, err := MergeRequestForRequest(ctx, f.env.pool, "  ", f.proj); mr != nil || msgs != nil || err != nil {
		t.Fatalf("empty id: %v %v %v", mr, msgs, err)
	}
	mr, msgs, err := MergeRequestForRequest(ctx, f.env.pool, f.rec.ID, f.proj)
	if err != nil || msgs != nil || mr == nil || mr.ID != f.rec.ID {
		t.Fatalf("open record: %+v %v %v", mr, msgs, err)
	}
	if mr, msgs, _ = MergeRequestForRequest(ctx, f.env.pool, f.rec.ID, nil); mr != nil ||
		!slices.Equal(msgs, []string{"merge_request_id: the workflow does not use a project"}) {
		t.Fatalf("no project: %+v %q", mr, msgs)
	}
	f.setState(store.MergeRequestMerged)
	if mr, msgs, _ = MergeRequestForRequest(ctx, f.env.pool, f.rec.ID, f.proj); mr != nil ||
		!slices.Equal(msgs, []string{"merge_request_id: merge request !4 is merged"}) {
		t.Fatalf("merged: %+v %q", mr, msgs)
	}
}

// repoFile reads a file of the run's checkout in the workspace of the agent whose turn is
// starting; a file that is not there reads as "".
func repoFile(env *testEnv, p openclaw.ChatSendParams, runID, rel string) string {
	b, err := os.ReadFile(filepath.Join(env.dir, "workspace-"+sessionAgent(p.SessionKey), "runs", runID, "repo", rel))
	if err != nil {
		return ""
	}
	return string(b)
}

// cloneSeen is what a turn found in the checkout when it started.
type cloneSeen struct{ feature, later, readme string }

// runSeeing runs the queued run to its end, recording the checkout at the first turn.
func runSeeing(t *testing.T, env *testEnv, runID string) (*store.Run, cloneSeen) {
	t.Helper()
	var mu sync.Mutex
	var seen cloneSeen
	first := true
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		mu.Lock()
		if first {
			first = false
			seen = cloneSeen{feature: repoFile(env, p, runID, "feature.txt"), later: repoFile(env, p, runID, "later.txt"),
				readme: repoFile(env, p, runID, "README.md")}
		}
		mu.Unlock()
		go finishThenFinal(t, env, p, `{"summary":"done"}`)
		return startedTurn(p)
	}
	r := env.resume(env.e, runID)
	mu.Lock()
	defer mu.Unlock()
	return r, seen
}

// cloneSpanAttrs are the attributes of the run's project_clone span.
func cloneSpanAttrs(t *testing.T, env *testEnv, runID string) map[string]any {
	t.Helper()
	var raw json.RawMessage
	if err := env.pool.QueryRow(context.Background(), `SELECT attrs FROM spans WHERE run_id=$1 AND name='project_clone'`, runID).Scan(&raw); err != nil {
		t.Fatalf("project_clone span: %v", err)
	}
	var attrs map[string]any
	if err := json.Unmarshal(raw, &attrs); err != nil {
		t.Fatal(err)
	}
	return attrs
}

// T5.22.2, DESIGN 5.22 rule 2: a run that names a request clones the request's source
// branch (its file is there, the default branch's later commit is not), the span and the
// log line name the branch and the number, and checkout_sha is the branch head. A run that
// names none keeps the default branch and records its head too (every project run).
func TestIterationRule2CloneChecksOutTheRequestBranch(t *testing.T) {
	f := newIterFixture(t)
	env, ctx := f.env, context.Background()
	branchHead := projecttest.Git(t, f.bare, "rev-parse", iterBranch)
	mainHead := projecttest.Git(t, f.bare, "rev-parse", "main")
	if branchHead == mainHead {
		t.Fatal("fixture: the branch and main are at one commit")
	}

	run, err := env.e.Trigger(ctx, f.request())
	if err != nil {
		t.Fatal(err)
	}
	r, seen := runSeeing(t, env, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if seen.feature != "the branch's work\n" || seen.later != "" || seen.readme != "# demo\n" {
		t.Fatalf("the clone at the first turn: %+v, want the branch's file and not main's later commit", seen)
	}
	if r.CheckoutSHA == nil || *r.CheckoutSHA != branchHead {
		t.Fatalf("checkout_sha = %v, want the branch head %s", r.CheckoutSHA, branchHead)
	}
	if r.MergeRequestID == nil || *r.MergeRequestID != f.rec.ID || r.Toolchain == nil || *r.Toolchain != "go" {
		t.Fatalf("run = %v toolchain %v", r.MergeRequestID, r.Toolchain)
	}
	attrs := cloneSpanAttrs(t, env, run.ID)
	if attrs["branch"] != iterBranch || attrs["merge_request"] != float64(4) || attrs["repo"] != f.bare || attrs["toolchain"] != "go" {
		t.Fatalf("project_clone attrs = %v", attrs)
	}
	if n := env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='log' AND data::text LIKE $2`, run.ID,
		"%project repository cloned: "+f.bare+" ("+iterBranch+"), toolchain go%"); n != 1 {
		t.Fatalf("clone log lines naming the branch = %d", n)
	}

	// A run that names no request: the default branch, its head, no request on the span.
	plain, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: f.wf.ID, ProjectID: f.proj.ID})
	if err != nil {
		t.Fatal(err)
	}
	r, seen = runSeeing(t, env, plain.ID)
	if r.Status != store.RunSucceeded || seen.feature != "" || seen.later != "main moved on\n" {
		t.Fatalf("plain run = %s %q, clone %+v", r.Status, deref(r.FailureReason), seen)
	}
	if r.CheckoutSHA == nil || *r.CheckoutSHA != mainHead {
		t.Fatalf("plain run checkout_sha = %v, want main's head %s", r.CheckoutSHA, mainHead)
	}
	attrs = cloneSpanAttrs(t, env, plain.ID)
	if _, has := attrs["merge_request"]; has || attrs["branch"] != "main" {
		t.Fatalf("plain run project_clone attrs = %v", attrs)
	}
	if n := env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='log' AND data::text LIKE $2`, plain.ID,
		"%project repository cloned: "+f.bare+" (main), toolchain go%"); n != 1 {
		t.Fatalf("plain clone log lines = %d", n)
	}
}

// T5.22.2, DESIGN 5.22 rule 2: a branch the provider no longer has fails the run with
// "project clone:" before any turn, the record untouched, nothing recorded for the checkout.
func TestIterationRule2DeletedBranchFailsTheClone(t *testing.T) {
	f := newIterFixture(t)
	env, ctx := f.env, context.Background()
	run, err := env.e.Trigger(ctx, f.request())
	if err != nil {
		t.Fatal(err)
	}
	projecttest.Git(t, f.bare, "branch", "-D", iterBranch)
	r, _ := runSeeing(t, env, run.ID)
	if r.Status != store.RunFailed || !strings.HasPrefix(deref(r.FailureReason), "project clone: ") {
		t.Fatalf("run = %s %q, want a failure beginning with \"project clone:\"", r.Status, deref(r.FailureReason))
	}
	if r.Toolchain != nil || r.CheckoutSHA != nil {
		t.Fatalf("a failed clone recorded toolchain %v and checkout_sha %v", r.Toolchain, r.CheckoutSHA)
	}
	if n := len(env.fc.sendParams()); n != 0 {
		t.Fatalf("%d turns started", n)
	}
	if got, err := store.GetMergeRequest(ctx, env.pool, f.rec.ID); err != nil || got.State != store.MergeRequestOpen {
		t.Fatalf("record after the failed clone = %+v %v", got, err)
	}
}

// DESIGN 5.22 rule 2: the record is read when the visit prepares the checkout; one that is
// gone by then fails the visit with the text of the rule.
func TestIterationRule2RecordGoneFailsTheVisit(t *testing.T) {
	f := newIterFixture(t)
	missing := "no-such-record"
	v := &visit{e: f.env.e, ocID: "agx", project: &project.Config{DefaultBranch: "main"},
		run: &store.Run{ID: "run-x", MergeRequestID: &missing}}
	res := v.prepareProject(context.Background())
	if res == nil || res.runStatus != store.RunFailed || res.reason != "project clone: merge request no-such-record no longer exists" {
		t.Fatalf("outcome = %+v", res)
	}
}

// T5.22.3, DESIGN 5.22 rule 5 and 5.16 rule 9: a visit of a run that names a request gets
// the Merge request paragraph after the repository line, whose branch is the source
// branch, and before the sandbox network line; a run that names none gets neither.
func TestIterationRule5ProtocolParagraph(t *testing.T) {
	f := newIterFixture(t)
	env := f.env
	const gitlabParagraph = "Merge request: this run updates merge request !4 (https://gitlab.test/g/p/-/merge_requests/4), " +
		"whose branch " + iterBranch + " is the checkout. Read it first with read_merge_request when you have that tool and " +
		"address its unresolved threads; the platform pushes your working tree onto that branch as a new commit when " +
		"open_merge_request is called.\n"
	check := func(what, msg, paragraph, branch string) {
		t.Helper()
		repoLine := "Project repository: runs/"
		i, j, k := strings.Index(msg, repoLine), strings.Index(msg, "Merge request: "), strings.Index(msg, "Sandbox network: on.")
		if paragraph == "" {
			if j >= 0 {
				t.Errorf("%s: the task has a merge request paragraph:\n%s", what, msg)
			}
		} else if !strings.Contains(msg, "\n"+paragraph) || !(i >= 0 && i < j && j < k) {
			t.Errorf("%s: paragraph missing or out of order (repository line %d, paragraph %d, network line %d):\n%s", what, i, j, k, msg)
		}
		if want := "(branch " + branch + ", toolchain go). Work inside it."; !strings.Contains(msg, want) {
			t.Errorf("%s: the repository line lacks %q:\n%s", what, want, msg)
		}
		if strings.Count(msg, "Merge request: ") > 1 {
			t.Errorf("%s: more than one paragraph:\n%s", what, msg)
		}
	}

	msg, r := visitMessage(t, env, f.request(), nil)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	check("named request", msg, gitlabParagraph, iterBranch)
	// The sandbox lines stay as they were: the paragraph sits between the two.
	if !strings.Contains(msg, "\nSandbox network: on. Install the dependencies from the lockfile first (go mod download), then run the project's tests (go test ./...).\n") {
		t.Errorf("named request: the sandbox network line changed:\n%s", msg)
	}

	msg, r = visitMessage(t, env, TriggerRequest{WorkflowID: f.wf.ID, ProjectID: f.proj.ID}, nil)
	if r.Status != store.RunSucceeded {
		t.Fatalf("plain run = %s %q", r.Status, deref(r.FailureReason))
	}
	check("no request", msg, "", "main")

	// GitHub names its requests pull requests, with a #.
	if _, err := env.pool.Exec(context.Background(), `UPDATE merge_requests SET provider='github', url='https://github.test/g/p/pull/4' WHERE id=$1`, f.rec.ID); err != nil {
		t.Fatal(err)
	}
	msg, _ = visitMessage(t, env, f.request(), nil)
	check("github request", msg, "Merge request: this run updates pull request #4 (https://github.test/g/p/pull/4), whose branch "+iterBranch+
		" is the checkout. Read it first with read_merge_request when you have that tool and address its unresolved threads; "+
		"the platform pushes your working tree onto that branch as a new commit when open_merge_request is called.\n", iterBranch)
}

// DESIGN 5.22 rule 5: every visit of the run is told, not only the one that cloned: the
// second node of a workflow reads the record again and its task carries the paragraph.
func TestIterationRule5EveryVisitIsTold(t *testing.T) {
	f := newIterFixture(t)
	env := f.env
	second := env.agent(func(a *store.Agent) { a.Name = "Reviewer" })
	def := line(workflow.Node{ID: "one", AgentID: f.agent.ID, Task: "write"}, workflow.Node{ID: "two", AgentID: second.ID, Task: "review"})
	def.Project = true
	wf := env.createWorkflow(def)
	sc := env.script(map[string][]string{"one": {`{"ok":true}`}, "two": {`{"ok":true}`}})
	run := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, ProjectID: f.proj.ID, MergeRequestID: f.rec.ID})
	if run.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", run.Status, deref(run.FailureReason))
	}
	for _, node := range []string{"one", "two"} {
		msg := sc.message(node, 1)
		if !strings.Contains(msg, "\nMerge request: this run updates merge request !4 (") || !strings.Contains(msg, "(branch "+iterBranch+", toolchain go)") {
			t.Errorf("node %s task lacks the paragraph or the source branch:\n%s", node, msg)
		}
	}
	// One clone for the run, one span.
	if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND name='project_clone'`, run.ID); n != 1 {
		t.Errorf("project_clone spans = %d", n)
	}
}

// T5.22.6, DESIGN 5.22 rule 3 (D-044): a request that turns merged or closed after the
// run was queued is not checked again at clone time: the clone proceeds from the branch
// and the agents are told about it as before.
func TestIterationRule3StateChangeAfterQueueingStillClones(t *testing.T) {
	for _, state := range []string{store.MergeRequestMerged, store.MergeRequestClosed} {
		f := newIterFixture(t)
		env := f.env
		run, err := env.e.Trigger(context.Background(), f.request())
		if err != nil {
			t.Fatal(err)
		}
		f.setState(state)
		r, seen := runSeeing(t, env, run.ID)
		if r.Status != store.RunSucceeded || seen.feature != "the branch's work\n" || seen.later != "" {
			t.Fatalf("%s after queueing: run = %s %q, clone %+v", state, r.Status, deref(r.FailureReason), seen)
		}
		if head := projecttest.Git(t, f.bare, "rev-parse", iterBranch); r.CheckoutSHA == nil || *r.CheckoutSHA != head {
			t.Fatalf("%s: checkout_sha = %v, want %s", state, r.CheckoutSHA, head)
		}
		if msg := env.fc.sendParams()[0].Message; !strings.Contains(msg, "\nMerge request: this run updates merge request !4 (") {
			t.Errorf("%s: the task lacks the paragraph:\n%s", state, msg)
		}
	}
}

// failedRun builds a run that failed at the node "one", with set as extra SQL assignments
// for its columns.
func (f *iterFixture) failedRun(set string) *store.Run {
	f.env.t.Helper()
	return f.env.endedRun(store.RunFailed, "one", set)
}

// DESIGN 5.21 rule 3 amended, 5.22 rule 1: a retry re-enters the live-run index of the
// merge request; while another live run names it the retry is refused 409
// run_not_retryable with the text of the rule, the run unchanged, and it goes through once
// that run ends.
func TestIterationRetryRefusedWhileAnotherRunUpdatesTheRequest(t *testing.T) {
	for _, provider := range []string{"gitlab", "github"} {
		f := newIterFixture(t)
		env, ctx := f.env, context.Background()
		label := "merge request !4"
		if provider == "github" {
			label = "pull request #4"
			if _, err := env.pool.Exec(ctx, `UPDATE merge_requests SET provider='github' WHERE id=$1`, f.rec.ID); err != nil {
				t.Fatal(err)
			}
		}
		failed := f.failedRun("merge_request_id='" + f.rec.ID + "'")
		live := f.liveNaming()
		_, err := env.e.Retry(ctx, failed.ID, "tester", nil)
		wantRetryError(t, err, RetryNotRetryable, label+" is being updated by run "+live.ID)
		unchanged(t, failed, env.run(failed.ID))
		if n := len(env.runEvents(failed.ID, "run.retry")); n != 0 {
			t.Fatalf("%s: a refused retry wrote %d retry events", provider, n)
		}
		f.setRunStatus(live.ID, store.RunSucceeded)
		if got, err := env.e.Retry(ctx, failed.ID, "tester", nil); err != nil || got.Status != store.RunQueued || got.Retries != 1 {
			t.Fatalf("%s: retry once the request is free: %+v %v", provider, got, err)
		}
	}
}

// DESIGN 5.21 rule 3 amended, 5.23 rule 6: a retry while another live run has the same
// issue is refused with the issue's text, naming the number after the # of trigger_ref.
func TestIterationRetryRefusedWhileTheIssueHasARunningRun(t *testing.T) {
	f := newIterFixture(t)
	env, ctx := f.env, context.Background()
	ref := f.proj.ID + "#7"
	failed := f.failedRun("trigger='issue', trigger_ref='" + ref + "'")
	snap, _ := json.Marshal(runSnapshot{Definition: workflow.Definition{}})
	live := &store.Run{WorkflowID: f.wf.ID, WorkflowVersion: 1, WorkflowSnapshot: snap, Trigger: store.TriggerIssue, TriggerRef: &ref,
		Limits: store.DefaultLimits}
	if err := store.CreateRun(ctx, env.pool, live); err != nil {
		t.Fatal(err)
	}
	_, err := env.e.Retry(ctx, failed.ID, "tester", nil)
	wantRetryError(t, err, RetryNotRetryable, "issue #7 already has a running run "+live.ID)
	unchanged(t, failed, env.run(failed.ID))
	f.setRunStatus(live.ID, store.RunFailed)
	if got, err := env.e.Retry(ctx, failed.ID, "tester", nil); err != nil || got.Status != store.RunQueued {
		t.Fatalf("retry once the issue is free: %+v %v", got, err)
	}
}

// DESIGN 5.22 edge cases: a retry of the run that opened the request is refused while
// another run updates it, though the opener names no request (the index does not see it).
func TestIterationRetryOfTheOpeningRunRefusedWhileAnotherRunUpdates(t *testing.T) {
	f := newIterFixture(t)
	env, ctx := f.env, context.Background()
	if _, err := env.pool.Exec(ctx, `UPDATE runs SET status='failed', current_node='one', failure_reason='boom' WHERE id=$1`, f.opener.ID); err != nil {
		t.Fatal(err)
	}
	live := f.liveNaming()
	before := env.run(f.opener.ID)
	_, err := env.e.Retry(ctx, f.opener.ID, "tester", nil)
	wantRetryError(t, err, RetryNotRetryable, "merge request !4 is being updated by run "+live.ID)
	unchanged(t, before, env.run(f.opener.ID))
	f.setRunStatus(live.ID, store.RunSucceeded)
	if got, err := env.e.Retry(ctx, f.opener.ID, "tester", nil); err != nil || got.Status != store.RunQueued {
		t.Fatalf("retry of the opener once the request is free: %+v %v", got, err)
	}
}
