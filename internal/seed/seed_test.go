package seed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/config"
	"yuno/internal/httpapi"
	"yuno/internal/store"
	"yuno/internal/tools"
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
	want := []string{"delegate_task", "list_agents", "list_projects", "list_workflows", "memory_recall", "memory_save", "read_merge_request", "run_status"}
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

// DESIGN 5.15 rule 13: both shipped templates are instantiated once, through the API's
// own instantiation code, with their agents resolved; a workflow made from a template
// by hand counts.
func TestSeedInstantiatesTemplatesOnce(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	_, _, err := Seed(ctx, pool)
	must(t, err)
	sh := httpapi.NewShared(httpapi.Deps{Pool: pool, Log: log})

	// Before the api has loaded the templates there is nothing to instantiate.
	made, err := Templates(ctx, pool, sh, log)
	if err != nil || len(made) != 0 {
		t.Fatalf("without templates: %v %v", made, err)
	}
	srv, err := httpapi.New(httpapi.Deps{Cfg: config.Config{AnthropicBaseURL: "http://127.0.0.1:1"}, Pool: pool, Log: log,
		TemplatesDir: "../../templates"})
	must(t, err)
	must(t, srv.LoadTemplates(ctx))

	made, err = Templates(ctx, pool, sh, log)
	if err != nil || !slices.Equal(made, []string{"Dev pipeline", "Research pipeline"}) {
		t.Fatalf("first: %v %v", made, err)
	}
	made, err = Templates(ctx, pool, sh, log)
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
	if instances != 2 {
		t.Fatalf("instances = %d", instances)
	}

	// Deleting one brings it back at the next seed; the other stays single.
	for _, wf := range all {
		if wf.FromTemplate != nil && *wf.FromTemplate == "research_pipeline" {
			must(t, store.DeleteWorkflow(ctx, pool, wf.ID))
		}
	}
	made, err = Templates(ctx, pool, sh, log)
	if err != nil || !slices.Equal(made, []string{"Research pipeline"}) {
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

	coder, err := findAgent(ctx, pool, "Coder")
	must(t, err)
	_, err = store.BindChannel(ctx, pool, store.ChannelTelegram, coder.ID)
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
// counts as instantiated; a failing template does not stop the others, and each
// failure is reported.
func TestSeedTemplatesUpgradeAndFailures(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	_, _, err := Seed(ctx, pool)
	must(t, err)
	srv, err := httpapi.New(httpapi.Deps{Cfg: config.Config{AnthropicBaseURL: "http://127.0.0.1:1"}, Pool: pool, Log: log,
		TemplatesDir: "../../templates"})
	must(t, err)
	must(t, srv.LoadTemplates(ctx))
	sh := httpapi.NewShared(httpapi.Deps{Pool: pool, Log: log})

	// An older copy of the Dev pipeline, made with "Create copy" before wave 5.
	must(t, store.CreateWorkflow(ctx, pool, &store.Workflow{Name: "Dev pipeline", Definition: []byte(`{"nodes":[],"edges":[]}`)}))
	both := &failingInstantiator{in: sh, fail: map[string]bool{"dev_pipeline": true, "research_pipeline": true}}
	made, err := Templates(ctx, pool, both, log)
	if len(made) != 0 || err == nil || !strings.Contains(err.Error(), "template research_pipeline: agent missing") ||
		strings.Contains(err.Error(), "dev_pipeline") || !slices.Equal(both.asked, []string{"research_pipeline"}) {
		t.Fatalf("old copy and a failure: %v %v, asked %v", made, err, both.asked)
	}

	// Every key is tried and every failure named.
	must(t, store.DeleteWorkflow(ctx, pool, mustWorkflowNamed(t, pool, "Dev pipeline")))
	both = &failingInstantiator{in: sh, fail: map[string]bool{"dev_pipeline": true, "research_pipeline": true}}
	made, err = Templates(ctx, pool, both, log)
	if len(made) != 0 || err == nil || !strings.Contains(err.Error(), "template dev_pipeline: agent missing") ||
		!strings.Contains(err.Error(), "template research_pipeline: agent missing") || len(both.asked) != 2 {
		t.Fatalf("two failures: %v %v, asked %v", made, err, both.asked)
	}
	one := &failingInstantiator{in: sh, fail: map[string]bool{"dev_pipeline": true}}
	made, err = Templates(ctx, pool, one, log)
	if !slices.Equal(made, []string{"Research pipeline"}) || err == nil || !strings.Contains(err.Error(), "dev_pipeline") {
		t.Fatalf("first fails, second works: %v %v", made, err)
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

// DESIGN 5.16 rule 14: the reset restores the shipped agents' role, prompt and
// allowlist and the template workflows' definitions, keeps everything else, and leaves
// agents and workflows that are not shipped alone.
func TestResetShipped(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	_, _, err := Seed(ctx, pool)
	must(t, err)
	srv, err := httpapi.New(httpapi.Deps{Cfg: config.Config{AnthropicBaseURL: "http://127.0.0.1:1"}, Pool: pool, Log: log,
		TemplatesDir: "../../templates"})
	must(t, err)
	must(t, srv.LoadTemplates(ctx))
	sh := httpapi.NewShared(httpapi.Deps{Pool: pool, Log: log})
	_, err = Templates(ctx, pool, sh, log)
	must(t, err)

	// An operator edited the Deployer (an older prompt, no open_merge_request, a model
	// of their own) and the Dev pipeline (no project flag), and made an agent of their own.
	deployer, err := findAgent(ctx, pool, "Deployer")
	must(t, err)
	deployer.SystemPrompt, deployer.ToolAllowlist, deployer.Model = "old prompt", []string{"read", "ls", "deploy_release", "finish"}, "claude-haiku-5-5"
	deployer.Limits.MaxCostUSD = 0.25
	must(t, store.UpdateAgent(ctx, pool, deployer))
	own := &store.Agent{Name: "Own agent", Role: "mine", SystemPrompt: "mine", Model: "claude-sonnet-5-5", ToolAllowlist: []string{"read", "finish"}}
	must(t, store.CreateAgent(ctx, pool, own))
	devID := mustWorkflowNamed(t, pool, "Dev pipeline")
	dev, err := store.GetWorkflow(ctx, pool, devID)
	must(t, err)
	var def map[string]any
	must(t, json.Unmarshal(dev.Definition, &def))
	delete(def, "project")
	dev.Definition, _ = json.Marshal(def)
	dev.Name = "Dev pipeline (ours)"
	must(t, store.UpdateWorkflow(ctx, pool, dev))

	agents, workflows, err := ResetShipped(ctx, pool, sh)
	must(t, err)
	if !slices.Equal(agents, []string{"Deployer"}) || !slices.Equal(workflows, []string{"Dev pipeline (ours)", "Research pipeline"}) {
		t.Fatalf("reset %v %v", agents, workflows)
	}
	deployer, err = findAgent(ctx, pool, "Deployer")
	must(t, err)
	if deployer.SystemPrompt != Agents[2].SystemPrompt || !slices.Contains(deployer.ToolAllowlist, tools.OpenMergeRequestName) {
		t.Fatalf("deployer not reset: %q %v", deployer.SystemPrompt, deployer.ToolAllowlist)
	}
	if deployer.Model != "claude-haiku-5-5" || deployer.Limits.MaxCostUSD != 0.25 || deployer.MaterializedHash != "" {
		t.Fatalf("deployer's own settings changed: model %s limits %+v hash %q", deployer.Model, deployer.Limits, deployer.MaterializedHash)
	}
	got, err := store.GetAgent(ctx, pool, own.ID)
	must(t, err)
	if got.SystemPrompt != "mine" {
		t.Fatal("an agent that is not shipped was changed")
	}
	dev, err = store.GetWorkflow(ctx, pool, devID)
	must(t, err)
	must(t, json.Unmarshal(dev.Definition, &def))
	if def["project"] != true || dev.Name != "Dev pipeline (ours)" || dev.Version != 3 || strings.Contains(string(dev.Definition), "agent_name") {
		t.Fatalf("dev pipeline after the reset: name %q version %d project %v", dev.Name, dev.Version, def["project"])
	}
	// A second reset changes nothing on the agents and rewrites the workflows (a new
	// version each), which is what a deploy does on purpose.
	agents, workflows, err = ResetShipped(ctx, pool, sh)
	must(t, err)
	if len(agents) != 0 || len(workflows) != 2 {
		t.Fatalf("second reset %v %v", agents, workflows)
	}
}
