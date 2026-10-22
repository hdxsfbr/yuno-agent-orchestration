package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"yuno/internal/project/projecttest"
)

// Chief platform control, DESIGN wave 3 rules 9 and 10, against a local bare repository:
// the platform merges the default branch into a request's branch and pushes the merge,
// never forced.

// branchFrom makes branch from the tip of from in the bare repository with one commit of
// files, as an agent's push does, and returns the branch's head.
func branchFrom(t *testing.T, bare, from, branch string, files map[string]string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "branch")
	projecttest.Git(t, filepath.Dir(work), "clone", "--quiet", "--branch", from, bare, work)
	projecttest.Git(t, work, "checkout", "--quiet", "-b", branch)
	for name, body := range files {
		p := filepath.Join(work, filepath.FromSlash(name))
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(body), 0o644))
	}
	projecttest.Git(t, work, "add", "--all")
	projecttest.Git(t, work, "commit", "--quiet", "-m", "the request's work")
	projecttest.Git(t, work, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
	return projecttest.Git(t, work, "rev-parse", "HEAD")
}

func tipOf(t *testing.T, bare, branch string) string {
	t.Helper()
	return projecttest.Git(t, bare, "rev-parse", "refs/heads/"+branch)
}

func TestUpdateFromDefault(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (Config, string, string) {
		t.Helper()
		bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n", "app.txt": "app v1\n"})
		cfg := Config{RepoURL: bare, Token: "glpat-not-a-real-token-0", DefaultBranch: "main", Provider: ProviderGitLab}
		head := branchFrom(t, bare, "main", "yuno/t1", map[string]string{"feature.txt": "feature\n"})
		return cfg, bare, head
	}

	t.Run("up to date", func(t *testing.T) {
		cfg, bare, head := setup(t)
		got, err := cfg.UpdateFromDefault(ctx, "yuno/t1", head)
		if err != nil || got.Outcome != UpdateUpToDate || got.Head != head || got.Default != tipOf(t, bare, "main") || got.Commit != "" {
			t.Fatalf("update = %+v %v", got, err)
		}
		if tipOf(t, bare, "yuno/t1") != head {
			t.Fatal("an up-to-date branch was pushed")
		}
		// A branch that has the default branch's newer commit merged in already is up to
		// date as well (DESIGN wave 3 edge cases).
		handCommit(t, bare, "main", map[string]string{"main.txt": "main\n"})
		merged, err := cfg.UpdateFromDefault(ctx, "yuno/t1", head)
		if err != nil || merged.Outcome != UpdateUpdated {
			t.Fatalf("first update = %+v %v", merged, err)
		}
		again, err := cfg.UpdateFromDefault(ctx, "yuno/t1", merged.Commit)
		if err != nil || again.Outcome != UpdateUpToDate || tipOf(t, bare, "yuno/t1") != merged.Commit {
			t.Fatalf("second update = %+v %v", again, err)
		}
	})

	t.Run("clean push", func(t *testing.T) {
		cfg, bare, head := setup(t)
		mainTip := handCommit(t, bare, "main", map[string]string{"main.txt": "from main\n", "app.txt": "app v2\n"})
		got, err := cfg.UpdateFromDefault(ctx, "yuno/t1", head)
		if err != nil || got.Outcome != UpdateUpdated || got.Head != head || got.Default != mainTip || !IsCommitID(got.Commit) {
			t.Fatalf("update = %+v %v", got, err)
		}
		if tipOf(t, bare, "yuno/t1") != got.Commit {
			t.Fatal("the merge was not pushed")
		}
		// A two-parent merge commit by the platform: the branch's head, then main's.
		if parents := projecttest.Git(t, bare, "log", "-1", "--format=%P", got.Commit); parents != head+" "+mainTip {
			t.Fatalf("parents = %q", parents)
		}
		if author := projecttest.Author(t, bare, "yuno/t1"); author != CommitName+" <"+CommitEmail+">" {
			t.Fatalf("author = %q", author)
		}
		if subject := projecttest.Subject(t, bare, "yuno/t1"); subject != "Merge branch 'main' into yuno/t1" {
			t.Fatalf("subject = %q", subject)
		}
		for path, want := range map[string]string{"feature.txt": "feature\n", "main.txt": "from main\n", "app.txt": "app v2\n"} {
			if got := projecttest.FileAt(t, bare, "yuno/t1", path); got != want {
				t.Fatalf("%s = %q, want %q", path, got, want)
			}
		}
		if tipOf(t, bare, "main") != mainTip {
			t.Fatal("main moved")
		}
	})

	t.Run("conflict recorded, nothing pushed", func(t *testing.T) {
		cfg, bare, _ := setup(t)
		head := handCommit(t, bare, "yuno/t1", map[string]string{"app.txt": "app from the branch\n", "z.txt": "branch z\n"})
		mainTip := handCommit(t, bare, "main", map[string]string{"app.txt": "app from main\n", "z.txt": "main z\n", "ok.txt": "fine\n"})
		got, err := cfg.UpdateFromDefault(ctx, "yuno/t1", head)
		if err != nil || got.Outcome != UpdateConflicts || got.Default != mainTip || got.Commit != "" ||
			!slices.Equal(got.Conflicts, []string{"app.txt", "z.txt"}) {
			t.Fatalf("update = %+v %v", got, err)
		}
		if tipOf(t, bare, "yuno/t1") != head {
			t.Fatal("a conflicting merge was pushed")
		}
	})

	t.Run("head moved before the fetch", func(t *testing.T) {
		cfg, bare, head := setup(t)
		handCommit(t, bare, "main", map[string]string{"main.txt": "main\n"})
		moved := handCommit(t, bare, "yuno/t1", map[string]string{"feature.txt": "feature, by hand\n"})
		_, err := cfg.UpdateFromDefault(ctx, "yuno/t1", head)
		var hm *ErrHeadMoved
		if !errors.As(err, &hm) || hm.Current != moved {
			t.Fatalf("err = %v", err)
		}
		if tipOf(t, bare, "yuno/t1") != moved {
			t.Fatal("the branch was pushed")
		}
	})

	t.Run("push refused because the head moved", func(t *testing.T) {
		cfg, bare, head := setup(t)
		handCommit(t, bare, "main", map[string]string{"main.txt": "main\n"})
		var moved string
		beforeUpdatePush = func() {
			moved = handCommit(t, bare, "yuno/t1", map[string]string{"late.txt": "pushed meanwhile\n"})
		}
		t.Cleanup(func() { beforeUpdatePush = nil })
		_, err := cfg.UpdateFromDefault(ctx, "yuno/t1", head)
		var hm *ErrHeadMoved
		if !errors.As(err, &hm) || hm.Current != moved || moved == "" {
			t.Fatalf("err = %v (moved to %s)", err, moved)
		}
		// Not forced: the commit pushed meanwhile stays.
		if tipOf(t, bare, "yuno/t1") != moved || projecttest.FileAt(t, bare, "yuno/t1", "late.txt") != "pushed meanwhile\n" {
			t.Fatal("the push overwrote the branch")
		}
	})

	t.Run("refusals", func(t *testing.T) {
		cfg, _, head := setup(t)
		if _, err := cfg.UpdateFromDefault(ctx, "yuno/t1", head[:7]); err == nil || !strings.Contains(err.Error(), "not a full commit id") {
			t.Fatalf("short sha = %v", err)
		}
		if _, err := cfg.UpdateFromDefault(ctx, "main", head); err == nil {
			t.Fatal("the default branch was updated from itself")
		}
	})
}

func TestParseMergeTree(t *testing.T) {
	tree := strings.Repeat("a", 40)
	if got, files := parseMergeTree([]byte(tree + "\x00")); got != tree || len(files) != 0 {
		t.Fatalf("clean = %q %v", got, files)
	}
	got, files := parseMergeTree([]byte(tree + "\x00b.txt\x00a.txt\x00b.txt\x00\x00"))
	if got != tree || !slices.Equal(files, []string{"a.txt", "b.txt"}) {
		t.Fatalf("conflicts = %q %v", got, files)
	}
}
