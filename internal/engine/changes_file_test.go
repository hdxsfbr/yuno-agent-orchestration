package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"yuno/internal/openclaw"
	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// DESIGN 5.30 rule 11 (D-090): a later step of a project run finds the run's changes so far
// in runs/<run id>/changes.diff before its turn, outside the checkout, so a step that runs
// no commands (the Code Reviewer) reads what git diff would show, new files included. The
// first step has none, and a file an earlier step left under that name is never carried
// forward, while the step's other files are.
func TestLaterStepGetsTheRunsChanges(t *testing.T) {
	env, _, p := projectEnv(t, map[string]string{"README.md": "# demo\n", "go.mod": "module demo\n"}, false)
	ctx := context.Background()
	builder, reviewer := env.agent(nil), env.agent(func(a *store.Agent) { a.Name = "Code Reviewer" })
	def := line(workflow.Node{ID: "one", AgentID: builder.ID, Task: "write"},
		workflow.Node{ID: "two", AgentID: reviewer.ID, Task: "Read runs/$RUN_ID/changes.diff."})
	def.Project = true
	wf := env.createWorkflow(def)
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := map[string]string{} // node → the changes file when its turn started
	var inCheckout, carried bool
	var message string
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		node, _ := visitOf(p.SessionKey)
		runDir := filepath.Join(turnWS(env, p), "runs", run.ID)
		got := "missing"
		if b, err := os.ReadFile(filepath.Join(runDir, project.ChangesFile)); err == nil {
			got = string(b)
		}
		mu.Lock()
		seen[node] = got
		mu.Unlock()
		switch node {
		case "one":
			writeFile(t, filepath.Join(runDir, project.RepoDir, "README.md"), "# demo\nedited by one\n")
			writeFile(t, filepath.Join(runDir, project.RepoDir, "cmd", "main.go"), "package main\n")
			writeFile(t, filepath.Join(runDir, project.ChangesFile), "a diff the builder wrote\n")
			writeFile(t, filepath.Join(runDir, "notes.txt"), "for the next step\n")
		case "two":
			_, err := os.Lstat(filepath.Join(runDir, project.RepoDir, project.ChangesFile))
			b, _ := os.ReadFile(filepath.Join(runDir, "notes.txt"))
			mu.Lock()
			inCheckout, carried, message = err == nil, string(b) == "for the next step\n", p.Message
			mu.Unlock()
		}
		go finishThenFinal(t, env, p, `{"ok":true}`)
		return startedTurn(p)
	}
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["one"] != "missing" {
		t.Fatalf("the first step found a changes file:\n%s", seen["one"])
	}
	for _, want := range []string{"diff --git a/README.md b/README.md", "+edited by one", "diff --git a/cmd/main.go b/cmd/main.go",
		"new file mode", "+package main"} {
		if !strings.Contains(seen["two"], want) {
			t.Errorf("the second step's changes file lacks %q:\n%s", want, seen["two"])
		}
	}
	if strings.Contains(seen["two"], "a diff the builder wrote") || !carried {
		t.Errorf("carry: the builder's own changes.diff reached the reviewer (%q), or its other file did not (%v)", seen["two"], carried)
	}
	if inCheckout {
		t.Error("the changes file is in the checkout")
	}
	if !strings.Contains(message, "Read runs/"+run.ID+"/changes.diff.") {
		t.Errorf("the task does not name the file:\n%s", message)
	}
}

// A file named like the changes file that a step writes is never carried to the next one,
// also in a run without a project, where the platform writes none: only the platform's own
// reaches a step.
func TestChangesFileIsNeverCarried(t *testing.T) {
	env := newEnv(t)
	one, two := env.agent(nil), env.agent(func(a *store.Agent) { a.Name = "Code Reviewer" })
	wf := env.createWorkflow(line(workflow.Node{ID: "one", AgentID: one.ID, Task: "write"}, workflow.Node{ID: "two", AgentID: two.ID, Task: "read"}))
	var runID string
	var mu sync.Mutex
	var present bool
	var notes string
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		node, _ := visitOf(p.SessionKey)
		mu.Lock()
		runDir := filepath.Join(turnWS(env, p), "runs", runID)
		mu.Unlock()
		if node == "one" {
			writeFile(t, filepath.Join(runDir, project.ChangesFile), "a diff the step wrote\n")
			writeFile(t, filepath.Join(runDir, "notes.txt"), "for the next step\n")
		} else {
			_, err := os.Lstat(filepath.Join(runDir, project.ChangesFile))
			b, _ := os.ReadFile(filepath.Join(runDir, "notes.txt"))
			mu.Lock()
			present, notes = err == nil, string(b)
			mu.Unlock()
		}
		go finishThenFinal(t, env, p, `{"ok":true}`)
		return startedTurn(p)
	}
	run, err := env.e.Trigger(context.Background(), TriggerRequest{WorkflowID: wf.ID})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	runID = run.ID
	mu.Unlock()
	if r := env.resume(env.e, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	mu.Lock()
	defer mu.Unlock()
	if present || notes != "for the next step\n" {
		t.Fatalf("the second step: changes file present %v, notes %q", present, notes)
	}
}
