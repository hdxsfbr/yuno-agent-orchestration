package openclaw

import (
	"os"
	"path/filepath"
	"testing"
)

// DESIGN 5.6 rule 13: the copy takes files and directories, skips links, replaces a
// destination entry of another type and never writes through a destination link.
func TestCopyRunDir(t *testing.T) {
	state, outside := t.TempDir(), t.TempDir()
	const run = "run-1"
	src := filepath.Join(state, "workspace-agsrc", "runs", run)
	dst := filepath.Join(state, "workspace-agdst", "runs", run)
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
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(src, "notes.md"), "from src")
	write(filepath.Join(src, "app", "main.py"), "print(1)")
	write(filepath.Join(src, "was_dir"), "now a file")
	write(filepath.Join(src, "linked.txt"), "src content")
	write(filepath.Join(outside, "secret.txt"), "outside")
	link(filepath.Join(outside, "secret.txt"), filepath.Join(src, "escape.txt"))
	link(outside, filepath.Join(src, "escape_dir"))
	link("notes.md", filepath.Join(src, "alias.md"))

	write(filepath.Join(dst, "notes.md"), "old dst")
	write(filepath.Join(dst, "keep.txt"), "dst only")
	write(filepath.Join(dst, "was_dir", "inner.txt"), "x")
	write(filepath.Join(outside, "victim.txt"), "untouched")
	link(filepath.Join(outside, "victim.txt"), filepath.Join(dst, "linked.txt"))

	n, err := CopyRunDir(state, "agsrc", "agdst", run)
	if err != nil || n != 4 {
		t.Fatalf("copied %d files, err %v; want 4", n, err)
	}
	for rel, want := range map[string]string{"notes.md": "from src", "app/main.py": "print(1)", "was_dir": "now a file",
		"linked.txt": "src content", "keep.txt": "dst only"} {
		if b, err := os.ReadFile(filepath.Join(dst, rel)); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v; want %q", rel, b, err, want)
		}
	}
	for _, rel := range []string{"escape.txt", "escape_dir", "alias.md"} {
		if _, err := os.Lstat(filepath.Join(dst, rel)); !os.IsNotExist(err) {
			t.Errorf("link %s was copied: %v", rel, err)
		}
	}
	if info, err := os.Lstat(filepath.Join(dst, "linked.txt")); err != nil || !info.Mode().IsRegular() {
		t.Errorf("destination link not replaced: %v %v", info, err)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "victim.txt")); string(b) != "untouched" {
		t.Errorf("the copy wrote through a destination link: %q", b)
	}

	// A run directory that is itself a link is not the agent's own: nothing is copied.
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(outside, "planted", "x.txt"), "outside")
	link(filepath.Join(outside, "planted"), src)
	if n, err := CopyRunDir(state, "agsrc", "agdst", run); err != nil || n != 0 {
		t.Fatalf("linked run directory: copied %d, %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "x.txt")); !os.IsNotExist(err) {
		t.Fatalf("a linked run directory was followed: %v", err)
	}
	// An agent that never wrote anything copies nothing.
	if n, err := CopyRunDir(state, "agnone", "agdst", run); err != nil || n != 0 {
		t.Fatalf("missing source: copied %d, %v", n, err)
	}
}
