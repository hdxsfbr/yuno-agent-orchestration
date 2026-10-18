package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"yuno/internal/memory"
	"yuno/internal/openclaw"
	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// Wave 8 agent A: what a project visit starts knowing (DESIGN 5.18) and the project
// budget at queue time (DESIGN 5.19).

// goProject is a project whose bare remote holds a Go module and has network, so the task
// carries the install and test lines the blocks follow.
func goProject(t *testing.T) (*testEnv, *store.Project) {
	t.Helper()
	env, _, p := projectEnv(t, map[string]string{"README.md": "# demo\n", "go.mod": "module demo\n"}, true)
	return env, p
}

// setInstructions stores the operator's instructions on the project row.
func setProjectInstructions(t *testing.T, env *testEnv, p *store.Project, text string) {
	t.Helper()
	p.Instructions = text
	if err := store.UpdateProject(context.Background(), env.pool, p); err != nil {
		t.Fatal(err)
	}
}

// setCaps stores the budget caps (micro-USD, 0 for none) on the project row.
func setProjectCaps(t *testing.T, env *testEnv, p *store.Project, day, month int64) {
	t.Helper()
	p.BudgetDayUSDMicros, p.BudgetMonthUSDMicros = nil, nil
	if day > 0 {
		d := store.USDMicros(day)
		p.BudgetDayUSDMicros = &d
	}
	if month > 0 {
		m := store.USDMicros(month)
		p.BudgetMonthUSDMicros = &m
	}
	if err := store.UpdateProject(context.Background(), env.pool, p); err != nil {
		t.Fatal(err)
	}
}

// addNote saves a note at age before the engine's clock; ttl nil is the 90-day default.
func addProjectNote(t *testing.T, env *testEnv, p *store.Project, key, value string, age time.Duration, ttl *int) {
	t.Helper()
	now := env.clock.Now().Add(-age)
	exp := memory.ProjectFactExpiresAt(ttl, now)
	f := &store.ProjectFact{ProjectID: p.ID, Key: key, Value: value, ExpiresAt: &exp}
	if err := memory.SaveProjectFact(context.Background(), env.pool, f, now); err != nil {
		t.Fatal(err)
	}
}

// visitMessage queues a run, runs it to its end with beforeRun called in between, and
// returns the first message the agent was sent.
func visitMessage(t *testing.T, env *testEnv, req TriggerRequest, beforeRun func()) (string, *store.Run) {
	t.Helper()
	var mu sync.Mutex
	var msg string
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		mu.Lock()
		if msg == "" {
			msg = p.Message
		}
		mu.Unlock()
		go finishThenFinal(t, env, p, `{"summary":"done"}`)
		return startedTurn(p)
	}
	run, err := env.e.Trigger(context.Background(), req)
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if beforeRun != nil {
		beforeRun()
	}
	r := env.resume(env.e, run.ID)
	mu.Lock()
	defer mu.Unlock()
	return msg, r
}

// blocksOf is what the task holds between the sandbox network line and the finish
// instruction: the two blocks of DESIGN 5.18 rule 4, when there are any.
func blocksOf(t *testing.T, msg string) string {
	t.Helper()
	i := strings.Index(msg, "Sandbox network: ")
	j := strings.Index(msg, "When finished, call the tool")
	if i < 0 || j < i {
		t.Fatalf("not a project task:\n%s", msg)
	}
	nl := strings.Index(msg[i:], "\n")
	return msg[i+nl+1 : j]
}

func projectWorkflow(env *testEnv, agent *store.Agent) *store.Workflow {
	def := line(workflow.Node{ID: "one", AgentID: agent.ID, Task: "write"})
	def.Project = true
	return env.createWorkflow(def)
}

