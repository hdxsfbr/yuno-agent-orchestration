// Package plans keeps the state of a plan's tickets true to the platform (DESIGN 5.29 rules
// 6 to 10): intake's take sets a ticket building, its merge request record sets it in
// review, the merge sets it merged, and a ticket whose run ends badly, or whose agent
// escalates, is failed, pauses the project's waves, is recorded as a plan event and is told
// to the Chief of Staff's Telegram conversation.
package plans

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"yuno/internal/events"
	"yuno/internal/obs"
	"yuno/internal/project"
	"yuno/internal/store"
)

// EventPlanUpdated is the SSE event that says a plan, its tickets or its events changed;
// its data is {"plan_id", "project_id"}.
const EventPlanUpdated = "plan.updated"

// Escalation kinds of the escalate tool (rule 6).
const (
	KindPRDGap      = "prd_gap"
	KindDesignWrong = "design_wrong"
	KindLoopLimit   = "loop_limit"
	KindOther       = "other"
)

// ValidKind reports whether k is one of the escalation kinds.
func ValidKind(k string) bool {
	switch k {
	case KindPRDGap, KindDesignWrong, KindLoopLimit, KindOther:
		return true
	}
	return false
}

// ReasonMax caps a reason written to the pause, the event and the message.
const ReasonMax = 2000

// Reporter sends a platform message into a conversation (the engine satisfies it).
type Reporter interface {
	SendPlatformMessage(ctx context.Context, runID, conversationID, key, text string) (deferred bool)
}

// Service applies the rules. Hub (SSE) and Reporter (Telegram) may be nil.
type Service struct {
	Pool     store.Querier
	Hub      *events.Hub
	Reporter Reporter
	Log      *slog.Logger

	// SweepInterval is how often Run sweeps; DefaultSweepInterval when zero.
	SweepInterval time.Duration
	// Now is the clock of the sweeps (nil: the system's).
	Now func() time.Time

	notified sync.Map // plan event id → struct{}: notified in this process
	wg       sync.WaitGroup

	mu       sync.Mutex
	lastScan time.Time // publishReviews: tickets in review changed since
}

const (
	// DefaultSweepInterval is how often the resend sweep runs (rule 8).
	DefaultSweepInterval = time.Minute
	// ResendAfter is how old an unnotified event is before the sweep sends it again: the
	// original send is allowed to finish first.
	ResendAfter = 30 * time.Second
	// ResendWindow is how far back the sweep looks; older notices are not sent.
	ResendWindow = 24 * time.Hour
	sweepLimit   = 50
)

// notifiable are the event kinds told to the operator (rule 8).
var notifiable = []string{store.PlanEventEscalated, store.PlanEventSmokeTestWaiting, store.PlanEventAcceptWaiting,
	store.PlanEventWaveDone, store.PlanEventFeatureDone}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func beginFunc(ctx context.Context, b beginner, fn func(pgx.Tx) error) error {
	tx, err := b.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	return tx.Commit(ctx)
}

