package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/engine"
	"yuno/internal/project"
	"yuno/internal/project/projecttest"
	"yuno/internal/store"
)

// logged is a run log line the collector wrote through LogEvent.
type logged struct{ level, msg string }

// emitted is an SSE event the collector published through Emit.
type emitted struct {
	runID, event string
	payload      map[string]any
}

// fixture is one run with its workspace, its store and a collector that records what it
// logs and publishes.
type fixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	c    *Collector
	run  *store.Run
	ws   string // the agent's workspace directory

	mu     sync.Mutex
	lines  []logged
	events []emitted
	clock  time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := needDB(t)
	ctx := context.Background()
	wf := &store.Workflow{Name: "artifacts " + store.NewID(), Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	if err := store.CreateWorkflow(ctx, pool, wf); err != nil {
		t.Fatal(err)
	}
	run := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: store.TriggerManual}
	if err := store.CreateRun(ctx, pool, run); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, pool: pool, run: run, ws: filepath.Join(t.TempDir(), "workspace-agent"),
		clock: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	f.c = &Collector{Dir: filepath.Join(t.TempDir(), "store"), Pool: pool, Log: slog.New(slog.DiscardHandler),
		Emit: func(_ context.Context, runID, event string, payload map[string]any) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.events = append(f.events, emitted{runID, event, payload})
		},
		LogEvent: func(_ context.Context, runID, level, msg string, _ ...any) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if runID != f.run.ID {
				t.Errorf("a line for run %q, want %q", runID, f.run.ID)
			}
			f.lines = append(f.lines, logged{level, msg})
		},
		Now: func() time.Time { return f.clock },
		TTL: 30 * 24 * time.Hour,
	}
	must(t, os.MkdirAll(f.c.Dir, 0o755))
	must(t, os.MkdirAll(f.runPath(), 0o755))
	return f
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// runPath is runs/<run id> of the workspace.
func (f *fixture) runPath() string { return filepath.Join(f.ws, "runs", f.run.ID) }

// out writes a file under out/ (creating directories) and returns its path.
func (f *fixture) out(rel, content string) string {
	f.t.Helper()
	p := filepath.Join(f.runPath(), "out", rel)
	must(f.t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(f.t, os.WriteFile(p, []byte(content), 0o644))
	return p
}

// collect ends a visit of node (its visit number n) and collects.
func (f *fixture) collect(node string, visit int) {
	f.t.Helper()
	f.collectWith(engine.ArtifactInput{Node: &store.RunNode{RunID: f.run.ID, NodeID: node, Visit: visit}})
}

func (f *fixture) collectWith(in engine.ArtifactInput) {
	f.t.Helper()
	in.Workspace, in.Run = f.ws, f.run
	f.c.Collect(context.Background(), in)
}

// rows are the run's artifacts keyed by path.
func (f *fixture) rows() map[string]store.RunArtifact {
	f.t.Helper()
	list, err := store.ListRunArtifacts(context.Background(), f.pool, f.run.ID)
	must(f.t, err)
	out := make(map[string]store.RunArtifact, len(list))
	for _, a := range list {
		out[a.Path] = a
	}
	return out
}

// stored is the content of the file a row points at; ok is false when it is missing.
func (f *fixture) stored(a store.RunArtifact) (string, bool) {
	b, err := os.ReadFile(filepath.Join(f.c.Dir, f.run.ID, a.ID))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// takeLines returns the lines logged since the last call.
func (f *fixture) takeLines() []logged {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.lines
	f.lines = nil
	return out
}

func (f *fixture) takeEvents() []emitted {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.events
	f.events = nil
	return out
}

// noLines fails when the collector logged anything since the last call.
func (f *fixture) noLines() {
	f.t.Helper()
	if lines := f.takeLines(); len(lines) != 0 {
		f.t.Fatalf("unexpected log lines: %+v", lines)
	}
}

// onlyLine returns the one line logged since the last call.
func (f *fixture) onlyLine() logged {
	f.t.Helper()
	lines := f.takeLines()
	if len(lines) != 1 {
		f.t.Fatalf("log lines = %+v, want exactly one", lines)
	}
	return lines[0]
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// leftovers lists the files of the run's store directory that are not named by a row:
// temporary copies and orphans.
func (f *fixture) leftovers() []string {
	f.t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.c.Dir, f.run.ID))
	if os.IsNotExist(err) {
		return nil
	}
	must(f.t, err)
	named := map[string]bool{}
	for _, a := range f.rows() {
		named[a.ID] = true
	}
	var out []string
	for _, e := range entries {
		if !named[e.Name()] {
			out = append(out, e.Name())
		}
	}
	return out
}

// lowerCaps sets the caps for one test and restores them afterwards.
func lowerCaps(t *testing.T, files int, total, file, patch int64) {
	t.Helper()
	oldFiles, oldTotal, oldFile, oldPatch := maxFiles, maxTotalBytes, maxFileBytes, maxPatchBytes
	maxFiles, maxTotalBytes, maxFileBytes, maxPatchBytes = files, total, file, patch
	t.Cleanup(func() { maxFiles, maxTotalBytes, maxFileBytes, maxPatchBytes = oldFiles, oldTotal, oldFile, oldPatch })
}

// DESIGN 5.25 rule 2 (T5.25.1): nested files under out/ are collected with their paths
// relative to out/, sizes, sha256 and content types, from the visit that ended; the
// store holds each under <run id>/<artifact id> with the content and mode 0644, no
// temporary copy is left, the SSE event follows with the run's count, and a clean
// collection logs nothing.
func TestCollectNestedFiles(t *testing.T) {
	f := newFixture(t)
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 24)
	files := map[string]string{
		"report.md":          "# Report\n\nAll good.\n",
		"data/results.csv":   "a,b\n1,2\n",
		"data/deep/x/y.json": `{"ok":true}`,
		"chart.png":          png,
		"page.html":          "<!doctype html><html><body>hi</body></html>",
		"notes/zero":         "",
		"café/ünï.txt":       "unicode names are fine\n",
	}
	for rel, content := range files {
		f.out(rel, content)
	}
	f.collect("writer", 2)

	rows := f.rows()
	if len(rows) != len(files) {
		t.Fatalf("%d rows, want %d: %+v", len(rows), len(files), rows)
	}
	wantTypes := map[string]string{
		"report.md": "text/markdown", "data/results.csv": "text/csv", "data/deep/x/y.json": "application/json",
		"chart.png": "image/png", "page.html": "text/html; charset=utf-8", "notes/zero": "text/plain; charset=utf-8",
		"café/ünï.txt": "text/plain; charset=utf-8",
	}
	for rel, content := range files {
		a, ok := rows[rel]
		if !ok {
			t.Errorf("no row for %q", rel)
			continue
		}
		if a.RunID != f.run.ID || a.NodeID != "writer" || a.Visit != 2 || a.Size != int64(len(content)) || a.SHA256 != sum(content) ||
			a.ContentType != wantTypes[rel] || a.Generated || !a.CreatedAt.Equal(f.clock) || !a.UpdatedAt.Equal(f.clock) {
			t.Errorf("row for %q = %+v", rel, a)
		}
		if got, ok := f.stored(a); !ok || got != content {
			t.Errorf("stored file of %q = %q (found %v), want %q", rel, got, ok, content)
		}
		if info, err := os.Stat(filepath.Join(f.c.Dir, f.run.ID, a.ID)); err != nil || info.Mode().Perm() != 0o644 {
			t.Errorf("stored file of %q: %v, %v; want mode 0644", rel, info, err)
		}
	}
	if left := f.leftovers(); len(left) != 0 {
		t.Errorf("files without a row: %v", left)
	}
	f.noLines()
	events := f.takeEvents()
	if len(events) != 1 || events[0].event != "run.artifacts" || events[0].runID != f.run.ID ||
		events[0].payload["run_id"] != f.run.ID || events[0].payload["count"] != len(files) {
		t.Errorf("events = %+v, want one run.artifacts with count %d", events, len(files))
	}
}

