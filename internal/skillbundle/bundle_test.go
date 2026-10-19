package skillbundle

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type zent struct {
	name string
	data string
	mode fs.FileMode // 0 means 0644; fs.ModeDir, fs.ModeSymlink for those kinds
}

func buildZip(t *testing.T, ents ...zent) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range ents {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		m := e.mode
		if m == 0 {
			m = 0o644
		}
		h.SetMode(m)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func extract(t *testing.T, zipName string, ents ...zent) (*Bundle, error) {
	t.Helper()
	b := buildZip(t, ents...)
	return ExtractZip(bytes.NewReader(b), int64(len(b)), zipName)
}

const goodMD = "---\nname: pdf-reports\ndescription: Write PDF reports\n---\n\n# PDF\n\nUse reportlab.\n"

// T5.27.5: single top folder stripped, front matter read, modes kept.
func TestExtractZipBundle(t *testing.T) {
	b, err := extract(t, "upload.zip",
		zent{name: "pdf-reports/", mode: fs.ModeDir | 0o755},
		zent{name: "pdf-reports/SKILL.md", data: goodMD},
		zent{name: "pdf-reports/scripts/run.sh", data: "#!/bin/sh\n", mode: 0o755},
		zent{name: "pdf-reports/notes.txt", data: "n", mode: 0o600},
		zent{name: "pdf-reports/templates/a.txt", data: "a", mode: 0o664},
		zent{name: "__MACOSX/pdf-reports/._SKILL.md", data: "x"},
		zent{name: "pdf-reports/.DS_Store", data: "x"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "pdf-reports" || b.Description != "Write PDF reports" || b.Body != "# PDF\n\nUse reportlab.\n" {
		t.Fatalf("bundle = %+v", b)
	}
	got := map[string]int{}
	for _, f := range b.Files {
		got[f.Path] = f.Mode
	}
	want := map[string]int{"scripts/run.sh": 0o755, "notes.txt": 0o644, "templates/a.txt": 0o644}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
}

func TestExtractZipRootWithoutFolder(t *testing.T) {
	b, err := extract(t, "My Skill.zip", zent{name: "SKILL.md", data: "# Title\n\nDoes a thing,\nacross two lines.\n\nSecond.\n"}, zent{name: "x/y.txt", data: "y"})
	if err != nil {
		t.Fatal(err)
	}
	// No front matter: the zip's file name gives the name, the first paragraph after the title the description.
	if b.Name != "my-skill" || b.Description != "Does a thing, across two lines." || len(b.Files) != 1 {
		t.Fatalf("bundle = %+v", b)
	}
}

func TestExtractZipFolderNameWithoutFrontMatter(t *testing.T) {
	b, err := extract(t, "ignored.zip", zent{name: "Some_Folder/SKILL.md", data: "Plain text paragraph.\n"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "some-folder" || b.Description != "Plain text paragraph." {
		t.Fatalf("bundle = %+v", b)
	}
}

func TestExtractZipFrontMatterForms(t *testing.T) {
	md := "\ufeff---\r\nname: crlf-skill\r\ndescription: >\r\n  folded text\r\n  over lines\r\nlicense: MIT\r\nallowed-tools: Bash\r\n---\r\n\r\nBody\r\n"
	b, err := extract(t, "a.zip", zent{name: "SKILL.md", data: md})
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "crlf-skill" || b.Description != "folded text over lines" || b.License != "MIT" || b.Body != "Body\r\n" {
		t.Fatalf("bundle = %+v", b)
	}
	if _, err := extract(t, "a.zip", zent{name: "SKILL.md", data: "---\nname: [unclosed\n---\nbody"}); !isFormat(err) {
		t.Fatalf("bad yaml: %v", err)
	}
	// An opening --- with no closing one is just text.
	b, err = extract(t, "a.zip", zent{name: "dir/SKILL.md", data: "---\nnot front matter\n"})
	if err != nil || b.Name != "dir" {
		t.Fatalf("%v %+v", err, b)
	}
}

func isFormat(err error) bool { var e *FormatError; return errors.As(err, &e) }
func isCap(err error) bool    { var e *CapError; return errors.As(err, &e) }
func isInput(err error, path string) bool {
	var e *InputError
	return errors.As(err, &e) && e.Path == path
}

func TestExtractZipStructure(t *testing.T) {
	for name, ents := range map[string][]zent{
		"no SKILL.md":      {{name: "a/readme.md", data: "x"}},
		"two SKILL.md":     {{name: "a/SKILL.md", data: goodMD}, {name: "b/SKILL.md", data: goodMD}},
		"nested SKILL.md":  {{name: "SKILL.md", data: goodMD}, {name: "examples/SKILL.md", data: goodMD}},
		"not utf8":         {{name: "SKILL.md", data: "\xff\xfe"}},
		"duplicate path":   {{name: "SKILL.md", data: goodMD}, {name: "a.txt", data: "1"}, {name: "a.txt", data: "2"}},
		"traversal":        {{name: "SKILL.md", data: goodMD}, {name: "../evil.txt", data: "x"}},
		"deep traversal":   {{name: "SKILL.md", data: goodMD}, {name: "a/../../evil.txt", data: "x"}},
		"absolute":         {{name: "SKILL.md", data: goodMD}, {name: "/etc/passwd", data: "x"}},
		"backslash":        {{name: "SKILL.md", data: goodMD}, {name: "a\\b.txt", data: "x"}},
		"symlink":          {{name: "SKILL.md", data: goodMD}, {name: "link", data: "/etc/passwd", mode: fs.ModeSymlink | 0o777}},
		"device":           {{name: "SKILL.md", data: goodMD}, {name: "dev", mode: fs.ModeDevice | 0o666}},
		"odd characters":   {{name: "SKILL.md", data: goodMD}, {name: "a b.txt", data: "x"}},
		"outside traverse": {{name: "top/SKILL.md", data: goodMD}, {name: "other/../../x", data: "x"}},
	} {
		if _, err := extract(t, "a.zip", ents...); !isFormat(err) {
			t.Errorf("%s: err = %v, want a FormatError", name, err)
		}
	}
	_, err := extract(t, "a.zip", zent{name: "a/SKILL.md", data: goodMD}, zent{name: "b/SKILL.md", data: goodMD})
	if err == nil || !strings.Contains(err.Error(), "several SKILL.md") {
		t.Errorf("two SKILL.md: %v", err)
	}
	if _, err := ExtractZip(bytes.NewReader([]byte("not a zip")), 9, "a.zip"); !isFormat(err) {
		t.Errorf("garbage: %v", err)
	}
	// SKILL.md as a bundle path is refused by CheckPath; files outside the root are ignored.
	b, err := extract(t, "a.zip", zent{name: "top/SKILL.md", data: goodMD}, zent{name: "elsewhere/x.txt", data: "x"})
	if err != nil || len(b.Files) != 0 {
		t.Errorf("outside files: %v %+v", err, b)
	}
}

func TestExtractZipNames(t *testing.T) {
	if _, err := extract(t, "a.zip", zent{name: "SKILL.md", data: "---\nname: Bad_Name\n---\nx"}); !isInput(err, "name") {
		t.Errorf("bad name: %v", err)
	}
	if _, err := extract(t, "a.zip", zent{name: "SKILL.md", data: "---\nname: ok\ndescription: " + strings.Repeat("d", 1025) + "\n---\nx"}); !isInput(err, "description") {
		t.Errorf("long description: %v", err)
	}
	if _, err := extract(t, "!!!.zip", zent{name: "SKILL.md", data: "x"}); !isInput(err, "name") {
		t.Errorf("no usable name: %v", err)
	}
}

// T5.27.6: caps, counted while reading.
func TestExtractZipCaps(t *testing.T) {
	ents := []zent{{name: "SKILL.md", data: goodMD}}
	for i := 0; i <= MaxFiles; i++ {
		ents = append(ents, zent{name: fmt.Sprintf("f/%03d.txt", i), data: "x"})
	}
	if _, err := extract(t, "a.zip", ents...); !isCap(err) {
		t.Errorf("201 files: %v", err)
	}
	ents = ents[:MaxFiles+1] // SKILL.md plus 200 files
	if _, err := extract(t, "a.zip", ents...); err != nil {
		t.Errorf("200 files: %v", err)
	}
	big := strings.Repeat("a", MaxFileBytes+1)
	if _, err := extract(t, "a.zip", zent{name: "SKILL.md", data: goodMD}, zent{name: "big.bin", data: big}); !isCap(err) {
		t.Errorf("2 MiB + 1 file: %v", err)
	}
	ok := strings.Repeat("a", MaxFileBytes)
	if _, err := extract(t, "a.zip", zent{name: "SKILL.md", data: goodMD}, zent{name: "ok.bin", data: ok}); err != nil {
		t.Errorf("2 MiB file: %v", err)
	}
	var many []zent
	many = append(many, zent{name: "SKILL.md", data: goodMD})
	for i := 0; i < 11; i++ {
		many = append(many, zent{name: fmt.Sprintf("m%d.bin", i), data: ok})
	}
	if _, err := extract(t, "a.zip", many...); !isCap(err) {
		t.Errorf("22 MiB total: %v", err)
	}
	if _, err := extract(t, "a.zip", zent{name: "SKILL.md", data: "---\nname: x\n---\n" + strings.Repeat("b", MaxBodyBytes+1)}); !isCap(err) {
		t.Errorf("body over 256 KiB: %v", err)
	}
}

// A zip bomb: a few KiB that inflate to far more than the cap. The extraction stops at the
// cap because it counts what it reads, whatever the header says.
func TestExtractZipBomb(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("SKILL.md")
	w.Write([]byte(goodMD))
	bw, _ := zw.Create("bomb.bin")
	chunk := make([]byte, 1<<20)
	for i := 0; i < 48; i++ {
		bw.Write(chunk)
	}
	zw.Close()
	if buf.Len() > 1<<20 {
		t.Fatalf("test archive is %d bytes, not a bomb", buf.Len())
	}
	_, err := ExtractZip(bytes.NewReader(buf.Bytes()), int64(buf.Len()), "a.zip")
	if !isCap(err) {
		t.Fatalf("bomb: %v", err)
	}
	// Spread over many files under the per-file cap, the total cap stops it.
	buf.Reset()
	zw = zip.NewWriter(&buf)
	w, _ = zw.Create("SKILL.md")
	w.Write([]byte(goodMD))
	for i := 0; i < 15; i++ {
		bw, _ = zw.Create(fmt.Sprintf("b%d.bin", i))
		bw.Write(make([]byte, MaxFileBytes))
	}
	zw.Close()
	if _, err := ExtractZip(bytes.NewReader(buf.Bytes()), int64(buf.Len()), "a.zip"); !isCap(err) {
		t.Fatalf("spread bomb: %v", err)
	}
}

// A header that lies about the sizes is refused or capped, never trusted.
func TestExtractZipLyingHeader(t *testing.T) {
	var comp bytes.Buffer
	fw, _ := flate.NewWriter(&comp, flate.BestCompression)
	chunk := make([]byte, 1<<20)
	crc := crc32.NewIEEE()
	for i := 0; i < 8; i++ {
		fw.Write(chunk)
		crc.Write(chunk)
	}
	fw.Close()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("SKILL.md")
	w.Write([]byte(goodMD))
	rw, err := zw.CreateRaw(&zip.FileHeader{Name: "bomb.bin", Method: zip.Deflate, CRC32: crc.Sum32(),
		CompressedSize64: uint64(comp.Len()), UncompressedSize64: 10})
	if err != nil {
		t.Fatal(err)
	}
	rw.Write(comp.Bytes())
	zw.Close()
	if _, err := ExtractZip(bytes.NewReader(buf.Bytes()), int64(buf.Len()), "a.zip"); err == nil {
		t.Fatal("lying header accepted")
	}
}

func TestCheckPath(t *testing.T) {
	for _, p := range []string{"a.txt", "scripts/run.sh", "a/b/c-d_e.f", ".env.example", "LICENSE"} {
		if err := CheckPath(p); err != nil {
			t.Errorf("%q refused: %v", p, err)
		}
	}
	for _, p := range []string{"", "SKILL.md", "../x", "a/../b", "/abs", "a//b", "./a", "a/", "a\\b", "a\x00b", ".hidden/x", "a/.hidden/x", "a b", strings.Repeat("a", 201), "."} {
		if err := CheckPath(p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestReadDir(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, data string, mode os.FileMode) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, mode)
	}
	write("SKILL.md", goodMD, 0o644)
	write("scripts/go.sh", "#!/bin/sh", 0o755)
	write(".git/HEAD", "ref", 0o644)
	b, err := ReadDir(dir, "fallback", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Files) != 1 || b.Files[0].Path != "scripts/go.sh" || b.Files[0].Mode != 0o755 {
		t.Fatalf("files = %+v", b.Files)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDir(dir, "fallback", false); !isFormat(err) {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := ReadDir(t.TempDir(), "x", false); !isFormat(err) {
		t.Fatalf("empty dir: %v", err)
	}
}

func TestSkillMDRoundTrip(t *testing.T) {
	fm, body, has, err := ParseSkillMD(SkillMD(skillOf("round-trip", `He said "hi": yes`, "# T\n\ntext\n", "MIT")))
	if err != nil || !has || fm.Name != "round-trip" || fm.Description != `He said "hi": yes` || fm.License != "MIT" || body != "# T\n\ntext\n" {
		t.Fatalf("%v %v %+v %q", err, has, fm, body)
	}
}

func filePaths(b *Bundle) []string {
	var out []string
	for _, f := range b.Files {
		out = append(out, f.Path)
	}
	return out
}

// Finding 4: hidden directories, .git at any depth and hidden top-level files are skipped,
// not refused; traversal still is.
func TestHiddenEntriesAreSkipped(t *testing.T) {
	b, err := extract(t, "a.zip",
		zent{name: "pdf/SKILL.md", data: goodMD},
		zent{name: "pdf/.github/workflows/ci.yml", data: "x"},
		zent{name: "pdf/.git/config", data: "x"},
		zent{name: "pdf/.git/hooks/link", data: "/etc/passwd", mode: fs.ModeSymlink | 0o777},
		zent{name: "pdf/vendor/.git/HEAD", data: "x"},
		zent{name: "pdf/.gitignore", data: "x"},
		zent{name: "pdf/.platform-files-not", data: "x"},
		zent{name: "pdf/docs/.gitkeep", data: ""},
		zent{name: "pdf/docs/guide.md", data: "g"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(filePaths(b), ","); got != "docs/.gitkeep,docs/guide.md" {
		t.Fatalf("zip files = %s", got)
	}
	for _, name := range []string{"../evil.txt", ".github/../../evil.txt"} {
		if _, err := extract(t, "a.zip", zent{name: "SKILL.md", data: goodMD}, zent{name: name, data: "x"}); !isFormat(err) {
			t.Errorf("%s: err = %v, want a FormatError", name, err)
		}
	}

	dir := t.TempDir()
	put := func(rel, data string) {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("SKILL.md", goodMD)
	put(".github/workflows/ci.yml", "x")
	put(".git/config", "x")
	put("vendor/.git/HEAD", "x")
	put(".gitignore", "x")
	put("scripts/run.sh", "#!/bin/sh\n")
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, ".git", "link")); err != nil {
		t.Fatal(err)
	}
	b, err = ReadDir(dir, "pdf", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(filePaths(b), ","); got != "scripts/run.sh" {
		t.Fatalf("dir files = %s", got)
	}
	// A symlink outside hidden folders is still refused.
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "scripts", "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDir(dir, "pdf", false); !isFormat(err) {
		t.Fatalf("visible symlink: %v", err)
	}
	// The API refuses a hidden directory in a path it is given, and the manifest name.
	if CheckPath(".hidden/x") == nil || CheckPath(".platform-files") == nil {
		t.Fatal("CheckPath must still refuse these")
	}
}
