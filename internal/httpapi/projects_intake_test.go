package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"yuno/internal/engine"
	"yuno/internal/store"
)

// Issue intake over HTTP (DESIGN 5.23 rules 1 and 2): the project's intake settings and
// their 422 texts, and the poll-now route.

const (
	taskSchema = `{"type":"object","properties":{"task":{"type":"string"}},"required":["task"]}`
	// repoSchema is an input_schema that cannot take {"task": ...}: it needs repo and
	// forbids anything else, so the sample breaks two keywords.
	repoSchema = `{"type":"object","properties":{"repo":{"type":"string"}},"required":["repo"],"additionalProperties":false}`
)

// intakeWorkflow stores a workflow with the given definition members; templateKey makes it
// a template or a quick-run wrapper.
func intakeWorkflow(t *testing.T, name, members string, templateKey *string) *store.Workflow {
	t.Helper()
	def := `{"nodes":[],"edges":[]`
	if members != "" {
		def += "," + members
	}
	wf := &store.Workflow{Name: name + " " + uniqueSuffix(), Definition: json.RawMessage(def + `}`), TemplateKey: templateKey}
	mustDo(t, store.CreateWorkflow(context.Background(), testPool, wf))
	t.Cleanup(func() { _ = store.DeleteWorkflow(context.Background(), testPool, wf.ID) })
	return wf
}

func goodIntakeWorkflow(t *testing.T) *store.Workflow {
	return intakeWorkflow(t, "intake good", `"project":true,"input_schema":`+taskSchema, nil)
}

// intakeOf decodes the intake member of a project body.
func intakeOf(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var p struct {
		Intake map[string]any `json:"intake"`
	}
	mustDo(t, json.Unmarshal(body, &p))
	if p.Intake == nil {
		t.Fatalf("no intake in %s", body)
	}
	return p.Intake
}

