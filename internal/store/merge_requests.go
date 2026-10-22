package store

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Merge request records (DESIGN 5.20): one row per run that opened a merge or pull
// request, written with the side effect, refreshed from the provider by the sync loop,
// merged on the operator's request. The token never reaches this package.

const (
	// DefaultMergeRequestsLimit and MaxMergeRequestsLimit bound a listing (rule 6).
	DefaultMergeRequestsLimit = 50
	MaxMergeRequestsLimit     = 200
)

const mergeRequestColumnList = `id, project_id, run_id, provider, number, url, source_branch, target_branch, title,
	state, ci_status, head_sha, unresolved_threads, last_sync_error, synced_at, reported_state, merged_by,
	created_at, updated_at, closed_at, draft`

// mergeRequestColumns is the column list qualified with a table alias, for queries that
// join the runs table.
func mergeRequestColumns(alias string) string {
	if alias == "" {
		return mergeRequestColumnList
	}
	cols := strings.Split(mergeRequestColumnList, ",")
	for i, c := range cols {
		cols[i] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(cols, ", ")
}

func collectMergeRequests(rows pgx.Rows) ([]MergeRequest, error) {
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[MergeRequest])
	if out == nil {
		out = []MergeRequest{}
	}
	return out, err
}

// InsertMergeRequest stores a record for a run (rule 2): state open, ci_status none and
// synced_at null unless the caller says otherwise, an id and the times filled in. q is
// the side effect's transaction when the tool writes it. It reports false, writing
// nothing, when the run already has a record or the provider's number is recorded for
// the project (the two unique indexes): ON CONFLICT DO NOTHING rather than an error, so
// the surrounding transaction is not aborted.
func InsertMergeRequest(ctx context.Context, q Querier, mr *MergeRequest) (inserted bool, err error) {
	inserted, err = insertMergeRequest(ctx, q, mr, nil)
	if err != nil || !inserted {
		return inserted, err
	}
	// The run's plan ticket, if it builds one, is now in review (DESIGN 5.29 rule 10), in
	// the transaction that wrote the record.
	if _, _, err := LinkTicketMergeRequest(ctx, q, mr.RunID, mr.ID); err != nil {
		return false, err
	}
	return true, nil
}

// InsertFollowUpMergeRequest stores the follow-up of the record followed (DESIGN 5.29 rule
// 4): a record of the same run that names it, written as InsertMergeRequest writes one. It
// reports false, writing nothing, when the record followed has a follow-up already or the
// provider's number is recorded for the project. The run's plan ticket keeps the request it
// was built in.
func InsertFollowUpMergeRequest(ctx context.Context, q Querier, mr *MergeRequest, followed string) (inserted bool, err error) {
	return insertMergeRequest(ctx, q, mr, &followed)
}

func insertMergeRequest(ctx context.Context, q Querier, mr *MergeRequest, followUpOf *string) (inserted bool, err error) {
	if mr.ID == "" {
		mr.ID = NewID()
	}
	if mr.State == "" {
		mr.State = MergeRequestOpen
	}
	if mr.CIStatus == "" {
		mr.CIStatus = CIStatusNone
	}
	now := time.Now().UTC()
	if mr.CreatedAt.IsZero() {
		mr.CreatedAt = now
	}
	if mr.UpdatedAt.IsZero() {
		mr.UpdatedAt = mr.CreatedAt
	}
	tag, err := q.Exec(ctx, `INSERT INTO merge_requests (`+mergeRequestColumnList+`, follow_up_of)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
		ON CONFLICT DO NOTHING`,
		mr.ID, mr.ProjectID, mr.RunID, mr.Provider, mr.Number, mr.URL, mr.SourceBranch, mr.TargetBranch, mr.Title,
		mr.State, mr.CIStatus, mr.HeadSHA, mr.UnresolvedThreads, mr.LastSyncError, mr.SyncedAt, mr.ReportedState, mr.MergedBy,
		mr.CreatedAt, mr.UpdatedAt, mr.ClosedAt, mr.Draft, followUpOf)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// GetMergeRequest returns a record by id, or ErrNotFound.
func GetMergeRequest(ctx context.Context, q Querier, id string) (*MergeRequest, error) {
	rows, err := q.Query(ctx, `SELECT `+mergeRequestColumnList+` FROM merge_requests WHERE id=$1`, id)
	if err != nil {
		return nil, err
	}
	mr, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[MergeRequest])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return mr, err
}

