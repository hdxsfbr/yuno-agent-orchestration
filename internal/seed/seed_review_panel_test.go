package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	"yuno/internal/config"
	"yuno/internal/httpapi"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// panelReviewers are the three agents the Review panel adds (DESIGN 5.26 rule 14, D-064):
// what each is called, the concern its prompt names, and the node that runs it.
var panelReviewers = []struct {
	name, role, node string
	concern          []string
}{
	{"Security Reviewer", "Security reviewer", "security",
		[]string{"security and only security", "injection", "secrets in code", "missing authorisation and input checks", "unsafe deserialisation", "dependency risk"}},
	{"Performance Reviewer", "Performance reviewer", "performance",
		[]string{"performance and only performance", "complexity", "repeated queries", "needless allocation", "blocking I/O", "missing bounds"}},
	{"Maintainability Reviewer", "Maintainability reviewer", "maintainability",
		[]string{"maintainability and only maintainability", "naming", "structure", "duplication", "tests that prove the change", "documentation where a reader needs it"}},
}

// What the three prompts say in common (DESIGN 5.26 rule 14): the change is in the
// checkout, read with git status and git diff <default branch>, new files are read, the
// tests are run only when the concern needs it, nothing is edited, and a change with
// nothing of the concern blocking it is approved.
var panelCommonPhrases = []string{
	"git status and git diff <default branch>", "read new files", "Run the project's tests only when your concern needs it",
	"Never change a file", "When nothing of your concern blocks the change, approve it",
}