// T5.23.1: each text of rule 1, a valid save round-tripping `intake`, absent on update
// keeping the settings.
func TestProjectIntakeSettingsValidationT5231(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	good := goodIntakeWorkflow(t)
	other := intakeWorkflow(t, "intake other", `"project":true`, nil) // no input_schema: takes anything
	noProject := intakeWorkflow(t, "intake no project", `"input_schema":`+taskSchema, nil)
	badSchema := intakeWorkflow(t, "intake bad schema", `"project":true,"input_schema":`+repoSchema, nil)
	tplKey, quickKey := "intake-template-"+uniqueSuffix(), "quickrun:"+uniqueSuffix()
	template := intakeWorkflow(t, "intake template", `"project":true`, &tplKey)
	quick := intakeWorkflow(t, "intake quick run", `"project":true`, &quickKey)
	gone := intakeWorkflow(t, "intake gone", `"project":true`, nil)
	mustDo(t, store.DeleteWorkflow(ctx, pool, gone.ID))

	const (
		labelText   = "intake.label: 1 to 50 characters without spaces or commas"
		requiredMsg = "intake.workflow_id: required when intake is enabled"
		budgetText  = "intake.enabled: set a daily or monthly budget before enabling intake"
	)
	taskMsgs := engine.ValidateInput(json.RawMessage(repoSchema), intakeTaskSample())
	if len(taskMsgs) < 2 {
		t.Fatalf("the sample should break two keywords of the schema: %q", taskMsgs)
	}
	schemaText := `intake.workflow_id: the workflow's input must accept {"task": string}: ` + strings.Join(taskMsgs, "; ")
	noWorkflow := func(id string) string { return "intake.workflow_id: no workflow with id " + id }

	in := func(enabled bool, label, workflowID string) string {
		s := fmt.Sprintf(`"intake":{"enabled":%v,"label":%s`, enabled, jsonText(label))
		if workflowID != "" {
			s += `,"workflow_id":` + jsonText(workflowID)
		}
		return s + "}"
	}
	const budget = `"max_cost_usd_per_day":5`
	reject := func(what, extra string, want ...string) {
		t.Helper()
		code, body := do(t, ts, http.MethodPost, "/api/projects", projectBody(uniqueSuffix(), extra))
		if code != http.StatusUnprocessableEntity || fmt.Sprint(fieldMessages(t, body)) != fmt.Sprint(want) {
			t.Errorf("%s: %d %s, want 422 %q", what, code, body, want)
		}
	}

	// intake.label: trimmed, 1 to 50 bytes, no comma, no whitespace, no control character.
	for what, label := range map[string]string{
		"a comma": "a,b", "a space": "a b", "a tab": "a\tb", "a line break": "a\nb", "a bell": "a\u0007b", "a NUL": "a\u0000b",
		"a no-break space": "a b", "empty": "", "blank": "   ", "51 bytes": strings.Repeat("a", 51),
		"26 two-byte characters": strings.Repeat("é", 26),
	} {
		reject("label with "+what, in(false, label, ""), labelText)
	}
	// GitLab reads labels=None as the unlabelled issues and labels=Any as every labelled
	// one: neither can be the intake label, in any case (review L7).
	const noneAnyText = "intake.label: must not be None or Any"
	for _, label := range []string{"None", "none", "NONE", "nOnE", "Any", "any", "ANY", " Any ", "\tnone\n"} {
		reject("the label "+jsonText(label), in(false, label, ""), noneAnyText)
	}
	create := func(what, extra string) store.Project {
		t.Helper()
		code, body := do(t, ts, http.MethodPost, "/api/projects", projectBody(uniqueSuffix(), extra))
		if code != http.StatusCreated {
			t.Fatalf("%s: %d %s, want 201", what, code, body)
		}
		p := decodeProject(t, body)
		t.Cleanup(func() { _ = store.DeleteProject(ctx, pool, p.ID) })
		return p
	}
	if p := create("50 bytes", in(false, strings.Repeat("a", 50), "")); p.Intake.Label != strings.Repeat("a", 50) {
		t.Errorf("50 byte label = %q", p.Intake.Label)
	}
	if p := create("25 two-byte characters", in(false, strings.Repeat("é", 25), "")); p.Intake.Label != strings.Repeat("é", 25) {
		t.Errorf("50 byte label of two-byte characters = %q", p.Intake.Label)
	}
	if p := create("trimmed", in(false, "  bot  ", "")); p.Intake.Label != "bot" {
		t.Errorf("trimmed label = %q", p.Intake.Label)
	}
	// Only the whole word is refused.
	for _, label := range []string{"Anything", "none-of-these", "any:thing", "Nonexistent", "n", "a"} {
		if p := create("the label "+label, in(false, label, "")); p.Intake.Label != label {
			t.Errorf("label %q saved as %q", label, p.Intake.Label)
		}
	}
	if p := create("label absent", `"intake":{"enabled":false}`); p.Intake.Label != "yuno" {
		t.Errorf("default label = %q", p.Intake.Label)
	}

	// intake.workflow_id.
	reject("enabled without a workflow", in(true, "yuno", "")+","+budget, requiredMsg)
	reject("enabled with a blank workflow", in(true, "yuno", "  ")+","+budget, requiredMsg)
	reject("an unknown workflow", in(true, "yuno", "no-such-workflow")+","+budget, noWorkflow("no-such-workflow"))
	reject("a template", in(true, "yuno", template.ID)+","+budget, noWorkflow(template.ID))
	reject("a quick-run wrapper", in(true, "yuno", quick.ID)+","+budget, noWorkflow(quick.ID))
	reject("a deleted workflow", in(true, "yuno", gone.ID)+","+budget, noWorkflow(gone.ID))
	reject("a workflow without a project", in(true, "yuno", noProject.ID)+","+budget, "intake.workflow_id: the workflow does not use a project")
	reject("an input schema without task", in(true, "yuno", badSchema.ID)+","+budget, schemaText)
	reject("a workflow id with a control character", in(true, "yuno", "a\u0000b")+","+budget, noWorkflow("a\u0000b"))
	// A workflow set while intake is off is checked all the same when it is a new one.
	reject("disabled, an unknown workflow", in(false, "yuno", "no-such-workflow"), noWorkflow("no-such-workflow"))

	// intake.enabled: a budget cap first (D-055).
	reject("enabled without a budget", in(true, "yuno", good.ID), budgetText)
	reject("enabled with both caps null", in(true, "yuno", good.ID)+`,"max_cost_usd_per_day":null,"max_cost_usd_per_month":null`, budgetText)
	create("enabled with a daily cap", in(true, "yuno", good.ID)+`,"max_cost_usd_per_day":5`)
	create("enabled with a monthly cap", in(true, "yuno", good.ID)+`,"max_cost_usd_per_month":50`)
	// A refused cap has its own detail and does not add the intake's.
	reject("enabled with a refused cap", in(true, "yuno", good.ID)+`,"max_cost_usd_per_day":0`, "max_cost_usd_per_day: must be greater than 0")
	// Disabled needs no budget and no workflow.
	if p := create("disabled", in(false, "yuno", "")); p.Intake.Enabled || p.Intake.WorkflowID != nil {
		t.Errorf("disabled project = %+v", p.Intake)
	}
	// Every detail at once, in the rule's order.
	reject("all three", in(true, "a b", ""), labelText, requiredMsg, budgetText)
	reject("all three with a workflow that breaks the schema", in(true, "a,b", badSchema.ID), labelText, schemaText, budgetText)

	// A valid save: the answer, the stored columns and a later get all show it, with the
	// workflow's name joined and no poll yet.
	p := create("valid", in(true, "bot", good.ID)+","+budget)
	stored, err := store.GetProject(ctx, pool, p.ID)
	mustDo(t, err)
	if !stored.IntakeEnabled || stored.IntakeLabel != "bot" || stored.IntakeWorkflowID == nil || *stored.IntakeWorkflowID != good.ID {
		t.Fatalf("stored = %+v %q %v", stored.IntakeEnabled, stored.IntakeLabel, stored.IntakeWorkflowID)
	}
	code, body := do(t, ts, http.MethodGet, "/api/projects/"+p.ID, "")
	if code != http.StatusOK {
		t.Fatalf("get: %d %s", code, body)
	}
	got := intakeOf(t, body)
	if got["enabled"] != true || got["label"] != "bot" || got["workflow_id"] != good.ID || got["workflow_name"] != good.Name {
		t.Errorf("intake = %v", got)
	}
	if _, ok := got["polled_at"]; ok {
		t.Errorf("polled_at before the first poll: %v", got)
	}
	if _, ok := got["last_error"]; ok {
		t.Errorf("last_error before the first poll: %v", got)
	}
	// The list carries it too.
	_, body = do(t, ts, http.MethodGet, "/api/projects", "")
	var list []store.Project
	mustDo(t, json.Unmarshal(body, &list))
	found := false
	for _, q := range list {
		if q.ID == p.ID {
			found = q.Intake != nil && q.Intake.Enabled && q.Intake.Label == "bot"
		}
	}
	if !found {
		t.Errorf("the list does not show the project's intake: %s", body)
	}

	// PUT back what was fetched (the view's members are accepted and ignored): unchanged.
	_, body = do(t, ts, http.MethodGet, "/api/projects/"+p.ID, "")
	if c, b := do(t, ts, http.MethodPut, "/api/projects/"+p.ID, string(body)); c != http.StatusOK || intakeOf(t, b)["label"] != "bot" {
		t.Fatalf("PUT back: %d %s", c, b)
	}

	put := func(extra string) (int, []byte) {
		base := `"name":"` + p.Name + `","repo_url":"` + p.RepoURL + `"`
		return do(t, ts, http.MethodPut, "/api/projects/"+p.ID, `{`+base+","+extra+`}`)
	}

	// Absent keeps: the update names no intake and the settings stay; a null intake is
	// absent too. The cap travels with the update (an update replaces the caps, 5.19 rule
	// 1), since intake on without a cap is refused whichever way it comes about (D-055).
	for what, extra := range map[string]string{"absent": `"instructions":"x",` + budget, "null": `"intake":null,` + budget} {
		code, body = put(extra)
		got = intakeOf(t, body)
		if code != http.StatusOK || got["enabled"] != true || got["label"] != "bot" || got["workflow_id"] != good.ID {
			t.Errorf("update with intake %s: %d %s", what, code, body)
		}
		if s, _ := store.GetProject(ctx, pool, p.ID); !s.IntakeEnabled || s.IntakeLabel != "bot" || *s.IntakeWorkflowID != good.ID {
			t.Errorf("update with intake %s changed the stored settings", what)
		}
	}
	// D-055 on the resulting state: keeping intake on while both caps are cleared is refused.
	if code, body := put(`"instructions":"x"`); code != http.StatusUnprocessableEntity || !strings.Contains(string(body), "set a daily or monthly budget before enabling intake") {
		t.Errorf("update clearing the caps with intake on: %d %s", code, body)
	}
	// A refused update changes nothing.
	if code, _ = put(in(true, "a b", other.ID) + "," + budget); code != http.StatusUnprocessableEntity {
		t.Fatalf("refused update: %d", code)
	}
	if s, _ := store.GetProject(ctx, pool, p.ID); s.IntakeLabel != "bot" || *s.IntakeWorkflowID != good.ID {
		t.Error("a refused update changed the stored settings")
	}
	// Enabling when this update clears the caps is refused: the body says enabled and sets
	// no cap.
	if code, body = put(in(true, "bot", good.ID)); code != http.StatusUnprocessableEntity ||
		fmt.Sprint(fieldMessages(t, body)) != "["+budgetText+"]" {
		t.Errorf("enabled without the cap on an update: %d %s", code, body)
	}

	// Present replaces the three: another workflow, label and switch; an absent member is
	// its default (off, yuno, none).
	code, body = put(in(true, "next", other.ID) + `,"max_cost_usd_per_month":10`)
	got = intakeOf(t, body)
	if code != http.StatusOK || got["label"] != "next" || got["workflow_id"] != other.ID || got["workflow_name"] != other.Name {
		t.Fatalf("replace: %d %s", code, body)
	}
	code, body = put(`"intake":{"enabled":false}`)
	got = intakeOf(t, body)
	if _, hasWorkflow := got["workflow_id"]; code != http.StatusOK || got["enabled"] != false || got["label"] != "yuno" || hasWorkflow {
		t.Fatalf("present with defaults: %d %s", code, body)
	}

	// A stored workflow that was deleted since does not stop the project from being saved
	// with intake off, as it was fetched; naming another one, or enabling, checks again.
	stale := intakeWorkflow(t, "intake stale", `"project":true`, nil)
	code, body = put(in(true, "yuno", stale.ID) + `,"max_cost_usd_per_day":5`)
	if code != http.StatusOK {
		t.Fatalf("set the stale workflow: %d %s", code, body)
	}
	mustDo(t, store.DeleteWorkflow(ctx, pool, stale.ID))
	if code, body = put(in(true, "yuno", stale.ID) + `,"max_cost_usd_per_day":5`); code != http.StatusUnprocessableEntity ||
		fmt.Sprint(fieldMessages(t, body)) != "["+noWorkflow(stale.ID)+"]" {
		t.Errorf("enabled with a deleted workflow: %d %s", code, body)
	}
	if code, body = put(in(false, "yuno", stale.ID)); code != http.StatusOK || intakeOf(t, body)["enabled"] != false {
		t.Errorf("disabled, the stored workflow deleted since: %d %s", code, body)
	}
	if code, body = put(in(false, "yuno", gone.ID)); code != http.StatusUnprocessableEntity {
		t.Errorf("disabled, another deleted workflow: %d %s", code, body)
	}
}

