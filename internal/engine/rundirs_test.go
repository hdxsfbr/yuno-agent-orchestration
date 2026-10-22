package engine

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// runDirOf is the run's folder in an agent's workspace (agent is the workspace suffix).
func (env *testEnv) runDirOf(agent, runID string) string {
	return filepath.Join(env.dir, "workspace-"+agent, "runs", runID)
}

func (env *testEnv) plant(agent, runID string) string {
	env.t.Helper()
	d := env.runDirOf(agent, runID)
	writeFile(env.t, filepath.Join(d, "repo", "main.go"), "package main")
	return d
}

func present(path string) bool { _, err := os.Lstat(path); return err == nil }

// A run that ends succeeded has its visits' private workspaces removed (DESIGN 5.30 rule
// 8), and a runs/<id>/ an agent workspace still holds from before 5.30; another run's
// folder stays.
func TestRunDirsRemovedWhenRunSucceeds(t *testing.T) {
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
		writeFile(t, filepath.Join(turnWS(env, p), "runs", run.ID, "notes.md"), "n")
		go finishThenFinal(t, env, p, `{"ok":true}`)
		return startedTurn(p)
	}
	other := env.plant("unrelated", "33333333-3333-4333-8333-333333333333")
	old := env.plant(openclaw.DeriveAgentID(coder.ID), run.ID)
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	nodes := runNodes(t, env, run.ID)
	if len(nodes) != 2 {
		t.Fatalf("visits = %v", visitsOf(nodes))
	}
	for _, n := range nodes {
		if n.WorkspaceDir == nil || present(*n.WorkspaceDir) {
			t.Errorf("visit %s: workspace %v still there", n.NodeID, n.WorkspaceDir)
		}
	}
	if present(old) {
		t.Error("the agent workspace's runs folder of the run was left")
	}
	if !present(other) {
		t.Error("another run's folder was removed")
	}
}

// A cancelled run keeps its folders: Retry accepts it and resumes on them.
func TestRunDirsKeptWhenRunCancelled(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	wf := env.createWorkflow(line(workflow.Node{ID: "one", AgentID: a.ID, Task: "write"}))
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID})
	if err != nil {
		t.Fatal(err)
	}
	dir := env.plant("a", run.ID)
	got, err := env.e.Cancel(ctx, run.ID, "tester")
	if err != nil || got.Status != store.RunCancelled {
		t.Fatalf("cancel = %+v, %v", got, err)
	}
	if !present(dir) {
		t.Error("a cancelled run lost its folder")
	}
}

// A failed or timed-out run keeps its folders: Retry resumes on them.
func TestRunDirsKeptWhenRunFails(t *testing.T) {
	env := newEnv(t)
	for _, status := range []string{store.RunFailed, store.RunTimedOut} {
		run := env.endedRun(status, "build", "")
		dir := env.plant("a", run.ID)
		env.e.afterTerminal(context.Background(), run)
		if !present(dir) {
			t.Errorf("a %s run lost its folder", status)
		}
	}
}

