package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Issue intake's queries over the runs table (DESIGN 5.23, decision D-052): the run row
// is the record of an issue, so there is no table of its own. The project settings
// queries are in projects.go.

// IssueTriggerRef is the trigger_ref of the run that works on an issue: "<project
// id>#<n>", n being the GitLab issue iid or the GitHub issue number (DESIGN 5.23 units).
func IssueTriggerRef(projectID string, number int64) string {
	return projectID + "#" + strconv.FormatInt(number, 10)
}

// ParseIssueTriggerRef splits a trigger_ref made by IssueTriggerRef; ok is false for any
// other text, a missing project id and a number that is not a positive integer included.
func ParseIssueTriggerRef(ref string) (projectID string, number int64, ok bool) {
	i := strings.LastIndexByte(ref, '#')
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.ParseInt(ref[i+1:], 10, 64)
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return ref[:i], n, true
}

// IssueRun is a run of an issue with the project id and the issue number parsed from its
// trigger_ref.
type IssueRun struct {
	Run
	ProjectID string
	Number    int64
}

// ListUnreportedIssueRuns returns the runs with trigger issue that ended (succeeded,
// failed, cancelled or timed out) and whose outcome was not posted on their issue yet
// (reported_at is null), newest finished_at first, at most limit (DESIGN 5.23 rule 5: the
// closer's list; the channel report's queries filter on trigger = channel and never see
// these). Newest first, so runs that can never be posted do not starve the newer ones
// (review M1). A row whose trigger_ref is not "<project id>#<n>" (written by hand) is left
// out.
func ListUnreportedIssueRuns(ctx context.Context, q Querier, limit int) ([]IssueRun, error) {
	return listUnreportedIssueRuns(ctx, q, "", limit)
}

// ListUnreportedIssueRunsOfProject is ListUnreportedIssueRuns for the runs of one
// project's issues, the list the manual poll's closer works on.
func ListUnreportedIssueRunsOfProject(ctx context.Context, q Querier, projectID string, limit int) ([]IssueRun, error) {
	return listUnreportedIssueRuns(ctx, q, projectID, limit)
}

// unreportedIssueRunsSQL is the closer's list. The trigger is written as a literal, not
// bound, so the planner can use the partial index runs_issue_unreported_idx (migration
// 0013), whose predicate it must prove from the query text (review L9); the order is the
// index's.
const unreportedIssueRunsSQL = `SELECT ` + runColumns + ` FROM runs WHERE trigger = 'issue' AND reported_at IS NULL
	AND status IN ($1, $2, $3, $4) AND ($5 = '' OR starts_with(trigger_ref, $5))
	ORDER BY finished_at DESC NULLS LAST, id LIMIT $6`

func listUnreportedIssueRuns(ctx context.Context, q Querier, projectID string, limit int) ([]IssueRun, error) {
	if limit <= 0 {
		limit = 50
	}
	prefix := ""
	if projectID != "" {
		prefix = projectID + "#"
	}
	rows, err := q.Query(ctx, unreportedIssueRunsSQL, RunSucceeded, RunFailed, RunCancelled, RunTimedOut, prefix, limit)
	if err != nil {
		return nil, err
	}
	runs, err := pgx.CollectRows(rows, pgx.RowToStructByName[Run])
	if err != nil {
		return nil, err
	}
	out := make([]IssueRun, 0, len(runs))
	for _, r := range runs {
		if r.TriggerRef == nil {
			continue
		}
		pid, n, ok := ParseIssueTriggerRef(*r.TriggerRef)
		if !ok {
			continue
		}
		out = append(out, IssueRun{Run: r, ProjectID: pid, Number: n})
	}
	return out, nil
}