// ---- the poll ---------------------------------------------------------------------------

// intakeAPI is a fake of GitLab's issue API for one repository: the open issues labelled
// for the intake (one disappears from the list when its labels are swapped), a status to
// fail the listing with, and every comment.
type intakeAPI struct {
	mu       sync.Mutex
	issues   []map[string]any
	failList int
	notes    []string
	calls    []string
	tokens   []string
}

func newIntakeAPI(t *testing.T) (*intakeAPI, http.RoundTripper) {
	t.Helper()
	f := &intakeAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.tokens = append(f.tokens, r.Header.Get("PRIVATE-TOKEN"))
		rest := r.URL.EscapedPath()
		if i := strings.Index(rest, "/api/v4/projects/"); i >= 0 {
			rest = rest[i+len("/api/v4/projects/"):]
			if j := strings.Index(rest, "/"); j >= 0 {
				rest = rest[j:]
			}
		}
		f.calls = append(f.calls, r.Method+" "+rest)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case !strings.HasPrefix(r.Header.Get("PRIVATE-TOKEN"), acceptedTokenPrefix):
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
		case r.Method == http.MethodGet && rest == "/labels":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"name": r.URL.Query().Get("search")}})
		case r.Method == http.MethodGet && rest == "/issues":
			if f.failList != 0 {
				w.WriteHeader(f.failList)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(f.issues)
		case r.Method == http.MethodPut && strings.HasPrefix(rest, "/issues/"):
			n := strings.TrimPrefix(rest, "/issues/")
			kept := f.issues[:0]
			for _, i := range f.issues {
				if fmt.Sprint(i["iid"]) != n {
					kept = append(kept, i)
				}
			}
			f.issues = kept
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && strings.HasSuffix(rest, "/notes"):
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			f.notes = append(f.notes, fmt.Sprint(b["body"]))
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"404 Not Found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	return f, rewriteTransport{target: target}
}

