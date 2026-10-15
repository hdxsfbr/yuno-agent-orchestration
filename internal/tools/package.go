package tools

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"yuno/internal/pathcheck"
)

// packagedFile is one file read from a run directory for a release or a commit.
type packagedFile struct {
	Path    string
	Content []byte
}

// packageRunFiles reads the named paths from <workspace>/runs/<run_id>/ (DESIGN 5.7
// table). A directory contributes every regular file under it. Paths are relative to
// the run directory; a path that repeats the "runs/<run_id>/" prefix (relative to the
// workspace, as the model sees its files) is accepted too. The workspace is opened as an
// os.Root and the run directory through it, so neither a link the agent planted at
// runs/<run_id> nor one inside it reaches outside the workspace; links are refused when
// named and skipped inside a named directory. Problems the model can fix come back as
// *ModelError.
func packageRunFiles(workspace, runID string, paths []string, maxBytes int64) ([]packagedFile, error) {
	if workspace == "" || runID == "" {
		return nil, errors.New("no workspace for this call")
	}
	runRel := "runs/" + runID
	root, err := openRunDir(workspace, runRel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, &ModelError{Message: "the run directory " + runRel + "/ does not exist"}
	case errors.Is(err, errLink):
		return nil, &ModelError{Message: "the run directory " + runRel + "/ is a link; work in the directory itself"}
	case err != nil:
		return nil, err
	}
	defer root.Close()

	var out []packagedFile
	seen := map[string]bool{}
	var total int64
	add := func(p string) error {
		if seen[p] {
			return nil
		}
		f, err := root.Open(p)
		if err != nil {
			return &ModelError{Message: fmt.Sprintf("path %s: cannot be read in %s/", p, runRel)}
		}
		defer f.Close()
		if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() { // replaced since it was listed
			return &ModelError{Message: fmt.Sprintf("path %s: not a regular file", p)}
		}
		data, err := io.ReadAll(io.LimitReader(f, maxBytes-total+1))
		if err != nil {
			return &ModelError{Message: fmt.Sprintf("path %s: cannot be read in %s/", p, runRel)}
		}
		total += int64(len(data))
		if total > maxBytes {
			return &ModelError{Message: fmt.Sprintf("the files exceed the %d MiB limit", maxBytes>>20)}
		}
		seen[p] = true
		out = append(out, packagedFile{Path: p, Content: data})
		return nil
	}
	for _, raw := range paths {
		p := strings.TrimPrefix(strings.TrimSpace(raw), "./")
		p = strings.TrimPrefix(p, runRel+"/")
		if p == runRel {
			p = "."
		}
		if err := pathcheck.Valid(p); err != nil {
			return nil, &ModelError{Message: err.Error()}
		}
		p = path.Clean(p)
		info, err := statNoLinks(root, p)
		if errors.Is(err, errLink) {
			return nil, &ModelError{Message: fmt.Sprintf("path %s: links are not packaged; name the file itself", p)}
		}
		if err != nil {
			return nil, &ModelError{Message: fmt.Sprintf("path %s: not found in %s/", p, runRel)}
		}
		if !info.IsDir() {
			if !info.Mode().IsRegular() {
				return nil, &ModelError{Message: fmt.Sprintf("path %s: not a regular file", p)}
			}
			if err := add(p); err != nil {
				return nil, err
			}
			continue
		}
		var files []string
		err = fs.WalkDir(root.FS(), p, func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type().IsRegular() {
				files = append(files, name)
			}
			return nil
		})
		if err != nil {
			return nil, &ModelError{Message: fmt.Sprintf("path %s: cannot be listed in %s/", p, runRel)}
		}
		sort.Strings(files)
		for _, name := range files {
			if err := add(name); err != nil {
				return nil, err
			}
		}
	}
	if len(out) == 0 {
		return nil, &ModelError{Message: "no files to package: name at least one file in " + runRel + "/"}
	}
	return out, nil
}

// errLink is an entry, or a directory on the way to it, that is a symbolic link.
var errLink = errors.New("symbolic link")

// statNoLinks describes p inside root without following links: neither p nor any
// directory on the way to it may be one, even a link that stays inside root.
func statNoLinks(root *os.Root, p string) (fs.FileInfo, error) {
	if p == "." {
		return root.Stat(p)
	}
	var info fs.FileInfo
	parts := strings.Split(p, "/")
	for i := range parts {
		var err error
		if info, err = root.Lstat(path.Join(parts[:i+1]...)); err != nil {
			return nil, err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, errLink
		}
	}
	return info, nil
}

// openRunDir opens <workspace>/<runRel> through an os.Root of the workspace, refusing a
// link at runs or runs/<run_id> even when it stays inside the workspace.
func openRunDir(workspace, runRel string) (*os.Root, error) {
	ws, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	defer ws.Close()
	dir, err := statNoLinks(ws, runRel)
	if err != nil {
		return nil, err
	}
	root, err := ws.OpenRoot(runRel)
	if err != nil {
		return nil, err
	}
	// The directory opened is the one checked, not a link swapped in meanwhile.
	if opened, err := root.Stat("."); err != nil || !os.SameFile(dir, opened) {
		root.Close()
		return nil, errLink
	}
	return root, nil
}
