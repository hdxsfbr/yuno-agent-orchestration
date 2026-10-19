package plans

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"yuno/internal/events"
	"yuno/internal/store"
)

// DESIGN 5.29 rules 6 to 10: plan ticket state follows the platform.

type sentMessage struct{ runID, conversation, key, text string }

type fakeReporter struct {
	mu       sync.Mutex
	sent     []sentMessage
	deferred bool
}

func (r *fakeReporter) SendPlatformMessage(_ context.Context, runID, conversationID, key, text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deferred {
		return true
	}
	r.sent = append(r.sent, sentMessage{runID, conversationID, key, text})
	return false
}

func (r *fakeReporter) messages() []sentMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sentMessage(nil), r.sent...)
}

type env struct {
	t       *testing.T
	svc     *Service
	rep     *fakeReporter
	hub     *events.Hub
	project *store.Project
	plan    *store.Plan
	planRun *store.Run
	conv    string
	tickets map[string]*store.PlanTicket
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := needDB(t)
	ctx := context.Background()
	hub := events.NewHub()
	rep := &fakeReporter{}
	e := &env{t: t, rep: rep, hub: hub, tickets: map[string]*store.PlanTicket{},
		svc: &Service{Pool: pool, Hub: hub, Reporter: rep}}
	t.Cleanup(e.svc.Wait) // no notice in flight when the next test empties the tables
	e.project = &store.Project{Name: "demo-" + store.NewID()[20:], RepoURL: "https://gitlab.test/g/demo.git", Provider: "gitlab", DefaultBranch: "main"}
	mustDo(t, store.CreateProject(ctx, pool, e.project, []byte("sealed")))
	e.planRun = e.newRun()
	e.plan = &store.Plan{ProjectID: e.project.ID, Feature: "search", PlanRunID: e.planRun.ID, Status: store.PlanBuilding}
	mustDo(t, store.CreatePlan(ctx, pool, e.plan))
	e.ticket("T1", 1, 11)
	e.ticket("T2", 1, 12)
	return e
}

func (e *env) newRun() *store.Run {
	e.t.Helper()
	ctx := context.Background()
	wf := &store.Workflow{Name: "wf " + store.NewID(), Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(e.t, store.CreateWorkflow(ctx, testPool, wf))
	settings := e.project.Settings()
	run := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: store.TriggerIssue,
		ProjectID: &e.project.ID, Project: &settings}
	mustDo(e.t, store.CreateRun(ctx, testPool, run))
	return run
}

func (e *env) ticket(id string, wave int, issue int64) {
	e.t.Helper()
	n := issue
	tk := &store.PlanTicket{PlanID: e.plan.ID, TicketID: id, Wave: wave, Title: "Title " + id, IssueNumber: &n,
		IssueURL: "https://gitlab.test/g/demo/-/issues/" + id, Status: store.TicketReleased}
	mustDo(e.t, store.UpsertPlanTicket(context.Background(), testPool, tk))
	e.tickets[id] = tk
}

func (e *env) get(id string) *store.PlanTicket {
	e.t.Helper()
	for _, tk := range mustTickets(e.t, e.plan.ID) {
		if tk.TicketID == id {
			return &tk
		}
	}
	e.t.Fatalf("no ticket %s", id)
	return nil
}

func mustTickets(t *testing.T, planID string) []store.PlanTicket {
	t.Helper()
	l, err := store.ListPlanTickets(context.Background(), testPool, planID)
	mustDo(t, err)
	return l
}

func (e *env) events() []store.PlanEvent {
	e.t.Helper()
	l, err := store.ListPlanEvents(context.Background(), testPool, e.plan.ID, 0)
	mustDo(e.t, err)
	return l
}

func (e *env) project2() *store.Project {
	e.t.Helper()
	p, err := store.GetProject(context.Background(), testPool, e.project.ID)
	mustDo(e.t, err)
	return p
}

