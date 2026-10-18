package openclaw

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// CopyRunDir copies runs/<run_id>/ of one agent's workspace into another's (DESIGN 5.6
// rule 13) and returns how many files it copied. Only directories and regular files are
// copied: symlinks and special files are skipped, never followed. A destination entry of
// another type is replaced rather than written through. Both run directories are opened
// through os.Root, so a link either agent planted cannot reach outside its workspace. A
// source without a run directory (the agent wrote nothing) copies nothing.
func CopyRunDir(stateDir, fromID, toID, runID string) (int, error) {
	srcWS, err := os.OpenRoot(WorkspaceDir(stateDir, fromID))
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

	if err := EnsureRunDir(stateDir, toID, runID); err != nil {
		return 0, err
	}
	dstWS, err := os.OpenRoot(WorkspaceDir(stateDir, toID))
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
