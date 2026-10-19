package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"yuno/internal/openclaw"
	"yuno/internal/plans"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// T5.29.5 and T5.29.6, engine side (DESIGN 5.29 rules 6 and 7): escalate ends the calling
// run failed at once and fails the plan ticket, pauses the waves, records and tells it;
// a ticket run that ends badly any other way does the same. The tests play the escalate
// tool as the platform registry will: tools.NewEscalate over EscalateRun.

type planEnv struct {
	*testEnv
	svc  *plans.Service
	proj *store.Project
	plan *store.Plan
	conv *store.Conversation
}

// newPlanEnv is an engine with the plans service, a project with an open plan of two
// tickets (issues 11 and 12) and a Chief conversation bound to Telegram.
func newPlanEnv(t *testing.T) *planEnv {
	t.Helper()
	env := newEnv(t)
	ctx := context.Background()
	svc := &plans.Service{Pool: env.pool, Reporter: env.e}
	env.e.deps.Plans = svc
	t.Cleanup(svc.Wait)
	p := env.project("https://gitlab.test/group/demo.git", false)
	wf := &store.Workflow{Name: "plan", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	if err := store.CreateWorkflow(ctx, env.pool, wf); err != nil {
		t.Fatal(err)
	}
	planRun := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: store.TriggerManual, ProjectID: &p.ID, Status: store.RunSucceeded}
	if err := store.CreateRun(ctx, env.pool, planRun); err != nil {
		t.Fatal(err)
	}
	plan := &store.Plan{ProjectID: p.ID, Feature: "search", PlanRunID: planRun.ID, Status: store.PlanBuilding}
	if err := store.CreatePlan(ctx, env.pool, plan); err != nil {
		t.Fatal(err)
	}
	pe := &planEnv{testEnv: env, svc: svc, proj: p, plan: plan}
	for i, id := range []string{"T1", "T2"} {
		n := int64(11 + i)
		tk := &store.PlanTicket{PlanID: plan.ID, TicketID: id, Wave: 1, Title: "Ticket " + id, IssueNumber: &n, Status: store.TicketReleased}
		if err := store.UpsertPlanTicket(ctx, env.pool, tk); err != nil {
			t.Fatal(err)
		}
	}
	pe.conv = env.boundConversation("agent:main:telegram:direct:42")
	return pe
}

func (pe *planEnv) ticket(id string) store.PlanTicket {
	pe.t.Helper()
	l, err := store.ListPlanTickets(context.Background(), pe.pool, pe.plan.ID)
	if err != nil {
		pe.t.Fatal(err)
	}
	for _, tk := range l {
		if tk.TicketID == id {
			return tk
		}
	}
	pe.t.Fatalf("no ticket %s", id)
	return store.PlanTicket{}
}

func (pe *planEnv) planEvents() []store.PlanEvent {
	pe.t.Helper()
	l, err := store.ListPlanEvents(context.Background(), pe.pool, pe.plan.ID, 0)
	if err != nil {
		pe.t.Fatal(err)
	}
	return l
}

func (pe *planEnv) waves() (bool, string) {
	pe.t.Helper()
	p, err := store.GetProject(context.Background(), pe.pool, pe.proj.ID)
	if err != nil {
		pe.t.Fatal(err)
	}
	reason := ""
	if p.WavesPausedReason != nil {
		reason = *p.WavesPausedReason
	}
	return p.WavesPaused, reason
}

// ticketRun starts a run of an agent whose turn never ends and links it to ticket id as
// intake's take does.
// onSend, when set, runs on its own goroutine once the ticket is linked, with the run's id.
func (pe *planEnv) ticketRun(id string, issue int64, onSend func(runID string, p openclaw.ChatSendParams)) (*store.Run, <-chan struct{}) {
	pe.t.Helper()
	// linked is closed once the ticket is linked; every turn's goroutine waits on it, so
	// none outlives the test even when the run is linked late or never (the cleanup closes
	// it).
	linked := make(chan struct{})
	var runID string
	var once sync.Once
	release := func() { once.Do(func() { close(linked) }) }
	pe.t.Cleanup(release)
	ag := pe.agent(func(a *store.Agent) { a.ToolAllowlist = []string{"exec", "finish", tools.EscalateName} })
	pe.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if onSend != nil {
			go func() {
				<-linked
				onSend(runID, p)
			}()
		}
		return startedTurn(p)
	}
	run, done := pe.start(TriggerRequest{AgentID: ag.ID, Task: "build " + id})
	if err := pe.svc.OnIssueTaken(context.Background(), pe.proj.ID, issue, run.ID); err != nil {
		pe.t.Fatal(err)
	}
	runID = run.ID
	release()
	return run, done
}

