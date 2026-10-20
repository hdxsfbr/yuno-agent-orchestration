package engine

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/project"
	"yuno/internal/store"
)

// Run folders: each step of a run works in a private workspace of its own (DESIGN 5.30),
// recorded on its visit, and a project run's checkout moves between steps through the run
// repository <state>/yuno-runs/<run id>.git. Before 5.30 the steps shared runs/<run id>/
// in the agents' workspaces (5.6 rule 13); runs from then still have those folders. A
// run's artifacts and diff are in the artifact store once each visit ends, so all of them
// go when nothing can need them. A retry (5.21) resumes a failed, timed-out or cancelled
// run at the node it stopped at and starts from the earlier steps' workspaces and commits,
// so those runs keep theirs for runDirRetention.

const (
	// runDirRetention is how long a failed, timed-out or cancelled run keeps its folders
	// for a retry, and how old a folder of a run the database does not know must be to go.
	runDirRetention = 7 * 24 * time.Hour
	// runDirSweepEvery is the cadence of the folder sweep.
	runDirSweepEvery = time.Hour
)

// removeRunDirsAfterTerminal deletes the folders of a run that just succeeded: its steps'
// private workspaces, its run repository and any runs/<run id>/ an agent workspace holds;
// a run that failed, timed out or was cancelled keeps them, since it can be retried. It
// runs after the run's visits ended, so their artifact collection is done.
func (e *Engine) removeRunDirsAfterTerminal(ctx context.Context, run *store.Run) {
	if run == nil || e.deps.StateDir == "" {
		return
	}
	if run.Status != store.RunSucceeded {
		return
	}
	n, err := e.removeRunFolders(context.WithoutCancel(ctx), run.ID)
	if err != nil {
		e.log.Warn("run folders not fully removed", "run_id", run.ID, "removed", n, "error", err)
		return
	}
	if n > 0 {
		e.log.Info("run folders removed", "run_id", run.ID, "folders", n)
	}
}

