package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"yuno/internal/project"
	"yuno/internal/store"
)

// Revising a plan (DESIGN 5.29 rule 16): publish_tickets in a run whose input names
// revise_plan amends that plan. New tickets get new issues, changed tickets update theirs
// (found by marker), tickets dropped from the plan have their issue closed and are set
// closed, and the plan resumes at its current wave.

// droppedComment is posted on the issue of a ticket the revision dropped.
const droppedComment = "This ticket was dropped when the plan was revised."

// currentWaveOf is the lowest wave with a ticket not closed; when every ticket is closed it
// is the wave after the last, so that nothing counts as unfinished.
func currentWaveOf(rows []store.PlanTicket) int {
	cur, last := 0, 0
	for _, t := range rows {
		if t.Wave > last {
			last = t.Wave
		}
		if t.Status != store.TicketClosed && (cur == 0 || t.Wave < cur) {
			cur = t.Wave
		}
	}
	if cur == 0 {
		return last + 1
	}
	return cur
}

// settled is a ticket a revision leaves as it is: its work is merged or its wave is closed.
func settled(t store.PlanTicket) bool {
	return t.Status == store.TicketMerged || t.Status == store.TicketClosed
}

// reviseDescription is the approval sentence of a revision.
func reviseDescription(ctx context.Context, db store.Querier, plan *publishPlan, parts []string) string {
	text := fmt.Sprintf("Revise the plan of feature %q: %d tickets (%s). New tickets get issues, changed tickets update theirs, "+
		"tickets left out are closed", plan.feature, plan.count, strings.Join(parts, ", "))
	if rows, err := store.ListPlanTickets(ctx, db, plan.revise); err == nil && len(rows) > 0 {
		cur := currentWaveOf(rows)
		if cur > 1 {
			text += fmt.Sprintf(". Waves before wave %d are finished: their tickets stay as they are", cur)
		}
		if hashes, err := store.TicketContentHashes(ctx, db, plan.revise); err == nil {
			ids := inFlightChanges(rows, hashes, plan)
			if fus, _, err := followUpsOf(ctx, db, plan.revise, rows, hashes, plan); err == nil {
				if lines := followUpSentences(fus); len(lines) > 0 {
					text += ". " + strings.Join(lines, ". ")
				}
				ids = append(ids, followUpsInFlight(rows, fus)...)
				sort.Strings(ids)
			}
			if len(ids) > 0 {
				text += fmt.Sprintf(". The text of %s changes while it is being built or in review: the work in progress "+
					"keeps the old text, so request changes on its merge request after approving this", strings.Join(ids, ", "))
			}
		}
	}
	if stored, err := store.GetPlan(ctx, db, plan.revise); err == nil {
		if proj, err := store.GetProject(ctx, db, stored.ProjectID); err == nil && proj.WavesPaused {
			why := "no reason given"
			if proj.WavesPausedReason != nil && strings.TrimSpace(*proj.WavesPausedReason) != "" {
				why = strings.TrimSpace(*proj.WavesPausedReason)
			}
			text += fmt.Sprintf(". It also resumes the project's paused waves (paused: %s)", why)
		}
	}
	return text
}

// inFlightChanges lists the tickets that are building or in review whose text the revision
// changes (rule 16): by the recorded content hash, or by title when none was recorded.
func inFlightChanges(rows []store.PlanTicket, hashes map[string]string, plan *publishPlan) []string {
	prev := map[string]store.PlanTicket{}
	for _, t := range rows {
		prev[t.TicketID] = t
	}
	var ids []string
	for _, wave := range plan.waves {
		for _, t := range wave {
			p, ok := prev[t.ID]
			if !ok || (p.Status != store.TicketBuilding && p.Status != store.TicketInReview) {
				continue
			}
			if textChanged(p, hashes, plan.feature, t) {
				ids = append(ids, t.ID)
			}
		}
	}
	sort.Strings(ids)
	return ids
}

