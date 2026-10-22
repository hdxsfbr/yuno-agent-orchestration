// Package mergereq keeps the platform's merge request records true to their providers
// (DESIGN 5.20): a loop refreshes the open ones, a merge made through the platform is
// recorded, and the final state is reported once into the conversation that asked for
// the work. The records themselves are the store's; the provider calls are the tools
// package's, the same ones open_merge_request makes.
package mergereq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/events"
	"yuno/internal/obs"
	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/tools"
)

const (
	// DefaultInterval is MERGE_REQUEST_SYNC_INTERVAL's default (DESIGN 5.20 inputs).
	DefaultInterval = 120 * time.Second
	// Window is how far back from now the loop looks for open records, by created_at
	// (rule 3), and for final states still to report.
	Window = 14 * 24 * time.Hour
	// PassLimit is how many records one pass refreshes (rule 3).
	PassLimit = 50
	// SyncTimeout bounds the refresh of one record (rule 3).
	SyncTimeout = 10 * time.Second

	// errorMax caps last_sync_error.
	errorMax = 500
)

// Reporter sends a message into a conversation's session; the engine satisfies it
// (engine.SendPlatformMessage). deferred means the send was not attempted because the
// gateway or the database could not be reached, so the caller tries again later.
type Reporter interface {
	SendPlatformMessage(ctx context.Context, runID, conversationID, key, text string) (deferred bool)
}

// PlanHook is the part of the plans service the sync calls: a plan ticket whose merge
// request merged is merged.
type PlanHook interface {
	OnMergeRequestMerged(ctx context.Context, mrID string) error
	// ReapplyMerged sets every ticket whose record is merged to merged (review L5).
	ReapplyMerged(ctx context.Context) (int, error)
}

// Sync refreshes merge request records from their providers (DESIGN 5.20 rules 3, 4 and
// 8). Client should come from tools.ProviderClient, so a token header never follows a
// redirect to another host; nil is that client over the default transport. A nil
// Reporter reports nothing. Now is the clock (nil: the system's, in UTC).
type Sync struct {
	Pool     *pgxpool.Pool
	Projects *project.Resolver
	Client   *http.Client
	Reporter Reporter
	// Plans, when set, hears every record that turns merged (DESIGN 5.29 rule 10).
	Plans PlanHook
	// Events, when set, records merge_request.updated after a push the platform made
	// (Chief platform control, DESIGN 5.34 rule 11).
	Events *events.Recorder
	// Tickets, when set, releases the held plan tickets whose prerequisites are satisfied
	// after each dependency pass (Chief platform control, DESIGN 5.34 rule 8b).
	Tickets TicketReleaser
	// RequeueDependencyRuns, when set, is called once for a dependent merge request the
	// dependency watcher lets go, with the task note of rule 8a: the server re-queues the
	// runs parked on the request for its dependencies (rule 7).
	RequeueDependencyRuns func(ctx context.Context, rec *store.MergeRequest, note string) error
	Interval              time.Duration
	// Timeout bounds the refresh of one record; SyncTimeout (10 s) when zero.
	Timeout time.Duration
	Log     *slog.Logger
	Now     func() time.Time
}

// Run passes every Interval until ctx ends, the first one at once. A pass that fails is
// logged by TickOnce and the next tick tries again.
func (s *Sync) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	s.log().Info("merge request sync started", "interval", interval.String())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.TickOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// TickOnce is one pass (rule 3): the open records created in the last 14 days, the ones
// never synced first and then the longest unsynced, at most 50, each refreshed under
// its own 10 s timeout. A provider error is recorded on its record and counted in
// failed; a database error ends the pass with a log line and the next tick retries.
// Final states whose report was deferred are sent again at the start of the pass, so a
// state that turns final in a pass gets one attempt in it and the next one on the next.
func (s *Sync) TickOnce(ctx context.Context) (synced, failed int) {
	now := s.now()
	s.reportPending(ctx, now)
	s.reapplyPlans(ctx)
	recs, err := store.ListMergeRequestsToSync(ctx, s.Pool, now.Add(-Window), PassLimit)
	if err != nil {
		s.logDB(ctx, "list the merge requests to sync", err)
		return 0, 0
	}
	for i := range recs {
		if ctx.Err() != nil {
			return synced, failed
		}
		_, failedRecord, err := s.refresh(ctx, &recs[i])
		if err != nil {
			s.logDB(ctx, "sync a merge request", err, "merge_request_id", recs[i].ID)
			return synced, failed
		}
		if failedRecord {
			failed++
		} else {
			synced++
		}
	}
	// The dependency watcher runs on the sync tick, after the records it reads were
	// refreshed (Chief platform control, DESIGN 5.34 rule 8).
	s.WatchDependencies(ctx)
	return synced, failed
}

