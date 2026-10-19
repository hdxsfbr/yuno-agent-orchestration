// Package artifacts collects the files a run delivers (DESIGN 5.25): when an agent visit
// ends, whatever it left under runs/<run id>/out/ of its workspace, and for a project run
// the diff of its checkout, is copied into the artifact store (ARTIFACT_DIR, a named
// volume) and recorded in run_artifacts. Collection is best effort: nothing in this
// package changes a visit's outcome, and what was stored before a failure stays.
package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/engine"
	"yuno/internal/obs"
	"yuno/internal/openclaw"
	"yuno/internal/project"
	"yuno/internal/store"
)

// The caps and the deadline of DESIGN 5.25 (units and rule 2). Variables, not constants,
// so the tests can lower them instead of writing 50 MiB.
var (
	maxFiles              = 200      // files per run
	maxTotalBytes   int64 = 50 << 20 // the sum of the stored sizes per run
	maxFileBytes    int64 = 20 << 20 // one file
	maxPatchBytes   int64 = project.MaxDiffBytes
	maxPathBytes          = 512 // the path relative to out/
	collectDeadline       = 60 * time.Second
	// orphanGrace is how old a file in a run's directory must be, with no row, before the
	// sweep treats it as left by a crash rather than as a collection still in progress.
	orphanGrace = time.Hour
)

// putTestHook lets a test act between reading a path's row and writing it, where another
// writer's row can appear; nil in production.
var putTestHook func(path string)

// cutMarker ends a patch that was cut at the cap (DESIGN 5.25 rule 3).
const cutMarker = "\n# cut at 10 MiB\n"

// logSkipNamed is how many skipped files the log line of a visit names.
const logSkipNamed = 3

// defaultTTLDays is ARTIFACT_TTL_DAYS' default (DESIGN 5.25 inputs), which a negative value
// also means (rule 6); 0 keeps artifacts for ever.
const defaultTTLDays = 30

// Collector keeps the artifacts of runs. The zero value is not usable: build one with New.
type Collector struct {
	// Dir is ARTIFACT_DIR: <Dir>/<run id>/<artifact id> holds each file.
	Dir  string
	Pool *pgxpool.Pool
	Log  *slog.Logger
	// Emit publishes an SSE event of a run (the engine's Emit); LogEvent writes a run log
	// line (the engine's LogEvent). Either may be nil.
	Emit     func(ctx context.Context, runID, event string, payload map[string]any)
	LogEvent func(ctx context.Context, runID, level, msg string, attrs ...any)
	// Now is the clock for created_at and updated_at; nil means time.Now in UTC.
	Now func() time.Time
	// TTL is how long after finished_at a run keeps its artifacts; 0 keeps them forever.
	TTL time.Duration

	// slots serialize the collections of one run (review L9): the parked visit's and the
	// continuation's can overlap, and two passes over one run's rows and files would race.
	slotsMu sync.Mutex
	slots   map[string]*runSlot
}

// runSlot is the lock of one run's collections: a channel of one, so a waiter can give
// up when its deadline passes, and a count of the passes holding or waiting for it, so the
// map does not keep a run once nobody uses it.
type runSlot struct {
	sem  chan struct{}
	refs int
}