// followUpsInFlight lists the open follow-ups that are building or in review and take a
// further change from the revision.
func followUpsInFlight(rows []store.PlanTicket, fus []followUp) []string {
	status := map[string]string{}
	for _, t := range rows {
		status[t.TicketID] = t.Status
	}
	var ids []string
	for _, fu := range fus {
		if fu.reused && (status[fu.id] == store.TicketBuilding || status[fu.id] == store.TicketInReview) {
			ids = append(ids, fu.id)
		}
	}
	return ids
}

// followUpsOf reads the plan's follow-ups and its current wave's ship run, and lists the
// follow-ups of the revision (planFollowUps). shipping is the current wave's ship run while
// it has not ended, nil otherwise.
func followUpsOf(ctx context.Context, q store.Querier, planID string, rows []store.PlanTicket, hashes map[string]string, plan *publishPlan) (fus []followUp, shipping *store.Run, err error) {
	links, err := store.ListFollowUps(ctx, q, planID)
	if err != nil {
		return nil, nil, err
	}
	run, _, err := store.LatestShipRun(ctx, q, planID, currentWaveOf(rows))
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return nil, nil, err
	case !store.IsTerminal(run.Status):
		shipping = run
	}
	fus, err = planFollowUps(rows, hashes, links, plan, shipping != nil)
	return fus, shipping, err
}

