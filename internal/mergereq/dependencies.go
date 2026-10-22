package mergereq

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"yuno/internal/deps"
	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// The dependency watcher (Chief platform control, DESIGN wave 3 rules 8 and 12): an open
// merge request held by prerequisites waits until they are all satisfied, is then brought
// up to date with the default branch by the platform, and is let go once CI passed on
// exactly its new head. No agent polls.

const (
	// EventUnblocked is the event a dependent request the watcher let go emits, under the
	// request's run; its data is {"merge_request": record, "note": task note}.
	EventUnblocked = "merge_request.unblocked"
	// DependencyTimeout bounds the watcher's work on one dependent: its prerequisites' CI
	// and, once they are satisfied, the update from the default branch (a fetch and a push).
	DependencyTimeout = 2 * time.Minute
)

// TicketReleaser is the wave watcher's part of a dependency pass (rule 8b): the held tickets
// of released waves whose prerequisites are now satisfied are labelled for intake.
type TicketReleaser interface {
	ReleaseSatisfied(ctx context.Context) error
}

// Gate is the judge of the prerequisites, reading merge commits' CI through each record's
// project, as the sync reads the records.
func (s *Sync) Gate() *deps.Gate {
	return &deps.Gate{Pool: s.Pool, Now: s.now, Provider: func(ctx context.Context, mr *store.MergeRequest) (*project.Config, tools.RequestProvider, error) {
		pc, err := s.Projects.ForRunID(ctx, mr.RunID)
		if err != nil {
			return nil, nil, err
		}
		return pc, tools.ProviderFor(pc, s.Client), nil
	}}
}

// WatchDependencies is one pass of the dependency watcher (rule 8): every open merge request
// an edge holds, the ones looked at longest ago first, at most PassLimit, each under its own
// DependencyTimeout; then the held tickets (Tickets). One dependent failing does not stop
// the others; the next pass tries it again.
func (s *Sync) WatchDependencies(ctx context.Context) {
	ids, err := store.BlockedMergeRequests(ctx, s.Pool, PassLimit)
	if err != nil {
		s.logDB(ctx, "list the merge requests held by dependencies", err)
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		s.watchOne(ctx, id)
	}
	if s.Tickets != nil && ctx.Err() == nil {
		if err := s.Tickets.ReleaseSatisfied(ctx); err != nil && ctx.Err() == nil {
			s.log().Warn("dependency watcher: releasing held tickets failed; the next pass tries again", "error", err)
		}
	}
}

// watchDependentsOf looks at once at the open merge requests waiting on the record mrID
// (rule 8: right after a sync that changed a prerequisite).
func (s *Sync) watchDependentsOf(ctx context.Context, mrID string) {
	ids, err := store.MergeRequestsWaitingOn(ctx, s.Pool, mrID)
	if err != nil {
		s.logDB(ctx, "list the dependents of a merge request", err, "merge_request_id", mrID)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		s.watchOne(ctx, id)
	}
}

func (s *Sync) watchOne(ctx context.Context, id string) {
	dctx, cancel := context.WithTimeout(ctx, DependencyTimeout)
	defer cancel()
	if err := s.gateRequest(dctx, id); err != nil && ctx.Err() == nil {
		s.log().Warn("dependency watcher: a dependent merge request could not be looked at; the next pass tries again",
			"merge_request_id", id, "error", err)
	}
}

