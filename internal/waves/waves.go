// Package waves moves a plan's waves along by itself (DESIGN 5.29 rules 11 to 14): the
// watcher releases wave 1 when the plan merge request merged, starts one ship run per wave
// once its tickets merged and the default branch is green after them, and Advance, called
// by the Integrator's advance_wave tool, closes a shipped wave and releases the next. While a
// revision of the plan runs, no wave is released and no ship run started (D-089).
package waves

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"yuno/internal/deps"
	"yuno/internal/engine"
	"yuno/internal/plans"
	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// ShipTemplateKey is the template of the ship pipeline (rule 13).
const ShipTemplateKey = "ship_pipeline"

// DefaultInterval is how often the watcher passes when Interval is unset (rule 12).
const DefaultInterval = time.Minute

// passTimeout bounds the work on one plan in one pass.
const passTimeout = 2 * time.Minute

// Triggerer is the part of the engine the watcher needs: the trigger transaction every run goes through.
type Triggerer interface {
	Trigger(ctx context.Context, req engine.TriggerRequest) (*store.Run, error)
}

// Service is the wave watcher and the advance of a shipped wave. Pool, Plans and Engine are
// required; Projects and Client serve the providers unless the Config, Issues and Requests
// hooks (tests) replace them.
type Service struct {
	Pool     store.Querier
	Plans    *plans.Service
	Projects *project.Resolver
	Client   *http.Client
	Engine   Triggerer
	// Interval is WAVE_WATCH_INTERVAL; DefaultInterval when zero.
	Interval time.Duration
	Log      *slog.Logger

	// Config returns a project's provider configuration, token included (default: the
	// project's settings and the Resolver's token).
	Config func(ctx context.Context, p *store.Project) (*project.Config, error)
	// Issues and Requests build the providers (default: tools.IssueProviderFor and tools.ProviderFor over Client).
	Issues   func(pc *project.Config) tools.IssueProvider
	Requests func(pc *project.Config) tools.RequestProvider
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

type beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// inTx runs fn in one transaction when the pool can open one, else directly on the pool.
func (s *Service) inTx(ctx context.Context, fn func(q store.Querier) error) error {
	b, ok := s.Pool.(beginner)
	if !ok {
		return fn(s.Pool)
	}
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

func (s *Service) config(ctx context.Context, p *store.Project) (*project.Config, error) {
	if s.Config != nil {
		return s.Config(ctx, p)
	}
	token, err := s.Projects.Token(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	pc := project.ConfigOf(p.Settings(), token)
	return &pc, nil
}

func (s *Service) issues(pc *project.Config) tools.IssueProvider {
	if s.Issues != nil {
		return s.Issues(pc)
	}
	return tools.IssueProviderFor(pc, s.Client)
}

func (s *Service) requests(pc *project.Config) tools.RequestProvider {
	if s.Requests != nil {
		return s.Requests(pc)
	}
	return tools.ProviderFor(pc, s.Client)
}

// gate judges the prerequisites of the plans' tickets (Chief platform control, DESIGN wave 3
// rules 4 and 6), reading a merge commit's CI through its project's provider.
func (s *Service) gate() *deps.Gate {
	return &deps.Gate{Pool: s.Pool, Provider: func(ctx context.Context, mr *store.MergeRequest) (*project.Config, tools.RequestProvider, error) {
		proj, err := store.GetProject(ctx, s.Pool, mr.ProjectID)
		if err != nil {
			return nil, nil, err
		}
		pc, err := s.config(ctx, proj)
		if err != nil {
			return nil, nil, err
		}
		if pc.DefaultBranch == "" {
			c := *pc
			c.DefaultBranch = proj.DefaultBranch
			pc = &c
		}
		return pc, s.requests(pc), nil
	}}
}

// held judges the tickets of the wave that have prerequisites and returns the ones that are
// not to be labelled for intake (DESIGN wave 3 rule 6): a prerequisite is not satisfied, or
// could not be judged now (the next pass judges it again).
func (s *Service) held(ctx context.Context, planID string, wave int) (map[string]bool, error) {
	open, err := store.TicketsWithOpenDependencies(ctx, s.Pool, planID)
	if err != nil || len(open) == 0 {
		return nil, err
	}
	tickets, err := store.ListWaveTickets(ctx, s.Pool, planID, wave)
	if err != nil {
		return nil, err
	}
	held := map[string]bool{}
	g := s.gate()
	for _, t := range tickets {
		if !open[t.TicketID] {
			continue
		}
		st, err := g.Ticket(ctx, planID, t.TicketID)
		if err != nil {
			s.log().Warn("waves: judging a ticket's prerequisites failed; it waits for the next pass", "plan_id", planID,
				"ticket", t.TicketID, "error", err)
			held[t.TicketID] = true
			continue
		}
		if !st.Satisfied {
			held[t.TicketID] = true
		}
	}
	return held, nil
}

// ReleaseSatisfied labels for intake the held tickets of every building plan whose
// prerequisites are now satisfied (DESIGN wave 3 rules 6 and 8b); the dependency watcher
// calls it on the merge request sync tick, and each pass of the wave watcher does too. A
// failure on one plan leaves the others to their turn.
func (s *Service) ReleaseSatisfied(ctx context.Context) error {
	list, err := store.ListOpenPlans(ctx, s.Pool)
	if err != nil {
		return err
	}
	var errs []error
	for i := range list {
		plan := &list[i]
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if plan.Status != store.PlanBuilding {
			continue
		}
		proj, err := store.GetProject(ctx, s.Pool, plan.ProjectID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if proj.DeletedAt != nil || proj.WavesPaused {
			continue
		}
		if err := s.releaseSatisfied(ctx, plan, proj, nil); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// releaseSatisfied is rule 6 for one plan: each pending ticket of its current, released wave
// that an edge holds is judged; the ones whose prerequisites are all satisfied are labelled
// and set released, and their edges cleared, under the plan lock with the pause and the
// revision read again, as a wave release does. A ticket whose prerequisite was closed
// without merging can never be released: it is escalated, which tells the operator. pc is
// the project's access, read here when nil and a ticket is ready.
func (s *Service) releaseSatisfied(ctx context.Context, plan *store.Plan, proj *store.Project, pc *project.Config) error {
	wave := plan.CurrentWave
	if _, err := store.FindPlanEvent(ctx, s.Pool, plan.ID, store.PlanEventWaveReleased, wave, ""); errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	open, err := store.TicketsWithOpenDependencies(ctx, s.Pool, plan.ID)
	if err != nil || len(open) == 0 {
		return err
	}
	tickets, err := store.ListWaveTickets(ctx, s.Pool, plan.ID, wave)
	if err != nil {
		return err
	}
	g := s.gate()
	var ready []store.WaveTicketMerge
	for _, t := range tickets {
		if t.Status != store.TicketPending || !open[t.TicketID] {
			continue
		}
		st, err := g.Ticket(ctx, plan.ID, t.TicketID)
		switch {
		case err != nil:
			s.log().Warn("waves: judging a ticket's prerequisites failed; the next pass tries again", "plan_id", plan.ID,
				"ticket", t.TicketID, "error", err)
		case len(st.Closed) > 0:
			s.escalate(ctx, t, st.Closed[0].Reason)
		case st.Satisfied:
			ready = append(ready, t)
		}
	}
	if len(ready) == 0 {
		return nil
	}
	if pc == nil {
		if pc, err = s.config(ctx, proj); err != nil {
			return fmt.Errorf("read the project's access: %w", err)
		}
	}
	released := 0
	err = s.inTx(ctx, func(q store.Querier) error {
		cur, err := store.LockPlan(ctx, q, plan.ID)
		if err != nil {
			return err
		}
		if cur.Status != store.PlanBuilding || cur.CurrentWave != wave {
			return nil
		}
		if paused, _, err := pausedIn(ctx, q, cur.ProjectID); err != nil || paused {
			return err
		}
		if revising, err := s.revising(ctx, q, cur); err != nil || revising {
			return err
		}
		prov := s.issues(pc)
		var ids []string
		for _, t := range ready {
			if t.IssueNumber == nil {
				return fmt.Errorf("ticket %s has no issue to release", t.TicketID)
			}
			if err := prov.SwapLabels(ctx, *t.IssueNumber, proj.IntakeLabel, ""); err != nil {
				return fmt.Errorf("label the issue of ticket %s: %w", t.TicketID, err)
			}
			moved, err := store.MoveTicket(ctx, q, plan.ID, t.TicketID, store.TicketReleased, store.TicketPending)
			if err != nil {
				return err
			}
			if moved {
				released++
				ids = append(ids, t.TicketID)
			}
		}
		return store.ClearTicketDependencies(ctx, q, plan.ID, ids, time.Now())
	})
	if err != nil || released == 0 {
		return err
	}
	s.Plans.Publish(plan.ID, plan.ProjectID)
	s.log().Info("waves: tickets released once their prerequisites were satisfied", "plan_id", plan.ID, "wave", wave, "count", released)
	return nil
}

// Run passes every Interval until ctx ends, the first one at once (rule 12).
func (s *Service) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	s.log().Info("wave watcher started", "interval", interval.String())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.Pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Pass looks once at every open plan. A failure on one plan is logged and leaves the others
// to their turn; a tracker that does not answer is retried by the next pass and never
// escalated (rule 12, failure handling).
func (s *Service) Pass(ctx context.Context) {
	list, err := store.ListOpenPlans(ctx, s.Pool)
	if err != nil {
		if ctx.Err() == nil {
			s.log().Warn("waves: list the open plans failed", "error", err)
		}
		return
	}
	for i := range list {
		if ctx.Err() != nil {
			return
		}
		pctx, cancel := context.WithTimeout(ctx, passTimeout)
		s.passPlan(pctx, &list[i])
		cancel()
	}
}

func (s *Service) passPlan(ctx context.Context, plan *store.Plan) {
	log := s.log().With("plan_id", plan.ID, "feature", plan.Feature)
	proj, err := store.GetProject(ctx, s.Pool, plan.ProjectID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) && ctx.Err() == nil {
			log.Warn("waves: read the project failed", "error", err)
		}
		return
	}
	if proj.DeletedAt != nil {
		return
	}
	switch plan.Status {
	case store.PlanWaitingPlanMerge:
		err = s.afterPlanMerge(ctx, plan, proj)
	case store.PlanBuilding:
		err = s.building(ctx, plan, proj)
	}
	if err != nil && ctx.Err() == nil {
		log.Warn("waves: pass over the plan failed; the next pass tries again", "status", plan.Status, "error", err)
	}
}

// afterPlanMerge releases the first wave once the plan's merge request merged (rule 12).
func (s *Service) afterPlanMerge(ctx context.Context, plan *store.Plan, proj *store.Project) error {
	if plan.PlanMRID == nil || proj.WavesPaused {
		return nil
	}
	mr, err := store.GetMergeRequest(ctx, s.Pool, *plan.PlanMRID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if mr.State != store.MergeRequestMerged {
		return nil
	}
	all, err := store.ListPlanTickets(ctx, s.Pool, plan.ID)
	if err != nil || len(all) == 0 {
		return err
	}
	if revising, err := s.revising(ctx, s.Pool, plan); err != nil || revising {
		return err
	}
	first := all[0].Wave // by wave
	pc, err := s.config(ctx, proj)
	if err != nil {
		return fmt.Errorf("read the project's access: %w", err)
	}
	held, err := s.held(ctx, plan.ID, first)
	if err != nil {
		return err
	}
	moved := false
	err = s.inTx(ctx, func(q store.Querier) error {
		cur, err := store.LockPlan(ctx, q, plan.ID)
		if err != nil {
			return err
		}
		if cur.Status != store.PlanWaitingPlanMerge {
			return nil
		}
		// The pause is read again under the plan lock and the project's row is held until the
		// commit, before the tracker is written (rule 12): a pause that landed since the
		// pass began stops the release, and one that comes later waits for it.
		if paused, _, err := pausedIn(ctx, q, cur.ProjectID); err != nil || paused {
			return err
		}
		if revising, err := s.revising(ctx, q, cur); err != nil || revising {
			return err
		}
		if err := s.labelWave(ctx, s.issues(pc), proj.IntakeLabel, plan.ID, first, held); err != nil {
			return err
		}
		if moved, err = store.MovePlan(ctx, q, cur.ID, store.PlanBuilding, first, store.PlanWaitingPlanMerge); err != nil || !moved {
			return err
		}
		_, err = releaseTx(ctx, q, cur.ID, first, held)
		return err
	})
	if err != nil || !moved {
		return err
	}
	s.Plans.Publish(plan.ID, plan.ProjectID)
	s.log().Info("waves: first wave released", "plan_id", plan.ID, "wave", first)
	return nil
}

// revising reports whether a revision of the plan is running: a run of its project whose
// input revise_plan names it and that has not ended (rule 12, D-089). While one runs the
// watcher releases no wave and starts no ship run for the plan; nothing is stored, so a
// revision that fails or is cancelled holds nothing back once it ended.
func (s *Service) revising(ctx context.Context, q store.Querier, plan *store.Plan) (bool, error) {
	id, err := store.LiveRevisionRun(ctx, q, plan.ProjectID, plan.ID)
	if err != nil {
		return false, fmt.Errorf("look for a revision of the plan: %w", err)
	}
	return id != "", nil
}

// pausedIn reads the project's wave pause inside the caller's transaction and holds the row
// shared until it ends, so a pause set meanwhile waits for the commit (rule 12).
func pausedIn(ctx context.Context, q store.Querier, projectID string) (paused bool, reason string, err error) {
	p, err := store.LockProjectPause(ctx, q, projectID)
	if err != nil {
		return false, "", err
	}
	if p.WavesPausedReason != nil {
		reason = *p.WavesPausedReason
	}
	return p.WavesPaused, reason, nil
}

// labelWave adds the project's intake label to the issue of each ticket of the wave that is
// still pending or released (rule 12); adding a label an issue carries is fine, so a retry
// repeats it. A ticket without an issue cannot be released. A held ticket (an unsatisfied
// prerequisite, Chief platform control DESIGN wave 3 rule 6) is not labelled.
func (s *Service) labelWave(ctx context.Context, prov tools.IssueProvider, label string, planID string, wave int, held map[string]bool) error {
	tickets, err := store.ListWaveTickets(ctx, s.Pool, planID, wave)
	if err != nil {
		return err
	}
	for _, t := range tickets {
		if t.Status != store.TicketPending && t.Status != store.TicketReleased || held[t.TicketID] {
			continue
		}
		if t.IssueNumber == nil {
			return fmt.Errorf("ticket %s has no issue to release", t.TicketID)
		}
		if err := prov.SwapLabels(ctx, *t.IssueNumber, label, ""); err != nil {
			return fmt.Errorf("label the issue of ticket %s: %w", t.TicketID, err)
		}
	}
	return nil
}

// releaseTx sets the wave's pending tickets released and records `wave_released` (once per
// wave), in the caller's transaction. The event is nil when it was recorded already. A held
// ticket stays pending until its prerequisites are satisfied (Chief platform control, DESIGN
// wave 3 rule 6); the edges of the released ones are cleared.
func releaseTx(ctx context.Context, q store.Querier, planID string, wave int, held map[string]bool) (*store.PlanEvent, error) {
	tickets, err := store.ListWaveTickets(ctx, q, planID, wave)
	if err != nil {
		return nil, err
	}
	n := 0
	var released []string
	for _, t := range tickets {
		if held[t.TicketID] {
			continue
		}
		if t.Status == store.TicketPending || t.Status == store.TicketReleased {
			released = append(released, t.TicketID)
		}
		moved, err := store.MoveTicket(ctx, q, planID, t.TicketID, store.TicketReleased, store.TicketPending)
		if err != nil {
			return nil, err
		}
		if moved {
			n++
		}
	}
	if err := store.ClearTicketDependencies(ctx, q, planID, released, time.Now()); err != nil {
		return nil, err
	}
	if _, err := store.FindPlanEvent(ctx, q, planID, store.PlanEventWaveReleased, wave, ""); err == nil {
		return nil, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	raw, _ := json.Marshal(map[string]any{"tickets": len(tickets)})
	w := wave
	ev := &store.PlanEvent{PlanID: planID, Kind: store.PlanEventWaveReleased, Wave: &w, Detail: raw}
	return ev, store.AddPlanEvent(ctx, q, ev)
}

// building is the watcher's look at a plan whose current wave is being built (rules 11
// and 12): tickets whose merge request or issue ended without a merge escalate, the default
// branch's status is read and recorded, and a wave whose tickets all merged, with the default
// branch green after the newest merge, gets its ship run unless the project is paused or a
// revision of the plan is running. A wave advance_wave held back for a revision is released
// once none runs (rule 14).
func (s *Service) building(ctx context.Context, plan *store.Plan, proj *store.Project) error {
	wave := plan.CurrentWave
	tickets, err := store.ListWaveTickets(ctx, s.Pool, plan.ID, wave)
	if err != nil || len(tickets) == 0 {
		return err
	}
	pc, err := s.config(ctx, proj)
	if err != nil {
		return fmt.Errorf("read the project's access: %w", err)
	}
	if s.escalateEnded(ctx, plan, proj, pc, tickets) {
		return nil
	}
	main, err := s.requests(pc).DefaultBranchStatus(ctx)
	if err != nil {
		return fmt.Errorf("read the default branch's status: %w", err)
	}
	m := store.PlanMain{State: main.State, SHA: main.SHA, URL: main.URL}
	if !main.At.IsZero() {
		at := main.At.UTC()
		m.At = &at
	}
	if changed, err := store.SetPlanMain(ctx, s.Pool, plan.ID, m); err != nil {
		return err
	} else if changed {
		s.Plans.Publish(plan.ID, plan.ProjectID)
	}
	if proj.WavesPaused {
		return nil
	}
	revising, err := s.revising(ctx, s.Pool, plan)
	if err != nil {
		return err
	}
	if !revising {
		if released, err := s.releaseHeld(ctx, plan, proj, pc, wave); err != nil || released {
			return err
		}
		// Tickets of the wave held by a prerequisite are released once it is satisfied
		// (Chief platform control, DESIGN wave 3 rule 6).
		if err := s.releaseSatisfied(ctx, plan, proj, pc); err != nil {
			return err
		}
	}
	attempt := 1
	if run, last, err := store.LatestShipRun(ctx, s.Pool, plan.ID, wave); err == nil {
		if !store.IsTerminal(run.Status) {
			// The run was started and the plan not moved (the process stopped between the two).
			return s.recordShipping(ctx, plan, wave, run.ID)
		}
		// The wave's latest attempt ended and the plan is building again: a revision added
		// or reset tickets of the wave (rule 16). The wave ships again, as a new attempt,
		// once those tickets merged, unless its `wave_done` is recorded already.
		if _, done, err := s.advanced(ctx, s.Pool, plan.ID, wave); err != nil || done {
			return err
		}
		attempt = last + 1
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if revising || !shippable(tickets, main) {
		return nil
	}
	return s.startShip(ctx, plan, proj, wave, attempt, tickets, main.URL)
}

// releaseHeld releases the current wave when advance_wave held it back because a revision of
// the plan was running (rule 14, D-089) and it was not released since: as afterPlanMerge
// does, the pause and the revision are read again under the plan lock, the issues labelled,
// then the tickets set released and `wave_released` recorded. It reports whether it released.
func (s *Service) releaseHeld(ctx context.Context, plan *store.Plan, proj *store.Project, pc *project.Config, wave int) (bool, error) {
	if held, err := store.HeldWave(ctx, s.Pool, plan.ID, wave); err != nil || !held {
		return false, err
	}
	if _, err := store.FindPlanEvent(ctx, s.Pool, plan.ID, store.PlanEventWaveReleased, wave, ""); err == nil {
		return false, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	held, err := s.held(ctx, plan.ID, wave)
	if err != nil {
		return false, err
	}
	var ev *store.PlanEvent
	err = s.inTx(ctx, func(q store.Querier) error {
		cur, err := store.LockPlan(ctx, q, plan.ID)
		if err != nil {
			return err
		}
		if cur.Status != store.PlanBuilding || cur.CurrentWave != wave {
			return nil
		}
		if paused, _, err := pausedIn(ctx, q, cur.ProjectID); err != nil || paused {
			return err
		}
		if revising, err := s.revising(ctx, q, cur); err != nil || revising {
			return err
		}
		if err := s.labelWave(ctx, s.issues(pc), proj.IntakeLabel, cur.ID, wave, held); err != nil {
			return err
		}
		ev, err = releaseTx(ctx, q, cur.ID, wave, held)
		return err
	})
	if err != nil || ev == nil {
		return false, err
	}
	s.Plans.Publish(plan.ID, plan.ProjectID)
	s.log().Info("waves: wave released after the plan's revision", "plan_id", plan.ID, "wave", wave)
	return true, nil
}

// shippable is rule 11: every ticket merged and the default branch passed for the pipeline
// of the newest merge's commit or one created at or after that merge. Both times are the
// provider's (merged_at and the pipeline's created_at; the platform's own clock stands in
// only for a merge the provider gave no time for). A branch status without a time and
// without the merge commit never ships.
func shippable(tickets []store.WaveTicketMerge, main tools.BranchStatus) bool {
	var newest time.Time
	newestSHA := ""
	for _, t := range tickets {
		if t.Status != store.TicketMerged {
			return false
		}
		merged := t.UpdatedAt
		if t.MergedAt != nil {
			merged = *t.MergedAt
		}
		if merged.After(newest) {
			newest, newestSHA = merged, t.MergeSHA
		} else if merged.Equal(newest) && newestSHA == "" {
			newestSHA = t.MergeSHA
		}
	}
	if main.State != tools.BranchPassed {
		return false
	}
	if newestSHA != "" && main.SHA == newestSHA {
		return true
	}
	return !main.At.IsZero() && !main.At.Before(newest)
}

// escalateEnded escalates the tickets of the wave that cannot merge any more: a merge
// request closed without merging, or a released ticket without a merge request whose issue
// is no longer open (read issue by issue) (the edge cases of rule 12). It reports whether it escalated any. A
// tracker that does not answer is logged and leaves the issue check for the next pass.
func (s *Service) escalateEnded(ctx context.Context, plan *store.Plan, proj *store.Project, pc *project.Config, tickets []store.WaveTicketMerge) bool {
	escalated := false
	var noRequest []store.WaveTicketMerge
	for _, t := range tickets {
		switch t.Status {
		case store.TicketReleased, store.TicketBuilding, store.TicketInReview:
		default:
			continue
		}
		switch {
		case t.MRState == store.MergeRequestClosed:
			noun := fmt.Sprintf("merge request !%d", t.MRNumber)
			if proj.Provider == "github" {
				noun = fmt.Sprintf("pull request #%d", t.MRNumber)
			}
			escalated = s.escalate(ctx, t, "its "+noun+" was closed without being merged") || escalated
		case t.MRState == "" && t.Status == store.TicketReleased && t.IssueNumber != nil:
			// Only a released ticket is judged by its issue (rule 12): a building ticket has
			// a run, and its merge request is what ends it.
			noRequest = append(noRequest, t)
		}
	}
	if len(noRequest) == 0 {
		return escalated
	}
	prov := s.issues(pc)
	for _, t := range noRequest {
		// The issue itself is read, not the feature's list of open issues: a list leaves out
		// confidential issues and the pages it was not read to the end.
		st, err := prov.IssueState(ctx, *t.IssueNumber)
		switch {
		case tools.IssueGone(err) || (err == nil && st == tools.IssueStateClosed):
			escalated = s.escalate(ctx, t, "its issue was closed without a merged merge request") || escalated
		case err != nil:
			s.log().Warn("waves: read an issue failed; the next pass tries again", "plan_id", plan.ID, "ticket", t.TicketID, "error", err)
		}
	}
	return escalated
}

func (s *Service) escalate(ctx context.Context, t store.WaveTicketMerge, reason string) bool {
	runID := ""
	if t.RunID != nil {
		runID = *t.RunID
	}
	pt := t.PlanTicket
	ev, err := s.Plans.Escalate(ctx, &pt, "", reason, runID)
	if err != nil {
		s.log().Warn("waves: escalating a ticket failed", "plan_id", t.PlanID, "ticket", t.TicketID, "error", err)
		return false
	}
	return ev != nil
}

// startShip starts attempt k at the wave's ship run (rules 12 and 13) and moves the plan to shipping. The
// unique index on the run's trigger_ref settles two watchers starting it together: the loser
// finds the run and records it.
func (s *Service) startShip(ctx context.Context, plan *store.Plan, proj *store.Project, wave, attempt int, tickets []store.WaveTicketMerge, mainURL string) error {
	mergeRequests := []map[string]string{}
	ids := []string{}
	for _, t := range tickets {
		if t.MRState == store.MergeRequestMerged {
			mergeRequests = append(mergeRequests, map[string]string{"title": t.MRTitle, "url": t.MRURL})
		}
		ids = append(ids, t.TicketID)
	}
	wf, err := store.GetWorkflowByFromTemplate(ctx, s.Pool, ShipTemplateKey)
	if errors.Is(err, store.ErrNotFound) {
		wf, err = store.GetWorkflowByTemplateKey(ctx, s.Pool, ShipTemplateKey)
	}
	if errors.Is(err, store.ErrNotFound) {
		return errors.New("no ship_pipeline workflow")
	}
	if err != nil {
		return err
	}
	all, err := store.ListPlanTickets(ctx, s.Pool, plan.ID)
	if err != nil {
		return err
	}
	last := wave
	for _, t := range all {
		last = max(last, t.Wave)
	}
	// The wave's tickets name their files, whose notes for the plan documents the ship run
	// applies (rule 13, D-089).
	in := map[string]any{"plan_id": plan.ID, "feature": plan.Feature, "wave": wave, "last_wave": last,
		"merge_requests": mergeRequests, "tickets": ids}
	if mainURL != "" {
		in["main_url"] = mainURL
	}
	input, _ := json.Marshal(in)
	ref := store.ShipAttemptRef(plan.ID, wave, attempt)
	run, err := s.Engine.Trigger(ctx, engine.TriggerRequest{WorkflowID: wf.ID, ProjectID: proj.ID, Trigger: store.TriggerPlan,
		TriggerRef: ref, Input: input})
	if errors.Is(err, engine.ErrDuplicateTrigger) || errors.Is(err, store.ErrDuplicateTrigger) {
		run, err = store.GetRunByTrigger(ctx, s.Pool, store.TriggerPlan, ref)
	}
	if err != nil {
		return fmt.Errorf("start the ship run: %w", err)
	}
	s.log().Info("waves: ship run started", "plan_id", plan.ID, "wave", wave, "run_id", run.ID)
	return s.recordShipping(ctx, plan, wave, run.ID)
}

// recordShipping sets the plan shipping and records `smoke_test_waiting` with the run id,
// once per ship run.
func (s *Service) recordShipping(ctx context.Context, plan *store.Plan, wave int, runID string) error {
	raw, _ := json.Marshal(map[string]any{"run_id": runID})
	w := wave
	ev := &store.PlanEvent{PlanID: plan.ID, Kind: store.PlanEventSmokeTestWaiting, Wave: &w, Detail: raw}
	moved := false
	err := s.inTx(ctx, func(q store.Querier) error {
		var err error
		if moved, err = store.MovePlan(ctx, q, plan.ID, store.PlanShipping, wave, store.PlanBuilding); err != nil || !moved {
			return err
		}
		return store.AddPlanEvent(ctx, q, ev)
	})
	if err != nil || !moved {
		return err
	}
	s.Plans.Publish(plan.ID, plan.ProjectID)
	s.Plans.NotifyEvent(ctx, plan, ev)
	return nil
}

// Advance is tools.AdvanceWaveFunc (rule 14). It runs for the ship run runID: it closes the
// wave's issues, sets its tickets closed and records `wave_done`, then releases the next
// wave or, after the last, sets the plan accepting and records `accept_waiting`. While a
// revision of the plan runs the next wave is not released: the plan moves to it, `wave_done`
// names it as held_wave, and the watcher releases it once the revision ended. It refuses
// when the run is not a ship run of an open plan that is shipping this wave, and while the
// project's waves are paused. A second call for the same plan and wave returns the first
// result and changes nothing: `wave_done` is the record. The tracker is written first and
// the database last, every tracker step being safe to repeat, so a failed call is retried whole.
// The pause is checked again in the transaction, before the tracker is written, and the
// project's row is held until the commit.
func (s *Service) Advance(ctx context.Context, runID string) (tools.AdvanceResult, error) {
	var zero tools.AdvanceResult
	run, err := store.GetRun(ctx, s.Pool, runID)
	if err != nil {
		return zero, fmt.Errorf("read the run: %w", err)
	}
	planID, wave, ok := "", 0, false
	if run.Trigger == store.TriggerPlan && run.TriggerRef != nil {
		planID, wave, ok = store.ParseShipRef(*run.TriggerRef)
	}
	if !ok {
		return zero, &tools.AdvanceRefused{Reason: "this run is not a ship run; only a ship run advances a wave"}
	}
	plan, err := store.GetPlan(ctx, s.Pool, planID)
	if errors.Is(err, store.ErrNotFound) {
		return zero, &tools.AdvanceRefused{Reason: "the plan of this ship run no longer exists"}
	}
	if err != nil {
		return zero, err
	}
	if res, done, err := s.advanced(ctx, s.Pool, planID, wave); err != nil || done {
		return res, err
	}
	if plan.Status == store.PlanDone || plan.Status == store.PlanCancelled {
		return zero, &tools.AdvanceRefused{Reason: "the plan is " + plan.Status}
	}
	if wave != plan.CurrentWave || plan.Status != store.PlanShipping {
		return zero, &tools.AdvanceRefused{Reason: fmt.Sprintf("wave %d is not the wave being shipped (the plan is %s at wave %d)", wave, plan.Status, plan.CurrentWave)}
	}
	proj, err := store.GetProject(ctx, s.Pool, plan.ProjectID)
	if err != nil {
		return zero, err
	}
	if proj.WavesPaused {
		why := ""
		if proj.WavesPausedReason != nil {
			why = *proj.WavesPausedReason
		}
		return zero, &tools.AdvanceRefused{Reason: pausedReason(why)}
	}
	all, err := store.ListPlanTickets(ctx, s.Pool, planID)
	if err != nil {
		return zero, err
	}
	next := 0
	for _, t := range all {
		if t.Wave > wave && (next == 0 || t.Wave < next) {
			next = t.Wave
		}
	}
	pc, err := s.config(ctx, proj)
	if err != nil {
		return zero, fmt.Errorf("read the project's access: %w", err)
	}
	prov := s.issues(pc)
	var held map[string]bool
	if next > 0 {
		if held, err = s.held(ctx, planID, next); err != nil {
			return zero, err
		}
	}

	var res tools.AdvanceResult
	var done, accept *store.PlanEvent
	replay := false
	err = s.inTx(ctx, func(q store.Querier) error {
		cur, err := store.LockPlan(ctx, q, planID)
		if err != nil {
			return err
		}
		if r, ok, err := s.advanced(ctx, q, planID, wave); err != nil || ok {
			res, replay = r, ok
			return err
		}
		if cur.Status != store.PlanShipping || cur.CurrentWave != wave {
			return &tools.AdvanceRefused{Reason: fmt.Sprintf("wave %d is not the wave being shipped (the plan is %s at wave %d)", wave, cur.Status, cur.CurrentWave)}
		}
		// The pause once more, under the plan lock, with the project's row held to the commit;
		// then the tracker, then the database. A tracker failure rolls everything back.
		if paused, reason, err := pausedIn(ctx, q, cur.ProjectID); err != nil {
			return err
		} else if paused {
			return &tools.AdvanceRefused{Reason: pausedReason(reason)}
		}
		// A ticket the wave gained since its smoke test began (a revision) is not shipped.
		fresh, err := store.ListWaveTickets(ctx, q, planID, wave)
		if err != nil {
			return err
		}
		for _, t := range fresh {
			if t.Status != store.TicketMerged && t.Status != store.TicketClosed {
				return &tools.AdvanceRefused{Reason: fmt.Sprintf("ticket %s of wave %d is %s, not merged; the wave cannot be closed yet", t.TicketID, wave, t.Status)}
			}
		}
		// A revision of the plan that is running holds the next wave back: the wave closes,
		// and the watcher releases the next one once the revision ended (D-089).
		revisionHeld := false
		if next > 0 {
			if revisionHeld, err = s.revising(ctx, q, cur); err != nil {
				return err
			}
		}
		for _, t := range all {
			if t.Wave != wave || t.IssueNumber == nil {
				continue
			}
			if err := prov.Close(ctx, *t.IssueNumber); err != nil {
				return fmt.Errorf("close the issue of ticket %s: %w", t.TicketID, err)
			}
		}
		if next > 0 && !revisionHeld {
			if err := s.labelWave(ctx, prov, proj.IntakeLabel, planID, next, held); err != nil {
				return err
			}
		}
		res = tools.AdvanceResult{PlanID: planID, Wave: wave, Last: next == 0}
		if revisionHeld {
			res.HeldWave = next
		}
		tickets, err := store.ListWaveTickets(ctx, q, planID, wave)
		if err != nil {
			return err
		}
		for _, t := range tickets {
			moved, err := store.MoveTicket(ctx, q, planID, t.TicketID, store.TicketClosed,
				store.TicketPending, store.TicketReleased, store.TicketBuilding, store.TicketInReview, store.TicketMerged, store.TicketFailed)
			if err != nil {
				return err
			}
			if moved {
				res.Closed++
			}
		}
		if next > 0 {
			if _, err := store.MovePlan(ctx, q, planID, store.PlanBuilding, next, store.PlanShipping); err != nil {
				return err
			}
			if !revisionHeld {
				if _, err := releaseTx(ctx, q, planID, next, held); err != nil {
					return err
				}
				nt, err := store.ListWaveTickets(ctx, q, planID, next)
				if err != nil {
					return err
				}
				for _, t := range nt {
					if t.Status == store.TicketReleased {
						res.Released++
					}
				}
			}
		} else if _, err := store.MovePlan(ctx, q, planID, store.PlanAccepting, wave, store.PlanShipping); err != nil {
			return err
		}
		detail := map[string]any{"plan_id": res.PlanID, "wave": res.Wave, "closed": res.Closed,
			"released": res.Released, "last": res.Last, "run_id": runID}
		if res.HeldWave > 0 {
			detail["held_wave"] = res.HeldWave
		}
		raw, _ := json.Marshal(detail)
		w := wave
		done = &store.PlanEvent{PlanID: planID, Kind: store.PlanEventWaveDone, Wave: &w, Detail: raw}
		if err := store.AddPlanEvent(ctx, q, done); err != nil {
			return err
		}
		if res.Last {
			rawAccept, _ := json.Marshal(map[string]any{"run_id": runID})
			accept = &store.PlanEvent{PlanID: planID, Kind: store.PlanEventAcceptWaiting, Wave: &w, Detail: rawAccept}
			return store.AddPlanEvent(ctx, q, accept)
		}
		return nil
	})
	if err != nil || replay {
		return res, err
	}
	s.Plans.Publish(plan.ID, plan.ProjectID)
	s.Plans.NotifyEvent(ctx, plan, done)
	if accept != nil {
		s.Plans.NotifyEvent(ctx, plan, accept)
	}
	return res, nil
}

func pausedReason(reason string) string {
	why := "the project's waves are paused"
	if reason != "" {
		why += ": " + reason
	}
	return why
}

// advanced returns the recorded result of a wave already advanced (its `wave_done` event).
func (s *Service) advanced(ctx context.Context, q store.Querier, planID string, wave int) (res tools.AdvanceResult, done bool, err error) {
	ev, err := store.FindPlanEvent(ctx, q, planID, store.PlanEventWaveDone, wave, "")
	if errors.Is(err, store.ErrNotFound) {
		return res, false, nil
	}
	if err != nil {
		return res, false, err
	}
	if err := json.Unmarshal(ev.Detail, &res); err != nil {
		return res, false, fmt.Errorf("read the record of the advanced wave: %w", err)
	}
	return res, true, nil
}