// MergeRequestByRun returns the run's record, or nil when it has none (the answer
// MergeRequestOfRun gave, which this replaces). It is the request the run opened, never a
// follow-up of it (FollowUpMergeRequest).
func MergeRequestByRun(ctx context.Context, q Querier, runID string) (*MergeRequest, error) {
	return oneMergeRequest(ctx, q, `SELECT `+mergeRequestColumnList+` FROM merge_requests WHERE run_id=$1 AND follow_up_of IS NULL`, runID)
}

// FollowUpMergeRequest returns the follow-up of a record (DESIGN 5.29 rule 4), or nil when
// it has none.
func FollowUpMergeRequest(ctx context.Context, q Querier, id string) (*MergeRequest, error) {
	return oneMergeRequest(ctx, q, `SELECT `+mergeRequestColumnList+` FROM merge_requests WHERE follow_up_of=$1`, id)
}

// oneMergeRequest is the record a query answers, or nil when it answers none.
func oneMergeRequest(ctx context.Context, q Querier, sql string, args ...any) (*MergeRequest, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	mr, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[MergeRequest])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return mr, err
}

// MergeRequestByNumber returns the project's record with the provider's number (the
// unique (project_id, number) pair of rule 1), or nil when the project has none: the
// answer shape of MergeRequestByRun, so a missing record is not an error. The Chief of
// Staff resolves the number or URL a person gave against it (DESIGN 5.22 rule 8).
func MergeRequestByNumber(ctx context.Context, q Querier, projectID string, number int64) (*MergeRequest, error) {
	rows, err := q.Query(ctx, `SELECT `+mergeRequestColumnList+` FROM merge_requests WHERE project_id=$1 AND number=$2`,
		projectID, number)
	if err != nil {
		return nil, err
	}
	mr, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[MergeRequest])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return mr, err
}

// ProjectNameAny returns the name of a project, deleted or not (a soft-deleted project
// keeps its row and its name, DESIGN 5.17), or ErrNotFound when there is no such row.
// A refusal that says which project a merge request belongs to reads it (DESIGN 5.22
// rule 1), since the record outlives the project's deletion.
func ProjectNameAny(ctx context.Context, q Querier, id string) (string, error) {
	var name string
	err := q.QueryRow(ctx, `SELECT name FROM projects WHERE id=$1`, id).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}

// liveRunStatuses are the statuses of a live run, the ones the two live-run indexes of
// migration 0013 hold (DESIGN 5.22 units); a paused run is live too (DESIGN W2 rule 14,
// migration 0032).
const liveRunStatuses = `('queued', 'running', 'waiting_approval', 'paused')`

// LiveRunForMergeRequest returns the live run (queued, running or waiting for an
// approval) that names the merge request record, the one runs_merge_request_live_idx
// allows at most once; ErrNotFound when no run does. A refusal that says which run is
// updating the request reads it (DESIGN 5.22 rule 1, 5.21 rule 3).
func LiveRunForMergeRequest(ctx context.Context, q Querier, mrID string) (*Run, error) {
	rows, err := q.Query(ctx, `SELECT `+runColumns+` FROM runs WHERE merge_request_id=$1 AND status IN `+liveRunStatuses+`
		ORDER BY created_at LIMIT 1`, mrID)
	if err != nil {
		return nil, err
	}
	r, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Run])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// LockRunMergeRequest takes a transaction-scoped advisory lock on the run's merge
