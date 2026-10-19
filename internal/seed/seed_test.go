package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/config"
	"yuno/internal/httpapi"
	"yuno/internal/store"
	"yuno/internal/tools"
	"yuno/internal/workflow"
)

// The seed's tests run in their own schema of the test database: they bind the one
// Telegram channel and count workflows, which other packages' tests must not see.
const testSchema = "yuno_seed_test"

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		if err := setupDB(url); err != nil {
			fmt.Fprintln(os.Stderr, "seed tests: database setup failed:", err)
			os.Exit(1)
		}
	}
	code := m.Run()
	if testPool != nil {
		testPool.Close()
	}
	os.Exit(code)
}

func setupDB(url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	base, err := store.Open(ctx, url)
	if err != nil {
		return err
	}
	_, err = base.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+testSchema)
	base.Close()
	if err != nil {
		return err
	}
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	if testPool, err = store.Open(ctx, url+sep+"search_path="+testSchema); err != nil {
		return err
	}
	return store.Migrate(ctx, testPool, slog.New(slog.DiscardHandler))
}

// needDB skips without a database and empties the seed's tables otherwise.
func needDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL not set")
	}
	if _, err := testPool.Exec(context.Background(), `TRUNCATE agents, workflows, runs, channel_bindings, conversations CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return testPool
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Seeding twice creates each demo agent once and leaves existing agents alone.
func TestSeedIsIdempotent(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	created, existing, err := Seed(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != len(Agents) || len(existing) != 0 {
		t.Fatalf("first seed: created %v existing %v", created, existing)
	}
	created, existing, err = Seed(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 || len(existing) != len(Agents) {
		t.Fatalf("second seed: created %v existing %v", created, existing)
	}
	agents, err := store.ListAgents(ctx, pool)
	must(t, err)
	if len(agents) != len(Agents) {
		t.Fatalf("agents = %d, want %d", len(agents), len(Agents))
	}
}

// DESIGN 5.15 rule 12: the Chief of Staff is seeded with the coordinator's role, model,
// the chat tools and no exec, memory on, and the coordinator prompt.
func TestSeedCreatesChief(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	_, _, err := Seed(ctx, pool)
	must(t, err)
	chief, err := findAgent(ctx, pool, ChiefName)
	must(t, err)
	if chief == nil || chief.Name != "Chief of Staff" || chief.Role != "Coordinator" || chief.Model != "claude-opus-5-5" ||
		!chief.MemoryPolicy.Enabled || !strings.HasPrefix(chief.ChatToken, "c_") {
		t.Fatalf("chief = %+v", chief)
	}
	want := []string{"answer_question", "delegate_task", "list_agents", "list_projects", "list_workflows", "memory_recall", "memory_save", "read_merge_request", "run_status"}
	got := slices.Sorted(slices.Values(chief.ToolAllowlist))
	if !slices.Equal(got, want) || slices.Contains(chief.ToolAllowlist, "exec") {
		t.Fatalf("chief tools = %v", chief.ToolAllowlist)
	}
	for _, tool := range chief.ToolAllowlist {
		if !tools.IsChatTool(tool) {
			t.Errorf("%s is not chat-capable", tool)
		}
	}
	for _, phrase := range []string{"only agent the person talks to", "Never do the work yourself", "list_agents and list_workflows",
		"complete, self-contained task", "the run id", "[platform]", "one or two sentences"} {
		if !strings.Contains(chief.SystemPrompt, phrase) {
			t.Errorf("prompt lacks %q", phrase)
		}
	}
}

// stubTemplates writes the three shipped templates' keys and names with one-node
// definitions into a directory, so these tests do not depend on the template files'
// content (T5.28.3 covers those).
func stubTemplates(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for key, tpl := range map[string][2]string{"plan_pipeline": {"Plan pipeline", "Analyst"}, "wave_pipeline": {"Wave pipeline", "Builder"}, "ship_pipeline": {"Ship pipeline", "Integrator"}} {
		def := `{"input_schema":{"type":"object","properties":{"task":{"type":"string"}},"required":["task"]},` +
			`"state_schema":{"type":"object"},"nodes":[{"id":"start","kind":"start"},` +
			`{"id":"work","kind":"agent","agent_name":"` + tpl[1] + `","task":"{{.input.task}}","output_schema":{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]},"input_from":["start"]},` +
			`{"id":"end","kind":"end"}],"edges":[{"from":"start","to":"work"},{"from":"work","to":"end"}],` +
			`"limits":{"max_steps":10,"max_duration_seconds":600,"max_node_iterations":5,"max_loop_visits":3,"max_cost_usd":1,"max_tool_calls":20,"approval_timeout_seconds":3600}}`
		raw := `{"key":"` + key + `","name":"` + tpl[0] + `","description":"d","definition":` + def + `}`
		must(t, os.WriteFile(filepath.Join(dir, key+".json"), []byte(raw), 0o644))
	}
	return dir
}

func loadStubTemplates(t *testing.T, pool *pgxpool.Pool) *httpapi.Shared {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	srv, err := httpapi.New(httpapi.Deps{Cfg: config.Config{AnthropicBaseURL: "http://127.0.0.1:1"}, Pool: pool, Log: log,
		TemplatesDir: stubTemplates(t)})
	must(t, err)
	must(t, srv.LoadTemplates(context.Background()))
	return httpapi.NewShared(httpapi.Deps{Pool: pool, Log: log})
}

// T5.28.1 (DESIGN 5.28 rule 1): the seed creates the eight factory agents with their
// models and tools, memory on for Scout and Builder only, none of the retired nine, and
// leaves the Chief of Staff as it was and bound.
func TestSeedCreatesFactoryAgents(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	created, _, err := Seed(ctx, pool)
	must(t, err)
	want := map[string]struct {
		model string
		tools []string
		mem   bool
	}{
		"Analyst":       {"claude-opus-5-5", []string{"ask_operator", "read", "ls", "write", "escalate", "finish"}, false},
		"Scout":         {"claude-sonnet-5-5", []string{"read", "ls", "exec", "memory_save", "memory_recall", "escalate", "finish"}, true},
		"Architect":     {"claude-opus-5-5", []string{"read", "ls", "write", "edit", "ask_operator", "escalate", "finish"}, false},
		"Planner":       {"claude-opus-5-5", []string{"read", "ls", "write", "edit", "escalate", "finish"}, false},
		"Builder":       {"claude-sonnet-5-5", []string{"read", "ls", "write", "edit", "exec", "ask_operator", "memory_save", "memory_recall", "escalate", "finish"}, true},
		"Code Reviewer": {"claude-opus-5-5", []string{"read", "ls", "exec", "read_merge_request", "escalate", "finish"}, false},
		"Doc Auditor":   {"claude-opus-5-5", []string{"read", "ls", "write", "edit", "exec", "escalate", "finish"}, false},
		"Integrator":    {"claude-sonnet-5-5", []string{"open_merge_request", "publish_tickets", "advance_wave", "read", "ls", "memory_save", "escalate", "finish"}, true},
	}
	if len(created) != len(want)+1 {
		t.Fatalf("created %v", created)
	}
	for name, w := range want {
		a, err := findAgent(ctx, pool, name)
		must(t, err)
		if a == nil {
			t.Fatalf("no agent %s", name)
		}
		got := slices.Sorted(slices.Values(a.ToolAllowlist))
		if a.Model != w.model || !slices.Equal(got, slices.Sorted(slices.Values(w.tools))) || a.MemoryPolicy.Enabled != w.mem {
			t.Errorf("%s = %s %v memory %v, want %s %v memory %v", name, a.Model, a.ToolAllowlist, a.MemoryPolicy.Enabled, w.model, w.tools, w.mem)
		}
		for _, tool := range a.ToolAllowlist {
			if tools.IsChatTool(tool) && tool != "memory_save" && tool != "memory_recall" && tool != "read_merge_request" {
				t.Errorf("%s holds the chat tool %s", name, tool)
			}
		}
		if a.Role == "" || len(a.SystemPrompt) < 400 || !strings.Contains(a.SystemPrompt, "output schema") {
			t.Errorf("%s: role %q, prompt of %d bytes without the output schema sentence", name, a.Role, len(a.SystemPrompt))
		}
		if slices.Contains(a.ToolAllowlist, "exec") != slices.Contains([]string{"Scout", "Builder", "Code Reviewer", "Doc Auditor"}, name) {
			t.Errorf("%s: exec = %v", name, slices.Contains(a.ToolAllowlist, "exec"))
		}
	}
	for _, name := range RetiredAgentNames {
		if a, err := findAgent(ctx, pool, name); err != nil || a != nil {
			t.Errorf("seed created the retired %s (%v)", name, err)
		}
	}
	for _, phrase := range []string{"docs/plans/<feature>/"} {
		for _, name := range []string{"Analyst", "Architect", "Planner", "Doc Auditor"} {
			a, _ := findAgent(ctx, pool, name)
			if !strings.Contains(a.SystemPrompt, phrase) {
				t.Errorf("%s prompt lacks %q", name, phrase)
			}
		}
	}
	builder, _ := findAgent(ctx, pool, "Builder")
	reviewer, _ := findAgent(ctx, pool, "Code Reviewer")
	auditor, _ := findAgent(ctx, pool, "Doc Auditor")
	// escalate is granted to every factory agent (Scout and Integrator included, review L7),
	// publish_tickets to the Integrator only.
	for _, name := range FactoryAgentNames {
		a, _ := findAgent(ctx, pool, name)
		if !slices.Contains(a.ToolAllowlist, "escalate") {
			t.Errorf("%s: escalate is not granted", name)
		}
		if slices.Contains(a.ToolAllowlist, "publish_tickets") != (name == "Integrator") {
			t.Errorf("%s: publish_tickets granted wrongly", name)
		}
		if slices.Contains(a.ToolAllowlist, "advance_wave") != (name == "Integrator") {
			t.Errorf("%s: advance_wave granted wrongly", name)
		}
		if !strings.Contains(a.SystemPrompt, "escalate") {
			t.Errorf("%s prompt does not say when to escalate", name)
		}
	}
	integrator, _ := findAgent(ctx, pool, "Integrator")
	if !strings.Contains(integrator.SystemPrompt, "publish_tickets") || !strings.Contains(integrator.SystemPrompt, "draft true") ||
		!strings.Contains(integrator.SystemPrompt, "draft false") {
		t.Error("the Integrator prompt lost its publish or draft steps")
	}
	if !strings.Contains(integrator.SystemPrompt, "advance_wave exactly once") || !strings.Contains(integrator.SystemPrompt, "after the smoke test passed") {
		t.Error("the Integrator prompt does not say when to advance the wave")
	}
	if !strings.Contains(builder.SystemPrompt, "failing test") || !strings.Contains(reviewer.SystemPrompt, "never rewrite") ||
		!strings.Contains(auditor.SystemPrompt, "never patch around an unmet requirement") {
		t.Error("the Builder, Reviewer or Auditor prompt lost its rule")
	}

	// The Chief is unchanged and a binding to it survives a second seed.
	chief, err := findAgent(ctx, pool, ChiefName)
	must(t, err)
	if chief.SystemPrompt != ChiefPrompt || chief.Model != "claude-opus-5-5" || !chief.MemoryPolicy.Enabled {
		t.Fatalf("chief = %+v", chief)
	}
	b := &fakeBinder{}
	bound, err := BindChief(ctx, pool, b)
	must(t, err)
	if !bound || !slices.Equal(b.bound, []string{chief.ID}) {
		t.Fatalf("the Chief was not bound: %v %v", bound, b.bound)
	}
}

// T5.28.2 (DESIGN 5.28 rule 2): the seed deletes the nine retired agents by exact name,
// through the delete path (the runtime is told), keeps one a workflow other than a copy of
// a retired template still uses, keeps one bound to a channel, never touches the Chief or
// an agent of another name, and is a no-op the second time.
func TestRetireAgents(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	_, _, err := Seed(ctx, pool)
	must(t, err)
	mk := func(name string) *store.Agent {
		a := &store.Agent{Name: name, Role: "r", SystemPrompt: "p", Model: "claude-sonnet-5-5", ToolAllowlist: []string{"read", "finish"}}
		must(t, store.CreateAgent(ctx, pool, a))
		return a
	}
	ids := map[string]string{}
	for _, name := range RetiredAgentNames {
		ids[name] = mk(name).ID
	}
	own := mk("Own agent")
	lookalike := mk("Coder 2")

	// A custom workflow uses the Reviewer; a copy of dev_pipeline uses the Coder and
	// the Deployer; the Critic answers a channel.
	node := func(id string) string {
		return `{"nodes":[{"id":"start","kind":"start"},{"id":"a","kind":"agent","agent_id":"` + id + `","task":"t"},{"id":"end","kind":"end"}],"edges":[{"from":"start","to":"a"},{"from":"a","to":"end"}]}`
	}
	custom := &store.Workflow{Name: "Our review", Definition: json.RawMessage(node(ids["Reviewer"]))}
	must(t, store.CreateWorkflow(ctx, pool, custom))
	dev := "dev_pipeline"
	copyWf := &store.Workflow{Name: "Dev pipeline", Definition: json.RawMessage(node(ids["Coder"])), FromTemplate: &dev}
	must(t, store.CreateWorkflow(ctx, pool, copyWf))
	_, err = store.BindChannel(ctx, pool, store.ChannelTelegram, ids["Critic"])
	must(t, err)
	chief, _ := findAgent(ctx, pool, ChiefName)

	gone, err := RetireWorkflows(ctx, pool, log)
	must(t, err)
	if !slices.Equal(gone, []string{"Dev pipeline"}) {
		t.Fatalf("workflows deleted: %v", gone)
	}
	rm := &fakeRemover{}
	deleted, err := RetireAgents(ctx, pool, rm, log)
	must(t, err)
	wantGone := slices.DeleteFunc(slices.Clone(RetiredAgentNames), func(n string) bool { return n == "Reviewer" || n == "Critic" })
	if !slices.Equal(slices.Sorted(slices.Values(deleted)), slices.Sorted(slices.Values(wantGone))) {
		t.Fatalf("deleted %v, want %v", deleted, wantGone)
	}
	if len(rm.removed) != len(wantGone) || slices.Contains(rm.removed, ids["Reviewer"]) || slices.Contains(rm.removed, chief.ID) {
		t.Fatalf("removed from the runtime: %v", rm.removed)
	}
	live, err := store.ListAgents(ctx, pool)
	must(t, err)
	names := map[string]bool{}
	for _, a := range live {
		names[a.Name] = true
	}
	for _, name := range []string{"Reviewer", "Critic", ChiefName, own.Name, lookalike.Name, "Analyst", "Builder"} {
		if !names[name] {
			t.Errorf("%s is gone", name)
		}
	}
	for _, name := range wantGone {
		if names[name] {
			t.Errorf("%s survived", name)
		}
	}
	if wf, err := store.GetWorkflow(ctx, pool, custom.ID); err != nil || wf == nil {
		t.Errorf("the custom workflow is gone: %v", err)
	}

	// Again: nothing left to do.
	rm = &fakeRemover{}
	deleted, err = RetireAgents(ctx, pool, rm, log)
	must(t, err)
	if len(deleted) != 0 || len(rm.removed) != 0 {
		t.Fatalf("second pass: %v %v", deleted, rm.removed)
	}
	// The workflow that used the Reviewer goes; the agent follows on the next seed.
	must(t, store.DeleteWorkflow(ctx, pool, custom.ID))
	_, err = store.UnbindChannel(ctx, pool, store.ChannelTelegram)
	must(t, err)
	deleted, err = RetireAgents(ctx, pool, nil, log)
	must(t, err)
	if !slices.Equal(slices.Sorted(slices.Values(deleted)), []string{"Critic", "Reviewer"}) {
		t.Fatalf("third pass: %v", deleted)
	}
}

// fakeRemover records the agents taken out of the runtime.
type fakeRemover struct{ removed []string }

func (f *fakeRemover) RemoveAgent(_ context.Context, id string) error {
	f.removed = append(f.removed, id)
	return nil
}

// T5.28.2 (DESIGN 5.28 rule 2): the shipped workflows of the retired templates are
// deleted, the template rows too; one with a run in flight stays; the new templates'
// workflows and others are untouched.
func TestRetireWorkflows(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	def := json.RawMessage(`{"nodes":[],"edges":[]}`)
	mk := func(name string, tplKey, from string) *store.Workflow {
		w := &store.Workflow{Name: name, Definition: def}
		if tplKey != "" {
			w.TemplateKey = &tplKey
		}
		if from != "" {
			w.FromTemplate = &from
		}
		must(t, store.CreateWorkflow(ctx, pool, w))
		return w
	}
	mk("Dev pipeline", "dev_pipeline", "")
	mk("Research pipeline", "", "research_pipeline")
	busy := mk("Review panel", "", "review_panel")
	mk("Plan pipeline", "plan_pipeline", "")
	mk("Plan pipeline copy", "", "plan_pipeline")
	mk("Mine", "", "")
	mk("Dev pipeline by hand", "", "")
	must(t, store.CreateRun(ctx, pool, &store.Run{WorkflowID: busy.ID, WorkflowVersion: 1, Status: store.RunRunning, Input: json.RawMessage(`{}`), State: json.RawMessage(`{}`), WorkflowSnapshot: def}))

	gone, err := RetireWorkflows(ctx, pool, log)
	must(t, err)
	if !slices.Equal(gone, []string{"Dev pipeline", "Research pipeline"}) {
		t.Fatalf("deleted %v", gone)
	}
	left, err := store.ListWorkflows(ctx, pool)
	must(t, err)
	var names []string
	for _, w := range left {
		names = append(names, w.Name)
	}
	if !slices.Equal(names, []string{"Review panel", "Plan pipeline", "Plan pipeline copy", "Mine", "Dev pipeline by hand"}) {
		t.Fatalf("left %v", names)
	}
}

// T5.28.1 and DESIGN 5.28 rule 2: the seed instantiates the plan and wave pipelines once,
// through the API's own instantiation code, with their agents resolved; a workflow made
// from a template by hand counts; a template that is not loaded yet is skipped.
func TestSeedInstantiatesTemplatesOnce(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	_, _, err := Seed(ctx, pool)
	must(t, err)
	sh := httpapi.NewShared(httpapi.Deps{Pool: pool, Log: log})

	made, _, err := Templates(ctx, pool, sh, log)
	if err != nil || len(made) != 0 {
		t.Fatalf("without templates: %v %v", made, err)
	}
	sh = loadStubTemplates(t, pool)
	made, _, err = Templates(ctx, pool, sh, log)
	if err != nil || !slices.Equal(made, []string{"Plan pipeline", "Wave pipeline", "Ship pipeline"}) {
		t.Fatalf("first: %v %v", made, err)
	}
	made, _, err = Templates(ctx, pool, sh, log)
	if err != nil || len(made) != 0 {
		t.Fatalf("second: %v %v", made, err)
	}
	all, err := store.ListWorkflows(ctx, pool)
	must(t, err)
	instances := 0
	for _, wf := range all {
		if wf.FromTemplate == nil {
			continue
		}
		instances++
		if wf.TemplateKey != nil || strings.Contains(string(wf.Definition), "agent_name") || !strings.Contains(string(wf.Definition), "agent_id") {
			t.Fatalf("instance %s = %s", wf.Name, wf.Definition)
		}
	}
	if instances != 3 {
		t.Fatalf("instances = %d", instances)
	}
	for _, wf := range all {
		if wf.FromTemplate != nil && *wf.FromTemplate == "wave_pipeline" {
			must(t, store.DeleteWorkflow(ctx, pool, wf.ID))
		}
	}
	made, _, err = Templates(ctx, pool, sh, log)
	if err != nil || !slices.Equal(made, []string{"Wave pipeline"}) {
		t.Fatalf("after a delete: %v %v", made, err)
	}
}

// fakeBinder records the agents it was asked to bind.
type fakeBinder struct {
	bound []string
	err   error
}

func (b *fakeBinder) BindTelegram(_ context.Context, agentID string) error {
	b.bound = append(b.bound, agentID)
	return b.err
}

// DESIGN 5.15 rule 14: the Chief of Staff is bound when nothing is; an existing binding,
// to the chief or anyone else, is never changed.
func TestBindChief(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()

	b := &fakeBinder{}
	if bound, err := BindChief(ctx, pool, b); err != nil || bound || len(b.bound) != 0 {
		t.Fatalf("no chief: %v %v %v", bound, err, b.bound)
	}
	_, _, err := Seed(ctx, pool)
	must(t, err)
	chief, err := findAgent(ctx, pool, ChiefName)
	must(t, err)
	if bound, err := BindChief(ctx, pool, b); err != nil || !bound || !slices.Equal(b.bound, []string{chief.ID}) {
		t.Fatalf("unbound: %v %v %v", bound, err, b.bound)
	}

	builder, err := findAgent(ctx, pool, "Builder")
	must(t, err)
	_, err = store.BindChannel(ctx, pool, store.ChannelTelegram, builder.ID)
	must(t, err)
	b = &fakeBinder{}
	if bound, err := BindChief(ctx, pool, b); err != nil || bound || len(b.bound) != 0 {
		t.Fatalf("bound to another agent: %v %v %v", bound, err, b.bound)
	}
	_, err = store.UnbindChannel(ctx, pool, store.ChannelTelegram)
	must(t, err)
	_, err = store.BindChannel(ctx, pool, store.ChannelTelegram, chief.ID)
	must(t, err)
	if bound, err := BindChief(ctx, pool, b); err != nil || bound || len(b.bound) != 0 {
		t.Fatalf("bound to the chief: %v %v %v", bound, err, b.bound)
	}

	_, err = store.UnbindChannel(ctx, pool, store.ChannelTelegram)
	must(t, err)
	b = &fakeBinder{err: errors.New("gateway away")}
	if bound, err := BindChief(ctx, pool, b); err == nil || bound {
		t.Fatalf("bind failure: %v %v", bound, err)
	}
}

// failingInstantiator fails the keys it names and records every key it was asked for.
type failingInstantiator struct {
	Resetter
	in    Instantiator
	fail  map[string]bool
	asked []string
}

func (f *failingInstantiator) InstantiateTemplate(ctx context.Context, key, name string) (*store.Workflow, error) {
	f.asked = append(f.asked, key)
	if f.fail[key] {
		return nil, errors.New("agent missing")
	}
	return f.in.InstantiateTemplate(ctx, key, name)
}

// A copy made before workflows recorded their template (same name, no from_template)
// counts as instantiated; a failing template does not stop the other, and each failure
// is reported.
func TestSeedTemplatesUpgradeAndFailures(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	_, _, err := Seed(ctx, pool)
	must(t, err)
	sh := loadStubTemplates(t, pool)

	must(t, store.CreateWorkflow(ctx, pool, &store.Workflow{Name: "Plan pipeline", Definition: []byte(`{"nodes":[],"edges":[]}`)}))
	both := &failingInstantiator{Resetter: sh, in: sh, fail: map[string]bool{"plan_pipeline": true, "wave_pipeline": true, "ship_pipeline": true}}
	made, _, err := Templates(ctx, pool, both, log)
	if len(made) != 0 || err == nil || !strings.Contains(err.Error(), "template wave_pipeline: agent missing") ||
		strings.Contains(err.Error(), "plan_pipeline") || !slices.Equal(both.asked, []string{"wave_pipeline", "ship_pipeline"}) {
		t.Fatalf("old copy and a failure: %v %v, asked %v", made, err, both.asked)
	}

	must(t, store.DeleteWorkflow(ctx, pool, mustWorkflowNamed(t, pool, "Plan pipeline")))
	one := &failingInstantiator{Resetter: sh, in: sh, fail: map[string]bool{"plan_pipeline": true}}
	made, _, err = Templates(ctx, pool, one, log)
	if !slices.Equal(made, []string{"Wave pipeline", "Ship pipeline"}) || err == nil || !strings.Contains(err.Error(), "plan_pipeline") || len(one.asked) != 3 {
		t.Fatalf("first fails, the other works: %v %v", made, err)
	}
}

// mustWorkflowNamed returns the id of the live non-template workflow with the name.
func mustWorkflowNamed(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	all, err := store.ListWorkflows(context.Background(), pool)
	must(t, err)
	for _, wf := range all {
		if wf.TemplateKey == nil && wf.Name == name {
			return wf.ID
		}
	}
	t.Fatalf("no workflow named %s", name)
	return ""
}

// DESIGN 5.16 rule 14 with 5.28 rule 2: the reset restores the shipped agents' role,
// prompt and allowlist and the template workflows' definitions, keeps everything else,
// leaves other agents and workflows alone, and removes the retired templates' copies.
func TestResetShipped(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	_, _, err := Seed(ctx, pool)
	must(t, err)
	sh := loadStubTemplates(t, pool)
	_, _, err = Templates(ctx, pool, sh, log)
	must(t, err)

	// An operator edited the Integrator (an older prompt, no open_merge_request, a model
	// of their own) and the Plan pipeline (no project flag), and made an agent of their own.
	integrator, err := findAgent(ctx, pool, "Integrator")
	must(t, err)
	integrator.SystemPrompt, integrator.ToolAllowlist, integrator.Model = "old prompt", []string{"read", "ls", "finish"}, "claude-haiku-5-5"
	integrator.Limits.MaxCostUSD = 0.25
	must(t, store.UpdateAgent(ctx, pool, integrator))
	own := &store.Agent{Name: "Own agent", Role: "mine", SystemPrompt: "mine", Model: "claude-sonnet-5-5", ToolAllowlist: []string{"read", "finish"}}
	must(t, store.CreateAgent(ctx, pool, own))
	planID := mustWorkflowNamed(t, pool, "Plan pipeline")
	plan, err := store.GetWorkflow(ctx, pool, planID)
	must(t, err)
	var def map[string]any
	must(t, json.Unmarshal(plan.Definition, &def))
	def["project"] = false
	plan.Definition, _ = json.Marshal(def)
	plan.Name = "Plan pipeline (ours)"
	must(t, store.UpdateWorkflow(ctx, pool, plan))

	agents, workflows, err := ResetShipped(ctx, pool, sh)
	must(t, err)
	if !slices.Equal(agents, []string{"Integrator"}) || !slices.Equal(workflows, []string{"Plan pipeline (ours)", "Wave pipeline", "Ship pipeline"}) {
		t.Fatalf("reset %v %v", agents, workflows)
	}
	integrator, err = findAgent(ctx, pool, "Integrator")
	must(t, err)
	if integrator.SystemPrompt != shippedPrompt(t, "Integrator") || !slices.Contains(integrator.ToolAllowlist, tools.OpenMergeRequestName) {
		t.Fatalf("integrator not reset: %q %v", integrator.SystemPrompt, integrator.ToolAllowlist)
	}
	if integrator.Model != "claude-haiku-5-5" || integrator.Limits.MaxCostUSD != 0.25 || integrator.MaterializedHash != "" {
		t.Fatalf("integrator's own settings changed: model %s limits %+v hash %q", integrator.Model, integrator.Limits, integrator.MaterializedHash)
	}
	got, err := store.GetAgent(ctx, pool, own.ID)
	must(t, err)
	if got.SystemPrompt != "mine" {
		t.Fatal("an agent that is not shipped was changed")
	}
	plan, err = store.GetWorkflow(ctx, pool, planID)
	must(t, err)
	must(t, json.Unmarshal(plan.Definition, &def))
	if plan.Name != "Plan pipeline (ours)" || plan.Version != 3 || strings.Contains(string(plan.Definition), "agent_name") {
		t.Fatalf("plan pipeline after the reset: name %q version %d", plan.Name, plan.Version)
	}
	// A second reset changes nothing on the agents and rewrites the workflows (a new
	// version each), which is what a deploy does on purpose.
	agents, workflows, err = ResetShipped(ctx, pool, sh)
	must(t, err)
	if len(agents) != 0 || len(workflows) != 3 {
		t.Fatalf("second reset %v %v", agents, workflows)
	}
}

func shippedPrompt(t *testing.T, name string) string {
	t.Helper()
	for _, a := range Agents {
		if a.Name == name {
			return a.SystemPrompt
		}
	}
	t.Fatalf("no shipped agent %s", name)
	return ""
}

// T5.28.3 support (DESIGN 5.28 rule 2): the retired templates stay valid definitions in
// templates/legacy, which the loader's non-recursive glob does not read.
func TestLegacyTemplatesAreValidAndNotLoaded(t *testing.T) {
	files, err := filepath.Glob("../../templates/legacy/*.json")
	must(t, err)
	var keys []string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		must(t, err)
		var tpl struct {
			Key        string          `json:"key"`
			Definition json.RawMessage `json:"definition"`
		}
		must(t, json.Unmarshal(raw, &tpl))
		def, err := workflow.Parse(tpl.Definition)
		must(t, err)
		if err := def.ValidateStructure(); err != nil {
			t.Errorf("%s: %v", f, err)
		}
		keys = append(keys, tpl.Key)
	}
	if !slices.Equal(slices.Sorted(slices.Values(keys)), slices.Sorted(slices.Values(RetiredTemplateKeys))) {
		t.Fatalf("legacy keys %v, want %v", keys, RetiredTemplateKeys)
	}
	top, err := filepath.Glob("../../templates/*.json")
	must(t, err)
	for _, f := range top {
		for _, k := range RetiredTemplateKeys {
			if strings.HasPrefix(filepath.Base(f), k) {
				t.Errorf("%s is still in the loaded directory", f)
			}
		}
	}
}

// H1, H2, L2 of the wave 1 review: a copy of a retired template the operator renamed or
// edited stays, so does one an intake setting or an enabled schedule points at (a
// disabled schedule does not count), each with a log line saying why; the untouched
// copies go, with their ids logged.
func TestRetireWorkflowsKeepsWhatIsUsedOrChanged(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	def := json.RawMessage(`{"nodes":[],"edges":[]}`)
	mk := func(name, from string) *store.Workflow {
		w := &store.Workflow{Name: name, Definition: def, FromTemplate: &from}
		must(t, store.CreateWorkflow(ctx, pool, w))
		return w
	}
	mk("Dev pipeline", "dev_pipeline") // untouched
	renamed := mk("My dev flow", "dev_pipeline")
	edited := mk("Research pipeline", "research_pipeline")
	must(t, store.UpdateWorkflow(ctx, pool, edited))
	intake := mk("Review panel", "review_panel")
	scheduled := mk("Research pipeline", "research_pipeline")
	disabled := mk("Research pipeline", "research_pipeline")

	p := &store.Project{Name: "demo", RepoURL: "https://gitlab.test/g/d.git", Provider: "gitlab", DefaultBranch: "main", IntakeWorkflowID: &intake.ID}
	must(t, store.CreateProject(ctx, pool, p, nil))
	cron := "0 * * * *"
	sched := &store.Schedule{WorkflowID: scheduled.ID, Kind: "cron", CronExpr: &cron, TZ: "UTC", Enabled: true}
	must(t, store.CreateSchedule(ctx, pool, sched))
	must(t, store.CreateSchedule(ctx, pool, &store.Schedule{WorkflowID: disabled.ID, Kind: "cron", CronExpr: &cron, TZ: "UTC", Enabled: false}))

	gone, err := RetireWorkflows(ctx, pool, log)
	must(t, err)
	if !slices.Equal(gone, []string{"Dev pipeline", "Research pipeline"}) {
		t.Fatalf("deleted %v", gone)
	}
	left, err := store.ListWorkflows(ctx, pool)
	must(t, err)
	var ids []string
	for _, w := range left {
		ids = append(ids, w.ID)
	}
	for _, w := range []*store.Workflow{renamed, edited, intake, scheduled} {
		if !slices.Contains(ids, w.ID) {
			t.Errorf("%s (%s) was deleted", w.Name, w.ID)
		}
	}
	if slices.Contains(ids, disabled.ID) {
		t.Error("a workflow only a disabled schedule points at was kept")
	}
	out := buf.String()
	for _, want := range []string{"referenced by intake on project demo", "referenced by schedule " + sched.ID, "the operator changed it", "retired workflow deleted"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

// M1 of the review: a retired agent with a node visit in flight is kept and logged; the
// next seed, with the run over, deletes it. A deleted agent is logged with its id.
func TestRetireAgentsKeepsBusyAgent(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	mk := func(name string) *store.Agent {
		a := &store.Agent{Name: name, Role: "r", SystemPrompt: "p", Model: "claude-sonnet-5-5", ToolAllowlist: []string{"finish"}}
		must(t, store.CreateAgent(ctx, pool, a))
		return a
	}
	busy, idle := mk("Coder"), mk("Writer")
	def := json.RawMessage(`{"nodes":[],"edges":[]}`)
	wf := &store.Workflow{Name: "Quick", Definition: def}
	must(t, store.CreateWorkflow(ctx, pool, wf))
	run := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, Status: store.RunRunning, Input: json.RawMessage(`{}`), State: json.RawMessage(`{}`), WorkflowSnapshot: def}
	must(t, store.CreateRun(ctx, pool, run))
	must(t, store.CreateRunNode(ctx, pool, &store.RunNode{RunID: run.ID, NodeID: "a", AgentID: &busy.ID, Status: store.NodeRunning}))

	rm := &fakeRemover{}
	deleted, err := RetireAgents(ctx, pool, rm, log)
	must(t, err)
	if !slices.Equal(deleted, []string{"Writer"}) || !slices.Equal(rm.removed, []string{idle.ID}) {
		t.Fatalf("deleted %v removed %v", deleted, rm.removed)
	}
	out := buf.String()
	if !strings.Contains(out, "it has a run in flight") || !strings.Contains(out, "id="+idle.ID) {
		t.Errorf("log:\n%s", out)
	}
	_, err = pool.Exec(ctx, `UPDATE runs SET status = $2 WHERE id = $1`, run.ID, store.RunSucceeded)
	must(t, err)
	deleted, err = RetireAgents(ctx, pool, nil, log)
	must(t, err)
	if !slices.Equal(deleted, []string{"Coder"}) {
		t.Fatalf("after the run: %v", deleted)
	}
}

// L1 of the review: the delete is one statement, so a run that appears (or an edit that
// lands) after the caller looked is not lost.
func TestDeleteWorkflowIfIdle(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	def := json.RawMessage(`{"nodes":[],"edges":[]}`)
	wf := &store.Workflow{Name: "W", Definition: def}
	must(t, store.CreateWorkflow(ctx, pool, wf))
	must(t, store.CreateRun(ctx, pool, &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, Status: store.RunQueued, Input: json.RawMessage(`{}`), State: json.RawMessage(`{}`), WorkflowSnapshot: def}))
	if ok, err := store.DeleteWorkflowIfIdle(ctx, pool, wf.ID, 1); err != nil || ok {
		t.Fatalf("with a queued run: %v %v", ok, err)
	}
	_, err := pool.Exec(ctx, `UPDATE runs SET status = $1`, store.RunSucceeded)
	must(t, err)
	if ok, err := store.DeleteWorkflowIfIdle(ctx, pool, wf.ID, 2); err != nil || ok {
		t.Fatalf("at a stale version: %v %v", ok, err)
	}
	if ok, err := store.DeleteWorkflowIfIdle(ctx, pool, wf.ID, 1); err != nil || !ok {
		t.Fatalf("idle: %v %v", ok, err)
	}
}

// templateServer loads the stub templates from a directory the test can rewrite.
type templateServer struct {
	dir string
	srv *httpapi.Server
	sh  *httpapi.Shared
}

func newTemplateServer(t *testing.T, pool *pgxpool.Pool) *templateServer {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	dir := stubTemplates(t)
	srv, err := httpapi.New(httpapi.Deps{Cfg: config.Config{AnthropicBaseURL: "http://127.0.0.1:1"}, Pool: pool, Log: log, TemplatesDir: dir})
	must(t, err)
	must(t, srv.LoadTemplates(context.Background()))
	return &templateServer{dir: dir, srv: srv, sh: httpapi.NewShared(httpapi.Deps{Pool: pool, Log: log})}
}

// change gives the template one more node, as a later release would, and reloads it.
func (ts *templateServer) change(t *testing.T, key string) {
	t.Helper()
	path := filepath.Join(ts.dir, key+".json")
	raw, err := os.ReadFile(path)
	must(t, err)
	next := strings.Replace(string(raw), `{"id":"end","kind":"end"}`, `{"id":"extra","kind":"agent","agent_name":"Analyst","task":"more","output_schema":{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]},"input_from":["work"]},{"id":"end","kind":"end"}`, 1)
	next = strings.Replace(next, `{"from":"work","to":"end"}`, `{"from":"work","to":"extra"},{"from":"extra","to":"end"}`, 1)
	if next == string(raw) {
		t.Fatal("template not changed")
	}
	must(t, os.WriteFile(path, []byte(next), 0o644))
	must(t, ts.srv.LoadTemplates(context.Background()))
}

// countingResetter counts the resets Templates asks for.
type countingResetter struct {
	Resetter
	resets []string
}

func (c *countingResetter) ResetFromTemplate(ctx context.Context, id string) error {
	c.resets = append(c.resets, id)
	return c.Resetter.ResetFromTemplate(ctx, id)
}

func copyOf(t *testing.T, pool *pgxpool.Pool, name string) *store.Workflow {
	t.Helper()
	wf, err := store.GetWorkflow(context.Background(), pool, mustWorkflowNamed(t, pool, name))
	must(t, err)
	return wf
}

// A newly instantiated copy records the hash of the definition as stored, and an unchanged
// template makes later seed runs no-ops: no reset, no version bump.
func TestSeedTemplatesRecordHashAndStayQuiet(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	_, _, err := Seed(ctx, pool)
	must(t, err)
	ts := newTemplateServer(t, pool)
	cr := &countingResetter{Resetter: ts.sh}

	made, upgraded, err := Templates(ctx, pool, cr, log)
	if err != nil || len(made) != 3 || len(upgraded) != 0 {
		t.Fatalf("first: %v %v %v", made, upgraded, err)
	}
	for _, name := range made {
		wf := copyOf(t, pool, name)
		if wf.SeededHash == nil || *wf.SeededHash != store.DefinitionHash(wf.Definition) || wf.Version != 1 {
			t.Fatalf("%s: seeded_hash %v version %d", name, wf.SeededHash, wf.Version)
		}
	}
	for range 2 {
		made, upgraded, err = Templates(ctx, pool, cr, log)
		if err != nil || len(made) != 0 || len(upgraded) != 0 || len(cr.resets) != 0 {
			t.Fatalf("repeat: %v %v %v resets %v", made, upgraded, err, cr.resets)
		}
	}
	if wf := copyOf(t, pool, "Wave pipeline"); wf.Version != 1 {
		t.Fatalf("version = %d", wf.Version)
	}
}

// A copy whose hash matches is upgraded when the template changed; the run after is a no-op.
func TestSeedTemplatesUpgradeMatchingHash(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	_, _, err := Seed(ctx, pool)
	must(t, err)
	ts := newTemplateServer(t, pool)
	_, _, err = Templates(ctx, pool, ts.sh, log)
	must(t, err)

	ts.change(t, "wave_pipeline")
	cr := &countingResetter{Resetter: ts.sh}
	made, upgraded, err := Templates(ctx, pool, cr, log)
	if err != nil || len(made) != 0 || !slices.Equal(upgraded, []string{"Wave pipeline"}) || len(cr.resets) != 1 {
		t.Fatalf("upgrade: %v %v %v resets %v", made, upgraded, err, cr.resets)
	}
	wf := copyOf(t, pool, "Wave pipeline")
	if wf.Version != 2 || !strings.Contains(string(wf.Definition), `"extra"`) || wf.SeededHash == nil || *wf.SeededHash != store.DefinitionHash(wf.Definition) {
		t.Fatalf("after upgrade: version %d hash %v def %s", wf.Version, wf.SeededHash, wf.Definition)
	}
	_, upgraded, err = Templates(ctx, pool, cr, log)
	if err != nil || len(upgraded) != 0 || len(cr.resets) != 1 || copyOf(t, pool, "Wave pipeline").Version != 2 {
		t.Fatalf("after: %v %v resets %v", upgraded, err, cr.resets)
	}
}

// A version-1 copy with no hash (made before the hash existed) is upgraded; a NULL-hash
// copy at a later version is kept.
func TestSeedTemplatesUpgradeNullHashVersionOne(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	_, _, err := Seed(ctx, pool)
	must(t, err)
	ts := newTemplateServer(t, pool)
	_, _, err = Templates(ctx, pool, ts.sh, log)
	must(t, err)
	_, err = pool.Exec(ctx, `UPDATE workflows SET seeded_hash = NULL`)
	must(t, err)

	ts.change(t, "wave_pipeline")
	ts.change(t, "ship_pipeline")
	ship := copyOf(t, pool, "Ship pipeline")
	must(t, store.UpdateWorkflow(ctx, pool, ship)) // version 2, same definition: touched
	var logs bytes.Buffer
	made, upgraded, err := Templates(ctx, pool, ts.sh, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil || len(made) != 0 || !slices.Equal(upgraded, []string{"Wave pipeline"}) {
		t.Fatalf("upgrade: %v %v %v", made, upgraded, err)
	}
	if wf := copyOf(t, pool, "Wave pipeline"); wf.Version != 2 || !strings.Contains(string(wf.Definition), `"extra"`) || wf.SeededHash == nil {
		t.Fatalf("wave: version %d hash %v", wf.Version, wf.SeededHash)
	}
	if wf := copyOf(t, pool, "Ship pipeline"); wf.Version != 2 || strings.Contains(string(wf.Definition), `"extra"`) {
		t.Fatalf("ship: version %d def %s", wf.Version, wf.Definition)
	}
	// The plan pipeline was current: it only gained its hash.
	if wf := copyOf(t, pool, "Plan pipeline"); wf.Version != 1 || wf.SeededHash == nil {
		t.Fatalf("plan: version %d hash %v", wf.Version, wf.SeededHash)
	}
	if !strings.Contains(logs.String(), "Ship pipeline") {
		t.Fatalf("no kept line: %s", logs.String())
	}
}

// A copy edited after it was seeded (hash mismatch) is kept, logged by name, and never reset.
func TestSeedTemplatesKeepEditedCopy(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	_, _, err := Seed(ctx, pool)
	must(t, err)
	ts := newTemplateServer(t, pool)
	quiet := slog.New(slog.DiscardHandler)
	_, _, err = Templates(ctx, pool, ts.sh, quiet)
	must(t, err)

	wf := copyOf(t, pool, "Wave pipeline")
	var def map[string]any
	must(t, json.Unmarshal(wf.Definition, &def))
	def["project"] = true
	wf.Definition, _ = json.Marshal(def)
	must(t, store.UpdateWorkflow(ctx, pool, wf))
	edited := copyOf(t, pool, "Wave pipeline")

	ts.change(t, "wave_pipeline")
	var logs bytes.Buffer
	cr := &countingResetter{Resetter: ts.sh}
	made, upgraded, err := Templates(ctx, pool, cr, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil || len(made) != 0 || len(upgraded) != 0 || len(cr.resets) != 0 {
		t.Fatalf("edited: %v %v %v resets %v", made, upgraded, err, cr.resets)
	}
	after := copyOf(t, pool, "Wave pipeline")
	if after.Version != edited.Version || string(after.Definition) != string(edited.Definition) {
		t.Fatalf("copy changed: version %d", after.Version)
	}
	out := logs.String()
	if !strings.Contains(out, "kept: edited since seeded; use seed-reset to restore") || !strings.Contains(out, `workflow="Wave pipeline"`) {
		t.Fatalf("log = %s", out)
	}
}