// removeRunFolders removes every folder of a run: the private workspaces its visits
// recorded, its run repository, and its runs/<run id>/ in the agents' workspaces. A
// failure does not stop the others; the first error is returned with the count removed.
func (e *Engine) removeRunFolders(ctx context.Context, runID string) (int, error) {
	n, first := 0, error(nil)
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	var dirs []string
	if e.deps.Pool != nil {
		var err error
		dirs, err = store.RunWorkspaceDirs(ctx, e.deps.Pool, runID)
		keep(err)
	}
	for _, dir := range dirs {
		if err := openclaw.RemoveSandboxWorkspace(e.deps.StateDir, dir); err != nil {
			keep(err)
			continue
		}
		n++
	}
	removed, err := removeRunRepo(e.deps.StateDir, runID)
	keep(err)
	if removed {
		n++
	}
	m, err := openclaw.RemoveRunDirs(e.deps.StateDir, runID)
	keep(err)
	return n + m, first
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

// sweepRunDirs removes the folders of runs that succeeded, and of runs failed, timed out
// or cancelled for more than runDirRetention: the private workspaces their visits
// recorded, their run repositories and their runs/<run id>/ folders. A run repository or
// runs/<run id>/ of a run id the database does not know goes once it is older than that.
// A folder under sandboxes/ that no visit recorded (the Chief of Staff's chat session
// among them) is never touched. It logs counts only.
func (e *Engine) sweepRunDirs(ctx context.Context) {
	if e.deps.StateDir == "" || e.deps.Pool == nil {
		return
	}
	dirs, err := openclaw.ListRunDirs(e.deps.StateDir)
	if err != nil {
		e.log.Warn("run folder sweep: listing failed", "error", err)
	}
	repos, err := listRunRepos(e.deps.StateDir)
	if err != nil {
		e.log.Warn("run folder sweep: listing run repositories failed", "error", err)
	}
	workspaces, err := openclaw.ListSandboxWorkspaces(e.deps.StateDir)
	if err != nil {
		e.log.Warn("run folder sweep: listing workspaces failed", "error", err)
	}
	owners := map[string]string{}
	if len(workspaces) > 0 {
		if owners, err = store.WorkspaceOwners(ctx, e.deps.Pool, workspaces); err != nil {
			e.log.Warn("run folder sweep: workspace lookup failed", "error", err)
			return
		}
	}
	if len(dirs) == 0 && len(repos) == 0 && len(owners) == 0 {
		return
	}
	idSet := map[string]bool{}
	for _, d := range dirs {
		idSet[d.RunID] = true
	}
	for _, d := range repos {
		idSet[d.RunID] = true
	}
	for _, id := range owners {
		idSet[id] = true
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
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
	// due says whether a folder of runID goes; modTime is the folder's age, which only an
	// unknown run's folder is judged by.
	due := func(runID string, modTime time.Time) bool {
		k, inDB := runs[runID]
		switch {
		case !inDB:
			return modTime.Before(cutoff)
		case k.status == store.RunSucceeded:
			return true
		case k.status == store.RunFailed || k.status == store.RunTimedOut || k.status == store.RunCancelled:
			return k.finished != nil && k.finished.Before(cutoff)
		}
		return false
	}
	removed, kept, failed := 0, 0, 0
	count := func(ok bool, err error) {
		switch {
		case !ok:
			kept++
		case err != nil:
			failed++
		default:
			removed++
		}
	}
	for _, d := range dirs {
		if !due(d.RunID, d.ModTime) {
			count(false, nil)
			continue
		}
		_, err := openclaw.RemoveRunDirs(e.deps.StateDir, d.RunID)
		count(true, err)
	}
	for _, d := range repos {
		if !due(d.RunID, d.ModTime) {
			count(false, nil)
			continue
		}
		_, err := removeRunRepo(e.deps.StateDir, d.RunID)
		count(true, err)
	}
	for dir, runID := range owners {
		// A recorded workspace belongs to a run the database knows, unless the run was
		// deleted meanwhile; then it is never judged by its age.
		if _, inDB := runs[runID]; !inDB || !due(runID, time.Time{}) {
			count(false, nil)
			continue
		}
		count(true, openclaw.RemoveSandboxWorkspace(e.deps.StateDir, dir))
	}
	if removed > 0 || failed > 0 {
		e.log.Info("run folder sweep", "removed", removed, "kept", kept, "failed", failed)
	}
}

// runRepoInfo is one run repository found under <state>/yuno-runs: the run id its name
// carries and its modification time.
type runRepoInfo struct {
	RunID   string
	ModTime time.Time
}

// listRunRepos lists the run repositories under <state>/yuno-runs whose names are
// <run id>.git; anything else there is ignored.
func listRunRepos(stateDir string) ([]runRepoInfo, error) {
	entries, err := os.ReadDir(filepath.Join(stateDir, project.RunReposDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []runRepoInfo
	for _, ent := range entries {
		id, ok := strings.CutSuffix(ent.Name(), ".git")
		if !ok || !openclaw.ValidRunID(id) {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		out = append(out, runRepoInfo{RunID: id, ModTime: info.ModTime()})
	}
	return out, nil
}

// removeRunRepo deletes the run repository of runID through an os.Root of
// <state>/yuno-runs, so a link in its place is unlinked and never followed. It reports
// whether there was one.
func removeRunRepo(stateDir, runID string) (bool, error) {
	if !openclaw.ValidRunID(runID) {
		return false, errors.New("remove run repository: not a run id")
	}
	root, err := os.OpenRoot(filepath.Join(stateDir, project.RunReposDir))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer root.Close()
	name := runID + ".git"
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	rm := root.RemoveAll
	if !info.IsDir() {
		rm = root.Remove
	}
	if err := rm(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return true, nil
}