// request, so two calls of one run that carry different idempotency keys cannot both
// find no record and open two requests (rule 2: never a second request). q must be a
// transaction; the lock ends with it.
func LockRunMergeRequest(ctx context.Context, q Querier, runID string) error {
	_, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('merge_request:' || $1::text, 0))`, runID)
	return err
}

// MergeRequestFilter narrows ListMergeRequests; an empty member does not filter. A
// Limit outside 1..MaxMergeRequestsLimit is the default (50), or 200 above it.
type MergeRequestFilter struct {
	ProjectID string
	RunID     string
	State     string
	Limit     int
}

// ListMergeRequests returns records newest first (rule 6).
func ListMergeRequests(ctx context.Context, q Querier, f MergeRequestFilter) ([]MergeRequest, error) {
	limit := f.Limit
	switch {
	case limit <= 0:
		limit = DefaultMergeRequestsLimit
	case limit > MaxMergeRequestsLimit:
		limit = MaxMergeRequestsLimit
	}
	rows, err := q.Query(ctx, `SELECT `+mergeRequestColumnList+` FROM merge_requests
		WHERE ($1 = '' OR project_id = $1) AND ($2 = '' OR run_id = $2) AND ($3 = '' OR state = $3)
		ORDER BY created_at DESC, id DESC LIMIT $4`, f.ProjectID, f.RunID, f.State, limit)
	if err != nil {
		return nil, err
	}
	return collectMergeRequests(rows)
}

// ListMergeRequestsToSync returns the open records created after since, the ones never
// synced first and then the longest unsynced (rule 3).
func ListMergeRequestsToSync(ctx context.Context, q Querier, since time.Time, limit int) ([]MergeRequest, error) {
	rows, err := q.Query(ctx, `SELECT `+mergeRequestColumnList+` FROM merge_requests
		WHERE state = $1 AND created_at > $2
		ORDER BY synced_at ASC NULLS FIRST, created_at, id LIMIT $3`, MergeRequestOpen, since.UTC(), limit)
	if err != nil {
		return nil, err
	}
	return collectMergeRequests(rows)
}

// ListMergeRequestsToReport returns the records that reached merged or closed after
// since and whose run reports to a conversation (DESIGN 5.15 rule 9's test) but whose
// final state was not reported yet: a send that was deferred (rule 4) is tried again.
func ListMergeRequestsToReport(ctx context.Context, q Querier, since time.Time, limit int) ([]MergeRequest, error) {
	rows, err := q.Query(ctx, `SELECT `+mergeRequestColumns("mr")+` FROM merge_requests mr JOIN runs r ON r.id = mr.run_id
		WHERE mr.state IN ($1, $2) AND mr.reported_state IS NULL AND COALESCE(mr.closed_at, mr.updated_at) > $3
		  AND r.trigger = $4 AND r.trigger_ref LIKE $5
		ORDER BY COALESCE(mr.closed_at, mr.updated_at), mr.id LIMIT $6`,
		MergeRequestMerged, MergeRequestClosed, since.UTC(), TriggerChannel, ConversationRefPrefix+"%", limit)
	if err != nil {
		return nil, err
	}
	return collectMergeRequests(rows)
}

// MergeRequestSync is what one refresh from the provider writes (rule 3). A state other
// than open needs ClosedAt (the provider's time, or now).
type MergeRequestSync struct {
	State             string
	CIStatus          string
	HeadSHA           *string
	UnresolvedThreads *int
	LastSyncError     *string
	SyncedAt          time.Time
	ClosedAt          *time.Time
	// MergeSHA is the commit the provider says the merge made, when it merged and says.
	MergeSHA *string
}

// UpdateMergeRequestSync writes a refresh in one statement conditional on state = 'open':
// it reports false, changing nothing, when the record is not open any more (a merge or a
// concurrent sync won; the caller re-reads). updated_at moves only when something the
// provider reports changed.
func UpdateMergeRequestSync(ctx context.Context, q Querier, id string, u MergeRequestSync) (bool, error) {
	at := u.SyncedAt.UTC()
	var closedAt *time.Time
	if u.State != MergeRequestOpen {
		c := at
		if u.ClosedAt != nil {
			c = u.ClosedAt.UTC()
		}
		closedAt = &c
	}
	tag, err := q.Exec(ctx, `UPDATE merge_requests SET
			updated_at = CASE WHEN (state, ci_status, head_sha, unresolved_threads)
				IS DISTINCT FROM ($2::text, $3::text, $4::text, $5::int) THEN $7::timestamptz ELSE updated_at END,
			state = $2, ci_status = $3, head_sha = $4, unresolved_threads = $5, last_sync_error = $6,
			synced_at = $7, closed_at = $8
		WHERE id = $1 AND state = $9`,
		id, u.State, u.CIStatus, u.HeadSHA, u.UnresolvedThreads, u.LastSyncError, at, closedAt, MergeRequestOpen)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// UpdateMergeRequestSyncError records a failed refresh: the text and the attempt's time,
// the rest of the record as it was (rule 3). Conditional on state = 'open'.
func UpdateMergeRequestSyncError(ctx context.Context, q Querier, id, message string, at time.Time) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE merge_requests SET last_sync_error = $2, synced_at = $3
		WHERE id = $1 AND state = $4`, id, message, at.UTC(), MergeRequestOpen)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// MarkMergeRequestMerged records a merge made through the platform (rule 5): state
