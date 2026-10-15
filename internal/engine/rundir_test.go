package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"yuno/internal/openclaw"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Error(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Error(err)
	}
}

// T5.6.12: a file node 1's agent wrote is in node 2's agent workspace before its turn;
// the file node 2 rewrote wins on node 3 (a third agent) and on node 4 (node 1's agent
// again); node 5, the same agent as node 4, keeps node 4's version. Links are not copied
// (DESIGN 5.6 rule 13).
func TestRunDirectoryFlowsForward(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	coder, reviewer := env.agent(nil), env.agent(func(a *store.Agent) { a.Name = "Reviewer" })
	deployer := env.agent(func(a *store.Agent) { a.Name = "Deployer" })
	wf := env.createWorkflow(line(workflow.Node{ID: "one", AgentID: coder.ID, Task: "write"},
		workflow.Node{ID: "two", AgentID: reviewer.ID, Task: "review"},
		workflow.Node{ID: "three", AgentID: deployer.ID, Task: "deploy"},
		workflow.Node{ID: "four", AgentID: coder.ID, Task: "revise"},
		workflow.Node{ID: "five", AgentID: coder.ID, Task: "polish"}))
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID})
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "outside")
	// path is a file in the run directory of the workspace the visit's turn works in.
	path := func(p openclaw.ChatSendParams, rel string) string {
		return filepath.Join(env.dir, "workspace-"+sessionAgent(p.SessionKey), "runs", run.ID, rel)
	}
	var mu sync.Mutex
	found := map[string]string{} // node → the files its agent had when its turn started
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		read := func(rel string) string {
			b, err := os.ReadFile(path(p, rel))
			if err != nil {
				return "missing"
			}
			return string(b)
		}
		node, _ := visitOf(p.SessionKey)
		mu.Lock()
		found[node] = read("notes.md") + " | " + read("src/app.py")
		mu.Unlock()
		switch node {
		case "one":
			writeFile(t, path(p, "notes.md"), "draft by one")
			writeFile(t, path(p, "src/app.py"), "print(1)")
			if err := os.Symlink(filepath.Join(outside, "secret.txt"), path(p, "secret.txt")); err != nil {
				t.Error(err)
			}
		case "two":
			if _, err := os.Lstat(path(p, "secret.txt")); !os.IsNotExist(err) {
				t.Errorf("a link was copied forward: %v", err)
			}
			writeFile(t, path(p, "notes.md"), "reviewed by two")
		case "four":
			writeFile(t, path(p, "notes.md"), "revised by four")
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
	want := map[string]string{
		"one":   "missing | missing",
		"two":   "draft by one | print(1)",
		"three": "reviewed by two | print(1)",
		"four":  "reviewed by two | print(1)",
		"five":  "revised by four | print(1)",
	}
	for node, w := range want {
		if found[node] != w {
			t.Errorf("%s started with %q, want %q", node, found[node], w)
		}
	}
}

// A run directory that cannot be copied fails the node with reason "run directory:
// <error>" before its turn starts (DESIGN 5.6 rule 13).
func TestRunDirectoryCopyFailureFailsTheNode(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	coder, reviewer := env.agent(nil), env.agent(func(a *store.Agent) { a.Name = "Reviewer" })
	wf := env.createWorkflow(line(workflow.Node{ID: "one", AgentID: coder.ID, Task: "write"},
		workflow.Node{ID: "two", AgentID: reviewer.ID, Task: "review"}))
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID})
	if err != nil {
		t.Fatal(err)
	}
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		ws := filepath.Join(env.dir, "workspace-"+sessionAgent(p.SessionKey))
		writeFile(t, filepath.Join(ws, "runs", run.ID, "notes.md"), "draft")
		// The reviewer's run directory is taken by a file: nothing can be copied into it.
		writeFile(t, filepath.Join(env.dir, "workspace-"+openclaw.DeriveAgentID(reviewer.ID), "runs", run.ID), "in the way")
		go finishThenFinal(t, env, p, `{"ok":true}`)
		return startedTurn(p)
	}
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunFailed || !strings.HasPrefix(deref(r.FailureReason), "run directory: copy from ") {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if got := visitsOf(runNodes(t, env, r.ID)); !slices.Equal(got, []string{"one#1:succeeded", "two#1:failed"}) {
		t.Fatalf("visits = %v", got)
	}
	if n := len(env.fc.sendParams()); n != 1 {
		t.Fatalf("%d turns started; the failed copy must stop the node before its turn", n)
	}
}
