package engine

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"yuno/internal/openclaw"
	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// Wave 10 prerequisites (DESIGN 5.24, 5.25): the task protocol block's two texts and the
// call that hands a finished agent visit to the artifact collector.

// fakeCollector records what the engine hands the collector. The node is copied at the
// call, since the visit goes on changing its row.
type fakeCollector struct {
	mu    sync.Mutex
	calls []ArtifactInput
}

func (f *fakeCollector) Collect(_ context.Context, in ArtifactInput) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in.Node != nil {
		n := *in.Node
		in.Node = &n
	}
	f.calls = append(f.calls, in)
}

func (f *fakeCollector) seen() []ArtifactInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ArtifactInput(nil), f.calls...)
}

// The engine's collector is the one in Deps, and nil disables collection.
var _ ArtifactCollector = (*fakeCollector)(nil)

// T5.25.1 (the engine's side): every agent visit that ends is handed to the collector
// once, after its row is finished, with the agent's workspace, the run and the visit; a
// run without a checkout passes no project.
func TestAgentVisitsAreHandedToTheCollector(t *testing.T) {
	env := newEnv(t)
	col := &fakeCollector{}
	env.e.deps.Artifacts = col
	one := env.agent(func(a *store.Agent) { a.Name = "First" })
	two := env.agent(func(a *store.Agent) { a.Name = "Second" })
	wf := env.createWorkflow(line(
		workflow.Node{ID: "one", AgentID: one.ID, Task: "first"},
		workflow.Node{ID: "two", AgentID: two.ID, Task: "second"}))
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go finishThenFinal(t, env, p, `{"summary":"done"}`)
		return startedTurn(p)
	}
	run := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID})
	if run.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", run.Status, deref(run.FailureReason))
	}

	calls := col.seen()
	if len(calls) != 2 {
		t.Fatalf("collector called %d times, want once per agent visit (2)", len(calls))
	}
	for i, ag := range []*store.Agent{one, two} {
		got, err := store.GetAgent(context.Background(), env.pool, ag.ID)
		if err != nil || got.OpenClawAgentID == nil {
			t.Fatalf("agent %d: %+v %v", i, got, err)
		}
		in := calls[i]
		if want := openclaw.WorkspaceDir(env.dir, *got.OpenClawAgentID); in.Workspace != want {
			t.Errorf("call %d: workspace = %q, want %q", i, in.Workspace, want)
		}
		if in.Run == nil || in.Run.ID != run.ID {
			t.Errorf("call %d: run = %+v, want %s", i, in.Run, run.ID)
		}
		if in.Node == nil || in.Node.NodeID != []string{"one", "two"}[i] || in.Node.Visit != 1 || in.Node.RunID != run.ID {
			t.Errorf("call %d: node = %+v", i, in.Node)
		} else if in.Node.Status != store.NodeSucceeded {
			t.Errorf("call %d: the visit was collected as %q, before its row was finished", i, in.Node.Status)
		}
		if in.Project != nil || in.Branch != "" {
			t.Errorf("call %d: project = %+v, branch = %q for a run without a checkout", i, in.Project, in.Branch)
		}
	}
}

// The visit of a project run passes the project configuration and its default branch.
func TestProjectVisitIsCollectedWithItsProject(t *testing.T) {
	env, p := goProject(t)
	col := &fakeCollector{}
	env.e.deps.Artifacts = col
	wf := projectWorkflow(env, env.agent(nil))
	_, r := visitMessage(t, env, TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID}, nil)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	calls := col.seen()
	if len(calls) != 1 {
		t.Fatalf("collector called %d times, want 1", len(calls))
	}
	if calls[0].Project == nil || calls[0].Project.RepoURL != p.RepoURL || calls[0].Branch != p.DefaultBranch || calls[0].Branch == "" {
		t.Errorf("project = %+v, branch = %q, want the project %s on %q", calls[0].Project, calls[0].Branch, p.RepoURL, p.DefaultBranch)
	}
}

// collectArtifacts picks the project, the branch and the workspace as the brief says, and
// does nothing without a collector.
func TestCollectArtifactsInput(t *testing.T) {
	col := &fakeCollector{}
	e := New(Deps{StateDir: "/state", Artifacts: col})
	cfg := &project.Config{RepoURL: "https://git.test/g/p.git", DefaultBranch: "main"}
	run, node := &store.Run{ID: "run-1"}, &store.RunNode{ID: "rn-1", NodeID: "coder", Visit: 2}
	mr := &store.MergeRequest{SourceBranch: "yuno/abc"}
	for _, c := range []struct {
		name        string
		v           *visit
		wantProject bool
		wantBranch  string
	}{
		{"no project", &visit{}, false, ""},
		{"project with a checkout", &visit{project: cfg, toolchain: project.Toolchain{Name: "go"}}, true, "main"},
		{"merge request branch", &visit{project: cfg, toolchain: project.Toolchain{Name: "go"}, mergeRequest: mr}, true, "yuno/abc"},
		{"project not cloned", &visit{project: cfg}, false, "main"},
	} {
		c.v.run, c.v.rn, c.v.ocID = run, node, "oc-1"
		before := len(col.seen())
		e.collectArtifacts(context.Background(), c.v)
		calls := col.seen()
		if len(calls) != before+1 {
			t.Errorf("%s: %d calls, want one more", c.name, len(calls))
			continue
		}
		in := calls[len(calls)-1]
		if in.Workspace != "/state/workspace-oc-1" || in.Run != run || in.Node.ID != "rn-1" || in.Node.Visit != 2 {
			t.Errorf("%s: input = %+v", c.name, in)
		}
		if (in.Project != nil) != c.wantProject || (c.wantProject && in.Project != cfg) || in.Branch != c.wantBranch {
			t.Errorf("%s: project = %+v, branch = %q; want project %v, branch %q", c.name, in.Project, in.Branch, c.wantProject, c.wantBranch)
		}
	}

	none := New(Deps{StateDir: "/state"})
	none.collectArtifacts(context.Background(), &visit{run: run, rn: node}) // must not panic
}