func (f *intakeAPI) labelled(iid int, title, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues = append(f.issues, map[string]any{"iid": iid, "title": title, "description": body,
		"web_url": fmt.Sprintf("https://gitlab.test/group/x/-/issues/%d", iid)})
}

func (f *intakeAPI) sentNotes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.notes...)
}

// intakeEngine is the engine fake with a Trigger that stores the run as the real one does:
// its reference, its input and the project's snapshot.
type intakeEngine struct{ *fakeEngine }

func (e intakeEngine) Trigger(ctx context.Context, req engine.TriggerRequest) (*store.Run, error) {
	e.mu.Lock()
	e.triggers = append(e.triggers, req)
	err := e.triggerErr
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	wf, err := store.GetWorkflow(ctx, e.pool, req.WorkflowID)
	if err != nil {
		return nil, err
	}
	p, err := store.GetProject(ctx, e.pool, req.ProjectID)
	if err != nil {
		return nil, err
	}
	settings, ref := p.Settings(), req.TriggerRef
	run := &store.Run{WorkflowID: wf.ID, WorkflowVersion: wf.Version, WorkflowSnapshot: wf.Definition, Trigger: req.Trigger,
		TriggerRef: &ref, Input: req.Input, ProjectID: &p.ID, Project: &settings}
	if err := store.CreateRun(ctx, e.pool, run); err != nil {
		return nil, err
	}
	return store.GetRun(ctx, e.pool, run.ID)
}