// lockRun waits for the run's turn, up to ctx; release gives it back. An error is the
// context's.
func (c *Collector) lockRun(ctx context.Context, runID string) (release func(), err error) {
	c.slotsMu.Lock()
	if c.slots == nil {
		c.slots = map[string]*runSlot{}
	}
	s := c.slots[runID]
	if s == nil {
		s = &runSlot{sem: make(chan struct{}, 1)}
		c.slots[runID] = s
	}
	s.refs++
	c.slotsMu.Unlock()
	drop := func() {
		c.slotsMu.Lock()
		if s.refs--; s.refs == 0 {
			delete(c.slots, runID)
		}
		c.slotsMu.Unlock()
	}
	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

var _ engine.ArtifactCollector = (*Collector)(nil)

// New builds the collector over dir (ARTIFACT_DIR, DESIGN 5.25 rule 4): it creates the
// directory when it is missing and writes and removes a probe file, so an unusable store
// is found at start. The error names the cause without the directory; the caller logs
// `artifacts: <dir>: <error>; artifacts are disabled` and runs without a collector. ttlDays
// is ARTIFACT_TTL_DAYS: 0 keeps artifacts for ever, a negative value is the default 30
// (DESIGN 5.25 rule 6, review L7).
func New(dir string, pool *pgxpool.Pool, log *slog.Logger,
	emit func(ctx context.Context, runID, event string, payload map[string]any),
	logEvent func(ctx context.Context, runID, level, msg string, attrs ...any), ttlDays int) (*Collector, error) {
	if dir == "" {
		return nil, errors.New("no directory is set")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, bare(err)
	}
	probe := filepath.Join(dir, ".probe-"+store.NewID())
	f, err := os.OpenFile(probe, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, bare(err)
	}
	_, werr := f.WriteString("ok")
	cerr := f.Close()
	rerr := os.Remove(probe)
	if err := errors.Join(werr, cerr, rerr); err != nil {
		return nil, bare(err)
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if ttlDays < 0 {
		ttlDays = defaultTTLDays
	}
	return &Collector{Dir: dir, Pool: pool, Log: log, Emit: emit, LogEvent: logEvent,
		TTL: time.Duration(ttlDays) * 24 * time.Hour}, nil
}

// bare is err without the file name an *fs.PathError carries, so the caller's log line
// names the directory once.
func bare(err error) error {
	var pe *fs.PathError
	for errors.As(err, &pe) {
		err = pe.Err
	}
	return err
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c *Collector) logLine(ctx context.Context, runID, level, msg string) {
	if c.LogEvent != nil {
		c.LogEvent(ctx, runID, level, msg)
		return
	}
	c.logger().Log(ctx, slogLevel(level), msg, "run_id", runID)
}

// logger is Log, or a logger that discards when none was set.
func (c *Collector) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.New(slog.DiscardHandler)
}

func slogLevel(level string) slog.Level {
	switch level {
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// runDir is <Dir>/<run id>, where the run's files live.
func (c *Collector) runDir(runID string) string { return filepath.Join(c.Dir, runID) }

// runIDPattern is the name of a run's directory in the store: the 36-character id the
// platform issues (store.NewID, a UUID with dashes in lower case), the shape the report
// keys accept (engine.reportRunID). Nothing else in ARTIFACT_DIR is ever touched, so a
// store that shares its directory with other data (review M1) loses none of it.
var runIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func isRunID(s string) bool { return runIDPattern.MatchString(s) }

// Collect collects what an agent visit left (DESIGN 5.25 rule 2), under a 60 s deadline.
// It is best effort: a failure is a log line of the run and never reaches the visit; what
// was stored before it stays. It satisfies engine.ArtifactCollector.
func (c *Collector) Collect(ctx context.Context, in engine.ArtifactInput) {
	if c == nil || in.Run == nil || in.Node == nil || !isRunID(in.Run.ID) {
		return
	}
	start := c.now()
	dctx, cancel := context.WithTimeout(ctx, collectDeadline)
	defer cancel()
	// One collection of a run at a time (review L9): the parked visit's and its
	// continuation's can overlap. The wait for the other is part of the deadline.
	release, err := c.lockRun(dctx, in.Run.ID)
	if err != nil {
		c.logLine(ctx, in.Run.ID, "warn", "artifacts: "+deadlineText(err))
		return
	}
	defer release()
	p := &pass{c: c, in: in, runID: in.Run.ID, dir: c.runDir(in.Run.ID)}
	err = p.run(dctx)
	p.reportSkips(ctx)
	if err != nil {
		c.logLine(ctx, p.runID, "warn", "artifacts: "+deadlineText(err))
	}
	if p.changed > 0 && c.Emit != nil {
		c.Emit(ctx, p.runID, "run.artifacts", map[string]any{"run_id": p.runID, "count": p.files})
	}
	if p.changed > 0 || p.skipN > 0 {
		c.logger().Info("artifacts collected", "run_id", p.runID, "node_id", in.Node.NodeID, "visit", in.Node.Visit,
			"changed", p.changed, "files", p.files, "bytes", p.bytes, "skipped", p.skipN,
			"duration_ms", c.now().Sub(start).Milliseconds())
	}
}

// deadlineText is the text of a collection's failure for its log line: the deadline gets
// its own sentence, anything else its redacted message.
func deadlineText(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("stopped at the %d s deadline; what was stored stays", int(collectDeadline.Seconds()))
	}
	return obs.Redact(bare(err).Error())
}

// skipped is a file that was not collected, and why.
type skipped struct{ path, reason string }

// pass is one collection: the run's room under the caps, read once before the walk and
// kept up to date with what this pass stored, and what it skipped.
type pass struct {
	c     *Collector
	in    engine.ArtifactInput
	runID string
	dir   string // <Dir>/<run id>

	files   int       // rows the run has, as of now
	bytes   int64     // their sizes added up
	changed int       // rows this pass inserted, updated or deleted
	skipN   int       // files not collected
	skips   []skipped // the first logSkipNamed of them
	ownDiff bool      // the agent's out/ holds a diff.patch of its own
}

// run does the whole collection under ctx (the deadline).
func (p *pass) run(ctx context.Context) error {
	runRoot, err := openclaw.OpenRunDirAt(p.in.Workspace, p.runID)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil // the agent wrote nothing for this run
	case errors.Is(err, openclaw.ErrRunDirLink):
		p.c.logLine(ctx, p.runID, "warn", "artifacts: out/ is not a plain directory")
		return nil
	case err != nil:
		return err
	}
	defer runRoot.Close()
	if p.files, p.bytes, err = store.ArtifactTotals(ctx, p.c.Pool, p.runID); err != nil {
		return err
	}

	out, err := openPlainDir(runRoot, "out")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No out/: nothing to walk, and no log line.
	case errors.Is(err, errNotPlain):
		p.c.logLine(ctx, p.runID, "warn", "artifacts: out/ is not a plain directory")
		return nil
	case err != nil:
		return err
	default:
		defer out.Close()
		if err := p.walk(ctx, out); err != nil {
			return err
		}
	}
	return p.diff(ctx, runRoot)
}

