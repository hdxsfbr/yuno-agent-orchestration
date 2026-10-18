package store

import (
	"context"
	"errors"
	"time"

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

// GeneratedArtifactPath is the path of the platform's own diff of a project run's
// checkout (DESIGN 5.25 rule 3): the one artifact a row can carry generated = true at. An
// empty diff leaves an earlier generated row in place, so nothing deletes it but the
// run's retention.
const GeneratedArtifactPath = "diff.patch"

// ArtifactTotals is how many files a run has stored and their sizes added up, the room
// the caps of DESIGN 5.25 rule 2 are measured against (read once before a collection).
func ArtifactTotals(ctx context.Context, q Querier, runID string) (files int, bytes int64, err error) {
	err = q.QueryRow(ctx, `SELECT count(*), COALESCE(sum(size), 0)::bigint FROM run_artifacts WHERE run_id=$1`, runID).Scan(&files, &bytes)
	return files, bytes, err
}

// GetRunArtifact returns one artifact of the run; ErrNotFound when the id is not this
// run's (another run's artifact is never readable through this run's path).
func GetRunArtifact(ctx context.Context, q Querier, runID, id string) (*RunArtifact, error) {
	return oneRunArtifact(ctx, q, `SELECT `+artifactColumns+` FROM run_artifacts WHERE run_id=$1 AND id=$2`, runID, id)
}

// GetRunArtifactByPath returns the run's artifact at path; ErrNotFound when there is none.
func GetRunArtifactByPath(ctx context.Context, q Querier, runID, path string) (*RunArtifact, error) {
	return oneRunArtifact(ctx, q, `SELECT `+artifactColumns+` FROM run_artifacts WHERE run_id=$1 AND path=$2`, runID, path)
}

func oneRunArtifact(ctx context.Context, q Querier, sql string, args ...any) (*RunArtifact, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[RunArtifact])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// UpsertRunArtifact stores a collected file's row (DESIGN 5.25 rule 2): inserted when the
// run has no row at a.Path; untouched when the row's sha256 equals a.SHA256 (the row as
// stored is returned, changed false); otherwise updated in place, keeping the row's id
// and created_at and taking node_id, visit, size, sha256, content_type, generated and
// updated_at from a. It returns the row as stored and whether it changed. The file
// behind the row is the caller's: it is renamed to <ARTIFACT_DIR>/<run id>/<row id>
// before this is called, so a row never points at a partial file.
func UpsertRunArtifact(ctx context.Context, q Querier, a RunArtifact) (*RunArtifact, bool, error) {
	got, err := oneRunArtifact(ctx, q, `INSERT INTO run_artifacts (`+artifactColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (run_id, path) DO UPDATE SET node_id=EXCLUDED.node_id, visit=EXCLUDED.visit, size=EXCLUDED.size,
			sha256=EXCLUDED.sha256, content_type=EXCLUDED.content_type, generated=EXCLUDED.generated,
			updated_at=EXCLUDED.updated_at
		WHERE run_artifacts.sha256 <> EXCLUDED.sha256
		RETURNING `+artifactColumns,
		a.ID, a.RunID, a.NodeID, a.Visit, a.Path, a.Size, a.SHA256, a.ContentType, a.Generated, a.CreatedAt, a.UpdatedAt)
	if errors.Is(err, ErrNotFound) {
		// The conflict's WHERE held back the update: the same file is already stored.
		got, err = GetRunArtifactByPath(ctx, q, a.RunID, a.Path)
		return got, false, err
	}
	if err != nil {
		return nil, false, err
	}
	return got, true, nil
}

// ListExpiredArtifactRuns returns the ids of the runs that have artifact rows and
// finished before the given time (DESIGN 5.25 rule 6); a run that has not finished
// never expires.
func ListExpiredArtifactRuns(ctx context.Context, q Querier, before time.Time) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT a.run_id FROM run_artifacts a JOIN runs r ON r.id = a.run_id
		WHERE r.finished_at < $1 ORDER BY a.run_id`, before)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// DeleteRunArtifacts removes every artifact row of a run and returns how many (the
// retention sweep deletes rows first, then the run's directory).
func DeleteRunArtifacts(ctx context.Context, q Querier, runID string) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM run_artifacts WHERE run_id=$1`, runID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
