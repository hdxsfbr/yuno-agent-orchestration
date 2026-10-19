package skillbundle

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
)

// ExtractZip reads a skill from a zip (DESIGN 5.27 rule 6). The skill root is the directory
// that holds SKILL.md, so a single top folder is stripped; two SKILL.md files are refused.
// Directory markers are ignored; symlinks, devices and unsafe names are refused; macOS
// resource-fork entries are skipped. zipName names the skill when there is no folder and no
// front matter name. Sizes are counted while reading, so a header that lies cannot make the
// extraction bigger than the caps.
func ExtractZip(ra io.ReaderAt, size int64, zipName string) (*Bundle, error) {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, &FormatError{Msg: "the upload is not a valid zip archive"}
	}
	if len(zr.File) > maxZipEntries {
		return nil, &CapError{Msg: fmt.Sprintf("the archive has more than %d entries", maxZipEntries)}
	}
	type item struct {
		f    *zip.File
		name string
	}
	var items []item
	var skillDirs []string
	for _, f := range zr.File {
		name := f.Name
		if strings.ContainsAny(name, "\\\x00") {
			return nil, &FormatError{Msg: fmt.Sprintf("entry %q: backslashes and NUL are not allowed in names", name)}
		}
		for strings.HasPrefix(name, "./") {
			name = name[2:]
		}
		if strings.HasPrefix(name, "/") {
			return nil, &FormatError{Msg: fmt.Sprintf("entry %q: absolute paths are not allowed", f.Name)}
		}
		for _, seg := range strings.Split(name, "/") {
			if seg == ".." {
				return nil, &FormatError{Msg: fmt.Sprintf("entry %q: parent traversal is not allowed", f.Name)}
			}
		}
		if strings.HasPrefix(name, "__MACOSX/") || path.Base(name) == ".DS_Store" {
			continue
		}
		if hiddenDir(name) {
			continue
		}
		mode := f.Mode()
		if strings.HasSuffix(name, "/") || mode.IsDir() {
			continue
		}
		if mode&(fs.ModeSymlink|fs.ModeDevice|fs.ModeCharDevice|fs.ModeNamedPipe|fs.ModeSocket|fs.ModeIrregular) != 0 {
			return nil, &FormatError{Msg: fmt.Sprintf("entry %q is a symlink or special file", f.Name)}
		}
		items = append(items, item{f, name})
		if path.Base(name) == SkillFile {
			skillDirs = append(skillDirs, path.Dir(name))
		}
	}
	switch len(skillDirs) {
	case 0:
		return nil, &FormatError{Msg: "no SKILL.md in the archive"}
	case 1:
	default:
		return nil, &FormatError{Msg: "several SKILL.md in the archive (" + strings.Join(skillDirs, ", ") + "): one skill per archive"}
	}
	root := skillDirs[0]
	prefix, fallback := "", strings.TrimSuffix(path.Base(zipName), path.Ext(zipName))
	if root != "." {
		prefix, fallback = root+"/", path.Base(root)
	}
	var entries []entry
	for _, it := range items {
		if !strings.HasPrefix(it.name, prefix) {
			continue
		}
		f := it.f
		entries = append(entries, entry{rel: strings.TrimPrefix(it.name, prefix), mode: f.Mode(), open: f.Open})
	}
	return assemble(entries, assembleOpts{fallbackName: fallback})
}