// T5.18.7: the task of a project visit carries the instructions block and the notes
// block, in that order, after the sandbox network line: both, the first only, the second
// only, neither. They are read when the task is rendered, so a deleted project's still
// show, an expired note does not, and a quick run on a project gets them too.
func TestProjectVisitCarriesInstructionsAndNotes(t *testing.T) {
	env, p := goProject(t)
	coder := env.agent(nil)
	wf := projectWorkflow(env, coder)
	run := func(beforeRun func()) string {
		t.Helper()
		msg, r := visitMessage(t, env, TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID}, beforeRun)
		if r.Status != store.RunSucceeded {
			t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
		}
		for _, want := range []string{"Sandbox network: on. Install the dependencies from the lockfile first (go mod download), then run the project's tests (go test ./...).\n",
			"When finished, call the tool yuno__finish"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("task lacks %q:\n%s", want, msg)
			}
		}
		return blocksOf(t, msg)
	}
	const (
		instructions = "Project instructions from the operator:\nUse tabs.\nNever edit vendor/.\n"
		notes        = "Project notes saved by earlier runs, newest first:\n- layout: cmd/ holds the binaries\n- test command: go test ./... -race\n"
	)

	if got := run(nil); got != "" {
		t.Fatalf("neither: the task carries %q", got)
	}

	setProjectInstructions(t, env, p, "  Use tabs.\nNever edit vendor/.\n\n")
	if got := run(nil); got != instructions {
		t.Fatalf("instructions only:\n%q\nwant\n%q", got, instructions)
	}

	setProjectInstructions(t, env, p, "   \n\t ")
	addProjectNote(t, env, p, "test command", "go test ./... -race", 2*time.Hour, nil)
	addProjectNote(t, env, p, "layout", "cmd/ holds the binaries", time.Hour, nil)
	addProjectNote(t, env, p, "stale", "an expired note", 48*time.Hour, wave8Int(1))
	if got := run(nil); got != notes {
		t.Fatalf("notes only (whitespace-only instructions are empty, an expired note is hidden):\n%q\nwant\n%q", got, notes)
	}

	setProjectInstructions(t, env, p, "Use tabs.\nNever edit vendor/.")
	if got := run(nil); got != instructions+notes {
		t.Fatalf("both:\n%q\nwant\n%q", got, instructions+notes)
	}

	// A quick run on the project gets the blocks too.
	msg, r := visitMessage(t, env, TriggerRequest{AgentID: coder.ID, Task: "fix it", ProjectID: p.ID}, nil)
	if r.Status != store.RunSucceeded || blocksOf(t, msg) != instructions+notes {
		t.Fatalf("quick run: %s %q\n%s", r.Status, deref(r.FailureReason), msg)
	}

	// The blocks are read when the visit's task is rendered: a note saved after the run
	// was queued is in, and a project deleted since still renders its row's text.
	msg, r = visitMessage(t, env, TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID}, func() {
		addProjectNote(t, env, p, "late", "saved after the run was queued", 0, nil)
		if err := store.DeleteProject(context.Background(), env.pool, p.ID); err != nil {
			t.Fatal(err)
		}
	})
	want := instructions + "Project notes saved by earlier runs, newest first:\n- late: saved after the run was queued\n- layout: cmd/ holds the binaries\n- test command: go test ./... -race\n"
	if r.Status != store.RunSucceeded || blocksOf(t, msg) != want {
		t.Fatalf("after the project was deleted: %s %q\n%q\nwant\n%q", r.Status, deref(r.FailureReason), blocksOf(t, msg), want)
	}
}

// T5.18.8: only the ten newest notes are considered; together with the instructions the
// blocks are at most 12 KiB, the instructions are never cut and the oldest notes go
// first; a long value ends with the cut marker.
func TestProjectVisitCapDropsTheOldestNotes(t *testing.T) {
	env, p := goProject(t)
	coder := env.agent(nil)
	wf := projectWorkflow(env, coder)
	instructions := strings.Repeat("i", 8192)
	setProjectInstructions(t, env, p, instructions)
	const total = 12
	for i := range total { // note 11 is the newest; 505 bytes a line with a value cut at 300
		key := fmt.Sprintf("note-%02d-", i) + strings.Repeat("k", 192)
		addProjectNote(t, env, p, key, strings.Repeat("v", 400), time.Duration(total-i)*time.Minute, nil)
	}
	msg, r := visitMessage(t, env, TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID}, nil)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	blocks := blocksOf(t, msg)
	if len(blocks) > project.MaxExtrasBytes {
		t.Fatalf("blocks are %d bytes, over the 12 KiB cap", len(blocks))
	}
	if !strings.Contains(blocks, "Project instructions from the operator:\n"+instructions+"\n") {
		t.Fatal("the instructions were cut")
	}
	room := project.MaxExtrasBytes - (len(project.InstructionsHeading) + 1 + 8192 + 1) - (len(project.NotesHeading) + 1)
	line := len("- ") + 200 + len(": ") + project.MaxNoteValueBytes + len("…") + 1
	want := room / line // fewer than the ten newest: the cap bites
	if want <= 0 || want >= 10 {
		t.Fatalf("fixture: the cap should keep between 1 and 9 notes, computed %d", want)
	}
	if got := strings.Count(blocks, "\n- note-"); got != want {
		t.Fatalf("kept %d notes, want %d\n%s", got, want, blocks)
	}
	for i := range total {
		kept := strings.Contains(blocks, fmt.Sprintf("- note-%02d-", i))
		if kept != (i >= total-want) {
			t.Errorf("note-%02d kept = %v: the oldest go first (kept the newest %d)", i, kept, want)
		}
	}
	if !strings.Contains(blocks, strings.Repeat("v", 300)+"…\n") || strings.Contains(blocks, strings.Repeat("v", 301)) {
		t.Error("a long value is cut at 300 bytes and marked")
	}
}

