package openclaw

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// CopyRunDir copies runs/<run_id>/ of one agent's workspace into another's (DESIGN 5.6
// rule 13) and returns how many files it copied. Only directories and regular files are
// copied: symlinks and special files are skipped, never followed. A destination entry of
// another type is replaced rather than written through. Both run directories are opened
// through os.Root, so a link either agent planted cannot reach outside its workspace. A
// source without a run directory (the agent wrote nothing) copies nothing.
func CopyRunDir(stateDir, fromID, toID, runID string) (int, error) {
	return CopyRunDirExcept(stateDir, fromID, toID, runID, nil)
}

// CopyRunDirExcept is CopyRunDir that leaves out the top-level entries of runs/<run_id>/
// named in skip, with everything under them (DESIGN 5.26 rule 10: the checkout of a
// parallel branch the join does not name stays in its agent's workspace).
func CopyRunDirExcept(stateDir, fromID, toID, runID string, skip []string) (int, error) {
	return CopyRunDirAt(WorkspaceDir(stateDir, fromID), WorkspaceDir(stateDir, toID), runID, skip)
}

// CopyRunDirAt is CopyRunDirExcept for workspaces given as their directories: since DESIGN
// 5.30 rule 5 the files of a run flow from the private workspaces of earlier visits into
// the private workspace of the next one, with the checkout (repo/) in skip, since it
// moves through the run repository instead.
func CopyRunDirAt(fromWS, toWS, runID string, skip []string) (int, error) {
	srcWS, err := os.OpenRoot(fromWS)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer srcWS.Close()
	src, err := openRunDir(srcWS, runID)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrRunDirLink) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer src.Close()

	if err := EnsureRunDirAt(toWS, runID); err != nil {
		return 0, err
	}
	dstWS, err := os.OpenRoot(toWS)
	if err != nil {
		return 0, err
	}
	defer dstWS.Close()
	dst, err := dstWS.OpenRoot(filepath.Join("runs", runID))
	if err != nil {
		return 0, err
	}
	defer dst.Close()

	n := 0
	err = fs.WalkDir(src.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case p == ".":
			return nil
		case slices.Contains(skip, p):
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		case d.IsDir():
			return ensureDir(dst, p)
		case d.Type().IsRegular():
			copied, err := copyFile(src, dst, p)
			if copied {
				n++
			}
			return err
		}
		return nil
	})
	return n, err
}

// ErrRunDirLink is a runs or runs/<run_id> entry that is not a plain directory.
var ErrRunDirLink = errors.New("run directory is not a plain directory")

// OpenRunDir opens runs/<run_id>/ of an OpenClaw agent's workspace as an os.Root, so that
// everything read through it stays inside the directory (DESIGN 5.25 rule 2 reads out/
// this way). A workspace or run directory that does not exist is fs.ErrNotExist, and a
// runs or runs/<run_id> entry that is a link or any other non-directory is ErrRunDirLink
// (wrapped; test with errors.Is). The caller closes the root.
func OpenRunDir(stateDir, openclawID, runID string) (*os.Root, error) {
	ws, err := os.OpenRoot(WorkspaceDir(stateDir, openclawID))
	if err != nil {
		return nil, err
	}
	defer ws.Close()
	return openRunDir(ws, runID)
}

// OpenRunDirAt is OpenRunDir for a workspace given as its directory (the path the engine
// hands the artifact collector, DESIGN 5.25 rule 2) rather than as a state directory and
// an agent id. The same refusals apply: a missing workspace or run directory is
// fs.ErrNotExist, and a runs or runs/<run_id> entry that is a link or any other
// non-directory is ErrRunDirLink. The caller closes the root.
func OpenRunDirAt(workspace, runID string) (*os.Root, error) {
	ws, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	defer ws.Close()
	return openRunDir(ws, runID)
}

// openRunDir opens runs/<run_id>/ inside a workspace root, refusing a symlink at either
// level so the directory is the workspace's own.
func openRunDir(ws *os.Root, runID string) (*os.Root, error) {
	for _, rel := range []string{"runs", filepath.Join("runs", runID)} {
		info, err := ws.Lstat(rel)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s: %w", rel, ErrRunDirLink)
		}
	}
	return ws.OpenRoot(filepath.Join("runs", runID))
}