// bind binds an agent to Telegram with one conversation, as the budget alerts' tests do.
func (e *env) bind() string {
	e.t.Helper()
	ctx := context.Background()
	a := &store.Agent{Name: "Chief " + store.NewID(), Model: "claude-opus-5-5"}
	mustDo(e.t, store.CreateAgent(ctx, testPool, a))
	_, err := store.BindChannel(ctx, testPool, store.ChannelTelegram, a.ID)
	mustDo(e.t, err)
	c, err := store.UpsertConversation(ctx, testPool, &store.Conversation{Channel: store.ChannelTelegram, ExternalChatID: "1", AgentID: a.ID,
		SessionKey: "agent:" + a.ID + ":telegram:direct:1"})
	mustDo(e.t, err)
	_, err = store.InsertConversationMessage(ctx, testPool, &store.ConversationMessage{ConversationID: c.ID, Direction: store.DirectionIn,
		ExternalMessageID: "m1", Text: "hi", CreatedAt: time.Now().UTC().Add(-time.Hour)})
	mustDo(e.t, err)
	e.conv = c.ID
	return c.ID
}

func ended(run *store.Run, status, reason string) *store.Run {
	cp := *run
	cp.Status = status
	if reason != "" {
		cp.FailureReason = &reason
	}
	return &cp
}

// T5.29.5/T5.29.6 (rule 10): the take links the run (building), the record puts the ticket
// in review, the merge sets it merged; an issue of no plan changes nothing.
func TestTicketStateFollowsThePlatform(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sub, cancel := e.hub.Subscribe("", 16)
	defer cancel()

	run := e.newRun()
	mustDo(t, e.svc.OnIssueTaken(ctx, e.project.ID, 99, run.ID)) // no ticket carries #99
	if tk := e.get("T1"); tk.Status != store.TicketReleased || tk.RunID != nil {
		t.Fatalf("an unrelated issue touched a ticket: %+v", tk)
	}
	mustDo(t, e.svc.OnIssueTaken(ctx, e.project.ID, 11, run.ID))
	tk := e.get("T1")
	if tk.Status != store.TicketBuilding || tk.RunID == nil || *tk.RunID != run.ID {
		t.Fatalf("after the take: %+v", tk)
	}
	if other := e.get("T2"); other.Status != store.TicketReleased {
		t.Fatalf("T2 = %s", other.Status)
	}
	select {
	case ev := <-sub:
		var d map[string]string
		_ = json.Unmarshal(ev.Data, &d)
		if ev.Type != EventPlanUpdated || d["plan_id"] != e.plan.ID || d["project_id"] != e.project.ID {
			t.Fatalf("event = %+v %v", ev, d)
		}
	case <-time.After(time.Second):
		t.Fatal("no plan.updated event")
	}

	// The record of the run's merge request, inserted as open_merge_request does.
	mr := &store.MergeRequest{ProjectID: e.project.ID, RunID: run.ID, Provider: "gitlab", Number: 5, URL: "https://gitlab.test/g/demo/-/merge_requests/5",
		SourceBranch: "yuno/t1", TargetBranch: "main", Title: "T1"}
	if ok, err := store.InsertMergeRequest(ctx, testPool, mr); err != nil || !ok {
		t.Fatalf("insert: %v %v", ok, err)
	}
	tk = e.get("T1")
	if tk.Status != store.TicketInReview || tk.MergeRequestID == nil || *tk.MergeRequestID != mr.ID {
		t.Fatalf("after the record: %+v", tk)
	}

	mustDo(t, e.svc.OnMergeRequestMerged(ctx, mr.ID))
	if tk = e.get("T1"); tk.Status != store.TicketMerged {
		t.Fatalf("after the merge: %+v", tk)
	}
	// A second call, and a merge request of no ticket, change nothing.
	mustDo(t, e.svc.OnMergeRequestMerged(ctx, mr.ID))
	mustDo(t, e.svc.OnMergeRequestMerged(ctx, "no-such-request"))
	if tk = e.get("T1"); tk.Status != store.TicketMerged {
		t.Fatalf("T1 = %s", tk.Status)
	}
	if len(e.events()) != 0 {
		t.Fatalf("state changes write no plan events: %+v", e.events())
	}
}

