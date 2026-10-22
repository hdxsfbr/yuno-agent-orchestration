package mergereq

import (
	"context"
	"errors"

	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// EventUpdated is the event a push the platform made to a request's branch emits (DESIGN
// wave 3 rule 11), filed under the record's run; its data is {"merge_request": record}.
const EventUpdated = "merge_request.updated"

// UpdateResult is what an update from the default branch did (DESIGN wave 3 rules 9 to
// 11): Outcome is project.UpdateUpToDate, UpdateUpdated or UpdateConflicts; Commit the
// merge commit pushed (updated only); DefaultSHA the default branch's commit merged in or
// found contained; Conflicts the files that conflicted (conflicts only).
type UpdateResult struct {
	Outcome    string   `json:"outcome"`
	Commit     string   `json:"commit,omitempty"`
	DefaultSHA string   `json:"default_sha,omitempty"`
	Conflicts  []string `json:"conflicts,omitempty"`
}

// ErrNotOpen is the answer for a record that is merged or closed: nothing is pushed to it.
type ErrNotOpen struct{ State string }

func (e *ErrNotOpen) Error() string { return "the request is " + e.State }

// ProviderError is a failure on the provider's side (its API or git), with its text
// cleaned as last_sync_error keeps one: redacted, the first line, capped. Transient says
// a retry may succeed (a timeout, a 5xx, a refused connection).
type ProviderError struct {
	Message   string
	Transient bool
}

func (e *ProviderError) Error() string { return e.Message }

// UpdateFromDefault merges the project's default branch into an open request's branch and
// pushes it (DESIGN wave 3 rules 9 to 11). POST /api/merge-requests/{id}/update-from-default
// and the dependency watcher (rule 8a) both call it; the watcher passes the record's
// head_sha, read fresh, as expectedHead.
//
//   - up_to_date: the branch has the default branch's commit; recorded conflicts are
//     cleared.
//   - conflicts: nothing was pushed; the files are recorded on the record (conflicts:
//     {files, at, base_sha}).
//   - updated: the merge commit was pushed, and the record is updated in one write that
//     invalidates the stale check (head_sha and ci_sha the new commit, ci_status pending,
//     no pipeline, updated_from_default_at now); merge_request.updated is emitted.
//
// The record returned is the one in the database afterwards. Errors: store.ErrNotFound for
// an unknown id; *ErrNotOpen; the project resolver's (project.ErrNoProject, ErrCannotRead,
// ErrNoKey and the like); *project.ErrHeadMoved when the branch is not at expectedHead, or
// moved before the push landed; project.ErrNoMergeBase; *ProviderError for git's or the
// provider's other failures; anything else is a database error.
func (s *Sync) UpdateFromDefault(ctx context.Context, id, expectedHead string) (*store.MergeRequest, UpdateResult, error) {
	rec, err := store.GetMergeRequest(ctx, s.Pool, id)
	if err != nil {
		return nil, UpdateResult{}, err
	}
	if rec.State != store.MergeRequestOpen {
		return rec, UpdateResult{}, &ErrNotOpen{State: rec.State}
	}
	pc, err := s.Projects.ForRunID(ctx, rec.RunID)
	if err != nil {
		return rec, UpdateResult{}, err
	}
	bu, err := pc.UpdateFromDefault(ctx, rec.SourceBranch, expectedHead)
	var moved *project.ErrHeadMoved
	switch {
	case errors.As(err, &moved), errors.Is(err, project.ErrNoMergeBase):
		return rec, UpdateResult{}, err
	case ctx.Err() != nil:
		return rec, UpdateResult{}, ctx.Err()
	case err != nil:
		return rec, UpdateResult{}, &ProviderError{Message: cleanError(err.Error(), pc.Token), Transient: tools.TransientError(err)}
	}
	res := UpdateResult{Outcome: bu.Outcome, Commit: bu.Commit, DefaultSHA: bu.Default, Conflicts: bu.Conflicts}
	now := s.now()
	switch bu.Outcome {
	case project.UpdateConflicts:
		s.log().Info("merge request update from the default branch conflicts", "merge_request_id", rec.ID, "files", len(bu.Conflicts))
		if _, err := store.SetMergeRequestConflicts(ctx, s.Pool, rec.ID, &store.MergeConflicts{Files: bu.Conflicts, At: now, BaseSHA: bu.Default}); err != nil {
			return rec, res, err
		}
	case project.UpdateUpToDate:
		if rec.Conflicts != nil {
			if _, err := store.SetMergeRequestConflicts(ctx, s.Pool, rec.ID, nil); err != nil {
				return rec, res, err
			}
		}
	case project.UpdateUpdated:
		s.log().Info("merge request updated from the default branch", "merge_request_id", rec.ID, "run_id", rec.RunID, "head", bu.Commit)
		if _, err := store.MergeRequestPushed(ctx, s.Pool, rec.ID, bu.Commit, now, true); err != nil {
			return rec, res, err
		}
	}
	cur, err := store.GetMergeRequest(ctx, s.Pool, rec.ID)
	if err != nil {
		return rec, res, err
	}
	if bu.Outcome == project.UpdateUpdated {
		s.emit(ctx, cur)
	}
	return cur, res, nil
}

// emit records merge_request.updated under the record's run (rule 11). An event that
// cannot be written is logged; the update stands.
func (s *Sync) emit(ctx context.Context, rec *store.MergeRequest) {
	if s.Events == nil {
		return
	}
	if _, err := s.Events.Emit(ctx, rec.RunID, EventUpdated, map[string]any{"merge_request": rec}); err != nil {
		s.logDB(ctx, "emit merge_request.updated", err, "merge_request_id", rec.ID)
	}
}