// errNotPlain is a directory that is a link or another kind of entry.
var errNotPlain = errors.New("not a plain directory")

// openPlainDir opens the directory name of parent as a root, refusing a link there: the
// entry is looked at without being followed, and the directory opened is the one looked at.
func openPlainDir(parent *os.Root, name string) (*os.Root, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() {
		return nil, errNotPlain
	}
	sub, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	after, err := sub.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		sub.Close()
		if err == nil {
			err = errNotPlain
		}
		return nil, err
	}
	return sub, nil
}

// walk collects the regular files of out/ in the order fs.WalkDir lists them (lexical
// within each directory). Links and special files are skipped and never followed.
func (p *pass) walk(ctx context.Context, out *os.Root) error {
	return fs.WalkDir(out.FS(), ".", func(rel string, d fs.DirEntry, werr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if werr != nil {
			if d == nil {
				return werr
			}
			p.skip(rel, "unreadable: "+bare(werr).Error())
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if rel == "." || d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			p.skip(rel, notRegularReason(d.Type()))
			return nil
		}
		if rel == store.GeneratedArtifactPath {
			p.ownDiff = true
		}
		switch {
		case len(rel) > maxPathBytes:
			p.skip(rel, fmt.Sprintf("path over %d bytes", maxPathBytes))
			return nil
		case !usableName(rel):
			p.skip(rel, "name is not plain text")
			return nil
		}
		return p.collectFile(ctx, out, rel)
	})
}

func notRegularReason(m fs.FileMode) string {
	if m&fs.ModeSymlink != 0 {
		return "a link"
	}
	return "not a regular file"
}

// usableName reports whether a path can be stored and shown: valid UTF-8 without control
// characters (the database refuses a NUL and invalid UTF-8; a newline would break the log
// line that names the file) and without format characters or the line and paragraph
// separators, which can make a name read as another one (a right-to-left override reverses
// what follows it, so a name can look like another type; review L4).
func usableName(rel string) bool {
	if !utf8.ValidString(rel) {
		return false
	}
	return !strings.ContainsFunc(rel, unsafeRune)
}

// unsafeRune is a character a stored name and the log line that names it must not carry:
// a control character, a format character or a line or paragraph separator.
func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)
}

// collectFile opens one regular file of out/ without following a link and stores it. A
// problem with the file itself is a skip; only a failure on the store's side is returned.
func (p *pass) collectFile(ctx context.Context, out *os.Root, rel string) error {
	before, err := out.Lstat(rel)
	if err != nil {
		p.skip(rel, "unreadable: "+bare(err).Error())
		return nil
	}
	if !before.Mode().IsRegular() {
		p.skip(rel, notRegularReason(before.Mode()))
		return nil
	}
	// O_NONBLOCK: a pipe swapped in since the walk must not block the open.
	f, err := out.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		p.skip(rel, "unreadable: "+bare(err).Error())
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(before, info) {
		p.skip(rel, "changed while it was collected")
		return nil
	}
	return p.put(ctx, rel, f, info.Size(), false)
}

// noRoom is why a file of size bytes at path cannot be added to the run, "" when it can:
// a new path needs a free slot under maxFiles, and the sizes of the run, with this one
// taking the place of the stored version of its path, must stay under maxTotalBytes.
func (p *pass) noRoom(existing *store.RunArtifact, size int64) string {
	files, total := p.files, p.bytes
	if existing == nil {
		files++
	} else {
		total -= existing.Size
	}
	total += size
	switch {
	case files > maxFiles:
		return fmt.Sprintf("past the %d files cap", maxFiles)
	case total > maxTotalBytes:
		return "past the " + sizeText(maxTotalBytes) + " cap"
	}
	return ""
}

