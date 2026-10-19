package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"yuno/internal/project"
	"yuno/internal/store"
)

// T5.29.2: publish_tickets against a fake GitLab and a fake GitHub that keep the issues
// they are given, with a real database for the plan.

const testPlanState = `{
  "slicer": {"tickets": [
    {"id": "T1", "title": "Walking skeleton", "slice": "End to end.", "files": ["a.go", "a_test.go"], "tests": ["TestA", "TestB"]},
    {"id": "T2", "title": "Second", "slice": "More.", "files": ["b.go"], "tests": ["TestC"]},
    {"id": "T3", "title": "Third", "slice": "Later.", "files": ["c.go"], "tests": ["TestD"]}
  ]},
  "waves": {"waves": [{"n": 1, "tickets": ["T1", "T2"]}, {"n": 2, "tickets": ["T3"]}]}
}`

// ticketEnv is the tool over a fake tracker that stores issues.
type ticketEnv struct {
	t       *testing.T
	tool    *Tool
	project *store.Project
	run     *store.Run
	api     *issueAPI
	mu      sync.Mutex
	issues  []map[string]any // created, in order
	failAt  int              // the nth creation (1-based) answers 500 when > 0
	gh      bool
}

func newTicketEnv(t *testing.T, provider project.Provider, state string) *ticketEnv {
	t.Helper()
	pool := needDB(t)
	ctx := context.Background()
	e := &ticketEnv{t: t, gh: provider == project.ProviderGitHub}
	prefix, repo := "/api/v4/projects/group%2Fdemo", "https://gitlab.test/group/demo.git"
	if e.gh {
		prefix, repo = "/repos/owner/demo", "https://github.com/owner/demo.git"
	}
	e.api = newIssueAPI(t, prefix)
	// Labels all exist.
	if e.gh {
		for _, l := range []string{"wave:1", "wave:2", "feature:checkout"} {
			e.api.onFunc("GET /labels/"+l, func(issueCall) issueAnswer { return issueAnswer{200, map[string]any{}} })
		}
	}
	e.api.onFunc("GET /labels", func(c issueCall) issueAnswer {
		return issueAnswer{200, []map[string]any{{"name": c.Query.Get("search")}}}
	})
	e.api.onFunc("POST /issues", e.create)
	e.api.onFunc("GET /issues", e.list)

	e.project = &store.Project{Name: "tickets-" + store.NewID(), RepoURL: repo, Provider: string(provider), DefaultBranch: "main"}
	must(t, store.CreateProject(ctx, pool, e.project, []byte("sealed")))
	wf := &store.Workflow{Name: "tickets " + store.NewID(), Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	must(t, store.CreateWorkflow(ctx, pool, wf))
	settings := e.project.Settings()
	e.run = &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: store.TriggerManual,
		Status: store.RunRunning, ProjectID: &e.project.ID, Project: &settings,
		Input: json.RawMessage(`{"feature":"checkout"}`), State: json.RawMessage(state)}
	must(t, store.CreateRun(ctx, pool, e.run))
	dropRunAfter(t, e.run.ID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM plans WHERE project_id = $1`, e.project.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM merge_requests WHERE project_id = $1`, e.project.ID)
		_ = store.DeleteProject(context.Background(), pool, e.project.ID)
	})

	pc := project.Config{RepoURL: repo, Token: issueTestToken, DefaultBranch: "main", Provider: provider}
	e.tool = NewPublishTickets(PublishTicketsConfig{DB: pool, Projects: staticProjects{pc: pc}, APIBase: e.api.base,
		RepoPath: pc.Path(), RunIDForToken: func(tok string) (string, bool) { return e.run.ID, tok == "tok" }})
	return e
}