// DESIGN 5.25 rule 2 and edge cases (T5.25.1): links and special files inside out/ are
// skipped and never followed (a link to a file in the run, to a file outside the
// workspace and to a directory, and a named pipe, which would block a naive open), with
// one log line that names the first three.
func TestCollectSkipsLinksAndSpecialFiles(t *testing.T) {
	f := newFixture(t)
	secret := filepath.Join(t.TempDir(), "secret.txt")
	must(t, os.WriteFile(secret, []byte("top secret"), 0o600))
	f.out("real.txt", "real")
	f.out("sub/inner.txt", "inner")
	outDir := filepath.Join(f.runPath(), "out")
	must(t, os.Symlink("real.txt", filepath.Join(outDir, "a-link-to-real")))
	must(t, os.Symlink(secret, filepath.Join(outDir, "b-link-outside")))
	must(t, os.Symlink("sub", filepath.Join(outDir, "c-link-to-dir")))
	must(t, os.Symlink("../../..", filepath.Join(outDir, "d-link-up")))
	if err := syscall.Mkfifo(filepath.Join(outDir, "e-pipe"), 0o644); err != nil {
		t.Skipf("no named pipes here: %v", err)
	}
	f.collect("writer", 1)

	rows := f.rows()
	if len(rows) != 2 || rows["real.txt"].SHA256 != sum("real") || rows["sub/inner.txt"].SHA256 != sum("inner") {
		t.Fatalf("rows = %+v, want only real.txt and sub/inner.txt", rows)
	}
	line := f.onlyLine()
	want := "artifacts: 5 files not collected: a-link-to-real: a link; b-link-outside: a link; c-link-to-dir: a link; ..."
	if line.level != "info" || line.msg != want {
		t.Fatalf("line = %+v, want info %q", line, want)
	}
}