func (p *publishTickets) revise(ctx context.Context, q store.Querier, run *store.Run, pc *project.Config, plan *publishPlan) (Result, error) {
	refuse := func(format string, args ...any) (Result, error) {
		return Result{}, &ModelError{Message: "publish_tickets: " + fmt.Sprintf(format, args...)}
	}
	stored, err := store.LockPlan(ctx, q, plan.revise)
	if errors.Is(err, store.ErrNotFound) {
		return refuse("there is no plan %s to revise", plan.revise)
	}
	if err != nil {
		return Result{}, err
	}
	switch {
	case stored.ProjectID != *run.ProjectID:
		return refuse("plan %s belongs to another project", stored.ID)
	case stored.Feature != plan.feature:
		return refuse("plan %s is for feature %q, not %q", stored.ID, stored.Feature, plan.feature)
	case stored.Status == store.PlanDone || stored.Status == store.PlanCancelled:
		return refuse("plan %s is %s and cannot be revised", stored.ID, stored.Status)
	}
	planID := stored.ID
	rows, err := store.ListPlanTickets(ctx, q, planID)
	if err != nil {
		return Result{}, err
	}
	existing := map[string]store.PlanTicket{}
	for _, t := range rows {
		existing[t.TicketID] = t
	}
	cur := currentWaveOf(rows)
	hashes, err := store.TicketContentHashes(ctx, q, planID)
	if err != nil {
		return Result{}, err
	}
	// A change to a merged or closed ticket becomes a follow-up ticket; the ticket and its
	// issue stay as they are.
	fus, shipping, err := followUpsOf(ctx, q, planID, rows, hashes, plan)
	if err != nil {
		return Result{}, err
	}
	amended := withFollowUps(plan, fus)
	if amended.count > MaxPlanTickets {
		return refuse("with its follow-up tickets the plan has %d tickets; at most %d", amended.count, MaxPlanTickets)
	}
	revised := map[string]bool{}
	touchesCurrent, anyOpen := false, false
	for i, wave := range amended.waves {
		for _, t := range wave {
			revised[t.ID] = true
			prev, ok := existing[t.ID]
			if (!ok || !settled(prev)) && i+1 < cur {
				return refuse("ticket %s is placed in wave %d, which is finished; the current wave is %d", t.ID, i+1, cur)
			}
			if !ok || prev.Status != store.TicketClosed {
				anyOpen = true
			}
			if (!ok || !settled(prev)) && i+1 == cur {
				touchesCurrent = true
			}
			// A ticket with a run is not moved to a later wave: its work would start before
			// the wave it now belongs to (review M5).
			if ok && i+1 > prev.Wave && (prev.Status == store.TicketBuilding || prev.Status == store.TicketInReview) {
				return refuse("ticket %s is %s in wave %d and cannot be moved to wave %d; drop it or leave it in its wave",
					t.ID, strings.ReplaceAll(prev.Status, "_", " "), prev.Wave, i+1)
			}
		}
	}
	// A revision that leaves no ticket to build is refused (review L3): cancel the plan.
	if !anyOpen {
		return refuse("the revised plan leaves no ticket to build; cancel plan %s instead", stored.ID)
	}
	// The wave being shipped is not changed under its ship run (review H1): its advance would
	// close the new ticket's issue before it is built.
	if touchesCurrent && shipping != nil {
		return refuse("wave %d is being shipped (run %s); revise it when that run has ended", cur, shipping.ID)
	}

	base, path := p.cfg.APIBase, p.cfg.RepoPath
	if base == "" {
		base = pc.APIBase()
	}
	if path == "" {
		path = pc.Path()
	}
	issues := newIssueProvider(pc, p.cfg.Client, base, path)
	proj, err := store.GetProject(ctx, q, *run.ProjectID)
	if err != nil {
		return Result{}, err
	}

	// The issues keep the plan's first merge request link, so a revision's own request does
	// not rewrite every open ticket (review L5).
	planMRURL := ""
	if stored.PlanMRID != nil {
		if mr, err := store.GetMergeRequest(ctx, q, *stored.PlanMRID); err == nil {
			planMRURL = mr.URL
		}
	}
	if planMRURL == "" {
		if mr, err := store.MergeRequestByRun(ctx, q, run.ID); err != nil {
			return Result{}, err
		} else if mr != nil {
			planMRURL = mr.URL
		}
	}
	docs := planDocLinks(pc, pc.DefaultBranch, plan.feature)

	label := "feature:" + plan.feature
	var byMarker map[string]Issue
	lookup := func() (map[string]Issue, *Result) {
		if byMarker != nil {
			return byMarker, nil
		}
		all, err := issues.ListAll(ctx, label, issueScanCap)
		if err != nil {
			return nil, &Result{IsError: true, Text: fmt.Sprintf("publish_tickets: look up the issues of %s: %s", label, err)}
		}
		byMarker = map[string]Issue{}
		for _, is := range all {
			if m := lastLine(is.Body); strings.HasPrefix(m, "<!-- yuno-ticket:") {
				if _, dup := byMarker[m]; !dup {
					byMarker[m] = is
				}
			}
		}
		return byMarker, nil
	}
	editor, _ := issues.(IssueEditor)

	out := publishTicketsOutput{PlanID: planID, Issues: []publishedTicketOut{}, Revised: true}
	out.ChangedInFlight = inFlightChanges(rows, hashes, plan)
	if more := followUpsInFlight(rows, fus); len(more) > 0 {
		out.ChangedInFlight = append(out.ChangedInFlight, more...)
		sort.Strings(out.ChangedInFlight)
	}
	followUps := map[string]followUp{}
	for _, fu := range fus {
		followUps[fu.id] = fu
	}
	var reset []string
	// Tickets the current wave gained: added, moved into it or reset (review L6).
	var gained []int
	recorded := 0
	for i, wave := range amended.waves {
		n := i + 1
		for _, t := range wave {
			prev, had := existing[t.ID]
			if had && settled(prev) {
				out.Reused++
				if prev.IssueNumber != nil {
					out.Issues = append(out.Issues, publishedTicketOut{Ticket: t.ID, Number: *prev.IssueNumber, URL: prev.IssueURL})
				}
				continue
			}
			fu, isFollowUp := followUps[t.ID]
			if isFollowUp && !fu.planner {
				// The follow-up's text says what changed against the old issue, which the
				// revision leaves as it is.
				found, res := lookup()
				if res != nil {
					return *res, nil
				}
				var mr *store.MergeRequest
				if fu.of.MergeRequestID != nil {
					if mr, err = store.GetMergeRequest(ctx, q, *fu.of.MergeRequestID); errors.Is(err, store.ErrNotFound) {
						mr = nil
					} else if err != nil {
						return Result{}, err
					}
				}
				t = fu.ticket(found[ticketMarker(planID, fu.of.TicketID)].Body, mr)
			}
			marker := ticketMarker(planID, t.ID)
			title, body := ticketIssueTitle(plan.feature, t), ticketIssueBody(plan.feature, t, docs, planMRURL, marker)
			row := store.PlanTicket{PlanID: planID, TicketID: t.ID, Wave: n, Title: t.Title}
			if had && prev.IssueNumber != nil {
				row.IssueNumber, row.IssueURL = prev.IssueNumber, prev.IssueURL
				found, res := lookup()
				if res != nil {
					return *res, nil
				}
				is, listed := found[marker]
				if !listed || strings.TrimSpace(is.Title) != title || strings.TrimSpace(is.Body) != strings.TrimSpace(body) || prev.Wave != n {
					if editor == nil {
						return Result{IsError: true, Text: "publish_tickets: the provider cannot update issues"}, nil
					}
					if err := editor.Update(ctx, *prev.IssueNumber, title, body); err != nil {
						return Result{IsError: true, Text: fmt.Sprintf("publish_tickets: update the issue of ticket %s: %s", t.ID, err)}, nil
					}
					if prev.Wave != n {
						add := fmt.Sprintf("wave:%d", n)
						if err := issues.EnsureLabels(ctx, ticketLabels([]string{add})); err != nil {
							return Result{IsError: true, Text: fmt.Sprintf("publish_tickets: label the issue of ticket %s: %s", t.ID, err)}, nil
						}
						if err := issues.SwapLabels(ctx, *prev.IssueNumber, add, fmt.Sprintf("wave:%d", prev.Wave)); err != nil {
							return Result{IsError: true, Text: fmt.Sprintf("publish_tickets: label the issue of ticket %s: %s", t.ID, err)}, nil
						}
						gained = append(gained, n)
						// A ticket moved to a later wave is not built before it (review M5): the
						// intake label comes off and it waits for its wave again.
						if n > prev.Wave && (prev.Status == store.TicketReleased || prev.Status == store.TicketFailed) {
							off := proj.IntakeLabel + "," + proj.IntakeLabel + IssueFailedSuffix + "," + proj.IntakeLabel + IssueRunningSuffix
							if err := issues.SwapLabels(ctx, *prev.IssueNumber, "", off); err != nil {
								return Result{IsError: true, Text: fmt.Sprintf("publish_tickets: label the issue of ticket %s: %s", t.ID, err)}, nil
							}
							var err error
							if prev.Status == store.TicketReleased {
								_, err = store.MoveTicket(ctx, q, planID, t.ID, store.TicketPending, store.TicketReleased)
							} else {
								_, err = store.ResetFailedTicket(ctx, q, planID, t.ID)
							}
							if err != nil {
								return Result{}, err
							}
						}
					}
					out.Updated++
				}
			}
			if err := store.UpsertPlanTicket(ctx, q, &row); err != nil {
				return Result{}, err
			}
			if row.IssueNumber == nil {
				found, res := lookup()
				if res != nil {
					return *res, nil
				}
				var issue Issue
				if is, ok := found[marker]; ok {
					issue = is
					out.Reused++
				} else {
					issue, err = issues.Create(ctx, title, body, []string{fmt.Sprintf("wave:%d", n), label})
					if err != nil {
						return Result{IsError: true, Text: fmt.Sprintf("publish_tickets: create the issue of ticket %s: %s", t.ID, err)}, nil
					}
				}
				if err := store.SetTicketIssue(ctx, q, planID, t.ID, issue.Number, issue.URL); err != nil {
					return Result{}, err
				}
				row.IssueNumber, row.IssueURL = &issue.Number, issue.URL
				out.Added++
				gained = append(gained, n)
			}
			if err := store.SetTicketContentHash(ctx, q, planID, t.ID, ticketContentHash(plan.feature, t)); err != nil {
				return Result{}, err
			}
			out.Issues = append(out.Issues, publishedTicketOut{Ticket: t.ID, Number: *row.IssueNumber, URL: row.IssueURL})
			if isFollowUp {
				if fu.record {
					w := n
					raw, _ := json.Marshal(store.FollowUp{Follows: fu.of.TicketID, Hash: fu.hash})
					if err := store.AddPlanEvent(ctx, q, &store.PlanEvent{PlanID: planID, Kind: store.PlanEventFollowUp, Wave: &w,
						TicketID: &fu.id, Detail: raw}); err != nil {
						return Result{}, err
					}
					recorded++
				}
				if !fu.planner {
					out.FollowUps = append(out.FollowUps, followUpOut{Ticket: fu.id, Follows: fu.of.TicketID, Number: *row.IssueNumber, URL: row.IssueURL})
				}
			}
			if had && prev.Status == store.TicketFailed && prev.Wave == cur {
				if ok, err := store.ResetFailedTicket(ctx, q, planID, t.ID); err != nil {
					return Result{}, err
				} else if ok {
					reset = append(reset, t.ID)
					out.Reset++
				}
			}
		}
	}

	// Tickets left out of the revised plan are closed, unless their work is merged.
	var dropped []string
	for _, t := range rows {
		if revised[t.TicketID] || settled(t) {
			continue
		}
		dropped = append(dropped, t.TicketID)
	}
	sort.Strings(dropped)
	for _, id := range dropped {
		t := existing[id]
		if t.IssueNumber != nil {
			state, err := issues.IssueState(ctx, *t.IssueNumber)
			switch {
			case IssueGone(err):
			case err != nil:
				return Result{IsError: true, Text: fmt.Sprintf("publish_tickets: read the issue of ticket %s: %s", id, err)}, nil
			case state == IssueStateOpen:
				if err := issues.Comment(ctx, *t.IssueNumber, droppedComment); err != nil {
					return Result{IsError: true, Text: fmt.Sprintf("publish_tickets: comment on the issue of ticket %s: %s", id, err)}, nil
				}
				if err := issues.Close(ctx, *t.IssueNumber); err != nil {
					return Result{IsError: true, Text: fmt.Sprintf("publish_tickets: close the issue of ticket %s: %s", id, err)}, nil
				}
			}
		}
		if err := store.SetTicketStatus(ctx, q, planID, id, store.TicketClosed); err != nil {
			return Result{}, err
		}
		out.Dropped++
	}

	// The Planner's edges are the revised plan's (Chief platform control, DESIGN wave 3 rules 2
	// and 7: a revision is how a Planner edge is removed).
	if err := store.ReplacePlannerDependencies(ctx, q, stored.ProjectID, planID, plan.edges); err != nil {
		return Result{}, err
	}

	// The plan resumes at its current wave: unpaused, and the wave re-released when it was
	// released before.
	rows, err = store.ListPlanTickets(ctx, q, planID)
	if err != nil {
		return Result{}, err
	}
	newCur := currentWaveOf(rows)
	open := false
	for _, t := range rows {
		open = open || t.Status != store.TicketClosed
	}
	if pause, err := store.LockProjectPause(ctx, q, stored.ProjectID); err != nil {
		return Result{}, err
	} else if pause.WavesPaused {
		if err := store.SetWavesPaused(ctx, q, stored.ProjectID, false, ""); err != nil {
			return Result{}, err
		}
		if err := store.AddPlanEvent(ctx, q, &store.PlanEvent{PlanID: planID, Kind: store.PlanEventResumed,
			Detail: json.RawMessage(`{"reason":"plan revised"}`)}); err != nil {
			return Result{}, err
		}
	}
	status := stored.Status
	if open {
		switch {
		case (status == store.PlanShipping || status == store.PlanAccepting) &&
			(newCur != stored.CurrentWave || len(reset) > 0 || slices.Contains(gained, newCur)):
			if moved, err := store.MovePlan(ctx, q, planID, store.PlanBuilding, newCur, store.PlanShipping, store.PlanAccepting); err != nil {
				return Result{}, err
			} else if moved {
				status = store.PlanBuilding
			}
		case status == store.PlanBuilding && newCur != stored.CurrentWave:
			if err := store.SetCurrentWave(ctx, q, planID, newCur); err != nil {
				return Result{}, err
			}
		}
	}
	if open && status == store.PlanBuilding {
		_, evErr := store.FindPlanEvent(ctx, q, planID, store.PlanEventWaveReleased, newCur, "")
		if evErr != nil && !errors.Is(evErr, store.ErrNotFound) {
			return Result{}, evErr
		}
		if evErr == nil || newCur != stored.CurrentWave {
			n, err := p.rerelease(ctx, q, issues, proj.IntakeLabel, planID, newCur, rows, evErr != nil, reset)
			if err != nil {
				return Result{IsError: true, Text: err.Error()}, nil
			}
			out.Released = n
		}
	}

	detail, _ := json.Marshal(map[string]any{"tickets": amended.count, "waves": len(amended.waves), "added": out.Added, "updated": out.Updated,
		"dropped": out.Dropped, "reset": out.Reset, "released": out.Released, "current_wave": newCur, "follow_ups": recorded})
	if err := store.AddPlanEvent(ctx, q, &store.PlanEvent{PlanID: planID, Kind: store.PlanEventRevised, Detail: detail}); err != nil {
		return Result{}, err
	}
	return JSONResult(out), nil
}