// beginner is what a pool offers to open a transaction.
type beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// inTx runs fn in one transaction when the pool can open one, else directly on the pool.
func (s *Service) inTx(ctx context.Context, fn func(q store.Querier) error) error {
	if b, ok := s.Pool.(beginner); ok {
		return beginFunc(ctx, b, func(tx pgx.Tx) error { return fn(tx) })
	}
	return fn(s.Pool)
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Wait blocks until the notifications in flight are sent (tests, shutdown).
func (s *Service) Wait() { s.wg.Wait() }

// Publish tells SSE subscribers that the plan changed.
func (s *Service) Publish(planID, projectID string) {
	if s == nil || s.Hub == nil {
		return
	}
	data, _ := json.Marshal(map[string]string{"plan_id": planID, "project_id": projectID})
	s.Hub.Publish(events.Event{Type: EventPlanUpdated, Data: data, At: time.Now().UTC()})
}

// openPlanOf returns the ticket's plan when it is still open, nil otherwise.
func (s *Service) openPlanOf(ctx context.Context, t *store.PlanTicket) (*store.Plan, error) {
	p, err := store.GetPlan(ctx, s.Pool, t.PlanID)
	if err != nil {
		return nil, err
	}
	if p.Status == store.PlanDone || p.Status == store.PlanCancelled {
		return nil, nil
	}
	return p, nil
}

// OnIssueTaken links the run that intake created for an issue to the plan ticket carrying
// that issue and sets the ticket building (rule 10). An issue that belongs to no plan is
// ignored. A run that ended before the link was made is handled as OnRunTerminal would.
func (s *Service) OnIssueTaken(ctx context.Context, projectID string, issueNumber int64, runID string) error {
	t, err := store.TicketByIssue(ctx, s.Pool, projectID, issueNumber)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	plan, err := s.openPlanOf(ctx, t)
	if err != nil || plan == nil {
		return err
	}
	moved, err := store.LinkTicketRun(ctx, s.Pool, t.PlanID, t.TicketID, runID)
	if err != nil || !moved {
		return err
	}
	s.Publish(plan.ID, plan.ProjectID)
	// The run may have ended before it was linked (a refusal at its first node).
	if run, err := store.GetRun(ctx, s.Pool, runID); err == nil && store.IsTerminal(run.Status) {
		s.OnRunTerminal(ctx, run)
	}
	return nil
}

// OnMergeRequestRecorded sets the ticket of the run in review and links the record (rule
// 10). The store does this itself when the record is inserted (InsertMergeRequest); this
// is the same step for a caller that holds only the ids, and it publishes the change.
func (s *Service) OnMergeRequestRecorded(ctx context.Context, runID, mrID string) error {
	planID, _, err := store.LinkTicketMergeRequest(ctx, s.Pool, runID, mrID)
	if err != nil || planID == "" {
		return err
	}
	if p, err := store.GetPlan(ctx, s.Pool, planID); err == nil {
		s.Publish(p.ID, p.ProjectID)
	}
	return nil
}

// OnMergeRequestMerged sets the ticket whose merge request merged to merged (rule 10).
func (s *Service) OnMergeRequestMerged(ctx context.Context, mrID string) error {
	t, err := store.TicketByMergeRequest(ctx, s.Pool, mrID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	moved, err := store.MoveTicket(ctx, s.Pool, t.PlanID, t.TicketID, store.TicketMerged,
		store.TicketPending, store.TicketReleased, store.TicketBuilding, store.TicketInReview)
	if err != nil || !moved {
		return err
	}
	if p, err := store.GetPlan(ctx, s.Pool, t.PlanID); err == nil {
		s.Publish(p.ID, p.ProjectID)
	}
	return nil
}

// OnRunTerminal applies rule 7: a plan ticket's run that ended failed, timed_out or
// cancelled marks the ticket failed, pauses the waves and records an escalation with the
// run's reason, unless the ticket is failed already (an escalate call recorded it).
func (s *Service) OnRunTerminal(ctx context.Context, run *store.Run) {
	if s == nil || run == nil {
		return
	}
	if run.Trigger == store.TriggerPlan && run.TriggerRef != nil {
		if planID, wave, ok := store.ParseShipRef(*run.TriggerRef); ok {
			s.onShipRunTerminal(ctx, run, planID, wave)
			return
		}
	}
	switch run.Status {
	case store.RunFailed, store.RunTimedOut, store.RunCancelled:
	default:
		return
	}
	t, err := store.TicketByRun(ctx, s.Pool, run.ID)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		s.log().Warn("plans: read the ticket of a finished run failed", "run_id", run.ID, "error", err)
		return
	}
	reason := run.Status
	if run.FailureReason != nil && strings.TrimSpace(*run.FailureReason) != "" {
		reason = *run.FailureReason
	}
	kind := ""
	if k, rest, ok := parseEscalated(reason); ok {
		kind, reason = k, rest
	}
	if _, err := s.Escalate(ctx, t, kind, reason, run.ID); err != nil {
		s.log().Warn("plans: escalating a failed ticket run failed", "run_id", run.ID, "error", err)
	}
}

// Escalate is the step rules 6 and 7 share: the ticket becomes failed, the project's
// waves are paused with the reason, an `escalated` event is written and the operator is
// told. kind is "" for a run that failed for another reason. It does nothing, and returns
// the zero event, when the ticket is failed already, merged or closed or its plan is
// finished. runID is the run that ended.
func (s *Service) Escalate(ctx context.Context, t *store.PlanTicket, kind, reason, runID string) (*store.PlanEvent, error) {
	plan, err := s.openPlanOf(ctx, t)
	if err != nil || plan == nil {
		return nil, err
	}
	reason = cut(obs.Redact(strings.TrimSpace(reason)), ReasonMax)
	detail := map[string]any{"reason": reason, "run_id": runID}
	if kind != "" {
		detail["kind"] = kind
	}
	raw, _ := json.Marshal(detail)
	wave, ticket := t.Wave, t.TicketID
	ev := &store.PlanEvent{PlanID: plan.ID, Kind: store.PlanEventEscalated, Wave: &wave, TicketID: &ticket, Detail: raw}
	// The ticket's move, the pause and the event are one transaction (review M3): a failure
	// half way leaves none of them, and the run's end tries again.
	var moved bool
	err = s.inTx(ctx, func(q store.Querier) error {
		var err error
		if moved, err = store.MoveTicket(ctx, q, t.PlanID, t.TicketID, store.TicketFailed,
			store.TicketPending, store.TicketReleased, store.TicketBuilding, store.TicketInReview); err != nil || !moved {
			return err
		}
		if err := store.SetWavesPaused(ctx, q, plan.ProjectID, true, reason); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("pause the waves: %w", err)
		}
		if err := store.AddPlanEvent(ctx, q, ev); err != nil {
			return fmt.Errorf("record the escalation: %w", err)
		}
		return nil
	})
	if err != nil || !moved {
		return nil, err
	}
	s.Publish(plan.ID, plan.ProjectID)
	s.Notify(ctx, plan, ev, escalationText(plan, t, kind, reason))
	return ev, nil
}

