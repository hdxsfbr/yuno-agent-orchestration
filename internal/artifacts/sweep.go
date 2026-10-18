package artifacts

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"yuno/internal/store"
)

// Sweep applies the retention of DESIGN 5.25 rule 6; it has memory.SweepFunc's signature
// and runs in the sweeper's pass. When TTL is set, the rows and files of runs whose
// finished_at is older than TTL are deleted, rows first and then the run's directory; a
// run that has not finished keeps its artifacts. Then a run directory in the store with no
// rows (a crash between a file write and its row) is removed too when its run is gone or
// finished, and a file in a directory that has rows but is not any row's, once it is old
// enough not to be a collection in progress. TTL 0 keeps artifacts forever and removes
// only those orphans. It returns how many rows it deleted and logs one line with the
// counts when anything was removed; a failure is returned after the rest was done, and
// the next pass retries.
func (c *Collector) Sweep(ctx context.Context, now time.Time) (int64, error) {
	var (
		rows                              int64
		errs                              []error
		runs, orphanDirs, orphanFileCount int
	)
	if c.TTL > 0 {
		ids, err := store.ListExpiredArtifactRuns(ctx, c.Pool, now.Add(-c.TTL))
		if err != nil {
			errs = append(errs, err)
		}
		for _, id := range ids {
			n, err := store.DeleteRunArtifacts(ctx, c.Pool, id)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			rows += n
			runs++
			if safeName(id) {
				if err := os.RemoveAll(c.runDir(id)); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}

	entries, err := os.ReadDir(c.Dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		// Only the directories the platform makes: never a link, never a name that is
		// not a run id (the probe file, a volume's own directories).
		if !e.IsDir() || !safeName(e.Name()) {
			continue
		}
		id := e.Name()
		files, _, err := store.ArtifactTotals(ctx, c.Pool, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if files > 0 {
			n, err := c.removeStrays(ctx, id, now)
			orphanFileCount += n
			if err != nil {
				errs = append(errs, err)
			}
			continue
		}
		run, err := store.GetRun(ctx, c.Pool, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			errs = append(errs, err)
			continue
		case !store.IsTerminal(run.Status):
			continue // a live run may be between writing a file and its row
		}
		if err := os.RemoveAll(c.runDir(id)); err != nil {
			errs = append(errs, err)
			continue
		}
		orphanDirs++
	}

	if runs+orphanDirs+orphanFileCount > 0 {
		c.logger().Info("artifacts swept", "expired_runs", runs, "rows", rows, "orphan_dirs", orphanDirs, "orphan_files", orphanFileCount)
	}
	return rows, errors.Join(errs...)
}

// removeStrays deletes the files of a run's directory that no row names and that are older
// than orphanGrace: what a crash between a write and its row (or a copy that never
// finished) left behind. It returns how many it removed.
func (c *Collector) removeStrays(ctx context.Context, runID string, now time.Time) (int, error) {
	rows, err := store.ListRunArtifacts(ctx, c.Pool, runID)
	if err != nil {
		return 0, err
	}
	named := make(map[string]bool, len(rows))
	for _, r := range rows {
		named[r.ID] = true
	}
	entries, err := os.ReadDir(c.runDir(runID))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var errs []error
	removed := 0
	for _, e := range entries {
		if !e.Type().IsRegular() || named[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < orphanGrace {
			continue
		}
		if err := os.Remove(filepath.Join(c.runDir(runID), e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}
