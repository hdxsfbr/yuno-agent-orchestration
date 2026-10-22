package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/project"
	"yuno/internal/project/projecttest"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// checkoutFlow is a project run one (the coder, edits README.md in the checkout) → two
// (the reviewer) → end. It records what each turn of two found in its checkout; what the
// first visit of two does is up to the test: its turn is held open (holdTwo), it fails
// (failTwo), or its setup never gets that far.
type checkoutFlow struct {
	env     *testEnv
	run     *store.Run
	mu      sync.Mutex
	seen    map[int]string // visit of two → "<README.md> | <PARTIAL.md>" when its turn started
	held    chan int       // two's visits whose turn started and is held open
	holdTwo bool           // two's first visit edits PARTIAL.md and its turn never ends
	failTwo bool           // two's first visit fails during its turn
	// onOne runs during one's turn, after its edit: the test breaks two's setup there.
	onOne func()
}

func newCheckoutFlow(t *testing.T) *checkoutFlow {
	t.Helper()
	env, _, p := projectEnv(t, map[string]string{"README.md": "# demo\n", "go.mod": "module demo\n"}, false)
	f := &checkoutFlow{env: env, seen: map[int]string{}, held: make(chan int, 4)}
	coder, reviewer := env.agent(nil), env.agent(func(a *store.Agent) { a.Name = "Reviewer" })
	def := line(workflow.Node{ID: "one", AgentID: coder.ID, Task: "write"}, workflow.Node{ID: "two", AgentID: reviewer.ID, Task: "review"})
	def.Project = true
	wf := env.createWorkflow(def)
	run, err := env.e.Trigger(context.Background(), TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	f.run = run
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		node, visit := visitOf(p.SessionKey)
		dir := filepath.Join(turnWS(env, p), "runs", run.ID)
		read := func(rel string) string {
			b, err := os.ReadFile(filepath.Join(dir, rel))
			if err != nil {
				return "missing"
			}
			return string(b)
		}
		switch node {
		case "one":
			writeFile(t, filepath.Join(dir, "repo", "README.md"), "edited by one\n")
			if f.onOne != nil {
				f.onOne()
			}
			go finishThenFinal(t, env, p, `{"ok":true}`)
		case "two":
			f.mu.Lock()
			f.seen[visit] = read("repo/README.md") + " | " + read("repo/PARTIAL.md")
			hold, fail := f.holdTwo && visit == 1, f.failTwo && visit == 1
			f.mu.Unlock()
			switch {
			case hold:
				writeFile(t, filepath.Join(dir, "repo", "PARTIAL.md"), "half\n")
				f.held <- visit
			case fail:
				go env.fc.push("chat", chatEvent(p.SessionKey, p.IdempotencyKey, "error", map[string]any{
					"errorKind": "invalid_request", "errorMessage": "messages: bad shape",
					"errorDetail": map[string]any{"provider": "scripted", "model": "scripted-1", "httpStatus": 400}}))
			default:
				go finishThenFinal(t, env, p, `{"ok":true}`)
			}
		}
		return startedTurn(p)
	}
	return f
}

func (f *checkoutFlow) found(visit int) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.seen[visit]
	return s, ok
}

func (f *checkoutFlow) visit(t *testing.T, node string, n int) store.RunNode {
	t.Helper()
	for _, rn := range runNodes(t, f.env, f.run.ID) {
		if rn.NodeID == node && rn.Visit == n {
			return rn
		}
	}
	t.Fatalf("no visit %s#%d", node, n)
	return store.RunNode{}
}

// breakTwoSetup makes the gateway's agents.list, which the reviewer's first materialisation
// calls, wait for its context (block) or fail (fail): two's first visit then never gets as
// far as its private workspace. one's own materialisation is done by the time its turn runs.
func (f *checkoutFlow) breakTwoSetup(block bool) (undo func()) {
	f.onOne = func() {
		f.env.fc.mu.Lock()
		if block {
			f.env.fc.block["agents.list"] = true
		} else {
			f.env.fc.fail["agents.list"] = errors.New("fake: gateway restarting")
		}
		f.env.fc.mu.Unlock()
	}
	return func() {
		f.env.fc.mu.Lock()
		delete(f.env.fc.block, "agents.list")
		delete(f.env.fc.fail, "agents.list")
		f.env.fc.mu.Unlock()
	}
}