// parseEscalated splits "escalated (<kind>): <reason>", the reason an escalated run fails
// with (engine.EscalateReason).
func parseEscalated(reason string) (kind, rest string, ok bool) {
	after, found := strings.CutPrefix(reason, "escalated (")
	if !found {
		return "", "", false
	}
	kind, rest, found = strings.Cut(after, "): ")
	if !found || !ValidKind(kind) {
		return "", "", false
	}
	return kind, rest, true
}

func escalationText(plan *store.Plan, t *store.PlanTicket, kind, reason string) string {
	how := "failed"
	if kind != "" {
		how = "escalated (" + kind + ")"
	}
	return fmt.Sprintf("[platform] Feature %s, ticket %s (%s) %s: %s. The waves of the project are paused; resume them from the project page once it is sorted out.",
		plan.Feature, t.TicketID, t.Title, how, sentenceEnd(reason))
}

// NotifyEvent tells the operator of a plan event in the words the resend sweep would use
// (rule 8); see Notify.
func (s *Service) NotifyEvent(ctx context.Context, plan *store.Plan, ev *store.PlanEvent) {
	if s == nil || ev == nil {
		return
	}
	s.Notify(ctx, plan, ev, s.noticeText(ctx, plan, ev))
}

// Notify delivers text to the conversation of the agent bound to Telegram, as the budget
// alerts are, once per plan event: the idempotency key is plan-event-<event id>, and an
// event notified in this process is not sent again. It returns at once; the send runs in
// the background (Wait). A send that went out sets the event's notified_at; without a
// binding or a conversation, or when the send is deferred, the event stays unnotified and
// the sweep (Run) tries again (review M2).
func (s *Service) Notify(ctx context.Context, plan *store.Plan, ev *store.PlanEvent, text string) {
	s.notify(ctx, plan, ev, text, false)
}

