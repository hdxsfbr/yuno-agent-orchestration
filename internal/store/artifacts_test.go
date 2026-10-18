package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// insertArtifact stores one run_artifacts row by hand.
func insertArtifact(t *testing.T, q Querier, a RunArtifact) error {
	t.Helper()
	_, err := q.Exec(context.Background(), `INSERT INTO run_artifacts (`+artifactColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		a.ID, a.RunID, a.NodeID, a.Visit, a.Path, a.Size, a.SHA256, a.ContentType, a.Generated, a.CreatedAt, a.UpdatedAt)
	return err
}

// sameArtifact compares two rows, the times by instant (a time read back carries the
// session's zone).
func sameArtifact(a, b RunArtifact) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) || !a.UpdatedAt.Equal(b.UpdatedAt) {
		return false
	}
	a.CreatedAt, a.UpdatedAt, b.CreatedAt, b.UpdatedAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	return a == b
}

// DESIGN 5.25 rule 4: ListRunArtifacts returns the run's rows ordered by path, an empty
// slice (not nil) for a run without any, never another run's; a path is unique within a
// run only; the rows go with the run.
func TestListRunArtifacts(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := w9Workflow(t, pool)
	run := w9MustRun(t, pool, wf, nil)
	other := w9MustRun(t, pool, wf, nil)

	if got, err := ListRunArtifacts(ctx, pool, run.ID); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("a run without artifacts: %#v, %v; want an empty slice", got, err)
	}

	at := time.Now().UTC().Truncate(time.Microsecond)
	summary := RunArtifact{ID: NewID(), RunID: run.ID, NodeID: "writer", Visit: 2, Path: "reports/summary.md", Size: 3 << 30,
		SHA256: sha1Hex + sha1Hex[:24], ContentType: "text/markdown", CreatedAt: at, UpdatedAt: at.Add(time.Minute)}
	diff := RunArtifact{ID: NewID(), RunID: run.ID, NodeID: "coder", Visit: 1, Path: "diff.patch", Size: 812,
		SHA256: sha2Hex + sha2Hex[:24], ContentType: "text/x-patch", Generated: true, CreatedAt: at, UpdatedAt: at}
	elsewhere := RunArtifact{ID: NewID(), RunID: other.ID, NodeID: "writer", Visit: 1, Path: "diff.patch", Size: 1,
		SHA256: sha1Hex, ContentType: "text/x-patch", CreatedAt: at, UpdatedAt: at}
	// Inserted out of path order, with the same path in another run.
	for _, a := range []RunArtifact{summary, diff, elsewhere} {
		mustDo(t, insertArtifact(t, pool, a))
	}

	got, err := ListRunArtifacts(ctx, pool, run.ID)
	if err != nil || len(got) != 2 {
		t.Fatalf("artifacts = %+v, %v; want 2", got, err)
	}
	if !sameArtifact(got[0], diff) || !sameArtifact(got[1], summary) {
		t.Errorf("artifacts = %+v\nwant [%+v %+v] ordered by path", got, diff, summary)
	}

	// The contract's names.
	raw, err := json.Marshal(got[1])
	mustDo(t, err)
	var fields map[string]any
	mustDo(t, json.Unmarshal(raw, &fields))
	for _, name := range []string{"id", "run_id", "node_id", "visit", "path", "size", "sha256", "content_type", "generated", "created_at", "updated_at"} {
		if _, ok := fields[name]; !ok {
			t.Errorf("artifact JSON lacks %q: %s", name, raw)
		}
	}

	// A path is unique within a run (23505), not across runs.
	dup := diff
	dup.ID = NewID()
	var pgErr *pgconn.PgError
	if err := insertArtifact(t, pool, dup); !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("second row at diff.patch: %v, want a unique violation", err)
	}

	// The rows go with their run.
	mustDo(t, execSQL(ctx, pool, `DELETE FROM runs WHERE id='`+run.ID+`'`))
	if got, err := ListRunArtifacts(ctx, pool, run.ID); err != nil || len(got) != 0 {
		t.Errorf("after the run was deleted: %+v, %v", got, err)
	}
	if got, err := ListRunArtifacts(ctx, pool, other.ID); err != nil || len(got) != 1 {
		t.Errorf("the other run's artifacts: %+v, %v", got, err)
	}
}
