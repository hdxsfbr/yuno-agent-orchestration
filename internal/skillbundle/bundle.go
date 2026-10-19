// Package skillbundle turns an uploaded zip, a Git directory or a library directory into a
// skill (DESIGN 5.27): SKILL.md's front matter and body, plus the other files with their
// modes, under the caps of rule 5. Nothing here executes anything from a bundle.
package skillbundle

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"yuno/internal/pathcheck"
	"yuno/internal/store"
)

// Caps of DESIGN 5.27 rules 5 to 7, counted while reading.
const (
	MaxFiles       = 200
	MaxFileBytes   = 2 << 20
	MaxTotalBytes  = 20 << 20
	MaxBodyBytes   = 256 << 10
	MaxDescription = 1024
	MaxNameLen     = 64
	MaxZipBytes    = 25 << 20
	MaxCloneBytes  = 50 << 20

	// maxZipEntries bounds the central directory a request can make us walk.
	maxZipEntries = 5000

	// SkillFile is the skill body's file name; it is never a bundle file.
	SkillFile = "SKILL.md"
)

// NamePattern is the skills table's name rule (DESIGN section 4).
var NamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// FormatError is a bundle that is malformed (HTTP 400).
type FormatError struct{ Msg string }

func (e *FormatError) Error() string { return e.Msg }

// CapError is a bundle over a cap (HTTP 413); Msg names the cap.
type CapError struct{ Msg string }

func (e *CapError) Error() string { return e.Msg }

// InputError is a field the caller supplied or the front matter gave that is not
// acceptable (HTTP 422), qualified by its path (name, description, url, ref, path).
type InputError struct{ Path, Msg string }

func (e *InputError) Error() string { return e.Path + ": " + e.Msg }

// Bundle is a parsed skill: the SKILL.md fields and the other files (Mode 0644 or 0755).
type Bundle struct {
	Name        string
	Description string
	Body        string
	License     string
	Files       []store.SkillFile
}

// CheckPath validates a bundle file path: relative, clean, no "..", no backslash or NUL
// (internal/pathcheck), not SKILL.md, no hidden directories.
func CheckPath(p string) error {
	if p == SkillFile {
		return errors.New("SKILL.md is the skill's body, not a bundle file")
	}
	if err := pathcheck.Valid(p); err != nil {
		return err
	}
	if p == "." || path.Clean(p) != p {
		return fmt.Errorf("path %q: must be clean (no ./, // or trailing /)", p)
	}
	if p == store.SkillManifestFile {
		return fmt.Errorf("path %q is reserved for the platform's file list", p)
	}
	segs := strings.Split(p, "/")
	for _, s := range segs[:len(segs)-1] {
		if strings.HasPrefix(s, ".") {
			return fmt.Errorf("path %q: hidden directories are not allowed", p)
		}
	}
	return nil
}

// hiddenDir reports whether a relative path lies under a hidden directory or, at any depth,
// under .git. Such files are skipped on import, not refused.
func hiddenDir(p string) bool {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if s == ".git" || (i < len(segs)-1 && strings.HasPrefix(s, ".")) {
			return true
		}
	}
	return false
}

// skipEntry reports whether an entry relative to the skill root is left out of the bundle:
// anything under a hidden directory or .git, and hidden files at the top level
// (.gitignore, .DS_Store).
func skipEntry(rel string) bool {
	return hiddenDir(rel) || (!strings.Contains(rel, "/") && strings.HasPrefix(rel, "."))
}

// ModeFor is 0755 when any execute bit is set and 0644 otherwise.
func ModeFor(m fs.FileMode) int {
	if m.Perm()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

// entry is a file found in an archive or directory, relative to the skill root.
type entry struct {
	rel  string
	mode fs.FileMode
	open func() (io.ReadCloser, error)
}

type assembleOpts struct {
	// fallbackName names the skill when the front matter has no name (folder or file
	// name); it is turned into a valid skill name.
	fallbackName string
	// truncateDescription cuts an over-long description instead of refusing it (library).
	truncateDescription bool
}

// assemble checks paths and caps, reads every entry (counting what is actually read, never
// a header's size) and parses SKILL.md.
func assemble(entries []entry, o assembleOpts) (*Bundle, error) {
	var skill *entry
	var rest []entry
	for i := range entries {
		e := entries[i]
		if skipEntry(e.rel) {
			continue
		}
		if e.rel == SkillFile {
			skill = &entries[i]
			continue
		}
		if err := CheckPath(e.rel); err != nil {
			return nil, &FormatError{Msg: err.Error()}
		}
		rest = append(rest, e)
	}
	if skill == nil {
		return nil, &FormatError{Msg: "no SKILL.md at the root of the bundle"}
	}
	if len(rest) > MaxFiles {
		return nil, &CapError{Msg: fmt.Sprintf("the bundle has more than %d files", MaxFiles)}
	}
	slices.SortFunc(rest, func(a, b entry) int { return strings.Compare(a.rel, b.rel) })
	for i := 1; i < len(rest); i++ {
		if rest[i].rel == rest[i-1].rel {
			return nil, &FormatError{Msg: fmt.Sprintf("path %q appears twice", rest[i].rel)}
		}
	}
	var total int64
	read := func(e entry) ([]byte, error) {
		rc, err := e.open()
		if err != nil {
			return nil, &FormatError{Msg: fmt.Sprintf("%s: %v", e.rel, err)}
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, MaxFileBytes+1))
		if err != nil {
			return nil, &FormatError{Msg: fmt.Sprintf("%s: %v", e.rel, err)}
		}
		if len(data) > MaxFileBytes {
			return nil, &CapError{Msg: fmt.Sprintf("%s is over the %d MiB per-file cap", e.rel, MaxFileBytes>>20)}
		}
		total += int64(len(data))
		if total > MaxTotalBytes {
			return nil, &CapError{Msg: fmt.Sprintf("the bundle is over the %d MiB total cap", MaxTotalBytes>>20)}
		}
		return data, nil
	}
	md, err := read(*skill)
	if err != nil {
		return nil, err
	}
	files := make([]store.SkillFile, 0, len(rest))
	for _, e := range rest {
		data, err := read(e)
		if err != nil {
			return nil, err
		}
		files = append(files, store.SkillFile{Path: e.rel, Content: data, Mode: ModeFor(e.mode), Size: len(data)})
	}
	b, err := parseSkill(md, o)
	if err != nil {
		return nil, err
	}
	b.Files = files
	return b, nil
}