func TestEscalateEndsTheRunAndFailsTheTicket(t *testing.T) {
	pe := newPlanEnv(t)
	ctx := context.Background()
	tool := tools.NewEscalate(tools.EscalateDeps{Escalate: pe.e.EscalateRun})
	called := make(chan error, 1)
	run, done := pe.ticketRun("T1", 11, func(runID string, p openclaw.ChatSendParams) {
		_, err := tool.Exec(ctx, tools.Call{RunID: runID, Input: json.RawMessage(`{"run_token":"` + tokenIn(p.Message) +
			`","kind":"prd_gap","reason":"the PRD never says who may export"}`)})
		called <- err
	})
	// The agent's turn is in flight; its escalate call is the first thing it does.
	if err := recv(t, called, "the escalate call"); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	r := pe.wait(done, run.ID)
	if r.Status != store.RunFailed || r.FailureReason == nil || *r.FailureReason != "escalated (prd_gap): the PRD never says who may export" {
		t.Fatalf("run = %s %v", r.Status, r.FailureReason)
	}
	if tk := pe.ticket("T1"); tk.Status != store.TicketFailed {
		t.Fatalf("T1 = %+v", tk)
	}
	if tk := pe.ticket("T2"); tk.Status != store.TicketReleased {
		t.Fatalf("T2 = %+v", tk)
	}
	if paused, reason := pe.waves(); !paused || reason != "the PRD never says who may export" {
		t.Fatalf("waves = %v %q", paused, reason)
	}
	evs := pe.planEvents()
	if len(evs) != 1 || evs[0].Kind != store.PlanEventEscalated || *evs[0].TicketID != "T1" {
		t.Fatalf("events = %+v", evs)
	}
	var d map[string]string
	_ = json.Unmarshal(evs[0].Detail, &d)
	if d["kind"] != "prd_gap" || d["run_id"] != run.ID {
		t.Fatalf("detail = %v", d)
	}
	// Told once to the Chief, with the platform marker, under the event's key.
	pe.svc.Wait()
	sent := pe.sendsWithPrefix(plans.NotifyKey(evs[0].ID))
	if len(sent) != 1 || sent[0].SessionKey != pe.conv.SessionKey || !strings.HasPrefix(sent[0].Message, "[platform] ") ||
		!strings.Contains(sent[0].Message, "escalated (prd_gap)") || !strings.Contains(sent[0].Message, "ticket T1") {
		t.Fatalf("notices = %+v", sent)
	}
	// The run's own end did not escalate a second time.
	if n := len(pe.planEvents()); n != 1 {
		t.Fatalf("%d events after the run ended", n)
	}
	if n := pe.openSpans(run.ID); n != 0 {
		t.Fatalf("%d spans left open", n)
	}
}

// A run of no plan ticket still ends failed with the escalation reason; the project's
// waves are not touched, and nothing is told.
func TestEscalateOfARunWithoutATicket(t *testing.T) {
	pe := newPlanEnv(t)
	ag := pe.agent(nil)
	pe.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) { return startedTurn(p) }
	run, done := pe.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
	eventually(t, "the visit", func() bool { return pe.activeVisit(run.ID) })
	if err := pe.e.EscalateRun(context.Background(), run.ID, "design_wrong", "the schema cannot hold two owners"); err != nil {
		t.Fatal(err)
	}
	r := pe.wait(done, run.ID)
	if r.Status != store.RunFailed || *r.FailureReason != "escalated (design_wrong): the schema cannot hold two owners" {
		t.Fatalf("run = %s %v", r.Status, r.FailureReason)
	}
	if paused, _ := pe.waves(); paused {
		t.Fatal("waves paused by a run of no ticket")
	}
	if n := len(pe.planEvents()); n != 0 {
		t.Fatalf("%d plan events", n)
	}
	pe.svc.Wait()
	if n := len(pe.fc.sendParams()); n != 1 { // only the turn itself
		t.Fatalf("%d chat.send calls", n)
	}
}