// reviewPanelTemplate is templates/review_panel.json.
type reviewPanelTemplate struct {
	Key         string          `json:"key"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Definition  json.RawMessage `json:"definition"`
}

func loadReviewPanel(t *testing.T) (reviewPanelTemplate, *workflow.Definition) {
	t.Helper()
	raw, err := os.ReadFile("../../templates/review_panel.json")
	must(t, err)
	var tpl reviewPanelTemplate
	must(t, json.Unmarshal(raw, &tpl))
	def, err := workflow.Parse(tpl.Definition)
	must(t, err)
	return tpl, def
}

func shippedAgent(name string) *store.Agent {
	for i := range Agents {
		if strings.EqualFold(Agents[i].Name, name) {
			return &Agents[i]
		}
	}
	return nil
}

// DESIGN 5.26 rule 14: the seed creates the three reviewers once (Sonnet, read, ls, exec
// and finish, memory off), each prompt names its concern and the shared instructions, and
// the Reviewer is the lead, unchanged.
func TestSeedCreatesReviewPanelReviewers(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	created, _, err := Seed(ctx, pool)
	must(t, err)
	for _, r := range panelReviewers {
		if !slices.Contains(created, r.name) {
			t.Errorf("%s was not created: %v", r.name, created)
		}
	}
	prompts := map[string]bool{}
	for _, r := range panelReviewers {
		a, err := findAgent(ctx, pool, r.name)
		must(t, err)
		if a == nil || a.Role != r.role || a.Model != "claude-sonnet-5-5" || a.MemoryPolicy.Enabled {
			t.Fatalf("%s = %+v", r.name, a)
		}
		if want := []string{"read", "ls", "exec", "finish"}; !slices.Equal(a.ToolAllowlist, want) {
			t.Errorf("%s tools = %v, want %v", r.name, a.ToolAllowlist, want)
		}
		for _, phrase := range append(slices.Clone(r.concern), panelCommonPhrases...) {
			if !strings.Contains(a.SystemPrompt, phrase) {
				t.Errorf("%s prompt lacks %q:\n%s", r.name, phrase, a.SystemPrompt)
			}
		}
		if prompts[a.SystemPrompt] {
			t.Errorf("%s has the prompt of another reviewer", r.name)
		}
		prompts[a.SystemPrompt] = true
		// Nothing of the other tools: the reviewers neither remember nor write nor ask.
		for _, word := range []string{"memory_", "ask_operator", "read_merge_request", "open_merge_request", "DESIGN"} {
			if strings.Contains(a.SystemPrompt, word) {
				t.Errorf("%s prompt names %q:\n%s", r.name, word, a.SystemPrompt)
			}
		}
	}
	reviewer := shippedAgent("Reviewer")
	if reviewer == nil || reviewer.Model != "claude-opus-5-5" || strings.Contains(reviewer.SystemPrompt, "panel") {
		t.Fatalf("the Reviewer, the panel's lead, changed: %+v", reviewer)
	}

	// Seeding again creates none of them and leaves one agent of each name.
	created, existing, err := Seed(ctx, pool)
	must(t, err)
	if len(created) != 0 || len(existing) != len(Agents) {
		t.Fatalf("second seed: created %v existing %v", created, existing)
	}
	all, err := store.ListAgents(ctx, pool)
	must(t, err)
	for _, r := range panelReviewers {
		n := 0
		for _, a := range all {
			if a.Name == r.name {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d agents named %s", n, r.name)
		}
	}
}

// DESIGN 5.26 rule 14, T5.26.13: the Review panel template is the graph of the rule, passes
// the structure checks of 5.26 rule 2, renders its tasks, and names only agents the seed
// creates.
func TestReviewPanelTemplateIsValidAndShaped(t *testing.T) {
	tpl, def := loadReviewPanel(t)
	if tpl.Key != "review_panel" || tpl.Name != "Review panel" || tpl.Description == "" {
		t.Fatalf("template = %s / %s / %q", tpl.Key, tpl.Name, tpl.Description)
	}
	if err := def.ValidateStructure(); err != nil {
		t.Fatalf("ValidateStructure: %v", err)
	}
	if msgs := def.ValidateTemplates(nil); len(msgs) != 0 {
		t.Fatalf("templates do not render: %v", msgs)
	}

	node := func(id string) workflow.Node {
		t.Helper()
		n, ok := def.Node(id)
		if !ok {
			t.Fatalf("no node %s", id)
		}
		return *n
	}
	kinds := map[string]string{"start": "start", "coder": "agent", "panel": "fork", "security": "agent", "performance": "agent",
		"maintainability": "agent", "merge": "join", "lead": "agent", "gate": "approval", "deployer": "agent", "end": "end"}
	if len(def.Nodes) != len(kinds) {
		t.Errorf("%d nodes, want %d", len(def.Nodes), len(kinds))
	}
	for id, kind := range kinds {
		if node(id).Kind != kind {
			t.Errorf("%s is a %s, want %s", id, node(id).Kind, kind)
		}
	}
	agentNames := map[string]string{"coder": "Coder", "security": "Security Reviewer", "performance": "Performance Reviewer",
		"maintainability": "Maintainability Reviewer", "lead": "Reviewer", "deployer": "Deployer"}
	for id, name := range agentNames {
		if n := node(id); n.AgentName != name || n.AgentID != "" {
			t.Errorf("%s names agent %q / %q, want %q", id, n.AgentName, n.AgentID, name)
		}
		if shippedAgent(name) == nil {
			t.Errorf("%s: no shipped agent named %s", id, name)
		}
	}
	if p := node("panel"); p.Join != "merge" || p.Task != "" || p.AgentName != "" {
		t.Errorf("panel = %+v", p)
	}
	if m := node("merge"); m.Checkout != "" || m.Join != "" || m.Task != "" {
		t.Errorf("merge = %+v (the reviewers edit nothing: no checkout)", m)
	}
	for _, id := range []string{"security", "performance", "maintainability"} {
		if got := node(id).InputFrom; !slices.Equal(got, []string{"start", "coder"}) {
			t.Errorf("%s input_from = %v", id, got)
		}
	}
	if got := node("lead").InputFrom; !slices.Equal(got, []string{"coder", "security", "performance", "maintainability"}) {
		t.Errorf("lead input_from = %v", got)
	}
	if got := node("deployer").InputFrom; !slices.Equal(got, []string{"coder", "lead"}) {
		t.Errorf("deployer input_from = %v", got)
	}
	if !def.Project || def.Limits.MaxLoopVisits != 3 {
		t.Errorf("project %v, max_loop_visits %d", def.Project, def.Limits.MaxLoopVisits)
	}

	// The edges: the fork's carry no condition and start one branch each, in order; the
	// branches end at the join; the lead loops back to the coder or goes to the gate.
	var fork, join []string
	for _, e := range def.OutgoingEdges("panel") {
		if e.When != nil {
			t.Errorf("the edge panel -> %s has a condition", e.To)
		}
		fork = append(fork, e.To)
	}
	for _, e := range def.Edges {
		if e.To == "merge" {
			join = append(join, e.From)
		}
	}
	if want := []string{"security", "performance", "maintainability"}; !slices.Equal(fork, want) || !slices.Equal(join, want) {
		t.Errorf("fork edges %v, join edges %v, want %v", fork, join, want)
	}
	lead := def.OutgoingEdges("lead")
	if len(lead) != 2 || lead[0].To != "coder" || lead[1].To != "gate" {
		t.Fatalf("lead edges = %+v", lead)
	}
	for i, verdict := range []string{"changes_requested", "approved"} {
		w := lead[i].When
		if w == nil || w.Path != "/lead/verdict" || w.Op != "eq" || string(w.Value) != `"`+verdict+`"` {
			t.Errorf("lead -> %s when = %+v", lead[i].To, w)
		}
	}
	region, err := def.Region("panel")
	must(t, err)
	if !slices.Equal(region.Heads, []string{"security", "performance", "maintainability"}) || region.Join != "merge" || len(region.Members) != 3 {
		t.Errorf("region = %+v", region)
	}
	if f, ok := def.ForkOf("merge"); !ok || f != "panel" {
		t.Errorf("fork of merge = %q %v", f, ok)
	}

	// The state schema lists every agent node, and the four reviews share one output schema.
	var state struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	must(t, json.Unmarshal(def.StateSchema, &state))
	for id := range agentNames {
		if _, ok := state.Properties[id]; !ok {
			t.Errorf("state_schema lacks %s", id)
		}
	}
	if len(state.Properties) != len(agentNames) {
		t.Errorf("state_schema lists %d nodes, want %d", len(state.Properties), len(agentNames))
	}
	review := compact(t, node("lead").OutputSchema)
	for _, id := range []string{"security", "performance", "maintainability"} {
		if got := compact(t, node(id).OutputSchema); got != review {
			t.Errorf("%s output_schema = %s, want the lead's %s", id, got, review)
		}
		if got := compact(t, state.Properties[id]); !strings.Contains(got, `"enum":["approved","changes_requested"]`) || !strings.Contains(got, `"required":["verdict","feedback"]`) {
			t.Errorf("state_schema of %s = %s", id, got)
		}
	}
	if !strings.Contains(review, `"enum":["approved","changes_requested"]`) || !strings.Contains(review, `"required":["verdict","feedback"]`) {
		t.Errorf("review schema = %s", review)
	}
}

func compact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var buf bytes.Buffer
	must(t, json.Compact(&buf, raw))
	return buf.String()
}

// The tasks say what each node is to do, render over the state the run holds when the node
// runs (the coder's report, the three reviews, the lead's feedback), and carry no note for
// the developer.
func TestReviewPanelTasks(t *testing.T) {
	tpl, def := loadReviewPanel(t)
	task := func(id string) string {
		n, _ := def.Node(id)
		return n.Task
	}
	focus := map[string][]string{
		"security":        {"for security: injection, secrets in code, missing authorisation and input checks, unsafe deserialisation and dependency risk"},
		"performance":     {"for performance: complexity, repeated queries, needless allocation and blocking I/O, and missing bounds"},
		"maintainability": {"for maintainability: naming, structure, duplication, tests that prove the change, and documentation where a reader needs it"},
	}
	for id, concern := range focus {
		for _, want := range append(concern, "Review the change", "{{.state.coder.summary}}", "{{range .state.coder.files}}",
			"run git status and git diff <default branch> there, read new files", "Do not change any file",
			`verdict "approved" when nothing of your concern blocks the change, otherwise "changes_requested" with specific, actionable feedback`) {
			if !strings.Contains(task(id), want) {
				t.Errorf("%s task lacks %q:\n%s", id, want, task(id))
			}
		}
	}
	for _, want := range []string{"Merge the three reviews", "{{.state.security.feedback}}", "{{.state.performance.feedback}}",
		"{{.state.maintainability.feedback}}", `"approved" only when all three reviewers approved`, `"changes_requested"`,
		"grouped by theme with duplicates merged into one"} {
		if !strings.Contains(task("lead"), want) {
			t.Errorf("lead task lacks %q:\n%s", want, task("lead"))
		}
	}

	input := json.RawMessage(`{"task":"add a /health endpoint","version":"2.1.0"}`)
	state := json.RawMessage(`{
		"coder": {"summary": "Added the endpoint", "files": ["health.go", "health_test.go"], "tests_passed": true},
		"security": {"verdict": "approved", "feedback": "nothing to flag in the handler"},
		"performance": {"verdict": "changes_requested", "feedback": "the handler queries twice per request"},
		"maintainability": {"verdict": "approved", "feedback": "clear names"},
		"lead": {"verdict": "changes_requested", "feedback": "Performance: query once per request."}
	}`)
	render := func(text string, state json.RawMessage) string {
		t.Helper()
		out, err := workflow.Render(text, input, state)
		must(t, err)
		return out
	}
	for _, id := range []string{"security", "performance", "maintainability"} {
		got := render(task(id), state)
		for _, want := range []string{"add a /health endpoint", "Added the endpoint", "health.go health_test.go", "git diff <default branch>", "runs/$RUN_ID/"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s task renders without %q:\n%s", id, want, got)
			}
		}
	}
	got := render(task("lead"), state)
	for _, want := range []string{"add a /health endpoint", "Added the endpoint", "Security reviewer (approved): nothing to flag in the handler",
		"Performance reviewer (changes_requested): the handler queries twice per request", "Maintainability reviewer (approved): clear names"} {
		if !strings.Contains(got, want) {
			t.Errorf("lead task renders without %q:\n%s", want, got)
		}
	}
	// The coder reads the lead's feedback, and only once there is one.
	if got := render(task("coder"), state); !strings.Contains(got, "The review panel asked for changes") || !strings.Contains(got, "Performance: query once per request.") {
		t.Errorf("coder task with feedback:\n%s", got)
	}
	if got := render(task("coder"), json.RawMessage(`{}`)); strings.Contains(got, "asked for changes") || !strings.Contains(got, "add a /health endpoint") {
		t.Errorf("coder task without feedback:\n%s", got)
	}
	for _, id := range []string{"gate", "deployer"} {
		n, _ := def.Node(id)
		text := n.Message + n.Task
		if got := render(text, state); !strings.Contains(got, "Review panel: Performance: query once per request.") || !strings.Contains(got, "Added the endpoint") {
			t.Errorf("%s names the lead's feedback and the coder's summary:\n%s", id, got)
		}
	}

	// What the operator and the model read is not about the implementation.
	texts := []string{tpl.Description}
	for _, n := range def.Nodes {
		texts = append(texts, n.Task, n.Message)
	}
	for _, r := range panelReviewers {
		texts = append(texts, shippedAgent(r.name).SystemPrompt)
	}
	for _, text := range texts {
		for _, word := range []string{"DESIGN", "D-06", "5.26", "TODO", "simulated", "stub"} {
			if strings.Contains(text, word) {
				t.Errorf("%q appears in %q", word, text)
			}
		}
	}
}

// DESIGN 5.26 rule 14, T5.26.13: the template instantiates against the agents the seed
// creates, and the result passes the structure checks by agent id too (three distinct
// agents in the three branches).
func TestReviewPanelInstantiatesAgainstTheSeededAgents(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	_, _, err := Seed(ctx, pool)
	must(t, err)
	srv, err := httpapi.New(httpapi.Deps{Cfg: config.Config{AnthropicBaseURL: "http://127.0.0.1:1"}, Pool: pool, Log: log, TemplatesDir: "../../templates"})
	must(t, err)
	must(t, srv.LoadTemplates(ctx))
	sh := httpapi.NewShared(httpapi.Deps{Pool: pool, Log: log})
	made, err := Templates(ctx, pool, sh, log)
	if err != nil || !slices.Contains(made, "Review panel") {
		t.Fatalf("templates: %v %v", made, err)
	}

	_, shipped := loadReviewPanel(t)
	wf, err := store.GetWorkflow(ctx, pool, mustWorkflowNamed(t, pool, "Review panel"))
	must(t, err)
	if wf.FromTemplate == nil || *wf.FromTemplate != "review_panel" || strings.Contains(string(wf.Definition), "agent_name") {
		t.Fatalf("instance = %+v", wf)
	}
	def, err := workflow.Parse(wf.Definition)
	must(t, err)
	if err := def.ValidateStructure(); err != nil {
		t.Fatalf("the instance does not validate: %v", err)
	}
	seen := map[string]string{}
	for _, n := range shipped.Nodes {
		if n.Kind != workflow.KindAgent {
			continue
		}
		got, ok := def.Node(n.ID)
		if !ok || got.AgentID == "" || got.AgentName != "" {
			t.Fatalf("%s in the instance = %+v", n.ID, got)
		}
		a, err := store.GetAgent(ctx, pool, got.AgentID)
		must(t, err)
		if !strings.EqualFold(a.Name, n.AgentName) {
			t.Errorf("%s runs %q, want %q", n.ID, a.Name, n.AgentName)
		}
		seen[n.ID] = got.AgentID
	}
	if len(seen) != 6 || seen["security"] == seen["performance"] || seen["security"] == seen["maintainability"] || seen["performance"] == seen["maintainability"] {
		t.Errorf("agents by node = %v", seen)
	}
	if p, _ := def.Node("panel"); p.Kind != workflow.KindFork || p.Join != "merge" {
		t.Errorf("panel in the instance = %+v", p)
	}
	if m, _ := def.Node("merge"); m.Kind != workflow.KindJoin {
		t.Errorf("merge in the instance = %+v", m)
	}
}

// DESIGN 5.16 rule 14 with 5.26 rule 14: on an install from before the wave `make seed`
// creates the three reviewers and instantiates the Review panel, and `make seed-reset`
// restores their prompts and tools and the template's definition when they drifted.
func TestResetShippedRestoresTheReviewPanel(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	_, _, err := Seed(ctx, pool)
	must(t, err)
	srv, err := httpapi.New(httpapi.Deps{Cfg: config.Config{AnthropicBaseURL: "http://127.0.0.1:1"}, Pool: pool, Log: log, TemplatesDir: "../../templates"})
	must(t, err)
	must(t, srv.LoadTemplates(ctx))
	sh := httpapi.NewShared(httpapi.Deps{Pool: pool, Log: log})
	_, err = Templates(ctx, pool, sh, log)
	must(t, err)

	// The older install has neither the reviewers nor the workflow.
	must(t, store.DeleteWorkflow(ctx, pool, mustWorkflowNamed(t, pool, "Review panel")))
	for _, r := range panelReviewers {
		a, err := findAgent(ctx, pool, r.name)
		must(t, err)
		must(t, store.DeleteAgent(ctx, pool, a.ID))
	}
	created, _, err := Seed(ctx, pool)
	must(t, err)
	if want := []string{"Security Reviewer", "Performance Reviewer", "Maintainability Reviewer"}; !slices.Equal(created, want) {
		t.Fatalf("created %v, want %v", created, want)
	}
	made, err := Templates(ctx, pool, sh, log)
	if err != nil || !slices.Equal(made, []string{"Review panel"}) {
		t.Fatalf("templates: %v %v", made, err)
	}
	agents, workflows, err := ResetShipped(ctx, pool, sh)
	must(t, err)
	if len(agents) != 0 || !slices.Contains(workflows, "Review panel") {
		t.Fatalf("reset of a fresh install: %v %v", agents, workflows)
	}

	// An operator changed the Security Reviewer (an older prompt, a write tool, a model of
	// their own) and edited the lead's task and the workflow's name.
	security, err := findAgent(ctx, pool, "Security Reviewer")
	must(t, err)
	security.SystemPrompt, security.ToolAllowlist, security.Model = "old prompt", []string{"read", "ls", "write", "finish"}, "claude-haiku-5-5"
	must(t, store.UpdateAgent(ctx, pool, security))
	panelID := mustWorkflowNamed(t, pool, "Review panel")
	wf, err := store.GetWorkflow(ctx, pool, panelID)
	must(t, err)
	wf.Name = "Review panel (ours)"
	wf.Definition = json.RawMessage(strings.Replace(string(wf.Definition), "Merge the three reviews", "Judge the change", 1))
	must(t, store.UpdateWorkflow(ctx, pool, wf))
	if strings.Contains(string(wf.Definition), "Merge the three reviews") {
		t.Fatal("test setup: the lead's task still merges")
	}

	agents, workflows, err = ResetShipped(ctx, pool, sh)
	must(t, err)
	if !slices.Equal(agents, []string{"Security Reviewer"}) || !slices.Contains(workflows, "Review panel (ours)") {
		t.Fatalf("reset %v %v", agents, workflows)
	}
	security, err = findAgent(ctx, pool, "Security Reviewer")
	must(t, err)
	shipped := shippedAgent("Security Reviewer")
	if security.SystemPrompt != shipped.SystemPrompt || !slices.Equal(security.ToolAllowlist, []string{"read", "ls", "exec", "finish"}) ||
		security.Model != "claude-haiku-5-5" || security.MaterializedHash != "" {
		t.Fatalf("security reviewer after the reset: %q %v %s %q", security.SystemPrompt, security.ToolAllowlist, security.Model, security.MaterializedHash)
	}
	wf, err = store.GetWorkflow(ctx, pool, panelID)
	must(t, err)
	def, err := workflow.Parse(wf.Definition)
	must(t, err)
	if lead, _ := def.Node("lead"); wf.Name != "Review panel (ours)" || !strings.Contains(lead.Task, "Merge the three reviews") ||
		strings.Contains(string(wf.Definition), "agent_name") || def.ValidateStructure() != nil {
		t.Fatalf("review panel after the reset: name %q, definition %s", wf.Name, wf.Definition)
	}
	agents, _, err = ResetShipped(ctx, pool, sh)
	must(t, err)
	if len(agents) != 0 {
		t.Fatalf("a second reset changed %v", agents)
	}
}