// merged, closed_at at, merged_by by. Conditional on state = 'open', so a merge that
// races a sync takes effect once: false means the other side won and the caller re-reads.
func MarkMergeRequestMerged(ctx context.Context, q Querier, id, by string, at time.Time) (bool, error) {
	return MarkMergeRequestMergedAs(ctx, q, id, by, at, nil, nil)
}

// MarkMergeRequestMergedAs is MarkMergeRequestMerged with the provider's own account of the
// merge (DESIGN 5.29 rule 11): closed_at is the provider's merged_at when it gave one (at,
// the platform's time, is the fallback and always the sync time), merge_sha its merge commit.
func MarkMergeRequestMergedAs(ctx context.Context, q Querier, id, by string, at time.Time, mergedAt *time.Time, mergeSHA *string) (bool, error) {
	closed := at.UTC()
	if mergedAt != nil {
		closed = mergedAt.UTC()
	}
	tag, err := q.Exec(ctx, `UPDATE merge_requests SET state = $2, closed_at = $6, merged_by = $4, synced_at = $3,
			last_sync_error = NULL, updated_at = $3, merge_sha = $7
		WHERE id = $1 AND state = $5`, id, MergeRequestMerged, at.UTC(), by, MergeRequestOpen, closed, mergeSHA)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// MarkMergeRequestReported records that the final state was reported into the run's
// conversation (rule 4), once: false when the record is not in that state or was
// reported already.
func MarkMergeRequestReported(ctx context.Context, q Querier, id, state string) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE merge_requests SET reported_state = $2
		WHERE id = $1 AND state = $2 AND reported_state IS NULL`, id, state)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// FillRunMergeRequests sets MergeRequest on each run that has one (rule 6), the way
// FillRunParticipants sets the agent names: the record the run names (runs.merge_request_id)
// when it names one, else the record the run opened (merge_requests.run_id), so a header,
// a card and run_status need no second lookup (DESIGN 5.22 rule 1, 5.20 rule 6 amended).
func FillRunMergeRequests(ctx context.Context, q Querier, runs []Run) error {
	if len(runs) == 0 {
		return nil
	}
	ids := make([]string, len(runs))
	var named []string
	for i := range runs {
		ids[i] = runs[i].ID
		if runs[i].MergeRequestID != nil {
			named = append(named, *runs[i].MergeRequestID)
		}
	}
	// A run's own record is the one that follows none (a follow-up is listed with the run's
	// merge requests, not in its header); a named record may be any.
	rows, err := q.Query(ctx, `SELECT `+mergeRequestColumnList+`, follow_up_of IS NULL AS own FROM merge_requests
		WHERE (run_id = ANY($1) AND follow_up_of IS NULL) OR id = ANY($2)`, ids, named)
	if err != nil {
		return err
	}
	type listed struct {
		MergeRequest
		Own bool `db:"own"`
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByName[listed])
	if err != nil {
		return err
	}
	byRun := make(map[string]*MergeRequest, len(list))
	byID := make(map[string]*MergeRequest, len(list))
	for i := range list {
		if list[i].Own {
			byRun[list[i].RunID] = &list[i].MergeRequest
		}
		byID[list[i].ID] = &list[i].MergeRequest
	}
	for i := range runs {
		runs[i].MergeRequest = byRun[runs[i].ID]
		if runs[i].MergeRequestID != nil {
			// The foreign key sets the column null with the record, so a named record
			// is there; the run's own would only fill in if a race had lost it.
			if mr := byID[*runs[i].MergeRequestID]; mr != nil {
				runs[i].MergeRequest = mr
			}
		}
	}
	return nil
}

// BackfillMergeRequests makes a record of every executed open_merge_request call that
// has none yet, for runs that name a project (rule 7), reading the calls the way
// MergeRequestOfRun did: the newest executed call with a URL wins, one record per run.
// The record is open with synced_at null and the time of the call; the first sync pass
// settles it. It is idempotent (ON CONFLICT DO NOTHING) and skips runs without a
// project, calls without a branch and a number it cannot read. It returns how many
// records it inserted.
func BackfillMergeRequests(ctx context.Context, q Querier) (inserted int64, err error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (tc.run_id) tc.run_id, r.project_id,
			COALESCE(r.project_snapshot->>'provider', ''), COALESCE(r.project_snapshot->>'default_branch', ''),
			tc.input, tc.output, COALESCE(tc.finished_at, tc.started_at)
		FROM tool_calls tc JOIN runs r ON r.id = tc.run_id
		WHERE tc.tool_name IN ('open_merge_request', 'yuno__open_merge_request') AND tc.status = 'executed'
		  AND NOT tc.is_error AND jsonb_typeof(tc.output) = 'object' AND tc.output ? 'url'
		  AND r.project_id IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM merge_requests m WHERE m.run_id = tc.run_id)
		ORDER BY tc.run_id, tc.finished_at DESC NULLS LAST, tc.started_at DESC`)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		RunID, ProjectID, Provider, DefaultBranch string
		Input, Output                             json.RawMessage
		At                                        *time.Time
	}
	cands, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.RunID, &c.ProjectID, &c.Provider, &c.DefaultBranch, &c.Input, &c.Output, &c.At)
		return c, err
	})
	if err != nil {
		return 0, err
	}
	for _, c := range cands {
		var out struct {
			Number   json.Number `json:"number"`
			URL      string      `json:"url"`
			Branch   string      `json:"branch"`
			Provider string      `json:"provider"`
		}
		if json.Unmarshal(c.Output, &out) != nil || out.URL == "" || out.Branch == "" {
			continue
		}
		number, err := out.Number.Int64()
		if err != nil || number <= 0 {
			number = numberOfURL(out.URL)
		}
		if number <= 0 {
			continue
		}
		var in struct {
			Title string `json:"title"`
		}
		_ = json.Unmarshal(c.Input, &in)
		provider := cmp.Or(out.Provider, c.Provider, providerOfURL(out.URL))
		mr := &MergeRequest{ProjectID: c.ProjectID, RunID: c.RunID, Provider: provider, Number: number, URL: out.URL,
			SourceBranch: out.Branch, TargetBranch: cmp.Or(c.DefaultBranch, "main"), Title: in.Title}
		if c.At != nil {
			mr.CreatedAt = c.At.UTC()
		}
		ok, err := InsertMergeRequest(ctx, q, mr)
		if err != nil {
			return inserted, err
		}
		if ok {
			inserted++
		}
	}
	return inserted, nil
}

// numberOfURL is the trailing number of a merge or pull request URL, 0 when it has none.
func numberOfURL(u string) int64 {
	u = strings.TrimRight(u, "/")
	n, err := strconv.ParseInt(u[strings.LastIndexByte(u, '/')+1:], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// providerOfURL guesses the provider of a request URL: GitHub's pull requests live under
// /pull/<n>, GitLab's under /merge_requests/<n>.
func providerOfURL(u string) string {
	if strings.Contains(u, "/pull/") {
		return "github"
	}
	return "gitlab"
}

// SetMergeRequestDraft records a title and draft state the platform set on the provider
// (DESIGN 5.29 rule 4).
func SetMergeRequestDraft(ctx context.Context, q Querier, id, title string, draft bool) error {
	_, err := q.Exec(ctx, `UPDATE merge_requests SET title=$2, draft=$3, updated_at=now() WHERE id=$1`, id, title, draft)
	return err
}