// T5.30.14: a step the engine shut down under (a deploy restarting the api) is run again
// by the replacement visit the reclaim starts, with the checkout the carry rules name: the
// interrupted visit's own commit when it had a checkout, which the shutdown saved, and the
// earlier step's commit when it was stopped before it had a workspace, which once made the
// replacement start with no checkout at all.
func TestRecoveredStepGetsItsCheckout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		during bool // interrupted during its turn, or while it was being set up
	}{{"interrupted during its turn", true}, {"interrupted before it had its workspace", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCheckoutFlow(t)
			env := f.env
			ctx := context.Background()
			undo := func() {}
			var setupCalls atomic.Int64
			if tc.during {
				f.holdTwo = true
			} else {
				undo = f.breakTwoSetup(true)
				prev := f.onOne
				f.onOne = func() {
					prev()
					setupCalls.Store(int64(len(env.fc.callsFor("agents.list"))))
				}
			}
			claimed, err := store.ClaimQueuedRun(ctx, env.pool)
			if err != nil || claimed.ID != f.run.ID {
				t.Fatalf("claim: %+v %v", claimed, err)
			}
			wctx, shutdown := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() {
				defer close(done)
				env.e.runRun(wctx, claimed)
			}()
			if tc.during {
				recv(t, f.held, "two's first turn")
			} else {
				eventually(t, "two's setup waiting on the gateway", func() bool {
					n := setupCalls.Load()
					return n > 0 && int64(len(env.fc.callsFor("agents.list"))) > n
				})
			}
			shutdown()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("worker did not stop")
			}
			undo()
			one, lost := f.visit(t, "one", 1), f.visit(t, "two", 1)
			if lost.Status != store.NodeRunning || (lost.WorkspaceDir == nil) == tc.during || (lost.RepoSHA == nil) == tc.during {
				t.Fatalf("the interrupted visit: %s, workspace %v, commit %q", lost.Status, lost.WorkspaceDir, deref(lost.RepoSHA))
			}
			want, wantSHA := "edited by one\n | missing", deref(one.RepoSHA)
			if tc.during {
				// The shutdown saved the interrupted visit's checkout: its commit is on top of
				// one's, and the replacement starts from it.
				want, wantSHA = "edited by one\n | half\n", deref(lost.RepoSHA)
				if parent := projecttest.Git(t, project.RunRepoPath(env.dir, f.run.ID), "rev-parse", wantSHA+"^"); parent != deref(one.RepoSHA) {
					t.Fatalf("the interrupted visit's commit %s has parent %s, want one's %s", wantSHA, parent, deref(one.RepoSHA))
				}
			}

			// The api is back: the stale-run sweep re-queues the run and a worker reclaims it.
			if n, _, err := store.RequeueStaleRuns(ctx, env.pool, -time.Hour); err != nil || n != 1 {
				t.Fatalf("requeue: %d %v", n, err)
			}
			r := env.resume(env.e, f.run.ID)
			if r.Status != store.RunSucceeded || r.Attempt != 2 {
				t.Fatalf("reclaimed run = %s %q (attempt %d)", r.Status, deref(r.FailureReason), r.Attempt)
			}
			lost, again := f.visit(t, "two", 1), f.visit(t, "two", 2)
			if lost.Status != store.NodeFailed || deref(lost.FailureReason) != "worker lost" {
				t.Fatalf("the interrupted visit after the reclaim: %s %q", lost.Status, deref(lost.FailureReason))
			}
			if got, _ := f.found(2); got != want {
				t.Fatalf("the replacement visit's checkout held %q, want %q", got, want)
			}
			if wantSHA == "" || deref(again.RepoSHA) != wantSHA {
				t.Fatalf("the replacement visit is at %q, want %q", deref(again.RepoSHA), wantSHA)
			}
		})
	}
}

