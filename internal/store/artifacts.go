package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Run artifacts (DESIGN 5.25 rule 4): the rows of the files a run delivered. The
// collector writes them; the API and the Chief's run_status read them.

// artifactColumns are the columns of run_artifacts in the order RunArtifact lists them.
const artifactColumns = `id, run_id, node_id, visit, path, size, sha256, content_type, generated, created_at, updated_at`

// ListRunArtifacts returns the run's artifacts ordered by path; an empty slice, not nil,
// when it has none.
func ListRunArtifacts(ctx context.Context, q Querier, runID string) ([]RunArtifact, error) {
	rows, err := q.Query(ctx, `SELECT `+artifactColumns+` FROM run_artifacts WHERE run_id=$1 ORDER BY path`, runID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[RunArtifact])
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []RunArtifact{}
	}
	return out, nil
}