// DESIGN 5.18 failure handling: a database error while reading the notes fails the visit
// with "project notes: <error>"; the agent is never sent a silently incomplete prompt.
func TestProjectNotesReadFailureFailsTheVisit(t *testing.T) {
	env, p := goProject(t)
	ctx := context.Background()
	// Put back a table an earlier killed run of this test may have left renamed.
	if _, err := env.pool.Exec(ctx, `DO $$ BEGIN
		IF to_regclass('project_facts_hidden') IS NOT NULL AND to_regclass('project_facts') IS NULL THEN
			ALTER TABLE project_facts_hidden RENAME TO project_facts;
		END IF; END $$`); err != nil {
		t.Fatal(err)
	}
	coder := env.agent(nil)
	wf := projectWorkflow(env, coder)
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `ALTER TABLE project_facts RENAME TO project_facts_hidden`); err != nil {
		t.Fatal(err)
	}
	restore := func() {
		if _, err := env.pool.Exec(ctx, `ALTER TABLE IF EXISTS project_facts_hidden RENAME TO project_facts`); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(restore)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go finishThenFinal(t, env, p, `{"summary":"done"}`)
		return startedTurn(p)
	}
	r := env.resume(env.e, run.ID)
	restore()
	if r.Status != store.RunFailed || !strings.HasPrefix(deref(r.FailureReason), "project notes: ") || !strings.Contains(deref(r.FailureReason), "project_facts") {
		t.Fatalf("run = %s %q, want failed with project notes: <error>", r.Status, deref(r.FailureReason))
	}
	if sends := env.fc.callsFor("chat.send"); len(sends) != 0 {
		t.Fatalf("the agent was sent %d messages on an incomplete prompt", len(sends))
	}
}

// setRunCost sets what a run has cost.
func setRunCost(t *testing.T, env *testEnv, runID string, micros int64) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(), `UPDATE runs SET cost_usd_micros=$2 WHERE id=$1`, runID, micros); err != nil {
		t.Fatal(err)
	}
}

