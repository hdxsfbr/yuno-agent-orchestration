package store

import (
	"context"
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
// (reported_at is null), oldest first, at most limit (DESIGN 5.23 rule 5: the closer's
// list; the channel report's queries filter on trigger = channel and never see these).
// A row whose trigger_ref is not "<project id>#<n>" (written by hand) is left out.
func ListUnreportedIssueRuns(ctx context.Context, q Querier, limit int) ([]IssueRun, error) {
	return listUnreportedIssueRuns(ctx, q, "", limit)
}

// ListUnreportedIssueRunsOfProject is ListUnreportedIssueRuns for the runs of one
// project's issues, the list the manual poll's closer works on.
func ListUnreportedIssueRunsOfProject(ctx context.Context, q Querier, projectID string, limit int) ([]IssueRun, error) {
	return listUnreportedIssueRuns(ctx, q, projectID, limit)
}

func listUnreportedIssueRuns(ctx context.Context, q Querier, projectID string, limit int) ([]IssueRun, error) {
	if limit <= 0 {
		limit = 50
	}
	prefix := ""
	if projectID != "" {
		prefix = projectID + "#"
	}
	rows, err := q.Query(ctx, `SELECT `+runColumns+` FROM runs WHERE trigger = $1 AND reported_at IS NULL
		AND status IN ($2, $3, $4, $5) AND ($6 = '' OR starts_with(trigger_ref, $6))
		ORDER BY created_at, id LIMIT $7`,
		TriggerIssue, RunSucceeded, RunFailed, RunCancelled, RunTimedOut, prefix, limit)
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