// The sweep removes finished runs' folders, keeps failed ones for seven days, and
// removes a folder no run owns once it is seven days old.
func TestSweepRunDirs(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	succeeded := env.endedRun(store.RunSucceeded, "", "")
	cancelled := env.endedRun(store.RunCancelled, "", "")
	cancelledOld := env.endedRun(store.RunCancelled, "", "")
	failed := env.endedRun(store.RunFailed, "build", "")
	timedOut := env.endedRun(store.RunTimedOut, "build", "")
	running := env.endedRun(store.RunRunning, "build", "")
	unknownOld, unknownNew := "44444444-4444-4444-8444-444444444444", "55555555-5555-4555-8555-555555555555"
	dirs := map[string]string{}
	for name, id := range map[string]string{"succeeded": succeeded.ID, "cancelled": cancelled.ID, "cancelledOld": cancelledOld.ID, "failed": failed.ID,
		"timedOut": timedOut.ID, "running": running.ID, "unknownOld": unknownOld, "unknownNew": unknownNew} {
		dirs[name] = env.plant("a", id)
		env.plant("b", id)
	}
	// finished_at is set in the engine's clock: failed ended 6 days ago, timed out 8.
	now := env.clock.Now()
	set := func(id string, ago time.Duration) {
		if _, err := env.pool.Exec(ctx, `UPDATE runs SET finished_at=$2 WHERE id=$1`, id, now.Add(-ago)); err != nil {
			t.Fatal(err)
		}
	}
	set(failed.ID, 6*24*time.Hour)
	set(timedOut.ID, 8*24*time.Hour)
	set(cancelled.ID, 6*24*time.Hour)
	set(cancelledOld.ID, 8*24*time.Hour)
	age := func(path string, ago time.Duration) {
		if err := os.Chtimes(path, now.Add(-ago), now.Add(-ago)); err != nil {
			t.Fatal(err)
		}
	}
	for _, ws := range []string{"a", "b"} {
		age(env.runDirOf(ws, unknownOld), 8*24*time.Hour)
		age(env.runDirOf(ws, unknownNew), 6*24*time.Hour)
	}

	env.e.sweepRunDirs(ctx)
	want := map[string]bool{"succeeded": false, "cancelled": true, "cancelledOld": false, "failed": true, "timedOut": false,
		"running": true, "unknownOld": false, "unknownNew": true}
	for name, keep := range want {
		if present(dirs[name]) != keep {
			t.Errorf("%s: present = %v, want %v", name, !keep, keep)
		}
	}
	if present(env.runDirOf("b", succeeded.ID)) {
		t.Error("the sweep left a copy in the second workspace")
	}

	// Two days later the failed run is past seven days as well, and the 6-day-old
	// unknown folder is past it; the running run is never swept.
	env.clock.Advance(2 * 24 * time.Hour)
	env.e.sweepRunDirs(ctx)
	for _, name := range []string{"failed", "unknownNew"} {
		if present(dirs[name]) {
			t.Errorf("%s survived past seven days", name)
		}
	}
	if !present(dirs["running"]) {
		t.Error("a running run's folder was swept")
	}
}

// A link planted as runs/<id> is not followed by the sweep.
func TestSweepRunDirsDoesNotFollowLinks(t *testing.T) {
	env := newEnv(t)
	run := env.endedRun(store.RunSucceeded, "", "")
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "outside")
	if err := os.MkdirAll(filepath.Join(env.dir, "workspace-a", "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := env.runDirOf("a", run.ID)
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	env.e.sweepRunDirs(context.Background())
	if !present(filepath.Join(outside, "secret.txt")) {
		t.Fatal("the sweep followed a link out of the workspace")
	}
	if present(link) {
		t.Error("the planted link was left in place")
	}
}

// keptStore stands in for the artifact collector, which lives in internal/artifacts and
// imports this package. Like the real one (DESIGN 5.25) it reads a finished visit's
// private workspace and keeps, in a store outside the state directory, the files under
// runs/<run id>/out/ and, for a project run, the checkout's diff as diff.patch.
type keptStore struct {
	t   *testing.T
	dir string
}

func (k *keptStore) Collect(ctx context.Context, in ArtifactInput) {
	k.t.Helper()
	runDir := filepath.Join(in.Workspace, "runs", in.Run.ID)
	if !present(runDir) {
		k.t.Errorf("visit %s#%d: no run folder to collect from in %s", in.Node.NodeID, in.Node.Visit, in.Workspace)
		return
	}
	out := filepath.Join(runDir, "out")
	_ = filepath.WalkDir(out, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			k.t.Error(err)
			return nil
		}
		rel, _ := filepath.Rel(out, path)
		writeFile(k.t, filepath.Join(k.dir, in.Run.ID, rel), string(b))
		return nil
	})
	if in.Project == nil {
		return
	}
	patch, err := in.Project.Diff(ctx, filepath.Join(runDir, project.RepoDir), in.Branch, deref(in.Run.CheckoutSHA))
	if err != nil {
		k.t.Errorf("visit %s#%d: diff: %v", in.Node.NodeID, in.Node.Visit, err)
		return
	}
	if len(patch) > 0 {
		writeFile(k.t, filepath.Join(k.dir, in.Run.ID, "diff.patch"), string(patch))
	}
}