// T5.19.3 (engine): a trigger that names a project whose spend is at or over a cap is
// refused with the typed error before anything is stored: cap - 1 passes, the cap
// refuses, the day before the month; a quick run and a workflow run are refused alike,
// and only the project's own spend, in the window, counts.
func TestTriggerRefusedAtTheProjectBudget(t *testing.T) {
	env, p := goProject(t)
	ctx := context.Background()
	coder := env.agent(nil)
	wf := projectWorkflow(env, coder)
	setProjectCaps(t, env, p, 5_000_000, 20_000_000)
	quick := TriggerRequest{AgentID: coder.ID, Task: "x", ProjectID: p.ID}
	runs := func() int { return env.count(`SELECT count(*) FROM runs`) }

	first, err := env.e.Trigger(ctx, quick)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	setRunCost(t, env, first.ID, 5_000_000-1)
	second, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID})
	if err != nil {
		t.Fatalf("spend one micro under the cap: %v", err)
	}
	if n := runs(); n != 2 {
		t.Fatalf("runs = %d", n)
	}
	setRunCost(t, env, first.ID, 5_000_000)
	for name, req := range map[string]TriggerRequest{"quick run": quick, "workflow run": {WorkflowID: wf.ID, ProjectID: p.ID}} {
		_, err := env.e.Trigger(ctx, req)
		var be *project.BudgetError
		if !errors.As(err, &be) {
			t.Fatalf("%s at the cap: %v, want *project.BudgetError", name, err)
		}
		_, end := store.DayBounds(env.clock.Now())
		if be.ProjectID != p.ID || be.ProjectName != p.Name || be.Window != "day" || be.CapMicros != 5_000_000 || be.SpendMicros != 5_000_000 || !be.WindowEnd.Equal(end) {
			t.Fatalf("%s: budget error = %+v", name, be)
		}
		if want := "project " + p.Name + " has reached its daily budget ($5.00); new runs are refused until " + end.Format(time.RFC3339); be.Error() != want {
			t.Fatalf("%s: text = %q\nwant   %q", name, be.Error(), want)
		}
	}
	if n := runs(); n != 2 {
		t.Fatalf("runs = %d: a refused trigger stores nothing", n)
	}

	// Only the project's own runs count: a run elsewhere, or without a project, does not.
	other := env.project("https://gitlab.test/g/other.git", false)
	elsewhere, err := env.e.Trigger(ctx, TriggerRequest{AgentID: coder.ID, Task: "x", ProjectID: other.ID})
	if err != nil {
		t.Fatalf("another project: %v", err)
	}
	setRunCost(t, env, elsewhere.ID, 900_000_000)
	plain, err := env.e.Trigger(ctx, TriggerRequest{AgentID: coder.ID, Task: "no project"})
	if err != nil {
		t.Fatalf("a run without a project: %v", err)
	}
	setRunCost(t, env, plain.ID, 900_000_000)

	// Yesterday's run counts in the month only: with the day cap off, the month cap
	// (20) is what refuses, at 20 and not at 20 - 1.
	setProjectCaps(t, env, p, 0, 20_000_000)
	setRunCost(t, env, first.ID, 0)
	// The first instant of the month is in the month, and in the day only on the 1st.
	monthStart, _ := store.MonthBounds(env.clock.Now())
	if _, err := env.pool.Exec(ctx, `UPDATE runs SET created_at = $2 WHERE id = $1`, second.ID, monthStart); err != nil {
		t.Fatal(err)
	}
	setRunCost(t, env, second.ID, 20_000_000-1)
	if _, err := env.e.Trigger(ctx, quick); err != nil {
		t.Fatalf("month spend one micro under the cap: %v", err)
	}
	setRunCost(t, env, second.ID, 20_000_000)
	_, err = env.e.Trigger(ctx, quick)
	var be *project.BudgetError
	_, monthEnd := store.MonthBounds(env.clock.Now())
	if !errors.As(err, &be) || be.Window != "month" || be.CapMicros != 20_000_000 || !be.WindowEnd.Equal(monthEnd) ||
		!strings.Contains(be.Error(), "monthly budget ($20.00)") {
		t.Fatalf("month cap: %v", err)
	}
	// A run from the month before is out of the window.
	if _, err := env.pool.Exec(ctx, `UPDATE runs SET created_at = $2 WHERE id = $1`, second.ID, monthStart.Add(-time.Microsecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Trigger(ctx, quick); err != nil {
		t.Fatalf("spend from last month still counted: %v", err)
	}
	// Both caps reached: the day's refusal comes first.
	setProjectCaps(t, env, p, 1_000_000, 1_000_000)
	if _, err := env.pool.Exec(ctx, `UPDATE runs SET created_at = $2 WHERE id = $1`, second.ID, env.clock.Now()); err != nil {
		t.Fatal(err)
	}
	setRunCost(t, env, second.ID, 2_000_000)
	if _, err := env.e.Trigger(ctx, quick); !errors.As(err, &be) || be.Window != "day" {
		t.Fatalf("both caps reached: %v, want the day", err)
	}
}

// T5.19.4: a running run is never aborted by a project cap: it finishes under its own
// limits whatever it has spent, and the next trigger is refused.
func TestRunningRunFinishesPastTheProjectCap(t *testing.T) {
	env, p := goProject(t)
	ctx := context.Background()
	coder := env.agent(nil)
	wf := projectWorkflow(env, coder)
	setProjectCaps(t, env, p, 1_000_000, 0)
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	env.fc.onSend = func(sp openclaw.ChatSendParams, n int) (any, error) {
		// The visit has started and the project goes three times over its day cap.
		setRunCost(t, env, run.ID, 3_000_000)
		go finishThenFinal(t, env, sp, `{"summary":"done"}`)
		return startedTurn(sp)
	}
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded || r.CostUSDMicros < 3_000_000 {
		t.Fatalf("run = %s %q cost %d: the cap must not abort a running run", r.Status, deref(r.FailureReason), r.CostUSDMicros)
	}
	var be *project.BudgetError
	if _, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID}); !errors.As(err, &be) || be.SpendMicros < 3_000_000 {
		t.Fatalf("the next trigger: %v, want the budget refusal", err)
	}
}

func wave8Int(v int) *int { return &v }

// The engine is the budget watcher's reporter (DESIGN 5.19 rule 6): server.go passes it.
var _ project.Reporter = (*Engine)(nil)
