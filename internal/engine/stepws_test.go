package engine

import (
	"context"
	"fmt"
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

// DESIGN 5.30: private step workspaces and the run repository. T5.30.1 is in the openclaw
// package (the path rule); T5.30.2 is TestProjectRunClonesCarriesAndProposes; T5.30.3 is
// TestBranchFilesCheckoutAndDiff with TestForkCarriesOnlyTheNamedBranchsCommit.

// Two runs of the same agent at once work in different private workspaces: what one
// writes is not in the other's, and nothing is in the agent's workspace.
func TestConcurrentRunsOfOneAgentDoNotShareAWorkspace(t *testing.T) {
	env := newEnv(t)
	a := env.agent(nil)
	both := make(chan openclaw.ChatSendParams, 2)
	release := make(chan struct{})
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		writeFile(t, filepath.Join(turnWS(env, p), "runs", tokenRunID(t, env, p), "mine.txt"), p.SessionKey)
		both <- p
		go func() {
			<-release
			finishThenFinal(t, env, p, `{"summary":"done"}`)
		}()
		return startedTurn(p)
	}
	r1, d1 := env.start(TriggerRequest{AgentID: a.ID, Task: "one"})
	r2, d2 := env.start(TriggerRequest{AgentID: a.ID, Task: "two"})
	p1, p2 := recv(t, both, "the first turn"), recv(t, both, "the second turn")
	ws1, ws2 := turnWS(env, p1), turnWS(env, p2)
	if ws1 == ws2 {
		t.Fatalf("both runs work in %s", ws1)
	}
	for _, c := range []struct{ ws, other string }{{ws1, tokenRunID(t, env, p2)}, {ws2, tokenRunID(t, env, p1)}} {
		if _, err := os.Lstat(filepath.Join(c.ws, "runs", c.other)); !os.IsNotExist(err) {
			t.Errorf("%s holds the other run's folder: %v", c.ws, err)
		}
	}
	close(release)
	for _, w := range []struct {
		run  *store.Run
		done <-chan struct{}
	}{{r1, d1}, {r2, d2}} {
		if r := env.wait(w.done, w.run.ID); r.Status != store.RunSucceeded {
			t.Fatalf("run %s = %s %q", w.run.ID, r.Status, deref(r.FailureReason))
		}
	}
	if _, err := os.Lstat(filepath.Join(openclaw.WorkspaceDir(env.dir, sessionAgent(p1.SessionKey)), "runs")); !os.IsNotExist(err) {
		t.Fatalf("the agent's workspace has a runs folder: %v", err)
	}
}

// tokenRunID is the run a turn belongs to, from its run token.
func tokenRunID(t *testing.T, env *testEnv, p openclaw.ChatSendParams) string {
	t.Helper()
	rc, ok := env.e.ResolveRunToken(tokenIn(p.Message))
	if !ok {
		t.Fatal("the turn's token does not resolve")
	}
	return rc.RunID
}