// DESIGN 5.25 rule 1 and 5.24 rule 12: the two texts, word for word.
func TestProtocolBlockTexts(t *testing.T) {
	const id = "0199f0a2-0000-7000-8000-000000000001"
	wantLine := "Deliverables: put files the operator should receive (reports, data, documents) under runs/" + id +
		"/out/; the platform collects them when your step ends and shows them on the run page.\n"
	if got := DeliverablesLine(id); got != wantLine {
		t.Errorf("DeliverablesLine:\n%q\nwant\n%q", got, wantLine)
	}
	wantParagraph := "Questions: when one fact you cannot find out yourself decides the work, ask the operator with " +
		"yuno__ask_operator: a short question, with options when a few answers are likely; at most 3 per step. " +
		"The answer usually comes back in the tool result. If the result says \"pending\", save your progress " +
		"in files under runs/" + id + "/ and end your turn without calling yuno__finish: the platform runs " +
		"this step again with the answer added to your task. Decide yourself what you can.\n"
	if got := QuestionsParagraph(id); got != wantParagraph {
		t.Errorf("QuestionsParagraph:\n%q\nwant\n%q", got, wantParagraph)
	}
}

// DESIGN 5.24 rule 12: the Questions paragraph is in the task of an agent that holds
// ask_operator and only there; the Deliverables line is in every task. Both come after the
// project lines and before the finish instruction, the paragraph after the line.
func TestTaskMessageQuestionsParagraphOnlyForAgentsHoldingTheTool(t *testing.T) {
	const id = "0199f0a2-0000-7000-8000-000000000001"
	run := &store.Run{ID: id}
	node := &workflow.Node{Task: "Do it.", OutputSchema: json.RawMessage(`{"type":"object"}`)}
	message := func(allowlist []string) string {
		v := &visit{run: run, node: node, task: node.Task, token: "r_abcdefghijklmnopqrstuvwx",
			agent: &store.Agent{ToolAllowlist: allowlist}}
		return v.taskMessage()
	}

	asker := message([]string{"read", "ask_operator", "finish"})
	plain := message([]string{"read", "finish"})
	for what, msg := range map[string]string{"asker": asker, "plain": plain} {
		if strings.Count(msg, DeliverablesLine(id)) != 1 {
			t.Errorf("%s: the deliverables line is not in the task exactly once:\n%s", what, msg)
		}
	}
	if strings.Count(asker, QuestionsParagraph(id)) != 1 {
		t.Errorf("an agent holding ask_operator: the questions paragraph is not in the task exactly once:\n%s", asker)
	}
	if strings.Contains(plain, "Questions: ") || strings.Contains(plain, "yuno__ask_operator") {
		t.Errorf("an agent without ask_operator got the questions paragraph:\n%s", plain)
	}
	i, j, k := strings.Index(asker, DeliverablesLine(id)), strings.Index(asker, QuestionsParagraph(id)), strings.Index(asker, "When finished, call the tool yuno__finish")
	if !(0 < i && i < j && j < k) {
		t.Errorf("order: deliverables %d, questions %d, finish instruction %d:\n%s", i, j, k, asker)
	}
	// The effective allow list decides, as AGENTS.md reads it (review M2): an agent whose
	// ask_operator is blocked by an interaction rule or a guardrail, though it stays on the
	// raw allowlist, is not told to ask; the tool allowed by either spelling is.
	for name, a := range map[string]*store.Agent{
		"blocked by an interaction rule": {ToolAllowlist: []string{"read", "ask_operator"}, InteractionRules: map[string]string{"ask_operator": "blocked"}},
		"blocked by a guardrail":         {ToolAllowlist: []string{"read", "ask_operator"}, Guardrails: store.Guardrails{BlockedTools: []string{"ask_operator"}}},
		"blocked, prefixed spelling":     {ToolAllowlist: []string{"read", "yuno__ask_operator"}, InteractionRules: map[string]string{"yuno__ask_operator": "blocked"}},
	} {
		v := &visit{run: run, node: node, task: node.Task, token: "r_abcdefghijklmnopqrstuvwx", agent: a}
		if msg := v.taskMessage(); strings.Contains(msg, "Questions: ") || strings.Contains(msg, "yuno__ask_operator") {
			t.Errorf("an agent %s was told to ask:\n%s", name, msg)
		}
	}
	for name, a := range map[string]*store.Agent{
		"the prefixed spelling": {ToolAllowlist: []string{"yuno__ask_operator"}},
		"an explicit auto rule": {ToolAllowlist: []string{"ask_operator"}, InteractionRules: map[string]string{"ask_operator": "auto"}},
	} {
		v := &visit{run: run, node: node, task: node.Task, token: "r_abcdefghijklmnopqrstuvwx", agent: a}
		if msg := v.taskMessage(); strings.Count(msg, QuestionsParagraph(id)) != 1 {
			t.Errorf("an agent with %s was not told to ask:\n%s", name, msg)
		}
	}
	// The same visit with a nil agent (a bare visit in a unit test) is a plain one.
	if msg := (&visit{run: run, node: node, task: node.Task, token: "r_x"}).taskMessage(); strings.Contains(msg, "Questions: ") {
		t.Errorf("a visit without an agent got the questions paragraph:\n%s", msg)
	}
}
