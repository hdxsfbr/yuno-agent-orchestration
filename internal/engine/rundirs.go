package engine

import (
	"context"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// Run folders (DESIGN 5.6 rule 13): runs/<run id>/ in the agents' workspaces holds the
// checkout and the files a run's agents passed along. Its artifacts and diff are in the
// artifact store once each visit ends, so the folders go when nothing can need them.
// A retry (5.21) resumes a failed, timed-out or cancelled run at the node it stopped at
// and relies on the earlier nodes' folders, so those runs keep theirs for runDirRetention.

const (
	// runDirRetention is how long a failed, timed-out or cancelled run keeps its folders
	// for a retry, and how old a folder of a run the database does not know must be to go.
	runDirRetention = 7 * 24 * time.Hour
	// runDirSweepEvery is the cadence of the folder sweep.
	runDirSweepEvery = time.Hour
)

// removeRunDirsAfterTerminal deletes the folders of a run that just succeeded; a run
// that failed, timed out or was cancelled keeps them, since it can be retried. It runs after the run's visits ended, so their artifact collection is done.
func (e *Engine) removeRunDirsAfterTerminal(run *store.Run) {
	if run == nil || e.deps.StateDir == "" {
		return
	}
	if run.Status != store.RunSucceeded {
		return
	}
	n, err := openclaw.RemoveRunDirs(e.deps.StateDir, run.ID)
	if err != nil {
		e.log.Warn("run folders not fully removed", "run_id", run.ID, "removed", n, "error", err)
		return
	}
	if n > 0 {
		e.log.Info("run folders removed", "run_id", run.ID, "workspaces", n)
	}
}

// runDirSweeper sweeps once at start, so leftovers on a host are cleaned on deploy,
// then every runDirSweepEvery.
func (e *Engine) runDirSweeper(ctx context.Context) {
	e.sweepRunDirs(ctx)
	t := time.NewTicker(runDirSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		e.sweepRunDirs(ctx)
	}
}

// sweepRunDirs removes the folders of runs that succeeded, of runs failed, timed out
// or cancelled for more than runDirRetention, and of run ids the database does
// not know whose folder is older than that. It logs counts only.
func (e *Engine) sweepRunDirs(ctx context.Context) {
	if e.deps.StateDir == "" || e.deps.Pool == nil {
		return
	}
	dirs, err := openclaw.ListRunDirs(e.deps.StateDir)
	if err != nil {
		e.log.Warn("run folder sweep: listing failed", "error", err)
	}
	if len(dirs) == 0 {
		return
	}
	ids := make([]string, len(dirs))
	for i, d := range dirs {
		ids[i] = d.RunID
	}
	type known struct {
		status   string
		finished *time.Time
	}
	runs := map[string]known{}
	rows, err := e.deps.Pool.Query(ctx, `SELECT id, status, finished_at FROM runs WHERE id = ANY($1::text[])`, ids)
	if err != nil {
		e.log.Warn("run folder sweep: run lookup failed", "error", err)
		return
	}
	for rows.Next() {
		var id string
		var k known
		if err := rows.Scan(&id, &k.status, &k.finished); err != nil {
			rows.Close()
			e.log.Warn("run folder sweep: run lookup failed", "error", err)
			return
		}
		runs[id] = k
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		e.log.Warn("run folder sweep: run lookup failed", "error", err)
		return
	}

	cutoff := e.clock.Now().Add(-runDirRetention)
	removed, kept, failed := 0, 0, 0
	for _, d := range dirs {
		k, inDB := runs[d.RunID]
		var due bool
		switch {
		case !inDB:
			due = d.ModTime.Before(cutoff)
		case k.status == store.RunSucceeded:
			due = true
		case k.status == store.RunFailed || k.status == store.RunTimedOut || k.status == store.RunCancelled:
			due = k.finished != nil && k.finished.Before(cutoff)
		}
		if !due {
			kept++
			continue
		}
		if _, err := openclaw.RemoveRunDirs(e.deps.StateDir, d.RunID); err != nil {
			failed++
			continue
		}
		removed++
	}
	if removed > 0 || failed > 0 {
		e.log.Info("run folder sweep", "removed", removed, "kept", kept, "failed", failed)
	}
}
