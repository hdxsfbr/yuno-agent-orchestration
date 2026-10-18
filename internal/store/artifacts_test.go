package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
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

// artifactAt is a row of run at path, built the way the collector builds one.
func artifactAt(run *Run, path, sha string, size int64, at time.Time) RunArtifact {
	return RunArtifact{ID: NewID(), RunID: run.ID, NodeID: "writer", Visit: 1, Path: path, Size: size, SHA256: sha,
		ContentType: "text/plain; charset=utf-8", CreatedAt: at, UpdatedAt: at}
}

// finishRun marks a run ended in the given status at finishedAt.
func finishRun(t *testing.T, pool *pgxpool.Pool, id, status string, finishedAt time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `UPDATE runs SET status=$2, finished_at=$3 WHERE id=$1`, id, status, finishedAt)
	mustDo(t, err)
}

// DESIGN 5.25 rule 2 (the room): ArtifactTotals counts the run's rows and adds their
// sizes, zero for a run without any, and never counts another run's.
func TestArtifactTotals(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := w9Workflow(t, pool)
	run, other := w9MustRun(t, pool, wf, nil), w9MustRun(t, pool, wf, nil)

	if files, size, err := ArtifactTotals(ctx, pool, run.ID); err != nil || files != 0 || size != 0 {
		t.Fatalf("a run without artifacts: %d files, %d bytes, %v", files, size, err)
	}
	at := time.Now().UTC()
	for _, a := range []RunArtifact{
		artifactAt(run, "a.txt", sha1Hex, 10, at),
		artifactAt(run, "dir/b.txt", sha2Hex, 3<<30, at), // past 32 bits: the sum is a bigint
		artifactAt(other, "a.txt", sha1Hex, 999, at),
	} {
		mustDo(t, insertArtifact(t, pool, a))
	}
	if files, size, err := ArtifactTotals(ctx, pool, run.ID); err != nil || files != 2 || size != 3<<30+10 {
		t.Fatalf("totals = %d files, %d bytes, %v; want 2 and %d", files, size, err, int64(3<<30+10))
	}
}

// DESIGN 5.25 rule 2 (UpsertRunArtifact): no row at the path inserts one; the same sha256
// changes nothing, whatever else the new row says (the first visit's node_id and visit
// stay); a different sha256 updates the row in place (the same id and created_at, the
// new node_id, visit, size, sha256, content_type and updated_at). GetRunArtifact finds a
// row only through its own run.
func TestUpsertRunArtifact(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := w9Workflow(t, pool)
	run, other := w9MustRun(t, pool, wf, nil), w9MustRun(t, pool, wf, nil)
	t0 := time.Now().UTC().Truncate(time.Microsecond)

	first := artifactAt(run, "reports/out.md", sha1Hex+sha1Hex[:24], 120, t0)
	first.ContentType = "text/markdown"
	got, changed, err := UpsertRunArtifact(ctx, pool, first)
	if err != nil || !changed || !sameArtifact(*got, first) {
		t.Fatalf("insert = %+v, changed %v, %v; want %+v", got, changed, err, first)
	}

	// The same file collected by a later visit: untouched, the stored row is returned.
	again := first
	again.ID, again.NodeID, again.Visit, again.UpdatedAt = NewID(), "reviewer", 3, t0.Add(time.Hour)
	got, changed, err = UpsertRunArtifact(ctx, pool, again)
	if err != nil || changed || !sameArtifact(*got, first) {
		t.Fatalf("same sha = %+v, changed %v, %v; want the stored row %+v", got, changed, err, first)
	}

	// A changed file: the same id, the new content's facts.
	next := RunArtifact{ID: NewID(), RunID: run.ID, NodeID: "reviewer", Visit: 2, Path: "reports/out.md", Size: 250,
		SHA256: sha2Hex + sha2Hex[:24], ContentType: "text/plain; charset=utf-8", CreatedAt: t0.Add(2 * time.Hour), UpdatedAt: t0.Add(2 * time.Hour)}
	got, changed, err = UpsertRunArtifact(ctx, pool, next)
	want := next
	want.ID, want.CreatedAt = first.ID, first.CreatedAt
	if err != nil || !changed || !sameArtifact(*got, want) {
		t.Fatalf("update = %+v, changed %v, %v; want %+v", got, changed, err, want)
	}
	if rows, _ := ListRunArtifacts(ctx, pool, run.ID); len(rows) != 1 || !sameArtifact(rows[0], want) {
		t.Fatalf("rows after the update: %+v", rows)
	}

	// The generated flag follows the file that took the path.
	own := next
	own.SHA256, own.Generated = sha1Hex+sha1Hex[:24], true
	if got, changed, err = UpsertRunArtifact(ctx, pool, own); err != nil || !changed || !got.Generated || got.ID != first.ID {
		t.Fatalf("generated update = %+v, %v, %v", got, changed, err)
	}

	// Reading: through the run that owns the row, by id and by path; another run, an
	// unknown id and an unknown path are ErrNotFound.
	if a, err := GetRunArtifact(ctx, pool, run.ID, first.ID); err != nil || a.Path != "reports/out.md" {
		t.Fatalf("GetRunArtifact = %+v, %v", a, err)
	}
	if a, err := GetRunArtifactByPath(ctx, pool, run.ID, "reports/out.md"); err != nil || a.ID != first.ID {
		t.Fatalf("GetRunArtifactByPath = %+v, %v", a, err)
	}
	for name, call := range map[string]func() (*RunArtifact, error){
		"another run's id": func() (*RunArtifact, error) { return GetRunArtifact(ctx, pool, other.ID, first.ID) },
		"an unknown id":    func() (*RunArtifact, error) { return GetRunArtifact(ctx, pool, run.ID, "no-such-id") },
		"an unknown path":  func() (*RunArtifact, error) { return GetRunArtifactByPath(ctx, pool, run.ID, "nope.md") },
		"another run's path": func() (*RunArtifact, error) {
			return GetRunArtifactByPath(ctx, pool, other.ID, "reports/out.md")
		},
	} {
		if a, err := call(); !errors.Is(err, ErrNotFound) || a != nil {
			t.Errorf("%s = %+v, %v; want ErrNotFound", name, a, err)
		}
	}
}

