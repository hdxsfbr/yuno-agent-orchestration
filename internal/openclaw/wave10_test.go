package openclaw

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"yuno/internal/tools"
)

// DESIGN 5.24 rules 1 and 9: the two question tools are platform tools, so an allowlist
// entry reaches the runtime as yuno__ask_operator and yuno__answer_question.
func TestQuestionToolsAreMaterialisedWithThePrefix(t *testing.T) {
	for _, name := range []string{tools.AskOperatorName, tools.AnswerQuestionName} {
		if got := ToolName(name); got != PlatformToolPrefix+name {
			t.Errorf("ToolName(%q) = %q, want %q", name, got, PlatformToolPrefix+name)
		}
	}
}

// DESIGN 5.25 rule 2: OpenRunDir opens runs/<run id>/ of a workspace as a root the
// caller reads out/ through; a missing workspace or run directory is fs.ErrNotExist and a
// link at either level is ErrRunDirLink, never followed.
func TestOpenRunDir(t *testing.T) {
	state, outside := t.TempDir(), t.TempDir()
	const run = "run-1"
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	read := func(root *os.Root, rel string) (string, error) {
		f, err := root.Open(rel)
		if err != nil {
			return "", err
		}
		defer f.Close()
		b, err := io.ReadAll(f)
		return string(b), err
	}

	// A plain run directory opens, and reads stay inside it.
	write(filepath.Join(state, "workspace-agok", "runs", run, "out", "report.md"), "# report")
	write(filepath.Join(outside, "secret.txt"), "outside")
	link(filepath.Join(outside, "secret.txt"), filepath.Join(state, "workspace-agok", "runs", run, "out", "escape.txt"))
	root, err := OpenRunDir(state, "agok", run)
	if err != nil {
		t.Fatalf("plain run directory: %v", err)
	}
	defer root.Close()
	if got, err := read(root, "out/report.md"); err != nil || got != "# report" {
		t.Errorf("out/report.md = %q, %v", got, err)
	}
	if got, err := read(root, "out/escape.txt"); err == nil {
		t.Errorf("a link out of the run directory was followed: %q", got)
	}

	// Missing: no workspace, no runs directory, no run directory.
	if err := os.MkdirAll(filepath.Join(state, "workspace-agempty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(state, "workspace-agnorun", "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]string{"no workspace": "agmissing", "no runs": "agempty", "no run directory": "agnorun"} {
		root, err := OpenRunDir(state, id, run)
		if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrRunDirLink) {
			t.Errorf("%s: err = %v, want fs.ErrNotExist", name, err)
		}
		if root != nil {
			root.Close()
		}
	}

	// A link at runs/<run id> or at runs: refused, whether it points outside or inside.
	link(outside, filepath.Join(state, "workspace-aglinkrun", "runs", run))
	link(outside, filepath.Join(state, "workspace-aglinkruns", "runs"))
	link(filepath.Join(state, "workspace-agok", "runs", run), filepath.Join(state, "workspace-aginside", "runs"))
	for name, id := range map[string]string{"linked run directory": "aglinkrun", "linked runs": "aglinkruns", "runs linked inside": "aginside"} {
		root, err := OpenRunDir(state, id, run)
		if !errors.Is(err, ErrRunDirLink) {
			t.Errorf("%s: err = %v, want ErrRunDirLink", name, err)
		}
		if root != nil {
			root.Close()
		}
	}
	// A run directory that is a file is not a plain directory either.
	write(filepath.Join(state, "workspace-agfile", "runs", run), "not a directory")
	if _, err := OpenRunDir(state, "agfile", run); !errors.Is(err, ErrRunDirLink) {
		t.Errorf("run directory that is a file: err = %v, want ErrRunDirLink", err)
	}
}