func (s *Service) notify(ctx context.Context, plan *store.Plan, ev *store.PlanEvent, text string, resend bool) {
	if s == nil || ev == nil {
		return
	}
	if _, dup := s.notified.LoadOrStore(ev.ID, struct{}{}); dup {
		return
	}
	wctx := context.WithoutCancel(ctx)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		convID, ok, err := project.BoundAgentConversation(wctx, s.Pool)
		switch {
		case err != nil:
			s.notified.Delete(ev.ID)
			s.log().Warn("plans: the operator could not be told", "plan_id", plan.ID, "event", ev.Kind, "error", err)
		case !ok || s.Reporter == nil:
			s.notified.Delete(ev.ID)
			if resend {
				return
			}
			s.log().Info("plans: nowhere to tell the operator: no Telegram binding or conversation", "plan_id", plan.ID, "event", ev.Kind, "text", text)
		default:
			if s.Reporter.SendPlatformMessage(wctx, "", convID, NotifyKey(ev.ID), text) {
				s.notified.Delete(ev.ID) // deferred: the sweep tries again
				s.log().Info("plans: notice deferred", "plan_id", plan.ID, "event", ev.Kind)
				return
			}
			if err := store.MarkPlanEventNotified(wctx, s.Pool, ev.ID, s.now()); err != nil {
				s.log().Warn("plans: marking an event notified failed", "plan_id", plan.ID, "event", ev.Kind, "error", err)
			}
		}
	}()
}