func TestEscalateRunRefusals(t *testing.T) {
	pe := newPlanEnv(t)
	ctx := context.Background()
	ended := pe.endedRun(store.RunFailed, "coder", "")
	if err := pe.e.EscalateRun(ctx, ended.ID, "other", "late"); !errors.Is(err, ErrRunEnded) {
		t.Fatalf("ended run = %v", err)
	}
	if err := pe.e.EscalateRun(ctx, ended.ID, "whim", "x"); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if err := pe.e.EscalateRun(ctx, "no-such-run", "other", "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown run = %v", err)
	}
}

// Rule 7: the ticket run is cancelled by the operator (or fails by a limit): the ticket
// fails, the waves pause with the run's reason, one escalated event, one notice.
func TestFailedTicketRunEscalatesFromTheEngine(t *testing.T) {
	pe := newPlanEnv(t)
	ctx := context.Background()
	run, done := pe.ticketRun("T2", 12, nil)
	eventually(t, "the visit", func() bool { return pe.activeVisit(run.ID) })
	if tk := pe.ticket("T2"); tk.Status != store.TicketBuilding || tk.RunID == nil || *tk.RunID != run.ID {
		t.Fatalf("after the take: %+v", tk)
	}
	if _, err := pe.e.Cancel(ctx, run.ID, "andre"); err != nil {
		t.Fatal(err)
	}
	if r := pe.wait(done, run.ID); r.Status != store.RunCancelled {
		t.Fatalf("run = %s", r.Status)
	}
	eventually(t, "the ticket to fail", func() bool { return pe.ticket("T2").Status == store.TicketFailed })
	if paused, reason := pe.waves(); !paused || reason != "cancelled by andre" {
		t.Fatalf("waves = %v %q", paused, reason)
	}
	evs := pe.planEvents()
	if len(evs) != 1 || evs[0].Kind != store.PlanEventEscalated || *evs[0].TicketID != "T2" {
		t.Fatalf("events = %+v", evs)
	}
	pe.svc.Wait()
	if sent := pe.sendsWithPrefix(plans.NotifyKey(evs[0].ID)); len(sent) != 1 || !strings.Contains(sent[0].Message, "cancelled by andre") {
		t.Fatalf("notices = %+v", sent)
	}
}

// A ticket run that succeeds changes nothing but its progress.
func TestSucceededTicketRunDoesNotEscalate(t *testing.T) {
	pe := newPlanEnv(t)
	ag := pe.agent(nil)
	pe.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go finishTurn(t, pe.testEnv, p, tokenIn(p.Message), `{"summary":"done"}`)
		return startedTurn(p)
	}
	run, done := pe.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
	if err := pe.svc.OnIssueTaken(context.Background(), pe.proj.ID, 11, run.ID); err != nil {
		t.Fatal(err)
	}
	if r := pe.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %v", r.Status, r.FailureReason)
	}
	if tk := pe.ticket("T1"); tk.Status != store.TicketBuilding {
		t.Fatalf("T1 = %+v", tk)
	}
	if paused, _ := pe.waves(); paused || len(pe.planEvents()) != 0 {
		t.Fatal("a succeeded run escalated")
	}
}

// linkedRun is a run stored in the given status with no visit, linked to the ticket.
func (pe *planEnv) linkedRun(status, ticket string, issue int64) *store.Run {
	pe.t.Helper()
	run := pe.endedRun(status, "coder", "")
	if _, err := pe.pool.Exec(context.Background(), `UPDATE runs SET finished_at=NULL, failure_reason=NULL WHERE id=$1`, run.ID); err != nil {
		pe.t.Fatal(err)
	}
	if err := pe.svc.OnIssueTaken(context.Background(), pe.proj.ID, issue, run.ID); err != nil {
		pe.t.Fatal(err)
	}
	return run
}