// OnMergeRequestRecorded is the same step for a caller holding only the ids.
func TestOnMergeRequestRecorded(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	run := e.newRun()
	mustDo(t, e.svc.OnIssueTaken(ctx, e.project.ID, 12, run.ID))
	// A record written without the store's hook (a bare insert), then linked.
	mr := &store.MergeRequest{ID: store.NewID(), ProjectID: e.project.ID, RunID: run.ID, Provider: "gitlab", Number: 6, URL: "u",
		SourceBranch: "b", TargetBranch: "main", Title: "t", State: "open", CIStatus: "none"}
	_, err := testPool.Exec(ctx, `INSERT INTO merge_requests (id, project_id, run_id, provider, number, url, source_branch, target_branch, title, state, ci_status, created_at, updated_at)
		VALUES ($1,$2,$3,'gitlab',6,'u','b','main','t','open','none',now(),now())`, mr.ID, e.project.ID, run.ID)
	mustDo(t, err)
	mustDo(t, e.svc.OnMergeRequestRecorded(ctx, run.ID, mr.ID))
	if tk := e.get("T2"); tk.Status != store.TicketInReview || tk.MergeRequestID == nil || *tk.MergeRequestID != mr.ID {
		t.Fatalf("T2 = %+v", tk)
	}
}

// A take of an issue whose plan is finished is ignored; a failed ticket taken again builds.
func TestTakeOfFinishedPlanAndRetake(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	run := e.newRun()
	mustDo(t, store.SetPlanStatus(ctx, testPool, e.plan.ID, store.PlanDone))
	mustDo(t, e.svc.OnIssueTaken(ctx, e.project.ID, 11, run.ID))
	if tk := e.get("T1"); tk.RunID != nil || tk.Status != store.TicketReleased {
		t.Fatalf("a done plan's ticket moved: %+v", tk)
	}
	mustDo(t, store.SetPlanStatus(ctx, testPool, e.plan.ID, store.PlanBuilding))
	mustDo(t, store.SetTicketStatus(ctx, testPool, e.plan.ID, "T1", store.TicketFailed))
	mustDo(t, e.svc.OnIssueTaken(ctx, e.project.ID, 11, run.ID))
	if tk := e.get("T1"); tk.Status != store.TicketBuilding {
		t.Fatalf("retaken ticket = %+v", tk)
	}
}

