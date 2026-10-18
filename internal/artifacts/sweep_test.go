package artifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yuno/internal/store"
)

// sweepRun adds a run to the fixture's database in a status, finished at finishedAt when
// set, with the given files stored the way a collection leaves them (rows and files).
func (f *fixture) sweepRun(status string, finishedAt *time.Time, files map[string]string) string {
	f.t.Helper()
	ctx := context.Background()
	wf := &store.Workflow{Name: "sweep " + store.NewID(), Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	must(f.t, store.CreateWorkflow(ctx, f.pool, wf))
	run := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: store.TriggerManual, Status: status}
	must(f.t, store.CreateRun(ctx, f.pool, run))
	if finishedAt != nil {
		_, err := f.pool.Exec(ctx, `UPDATE runs SET finished_at=$2 WHERE id=$1`, run.ID, *finishedAt)
		must(f.t, err)
	}
	for rel, content := range files {
		a := store.RunArtifact{ID: store.NewID(), RunID: run.ID, NodeID: "writer", Visit: 1, Path: rel, Size: int64(len(content)),
			SHA256: sum(content), ContentType: "text/plain", CreatedAt: f.clock, UpdatedAt: f.clock}
		_, _, err := store.UpsertRunArtifact(ctx, f.pool, a)
		must(f.t, err)
		must(f.t, os.MkdirAll(filepath.Join(f.c.Dir, run.ID), 0o755))
		must(f.t, os.WriteFile(filepath.Join(f.c.Dir, run.ID, a.ID), []byte(content), 0o644))
	}
	return run.ID
}

func (f *fixture) rowCount(runID string) int {
	f.t.Helper()
	files, _, err := store.ArtifactTotals(context.Background(), f.pool, runID)
	must(f.t, err)
	return files
}