// Run sweeps every SweepInterval until ctx ends, the first sweep at once.
func (s *Service) Run(ctx context.Context) {
	interval := s.SweepInterval
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.Sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep sends again the notices of events not notified (rule 8, review M2): events of the
// notifiable kinds with notified_at null, older than ResendAfter and younger than
// ResendWindow, under the same idempotency key as the first send; and it publishes
// plan.updated for tickets that went into review (the link is made in the tool's own
// transaction, which has no hub).
func (s *Service) Sweep(ctx context.Context) {
	if s == nil {
		return
	}
	s.publishReviews(ctx)
	evs, err := store.ListUnnotifiedPlanEvents(ctx, s.Pool, notifiable, ResendAfter, ResendWindow, sweepLimit)
	if err != nil {
		if ctx.Err() == nil {
			s.log().Warn("plans: list the events to tell failed", "error", err)
		}
		return
	}
	for i := range evs {
		ev := &evs[i]
		plan, err := store.GetPlan(ctx, s.Pool, ev.PlanID)
		if err != nil {
			continue
		}
		s.notify(ctx, plan, ev, s.noticeText(ctx, plan, ev), true)
	}
}

func (s *Service) publishReviews(ctx context.Context) {
	s.mu.Lock()
	since := s.lastScan
	s.lastScan = s.now().Add(-2 * time.Second)
	s.mu.Unlock()
	if since.IsZero() {
		return
	}
	ids, err := store.PlansWithTicketsInReviewSince(ctx, s.Pool, since)
	if err != nil {
		if ctx.Err() == nil {
			s.log().Warn("plans: list the tickets in review failed", "error", err)
		}
		return
	}
	for _, id := range ids {
		s.Publish(id[0], id[1])
	}
}

// noticeText rebuilds the text of an event's notice for a resend.
func (s *Service) noticeText(ctx context.Context, plan *store.Plan, ev *store.PlanEvent) string {
	var d struct {
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(ev.Detail, &d)
	wave := 0
	if ev.Wave != nil {
		wave = *ev.Wave
	}
	switch ev.Kind {
	case store.PlanEventEscalated:
		if ev.TicketID == nil {
			return shipEndText(plan, wave, d.Reason)
		}
		t := &store.PlanTicket{TicketID: "?"}
		if ev.TicketID != nil {
			t.TicketID = *ev.TicketID
			if list, err := store.ListPlanTickets(ctx, s.Pool, plan.ID); err == nil {
				for _, c := range list {
					if c.TicketID == t.TicketID {
						t = &c
						break
					}
				}
			}
		}
		return escalationText(plan, t, d.Kind, d.Reason)
	case store.PlanEventSmokeTestWaiting:
		return fmt.Sprintf("[platform] Feature %s, wave %d is waiting for your smoke test. Approve it once the app checks out.", plan.Feature, wave)
	case store.PlanEventAcceptWaiting:
		return fmt.Sprintf("[platform] Feature %s is waiting for your acceptance. Check the live app against the PRD.", plan.Feature)
	case store.PlanEventWaveDone:
		return fmt.Sprintf("[platform] Feature %s, wave %d is done.", plan.Feature, wave)
	case store.PlanEventFeatureDone:
		return fmt.Sprintf("[platform] Feature %s is done.", plan.Feature)
	}
	return fmt.Sprintf("[platform] Feature %s: %s.", plan.Feature, ev.Kind)
}

// ErrPlanFinished is Cancel's answer for a plan that is done or cancelled already.
var ErrPlanFinished = store.ErrPlanFinished

// Cancel ends an open plan (review M9): the plan becomes cancelled, which frees the
// project for another plan and the feature name for reuse, and a `cancelled` event records
// the reason. Its tickets and their issues stay as they are. store.ErrNotFound for an
// unknown plan, ErrPlanFinished for one that is finished.
func (s *Service) Cancel(ctx context.Context, planID, reason string) (*store.Plan, error) {
	reason = cut(obs.Redact(strings.TrimSpace(reason)), ReasonMax)
	raw, _ := json.Marshal(map[string]any{"reason": reason})
	err := s.inTx(ctx, func(q store.Querier) error {
		if err := store.CancelPlan(ctx, q, planID); err != nil {
			return err
		}
		return store.AddPlanEvent(ctx, q, &store.PlanEvent{PlanID: planID, Kind: store.PlanEventCancelled, Detail: raw})
	})
	if err != nil {
		return nil, err
	}
	plan, err := store.GetPlan(ctx, s.Pool, planID)
	if err != nil {
		return nil, err
	}
	s.Publish(plan.ID, plan.ProjectID)
	return plan, nil
}

// OnRunRetried publishes the change of a ticket that an operator's retry of its run moved
// out of failed (the move itself is store.RetryTicketOfRun, made in the retry's own
// transaction).
func (s *Service) OnRunRetried(planID, projectID string) { s.Publish(planID, projectID) }

// ReapplyMerged sets the tickets whose merge request record is merged to merged although
// they are not (review L5): a transition the sync missed, as when the process stopped
// between the record's update and the ticket's. It returns how many it moved.
func (s *Service) ReapplyMerged(ctx context.Context) (int, error) {
	list, err := store.MergedTicketsToApply(ctx, s.Pool, 200)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range list {
		if t.MergeRequestID == nil {
			continue
		}
		moved, err := store.MoveTicket(ctx, s.Pool, t.PlanID, t.TicketID, store.TicketMerged,
			store.TicketPending, store.TicketReleased, store.TicketBuilding, store.TicketInReview)
		if err != nil {
			return n, err
		}
		if moved {
			n++
			if p, err := store.GetPlan(ctx, s.Pool, t.PlanID); err == nil {
				s.Publish(p.ID, p.ProjectID)
			}
		}
	}
	return n, nil
}

// NotifyKey is the chat.send idempotency key of an event's notice.
func NotifyKey(eventID string) string { return "plan-event-" + eventID }

// Pause and Resume change a project's wave pause (rule 9) and write the event on the
// project's open plan, when it has one.
func (s *Service) Pause(ctx context.Context, projectID, reason string) error {
	return s.setPaused(ctx, projectID, true, reason)
}

// Resume clears the pause.
func (s *Service) Resume(ctx context.Context, projectID string) error {
	return s.setPaused(ctx, projectID, false, "")
}

func (s *Service) setPaused(ctx context.Context, projectID string, paused bool, reason string) error {
	reason = cut(obs.Redact(strings.TrimSpace(reason)), ReasonMax)
	if err := store.SetWavesPaused(ctx, s.Pool, projectID, paused, reason); err != nil {
		return err
	}
	plan, err := store.OpenPlanForProject(ctx, s.Pool, projectID)
	if err != nil || plan == nil {
		return err
	}
	kind, detail := store.PlanEventResumed, map[string]any{}
	if paused {
		kind, detail = store.PlanEventPaused, map[string]any{"reason": reason}
	}
	raw, _ := json.Marshal(detail)
	if err := store.AddPlanEvent(ctx, s.Pool, &store.PlanEvent{PlanID: plan.ID, Kind: kind, Detail: raw}); err != nil {
		return err
	}
	s.Publish(plan.ID, plan.ProjectID)
	return nil
}

func sentenceEnd(s string) string { return strings.TrimRight(s, ". \n") }

// cut shortens s to at most max bytes on a rune boundary, with an ellipsis.
func cut(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// Stages of a ship run, as the plan events name them.
const (
	stageSmokeTest = "smoke_test"
	stageAccept    = "accept_feature"
)

// onShipRunTerminal applies rules 14 and 15 to a ship run that ended (DESIGN 5.29): one
// that succeeded after the operator approved `accept_feature` finishes the plan (`done`,
// `feature_done`); one that ended any other way pauses the project's waves with the reason
// and records an `escalated` event without a ticket. The operator's rejection of the smoke
// test reads "smoke test rejected: <reason>", of the acceptance "feature not accepted:
// <reason>", any other ending the run's own reason. Both steps run once per ending
// of a run: the event carries the run id and its retry count, so a run retried after an
// escalation escalates again when it ends badly again. A run that succeeded without its
// wave closed (no `wave_done`) or, on the last wave, without the acceptance approved, is
// escalated as well: "ship run for wave N finished without closing the wave".
func (s *Service) onShipRunTerminal(ctx context.Context, run *store.Run, planID string, wave int) {
	plan, err := store.GetPlan(ctx, s.Pool, planID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log().Warn("plans: read the plan of a finished ship run failed", "run_id", run.ID, "error", err)
		}
		return
	}
	if plan.Status == store.PlanDone || plan.Status == store.PlanCancelled {
		return
	}
	approvals, err := store.ListApprovals(ctx, s.Pool, "", run.ID, 500)
	if err != nil {
		s.log().Warn("plans: read the approvals of a finished ship run failed", "run_id", run.ID, "error", err)
		return
	}
	stranded := ""
	if run.Status == store.RunSucceeded {
		for _, a := range approvals {
			if a.Kind == store.ApprovalKindNode && a.NodeID == stageAccept && a.Status == store.ApprovalApproved {
				s.finishPlan(ctx, plan, wave, run.ID)
				return
			}
		}
		// Otherwise the wave must be closed, which advance_wave records (`wave_done`), and a
		// plan that is accepting must have had its acceptance approved; a run that ended
		// well without either left the plan stranded, and the operator is told (rule 14).
		_, err := store.FindPlanEvent(ctx, s.Pool, plan.ID, store.PlanEventWaveDone, wave, "")
		switch {
		case err != nil && !errors.Is(err, store.ErrNotFound):
			s.log().Warn("plans: read the wave's record failed", "run_id", run.ID, "error", err)
			return
		case err == nil && plan.Status != store.PlanAccepting:
			return // advance_wave moved the plan on
		case err == nil:
			stranded = fmt.Sprintf("ship run for wave %d finished without the feature being accepted", wave)
		default:
			stranded = fmt.Sprintf("ship run for wave %d finished without closing the wave", wave)
		}
	}
	stage, reason := "", ""
	for _, a := range approvals {
		if stranded != "" {
			break
		}
		if a.Kind != store.ApprovalKindNode || a.Status != store.ApprovalRejected || (a.NodeID != stageSmokeTest && a.NodeID != stageAccept) {
			continue
		}
		why := "no reason given"
		if a.Reason != nil && strings.TrimSpace(*a.Reason) != "" {
			why = strings.TrimSpace(*a.Reason)
		}
		stage = a.NodeID
		if stage == stageSmokeTest {
			reason = "smoke test rejected: " + why
		} else {
			reason = "feature not accepted: " + why
		}
	}
	if stage == "" && stranded == "" && run.FailureReason != nil {
		// The engine fails a run at an approval node without on_reject as "rejected at
		// <node>: <operator's reason>".
		for _, n := range []string{stageSmokeTest, stageAccept} {
			if why, ok := strings.CutPrefix(strings.TrimSpace(*run.FailureReason), "rejected at "+n); ok {
				why = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(why), ":"))
				if why == "" {
					why = "no reason given"
				}
				stage = n
				reason = "smoke test rejected: " + why
				if n == stageAccept {
					reason = "feature not accepted: " + why
				}
			}
		}
	}
	if stranded != "" {
		stage, reason = "ship", stranded
	}
	if stage == "" {
		stage, reason = "ship", "ship run "+run.Status
		if run.FailureReason != nil && strings.TrimSpace(*run.FailureReason) != "" {
			reason = strings.TrimSpace(*run.FailureReason)
		}
	}
	reason = cut(obs.Redact(reason), ReasonMax)
	raw, _ := json.Marshal(map[string]any{"reason": reason, "run_id": run.ID, "retries": run.Retries, "stage": stage})
	ev := &store.PlanEvent{PlanID: plan.ID, Kind: store.PlanEventEscalated, Wave: &wave, Detail: raw}
	recorded := false
	err = s.inTx(ctx, func(q store.Querier) error {
		if _, err := store.FindPlanEventRetry(ctx, q, plan.ID, store.PlanEventEscalated, wave, run.ID, run.Retries); err == nil {
			return nil // this ending of the run is recorded already (a retry ends anew)
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := store.SetWavesPaused(ctx, q, plan.ProjectID, true, reason); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("pause the waves: %w", err)
		}
		if err := store.AddPlanEvent(ctx, q, ev); err != nil {
			return fmt.Errorf("record the ship run's ending: %w", err)
		}
		recorded = true
		return nil
	})
	if err != nil {
		s.log().Warn("plans: pausing for a finished ship run failed", "run_id", run.ID, "error", err)
		return
	}
	if !recorded {
		return
	}
	s.Publish(plan.ID, plan.ProjectID)
	s.Notify(ctx, plan, ev, shipEndText(plan, wave, reason))
}