// DESIGN 5.25 rule 2 and edge cases (T5.25.1): out/, runs/<run id> and runs being links,
// or out/ a file, collect nothing and log `artifacts: out/ is not a plain directory`;
// what a link points at is never read. A missing out/, run directory or workspace
// collects nothing and logs nothing.
func TestCollectRefusesLinksAtEveryLevel(t *testing.T) {
	const notPlain = "artifacts: out/ is not a plain directory"
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "loot.txt"), []byte("loot"), 0o644))
	must(t, os.MkdirAll(filepath.Join(outside, "out"), 0o755))
	must(t, os.WriteFile(filepath.Join(outside, "out", "loot.txt"), []byte("loot"), 0o644))

	cases := map[string]func(f *fixture){
		"out is a link": func(f *fixture) {
			must(t, os.Symlink(outside, filepath.Join(f.runPath(), "out")))
		},
		"out is a link to a directory of the run": func(f *fixture) {
			must(t, os.MkdirAll(filepath.Join(f.runPath(), "elsewhere"), 0o755))
			must(t, os.WriteFile(filepath.Join(f.runPath(), "elsewhere", "x.txt"), []byte("x"), 0o644))
			must(t, os.Symlink("elsewhere", filepath.Join(f.runPath(), "out")))
		},
		"out is a file": func(f *fixture) {
			must(t, os.WriteFile(filepath.Join(f.runPath(), "out"), []byte("not a directory"), 0o644))
		},
		"the run directory is a link": func(f *fixture) {
			must(t, os.RemoveAll(f.runPath()))
			must(t, os.Symlink(outside, f.runPath()))
		},
		"runs is a link": func(f *fixture) {
			must(t, os.RemoveAll(filepath.Join(f.ws, "runs")))
			must(t, os.MkdirAll(filepath.Join(outside, f.run.ID), 0o755))
			must(t, os.Symlink(outside, filepath.Join(f.ws, "runs")))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			setup(f)
			f.collect("writer", 1)
			if rows := f.rows(); len(rows) != 0 {
				t.Fatalf("collected through a link: %+v", rows)
			}
			if line := f.onlyLine(); line.msg != notPlain {
				t.Fatalf("line = %+v, want %q", line, notPlain)
			}
			if events := f.takeEvents(); len(events) != 0 {
				t.Errorf("events = %+v", events)
			}
			if entries, _ := os.ReadDir(f.c.Dir); len(entries) != 0 {
				t.Errorf("the store got something: %v", entries)
			}
		})
	}

	t.Run("nothing to collect", func(t *testing.T) {
		f := newFixture(t)
		f.collect("writer", 1) // the run directory exists, out/ does not
		must(t, os.RemoveAll(filepath.Join(f.ws, "runs")))
		f.collect("writer", 2) // runs/ does not exist
		must(t, os.RemoveAll(f.ws))
		f.collect("writer", 3) // the workspace does not exist
		f.noLines()
		if rows, events := f.rows(), f.takeEvents(); len(rows) != 0 || len(events) != 0 {
			t.Fatalf("rows %+v events %+v", rows, events)
		}
	})
}

// DESIGN 5.25 units and rule 2 (T5.25.1): at most 200 files per run (the 201st in lexical
// order and later are skipped), a path of at most 512 bytes, a file of at most 20 MiB (a
// sparse 21 MiB file is skipped without being read), names the database or a log line
// cannot hold are skipped; each skip is in the one log line of the visit.
func TestCollectCapsOnFilesSizeAndPath(t *testing.T) {
	f := newFixture(t)
	for i := range 201 {
		f.out(fmt.Sprintf("f%03d.txt", i), "x")
	}
	f.collect("writer", 1)
	rows := f.rows()
	if len(rows) != 200 {
		t.Fatalf("%d rows, want 200", len(rows))
	}
	if _, ok := rows["f199.txt"]; !ok {
		t.Error("the 200th file was not collected")
	}
	if _, ok := rows["f200.txt"]; ok {
		t.Error("the 201st file was collected")
	}
	if line := f.onlyLine(); line.msg != "artifacts: 1 files not collected: f200.txt: past the 200 files cap" {
		t.Fatalf("line = %+v", line)
	}
	if events := f.takeEvents(); len(events) != 1 || events[0].payload["count"] != 200 {
		t.Fatalf("events = %+v", events)
	}

	// Another visit adds nothing: the run is full, and unchanged files stay unchanged.
	f.out("zzz-late.txt", "late")
	f.collect("writer", 2)
	if len(f.rows()) != 200 {
		t.Fatalf("the run grew past 200 files")
	}
	if line := f.onlyLine(); line.msg != "artifacts: 2 files not collected: f200.txt: past the 200 files cap; zzz-late.txt: past the 200 files cap" {
		t.Fatalf("line = %+v", line)
	}
	if events := f.takeEvents(); len(events) != 0 {
		t.Fatalf("nothing changed, events = %+v", events)
	}

	g := newFixture(t)
	big, err := os.Create(g.out("big.bin", ""))
	must(t, err)
	must(t, big.Truncate(21<<20))
	must(t, big.Close())
	atCap, err := os.Create(g.out("exactly-20-mib.bin", ""))
	must(t, err)
	must(t, atCap.Truncate(20<<20))
	must(t, atCap.Close())
	atLimit, overLimit := longPath('p', 512), longPath('q', 513)
	g.out(atLimit, "path at the limit")
	g.out(overLimit, "path over the limit")
	g.out("bad\nname.txt", "a newline in the name")
	g.out("ok.txt", "ok")
	g.collect("writer", 1)
	rows = g.rows()
	if _, ok := rows["big.bin"]; ok {
		t.Error("a 21 MiB file was collected")
	}
	if a, ok := rows["exactly-20-mib.bin"]; !ok || a.Size != 20<<20 {
		t.Errorf("a 20 MiB file: %+v, %v", a, ok)
	}
	if _, ok := rows[atLimit]; !ok {
		t.Error("a 512 byte path was refused")
	}
	if len(rows) != 3 { // 20 MiB, the 512 byte path, ok.txt
		t.Errorf("rows = %d", len(rows))
	}
	line := g.onlyLine()
	for _, want := range []string{"artifacts: 3 files not collected: ", "bad?name.txt: name is not plain text", "big.bin: over 20 MiB", overLimit[:100] + "...: path over 512 bytes"} {
		if !strings.Contains(line.msg, want) {
			t.Errorf("line %q lacks %q", line.msg, want)
		}
	}
	if left := g.leftovers(); len(left) != 0 {
		t.Errorf("a refused file left %v in the store", left)
	}
}