// reapplyPlans re-applies the merges the plan tickets missed (DESIGN 5.29 rule 10, review
// L5): a record that turned merged while its ticket was not told, as when the process
// stopped between the two writes, is not lost.
func (s *Sync) reapplyPlans(ctx context.Context) {
	if s.Plans == nil {
		return
	}
	if n, err := s.Plans.ReapplyMerged(ctx); err != nil {
		s.logDB(ctx, "re-apply merged plan tickets", err)
	} else if n > 0 {
		s.log().Info("plan tickets of merged requests set merged", "count", n)
	}
}

// SyncOne refreshes one record at once (POST /api/merge-requests/{id}/sync, rule 3) and
// returns it as it is afterwards. A provider error is recorded on the record, not
// returned; a record that is not open is returned as it is. store.ErrNotFound for an
// unknown id.
func (s *Sync) SyncOne(ctx context.Context, id string) (*store.MergeRequest, error) {
	rec, err := store.GetMergeRequest(ctx, s.Pool, id)
	if err != nil {
		return nil, err
	}
	if rec.State != store.MergeRequestOpen {
		return rec, nil
	}
	cur, _, err := s.refresh(ctx, rec)
	if err == nil && changedForDependents(rec, cur) {
		// A prerequisite whose state or CI changed: its dependents are looked at now
		// (DESIGN 5.34 rule 8), and cur is re-read in case one of them is itself.
		s.watchDependentsOf(ctx, cur.ID)
		if again, gerr := store.GetMergeRequest(ctx, s.Pool, cur.ID); gerr == nil {
			cur = again
		}
	}
	return cur, err
}

// changedForDependents reports whether a refresh changed what a dependent's watcher reads
// of its prerequisite: the state, or the check of the head.
func changedForDependents(before, after *store.MergeRequest) bool {
	if after == nil {
		return false
	}
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return before.State != after.State || before.CIStatus != after.CIStatus || str(before.CISHA) != str(after.CISHA)
}

// MarkMerged records a merge the platform made on the provider (rule 5): the record
// becomes merged, closed_at now, merged_by by, and the report of rule 4 goes out. A
// record that left open meanwhile (a sync saw the merge first) is returned as it is,
// reported by whoever won. The error is a database one.
func (s *Sync) MarkMerged(ctx context.Context, id, by string) (*store.MergeRequest, error) {
	return s.markMerged(ctx, id, by, nil, "")
}

// markMerged is MarkMerged with the provider's merged_at and merge commit, which decide
// whether a pipeline ran after the merge (rule 11): closed_at is the provider's clock when it
// gave a time, the platform's only otherwise.
func (s *Sync) markMerged(ctx context.Context, id, by string, mergedAt *time.Time, mergeSHA string) (*store.MergeRequest, error) {
	var sha *string
	if mergeSHA != "" {
		sha = &mergeSHA
	}
	won, err := store.MarkMergeRequestMergedAs(ctx, s.Pool, id, by, s.now(), mergedAt, sha)
	if err != nil {
		return nil, err
	}
	cur, err := store.GetMergeRequest(ctx, s.Pool, id)
	if err != nil {
		return nil, err
	}
	if !won {
		return cur, nil
	}
	return s.reported(ctx, cur)
}

// ConfirmMerged follows a 2xx from the provider's merge call (rule 5). The request is read
// once, and the record becomes merged (MarkMerged) only when the provider reports it
// merged: a provider that accepted the call but still reports the request open (GitLab
// merges asynchronously, or when the pipeline passes), or in any other state, leaves the
// record open with last_sync_error saying so, and so does a status that cannot be read;
// the loop settles the record once the provider's state is final. The record returned is
// the one in the database afterwards; the error is a database one.
func (s *Sync) ConfirmMerged(ctx context.Context, rec *store.MergeRequest, pc *project.Config, by string) (*store.MergeRequest, error) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = SyncTimeout
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	st, err := tools.ProviderFor(pc, s.Client).Status(pctx, rec.Number)
	cancel()
	var text string
	switch {
	case err == nil && st.State == store.MergeRequestMerged:
		return s.markMerged(ctx, rec.ID, by, st.ClosedAt, st.MergeSHA)
	case err == nil:
		text = "the provider accepted the merge but reports the request as " + st.State
	case ctx.Err() != nil:
		return rec, ctx.Err()
	default:
		text = "the provider accepted the merge but the request could not be read back: " + err.Error()
	}
	cur, _, err := s.recordFailure(ctx, rec, s.now(), text, pc.Token)
	return cur, err
}