// gateRequest is rule 8a for one dependent merge request, read fresh: while a prerequisite
// is not satisfied it waits (and the operator is told once of a prerequisite closed without
// merging); once all are, the branch is updated from the default branch with the record's
// head as the expected head (rules 9 to 11), and the request is let go when CI passed on
// exactly its head (rule 12). A failed CI or conflicts keep it held, and the operator is
// told once per commit or conflict. A provider error is recorded in last_sync_error; the
// error returned is a database one.
func (s *Sync) gateRequest(ctx context.Context, id string) error {
	rec, err := store.GetMergeRequest(ctx, s.Pool, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if rec.State != store.MergeRequestOpen {
		return nil // closed or merged meanwhile: its edges stay for history, nothing resumes
	}
	pc, err := s.Projects.ForRunID(ctx, rec.RunID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		_, _, err := s.recordFailure(ctx, rec, s.now(), err.Error(), "")
		return err
	}
	st, err := s.Gate().MergeRequest(ctx, id)
	if err != nil {
		return s.dependencyFailure(ctx, rec, pc, err)
	}
	if len(st.Open) == 0 {
		return store.DeleteDependencyGate(ctx, s.Pool, id)
	}
	g, err := store.GetDependencyGate(ctx, s.Pool, id)
	if err != nil {
		return err
	}
	if g == nil {
		g = &store.DependencyGate{MergeRequestID: id}
	}
	if !st.Satisfied {
		g.Reason = nil
		if len(st.Closed) > 0 {
			s.tell(ctx, rec, g, "closed:"+st.Closed[0].Prerequisite, st.Closed[0].Reason)
		}
		return store.SetDependencyGate(ctx, s.Pool, g, s.now())
	}
	if rec.HeadSHA == nil || *rec.HeadSHA == "" {
		return nil // the sync reads the head first
	}
	if g.HeadSHA == nil || *g.HeadSHA != *rec.HeadSHA {
		cur, res, err := s.UpdateFromDefault(ctx, id, *rec.HeadSHA)
		var moved *project.ErrHeadMoved
		var notOpen *ErrNotOpen
		switch {
		case errors.As(err, &moved):
			// The branch moved since the sync read it: the next pass starts from its new head.
			s.log().Info("dependency watcher: the branch moved before the update; the next pass tries again", "merge_request_id", id)
			return nil
		case errors.As(err, &notOpen), errors.Is(err, store.ErrNotFound):
			return nil
		case err != nil:
			return s.dependencyFailure(ctx, rec, pc, err)
		}
		switch res.Outcome {
		case project.UpdateConflicts:
			reason := fmt.Sprintf("conflicts with %s in %s", pc.DefaultBranch, strings.Join(res.Conflicts, ", "))
			outcome := res.Outcome
			g.HeadSHA, g.Outcome, g.Reason = nil, &outcome, &reason
			s.tell(ctx, rec, g, "conflicts:"+res.DefaultSHA+":"+strings.Join(res.Conflicts, ","), reason)
			return store.SetDependencyGate(ctx, s.Pool, g, s.now())
		default:
			outcome := res.Outcome
			g.HeadSHA, g.Outcome = cur.HeadSHA, &outcome
			rec = cur
		}
	}
	head := *rec.HeadSHA
	checked := rec.CISHA != nil && *rec.CISHA == head
	reason := "waiting for CI on " + deps.Short(head)
	switch {
	case checked && rec.CIStatus == store.CIStatusPassed:
		return s.letGo(ctx, rec, st, g, pc, false)
	case checked && rec.CIStatus == store.CIStatusFailed:
		reason = "CI failed on " + deps.Short(head)
		if rec.CIURL != nil && *rec.CIURL != "" {
			reason += ": " + *rec.CIURL
		}
		s.tell(ctx, rec, g, "ci_failed:"+head, reason)
	case checked && rec.CIStatus == store.CIStatusNone:
		// No pipeline on the head: enough only in a project with no CI configured (rule 4's
		// test, as the merge guard applies it).
		cctx, cancel := context.WithTimeout(ctx, s.timeout())
		configured, err := tools.ProviderFor(pc, s.Client).CIConfigured(cctx, pc.DefaultBranch)
		cancel()
		if err != nil {
			return s.dependencyFailure(ctx, rec, pc, &deps.ProviderError{Err: err})
		}
		if !configured {
			return s.letGo(ctx, rec, st, g, pc, true)
		}
	}
	g.Reason = &reason
	return store.SetDependencyGate(ctx, s.Pool, g, s.now())
}

// letGo clears the request's edges and forgets its gate in one transaction that holds the
// record's row, so two passes let it go once; then the paused dependency runs are re-queued
// with the task note (rule 7's hook) and merge_request.unblocked is emitted.
func (s *Sync) letGo(ctx context.Context, rec *store.MergeRequest, st deps.State, g *store.DependencyGate, pc *project.Config, noCI bool) error {
	ids := make([]string, len(st.Open))
	for i, e := range st.Open {
		ids[i] = e.ID
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT 1 FROM merge_requests WHERE id = $1 FOR UPDATE`, rec.ID); err != nil {
		return err
	}
	n, err := store.ClearDependencies(ctx, tx, ids, s.now())
	if err != nil {
		return err
	}
	if err := store.DeleteDependencyGate(ctx, tx, rec.ID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if n == 0 {
		return nil // another pass let it go
	}
	outcome := ""
	if g.Outcome != nil {
		outcome = *g.Outcome
	}
	note := ResumeNote(st.Labels, pc.DefaultBranch, outcome, *rec.HeadSHA, noCI)
	s.log().Info("dependency watcher: merge request let go", "merge_request_id", rec.ID, "run_id", rec.RunID, "head", *rec.HeadSHA)
	s.requeueDependencyRuns(ctx, rec, note)
	if s.Events != nil {
		if _, err := s.Events.Emit(ctx, rec.RunID, EventUnblocked, map[string]any{"merge_request": rec, "note": note}); err != nil {
			s.logDB(ctx, "emit merge_request.unblocked", err, "merge_request_id", rec.ID)
		}
	}
	return nil
}

// requeueDependencyRuns is the hook of rule 7: the runs parked on the request because of a
// dependency are re-queued with the note. Nothing is re-queued unless RequeueDependencyRuns
// is set.
func (s *Sync) requeueDependencyRuns(ctx context.Context, rec *store.MergeRequest, note string) {
	if s.RequeueDependencyRuns == nil {
		return
	}
	if err := s.RequeueDependencyRuns(ctx, rec, note); err != nil && ctx.Err() == nil {
		s.log().Warn("dependency watcher: re-queueing the paused runs of a merge request failed", "merge_request_id", rec.ID, "error", err)
	}
}

// ResumeNote is the task note of rule 8a: "Its prerequisite !17 is merged; the platform
// merged main into this branch (<sha7>) and CI passed." A branch that already contained the
// default branch says so, and a project without CI says that instead of CI passing.
func ResumeNote(labels []string, defaultBranch, outcome, head string, noCI bool) string {
	what := "Its prerequisite " + joinLabels(labels) + " is merged"
	if len(labels) > 1 {
		what = "Its prerequisites " + joinLabels(labels) + " are merged"
	}
	how := fmt.Sprintf("the platform merged %s into this branch (%s)", defaultBranch, deps.Short(head))
	if outcome == project.UpdateUpToDate {
		how = fmt.Sprintf("this branch already contains %s (%s)", defaultBranch, deps.Short(head))
	}
	ci := " and CI passed."
	if noCI {
		ci = "; the project has no CI configured."
	}
	return what + "; " + how + ci
}

func joinLabels(labels []string) string {
	switch len(labels) {
	case 0:
		return ""
	case 1:
		return labels[0]
	}
	return strings.Join(labels[:len(labels)-1], ", ") + " and " + labels[len(labels)-1]
}

// dependencyFailure records a provider's failure during the watcher's look at a dependent
// in its last_sync_error (failure handling): the dependent stays held. A database error is
// returned as it is.
func (s *Sync) dependencyFailure(ctx context.Context, rec *store.MergeRequest, pc *project.Config, err error) error {
	var pe *ProviderError
	var de *deps.ProviderError
	var text string
	switch {
	case errors.As(err, &pe):
		text = pe.Message
	case errors.As(err, &de):
		text = de.Error()
	case errors.Is(err, project.ErrNoMergeBase):
		text = project.ErrNoMergeBase.Error()
	default:
		return err
	}
	_, _, werr := s.recordFailure(ctx, rec, s.now(), "dependency check: "+text, pc.Token)
	return werr
}

// tell sends the operator a line about a dependent that stays held (rules 4 and 12), once
// per key: one failed commit, one conflict, one prerequisite closed without merging. It goes
// into the conversation the request's run reports to, the way the merge report does; a run
// that reports to no conversation sends nothing. g.Notice records the key once the message
// was sent (or there was no one to send it to); a deferred send is tried on the next pass.
func (s *Sync) tell(ctx context.Context, rec *store.MergeRequest, g *store.DependencyGate, key, reason string) {
	if g.Notice != nil && *g.Notice == key {
		return
	}
	if s.Reporter != nil {
		run, err := store.GetRun(ctx, s.Pool, rec.RunID)
		if err != nil {
			s.logDB(ctx, "read the run of a held merge request", err, "merge_request_id", rec.ID)
			return
		}
		if convID, ok := store.ConversationOfRun(run); ok {
			sum := sha256.Sum256([]byte(key))
			msgKey := "report-" + rec.RunID + "-mr-dependency-" + hex.EncodeToString(sum[:6])
			if s.Reporter.SendPlatformMessage(ctx, rec.RunID, convID, msgKey, BlockedText(rec, reason)) {
				return // deferred: the next pass tries again
			}
		}
	}
	g.Notice = &key
}

// BlockedText is the operator's line about a held dependent: "[platform] Run <id>: merge
// request !24 is blocked: <reason>."
func BlockedText(rec *store.MergeRequest, reason string) string {
	what := "merge request " + store.RequestLabel(rec.Provider, rec.Number)
	if rec.Provider == string(project.ProviderGitHub) {
		what = "pull request " + store.RequestLabel(rec.Provider, rec.Number)
	}
	return fmt.Sprintf("[platform] Run %s: %s is blocked: %s.", rec.RunID, what, strings.TrimSuffix(reason, "."))
}

func (s *Sync) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return SyncTimeout
}
