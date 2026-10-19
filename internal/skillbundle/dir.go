package skillbundle

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
)

// ReadDir reads a skill from a directory tree (a Git checkout or a library entry). SKILL.md
// must be at its root. Symlinks and special files are refused; hidden directories (.github,
// .git at any depth) and hidden top-level files are skipped; the caps apply as for a zip.
// fallbackName names the skill when SKILL.md has no name.
func ReadDir(dir, fallbackName string, truncateDescription bool) (*Bundle, error) {
	return ReadDirWith(dir, fallbackName, truncateDescription)
}

// ExtraFile is a file added to a directory's bundle unless the directory has one at Path;
// it counts against the caps like any other.
type ExtraFile struct {
	Path    string
	Content []byte
	Mode    int
}

// ReadDirWith is ReadDir with extra files added before the caps are applied.
func ReadDirWith(dir, fallbackName string, truncateDescription bool, extra ...ExtraFile) (*Bundle, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, &FormatError{Msg: "cannot open the skill directory"}
	}
	defer root.Close()
	var entries []entry
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if d.IsDir() {
			if hiddenDir(p + "/x") {
				return fs.SkipDir
			}
			return nil
		}
		if skipEntry(p) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return &FormatError{Msg: fmt.Sprintf("%s is a symlink or special file", p)}
		}
		if len(entries) > MaxFiles+1 {
			return &CapError{Msg: fmt.Sprintf("the bundle has more than %d files", MaxFiles)}
		}
		entries = append(entries, entry{rel: p, mode: info.Mode(), open: func() (io.ReadCloser, error) { return root.Open(p) }})
		return nil
	})
	if err != nil {
		var fe *FormatError
		var ce *CapError
		if errors.As(err, &fe) || errors.As(err, &ce) {
			return nil, err
		}
		return nil, &FormatError{Msg: "cannot read the skill directory"}
	}
	for _, x := range extra {
		if slices.ContainsFunc(entries, func(e entry) bool { return e.rel == x.Path }) {
			continue
		}
		data, mode := x.Content, fs.FileMode(x.Mode)
		entries = append(entries, entry{rel: x.Path, mode: mode, open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }})
	}
	return assemble(entries, assembleOpts{fallbackName: fallbackName, truncateDescription: truncateDescription})
}