// longPath is a relative path of n bytes made of directories and a file name of one letter
// repeated (a file system refuses a single component over 255 bytes).
func longPath(letter byte, n int) string {
	var parts []string
	for n > 200 {
		parts = append(parts, strings.Repeat(string(letter), 200))
		n -= 201 // the component and its separator
	}
	parts = append(parts, strings.Repeat(string(letter), n))
	return strings.Join(parts, "/")
}

// DESIGN 5.25 units and rule 2 (T5.25.1): the run's room under 50 MiB is the sum of the
// stored sizes, across visits: a file past the remaining room is skipped (a smaller one
// after it still fits), a changed file takes the place of its old version in the sum, and
// the line names the cap. The cap is lowered here instead of writing 50 MiB.
func TestCollectTotalSizeCap(t *testing.T) {
	lowerCaps(t, 200, 1000, 20<<20, 10<<20)
	f := newFixture(t)
	f.out("a.txt", strings.Repeat("a", 400))
	f.out("b.txt", strings.Repeat("b", 400))
	f.out("c.txt", strings.Repeat("c", 400)) // 1200 > 1000
	f.out("d.txt", strings.Repeat("d", 100)) // 900: fits again
	f.collect("writer", 1)
	rows := f.rows()
	if len(rows) != 3 || rows["c.txt"].ID != "" || rows["d.txt"].Size != 100 {
		t.Fatalf("rows = %+v, want a, b and d", rows)
	}
	if line := f.onlyLine(); line.msg != "artifacts: 1 files not collected: c.txt: past the 1000 bytes cap" {
		t.Fatalf("line = %+v", line)
	}

	// Grown by a later visit: 900 - 400 + 450 = 950 fits; then 950 - 100 + 160 = 1010 does not,
	// and the old version of d stays.
	f.out("a.txt", strings.Repeat("A", 450))
	f.out("d.txt", strings.Repeat("D", 160))
	f.collect("reviewer", 1)
	rows = f.rows()
	if rows["a.txt"].Size != 450 || rows["d.txt"].Size != 100 || rows["d.txt"].SHA256 != sum(strings.Repeat("d", 100)) {
		t.Fatalf("rows = %+v", rows)
	}
	lines := f.takeLines()
	if len(lines) != 1 || !strings.Contains(lines[0].msg, "d.txt: past the 1000 bytes cap") {
		t.Fatalf("lines = %+v", lines)
	}
	if left := f.leftovers(); len(left) != 0 {
		t.Errorf("files without a row: %v", left)
	}
}

// DESIGN 5.25 rule 2 and edge cases (T5.25.2): the same file across visits is one row,
// the first visit's, and nothing is written or published again; a file a later agent
// changed updates its row in place (the same id, the new node, visit, size, sha256,
// content type and collection time, the old content replaced) and publishes the count; a
// file a later agent deleted keeps its row and its stored copy.
func TestCollectAcrossVisits(t *testing.T) {
	f := newFixture(t)
	f.out("report.md", "# v1\n")
	f.out("data.csv", "a,b\n")
	f.collect("coder", 1)
	first := f.rows()
	if len(first) != 2 {
		t.Fatalf("rows = %+v", first)
	}
	f.takeEvents()

	// Unchanged, twice: the same rows, the first visit's.
	f.clock = f.clock.Add(time.Hour)
	f.collect("reviewer", 1)
	f.collect("coder", 2)
	same := f.rows()
	for path, a := range first {
		if got := same[path]; got != a {
			t.Errorf("row %q changed: %+v -> %+v", path, a, got)
		}
	}
	if events := f.takeEvents(); len(events) != 0 {
		t.Errorf("unchanged files published %+v", events)
	}
	f.noLines()
	if left := f.leftovers(); len(left) != 0 {
		t.Errorf("an unchanged file left %v", left)
	}

	// Changed by the reviewer's visit 3: the row is updated in place.
	f.clock = f.clock.Add(time.Hour)
	f.out("report.md", "# v2, reviewed and a good deal longer\n")
	f.collect("reviewer", 3)
	rows := f.rows()
	got, old := rows["report.md"], first["report.md"]
	if got.ID != old.ID || got.NodeID != "reviewer" || got.Visit != 3 || got.SHA256 != sum("# v2, reviewed and a good deal longer\n") ||
		got.Size != int64(len("# v2, reviewed and a good deal longer\n")) || !got.CreatedAt.Equal(old.CreatedAt) || !got.UpdatedAt.Equal(f.clock) {
		t.Fatalf("updated row = %+v (was %+v)", got, old)
	}
	if rows["data.csv"] != first["data.csv"] {
		t.Errorf("the untouched file's row changed: %+v", rows["data.csv"])
	}
	if content, ok := f.stored(got); !ok || content != "# v2, reviewed and a good deal longer\n" {
		t.Errorf("stored content = %q (found %v)", content, ok)
	}
	if left := f.leftovers(); len(left) != 0 {
		t.Errorf("an update left %v", left)
	}
	if events := f.takeEvents(); len(events) != 1 || events[0].payload["count"] != 2 {
		t.Errorf("events = %+v, want one with count 2", events)
	}

	// A content type that follows the new content: a file renamed in kind keeps its path
	// but not its type.
	f.out("data.csv", "\x89PNG\r\n\x1a\n"+strings.Repeat("\x00", 8))
	f.collect("coder", 4)
	if a := f.rows()["data.csv"]; a.ContentType != "text/csv" || a.Visit != 4 { // by extension, still csv
		t.Errorf("data.csv = %+v", a)
	}

	// Deleted by a later agent: the row and the file stay (deletions do not flow).
	must(t, os.Remove(filepath.Join(f.runPath(), "out", "report.md")))
	f.collect("deployer", 1)
	after := f.rows()
	if after["report.md"] != rows["report.md"] {
		t.Errorf("the deleted file's row changed: %+v", after["report.md"])
	}
	if content, ok := f.stored(after["report.md"]); !ok || content != "# v2, reviewed and a good deal longer\n" {
		t.Errorf("the deleted file's copy = %q (found %v)", content, ok)
	}
}