// T5.30.3: after a join that names branch a, the next visit's checkout is a's commit, not
// b's, and with no checkout named it is the commit from before the fork; the commits are
// on the visits.
func TestForkCarriesOnlyTheNamedBranchsCommit(t *testing.T) {
	for _, named := range []bool{true, false} {
		t.Run(map[bool]string{true: "checkout a", false: "no checkout"}[named], func(t *testing.T) {
			env, _, p := projectEnv(t, map[string]string{"README.md": "# demo\n", "go.mod": "module demo\n"}, false)
			agents := forkAgents(env, 4) // coder, a, b, lead
			checkout := ""
			if named {
				checkout = "a"
			}
			def := forkDef(agents, checkout)
			def.Project = true
			wf := env.createWorkflow(def)
			run, err := env.e.Trigger(context.Background(), TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID})
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			readme := map[string]string{}
			env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
				node, _ := visitOf(p.SessionKey)
				file := filepath.Join(turnWS(env, p), "runs", run.ID, "repo", "README.md")
				b, _ := os.ReadFile(file)
				mu.Lock()
				readme[node] = string(b)
				mu.Unlock()
				switch node {
				case "coder", "a", "b":
					writeFile(t, file, node+"'s tree\n")
				}
				go finishThenFinal(t, env, p, `{"verdict":"approved","ok":true}`)
				return startedTurn(p)
			}
			r := env.resume(env.e, run.ID)
			if r.Status != store.RunSucceeded {
				t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
			}
			sha := map[string]string{}
			for _, n := range runNodes(t, env, run.ID) {
				if n.AgentID != nil && n.RepoSHA != nil {
					sha[n.NodeID] = *n.RepoSHA
				}
			}
			if len(sha) != 4 || sha["a"] == sha["b"] || sha["a"] == sha["coder"] || sha["b"] == sha["coder"] {
				t.Fatalf("commits = %v", sha)
			}
			want, wantTree := sha["coder"], "coder's tree\n"
			if named {
				want, wantTree = sha["a"], "a's tree\n"
			}
			mu.Lock()
			defer mu.Unlock()
			if sha["lead"] != want || readme["lead"] != wantTree {
				t.Fatalf("lead started at %s with %q, want %s with %q", sha["lead"], readme["lead"], want, wantTree)
			}
			if readme["a"] != "coder's tree\n" || readme["b"] != "coder's tree\n" {
				t.Fatalf("branch heads started with a %q, b %q", readme["a"], readme["b"])
			}
		})
	}
}

// stepFlow is a project run one (coder, edits the checkout) → two (reviewer) → end whose
// first visit of two writes a partial file, optionally edits the checkout, and fails.
type stepFlow struct {
	env       *testEnv
	run       *store.Run
	bare      string
	failTwo   atomic.Bool
	editInTwo bool
	mu        sync.Mutex
	seen      map[string]string // "<node>#<visit>" → README | PARTIAL.md | partial.txt when the turn started
}

func newStepFlow(t *testing.T, editInTwo bool) *stepFlow {
	t.Helper()
	env, bare, p := projectEnv(t, map[string]string{"README.md": "# demo\n", "go.mod": "module demo\n"}, false)
	f := &stepFlow{env: env, bare: bare, editInTwo: editInTwo, seen: map[string]string{}}
	coder, reviewer := env.agent(nil), env.agent(func(a *store.Agent) { a.Name = "Reviewer" })
	def := line(workflow.Node{ID: "one", AgentID: coder.ID, Task: "write"}, workflow.Node{ID: "two", AgentID: reviewer.ID, Task: "review"})
	def.Project = true
	wf := env.createWorkflow(def)
	run, err := env.e.Trigger(context.Background(), TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	f.run = run
	f.failTwo.Store(true)
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
		f.mu.Lock()
		f.seen[fmt.Sprintf("%s#%d", node, visit)] = read("repo/README.md") + " | " + read("repo/PARTIAL.md") + " | " + read("partial.txt")
		f.mu.Unlock()
		switch {
		case node == "one":
			writeFile(t, filepath.Join(dir, "repo", "README.md"), "edited by one\n")
			go finishThenFinal(t, env, p, `{"ok":true}`)
		case node == "two" && f.failTwo.Load():
			writeFile(t, filepath.Join(dir, "partial.txt"), "half a review")
			if f.editInTwo {
				writeFile(t, filepath.Join(dir, "repo", "PARTIAL.md"), "half\n")
			}
			go env.fc.push("chat", chatEvent(p.SessionKey, p.IdempotencyKey, "error", map[string]any{
				"errorKind": "invalid_request", "errorMessage": "messages: bad shape",
				"errorDetail": map[string]any{"provider": "scripted", "model": "scripted-1", "httpStatus": 400}}))
		default:
			go finishThenFinal(t, env, p, `{"ok":true}`)
		}
		return startedTurn(p)
	}
	return f
}

func (f *stepFlow) found(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[key]
}

// visitSHA is the commit a visit of the run recorded.
func (f *stepFlow) visitSHA(t *testing.T, node string, visit int) string {
	t.Helper()
	for _, n := range runNodes(t, f.env, f.run.ID) {
		if n.NodeID == node && n.Visit == visit {
			return deref(n.RepoSHA)
		}
	}
	t.Fatalf("no visit %s#%d", node, visit)
	return ""
}