// sizeText prints a cap: whole MiB as "20 MiB", anything else in bytes.
func sizeText(n int64) string {
	if n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}

// put stores the content of src as the artifact at rel (DESIGN 5.25 rule 2). The file is
// written to <Dir>/<run id>/<new id>.tmp while its sha256 is computed. A row at rel with
// the same sha256 leaves everything as it was (the copy is discarded). Otherwise the copy
// is renamed to <run id>/<row id> (the id of the row it replaces, else the new one) and
// only then is the row inserted or updated, so a row never points at a partial file.
// size is what the source claims, for a cheap refusal before any copy. A problem with the
// content is a skip; a failure on the store's side is returned and ends the collection.
func (p *pass) put(ctx context.Context, rel string, src io.Reader, size int64, generated bool) error {
	existing, err := store.GetRunArtifactByPath(ctx, p.c.Pool, p.runID, rel)
	if errors.Is(err, store.ErrNotFound) {
		existing, err = nil, nil
	}
	if err != nil {
		return err
	}
	if size > maxFileBytes {
		p.skip(rel, "over "+sizeText(maxFileBytes))
		return nil
	}
	if reason := p.noRoom(existing, size); reason != "" {
		p.skip(rel, reason)
		return nil
	}
	if err := os.MkdirAll(p.dir, 0o755); err != nil {
		return err
	}
	newID := store.NewID()
	tmpPath := filepath.Join(p.dir, newID+".tmp")
	tmp, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		if !renamed {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()

	sum := sha256.New()
	head := &headWriter{}
	n, readErr, writeErr := copyCounted(ctx, io.MultiWriter(tmp, sum, head), io.LimitReader(src, maxFileBytes+1))
	switch {
	case writeErr != nil:
		return writeErr
	case readErr != nil:
		p.skip(rel, "unreadable: "+bare(readErr).Error())
		return nil
	case n > maxFileBytes:
		p.skip(rel, "over "+sizeText(maxFileBytes)) // it grew while it was copied
		return nil
	}
	if reason := p.noRoom(existing, n); reason != "" {
		p.skip(rel, reason)
		return nil
	}
	digest := hex.EncodeToString(sum.Sum(nil))
	if existing != nil && existing.SHA256 == digest {
		return nil // unchanged: the row stays the first visit's, the copy is discarded
	}
	if err := tmp.Chmod(0o644); err != nil { // whatever the umask said
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	id := newID
	if existing != nil {
		id = existing.ID
	}
	if err := os.Rename(tmpPath, filepath.Join(p.dir, id)); err != nil {
		return err
	}
	renamed = true

	now := p.c.now()
	row := store.RunArtifact{ID: id, RunID: p.runID, NodeID: p.in.Node.NodeID, Visit: p.in.Node.Visit, Path: rel,
		Size: n, SHA256: digest, ContentType: contentType(rel, head.buf), Generated: generated, CreatedAt: now, UpdatedAt: now}
	if putTestHook != nil {
		putTestHook(rel)
	}
	saved, changed, err := store.UpsertRunArtifact(ctx, p.c.Pool, row)
	if err != nil {
		if existing == nil {
			os.Remove(filepath.Join(p.dir, id)) // no row names it
		}
		return err
	}
	if saved != nil && saved.ID != id {
		// A row at this path appeared since it was read (a writer the run's lock does not
		// serialize): the file written under the new id belongs to no row. Where the
		// upsert took the row over, the file replaces the row's own; otherwise it goes.
		if changed {
			if err := os.Rename(filepath.Join(p.dir, id), filepath.Join(p.dir, saved.ID)); err != nil {
				return err
			}
		} else {
			os.Remove(filepath.Join(p.dir, id))
		}
	}
	if changed {
		if existing == nil {
			p.files++
		} else {
			p.bytes -= existing.Size
		}
		p.bytes += n
		p.changed++
	}
	return nil
}

// copyCounted copies src to dst and reports a failure of the read and a failure of the
// write apart, since the first is a problem with the file (a skip) and the second one with
// the store (the collection ends). A cancelled context is the second kind.
func copyCounted(ctx context.Context, dst io.Writer, src io.Reader) (n int64, readErr, writeErr error) {
	buf := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return n, nil, err
		}
		m, err := src.Read(buf)
		if m > 0 {
			if _, werr := dst.Write(buf[:m]); werr != nil {
				return n, nil, werr
			}
			n += int64(m)
		}
		if errors.Is(err, io.EOF) {
			return n, nil, nil
		}
		if err != nil {
			return n, err, nil
		}
	}
}