// ensureDir makes p a directory in dst, replacing a file or a symlink found there.
func ensureDir(dst *os.Root, p string) error {
	info, err := dst.Lstat(p)
	switch {
	case err == nil && info.IsDir():
		return nil
	case err == nil:
		if err := dst.Remove(p); err != nil {
			return err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	return dst.Mkdir(p, 0o755)
}

// copyFile replaces p in dst with src's regular file p. It reports false when the
// entry stopped being a regular file since it was listed.
func copyFile(src, dst *os.Root, p string) (bool, error) {
	in, err := src.Open(p)
	if err != nil {
		return false, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	if cur, err := dst.Lstat(p); err == nil {
		rm := dst.Remove
		if cur.IsDir() {
			rm = dst.RemoveAll
		}
		if err := rm(p); err != nil {
			return false, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	// O_EXCL: the entry was just removed, so a link raced into its place is refused.
	out, err := dst.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return false, err
	}
	return true, out.Close()
}

// ValidRunID reports whether s has the shape of a run id (a lower-case UUID), the only
// names RemoveRunDirs and ListRunDirs treat as run directories.
func ValidRunID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case (c < '0' || c > '9') && (c < 'a' || c > 'f'):
			return false
		}
	}
	return true
}

// workspaceNames lists the agent workspace directories (workspace-<id>) under the state
// directory. A workspace that is a link or not a directory is left out.
func workspaceNames(stateDir string) ([]string, error) {
	entries, err := os.ReadDir(stateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "workspace-") {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// openRunsRoot opens the runs directory of a workspace directory under stateDir as an
// os.Root; a missing one, or one that is a link or not a directory, gives nil.
func openRunsRoot(stateDir, workspace string) (*os.Root, error) {
	ws, err := os.OpenRoot(filepath.Join(stateDir, workspace))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer ws.Close()
	info, err := ws.Lstat("runs")
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !info.IsDir()) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ws.OpenRoot("runs")
}

// RemoveRunDirs deletes runs/<run_id>/ from every agent workspace under the state
// directory and returns how many workspaces had one (DESIGN 5.6 rule 13). Everything goes
// through os.Root: a link planted as runs or runs/<run_id> is never followed (a link in
// the run's place is unlinked, its target untouched), and only a valid run id is
// accepted. Failures in one workspace do not stop the others; the first error is returned.
func RemoveRunDirs(stateDir, runID string) (int, error) {
	if !ValidRunID(runID) {
		return 0, fmt.Errorf("remove run directories: %q is not a run id", runID)
	}
	names, err := workspaceNames(stateDir)
	if err != nil {
		return 0, err
	}
	n := 0
	var first error
	for _, name := range names {
		runs, err := openRunsRoot(stateDir, name)
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if runs == nil {
			continue
		}
		info, err := runs.Lstat(runID)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			if first == nil {
				first = err
			}
		default:
			rm := runs.RemoveAll
			if !info.IsDir() {
				rm = runs.Remove // a link or file in the run's place: removed, never followed
			}
			if err := rm(runID); err != nil && !errors.Is(err, fs.ErrNotExist) {
				if first == nil {
					first = err
				}
			} else {
				n++
			}
		}
		runs.Close()
	}
	return n, first
}

// RunDirInfo is one run id found under the agents' workspaces: the newest modification
// time of its runs/<run_id> entries.
type RunDirInfo struct {
	RunID   string
	ModTime time.Time
}

// ListRunDirs lists the run ids that have a runs/<run_id> entry in any agent workspace
// under the state directory. Entries whose name is not a run id are ignored.
func ListRunDirs(stateDir string) ([]RunDirInfo, error) {
	names, err := workspaceNames(stateDir)
	if err != nil {
		return nil, err
	}
	found := map[string]time.Time{}
	var first error
	for _, name := range names {
		runs, err := openRunsRoot(stateDir, name)
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if runs == nil {
			continue
		}
		f, err := runs.Open(".")
		if err == nil {
			var entries []fs.DirEntry
			entries, err = f.ReadDir(-1)
			f.Close()
			for _, e := range entries {
				if !ValidRunID(e.Name()) {
					continue
				}
				info, err := runs.Lstat(e.Name())
				if err != nil {
					continue
				}
				if t, ok := found[e.Name()]; !ok || info.ModTime().After(t) {
					found[e.Name()] = info.ModTime()
				}
			}
		}
		if err != nil && first == nil {
			first = err
		}
		runs.Close()
	}
	out := make([]RunDirInfo, 0, len(found))
	for id, t := range found {
		out = append(out, RunDirInfo{RunID: id, ModTime: t})
	}
	return out, first
}
