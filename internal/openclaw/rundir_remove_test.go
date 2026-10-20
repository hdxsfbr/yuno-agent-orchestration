package openclaw

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	runA = "11111111-1111-4111-8111-111111111111"
	runB = "22222222-2222-4222-8222-222222222222"
)

func put(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

func TestRemoveRunDirsClearsEveryWorkspace(t *testing.T) {
	state := t.TempDir()
	for _, ag := range []string{"a", "b"} {
		put(t, filepath.Join(state, "workspace-"+ag, "runs", runA, "repo", "main.go"))
		put(t, filepath.Join(state, "workspace-"+ag, "runs", runB, "keep.txt"))
	}
	put(t, filepath.Join(state, "workspace-c", "notes.md")) // a workspace with no runs
	n, err := RemoveRunDirs(state, runA)
	if err != nil || n != 2 {
		t.Fatalf("removed from %d workspaces, err %v; want 2", n, err)
	}
	for _, ag := range []string{"a", "b"} {
		if exists(filepath.Join(state, "workspace-"+ag, "runs", runA)) {
			t.Errorf("workspace %s still has the run", ag)
		}
		if !exists(filepath.Join(state, "workspace-"+ag, "runs", runB, "keep.txt")) {
			t.Errorf("workspace %s lost another run's folder", ag)
		}
	}
	if n, err := RemoveRunDirs(state, runA); err != nil || n != 0 {
		t.Errorf("second removal = %d, %v", n, err)
	}
}

func TestRemoveRunDirsRefusesBadIDs(t *testing.T) {
	state := t.TempDir()
	put(t, filepath.Join(state, "workspace-a", "runs", runA, "f"))
	for _, id := range []string{"", ".", "..", "../runs", runA + "/repo", "not-a-run"} {
		if _, err := RemoveRunDirs(state, id); err == nil {
			t.Errorf("%q accepted", id)
		}
	}
	if !exists(filepath.Join(state, "workspace-a", "runs", runA, "f")) {
		t.Error("a refused id removed something")
	}
}

func TestRemoveRunDirsDoesNotFollowLinks(t *testing.T) {
	state, outside := t.TempDir(), t.TempDir()
	put(t, filepath.Join(outside, runA, "secret.txt"))

	// runs/<id> is a link to a directory elsewhere: the link goes, the target stays.
	if err := os.MkdirAll(filepath.Join(state, "workspace-a", "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, runA), filepath.Join(state, "workspace-a", "runs", runA)); err != nil {
		t.Fatal(err)
	}
	// runs itself is a link: nothing is removed through it.
	if err := os.MkdirAll(filepath.Join(state, "workspace-b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(state, "workspace-b", "runs")); err != nil {
		t.Fatal(err)
	}
	// a link inside the run directory to a directory elsewhere is unlinked, not entered.
	put(t, filepath.Join(state, "workspace-c", "runs", runA, "f"))
	if err := os.Symlink(outside, filepath.Join(state, "workspace-c", "runs", runA, "out")); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveRunDirs(state, runA); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(outside, runA, "secret.txt")) {
		t.Fatal("a link was followed out of the workspace")
	}
	if exists(filepath.Join(state, "workspace-a", "runs", runA)) || exists(filepath.Join(state, "workspace-c", "runs", runA)) {
		t.Error("the run's own entries were not removed")
	}
}

func TestListRunDirs(t *testing.T) {
	state := t.TempDir()
	put(t, filepath.Join(state, "workspace-a", "runs", runA, "f"))
	put(t, filepath.Join(state, "workspace-b", "runs", runA, "f"))
	put(t, filepath.Join(state, "workspace-b", "runs", "scratch", "f")) // not a run id
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(state, "workspace-a", "runs", runA), old, old); err != nil {
		t.Fatal(err)
	}
	got, err := ListRunDirs(state)
	if err != nil || len(got) != 1 || got[0].RunID != runA {
		t.Fatalf("list = %+v, %v", got, err)
	}
	if time.Since(got[0].ModTime) > time.Hour {
		t.Errorf("mod time %v is not the newest of the two", got[0].ModTime)
	}
}