// finishPlan sets an accepting plan done and records `feature_done` (rule 15).
func (s *Service) finishPlan(ctx context.Context, plan *store.Plan, wave int, runID string) {
	raw, _ := json.Marshal(map[string]any{"run_id": runID})
	ev := &store.PlanEvent{PlanID: plan.ID, Kind: store.PlanEventFeatureDone, Wave: &wave, Detail: raw}
	moved := false
	err := s.inTx(ctx, func(q store.Querier) error {
		var err error
		if moved, err = store.MovePlan(ctx, q, plan.ID, store.PlanDone, plan.CurrentWave, store.PlanAccepting); err != nil || !moved {
			return err
		}
		return store.AddPlanEvent(ctx, q, ev)
	})
	if err != nil {
		s.log().Warn("plans: finishing the plan failed", "plan_id", plan.ID, "run_id", runID, "error", err)
		return
	}
	if !moved {
		return
	}
	s.Publish(plan.ID, plan.ProjectID)
	s.NotifyEvent(ctx, plan, ev)
}

func shipEndText(plan *store.Plan, wave int, reason string) string {
	return fmt.Sprintf("[platform] Feature %s, wave %d: %s. The waves of the project are paused; resume them from the project page once it is sorted out.",
		plan.Feature, wave, sentenceEnd(reason))
}