// DESIGN 5.25 rule 3 (DeleteGeneratedArtifact): an empty patch removes the run's generated
// diff.patch and returns the row for its file; a diff.patch the agent wrote is never
// touched; a run without one answers nil; another run's row stays.
func TestDeleteGeneratedArtifact(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := w9Workflow(t, pool)
	run, agentRun, other := w9MustRun(t, pool, wf, nil), w9MustRun(t, pool, wf, nil), w9MustRun(t, pool, wf, nil)
	at := time.Now().UTC().Truncate(time.Microsecond)

	generated := artifactAt(run, "diff.patch", sha1Hex, 40, at)
	generated.Generated, generated.ContentType = true, "text/x-patch"
	agents := artifactAt(agentRun, "diff.patch", sha1Hex, 41, at)
	notDiff := artifactAt(run, "notes/diff.patch", sha2Hex, 7, at)
	notDiff.Generated = true // only the top-level diff.patch is the platform's
	theirs := artifactAt(other, "diff.patch", sha1Hex, 42, at)
	theirs.Generated = true
	for _, a := range []RunArtifact{generated, agents, notDiff, theirs} {
		mustDo(t, insertArtifact(t, pool, a))
	}

	if got, err := DeleteGeneratedArtifact(ctx, pool, agentRun.ID); err != nil || got != nil {
		t.Fatalf("the agent's own diff.patch: %+v, %v; want nil", got, err)
	}
	got, err := DeleteGeneratedArtifact(ctx, pool, run.ID)
	if err != nil || got == nil || !sameArtifact(*got, generated) {
		t.Fatalf("generated diff.patch: %+v, %v; want %+v", got, err, generated)
	}
	if got, err := DeleteGeneratedArtifact(ctx, pool, run.ID); err != nil || got != nil {
		t.Fatalf("a second call: %+v, %v; want nil", got, err)
	}
	for _, c := range []struct {
		run  *Run
		want int
	}{{run, 1}, {agentRun, 1}, {other, 1}} {
		if rows, _ := ListRunArtifacts(ctx, pool, c.run.ID); len(rows) != c.want {
			t.Errorf("run %s keeps %d rows, want %d", c.run.ID, len(rows), c.want)
		}
	}
}

// DESIGN 5.25 rule 6 (retention): ListExpiredArtifactRuns returns each run that has
// artifact rows and finished before the cutoff, once; a run finished after it, a run that
// has not finished (whatever its age) and a run without rows are not listed. DeleteRunArtifacts
// removes one run's rows and says how many.
func TestListExpiredArtifactRunsAndDeleteRunArtifacts(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := w9Workflow(t, pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	old := w9MustRun(t, pool, wf, nil)
	recent := w9MustRun(t, pool, wf, nil)
	live := w9MustRun(t, pool, wf, nil)
	rowless := w9MustRun(t, pool, wf, nil)
	finishRun(t, pool, old.ID, RunSucceeded, now.AddDate(0, 0, -31))
	finishRun(t, pool, recent.ID, RunFailed, now.AddDate(0, 0, -1))
	finishRun(t, pool, rowless.ID, RunCancelled, now.AddDate(0, 0, -90))
	// live: queued, finished_at null, created long ago.
	mustDo(t, execSQL(ctx, pool, `UPDATE runs SET created_at=now() - interval '90 days' WHERE id='`+live.ID+`'`))
	for _, r := range []*Run{old, recent, live} {
		mustDo(t, insertArtifact(t, pool, artifactAt(r, "a.txt", sha1Hex, 5, now)))
		mustDo(t, insertArtifact(t, pool, artifactAt(r, "b.txt", sha2Hex, 6, now)))
	}

	ids, err := ListExpiredArtifactRuns(ctx, pool, now.AddDate(0, 0, -30))
	if err != nil || len(ids) != 1 || ids[0] != old.ID {
		t.Fatalf("expired = %v, %v; want [%s]", ids, err, old.ID)
	}
	if ids, err = ListExpiredArtifactRuns(ctx, pool, now.AddDate(0, 0, -100)); err != nil || len(ids) != 0 {
		t.Fatalf("nothing is that old: %v, %v", ids, err)
	}
	if ids, err = ListExpiredArtifactRuns(ctx, pool, now); err != nil || len(ids) != 2 {
		t.Fatalf("both finished runs with rows: %v, %v", ids, err)
	}

	if n, err := DeleteRunArtifacts(ctx, pool, old.ID); err != nil || n != 2 {
		t.Fatalf("DeleteRunArtifacts = %d, %v; want 2", n, err)
	}
	if n, err := DeleteRunArtifacts(ctx, pool, old.ID); err != nil || n != 0 {
		t.Fatalf("a second call = %d, %v; want 0", n, err)
	}
	if rows, _ := ListRunArtifacts(ctx, pool, recent.ID); len(rows) != 2 {
		t.Errorf("the other runs' rows were touched: %d", len(rows))
	}
}
