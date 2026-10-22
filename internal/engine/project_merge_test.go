package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"yuno/internal/openclaw"
	"yuno/internal/project/projecttest"
	"yuno/internal/store"
	"yuno/internal/tools"
	"yuno/internal/workflow"
)

// T5.22.14, the acceptance test of D-088 (DESIGN 5.22 rule 13, 5.30 rule 10): a run on a
// request whose branch and main changed the same file. The first step's checkout has
// origin/main with its history and main merged in, the conflicting file holds the markers
// and the task names it; the agent's resolution, a file edit, reaches the next step, whose
// task says the merge is pending without conflicts; open_merge_request then pushes a merge
// commit of the branch and main, and the branch merges into main without a conflict.
func TestRunOnAConflictingRequestResolvesAndPushesAMerge(t *testing.T) {
	f := newIterFixture(t)
	env, ctx := f.env, context.Background()
	pushFiles(t, f.bare, iterBranch, map[string]string{"app.txt": "the branch's line\n"})
	pushFiles(t, f.bare, "main", map[string]string{"app.txt": "main's line\n"})
	branchHead := projecttest.Git(t, f.bare, "rev-parse", iterBranch)
	mainHead := projecttest.Git(t, f.bare, "rev-parse", "main")

	builder, reviewer := env.agent(nil), env.agent(func(a *store.Agent) { a.Name = "Reviewer" })
	def := line(workflow.Node{ID: "one", AgentID: builder.ID, Task: "resolve"}, workflow.Node{ID: "two", AgentID: reviewer.ID, Task: "review"})
	def.Project = true
	wf := env.createWorkflow(def)
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, ProjectID: f.proj.ID, MergeRequestID: f.rec.ID})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "run")
	var mu sync.Mutex
	messages := map[string]string{}
	var atStart, originMain, atTwo string
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		node, _ := visitOf(p.SessionKey)
		repo := filepath.Join(turnWS(env, p), "runs", run.ID, "repo")
		mu.Lock()
		defer mu.Unlock()
		messages[node] = p.Message
		switch node {
		case "one":
			atStart = repoFile(env, p, run.ID, "app.txt")
			originMain = projecttest.Git(t, repo, "rev-parse", "refs/remotes/origin/main")
			writeFile(t, filepath.Join(repo, "app.txt"), "both lines\n") // the agent resolves the conflict
		case "two":
			atTwo = repoFile(env, p, run.ID, "app.txt")
			if out, err := exec.Command("cp", "-a", filepath.Dir(repo), snapshot).CombinedOutput(); err != nil {
				t.Errorf("snapshot: %v %s", err, out)
			}
		}
		go finishThenFinal(t, env, p, `{"summary":"done"}`)
		return startedTurn(p)
	}
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"<<<<<<< ", "the branch's line", "main's line", ">>>>>>> "} {
		if !strings.Contains(atStart, want) {
			t.Fatalf("app.txt at the first turn lacks %q:\n%s", want, atStart)
		}
	}
	if originMain != mainHead {
		t.Fatalf("origin/main in the checkout = %s, want %s", originMain, mainHead)
	}
	const merged = "Merge: the platform merged origin/main into " + iterBranch + " in the checkout before the run's first step"
	if want := merged + ", and these files have conflicts: app.txt. Resolve the conflict markers in each of them first"; !strings.Contains(messages["one"], want) {
		t.Fatalf("the first task lacks %q:\n%s", want, messages["one"])
	}
	if want := merged + "; open_merge_request pushes it as a merge commit with your changes."; !strings.Contains(messages["two"], want) {
		t.Fatalf("the second task lacks %q:\n%s", want, messages["two"])
	}
	if atTwo != "both lines\n" {
		t.Fatalf("the second step's app.txt = %q", atTwo)
	}
	if deref(r.CheckoutSHA) != branchHead || deref(r.MergedSHA) != mainHead {
		t.Fatalf("checkout_sha %s (want %s), merged_sha %s (want %s)", deref(r.CheckoutSHA), branchHead, deref(r.MergedSHA), mainHead)
	}
	if n := env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='log' AND data::text LIKE $2`, run.ID,
		"%merged main into "+iterBranch+": conflicts in app.txt%"); n != 1 {
		t.Fatalf("merge log lines = %d", n)
	}

	// open_merge_request from the second step's checkout, through the executor.
	ws := t.TempDir()
	dir := filepath.Join(ws, "runs", run.ID)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cp", "-a", snapshot, dir).CombinedOutput(); err != nil {
		t.Fatalf("restore: %v %s", err, out)
	}
	tool := tools.NewOpenMergeRequest(tools.MergeRequestConfig{Projects: env.e.projects, APIBase: "http://127.0.0.1:1/api/v4/projects/g%2Fp"})
	input := json.RawMessage(`{"run_token":"r","title":"Merge main and keep both lines","description":"d"}`)
	key, err := tools.IdempotencyKey(run.ID, "visit-two", tool.Name, input)
	if err != nil {
		t.Fatal(err)
	}
	svc := &tools.Service{Pool: env.pool, Registry: tools.NewRegistry()}
	out, err := svc.Execute(ctx, tool, tools.Call{RunID: run.ID, Workspace: ws, IdempotencyKey: key, ToolCallID: store.NewID(), Input: input})
	if err != nil || out.Result.IsError {
		t.Fatalf("open_merge_request: %+v %v", out, err)
	}
	tip := projecttest.Git(t, f.bare, "rev-parse", iterBranch)
	if projecttest.Git(t, f.bare, "rev-parse", iterBranch+"^1") != branchHead || projecttest.Git(t, f.bare, "rev-parse", iterBranch+"^2") != mainHead {
		t.Fatalf("the pushed %s is not a merge of the branch %s and main %s", tip, branchHead, mainHead)
	}
	for file, want := range map[string]string{"app.txt": "both lines\n", "later.txt": "main moved on\n", "feature.txt": "the branch's work\n"} {
		if got := projecttest.FileAt(t, f.bare, iterBranch, file); got != want {
			t.Fatalf("%s on the branch = %q, want %q", file, got, want)
		}
	}
	work := filepath.Join(t.TempDir(), "merge")
	projecttest.Git(t, filepath.Dir(work), "clone", "--quiet", "--branch", "main", f.bare, work)
	projecttest.Git(t, work, "merge", "--quiet", "--no-ff", "--no-edit", "origin/"+iterBranch)
	if b, _ := os.ReadFile(filepath.Join(work, "app.txt")); string(b) != "both lines\n" {
		t.Fatalf("app.txt after merging the request into main = %q", b)
	}
	after, err := store.GetRun(ctx, env.pool, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.MergedSHA != nil || deref(after.CheckoutSHA) != tip {
		t.Fatalf("after the push: merged_sha %v, checkout_sha %s (tip %s)", after.MergedSHA, deref(after.CheckoutSHA), tip)
	}
}