var _ engine.Service = intakeEngine{}

// seedIntakeProject stores a project with intake enabled on a workflow, a budget cap and a
// token the fake accepts.
func seedIntakeProject(t *testing.T, name string, wf *store.Workflow, enabled bool) *store.Project {
	t.Helper()
	c := testCipher(t)
	p := &store.Project{ID: store.NewID(), Name: name, RepoURL: "https://gitlab.test/group/" + name + ".git", Provider: "gitlab",
		DefaultBranch: "main", IntakeEnabled: enabled, IntakeLabel: "yuno", IntakeWorkflowID: &wf.ID, BudgetDayUSDMicros: usd(5)}
	blob, err := c.Seal(p.ID, acceptedTokenPrefix+uniqueSuffix())
	mustDo(t, err)
	mustDo(t, store.CreateProject(context.Background(), testPool, p, blob))
	t.Cleanup(func() { _ = store.DeleteProject(context.Background(), testPool, p.ID) })
	return p
}

func usd(v float64) *store.USDMicros {
	m := store.USDMicros(v * 1e6)
	return &m
}

// pollResult decodes the poll's answer; Error is nil when the member is absent.
type pollResult struct {
	Taken  int     `json:"taken"`
	Closed int     `json:"closed"`
	Error  *string `json:"error"`
}

func postPoll(t *testing.T, ts *httptest.Server, id string) (int, []byte, pollResult) {
	t.Helper()
	code, body := do(t, ts, http.MethodPost, "/api/projects/"+id+"/intake/poll", "")
	var res pollResult
	if code == http.StatusOK {
		mustDo(t, json.Unmarshal(body, &res))
	}
	return code, body, res
}