// T5.29.6 (rule 7): a ticket run that ends failed, timed_out or cancelled fails its
// ticket, pauses the waves with the run's reason, records one escalated event and tells
// the Chief once; a succeeded run, a run of no ticket and a second report change nothing.
func TestFailedTicketRunEscalates(t *testing.T) {
	for _, tc := range []struct{ status, reason, want string }{
		{store.RunFailed, "loop limit: node coder visited 10 times", "loop limit: node coder visited 10 times"},
		{store.RunTimedOut, "time limit of 3600s reached", "time limit of 3600s reached"},
		{store.RunCancelled, "cancelled by andre", "cancelled by andre"},
		{store.RunFailed, "", store.RunFailed},
	} {
		t.Run(tc.status+"/"+tc.reason, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			conv := e.bind()
			run := e.newRun()
			mustDo(t, e.svc.OnIssueTaken(ctx, e.project.ID, 11, run.ID))

			e.svc.OnRunTerminal(ctx, ended(run, store.RunSucceeded, ""))
			e.svc.OnRunTerminal(ctx, ended(e.newRun(), tc.status, tc.reason)) // builds no ticket
			if tk := e.get("T1"); tk.Status != store.TicketBuilding {
				t.Fatalf("ticket moved without cause: %+v", tk)
			}
			if p := e.project2(); p.WavesPaused {
				t.Fatal("paused without cause")
			}

			e.svc.OnRunTerminal(ctx, ended(run, tc.status, tc.reason))
			e.svc.OnRunTerminal(ctx, ended(run, tc.status, tc.reason)) // reported again
			e.svc.Wait()

			if tk := e.get("T1"); tk.Status != store.TicketFailed {
				t.Fatalf("ticket = %s", tk.Status)
			}
			if tk := e.get("T2"); tk.Status != store.TicketReleased {
				t.Fatalf("T2 = %s", tk.Status)
			}
			p := e.project2()
			if !p.WavesPaused || p.WavesPausedReason == nil || *p.WavesPausedReason != tc.want {
				t.Fatalf("pause = %v %v", p.WavesPaused, p.WavesPausedReason)
			}
			evs := e.events()
			if len(evs) != 1 || evs[0].Kind != store.PlanEventEscalated || evs[0].TicketID == nil || *evs[0].TicketID != "T1" ||
				evs[0].Wave == nil || *evs[0].Wave != 1 {
				t.Fatalf("events = %+v", evs)
			}
			var d map[string]string
			_ = json.Unmarshal(evs[0].Detail, &d)
			if d["reason"] != tc.want || d["run_id"] != run.ID || d["kind"] != "" {
				t.Fatalf("detail = %v", d)
			}
			msgs := e.rep.messages()
			if len(msgs) != 1 || msgs[0].conversation != conv || msgs[0].runID != "" || msgs[0].key != NotifyKey(evs[0].ID) {
				t.Fatalf("messages = %+v", msgs)
			}
			if got := msgs[0].text; got[:11] != "[platform] " || !contains(got, "ticket T1") || !contains(got, tc.want) || !contains(got, "paused") {
				t.Fatalf("text = %q", got)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// T5.29.5 (rule 6): Escalate with a kind records the kind; a ticket failed by it is not
// escalated a second time when its run then ends failed.
func TestEscalateRecordsKindOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.bind()
	run := e.newRun()
	mustDo(t, e.svc.OnIssueTaken(ctx, e.project.ID, 11, run.ID))
	tk := e.get("T1")

	reason := "escalated (prd_gap): the PRD does not say who may export"
	ev, err := e.svc.Escalate(ctx, tk, "prd_gap", "the PRD does not say who may export", run.ID)
	if err != nil || ev == nil {
		t.Fatalf("Escalate = %v %v", ev, err)
	}
	// The run then ends with the engine's reason; the ticket is failed already.
	e.svc.OnRunTerminal(ctx, ended(run, store.RunFailed, reason))
	if again, err := e.svc.Escalate(ctx, tk, "prd_gap", "again", run.ID); err != nil || again != nil {
		t.Fatalf("second Escalate = %v %v", again, err)
	}
	e.svc.Wait()

	evs := e.events()
	if len(evs) != 1 || evs[0].ID != ev.ID {
		t.Fatalf("events = %+v", evs)
	}
	var d map[string]string
	_ = json.Unmarshal(evs[0].Detail, &d)
	if d["kind"] != "prd_gap" || d["reason"] != "the PRD does not say who may export" {
		t.Fatalf("detail = %v", d)
	}
	if p := e.project2(); !p.WavesPaused || *p.WavesPausedReason != "the PRD does not say who may export" {
		t.Fatalf("pause = %v %v", p.WavesPaused, p.WavesPausedReason)
	}
	msgs := e.rep.messages()
	if len(msgs) != 1 || !contains(msgs[0].text, "escalated (prd_gap)") {
		t.Fatalf("messages = %+v", msgs)
	}
}

// Rule 8: the notice goes once per event whatever the number of calls; with no binding it
// is only logged; a deferred send is tried again by a later call.
func TestNotifyOncePerEvent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ev := &store.PlanEvent{ID: store.NewID(), PlanID: e.plan.ID, Kind: store.PlanEventWaveDone}

	e.svc.Notify(ctx, e.plan, ev, "[platform] nowhere")
	e.svc.Wait()
	if n := len(e.rep.messages()); n != 0 {
		t.Fatalf("sent without a binding: %d", n)
	}

	e = newEnv(t)
	e.bind()
	e.rep.deferred = true
	ev = &store.PlanEvent{ID: store.NewID(), PlanID: e.plan.ID, Kind: store.PlanEventWaveDone}
	e.svc.Notify(ctx, e.plan, ev, "[platform] first")
	e.svc.Wait()
	e.rep.mu.Lock()
	e.rep.deferred = false
	e.rep.mu.Unlock()
	e.svc.Notify(ctx, e.plan, ev, "[platform] second")
	e.svc.Notify(ctx, e.plan, ev, "[platform] third")
	e.svc.Wait()
	msgs := e.rep.messages()
	if len(msgs) != 1 || msgs[0].text != "[platform] second" || msgs[0].key != "plan-event-"+ev.ID {
		t.Fatalf("messages = %+v", msgs)
	}
}

// T5.29.7 (rule 9): pause and resume write events on the open plan, none without one.
func TestPauseAndResume(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	mustDo(t, e.svc.Pause(ctx, e.project.ID, "  holding for the smoke test  "))
	p := e.project2()
	if !p.WavesPaused || *p.WavesPausedReason != "holding for the smoke test" {
		t.Fatalf("paused = %v %v", p.WavesPaused, p.WavesPausedReason)
	}
	mustDo(t, e.svc.Pause(ctx, e.project.ID, "second reason")) // replaces the reason
	if p = e.project2(); *p.WavesPausedReason != "second reason" {
		t.Fatalf("reason = %v", *p.WavesPausedReason)
	}
	mustDo(t, e.svc.Resume(ctx, e.project.ID))
	mustDo(t, e.svc.Resume(ctx, e.project.ID)) // not paused: succeeds
	if p = e.project2(); p.WavesPaused || p.WavesPausedReason != nil {
		t.Fatalf("resumed = %v %v", p.WavesPaused, p.WavesPausedReason)
	}
	evs := e.events() // newest first
	var kinds []string
	for _, ev := range evs {
		kinds = append(kinds, ev.Kind)
	}
	want := []string{"resumed", "resumed", "paused", "paused"}
	if len(kinds) != 4 || kinds[0] != want[0] || kinds[1] != want[1] || kinds[2] != want[2] || kinds[3] != want[3] {
		t.Fatalf("kinds = %v", kinds)
	}
	var d map[string]string
	_ = json.Unmarshal(evs[2].Detail, &d)
	if d["reason"] != "second reason" {
		t.Fatalf("paused detail = %v", d)
	}

	// No open plan: the flag still changes, no event is written.
	mustDo(t, store.SetPlanStatus(ctx, testPool, e.plan.ID, store.PlanDone))
	mustDo(t, e.svc.Pause(ctx, e.project.ID, "x"))
	if n := len(e.events()); n != 4 {
		t.Fatalf("events after a pause with no open plan = %d", n)
	}
	if err := e.svc.Pause(ctx, "no-such-project", "x"); err == nil {
		t.Fatal("pausing an unknown project succeeded")
	}
}

// Reasons are redacted and bounded.
func TestEscalationReasonIsRedactedAndCapped(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	run := e.newRun()
	mustDo(t, e.svc.OnIssueTaken(ctx, e.project.ID, 11, run.ID))
	long := "glpat-abcdefghijklmnopqrst0123 " + string(make([]rune, 0))
	for len(long) < 3*ReasonMax {
		long += "word "
	}
	_, err := e.svc.Escalate(ctx, e.get("T1"), "other", long, run.ID)
	mustDo(t, err)
	p := e.project2()
	if got := *p.WavesPausedReason; len(got) > ReasonMax+4 || contains(got, "glpat-abcdefghijklmnopqrst0123") {
		t.Fatalf("reason = %d bytes %q", len(got), got[:40])
	}
}