// Review M4: a run with no live visit (waiting, queued, or running with no worker left) is
// failed in place, and only then does the plan side apply, with the escalation's kind.
func TestEscalateRunEndsARunWithoutAVisit(t *testing.T) {
	for _, status := range []string{store.RunWaitingApproval, store.RunQueued, store.RunRunning} {
		t.Run(status, func(t *testing.T) {
			pe := newPlanEnv(t)
			run := pe.linkedRun(status, "T1", 11)
			if err := pe.e.EscalateRun(context.Background(), run.ID, "prd_gap", "who may export?"); err != nil {
				t.Fatal(err)
			}
			got, err := store.GetRun(context.Background(), pe.pool, run.ID)
			if err != nil || got.Status != store.RunFailed || got.FailureReason == nil ||
				*got.FailureReason != "escalated (prd_gap): who may export?" || got.FinishedAt == nil {
				t.Fatalf("run = %+v %v", got, err)
			}
			if tk := pe.ticket("T1"); tk.Status != store.TicketFailed {
				t.Fatalf("T1 = %+v", tk)
			}
			evs := pe.planEvents()
			if len(evs) != 1 || evs[0].Kind != store.PlanEventEscalated || !strings.Contains(string(evs[0].Detail), `"kind": "prd_gap"`) {
				t.Fatalf("events = %+v", evs)
			}
			if paused, reason := pe.waves(); !paused || reason != "who may export?" {
				t.Fatalf("waves = %v %q", paused, reason)
			}
			// A second escalation finds the run ended and changes nothing.
			if err := pe.e.EscalateRun(context.Background(), run.ID, "other", "again"); !errors.Is(err, ErrRunEnded) {
				t.Fatalf("second = %v", err)
			}
			if len(pe.planEvents()) != 1 {
				t.Fatal("a second event")
			}
			pe.svc.Wait()
		})
	}
}

// Review H2: an operator's retry of an escalated ticket's run puts the ticket back to
// building with a `retried` event, so the run's next failure escalates again.
func TestRetryOfAnEscalatedTicketRun(t *testing.T) {
	pe := newPlanEnv(t)
	ctx := context.Background()
	run := pe.linkedRun(store.RunRunning, "T1", 11)
	if err := pe.e.EscalateRun(ctx, run.ID, "loop_limit", "three rejections"); err != nil {
		t.Fatal(err)
	}
	if tk := pe.ticket("T1"); tk.Status != store.TicketFailed {
		t.Fatalf("T1 = %+v", tk)
	}
	if _, err := pe.pool.Exec(ctx, `UPDATE runs SET current_node='coder' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pe.e.Retry(ctx, run.ID, "andre", nil); err != nil {
		t.Fatal(err)
	}
	if tk := pe.ticket("T1"); tk.Status != store.TicketBuilding || tk.RunID == nil || *tk.RunID != run.ID {
		t.Fatalf("after the retry: %+v", tk)
	}
	var kinds []string
	for _, ev := range pe.planEvents() {
		kinds = append(kinds, ev.Kind)
	}
	if len(kinds) != 2 || kinds[0] != store.PlanEventRetried || kinds[1] != store.PlanEventEscalated {
		t.Fatalf("events = %v", kinds)
	}
	// The retried run fails again: the ticket escalates a second time.
	reason := "boom again"
	if _, err := pe.pool.Exec(ctx, `UPDATE runs SET status='failed', failure_reason=$2, finished_at=now() WHERE id=$1`, run.ID, reason); err != nil {
		t.Fatal(err)
	}
	failedRun, _ := store.GetRun(ctx, pe.pool, run.ID)
	pe.svc.OnRunTerminal(ctx, failedRun)
	if tk := pe.ticket("T1"); tk.Status != store.TicketFailed {
		t.Fatalf("after the second failure: %+v", tk)
	}
	n := 0
	for _, ev := range pe.planEvents() {
		if ev.Kind == store.PlanEventEscalated {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d escalations, want 2", n)
	}
	pe.svc.Wait()
}