// Backfill makes records of the merge requests opened before records existed (rule 7):
// once per start, idempotent. It logs what it did; the caller may ignore the results.
func (s *Sync) Backfill(ctx context.Context) (int64, error) {
	n, err := store.BackfillMergeRequests(ctx, s.Pool)
	if err != nil {
		s.log().Warn("merge request backfill failed", "error", err)
		return 0, err
	}
	if n > 0 {
		s.log().Info("merge requests backfilled", "count", n)
	}
	return n, nil
}

// refresh reads one open record from its provider and writes the result (rule 3). failed
// says the provider side went wrong and the record carries the text; err is a database
// error, which ends the pass. The record returned is the one in the database after.
func (s *Sync) refresh(ctx context.Context, rec *store.MergeRequest) (cur *store.MergeRequest, failed bool, err error) {
	now := s.now()
	pc, err := s.Projects.ForRunID(ctx, rec.RunID)
	switch {
	case ctx.Err() != nil:
		return rec, false, ctx.Err()
	case errors.Is(err, store.ErrNotFound):
		return rec, false, nil // the run is gone; so is the record, with it
	case err != nil:
		// A token that cannot be read (rule 8), a project without one: the operator fixes
		// it on the Projects page; the record is skipped until then.
		return s.recordFailure(ctx, rec, now, err.Error(), "")
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = SyncTimeout
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	st, err := tools.ProviderFor(pc, s.Client).Status(pctx, rec.Number)
	switch {
	case errors.Is(err, tools.ErrRequestNotFound):
		// The request is gone from the provider: closed, with the reason; what was known of
		// its check stays.
		msg := tools.ErrRequestNotFound.Error()
		return s.settle(ctx, rec, store.MergeRequestSync{State: store.MergeRequestClosed, CIStatus: rec.CIStatus,
			HeadSHA: rec.HeadSHA, UnresolvedThreads: rec.UnresolvedThreads, LastSyncError: &msg, SyncedAt: now, ClosedAt: &now,
			CISHA: rec.CISHA, CIPipelineID: rec.CIPipelineID, CIURL: rec.CIURL, Mergeable: rec.Mergeable, MergeStatus: rec.MergeStatus})
	case ctx.Err() != nil:
		return rec, false, ctx.Err()
	case err != nil:
		return s.recordFailure(ctx, rec, now, err.Error(), pc.Token)
	}
	u := store.MergeRequestSync{State: st.State, CIStatus: st.CIStatus, UnresolvedThreads: st.UnresolvedThreads,
		SyncedAt: now, ClosedAt: st.ClosedAt, Mergeable: st.Mergeable}
	if st.MergeSHA != "" && st.State == store.MergeRequestMerged {
		// DESIGN 5.34 rule 4: the merge commit is persisted from the sync too.
		u.MergeSHA = &st.MergeSHA
	}
	if st.HeadSHA != "" {
		// The provider read the CI of exactly this commit (rule 13).
		u.HeadSHA, u.CISHA = &st.HeadSHA, &st.HeadSHA
	}
	if st.PipelineID != 0 {
		u.CIPipelineID = &st.PipelineID
	}
	if st.CIURL != "" {
		u.CIURL = &st.CIURL
	}
	if st.MergeStatus != "" {
		u.MergeStatus = &st.MergeStatus
	}
	if st.Err != "" {
		msg := cleanError(st.Err, pc.Token)
		u.LastSyncError = &msg
	}
	return s.settle(ctx, rec, u)
}

// settle writes a successful refresh and, when the record turned merged or closed,
// reports it (rule 4). The update is conditional on the record still being open: when a
// merge or another sync got there first, nothing is written and the record is re-read.
func (s *Sync) settle(ctx context.Context, rec *store.MergeRequest, u store.MergeRequestSync) (*store.MergeRequest, bool, error) {
	won, err := store.UpdateMergeRequestSync(ctx, s.Pool, rec.ID, u)
	if err != nil {
		return rec, false, err
	}
	cur, err := store.GetMergeRequest(ctx, s.Pool, rec.ID)
	if err != nil {
		return rec, false, err
	}
	if won && cur.State != store.MergeRequestOpen {
		s.log().Info("merge request reached its final state", "merge_request_id", cur.ID, "run_id", cur.RunID, "state", cur.State)
		cur, err = s.reported(ctx, cur)
	}
	return cur, false, err
}

// recordFailure writes a refresh that failed: the text (redacted, one line, capped) and
// the attempt's time, the rest of the record as it was (rule 3).
func (s *Sync) recordFailure(ctx context.Context, rec *store.MergeRequest, at time.Time, text, token string) (*store.MergeRequest, bool, error) {
	msg := cleanError(text, token)
	s.log().Warn("merge request sync failed", "merge_request_id", rec.ID, "run_id", rec.RunID, "error", msg)
	if _, err := store.UpdateMergeRequestSyncError(ctx, s.Pool, rec.ID, msg, at); err != nil {
		return rec, true, err
	}
	cur, err := store.GetMergeRequest(ctx, s.Pool, rec.ID)
	if err != nil {
		return rec, true, err
	}
	return cur, true, nil
}

// reported sends the record's final-state report and returns the record re-read, so the
// caller sees reported_state.
func (s *Sync) reported(ctx context.Context, rec *store.MergeRequest) (*store.MergeRequest, error) {
	if s.Plans != nil && rec.State == store.MergeRequestMerged {
		if err := s.Plans.OnMergeRequestMerged(ctx, rec.ID); err != nil {
			s.logDB(ctx, "update the plan ticket of a merged request", err, "merge_request_id", rec.ID)
		}
	}
	s.report(ctx, rec)
	return store.GetMergeRequest(ctx, s.Pool, rec.ID)
}

// report sends the line of rule 4 into the conversation the run reports to, once per
// final state: the idempotency key is report-<run id>-mr-<state>, one attempt, and
// reported_state is written after a send that was not deferred. A run that reports to no
// conversation, a record that is open or reported already, and a missing Reporter send
// nothing.
func (s *Sync) report(ctx context.Context, rec *store.MergeRequest) {
	if s.Reporter == nil || rec.State == store.MergeRequestOpen || rec.ReportedState != nil {
		return
	}
	run, err := store.GetRun(ctx, s.Pool, rec.RunID)
	if err != nil {
		s.logDB(ctx, "read the run of a merge request to report it", err, "merge_request_id", rec.ID)
		return
	}
	// The test of DESIGN 5.15 rule 9, as engine.RunReportsTo applies it.
	convID, ok := store.ConversationOfRun(run)
	if !ok {
		return
	}
	key := "report-" + rec.RunID + "-mr-" + rec.State
	if s.Reporter.SendPlatformMessage(ctx, rec.RunID, convID, key, ReportText(rec)) {
		return // deferred: the next pass sends it again under the same key
	}
	if _, err := store.MarkMergeRequestReported(ctx, s.Pool, rec.ID, rec.State); err != nil {
		s.logDB(ctx, "mark a merge request reported", err, "merge_request_id", rec.ID)
	}
}

// reportPending sends the reports a deferred send left behind (rule 4).
func (s *Sync) reportPending(ctx context.Context, now time.Time) {
	if s.Reporter == nil {
		return
	}
	recs, err := store.ListMergeRequestsToReport(ctx, s.Pool, now.Add(-Window), PassLimit)
	if err != nil {
		s.logDB(ctx, "list the merge requests to report", err)
		return
	}
	for i := range recs {
		if ctx.Err() != nil {
			return
		}
		s.report(ctx, &recs[i])
	}
}

// ReportText is the line of DESIGN 5.20 rule 4: "[platform] Run <id>: merge request
// !<n> was merged." or "... was closed without being merged." A pull request is
// "pull request #<n>".
func ReportText(rec *store.MergeRequest) string {
	what := fmt.Sprintf("merge request !%d", rec.Number)
	if rec.Provider == string(project.ProviderGitHub) {
		what = fmt.Sprintf("pull request #%d", rec.Number)
	}
	outcome := "was merged"
	if rec.State != store.MergeRequestMerged {
		outcome = "was closed without being merged"
	}
	return fmt.Sprintf("[platform] Run %s: %s %s.", rec.RunID, what, outcome)
}

// cleanError is a provider's or resolver's text as last_sync_error keeps it: valid UTF-8
// without NUL bytes (the column refuses both, and a write that fails would end every
// pass at this record), the token and every credential-shaped value redacted, the first
// line only, capped at errorMax bytes on a rune boundary.
func cleanError(text, token string) string {
	text = strings.ToValidUTF8(text, "")
	text = strings.ReplaceAll(text, "\x00", "")
	if token != "" {
		text = strings.ReplaceAll(text, token, obs.Redacted)
	}
	text = obs.Redact(text)
	if i := strings.IndexAny(text, "\r\n"); i >= 0 {
		text = text[:i]
	}
	text = strings.TrimSpace(text)
	if len(text) > errorMax {
		cut := errorMax
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	return text
}

func (s *Sync) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Sync) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// logDB logs a database failure unless it is the shutdown's own.
func (s *Sync) logDB(ctx context.Context, what string, err error, attrs ...any) {
	if ctx.Err() != nil {
		return
	}
	s.log().Warn("merge request sync: "+what+" failed", append(attrs, "error", err)...)
}