// T5.30.4 and T5.30.5: the run fails in its second step and keeps its steps' private
// workspaces and its run repository; with no folder of the run left in the agents'
// workspaces, the retried step starts from the earlier step's commit (or, when the failed
// visit edited the checkout, from its commit on top of that one) with the failed visit's
// other files; once the run succeeds the workspaces and the run repository are gone.
func TestRetryStartsFromTheEarlierStepsCommit(t *testing.T) {
	for _, edit := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed visit changed nothing", true: "failed visit edited the checkout"}[edit], func(t *testing.T) {
			f := newStepFlow(t, edit)
			env := f.env
			ctx := context.Background()
			failed := env.resume(env.e, f.run.ID)
			if failed.Status != store.RunFailed || deref(failed.CurrentNode) != "two" {
				t.Fatalf("first attempt = %s %q at %q", failed.Status, deref(failed.FailureReason), deref(failed.CurrentNode))
			}
			// T5.30.5: a failed run keeps them.
			nodes := runNodes(t, env, f.run.ID)
			for _, n := range nodes {
				if n.WorkspaceDir == nil || !present(*n.WorkspaceDir) {
					t.Fatalf("visit %s#%d: workspace %v not kept", n.NodeID, n.Visit, n.WorkspaceDir)
				}
			}
			runRepo := project.RunRepoPath(env.dir, f.run.ID)
			if !present(runRepo) {
				t.Fatal("the failed run's repository was not kept")
			}
			env.e.afterTerminal(ctx, failed)
			env.e.sweepRunDirs(ctx)
			if !present(runRepo) || !present(*nodes[0].WorkspaceDir) {
				t.Fatal("a failed run's folders went before seven days")
			}
			// The shared run folders of before 5.30 are gone; nothing of the run is there.
			entries, _ := filepath.Glob(filepath.Join(env.dir, "workspace-*", "runs"))
			if len(entries) != 0 {
				t.Fatalf("agent workspaces hold runs folders: %v", entries)
			}
			one, twoFailed := f.visitSHA(t, "one", 1), f.visitSHA(t, "two", 1)
			if one == "" || one == deref(failed.CheckoutSHA) {
				t.Fatalf("one's commit = %q (clone %s)", one, deref(failed.CheckoutSHA))
			}
			if edit != (twoFailed != one) {
				t.Fatalf("the failed visit's commit %s, one's %s", twoFailed, one)
			}
			if edit {
				if parent := projecttest.Git(t, runRepo, "rev-parse", twoFailed+"^"); parent != one {
					t.Fatalf("the failed visit's commit has parent %s, want %s", parent, one)
				}
			}

			f.failTwo.Store(false)
			env.clock.Advance(time.Hour)
			if _, err := env.e.Retry(ctx, f.run.ID, "tester", nil); err != nil {
				t.Fatal(err)
			}
			done := env.resume(env.e, f.run.ID)
			if done.Status != store.RunSucceeded {
				t.Fatalf("retried run = %s %q", done.Status, deref(done.FailureReason))
			}
			want := "edited by one\n | missing | half a review"
			if edit {
				want = "edited by one\n | half\n | half a review"
			}
			if got := f.found("two#2"); got != want {
				t.Fatalf("the retried visit started with %q, want %q", got, want)
			}
			if got := f.visitSHA(t, "two", 2); got != twoFailed {
				t.Fatalf("the retried visit is at %s, want the commit it started from %s", got, twoFailed)
			}
			// T5.30.5: a succeeded run's workspaces and run repository are gone.
			for _, n := range runNodes(t, env, f.run.ID) {
				if n.WorkspaceDir != nil && present(*n.WorkspaceDir) {
					t.Errorf("visit %s#%d: workspace still there", n.NodeID, n.Visit)
				}
			}
			if present(runRepo) {
				t.Error("the succeeded run's repository is still there")
			}
		})
	}
}