// T5.23.9 (the handler): the poll answers the counts, 409 intake_disabled when the intake
// is off, 404 for a missing or deleted project; a provider error is the result's `error`
// and the project's intake.last_error, not an HTTP error; the closer of the pass posts the
// outcome of a finished run.
func TestPollIntakeEndpointT5239(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	api, transport := newIntakeAPI(t)
	cfg := testConfig()
	cfg.PublicBaseURL = "https://yuno.test"
	_, ts := newTestServerDeps(t, Deps{Cfg: cfg, Pool: pool, CheckTransport: transport}, intakeEngine{&fakeEngine{pool: pool}})
	wf := goodIntakeWorkflow(t)
	sfx := uniqueSuffix()
	p := seedIntakeProject(t, "poll-"+sfx, wf, true)
	off := seedIntakeProject(t, "poll-off-"+sfx, wf, false)

	// 404 and 409.
	if code, body, _ := postPoll(t, ts, "no-such-project"); code != http.StatusNotFound || decodeError(t, body).Error.Code != "not_found" {
		t.Errorf("unknown project: %d %s", code, body)
	}
	code, body, _ := postPoll(t, ts, off.ID)
	if e := decodeError(t, body); code != http.StatusConflict || e.Error.Code != "intake_disabled" || e.Error.Message != "the project's intake is off" {
		t.Errorf("disabled intake: %d %s", code, body)
	}
	gone := seedIntakeProject(t, "poll-gone-"+sfx, wf, true)
	mustDo(t, store.DeleteProject(ctx, pool, gone.ID))
	if code, body, _ := postPoll(t, ts, gone.ID); code != http.StatusNotFound {
		t.Errorf("deleted project: %d %s", code, body)
	}

	// 200 with the counts: one labelled issue becomes a run, labelled and commented.
	api.labelled(7, "Add a flag", "Please.")
	code, body, res := postPoll(t, ts, p.ID)
	if code != http.StatusOK || res.Taken != 1 || res.Closed != 0 || res.Error != nil {
		t.Fatalf("poll: %d %s", code, body)
	}
	if strings.Contains(string(body), `"error"`) {
		t.Errorf("a clean pass has an error member: %s", body)
	}
	runs, err := store.ListRuns(ctx, pool, store.RunFilter{Trigger: store.TriggerIssue, ProjectID: p.ID})
	mustDo(t, err)
	if len(runs) != 1 || *runs[0].TriggerRef != p.ID+"#7" || runs[0].Status != store.RunQueued {
		t.Fatalf("runs = %+v", runs)
	}
	run := runs[0]
	var in struct{ Task string }
	mustDo(t, json.Unmarshal(run.Input, &in))
	if !strings.Contains(in.Task, "Title: Add a flag") || !strings.Contains(in.Task, `<untrusted_content source="issue #7">`) {
		t.Errorf("task = %q", in.Task)
	}
	if got := api.sentNotes(); len(got) != 1 || got[0] != "Yuno started run https://yuno.test/runs/"+run.ID+" for this issue." {
		t.Errorf("comments = %q", got)
	}
	_, body = do(t, ts, http.MethodGet, "/api/projects/"+p.ID, "")
	got := intakeOf(t, body)
	if got["polled_at"] == nil || got["last_error"] != nil || got["workflow_name"] != wf.Name {
		t.Errorf("intake after the poll = %v", got)
	}
	for _, tok := range api.tokens {
		if !strings.HasPrefix(tok, acceptedTokenPrefix) {
			t.Errorf("a call carried the token %q", tok)
		}
		noToken(t, "the poll answer", body, tok)
	}

	// The run ends: the next poll closes it.
	_, err = pool.Exec(ctx, `UPDATE runs SET status='succeeded', finished_at=now() WHERE id=$1`, run.ID)
	mustDo(t, err)
	code, body, res = postPoll(t, ts, p.ID)
	if code != http.StatusOK || res.Taken != 0 || res.Closed != 1 {
		t.Fatalf("closing poll: %d %s", code, body)
	}
	if notes := api.sentNotes(); len(notes) != 2 || notes[1] != "Yuno finished run https://yuno.test/runs/"+run.ID+"; it opened no merge request." {
		t.Errorf("comments = %q", notes)
	}

	// A provider error is a result, 200, and the project's last error.
	api.mu.Lock()
	api.failList = http.StatusInternalServerError
	api.mu.Unlock()
	code, body, res = postPoll(t, ts, p.ID)
	if code != http.StatusOK || res.Error == nil || !strings.Contains(*res.Error, "gitlab: HTTP 500") || res.Taken != 0 {
		t.Fatalf("provider error: %d %s", code, body)
	}
	_, body = do(t, ts, http.MethodGet, "/api/projects/"+p.ID, "")
	if le, _ := intakeOf(t, body)["last_error"].(string); le != *res.Error {
		t.Errorf("intake.last_error = %q, want %q", le, *res.Error)
	}
}

// T5.23.9: without an engine the poll takes nothing and says so, rather than failing.
func TestPollIntakeWithoutAnEngine(t *testing.T) {
	pool := needDB(t)
	api, transport := newIntakeAPI(t)
	_, ts := newTestServerDeps(t, Deps{Cfg: testConfig(), Pool: pool, CheckTransport: transport}, nil)
	p := seedIntakeProject(t, "poll-noengine-"+uniqueSuffix(), goodIntakeWorkflow(t), true)
	api.labelled(1, "a", "b")
	code, body, res := postPoll(t, ts, p.ID)
	if code != http.StatusOK || res.Taken != 0 || res.Error == nil || !strings.Contains(*res.Error, "engine") {
		t.Fatalf("poll without an engine: %d %s", code, body)
	}
}