// DESIGN 5.25 Consistency: the copy is renamed into the store before its row is written,
// so a row never names a partial file. When the row cannot be written (here: a visit number
// the integer column cannot hold) an insert removes its file, so nothing without a row is
// left behind; an update has already replaced the file, and the row still says the old
// sha, so the next collection writes the row again.
func TestCollectRowFailureLeavesNothingHalfDone(t *testing.T) {
	f := newFixture(t)
	f.out("a.txt", "first")
	f.collect("writer", 1<<40)
	if line := f.onlyLine(); line.level != "warn" || !strings.HasPrefix(line.msg, "artifacts: ") {
		t.Fatalf("line = %+v, want a warn line starting with `artifacts: `", line)
	}
	if rows := f.rows(); len(rows) != 0 {
		t.Fatalf("rows = %+v", rows)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.c.Dir, f.run.ID)); len(entries) != 0 {
		t.Fatalf("a failed insert left %v in the store", entries)
	}
	if events := f.takeEvents(); len(events) != 0 {
		t.Errorf("events = %+v", events)
	}

	f.collect("writer", 1)
	f.takeEvents()
	old := f.rows()["a.txt"]
	f.out("a.txt", "second")
	f.collect("writer", 1<<40)
	f.onlyLine()
	if got := f.rows()["a.txt"]; got != old {
		t.Fatalf("a failed update changed the row: %+v -> %+v", old, got)
	}
	if content, _ := f.stored(old); content != "second" {
		t.Fatalf("the stored copy = %q: the rename comes before the row", content)
	}
	if left := f.leftovers(); len(left) != 0 {
		t.Fatalf("files without a row: %v", left)
	}
	// The row says "first", the file says "second", the source says "second": the sha
	// differs, so the next collection brings the row up to date.
	f.collect("writer", 2)
	if got := f.rows()["a.txt"]; got.SHA256 != sum("second") || got.Visit != 2 || got.ID != old.ID {
		t.Fatalf("after the retry: %+v", got)
	}
}

// DESIGN 5.25 rule 2 and Failure handling: collection is best effort. A store that cannot
// be written, a database that is gone and a deadline that passed are each one warn line
// `artifacts: ...`, never a panic or an error to the visit; what was stored before the
// failure stays. A nil collector and an input without a run or a node do nothing.
func TestCollectFailuresAreLogLines(t *testing.T) {
	t.Run("the store directory cannot be written", func(t *testing.T) {
		f := newFixture(t)
		f.out("a.txt", "a")
		blocker := filepath.Join(t.TempDir(), "file")
		must(t, os.WriteFile(blocker, []byte("x"), 0o644))
		f.c.Dir = filepath.Join(blocker, "store") // a directory below a file
		f.collect("writer", 1)
		if line := f.onlyLine(); line.level != "warn" || !strings.HasPrefix(line.msg, "artifacts: ") {
			t.Fatalf("line = %+v", line)
		}
		if rows := f.rows(); len(rows) != 0 {
			t.Fatalf("rows = %+v", rows)
		}
	})
	t.Run("the deadline passes", func(t *testing.T) {
		f := newFixture(t)
		f.out("a.txt", "a")
		f.collect("writer", 1)
		stored := f.rows()
		f.takeEvents()
		f.out("b.txt", "b")
		old := collectDeadline
		collectDeadline = time.Nanosecond
		t.Cleanup(func() { collectDeadline = old })
		f.collect("writer", 2)
		line := f.onlyLine()
		if line.level != "warn" || !strings.HasPrefix(line.msg, "artifacts: stopped at the 0 s deadline") {
			t.Fatalf("line = %+v", line)
		}
		if got := f.rows(); len(got) != len(stored) || got["a.txt"] != stored["a.txt"] {
			t.Fatalf("what was stored changed: %+v -> %+v", stored, got)
		}
	})
	t.Run("nothing to do", func(t *testing.T) {
		f := newFixture(t)
		f.out("a.txt", "a")
		var none *Collector
		none.Collect(context.Background(), engine.ArtifactInput{Workspace: f.ws, Run: f.run, Node: &store.RunNode{NodeID: "n", Visit: 1}})
		f.c.Collect(context.Background(), engine.ArtifactInput{Workspace: f.ws, Node: &store.RunNode{NodeID: "n", Visit: 1}})
		f.c.Collect(context.Background(), engine.ArtifactInput{Workspace: f.ws, Run: f.run})
		bad := *f.run
		bad.ID = "../escape"
		f.c.Collect(context.Background(), engine.ArtifactInput{Workspace: f.ws, Run: &bad, Node: &store.RunNode{NodeID: "n", Visit: 1}})
		if rows := f.rows(); len(rows) != 0 {
			t.Fatalf("rows = %+v", rows)
		}
		f.noLines()
	})
	t.Run("without LogEvent and Emit", func(t *testing.T) {
		f := newFixture(t)
		f.out("a.txt", "a")
		f.c.Emit, f.c.LogEvent, f.c.Log = nil, nil, nil
		f.collect("writer", 1)
		if rows := f.rows(); len(rows) != 1 {
			t.Fatalf("rows = %+v", rows)
		}
	})
}