// filesUnder lists the regular files under dir as sorted paths relative to it.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, path)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// The P3 acceptance for cleanup (DESIGN 5.30 rule 8): a project run that succeeds leaves
// only its artifacts, the files its steps put under out/ and the diff, which the
// collector copied out of the steps' workspaces while they still existed. Its steps'
// private workspaces and its run repository are gone, and nothing under the state
// directory names the run any more.
func TestSucceededRunLeavesOnlyItsArtifacts(t *testing.T) {
	env, _, p := projectEnv(t, map[string]string{"README.md": "# demo\n", "go.mod": "module demo\n"}, false)
	ctx := context.Background()
	kept := &keptStore{t: t, dir: t.TempDir()}
	env.e.deps.Artifacts = kept
	coder, reviewer := env.agent(nil), env.agent(func(a *store.Agent) { a.Name = "Reviewer" })
	def := line(workflow.Node{ID: "one", AgentID: coder.ID, Task: "write"},
		workflow.Node{ID: "two", AgentID: reviewer.ID, Task: "review"})
	def.Project = true
	wf := env.createWorkflow(def)
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		node, _ := visitOf(p.SessionKey)
		runDir := filepath.Join(turnWS(env, p), "runs", run.ID)
		switch node {
		case "one":
			writeFile(t, filepath.Join(runDir, "repo", "README.md"), "edited by one\n")
			writeFile(t, filepath.Join(runDir, "out", "report.md"), "the report")
			writeFile(t, filepath.Join(runDir, "scratch", "notes.txt"), "working notes")
		case "two":
			writeFile(t, filepath.Join(runDir, "out", "review", "summary.md"), "the review")
		}
		go finishThenFinal(t, env, p, `{"ok":true}`)
		return startedTurn(p)
	}
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}

	// The artifacts are in the store: out/ as the steps left it, and the diff.
	stored := filepath.Join(kept.dir, run.ID)
	if got, want := filesUnder(t, stored), []string{"diff.patch", "report.md", "review/summary.md"}; !slices.Equal(got, want) {
		t.Fatalf("the store holds %v, want %v", got, want)
	}
	for rel, want := range map[string]string{"report.md": "the report", "review/summary.md": "the review"} {
		if b, err := os.ReadFile(filepath.Join(stored, rel)); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v; want %q", rel, b, err, want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(stored, "diff.patch")); !strings.Contains(string(b), "+edited by one") {
		t.Errorf("diff.patch = %q, want the change of step one", b)
	}

	// Everything else of the run is gone: the steps' workspaces (the scratch notes with
	// them), the run repository, and any trace under the state directory.
	nodes := runNodes(t, env, run.ID)
	if len(nodes) != 2 {
		t.Fatalf("visits = %v", visitsOf(nodes))
	}
	for _, n := range nodes {
		if n.WorkspaceDir == nil || present(*n.WorkspaceDir) {
			t.Errorf("visit %s: workspace %q still there, or never recorded", n.NodeID, deref(n.WorkspaceDir))
		}
	}
	if left, _ := openclaw.ListSandboxWorkspaces(env.dir); len(left) > 0 {
		t.Errorf("private workspaces left: %v", left)
	}
	if repo := project.RunRepoPath(env.dir, run.ID); present(repo) {
		t.Errorf("the run repository %s is still there", repo)
	}
	if found := tracesIn(t, env.dir, run.ID); len(found) > 0 {
		t.Errorf("the state directory still names the run: %v", found)
	}
}