func (f *fixture) dirEntries(runID string) []string {
	f.t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.c.Dir, runID))
	if os.IsNotExist(err) {
		return nil
	}
	must(f.t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func (f *fixture) dirExists(name string) bool {
	_, err := os.Lstat(filepath.Join(f.c.Dir, name))
	return err == nil
}

// DESIGN 5.25 rule 6 (T5.25.6): a run finished 31 days ago loses its rows and then its
// directory, one finished 29 days or a day ago keeps both, a run that has not finished
// keeps its artifacts however old; a directory with no rows whose run is gone, or
// finished, is removed, and one whose run is live (between a file and its row) is kept.
// What is not a run directory of the store (a link, a name that is no run id, a loose
// file) is left alone. Sweep returns the rows it deleted and logs one line with the counts.
func TestSweep(t *testing.T) {
	f := newFixture(t)
	var logs bytes.Buffer
	f.c.Log = slog.New(slog.NewTextHandler(&logs, nil))
	now := time.Now().UTC()
	ago := func(days int) *time.Time { t := now.AddDate(0, 0, -days); return &t }

	old := f.sweepRun(store.RunSucceeded, ago(31), map[string]string{"a.md": "a", "b/c.md": "bc"})
	failedOld := f.sweepRun(store.RunFailed, ago(45), map[string]string{"x.txt": "x"})
	edge := f.sweepRun(store.RunSucceeded, ago(29), map[string]string{"a.md": "a"})
	yesterday := f.sweepRun(store.RunSucceeded, ago(1), map[string]string{"a.md": "a"})
	live := f.sweepRun(store.RunRunning, nil, map[string]string{"a.md": "a"})
	queuedOld := f.sweepRun(store.RunQueued, nil, nil) // created long ago, not finished, no rows

	goneRun := store.NewID() // a directory whose run was deleted
	must(t, os.MkdirAll(filepath.Join(f.c.Dir, goneRun), 0o755))
	must(t, os.WriteFile(filepath.Join(f.c.Dir, goneRun, store.NewID()), []byte("orphan"), 0o644))
	finishedNoRows := f.sweepRun(store.RunFailed, ago(2), nil)
	must(t, os.MkdirAll(filepath.Join(f.c.Dir, finishedNoRows), 0o755))
	must(t, os.WriteFile(filepath.Join(f.c.Dir, finishedNoRows, store.NewID()+".tmp"), []byte("half"), 0o644))
	liveNoRows := f.sweepRun(store.RunRunning, nil, nil)
	must(t, os.MkdirAll(filepath.Join(f.c.Dir, liveNoRows), 0o755))
	must(t, os.WriteFile(filepath.Join(f.c.Dir, liveNoRows, store.NewID()), []byte("renamed, its row not written yet"), 0o644))
	_ = queuedOld

	// Not the store's: a volume's own directory, a loose file, a link to a directory
	// somewhere else (which must neither be followed nor removed).
	must(t, os.MkdirAll(filepath.Join(f.c.Dir, "lost+found"), 0o755))
	must(t, os.WriteFile(filepath.Join(f.c.Dir, ".probe-leftover"), []byte("ok"), 0o644))
	elsewhere := t.TempDir()
	must(t, os.WriteFile(filepath.Join(elsewhere, "precious"), []byte("keep"), 0o644))
	linkName := store.NewID()
	must(t, os.Symlink(elsewhere, filepath.Join(f.c.Dir, linkName)))

	n, err := f.c.Sweep(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 { // old: 2 rows, failedOld: 1
		t.Errorf("Sweep returned %d rows, want 3", n)
	}
	for _, id := range []string{old, failedOld} {
		if f.rowCount(id) != 0 || f.dirExists(id) {
			t.Errorf("run %s was not swept: %d rows, dir %v", id, f.rowCount(id), f.dirEntries(id))
		}
	}
	for _, id := range []string{edge, yesterday, live} {
		if f.rowCount(id) != 1 || len(f.dirEntries(id)) != 1 {
			t.Errorf("run %s lost its artifacts: %d rows, dir %v", id, f.rowCount(id), f.dirEntries(id))
		}
	}
	for _, id := range []string{goneRun, finishedNoRows} {
		if f.dirExists(id) {
			t.Errorf("orphan directory %s was kept", id)
		}
	}
	if !f.dirExists(liveNoRows) {
		t.Error("the live run's directory was removed between its file and its row")
	}
	for _, name := range []string{"lost+found", ".probe-leftover", linkName} {
		if !f.dirExists(name) {
			t.Errorf("%s was removed", name)
		}
	}
	if b, err := os.ReadFile(filepath.Join(elsewhere, "precious")); err != nil || string(b) != "keep" {
		t.Errorf("the link's target was touched: %q, %v", b, err)
	}
	if line := logs.String(); !strings.Contains(line, `msg="artifacts swept"`) || !strings.Contains(line, "expired_runs=2") ||
		!strings.Contains(line, "rows=3") || !strings.Contains(line, "orphan_dirs=2") {
		t.Errorf("log = %q", line)
	}

	// A second pass has nothing to do: zero, and no log line.
	logs.Reset()
	if n, err := f.c.Sweep(context.Background(), now); err != nil || n != 0 {
		t.Fatalf("second pass = %d, %v", n, err)
	}
	if logs.Len() != 0 {
		t.Errorf("a pass that removed nothing logged %q", logs.String())
	}
}

// DESIGN 5.25 rule 6: ARTIFACT_TTL_DAYS=0 keeps artifacts forever and removes orphans
// only; shortening the TTL applies at the next sweep; the retention counts from
// finished_at, not from when the file was collected.
func TestSweepTTL(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC()
	ago := func(days int) *time.Time { t := now.AddDate(0, 0, -days); return &t }
	ancient := f.sweepRun(store.RunSucceeded, ago(400), map[string]string{"a.md": "a"})
	week := f.sweepRun(store.RunSucceeded, ago(8), map[string]string{"a.md": "a"})
	orphan := store.NewID()
	must(t, os.MkdirAll(filepath.Join(f.c.Dir, orphan), 0o755))

	f.c.TTL = 0
	if n, err := f.c.Sweep(context.Background(), now); err != nil || n != 0 {
		t.Fatalf("TTL 0: %d, %v", n, err)
	}
	if f.rowCount(ancient) != 1 || f.rowCount(week) != 1 || len(f.dirEntries(ancient)) != 1 {
		t.Error("TTL 0 removed artifacts")
	}
	if f.dirExists(orphan) {
		t.Error("TTL 0 kept an orphan directory")
	}

	f.c.TTL = 7 * 24 * time.Hour // shortened in .env
	n, err := f.c.Sweep(context.Background(), now)
	if err != nil || n != 2 {
		t.Fatalf("TTL 7 days: %d, %v; want 2", n, err)
	}
	if f.rowCount(ancient) != 0 || f.rowCount(week) != 0 || f.dirExists(ancient) || f.dirExists(week) {
		t.Error("the shortened TTL was not applied")
	}
}

// DESIGN 5.25 Consistency: a crash between a file's write and its row leaves a file the
// sweep removes, also in a directory that has rows, once it is old enough not to be a
// collection in progress; a row's own file and a fresh copy are never touched.
func TestSweepRemovesStrayFilesOfPopulatedDirectories(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC()
	id := f.sweepRun(store.RunRunning, nil, map[string]string{"a.md": "a", "b.md": "b"})
	dir := filepath.Join(f.c.Dir, id)
	write := func(name string, age time.Duration) {
		p := filepath.Join(dir, name)
		must(t, os.WriteFile(p, []byte("x"), 0o644))
		at := now.Add(-age)
		must(t, os.Chtimes(p, at, at))
	}
	write(store.NewID(), 3*time.Hour)        // renamed, row never written
	write(store.NewID()+".tmp", 2*time.Hour) // a copy that never finished
	fresh := store.NewID() + ".tmp"          // a collection in progress
	write(fresh, time.Minute)

	if n, err := f.c.Sweep(context.Background(), now); err != nil || n != 0 {
		t.Fatalf("Sweep = %d, %v", n, err)
	}
	left := f.dirEntries(id)
	if len(left) != 3 || f.rowCount(id) != 2 {
		t.Fatalf("left %v, %d rows; want the two rows' files and the fresh copy", left, f.rowCount(id))
	}
	found := false
	for _, e := range left {
		found = found || e == fresh
	}
	if !found {
		t.Errorf("the fresh copy %s was removed: %v", fresh, left)
	}
	for _, a := range f.rows() {
		if _, ok := f.stored(a); !ok {
			t.Errorf("the file of %s was removed", a.Path)
		}
	}
}

// DESIGN 5.25 rule 6 and Failure handling: a failed sweep is an error the sweeper logs
// and retries; the rest of the pass still runs. With the database gone the TTL half
// fails and the orphan half cannot tell a run from none, so nothing is removed.
func TestSweepFailureLeavesFilesAlone(t *testing.T) {
	f := newFixture(t)
	closed, err := store.Open(context.Background(), os.Getenv("DATABASE_URL"))
	must(t, err)
	closed.Close()
	orphan := store.NewID()
	must(t, os.MkdirAll(filepath.Join(f.c.Dir, orphan), 0o755))
	broken := *f.c
	broken.Pool = closed
	if _, err := broken.Sweep(context.Background(), time.Now()); err == nil {
		t.Fatal("a sweep without a database reported success")
	}
	if !f.dirExists(orphan) {
		t.Error("a directory was removed on a database error")
	}

	// A store directory that does not exist is an empty store, not an error.
	empty := *f.c
	empty.Dir = filepath.Join(t.TempDir(), "never-created")
	if n, err := empty.Sweep(context.Background(), time.Now()); err != nil || n != 0 {
		t.Errorf("missing directory: %d, %v", n, err)
	}
}