// liveIssueRunSQL is the live-run lookup of an issue. The trigger is a literal for the
// reason of unreportedIssueRunsSQL: the partial unique index runs_issue_live_idx has
// trigger = 'issue' in its predicate, which a bound parameter cannot prove (review L9).
// The status list is the index's own, word for word.
const liveIssueRunSQL = `SELECT ` + runColumns + ` FROM runs WHERE trigger = 'issue' AND trigger_ref = $1
	AND status IN ` + liveRunStatuses + ` LIMIT 1`

// LiveIssueRun returns the live run (queued, running or waiting for an approval) of an
// issue's trigger reference, "<project id>#<n>": the run runs_issue_live_idx allows at
// most once per issue (DESIGN 5.23 rule 3), so there is no order to ask for. ErrNotFound
// when none is live.
func LiveIssueRun(ctx context.Context, q Querier, ref string) (*Run, error) {
	rows, err := q.Query(ctx, liveIssueRunSQL, ref)
	if err != nil {
		return nil, err
	}
	r, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Run])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// takenIssueRunSQL is the second half of TakenIssueRun: the newest ended run of the
// reference whose outcome is not posted yet. Literal trigger, as above.
const takenIssueRunSQL = `SELECT ` + runColumns + ` FROM runs WHERE trigger = 'issue' AND trigger_ref = $1
	AND reported_at IS NULL AND status IN ($2, $3, $4, $5) ORDER BY finished_at DESC NULLS LAST, id LIMIT 1`

// TakenIssueRun returns the run that has taken an issue (DESIGN 5.23 rule 3): the live
// run of the reference, else the newest run that ended with reported_at still null, whose
// outcome the closer owns and has not posted. A reference with neither has not been
// taken (ErrNotFound): a run whose outcome is posted does not count, which is what lets
// re-labelling an issue start a new run (rule 6). A finished run that counts as taken
// stops the taker from creating a second run for an issue whose label swap never
// succeeded (review H1).
func TakenIssueRun(ctx context.Context, q Querier, ref string) (*Run, error) {
	r, err := LiveIssueRun(ctx, q, ref)
	if !errors.Is(err, ErrNotFound) {
		return r, err
	}
	rows, err := q.Query(ctx, takenIssueRunSQL, ref, RunSucceeded, RunFailed, RunCancelled, RunTimedOut)
	if err != nil {
		return nil, err
	}
	r, err = pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Run])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// LiveRunCostCeiling is how many live runs (queued, running or waiting for an approval)
// the project has and the sum of the max_cost_usd limits they carry, in micro-USD: the most
// those runs may still add to the project's spend, whatever each has spent so far (DESIGN
// 5.23 rule 2 (a), review M2). runs.limits is the effective limits JSON of the run, with
// max_cost_usd in USD; a run whose limits name none adds nothing. Runs of any trigger count,
// whenever they were created.
func LiveRunCostCeiling(ctx context.Context, q Querier, projectID string) (live int, ceilingMicros int64, err error) {
	err = q.QueryRow(ctx, `SELECT count(*)::int,
			COALESCE(sum(round(COALESCE((limits->>'max_cost_usd')::numeric, 0) * 1000000)), 0)::bigint
		FROM runs WHERE project_id = $1 AND status IN `+liveRunStatuses, projectID).Scan(&live, &ceilingMicros)
	return live, ceilingMicros, err
}

// SetRunReportedAt records that an ended run's outcome was posted where the work came
// from (DESIGN 5.23 rule 5: the issue). It is a plain update, not ClaimRunReport's claim
// before the send: the closer sets it after the label and the comment, so a crash
// between them repeats the pass once. A run that is not ended any more (a retry, 5.21,
// re-queued it between the list and now) and a run reported already are left as they are.
func SetRunReportedAt(ctx context.Context, q Querier, id string, at time.Time) error {
	_, err := q.Exec(ctx, `UPDATE runs SET reported_at = $2
		WHERE id = $1 AND reported_at IS NULL AND status IN ($3, $4, $5, $6)`,
		id, at.UTC(), RunSucceeded, RunFailed, RunCancelled, RunTimedOut)
	return err
}