// T5.30.6: the sweep removes the private workspaces and run repositories of succeeded
// runs and of failed ones past seven days, keeps a recent failed run's, removes a run
// repository of a run id the database does not know once it is seven days old, and never
// touches a sandboxes/ folder no visit recorded (the Chief's chat session among them),
// however old.
func TestSweepPrivateWorkspacesAndRunRepositories(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	now := env.clock.Now()
	succeeded := env.endedRun(store.RunSucceeded, "", "")
	failedOld := env.endedRun(store.RunFailed, "build", "")
	failedNew := env.endedRun(store.RunFailed, "build", "")
	running := env.endedRun(store.RunRunning, "build", "")
	for id, ago := range map[string]time.Duration{failedOld.ID: 8 * 24 * time.Hour, failedNew.ID: 6 * 24 * time.Hour} {
		if _, err := env.pool.Exec(ctx, `UPDATE runs SET finished_at=$2 WHERE id=$1`, id, now.Add(-ago)); err != nil {
			t.Fatal(err)
		}
	}
	age := func(path string, ago time.Duration) {
		if err := os.Chtimes(path, now.Add(-ago), now.Add(-ago)); err != nil {
			t.Fatal(err)
		}
	}
	// One recorded workspace per run, and a run repository per run.
	ws := map[string]string{}
	repo := map[string]string{}
	for name, run := range map[string]*store.Run{"succeeded": succeeded, "failedOld": failedOld, "failedNew": failedNew, "running": running} {
		key := SessionKey("oc", run.ID, "build", 1)
		dir := openclaw.SessionWorkspaceDir(env.dir, "oc", key)
		writeFile(t, filepath.Join(dir, "runs", run.ID, "notes.md"), "n")
		n := &store.RunNode{RunID: run.ID, NodeID: "build", Visit: 1, Status: store.NodeFailed, SessionKey: &key, WorkspaceDir: &dir}
		if err := store.CreateRunNode(ctx, env.pool, n); err != nil {
			t.Fatal(err)
		}
		ws[name] = dir
		repo[name] = project.RunRepoPath(env.dir, run.ID)
		if err := project.InitRunRepo(ctx, repo[name]); err != nil {
			t.Fatal(err)
		}
	}
	unknownOld, unknownNew := "44444444-4444-4444-8444-444444444444", "55555555-5555-4555-8555-555555555555"
	for id, ago := range map[string]time.Duration{unknownOld: 8 * 24 * time.Hour, unknownNew: 6 * 24 * time.Hour} {
		repo[id] = project.RunRepoPath(env.dir, id)
		if err := project.InitRunRepo(ctx, repo[id]); err != nil {
			t.Fatal(err)
		}
		age(repo[id], ago)
	}
	// Folders under sandboxes/ that no visit recorded: a session's of the right shape (the
	// Chief's chat), and one of another name; both a month old.
	chief := openclaw.SessionWorkspaceDir(env.dir, "chief", "agent:chief:telegram:direct:42")
	stray := filepath.Join(env.dir, openclaw.SandboxesDir, "agent-main-1")
	for _, d := range []string{chief, stray} {
		writeFile(t, filepath.Join(d, "MEMORY.md"), "keep")
		age(d, 30*24*time.Hour)
	}

	env.e.sweepRunDirs(ctx)
	for name, keep := range map[string]bool{"succeeded": false, "failedOld": false, "failedNew": true, "running": true} {
		if present(ws[name]) != keep || present(repo[name]) != keep {
			t.Errorf("%s: workspace %v, repository %v, want %v", name, present(ws[name]), present(repo[name]), keep)
		}
	}
	if present(repo[unknownOld]) || !present(repo[unknownNew]) {
		t.Errorf("unknown run repositories: old %v, new %v", present(repo[unknownOld]), present(repo[unknownNew]))
	}
	for _, d := range []string{chief, stray} {
		if !present(filepath.Join(d, "MEMORY.md")) {
			t.Errorf("the sweep touched %s, which no visit recorded", d)
		}
	}
	// Two days later the recent failed run is past seven days too; the unrecorded folders
	// still stay.
	env.clock.Advance(2 * 24 * time.Hour)
	env.e.sweepRunDirs(ctx)
	if present(ws["failedNew"]) || present(repo["failedNew"]) || present(repo[unknownNew]) {
		t.Error("folders past seven days survived")
	}
	if !present(ws["running"]) || !present(chief) || !present(stray) {
		t.Error("the sweep removed a running run's or an unrecorded folder")
	}
}