// T5.30.15: Retry of a failed later step (rule 7) starts from the commit the carry rules
// name, whether the failed visit had its checkout or failed before it had a workspace (a
// gateway that was restarting when the step was set up); that visit used to hide the
// earlier step's commit, and the retried step ran with no checkout.
func TestRetriedStepGetsItsCheckout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		during bool
	}{{"failed during its turn", true}, {"failed before it had its workspace", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCheckoutFlow(t)
			env := f.env
			ctx := context.Background()
			undo := func() {}
			if tc.during {
				f.failTwo = true
			} else {
				undo = f.breakTwoSetup(false)
			}
			failed := env.resume(env.e, f.run.ID)
			if failed.Status != store.RunFailed || deref(failed.CurrentNode) != "two" {
				t.Fatalf("first attempt = %s %q at %q", failed.Status, deref(failed.FailureReason), deref(failed.CurrentNode))
			}
			undo()
			one, first := f.visit(t, "one", 1), f.visit(t, "two", 1)
			if (first.WorkspaceDir == nil) == tc.during || (first.RepoSHA == nil) == tc.during {
				t.Fatalf("the failed visit: workspace %v, commit %q", first.WorkspaceDir, deref(first.RepoSHA))
			}
			if _, turned := f.found(1); turned != tc.during {
				t.Fatalf("the failed visit had a turn: %v", turned)
			}

			env.clock.Advance(time.Hour)
			if _, err := env.e.Retry(ctx, f.run.ID, "tester", nil); err != nil {
				t.Fatal(err)
			}
			done := env.resume(env.e, f.run.ID)
			if done.Status != store.RunSucceeded {
				t.Fatalf("retried run = %s %q", done.Status, deref(done.FailureReason))
			}
			if got, _ := f.found(2); got != "edited by one\n | missing" {
				t.Fatalf("the retried visit's checkout held %q", got)
			}
			if got := deref(f.visit(t, "two", 2).RepoSHA); got == "" || got != deref(one.RepoSHA) {
				t.Fatalf("the retried visit is at %q, want one's commit %q", got, deref(one.RepoSHA))
			}
		})
	}
}

// T5.30.16: a later step of a project run whose run repository is there, but for which no
// earlier visit recorded a commit to start from, fails before its first turn with the
// reason, instead of running with no checkout.
func TestStepWithNoCommitToStartFromFailsBeforeItsTurn(t *testing.T) {
	f := newCheckoutFlow(t)
	env := f.env
	ctx := context.Background()
	f.failTwo = true
	if r := env.resume(env.e, f.run.ID); r.Status != store.RunFailed || deref(r.CurrentNode) != "two" {
		t.Fatalf("first attempt = %s %q at %q", r.Status, deref(r.FailureReason), deref(r.CurrentNode))
	}
	if !present(project.RunRepoPath(env.dir, f.run.ID)) {
		t.Fatal("the run has no run repository")
	}
	// The commits the visits recorded are lost.
	if _, err := env.pool.Exec(ctx, `UPDATE run_nodes SET repo_sha = NULL WHERE run_id = $1`, f.run.ID); err != nil {
		t.Fatal(err)
	}
	sends := len(env.fc.sendParams())
	if _, err := env.e.Retry(ctx, f.run.ID, "tester", nil); err != nil {
		t.Fatal(err)
	}
	r := env.resume(env.e, f.run.ID)
	const reason = "run repository: no earlier step recorded a commit for this step's checkout to start from"
	if r.Status != store.RunFailed || deref(r.FailureReason) != reason {
		t.Fatalf("retried run = %s %q", r.Status, deref(r.FailureReason))
	}
	second := f.visit(t, "two", 2)
	if second.Status != store.NodeFailed || deref(second.FailureReason) != reason {
		t.Fatalf("the step: %s %q", second.Status, deref(second.FailureReason))
	}
	if _, turned := f.found(2); turned || len(env.fc.sendParams()) != sends {
		t.Fatalf("the step had a turn (%d chat.send, %d before)", len(env.fc.sendParams()), sends)
	}
}
