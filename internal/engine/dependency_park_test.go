package engine

import (
	"context"
	"strings"
	"testing"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// TestRequeuedRunCarriesNote (Chief platform control, DESIGN wave 3 rules 7 and 8a): a run
// the dependency watcher re-queued tells its first step the watcher's note, after the task,
// as a resumed step is told its resume; the steps after one succeeded are not told again.
func TestRequeuedRunCarriesNote(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	wf := env.createWorkflow(line(workflow.Node{ID: "code", AgentID: a.ID, Task: "write"}, workflow.Node{ID: "review", AgentID: a.ID, Task: "check"}))
	sc := env.script(map[string][]string{"code": {`{"ok":true}`}, "review": {`{"ok":true}`}})
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID})
	if err != nil {
		t.Fatal(err)
	}
	// The re-queue the watcher's hook makes keeps the note on the run (store.RequeueDependencyRuns).
	note := "Its prerequisite !17 is merged; the platform merged main into this branch (abc1234) and CI passed."
	if _, err := env.pool.Exec(ctx, `UPDATE runs SET resume_note=$2 WHERE id=$1`, run.ID, note); err != nil {
		t.Fatal(err)
	}
	if r := env.resume(env.e, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if msg := sc.message("code", 1); !strings.HasPrefix(msg, taskWith("write", note)) {
		t.Fatalf("first step's message = %q", msg)
	}
	if msg := sc.message("review", 1); strings.Contains(msg, note) || !strings.HasPrefix(msg, taskWith("check")) {
		t.Fatalf("second step's message = %q", msg)
	}
}
