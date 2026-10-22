package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"yuno/internal/openclaw"
	"yuno/internal/project/projecttest"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// T5.30.11 (DESIGN 5.30 rule 9, D-087): a run's first step clones the default branch as it is when
// the step starts, not as it was when the run was queued, and records that commit as the
// run's checkout_sha, the base its proposals count the run's changes from.
func TestProjectRunClonesTheDefaultBranchAtRunStart(t *testing.T) {
	env, bare, p := projectEnv(t, map[string]string{"README.md": "# demo\n"}, false)
	ctx := context.Background()
	coder := env.agent(nil)
	def := line(workflow.Node{ID: "one", AgentID: coder.ID, Task: "write"})
	def.Project = true
	wf := env.createWorkflow(def)
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}

	// Another ticket is merged after the run was queued and before its first step.
	work := filepath.Join(t.TempDir(), "hand")
	projecttest.Git(t, filepath.Dir(work), "clone", "--quiet", bare, work)
	writeFile(t, filepath.Join(work, "t2.txt"), "ticket two\n")
	projecttest.Git(t, work, "add", "--all")
	projecttest.Git(t, work, "commit", "--quiet", "-m", "T2")
	projecttest.Git(t, work, "push", "--quiet", "origin", "HEAD:refs/heads/main")
	head := projecttest.Git(t, bare, "rev-parse", "main")

	var mu sync.Mutex
	var seen string
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		b, _ := os.ReadFile(filepath.Join(turnWS(env, p), "runs", run.ID, "repo", "t2.txt"))
		mu.Lock()
		seen = string(b)
		mu.Unlock()
		go finishThenFinal(t, env, p, `{"ok":true}`)
		return startedTurn(p)
	}
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	mu.Lock()
	defer mu.Unlock()
	if seen != "ticket two\n" {
		t.Fatalf("the first step's checkout lacks the commit main gained before it started: t2.txt = %q", seen)
	}
	if deref(r.CheckoutSHA) != head {
		t.Fatalf("checkout_sha = %s, want main's head at the step's start %s", deref(r.CheckoutSHA), head)
	}
}