// DESIGN 5.25 rule 4 and T5.25.7: New creates the store directory (and its parents) when
// it is missing, proves it writable with a probe it removes, and reads the TTL in days;
// a directory it cannot use is an error that names the cause without the path, and the
// coordinator then runs with no collector.
func TestNew(t *testing.T) {
	pool := needDB(t)
	dir := filepath.Join(t.TempDir(), "a", "b", "artifacts")
	c, err := New(dir, pool, nil, nil, nil, 30)
	if err != nil {
		t.Fatal(err)
	}
	if c.Dir != dir || c.TTL != 30*24*time.Hour || c.Pool != pool || c.Log == nil {
		t.Errorf("collector = %+v", c)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the probe was left: %v", entries)
	}
	if c, err = New(dir, pool, nil, nil, nil, 0); err != nil || c.TTL != 0 {
		t.Errorf("ttl 0: %+v, %v", c, err)
	}
	if c, err = New(dir, pool, nil, nil, nil, -3); err != nil || c.TTL != 0 {
		t.Errorf("negative ttl: %+v, %v", c, err)
	}

	// Unusable: a directory below a regular file (true for any user, root included), a
	// path that is a file, an empty setting.
	file := filepath.Join(t.TempDir(), "file")
	must(t, os.WriteFile(file, []byte("x"), 0o644))
	for name, bad := range map[string]string{"below a file": filepath.Join(file, "artifacts"), "a file": file, "empty": ""} {
		c, err := New(bad, pool, nil, nil, nil, 30)
		if err == nil || c != nil {
			t.Errorf("%s: New = %+v, %v; want an error and no collector", name, c, err)
			continue
		}
		if bad != "" && strings.Contains(err.Error(), bad) {
			t.Errorf("%s: the error names the path: %v", name, err)
		}
	}
	if os.Getuid() != 0 {
		ro := filepath.Join(t.TempDir(), "ro")
		must(t, os.Mkdir(ro, 0o500))
		if c, err := New(ro, pool, nil, nil, nil, 30); err == nil || c != nil {
			t.Errorf("a read-only directory: %+v, %v", c, err)
		}
	}
}

// project run fixtures --------------------------------------------------------------

// projectFixture is a project run: a bare repository as the remote, the project's config,
// and the checkout in the agent's workspace at runs/<run id>/repo, cloned the way the
// engine clones it, with the run's checkout_sha recorded.
type projectFixture struct {
	*fixture
	bare string
	cfg  *project.Config
	base string
}

func newProjectFixture(t *testing.T) *projectFixture {
	t.Helper()
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n", "src/app.py": "print('hi')\n"})
	f := newFixture(t)
	cfg := &project.Config{RepoURL: bare, Token: "glpat-not-a-real-token-0", DefaultBranch: "main", Provider: project.ProviderGitLab}
	checkout := filepath.Join(f.runPath(), project.RepoDir)
	must(t, cfg.Clone(context.Background(), checkout))
	base, err := cfg.HeadSHA(context.Background(), checkout)
	must(t, err)
	f.run.CheckoutSHA = &base
	return &projectFixture{fixture: f, bare: bare, cfg: cfg, base: base}
}