// rerelease labels the pending tickets of the wave with the intake label and sets them
// released (rule 16); a label of an earlier attempt (failed, running) comes off. When the
// wave had never been released it also records `wave_released`. A ticket with a
// prerequisite the watcher has not found satisfied stays pending, and the wave watcher
// releases it once it is (Chief platform control, DESIGN wave 3 rule 6).
func (p *publishTickets) rerelease(ctx context.Context, q store.Querier, issues IssueProvider, label, planID string, wave int, rows []store.PlanTicket, first bool, reset []string) (int, error) {
	held, err := store.HeldTickets(ctx, q, planID)
	if err != nil {
		return 0, err
	}
	n := 0
	var released []string
	for _, t := range rows {
		if t.Wave != wave || t.Status != store.TicketPending || held[t.TicketID] {
			continue
		}
		released = append(released, t.TicketID)
		if t.IssueNumber == nil {
			return 0, fmt.Errorf("publish_tickets: ticket %s has no issue to release", t.TicketID)
		}
		// A ticket reset after its issue was closed (the reason it failed) gets the issue
		// back open, or intake would never take it (review M2).
		if slices.Contains(reset, t.TicketID) {
			if err := reopenIssue(ctx, issues, *t.IssueNumber); err != nil {
				return 0, fmt.Errorf("publish_tickets: reopen the issue of ticket %s: %s", t.TicketID, err)
			}
		}
		if err := issues.SwapLabels(ctx, *t.IssueNumber, label, label+IssueFailedSuffix+","+label+IssueRunningSuffix); err != nil {
			return 0, fmt.Errorf("publish_tickets: label the issue of ticket %s: %s", t.TicketID, err)
		}
		if moved, err := store.MoveTicket(ctx, q, planID, t.TicketID, store.TicketReleased, store.TicketPending); err != nil {
			return 0, err
		} else if moved {
			n++
		}
	}
	if err := store.ClearTicketDependencies(ctx, q, planID, released, time.Now()); err != nil {
		return 0, err
	}
	if first {
		w := wave
		raw, _ := json.Marshal(map[string]any{"tickets": n})
		if err := store.AddPlanEvent(ctx, q, &store.PlanEvent{PlanID: planID, Kind: store.PlanEventWaveReleased, Wave: &w, Detail: raw}); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// reopenIssue opens the issue again when it is closed.
func reopenIssue(ctx context.Context, issues IssueProvider, number int64) error {
	state, err := issues.IssueState(ctx, number)
	if err != nil {
		return err
	}
	if state != IssueStateClosed {
		return nil
	}
	editor, ok := issues.(IssueEditor)
	if !ok {
		return errors.New("the provider cannot reopen issues")
	}
	return editor.Reopen(ctx, number)
}