func parseSkill(md []byte, o assembleOpts) (*Bundle, error) {
	if !utf8.Valid(md) || bytes.IndexByte(md, 0) >= 0 {
		return nil, &FormatError{Msg: "SKILL.md is not UTF-8 text"}
	}
	fm, body, _, err := ParseSkillMD(string(md))
	if err != nil {
		return nil, &FormatError{Msg: err.Error()}
	}
	if len(body) > MaxBodyBytes {
		return nil, &CapError{Msg: fmt.Sprintf("the SKILL.md body is over the %d KiB cap", MaxBodyBytes>>10)}
	}
	b := &Bundle{Name: fm.Name, Description: fm.Description, Body: body, License: fm.License}
	if b.Name == "" {
		b.Name = Slug(o.fallbackName)
		if b.Name == "" {
			return nil, &InputError{Path: "name", Msg: "SKILL.md has no name and the folder or file name gives none"}
		}
	}
	if len(b.Name) > MaxNameLen || !NamePattern.MatchString(b.Name) {
		return nil, &InputError{Path: "name", Msg: fmt.Sprintf("%q must be lowercase letters and digits in words separated by single hyphens, at most %d characters", b.Name, MaxNameLen)}
	}
	if b.Description == "" {
		b.Description = FirstParagraph(body)
	}
	if utf8.RuneCountInString(b.Description) > MaxDescription {
		if !o.truncateDescription {
			return nil, &InputError{Path: "description", Msg: fmt.Sprintf("must be at most %d characters", MaxDescription)}
		}
		b.Description = string([]rune(b.Description)[:MaxDescription])
	}
	return b, nil
}

// FrontMatter is the part of SKILL.md's YAML header the platform keeps.
type FrontMatter struct {
	Name, Description, License string
}

// ParseSkillMD splits a SKILL.md into its front matter (YAML between "---" lines at the very
// start) and the body, which has its leading blank lines trimmed. has reports whether a
// front matter block was found.
func ParseSkillMD(src string) (fm FrontMatter, body string, has bool, err error) {
	src = strings.TrimPrefix(src, "\ufeff")
	lines := strings.SplitAfter(src, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], " \t\r\n") != "---" {
		return fm, src, false, nil
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t\r\n") == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return fm, src, false, nil
	}
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "")), &raw); err != nil {
		return fm, "", true, fmt.Errorf("the front matter of SKILL.md is not valid YAML: %v", firstLine(err.Error()))
	}
	str := func(k string) string {
		s, _ := raw[k].(string)
		return strings.TrimSpace(s)
	}
	fm = FrontMatter{Name: str("name"), Description: str("description"), License: str("license")}
	body = strings.TrimLeft(strings.Join(lines[end+1:], ""), "\r\n")
	return fm, body, true, nil
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// FirstParagraph is the first paragraph of text after the title, on one line.
func FirstParagraph(body string) string {
	var para []string
	for _, l := range strings.Split(body, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case t == "" || strings.HasPrefix(t, "#"):
			if len(para) > 0 {
				return strings.Join(strings.Fields(strings.Join(para, " ")), " ")
			}
		default:
			para = append(para, t)
		}
	}
	return strings.Join(strings.Fields(strings.Join(para, " ")), " ")
}

// Slug lowercases s and turns every run of other characters into one hyphen, so a folder or
// file name becomes a skill name.
func Slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		} else {
			dash = true
		}
	}
	out := b.String()
	if len(out) > MaxNameLen {
		out = strings.TrimRight(out[:MaxNameLen], "-")
	}
	return out
}
