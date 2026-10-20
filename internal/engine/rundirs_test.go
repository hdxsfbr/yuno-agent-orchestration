package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"yuno/internal/openclaw"
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

// A run that ends succeeded has its folders removed from every workspace, including the
// ones copied forward to later agents.
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
		writeFile(t, filepath.Join(env.dir, "workspace-"+sessionAgent(p.SessionKey), "runs", run.ID, "notes.md"), "n")
		go finishThenFinal(t, env, p, `{"ok":true}`)
		return startedTurn(p)
	}
	other := env.plant("unrelated", "33333333-3333-4333-8333-333333333333")
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	for _, a := range []*store.Agent{coder, reviewer} {
		if present(env.runDirOf(openclaw.DeriveAgentID(a.ID), run.ID)) {
			t.Errorf("agent %s still has the run's folder", a.Name)
		}
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