func (e *ticketEnv) create(c issueCall) issueAnswer {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failAt > 0 && len(e.issues)+1 == e.failAt {
		return issueAnswer{500, map[string]string{"message": "boom"}}
	}
	n := len(e.issues) + 1
	body := map[string]any{}
	for k, v := range c.Body {
		body[k] = v
	}
	e.issues = append(e.issues, body)
	if e.gh {
		return issueAnswer{201, map[string]any{"number": n, "html_url": fmt.Sprintf("https://github.com/owner/demo/issues/%d", n)}}
	}
	return issueAnswer{201, map[string]any{"iid": n, "web_url": fmt.Sprintf("https://gitlab.test/group/demo/-/issues/%d", n)}}
}

// list answers the marker lookup from the issues created so far (newest first).
func (e *ticketEnv) list(issueCall) issueAnswer {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []map[string]any{}
	for i := len(e.issues) - 1; i >= 0; i-- {
		is := e.issues[i]
		n := i + 1
		if e.gh {
			out = append(out, map[string]any{"number": n, "title": is["title"], "body": is["body"], "html_url": fmt.Sprintf("https://github.com/owner/demo/issues/%d", n)})
		} else {
			out = append(out, map[string]any{"iid": n, "title": is["title"], "description": is["description"], "web_url": fmt.Sprintf("https://gitlab.test/group/demo/-/issues/%d", n)})
		}
	}
	return issueAnswer{200, out}
}

func (e *ticketEnv) call(run *store.Run) (Result, error) {
	e.t.Helper()
	return e.tool.Exec(context.Background(), Call{RunID: run.ID, Input: json.RawMessage(`{"run_token":"tok"}`)})
}

func (e *ticketEnv) body(i int) string {
	if e.gh {
		return e.issues[i]["body"].(string)
	}
	return e.issues[i]["description"].(string)
}

func TestPublishTicketsCreatesPlanAndIssues(t *testing.T) {
	for _, provider := range []project.Provider{project.ProviderGitLab, project.ProviderGitHub} {
		t.Run(string(provider), func(t *testing.T) {
			e := newTicketEnv(t, provider, testPlanState)
			// The plan MR's record is linked to the plan; the doc links name the default
			// branch, not the MR's branch, and the MR's page is mentioned (M6).
			mr := projectRecord(t, e.project, e.run, 3, store.MergeRequestOpen)
			res, err := e.call(e.run)
			if err != nil || res.IsError {
				t.Fatalf("result = %+v, %v", res, err)
			}
			var out publishTicketsOutput
			must(t, json.Unmarshal(res.Output, &out))
			if len(out.Issues) != 3 || out.Reused != 0 || out.Issues[0].Ticket != "T1" || out.Issues[2].Ticket != "T3" ||
				out.Issues[0].Number != 1 || out.Issues[2].Number != 3 || out.Issues[0].URL == "" {
				t.Fatalf("output = %+v", out)
			}
			if len(e.issues) != 3 {
				t.Fatalf("created %d issues", len(e.issues))
			}
			first := e.issues[0]
			if first["title"] != "[checkout T1] Walking skeleton" {
				t.Errorf("title = %v", first["title"])
			}
			if e.gh {
				if fmt.Sprint(first["labels"]) != "[wave:1 feature:checkout]" {
					t.Errorf("labels = %v", first["labels"])
				}
			} else if first["labels"] != "wave:1,feature:checkout" {
				t.Errorf("labels = %v", first["labels"])
			}
			if got := e.issues[2]["title"]; got != "[checkout T3] Third" {
				t.Errorf("third title = %v", got)
			}
			marker := "<!-- yuno-ticket:" + out.PlanID + ":T1 -->"
			body := e.body(0)
			for _, want := range []string{"End to end.", "- a.go", "- a_test.go", "Acceptance tests:", "- TestA", "- TestB", "Feature: checkout",
				"/blob/main/docs/plans/checkout/PRD.md", mr.URL, "Plan merge request"} {
				if !strings.Contains(body, want) {
					t.Errorf("body lacks %q:\n%s", want, body)
				}
			}
			if !strings.HasSuffix(body, marker) {
				t.Errorf("body does not end with the marker %q:\n%s", marker, body)
			}
			if strings.Contains(body, mr.SourceBranch) {
				t.Errorf("a doc link names the plan MR's branch %q:\n%s", mr.SourceBranch, body)
			}
			if !strings.Contains(e.body(1), "- TestC") {
				t.Errorf("the tests of T2 are not listed:\n%s", e.body(1))
			}
			for _, bad := range e.api.seen() {
				if strings.Contains(bad, issueTestToken) {
					t.Errorf("token in call %q", bad)
				}
			}

			plan, err := store.GetPlan(context.Background(), testPool, out.PlanID)
			must(t, err)
			if plan.Status != store.PlanWaitingPlanMerge || plan.Feature != "checkout" || plan.PlanRunID != e.run.ID ||
				plan.PlanMRID == nil || *plan.PlanMRID != mr.ID {
				t.Fatalf("plan = %+v", plan)
			}
			tickets, err := store.ListPlanTickets(context.Background(), testPool, plan.ID)
			must(t, err)
			if len(tickets) != 3 || tickets[0].Wave != 1 || tickets[2].Wave != 2 || tickets[2].Status != store.TicketPending ||
				tickets[1].IssueNumber == nil || *tickets[1].IssueNumber != 2 || tickets[1].IssueURL == "" {
				t.Fatalf("tickets = %+v", tickets)
			}
			events, err := store.ListPlanEvents(context.Background(), testPool, plan.ID, 0)
			must(t, err)
			if len(events) != 1 || events[0].Kind != store.PlanEventPublished {
				t.Fatalf("events = %+v", events)
			}

			// A second visit of the same run reuses every issue and creates none.
			res, err = e.call(e.run)
			if err != nil || res.IsError {
				t.Fatalf("retry = %+v, %v", res, err)
			}
			var again publishTicketsOutput
			must(t, json.Unmarshal(res.Output, &again))
			if again.PlanID != out.PlanID || again.Reused != 3 || len(e.issues) != 3 {
				t.Fatalf("retry output = %+v, issues %d", again, len(e.issues))
			}
		})
	}
}