// intakeConformanceSteps exercises the intake's paths and fields against the contract
// (DESIGN 5.23 rules 1 and 2): a project saved with `intake`, read back and listed, the
// 422 of each path, and the poll route's 200, 409 and 404. The coordinator appends the
// call to TestOpenAPIConformance.
func intakeConformanceSteps(t *testing.T) []conformanceStep {
	t.Helper()
	sfx := uniqueSuffix()
	wf := goodIntakeWorkflow(t)
	p := seedProject(t, testCipher(t), "confi-"+sfx, acceptedTokenPrefix+sfx)
	off := seedProject(t, testCipher(t), "confi-off-"+sfx, acceptedTokenPrefix+sfx)
	repo := `"repo_url":"` + p.RepoURL + `"`
	intake := `"intake":{"enabled":true,"label":"yuno","workflow_id":"` + wf.ID + `"}`
	return []conformanceStep{
		{http.MethodPost, "/api/projects", `{"name":"confi-new-` + sfx + `","repo_url":"https://gitlab.test/group/confi-new-` + sfx + `.git",
			"token":"` + acceptedTokenPrefix + sfx + `","max_cost_usd_per_day":5,` + intake + `}`, http.StatusCreated},
		{http.MethodPut, "/api/projects/" + p.ID, `{"name":"confi-` + sfx + `",` + repo + `,"provider":"gitlab","max_cost_usd_per_day":5,` + intake + `}`, http.StatusOK},
		{http.MethodGet, "/api/projects/" + p.ID, "", http.StatusOK},
		{http.MethodGet, "/api/projects", "", http.StatusOK},
		{http.MethodPut, "/api/projects/" + p.ID, `{"name":"confi-` + sfx + `",` + repo + `,"provider":"gitlab","max_cost_usd_per_day":5,"instructions":"keeps the intake"}`, http.StatusOK},
		{http.MethodPut, "/api/projects/" + p.ID, `{"name":"confi-` + sfx + `",` + repo + `,"provider":"gitlab","intake":{"enabled":true,"label":"a b","workflow_id":"` + wf.ID + `"}}`, http.StatusUnprocessableEntity},
		{http.MethodPut, "/api/projects/" + p.ID, `{"name":"confi-` + sfx + `",` + repo + `,"provider":"gitlab","intake":{"enabled":true,"workflow_id":"` + wf.ID + `"}}`, http.StatusUnprocessableEntity},
		{http.MethodPut, "/api/projects/" + p.ID, `{"name":"confi-` + sfx + `",` + repo + `,"provider":"gitlab","intake":{"enabled":false,"workflow_id":"no-such-workflow"}}`, http.StatusUnprocessableEntity},
		{http.MethodPut, "/api/projects/" + p.ID, `{"name":"confi-` + sfx + `",` + repo + `,"provider":"gitlab","max_cost_usd_per_day":5,` + intake + `}`, http.StatusOK},
		{http.MethodPost, "/api/projects/" + p.ID + "/intake/poll", "", http.StatusOK},
		{http.MethodPost, "/api/projects/" + off.ID + "/intake/poll", "", http.StatusConflict},
		{http.MethodPost, "/api/projects/no-such-project/intake/poll", "", http.StatusNotFound},
	}
}

// TestIntakeConformanceSteps runs the intake's conformance steps against the contract on a
// server of their own, with a fake provider that serves the issue API.
func TestIntakeConformanceSteps(t *testing.T) {
	pool := needDB(t)
	router := contract(t)
	_, transport := newIntakeAPI(t)
	_, ts := newTestServerDeps(t, Deps{Cfg: testConfig(), Pool: pool, CheckTransport: transport}, intakeEngine{&fakeEngine{pool: pool}})
	for _, st := range intakeConformanceSteps(t) {
		code, body := checkConformance(t, router, ts.URL, st.method, st.path, st.body, st.want < 300)
		if code != st.want {
			t.Fatalf("%s %s: status %d, want %d: %s", st.method, st.path, code, st.want, body)
		}
	}
}