func (p *projectFixture) edit(rel, content string) {
	p.t.Helper()
	path := filepath.Join(p.runPath(), project.RepoDir, rel)
	must(p.t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(p.t, os.WriteFile(path, []byte(content), 0o644))
}

func (p *projectFixture) collectProject(node string, visit int) {
	p.t.Helper()
	p.collectWith(engine.ArtifactInput{Node: &store.RunNode{RunID: p.run.ID, NodeID: node, Visit: visit}, Project: p.cfg, Branch: "main"})
}

func (p *projectFixture) diffRow() (store.RunArtifact, bool) {
	a, ok := p.rows()["diff.patch"]
	return a, ok
}

// DESIGN 5.25 rule 3 (T5.25.3): a visit of a project run stores the checkout's diff as
// the generated artifact diff.patch (text/x-patch) with the hunk; the same diff collected
// again changes nothing; a later change replaces it in place; and a checkout back at the
// commit it was cloned from (or changed only in node_modules) has no diff: the earlier
// generated row and its file are removed and the count is published.
func TestCollectProjectDiff(t *testing.T) {
	p := newProjectFixture(t)
	p.edit("README.md", "# demo\nsecond line\n")
	p.edit("node_modules/dep/index.js", "x")
	p.collectProject("coder", 1)

	row, ok := p.diffRow()
	if !ok {
		t.Fatalf("no diff.patch: rows %+v, lines %+v", p.rows(), p.takeLines())
	}
	content, found := p.stored(row)
	if !found || !strings.Contains(content, "diff --git a/README.md b/README.md") || !strings.Contains(content, "+second line") ||
		strings.Contains(content, "node_modules") {
		t.Fatalf("diff.patch = %q (found %v)", content, found)
	}
	if !row.Generated || row.ContentType != "text/x-patch" || row.NodeID != "coder" || row.Visit != 1 ||
		row.Size != int64(len(content)) || row.SHA256 != sum(content) {
		t.Fatalf("row = %+v", row)
	}
	p.noLines()
	if events := p.takeEvents(); len(events) != 1 || events[0].payload["count"] != 1 {
		t.Fatalf("events = %+v", events)
	}

	// The same diff after another visit: nothing changes.
	p.collectProject("reviewer", 1)
	if again, _ := p.diffRow(); again != row {
		t.Errorf("an unchanged diff changed the row: %+v -> %+v", row, again)
	}
	if events := p.takeEvents(); len(events) != 0 {
		t.Errorf("events = %+v", events)
	}

	// More work: the row is updated in place.
	p.edit("src/app.py", "print('hello')\n")
	p.collectProject("coder", 2)
	next, _ := p.diffRow()
	if nextContent, _ := p.stored(next); next.ID != row.ID || next.Visit != 2 || !strings.Contains(nextContent, "src/app.py") || !next.Generated {
		t.Fatalf("updated diff row = %+v, content %q", next, nextContent)
	}
	if left := p.leftovers(); len(left) != 0 {
		t.Errorf("files without a row: %v", left)
	}
	p.takeEvents()

	// Back to the clone's tree: no diff, the earlier generated one is removed.
	p.edit("README.md", "# demo\n")
	p.edit("src/app.py", "print('hi')\n")
	p.collectProject("coder", 3)
	if _, ok := p.diffRow(); ok {
		t.Fatal("an empty diff left a diff.patch row")
	}
	if _, err := os.Stat(filepath.Join(p.c.Dir, p.run.ID, row.ID)); !os.IsNotExist(err) {
		t.Errorf("the removed diff's file: %v", err)
	}
	if events := p.takeEvents(); len(events) != 1 || events[0].payload["count"] != 0 {
		t.Errorf("events = %+v, want one with count 0", events)
	}
	p.noLines()
}

// DESIGN 5.25 rule 3: with only an excluded directory changed there is no diff and no
// artifact; the diff counts toward the caps like any file and is cut at its cap with the
// marker as its last line.
func TestCollectProjectDiffExclusionsAndCaps(t *testing.T) {
	p := newProjectFixture(t)
	p.edit("node_modules/dep/index.js", "x")
	p.edit(".venv/lib/x.py", "x")
	p.collectProject("coder", 1)
	if rows := p.rows(); len(rows) != 0 {
		t.Fatalf("rows = %+v", rows)
	}
	p.noLines()

	// Cut: lower the patch cap; the stored patch stays within it and ends with the marker.
	lowerCaps(t, 200, 50<<20, 20<<20, 400)
	p.edit("README.md", "# demo\n"+strings.Repeat("a very long line of new text\n", 100))
	p.collectProject("coder", 1)
	row, ok := p.diffRow()
	if !ok {
		t.Fatalf("no diff.patch; lines %+v", p.takeLines())
	}
	content, _ := p.stored(row)
	if row.Size != 400 || int64(len(content)) != 400 || !strings.HasSuffix(content, "\n# cut at 10 MiB\n") ||
		!strings.HasPrefix(content, "diff --git a/README.md") {
		t.Fatalf("cut patch: size %d, %d bytes, %q", row.Size, len(content), content[max(0, len(content)-60):])
	}
}

// DESIGN 5.25 rule 3 (caps): the patch takes a slot and bytes of the run's room like any
// file, so a run that is full does not get one, and says so in the line of the visit.
func TestCollectProjectDiffCountsTowardTheCaps(t *testing.T) {
	p := newProjectFixture(t)
	lowerCaps(t, 2, 50<<20, 20<<20, 10<<20)
	p.out("a.txt", "a")
	p.out("b.txt", "b")
	p.edit("README.md", "# demo\nchanged\n")
	p.collectProject("coder", 1)
	if _, ok := p.diffRow(); ok {
		t.Fatal("a diff was stored past the files cap")
	}
	if line := p.onlyLine(); line.msg != "artifacts: 1 files not collected: diff.patch: past the 2 files cap" {
		t.Fatalf("line = %+v", line)
	}
	if len(p.rows()) != 2 {
		t.Fatalf("rows = %+v", p.rows())
	}
}

// DESIGN 5.25 rule 3 and edge cases: a diff.patch the agent wrote under out/ is the
// artifact and the generated one is skipped with a log line, on that pass and on the
// later passes that still see the agent's row; a diff.patch the agent deleted afterwards
// does not hand its row to the generated one either.
func TestCollectProjectDiffAgentsOwnWins(t *testing.T) {
	p := newProjectFixture(t)
	p.edit("README.md", "# demo\nchanged\n")
	p.out("diff.patch", "the agent's own summary of the change\n")
	p.collectProject("coder", 1)
	row, ok := p.diffRow()
	content, _ := p.stored(row)
	if !ok || row.Generated || content != "the agent's own summary of the change\n" || row.ContentType != "text/x-patch" {
		t.Fatalf("row = %+v, content %q", row, content)
	}
	if line := p.onlyLine(); line.level != "info" || line.msg != "artifacts: diff.patch skipped: the agent wrote its own" {
		t.Fatalf("line = %+v", line)
	}

	// Gone from out/: the agent's row still wins over the generated diff.
	must(t, os.Remove(filepath.Join(p.runPath(), "out", "diff.patch")))
	p.collectProject("reviewer", 1)
	if again, _ := p.diffRow(); again != row {
		t.Fatalf("the agent's row was replaced: %+v", again)
	}
	if line := p.onlyLine(); line.msg != "artifacts: diff.patch skipped: the agent wrote its own" {
		t.Fatalf("line = %+v", line)
	}

	// The reverse: a generated diff, then the agent writes its own: the row becomes the agent's.
	q := newProjectFixture(t)
	q.edit("README.md", "# demo\nchanged\n")
	q.collectProject("coder", 1)
	generated, _ := q.diffRow()
	if !generated.Generated {
		t.Fatalf("row = %+v", generated)
	}
	q.out("diff.patch", "mine\n")
	q.collectProject("coder", 2)
	mine, _ := q.diffRow()
	if mine.ID != generated.ID || mine.Generated || mine.Size != 5 {
		t.Fatalf("row = %+v", mine)
	}
}

// DESIGN 5.25 rule 3 and edge cases: the base is the run's checkout_sha. A branch that
// moved since the clone is diffed against that commit, so the patch holds this run's
// changes only; a sha the provider will not serve skips the diff with the reason (no
// token in it) and changes nothing else; a branch that cannot be cloned does the same.
func TestCollectProjectDiffBaseAndFailures(t *testing.T) {
	p := newProjectFixture(t)
	work := filepath.Join(t.TempDir(), "hand")
	projecttest.Git(t, filepath.Dir(work), "clone", "--quiet", "--branch", "main", p.bare, work)
	must(t, os.WriteFile(filepath.Join(work, "theirs.txt"), []byte("theirs\n"), 0o644))
	projecttest.Git(t, work, "add", "--all")
	projecttest.Git(t, work, "commit", "--quiet", "-m", "by hand")
	projecttest.Git(t, work, "push", "--quiet", "origin", "HEAD:refs/heads/main")

	p.edit("README.md", "# demo\nmine\n")
	p.collectProject("coder", 1)
	row, ok := p.diffRow()
	content, _ := p.stored(row)
	if !ok || !strings.Contains(content, "+mine") || strings.Contains(content, "theirs.txt") {
		t.Fatalf("diff against the moved branch: %q (found %v), lines %+v", content, ok, p.takeLines())
	}
	p.noLines()

	// A sha nobody serves: the diff is skipped with git's reason, the earlier row stays.
	unfetchable := "0123456789abcdef0123456789abcdef01234567"
	p.run.CheckoutSHA = &unfetchable
	p.collectProject("coder", 2)
	line := p.onlyLine()
	if line.level != "warn" || !strings.HasPrefix(line.msg, "artifacts: diff skipped: git fetch") || strings.Contains(line.msg, p.cfg.Token) {
		t.Fatalf("line = %+v", line)
	}
	if again, _ := p.diffRow(); again != row {
		t.Errorf("a skipped diff changed the row: %+v", again)
	}

	// A branch the remote lacks.
	p.run.CheckoutSHA = &p.base
	p.collectWith(engine.ArtifactInput{Node: &store.RunNode{NodeID: "coder", Visit: 3}, Project: p.cfg, Branch: "yuno/missing"})
	line = p.onlyLine()
	if line.level != "warn" || !strings.HasPrefix(line.msg, "artifacts: diff skipped: git clone") || strings.Contains(line.msg, p.cfg.Token) {
		t.Fatalf("line = %+v", line)
	}

	// No branch given: the project's default branch.
	p.collectWith(engine.ArtifactInput{Node: &store.RunNode{NodeID: "coder", Visit: 4}, Project: p.cfg})
	p.noLines()
	if again, _ := p.diffRow(); again.Visit != 1 {
		t.Errorf("row = %+v, want the first visit's diff unchanged", again)
	}
}

// DESIGN 5.25 rule 3 and edge cases: a run without a checkout (Project nil, or the
// directory missing) has no diff and no log line; a checkout that is a link or has no
// .git is skipped with a line and the diff is never made from it.
func TestCollectProjectDiffNeedsACheckout(t *testing.T) {
	p := newProjectFixture(t)
	p.edit("README.md", "# demo\nchanged\n")

	p.collectWith(engine.ArtifactInput{Node: &store.RunNode{NodeID: "coder", Visit: 1}}) // Project nil
	p.noLines()
	if rows := p.rows(); len(rows) != 0 {
		t.Fatalf("a run without a project got %+v", rows)
	}

	repo := filepath.Join(p.runPath(), project.RepoDir)
	moved := repo + "-moved"
	must(t, os.Rename(repo, moved))
	p.collectProject("coder", 1) // no repo/ at all
	p.noLines()

	must(t, os.Symlink(moved, repo))
	p.collectProject("coder", 2) // repo/ is a link
	if line := p.onlyLine(); line.msg != "artifacts: diff skipped: repo/ is not a plain directory" {
		t.Fatalf("line = %+v", line)
	}
	must(t, os.Remove(repo))

	must(t, os.MkdirAll(repo, 0o755)) // a directory without .git
	must(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("x"), 0o644))
	p.collectProject("coder", 3)
	if line := p.onlyLine(); line.msg != "artifacts: diff skipped: "+project.ErrNoCheckout.Error() {
		t.Fatalf("line = %+v", line)
	}
	if rows := p.rows(); len(rows) != 0 {
		t.Fatalf("rows = %+v", rows)
	}
}

// The walk is fs.WalkDir's: directory by directory, each sorted by name (so a/b.txt
// precedes b.txt, and upper case precedes lower case).
func TestWalkOrderIsLexicalPerDirectory(t *testing.T) {
	f := newFixture(t)
	for _, n := range []string{"b.txt", "a/z.txt", "a/b.txt", "c/d/e.txt", "B.txt"} {
		f.out(n, n)
	}
	lowerCaps(t, 3, 50<<20, 20<<20, 10<<20)
	f.collect("writer", 1)
	rows := f.rows()
	for _, kept := range []string{"B.txt", "a/b.txt", "a/z.txt"} {
		if _, ok := rows[kept]; !ok {
			t.Errorf("%s was not among the first three files", kept)
		}
	}
	if line := f.onlyLine(); line.msg != "artifacts: 2 files not collected: b.txt: past the 3 files cap; c/d/e.txt: past the 3 files cap" {
		t.Fatalf("line = %+v", line)
	}
}