// A failure on the second ticket names the ticket; the retry of the visit (here: a fresh
// plan row, as after a rolled-back transaction) reuses the first issue by its marker.
func TestPublishTicketsFailureThenRetryReusesByMarker(t *testing.T) {
	e := newTicketEnv(t, project.ProviderGitLab, testPlanState)
	e.failAt = 2
	res, err := e.call(e.run)
	if err != nil || !res.IsError || !strings.Contains(res.Text, "ticket T2") || strings.Contains(res.Text, issueTestToken) {
		t.Fatalf("failure = %+v, %v", res, err)
	}
	if len(e.issues) != 1 {
		t.Fatalf("issues = %d", len(e.issues))
	}
	// The rolled-back transaction takes the plan with it.
	_, err = testPool.Exec(context.Background(), `DELETE FROM plans WHERE project_id = $1`, e.project.ID)
	must(t, err)
	e.failAt = 0
	res, err = e.call(e.run)
	if err != nil || res.IsError {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	var out publishTicketsOutput
	must(t, json.Unmarshal(res.Output, &out))
	if len(e.issues) != 3 || out.Reused != 1 || len(out.Issues) != 3 {
		t.Fatalf("issues %d, output %+v", len(e.issues), out)
	}
}

func TestPublishTicketsRefusals(t *testing.T) {
	ctx := context.Background()
	// A second open plan names the first.
	e := newTicketEnv(t, project.ProviderGitLab, testPlanState)
	other := newRunOf(t, e, `{"feature":"search"}`, testPlanState)
	open := &store.Plan{ProjectID: e.project.ID, Feature: "search", PlanRunID: other.ID}
	must(t, store.CreatePlan(ctx, testPool, open))
	_, err := e.call(e.run)
	var me *ModelError
	if !asModelError(err, &me) || !strings.Contains(me.Message, "open plan") || !strings.Contains(me.Message, `"search"`) || !strings.Contains(me.Message, open.ID) {
		t.Fatalf("second plan: %v", err)
	}
	if len(e.issues) != 0 {
		t.Errorf("issues were created: %d", len(e.issues))
	}
	// Finished plan of the same feature: refused too.
	must(t, store.SetPlanStatus(ctx, testPool, open.ID, store.PlanDone))
	done := &store.Plan{ProjectID: e.project.ID, Feature: "checkout", PlanRunID: other.ID, Status: store.PlanDone}
	must(t, store.CreatePlan(ctx, testPool, done))
	if _, err = e.call(e.run); !asModelError(err, &me) || !strings.Contains(me.Message, "plan for feature") {
		t.Fatalf("same feature: %v", err)
	}

	// A run without a project.
	wf := &store.Workflow{Name: "noproject " + store.NewID(), Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	must(t, store.CreateWorkflow(ctx, testPool, wf))
	bare := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: store.TriggerManual,
		Status: store.RunRunning, Input: json.RawMessage(`{"feature":"x"}`), State: json.RawMessage(testPlanState)}
	must(t, store.CreateRun(ctx, testPool, bare))
	dropRunAfter(t, bare.ID)
	if _, err = e.call(bare); !asModelError(err, &me) {
		t.Fatalf("no project: %v", err)
	}

	// Bad plans: no tickets, a ticket in no wave, too many tickets, too many waves.
	cases := map[string]string{
		"no tickets":  `{"waves": {"waves": [{"n":1,"tickets":["T1"]}]}}`,
		"no waves":    `{"slicer": {"tickets": [{"id":"T1","title":"a"}]}}`,
		"unplaced":    `{"slicer": {"tickets": [{"id":"T1","title":"a"},{"id":"T2","title":"b"}]}, "waves": {"waves": [{"n":1,"tickets":["T1"]}]}}`,
		"unknown":     `{"slicer": {"tickets": [{"id":"T1","title":"a"}]}, "waves": {"waves": [{"n":1,"tickets":["T1","T9"]}]}}`,
		"many":        bigPlan(MaxPlanTickets+1, 1),
		"many waves":  bigPlan(MaxPlanWaves+1, MaxPlanWaves+1),
		"cap ok":      "",
		"gap in wave": `{"slicer": {"tickets": [{"id":"T1","title":"a"}]}, "waves": {"waves": [{"n":2,"tickets":["T1"]}]}}`,
	}
	for name, state := range cases {
		if state == "" {
			continue
		}
		r := newRunOf(t, e, `{"feature":"f-`+strings.ReplaceAll(name, " ", "-")+`"}`, state)
		if _, err := e.call(r); !asModelError(err, &me) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(e.issues) != 0 {
		t.Errorf("issues were created: %d", len(e.issues))
	}
}

func TestPublishTicketsDescribeCountsPerWave(t *testing.T) {
	e := newTicketEnv(t, project.ProviderGitLab, testPlanState)
	got := e.tool.Message(json.RawMessage(`{"run_token":"tok"}`))
	for _, want := range []string{"3 tickets", `"checkout"`, "wave 1: 2", "wave 2: 1", "feature:checkout"} {
		if !strings.Contains(got, want) {
			t.Errorf("sentence %q lacks %q", got, want)
		}
	}
	if got := e.tool.Message(json.RawMessage(`{"run_token":"other"}`)); !strings.Contains(got, "Publish") {
		t.Errorf("generic sentence = %q", got)
	}
}

func bigPlan(tickets, waves int) string {
	var ts, ws []string
	per := make([][]string, waves)
	for i := 1; i <= tickets; i++ {
		id := fmt.Sprintf("T%d", i)
		ts = append(ts, fmt.Sprintf(`{"id":%q,"title":"t"}`, id))
		per[(i-1)%waves] = append(per[(i-1)%waves], fmt.Sprintf("%q", id))
	}
	for i, p := range per {
		ws = append(ws, fmt.Sprintf(`{"n":%d,"tickets":[%s]}`, i+1, strings.Join(p, ",")))
	}
	return fmt.Sprintf(`{"slicer":{"tickets":[%s]},"waves":{"waves":[%s]}}`, strings.Join(ts, ","), strings.Join(ws, ","))
}

// newRunOf queues another run of the environment's project with the given input and state.
func newRunOf(t *testing.T, e *ticketEnv, input, state string) *store.Run {
	t.Helper()
	wf := &store.Workflow{Name: "tickets " + store.NewID(), Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	must(t, store.CreateWorkflow(context.Background(), testPool, wf))
	settings := e.project.Settings()
	run := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: store.TriggerManual,
		Status: store.RunRunning, ProjectID: &e.project.ID, Project: &settings, Input: json.RawMessage(input), State: json.RawMessage(state)}
	must(t, store.CreateRun(context.Background(), testPool, run))
	dropRunAfter(t, run.ID)
	return run
}

func asModelError(err error, target **ModelError) bool {
	me, ok := err.(*ModelError)
	if ok {
		*target = me
	}
	return ok
}

// issueListCalls counts the calls that listed issues.
func (e *ticketEnv) issueListCalls() int {
	n := 0
	for _, c := range e.api.seen() {
		if strings.HasPrefix(c, "GET /issues") {
			n++
		}
	}
	return n
}

// M7: a call lists the feature's issues once, however many tickets there are, and a retry
// of the whole call still lists once.
func TestPublishTicketsListsOncePerCall(t *testing.T) {
	e := newTicketEnv(t, project.ProviderGitLab, testPlanState)
	if res, err := e.call(e.run); err != nil || res.IsError {
		t.Fatalf("first = %+v, %v", res, err)
	}
	if n := e.issueListCalls(); n != 1 {
		t.Fatalf("listed %d times for 3 tickets", n)
	}
	// Roll the plan back: all three issues are found by marker, in one more listing.
	_, err := testPool.Exec(context.Background(), `DELETE FROM plans WHERE project_id = $1`, e.project.ID)
	must(t, err)
	res, err := e.call(e.run)
	if err != nil || res.IsError {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	var out publishTicketsOutput
	must(t, json.Unmarshal(res.Output, &out))
	if out.Reused != 3 || len(e.issues) != 3 || e.issueListCalls() != 2 {
		t.Fatalf("reused %d, issues %d, listings %d", out.Reused, len(e.issues), e.issueListCalls())
	}
}

// L3: only a marker on the body's last non-empty line identifies an issue; a quote of one
// elsewhere in a body matches nothing.
func TestPublishTicketsMatchesTheLastLineOnly(t *testing.T) {
	e := newTicketEnv(t, project.ProviderGitLab, testPlanState)
	planID := uuidFor(e.run.ID, "checkout")
	// Someone else's issue quotes T1's marker in the middle of its text.
	e.issues = append(e.issues, map[string]any{"title": "quote", "description": "see " + ticketMarker(planID, "T1") + " above\nand more"})
	// A real earlier issue of T2 ends with its marker and trailing blank lines.
	e.issues = append(e.issues, map[string]any{"title": "[checkout T2] Second", "description": "text\n" + ticketMarker(planID, "T2") + "\n\n"})
	res, err := e.call(e.run)
	if err != nil || res.IsError {
		t.Fatalf("call = %+v, %v", res, err)
	}
	var out publishTicketsOutput
	must(t, json.Unmarshal(res.Output, &out))
	if out.Reused != 1 || len(e.issues) != 4 || out.Issues[1].Ticket != "T2" || out.Issues[1].Number != 2 {
		t.Fatalf("reused %d, issues %d, output %+v", out.Reused, len(e.issues), out.Issues)
	}
}

func TestLastLine(t *testing.T) {
	for in, want := range map[string]string{"": "", "a": "a", "a\nb\n": "b", "a\r\nb \r\n\r\n": "b", "\n\n": ""} {
		if got := lastLine(in); got != want {
			t.Errorf("lastLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// M5: a plan created before the run's merge request existed gets the link on a later call.
func TestPublishTicketsLinksThePlanMRLater(t *testing.T) {
	e := newTicketEnv(t, project.ProviderGitHub, testPlanState)
	res, err := e.call(e.run)
	if err != nil || res.IsError {
		t.Fatalf("first = %+v, %v", res, err)
	}
	var out publishTicketsOutput
	must(t, json.Unmarshal(res.Output, &out))
	plan, err := store.GetPlan(context.Background(), testPool, out.PlanID)
	must(t, err)
	if plan.PlanMRID != nil {
		t.Fatalf("plan_mr_id = %v before there is a request", *plan.PlanMRID)
	}
	mr := projectRecord(t, e.project, e.run, 4, store.MergeRequestOpen)
	if res, err = e.call(e.run); err != nil || res.IsError {
		t.Fatalf("second = %+v, %v", res, err)
	}
	plan, err = store.GetPlan(context.Background(), testPool, out.PlanID)
	must(t, err)
	if plan.PlanMRID == nil || *plan.PlanMRID != mr.ID {
		t.Fatalf("plan_mr_id = %v, want %s", plan.PlanMRID, mr.ID)
	}
}

// H1: the tool reads the slicer's own output: slice and tests; acceptance_tests still works
// as a fallback.
func TestPublishTicketsReadsTheSlicerShape(t *testing.T) {
	state := `{"slicer":{"tickets":[
	  {"id":"T1","title":"One","slice":"The slice text.","files":["a.go"],"tests":["TestOne","TestTwo"]},
	  {"id":"T2","title":"Two","slice":"Other.","files":["b.go"],"acceptance_tests":["TestOld"]}]},
	  "waves":{"waves":[{"n":1,"tickets":["T1","T2"]}]}}`
	e := newTicketEnv(t, project.ProviderGitLab, state)
	if res, err := e.call(e.run); err != nil || res.IsError {
		t.Fatalf("call = %+v, %v", res, err)
	}
	for i, wants := range [][]string{{"The slice text.", "- a.go", "Acceptance tests:", "- TestOne", "- TestTwo"}, {"Other.", "- TestOld"}} {
		for _, w := range wants {
			if !strings.Contains(e.body(i), w) {
				t.Errorf("body %d lacks %q:\n%s", i, w, e.body(i))
			}
		}
	}
}

// L4: the feature is a slug and a long title is capped in the issue title.
func TestPublishTicketsChecksFeatureAndTitle(t *testing.T) {
	e := newTicketEnv(t, project.ProviderGitLab, testPlanState)
	var me *ModelError
	for _, bad := range []string{"Check Out", "a/b", "../x", strings.Repeat("a", 41), "x y"} {
		r := newRunOf(t, e, fmt.Sprintf(`{"feature":%q}`, bad), testPlanState)
		if _, err := e.call(r); !asModelError(err, &me) || !strings.Contains(me.Message, "slug") {
			t.Errorf("feature %q: %v", bad, err)
		}
	}
	if len(e.issues) != 0 {
		t.Fatalf("issues were created: %d", len(e.issues))
	}
	long := strings.Repeat("é", 300)
	state := fmt.Sprintf(`{"slicer":{"tickets":[{"id":"T1","title":%q,"slice":"s","files":["a"],"tests":["t"]}]},"waves":{"waves":[{"n":1,"tickets":["T1"]}]}}`, long)
	r := newRunOf(t, e, `{"feature":"checkout"}`, state)
	if res, err := e.call(r); err != nil || res.IsError {
		t.Fatalf("long title = %+v, %v", res, err)
	}
	title := e.issues[0]["title"].(string)
	if want := "[checkout T1] " + strings.Repeat("é", maxTicketTitle); title != want {
		t.Fatalf("title has %d runes, want %d", len([]rune(title)), len([]rune(want)))
	}
}

func uuidFor(runID, feature string) string {
	return uuid.NewSHA1(planIDNamespace, []byte(runID+"\x00"+feature)).String()
}
