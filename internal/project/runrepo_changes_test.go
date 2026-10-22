package project

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yuno/internal/project/projecttest"
)

// DESIGN 5.30 rule 11 (D-090): the changes file of a later step is the run's changes so far,
// read from the run repository against where the checkout's branch is: edits, new files and
// deletions, the same files git status shows in that checkout; empty while nothing changed;
// cut with a marker past the cap.
func TestRunChangesAreTheRunsDiff(t *testing.T) {
	f := newRunRepoFixture(t, map[string]string{"README.md": "# demo\n", "old.txt": "old\n"})
	ctx := context.Background()
	if patch, err := f.cfg.RunChanges(ctx, f.runRepo, f.base, "main"); err != nil || len(patch) != 0 {
		t.Fatalf("a run with no changes: %q %v", patch, err)
	}
	one := f.checkoutPath("ws1")
	writeAt(t, filepath.Join(one, "README.md"), "# demo\nedited\n")
	writeAt(t, filepath.Join(one, "src", "new.txt"), "new\n")
	must(t, os.Remove(filepath.Join(one, "old.txt")))
	sha := f.commit(t, "ws1", f.base, "visit-1", "builder visit 1")

	patch, err := f.cfg.RunChanges(ctx, f.runRepo, sha, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"diff --git a/README.md b/README.md", "+edited", "diff --git a/src/new.txt b/src/new.txt", "new file mode",
		"+new", "diff --git a/old.txt b/old.txt", "deleted file mode", "-old"} {
		if !strings.Contains(string(patch), want) {
			t.Errorf("the changes lack %q:\n%s", want, patch)
		}
	}
	// The same files a checkout of the commit shows as the run's changes.
	two := f.checkoutPath("ws2")
	must(t, os.MkdirAll(filepath.Dir(two), 0o755))
	must(t, f.cfg.CheckoutRun(ctx, f.runRepo, sha, two, "main"))
	projecttest.Git(t, two, "add", "--all")
	if got, want := projecttest.Git(t, two, "diff", "--cached", "--name-only"), "README.md\nold.txt\nsrc/new.txt"; got != want {
		t.Fatalf("the checkout's changes = %q, want %q", got, want)
	}

	writeAt(t, filepath.Join(one, "big.txt"), strings.Repeat("a line of text\n", 1000))
	big := f.commit(t, "ws1", sha, "visit-2", "builder visit 2")
	old := diffOutputLimit
	diffOutputLimit = 4097
	t.Cleanup(func() { diffOutputLimit = old })
	patch, err = f.cfg.RunChanges(ctx, f.runRepo, big, "main")
	if err != nil || len(patch) != 4096 || !strings.HasSuffix(string(patch), changesCutMarker) {
		t.Fatalf("a long patch: %d bytes, %v, ends %q", len(patch), err, patch[max(len(patch)-80, 0):])
	}
	if _, err := f.cfg.RunChanges(ctx, f.runRepo, "HEAD", "main"); err == nil {
		t.Fatal("a commit that is not a full id was accepted")
	}
}

// A run on a merge request whose checkout has the default branch merged in (D-088): its
// changes are the agents' alone, not what the merge brought.
func TestRunChangesAfterTheDefaultBranchMerge(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n"})
	projecttest.Git(t, bare, "branch", "yuno/mr", "main")
	handCommit(t, bare, "yuno/mr", map[string]string{"feature.txt": "feature\n"})
	handCommit(t, bare, "main", map[string]string{"later.txt": "later\n"})
	f := newRunRepoFixtureOn(t, bare, "yuno/mr")
	ctx := context.Background()
	pm, err := f.cfg.MergeDefault(ctx, f.checkoutPath("ws1"), f.runRepo, "visit-1", f.fetched)
	if err != nil || pm.Commit == "" {
		t.Fatalf("merge = %+v %v", pm, err)
	}
	writeIn(t, f.checkoutPath("ws1"), "feature.txt", "feature v2\n")
	one := f.commit(t, "ws1", pm.Commit, "visit-1", "one visit 1")
	patch, err := f.cfg.RunChanges(ctx, f.runRepo, one, "yuno/mr")
	if err != nil || !strings.Contains(string(patch), "+feature v2") || strings.Contains(string(patch), "later.txt") {
		t.Fatalf("changes after the merge (%v):\n%s", err, patch)
	}
}

// WriteChanges writes the file read-only into runs/<run id>/, outside the checkout, replaces
// a link planted in its place without writing through it, and refuses a run directory that
// is a link.
func TestWriteChangesStaysInTheRunDirectory(t *testing.T) {
	ws := t.TempDir()
	runDir := filepath.Join(ws, "runs", runRepoRunID)
	must(t, os.MkdirAll(filepath.Join(runDir, RepoDir), 0o755))
	target := filepath.Join(t.TempDir(), "target")
	must(t, os.WriteFile(target, []byte("keep\n"), 0o644))
	must(t, os.Symlink(target, filepath.Join(runDir, ChangesFile)))

	must(t, WriteChanges(ws, runRepoRunID, []byte("diff --git a/x b/x\n")))
	info, err := os.Lstat(filepath.Join(runDir, ChangesFile))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o444 {
		t.Fatalf("changes file = %v %v", info, err)
	}
	if got, _ := os.ReadFile(filepath.Join(runDir, ChangesFile)); string(got) != "diff --git a/x b/x\n" {
		t.Fatalf("changes file holds %q", got)
	}
	if got, _ := os.ReadFile(target); string(got) != "keep\n" {
		t.Fatalf("the write went through the link: %q", got)
	}
	// Written again, it is replaced.
	must(t, WriteChanges(ws, runRepoRunID, nil))
	if got, err := os.ReadFile(filepath.Join(runDir, ChangesFile)); err != nil || len(got) != 0 {
		t.Fatalf("rewritten changes file = %q %v", got, err)
	}

	linked := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(linked, "runs"), 0o755))
	must(t, os.Symlink(t.TempDir(), filepath.Join(linked, "runs", runRepoRunID)))
	if err := WriteChanges(linked, runRepoRunID, []byte("x")); err == nil {
		t.Fatal("WriteChanges went through a linked run directory")
	}
}