// T5.23.1, review L5: the workflow's input_schema is checked against the task of 5.23 rule 4
// at its longest (the preamble, the title and a 16 KiB body), not against `{"task":"x"}`: a
// minLength above one is fine, a maxLength every real task would break is refused, and a
// schema message that quotes the whole value is cut.
func TestIntakeWorkflowInputIsCheckedAgainstARealTaskL5(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)

	var sample struct {
		Task string `json:"task"`
	}
	mustDo(t, json.Unmarshal(intakeTaskSample(), &sample))
	if !strings.HasPrefix(sample.Task, "Work on issue #1 of ") || !strings.Contains(sample.Task, "\n<untrusted_content source=\"issue #1\">\nTitle: ") ||
		!strings.HasSuffix(sample.Task, "\n</untrusted_content>") || !strings.Contains(sample.Task, strings.Repeat("x", 16<<10)) {
		t.Fatalf("the sample is not the task of rule 4 with a 16 KiB body: %.300s", sample.Task)
	}

	schema := func(keywords string) string {
		return `"project":true,"input_schema":{"type":"object","properties":{"task":{"type":"string"` + keywords + `}},"required":["task"]}`
	}
	save := func(wf *store.Workflow) (int, []byte) {
		return do(t, ts, http.MethodPost, "/api/projects", projectBody(uniqueSuffix(),
			`"max_cost_usd_per_day":5,"intake":{"enabled":true,"label":"yuno","workflow_id":`+jsonText(wf.ID)+`}`))
	}
	accept := func(what, keywords string) {
		t.Helper()
		wf := intakeWorkflow(t, "intake "+what, schema(keywords), nil)
		code, body := save(wf)
		if code != http.StatusCreated {
			t.Errorf("%s: %d %s, want 201", what, code, body)
			return
		}
		t.Cleanup(func() { _ = store.DeleteProject(context.Background(), pool, decodeProject(t, body).ID) })
	}
	refuse := func(what, keywords string) []string {
		t.Helper()
		wf := intakeWorkflow(t, "intake "+what, schema(keywords), nil)
		code, body := save(wf)
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: %d %s, want 422", what, code, body)
		}
		return fieldMessages(t, body)
	}

	accept("a minLength above one", `,"minLength":2000`)
	accept("a maxLength above any real task", `,"maxLength":100000`)
	accept("no keyword", ``)

	msgs := refuse("a maxLength a real task breaks", `,"maxLength":1000`)
	const prefix = `intake.workflow_id: the workflow's input must accept {"task": string}: input.task: `
	if len(msgs) != 1 || !strings.HasPrefix(msgs[0], prefix+"maxLength: got 16,") || !strings.HasSuffix(msgs[0], ", want 1,000") {
		t.Errorf("maxLength 1000: %q", msgs)
	}
	// A minLength no task reaches is refused the same way.
	if msgs = refuse("a minLength nothing reaches", `,"minLength":100000`); len(msgs) != 1 || !strings.HasPrefix(msgs[0], prefix) {
		t.Errorf("minLength 100000: %q", msgs)
	}
	// A pattern violation quotes the whole task: the detail is cut to a few hundred bytes
	// and keeps the pattern, which the operator needs.
	msgs = refuse("a pattern", `,"pattern":"^[a-z]+$"`)
	if len(msgs) != 1 || len(msgs[0]) > 600 || !strings.Contains(msgs[0], "…") || !strings.HasSuffix(msgs[0], "does not match pattern '^[a-z]+$'") {
		t.Errorf("pattern: %d bytes: %q", len(msgs[0]), msgs)
	}
}

func TestCutIntakeMessage(t *testing.T) {
	if got := cutIntakeMessage("short"); got != "short" {
		t.Errorf("short message changed: %q", got)
	}
	long := strings.Repeat("é", 400) // 800 bytes, every cut point a rune boundary or not
	for _, m := range []string{long, "x" + long, strings.Repeat("a", 301), strings.Repeat("a", 5000)} {
		got := cutIntakeMessage(m)
		if len(got) > maxIntakeMessageLen+len("…") || !utf8.ValidString(got) || !strings.Contains(got, "…") {
			t.Errorf("cut of %d bytes = %d bytes, valid %v", len(m), len(got), utf8.ValidString(got))
		}
	}
	if got := cutIntakeMessage(strings.Repeat("a", maxIntakeMessageLen)); len(got) != maxIntakeMessageLen {
		t.Errorf("a message of exactly the cap was cut: %d", len(got))
	}
}

// DESIGN 5.29 rule 10: a run Poll now starts for a plan ticket's issue is linked to the
// ticket, which leaves failed for building, as with the background loop. (Poll now used to
// build its own loop without the plan link, so a relabelled ticket stayed failed.)
func TestPollIntakeLinksThePlanTicket(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	api, transport := newIntakeAPI(t)
	_, ts := newTestServerDeps(t, Deps{Cfg: testConfig(), Pool: pool, CheckTransport: transport}, intakeEngine{&fakeEngine{pool: pool}})
	p := seedIntakeProject(t, "poll-plan-"+uniqueSuffix(), goodIntakeWorkflow(t), true)
	plan, tickets := seedPlan(t, p)
	moved, err := store.MoveTicket(ctx, pool, plan.ID, tickets[0].TicketID, "failed", "pending")
	mustDo(t, err)
	if !moved {
		t.Fatal("the ticket could not be set failed")
	}

	api.labelled(int(*tickets[0].IssueNumber), tickets[0].Title, "Relabelled after a failure.")
	if code, body, res := postPoll(t, ts, p.ID); code != http.StatusOK || res.Taken != 1 {
		t.Fatalf("poll: %d %s", code, body)
	}
	runs, err := store.ListRuns(ctx, pool, store.RunFilter{Trigger: store.TriggerIssue, ProjectID: p.ID})
	mustDo(t, err)
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	got, err := store.TicketByIssue(ctx, pool, p.ID, *tickets[0].IssueNumber)
	mustDo(t, err)
	if got.Status != "building" || got.RunID == nil || *got.RunID != runs[0].ID {
		t.Fatalf("ticket after the poll: status %s run %v, want building on %s", got.Status, got.RunID, runs[0].ID)
	}
}