// headWriter keeps the first 512 bytes written to it, what http.DetectContentType reads.
type headWriter struct{ buf []byte }

func (w *headWriter) Write(b []byte) (int, error) {
	if room := 512 - len(w.buf); room > 0 {
		w.buf = append(w.buf, b[:min(room, len(b))]...)
	}
	return len(b), nil
}

// diff stores the checkout's diff as the generated artifact diff.patch (DESIGN 5.25 rule
// 3) for a visit of a project run whose workspace holds the checkout. Nothing about it
// fails the collection but the deadline and the store: the diff itself failing is a log
// line.
func (p *pass) diff(ctx context.Context, runRoot *os.Root) error {
	if p.in.Project == nil {
		return nil
	}
	if p.in.SkipDiff {
		// A parallel branch the join does not name (DESIGN 5.26 rule 11).
		p.c.logLine(ctx, p.runID, "info", "artifacts: diff skipped: parallel branch "+p.in.BranchHead)
		return nil
	}
	info, err := runRoot.Lstat(project.RepoDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // a run without a checkout
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		p.c.logLine(ctx, p.runID, "warn", "artifacts: diff skipped: repo/ is not a plain directory")
		return nil
	}
	existing, err := store.GetRunArtifactByPath(ctx, p.c.Pool, p.runID, store.GeneratedArtifactPath)
	if errors.Is(err, store.ErrNotFound) {
		existing, err = nil, nil
	}
	if err != nil {
		return err
	}
	if p.ownDiff || (existing != nil && !existing.Generated) {
		p.c.logLine(ctx, p.runID, "info", "artifacts: diff.patch skipped: the agent wrote its own")
		return nil
	}
	branch := p.in.Branch
	if branch == "" {
		branch = p.in.Project.DefaultBranch
	}
	base := ""
	if p.in.Run.CheckoutSHA != nil {
		base = *p.in.Run.CheckoutSHA
	}
	patch, err := p.in.Project.Diff(ctx, filepath.Join(p.in.Workspace, project.RepoRel(p.runID)), branch, base)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.c.logLine(ctx, p.runID, "warn", "artifacts: diff skipped: "+obs.Redact(err.Error()))
		return nil
	}
	if len(patch) == 0 {
		// An empty diff leaves an earlier generated diff.patch where it is (review H1):
		// after the Deployer pushes, checkout_sha moves to the pushed commit and the
		// checkout matches it, but the stored patch is still what the run changed.
		return nil
	}
	patch = cutPatch(patch)
	return p.put(ctx, store.GeneratedArtifactPath, bytes.NewReader(patch), int64(len(patch)), true)
}

// cutPatch keeps a patch within the cap: a longer one is cut and ends with the marker
// line, the whole staying at most maxPatchBytes.
func cutPatch(patch []byte) []byte {
	if int64(len(patch)) <= maxPatchBytes {
		return patch
	}
	keep := max(maxPatchBytes-int64(len(cutMarker)), 0)
	return append(patch[:keep:keep], cutMarker...)
}

func (p *pass) skip(path, reason string) {
	p.skipN++
	if len(p.skips) < logSkipNamed {
		p.skips = append(p.skips, skipped{path: path, reason: reason})
	}
}

// reportSkips writes the one log line of the visit that names what was not collected
// (DESIGN 5.25 rule 2): `artifacts: <n> files not collected: <path>: <reason>; ...`, the
// first three named.
func (p *pass) reportSkips(ctx context.Context) {
	if p.skipN == 0 {
		return
	}
	named := make([]string, 0, len(p.skips))
	for _, s := range p.skips {
		named = append(named, printable(s.path)+": "+s.reason)
	}
	msg := fmt.Sprintf("artifacts: %d files not collected: %s", p.skipN, strings.Join(named, "; "))
	if p.skipN > len(p.skips) {
		msg += "; ..."
	}
	p.c.logLine(ctx, p.runID, "info", msg)
}

// printable is a path fit for a log line: control and format characters, the separators
// and invalid UTF-8 shown as ?, and a long path cut.
func printable(path string) string {
	const limit = 100
	var b strings.Builder
	for i, r := range strings.ToValidUTF8(path, "�") {
		if i >= limit {
			b.WriteString("...")
			break
		}
		if unsafeRune(r) || r == utf8.RuneError {
			r = '?'
		}
		b.WriteRune(r)
	}
	return b.String()
}
