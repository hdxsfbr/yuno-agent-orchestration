package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"yuno/internal/project/projecttest"
)

// DESIGN 5.16 rule 10 (D-087) against a local bare repository: a proposal counts the run's
// changes from its base and puts only those onto the branch as it is now, so work that
// reached the branch after the run's checkout was made is never deleted or reverted.

// handCommit commits files on branch of the bare repository from a clone of its own, the
// way another ticket's merge or a person's push does ("" deletes the file), and returns the
// commit's id.
func handCommit(t *testing.T, bare, branch string, files map[string]string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "hand")
	projecttest.Git(t, filepath.Dir(work), "clone", "--quiet", "--branch", branch, bare, work)
	for name, body := range files {
		p := filepath.Join(work, filepath.FromSlash(name))
		if body == "" {
			must(t, os.RemoveAll(p))
			continue
		}
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(body), 0o644))
	}
	projecttest.Git(t, work, "add", "--all")
	projecttest.Git(t, work, "commit", "--quiet", "-m", "by hand")
	projecttest.Git(t, work, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
	return projecttest.Git(t, work, "rev-parse", "HEAD")
}

// mergeInto merges branch into into in a clone of the bare repository, as the provider does
// when the request is merged (a conflict fails the test), and returns the merged files.
func mergeInto(t *testing.T, bare, into, branch string) []string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "merge")
	projecttest.Git(t, filepath.Dir(work), "clone", "--quiet", "--branch", into, bare, work)
	projecttest.Git(t, work, "merge", "--quiet", "--no-ff", "--no-edit", "origin/"+branch)
	return strings.Split(projecttest.Git(t, work, "ls-files"), "\n")
}

// checkoutOf clones the default branch of bare as a run's checkout and returns the
// configuration, the checkout and its commit, the run's base.
func checkoutOf(t *testing.T, bare string) (Config, string, string) {
	t.Helper()
	cfg := Config{RepoURL: bare, Token: "glpat-not-a-real-token-0", DefaultBranch: "main", Provider: ProviderGitLab}
	checkout := filepath.Join(t.TempDir(), "repo")
	must(t, cfg.Clone(context.Background(), checkout))
	base, err := cfg.HeadSHA(context.Background(), checkout)
	must(t, err)
	return cfg, checkout, base
}

func writeIn(t *testing.T, checkout, name, body string) {
	t.Helper()
	p := filepath.Join(checkout, filepath.FromSlash(name))
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(t, os.WriteFile(p, []byte(body), 0o644))
}

// The acceptance test of D-087: main gains files, and changes a file the run does not
// touch, after the run's checkout was made. The run's request keeps all of it, deletes
// only what the run deleted, and merges into main with both tickets' work.
func TestProposeKeepsWhatMainGainedSinceTheRunStarted(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n", "app.txt": "app v1\n", "keep/old.txt": "old\n"})
	cfg, checkout, base := checkoutOf(t, bare)
	ctx := context.Background()

	// Ticket T2 is merged while this run works.
	t2 := handCommit(t, bare, "main", map[string]string{"t2/feature.go": "package t2\n", "t2/feature_test.go": "package t2\n", "app.txt": "app v2\n"})

	// This run's work: an edit, a new file and a deletion.
	writeIn(t, checkout, "README.md", "# demo\nT3\n")
	writeIn(t, checkout, "t3.txt", "t3\n")
	must(t, os.RemoveAll(filepath.Join(checkout, "keep")))
	prop, err := cfg.ProposePaths(ctx, checkout, "yuno/t3", Base{Commit: base}, "T3", nil)
	if err != nil {
		t.Fatal(err)
	}
	at := func(p string) string { return projecttest.FileAt(t, bare, "yuno/t3", p) }
	if at("t2/feature.go") != "package t2\n" || at("t2/feature_test.go") != "package t2\n" || at("app.txt") != "app v2\n" {
		t.Fatalf("T2's work did not survive: feature.go %q, app.txt %q", at("t2/feature.go"), at("app.txt"))
	}
	if at("README.md") != "# demo\nT3\n" || at("t3.txt") != "t3\n" || at("keep/old.txt") != "" {
		t.Fatalf("the run's changes: README %q, t3.txt %q, keep/old.txt %q", at("README.md"), at("t3.txt"), at("keep/old.txt"))
	}
	// Against main the request changes only what the run changed.
	if got := projecttest.Git(t, bare, "diff", "--name-status", "main", "yuno/t3"); got != "M\tREADME.md\nD\tkeep/old.txt\nA\tt3.txt" {
		t.Fatalf("main..request = %q", got)
	}
	// The head merges main as it is with the run's own commit, which sits on the base.
	tip := projecttest.Git(t, bare, "rev-parse", "yuno/t3")
	if prop.Commit != tip || projecttest.Git(t, bare, "rev-parse", "yuno/t3^1") != t2 || projecttest.Git(t, bare, "rev-parse", "yuno/t3^2") != prop.Base ||
		projecttest.Git(t, bare, "rev-parse", prop.Base+"^") != base || prop.Base == prop.Commit {
		t.Fatalf("proposal %+v: tip %s, main %s, base %s", prop, tip, t2, base)
	}
	if projecttest.Subject(t, bare, "yuno/t3") != `Merge "T3" with main` || projecttest.Git(t, bare, "log", "-1", "--format=%s", prop.Base) != "T3" ||
		projecttest.Author(t, bare, "yuno/t3") != "Yuno <yuno@localhost>" {
		t.Fatalf("subjects %q / %q", projecttest.Subject(t, bare, "yuno/t3"), projecttest.Git(t, bare, "log", "-1", "--format=%s", prop.Base))
	}
	// The run's own commit holds its changes and nothing of T2.
	if got := projecttest.Git(t, bare, "diff", "--name-status", base, prop.Base); got != "M\tREADME.md\nD\tkeep/old.txt\nA\tt3.txt" {
		t.Fatalf("base..own = %q", got)
	}
	merged := mergeInto(t, bare, "main", "yuno/t3")
	for _, want := range []string{"t2/feature.go", "t2/feature_test.go", "t3.txt", "README.md", "app.txt"} {
		if !slices.Contains(merged, want) {
			t.Errorf("the merged main lacks %s: %v", want, merged)
		}
	}
	if slices.Contains(merged, "keep/old.txt") {
		t.Error("the run's deletion did not reach main")
	}
}

// A branch still at the base takes the run's changes as one commit on top of it, the run's
// own commit, as before D-087.
func TestProposeOnAnUnmovedBranchIsOneCommit(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n"})
	cfg, checkout, base := checkoutOf(t, bare)
	writeIn(t, checkout, "README.md", "# demo\nchanged\n")
	prop, err := cfg.ProposePaths(context.Background(), checkout, "yuno/one", Base{Commit: base}, "One", nil)
	if err != nil {
		t.Fatal(err)
	}
	if prop.Commit != prop.Base || projecttest.Git(t, bare, "rev-parse", "yuno/one") != prop.Commit ||
		projecttest.Git(t, bare, "rev-parse", "yuno/one~1") != base || projecttest.Git(t, bare, "log", "--format=%s", "yuno/one") != "One\nseed" {
		t.Fatalf("proposal %+v, history %q", prop, projecttest.Git(t, bare, "log", "--format=%s", "yuno/one"))
	}
	// A base that is not a commit id is refused before anything is cloned.
	if _, err := cfg.ProposePaths(context.Background(), checkout, "yuno/two", Base{Commit: "abc"}, "Two", nil); err == nil || !strings.Contains(err.Error(), "not a full commit id") {
		t.Fatalf("short base: %v", err)
	}
}

// A file both main and the run changed, to different content, is a conflict: nothing is
// pushed and the error names the file and the branch. The same content on both sides is
// not a conflict, and a run whose every change main has already has nothing to push.
func TestProposeConflictPushesNothing(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n", "app.txt": "app\n"})
	cfg, checkout, base := checkoutOf(t, bare)
	ctx := context.Background()
	handCommit(t, bare, "main", map[string]string{"README.md": "# demo\nfrom main\n", "main.txt": "main\n"})

	writeIn(t, checkout, "README.md", "# demo\nfrom the run\n")
	writeIn(t, checkout, "other.txt", "other\n")
	before := projecttest.Branches(t, bare)
	_, err := cfg.ProposePaths(ctx, checkout, "yuno/c", Base{Commit: base}, "C", nil)
	var conflict *ErrConflict
	if !errors.As(err, &conflict) || conflict.Branch != "main" || !slices.Equal(conflict.Paths, []string{"README.md"}) {
		t.Fatalf("conflict: %v (%+v)", err, conflict)
	}
	const text = "these files conflict with changes on main made since this run's checkout: README.md. Nothing was pushed. " +
		"Ask the operator how to resolve it, or finish and report the conflict"
	if err.Error() != text {
		t.Fatalf("text = %q", err.Error())
	}
	if after := projecttest.Branches(t, bare); !slices.Equal(after, before) {
		t.Fatalf("a conflict pushed: %v -> %v", before, after)
	}

	// The run takes main's version: the same content on both sides needs nothing.
	writeIn(t, checkout, "README.md", "# demo\nfrom main\n")
	if _, err := cfg.ProposePaths(ctx, checkout, "yuno/c", Base{Commit: base}, "C", nil); err != nil {
		t.Fatal(err)
	}
	if got := projecttest.Git(t, bare, "diff", "--name-status", "main", "yuno/c"); got != "A\tother.txt" {
		t.Fatalf("main..request = %q", got)
	}
	// Every change of the run is on main already: nothing to push.
	must(t, os.Remove(filepath.Join(checkout, "other.txt")))
	if _, err := cfg.ProposePaths(ctx, checkout, "yuno/c2", Base{Commit: base}, "C2", nil); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("nothing new: %v", err)
	}
	if slices.Contains(projecttest.Branches(t, bare), "yuno/c2") {
		t.Fatal("nothing new was pushed as a branch")
	}
}

// A file the run writes where main made a directory, or under a path main made a file, is
// a conflict too; a deletion of a file main left alone goes through.
func TestProposeDirectoryAndFileClash(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n", "gone.txt": "g\n"})
	cfg, checkout, base := checkoutOf(t, bare)
	handCommit(t, bare, "main", map[string]string{"docs/a.md": "a\n", "lib": "a file now\n"})
	writeIn(t, checkout, "docs", "a file of the run\n")
	writeIn(t, checkout, "lib/x.go", "package lib\n")
	must(t, os.Remove(filepath.Join(checkout, "gone.txt")))
	_, err := cfg.ProposePaths(context.Background(), checkout, "yuno/d", Base{Commit: base}, "D", nil)
	var conflict *ErrConflict
	if !errors.As(err, &conflict) || !slices.Equal(conflict.Paths, []string{"docs", "lib/x.go"}) {
		t.Fatalf("clash: %v (%+v)", err, conflict)
	}

	// Without the clashing files the deletion goes onto main as it is.
	must(t, os.Remove(filepath.Join(checkout, "docs")))
	must(t, os.RemoveAll(filepath.Join(checkout, "lib")))
	if _, err := cfg.ProposePaths(context.Background(), checkout, "yuno/d", Base{Commit: base}, "D", nil); err != nil {
		t.Fatal(err)
	}
	if got := projecttest.Git(t, bare, "diff", "--name-status", "main", "yuno/d"); got != "D\tgone.txt" {
		t.Fatalf("main..request = %q", got)
	}
}

// A run that pushes again (DESIGN 5.20 rule 2) counts from its own commit, the base the
// last push returned: a file it pushed and changes again is no conflict with itself, a
// person's commit on the request's branch is kept, and a file the person and the run both
// changed is a conflict on that branch. The guarded form (5.22 rule 4) refuses any move.
func TestProposeAgainFromTheRunsOwnCommit(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n", "app.txt": "app\n"})
	cfg, checkout, base := checkoutOf(t, bare)
	ctx := context.Background()
	handCommit(t, bare, "main", map[string]string{"t2.txt": "t2\n"})

	writeIn(t, checkout, "README.md", "# demo\nv1\n")
	first, err := cfg.ProposePaths(ctx, checkout, "yuno/r", Base{Commit: base}, "First", nil)
	if err != nil {
		t.Fatal(err)
	}
	writeIn(t, checkout, "README.md", "# demo\nv2\n")
	writeIn(t, checkout, "more.txt", "more\n")
	second, err := cfg.ProposeOntoPaths(ctx, checkout, "yuno/r", Base{Commit: first.Base}, "Second", nil)
	if err != nil {
		t.Fatalf("the run's second push: %v", err)
	}
	at := func(p string) string { return projecttest.FileAt(t, bare, "yuno/r", p) }
	if at("README.md") != "# demo\nv2\n" || at("more.txt") != "more\n" || at("t2.txt") != "t2\n" || at("app.txt") != "app\n" {
		t.Fatalf("after the second push: README %q, more %q, t2 %q", at("README.md"), at("more.txt"), at("t2.txt"))
	}
	if projecttest.Git(t, bare, "rev-parse", "yuno/r^1") != first.Commit || projecttest.Git(t, bare, "rev-parse", "yuno/r^2") != second.Base ||
		projecttest.Git(t, bare, "rev-parse", second.Base+"^") != first.Base {
		t.Fatalf("second %+v after first %+v", second, first)
	}
	// The run's own line holds only the run's changes.
	if got := projecttest.Git(t, bare, "diff", "--name-status", base, second.Base); got != "M\tREADME.md\nA\tmore.txt" {
		t.Fatalf("base..own = %q", got)
	}

	// A person pushes to the request's branch: their file stays.
	handCommit(t, bare, "yuno/r", map[string]string{"review.txt": "suggestion\n"})
	writeIn(t, checkout, "more.txt", "more v2\n")
	third, err := cfg.ProposeOntoPaths(ctx, checkout, "yuno/r", Base{Commit: second.Base}, "Third", nil)
	if err != nil {
		t.Fatal(err)
	}
	if at("review.txt") != "suggestion\n" || at("more.txt") != "more v2\n" || at("t2.txt") != "t2\n" {
		t.Fatalf("after the person's push: review %q, more %q", at("review.txt"), at("more.txt"))
	}

	// The person changes a file the run changes too: a conflict on the request's branch.
	moved := handCommit(t, bare, "yuno/r", map[string]string{"README.md": "# by a person\n"})
	writeIn(t, checkout, "README.md", "# demo\nv3\n")
	_, err = cfg.ProposeOntoPaths(ctx, checkout, "yuno/r", Base{Commit: third.Base}, "Fourth", nil)
	var conflict *ErrConflict
	if !errors.As(err, &conflict) || conflict.Branch != "yuno/r" || !slices.Equal(conflict.Paths, []string{"README.md"}) {
		t.Fatalf("conflict on the request's branch: %v", err)
	}
	var branchMoved *ErrBranchMoved
	if _, err := cfg.ProposeOntoPaths(ctx, checkout, "yuno/r", Base{Commit: third.Base, Guard: true}, "Fourth", nil); !errors.As(err, &branchMoved) || branchMoved.Head != moved {
		t.Fatalf("guarded: %v", err)
	}
	if projecttest.Git(t, bare, "rev-parse", "yuno/r") != moved {
		t.Fatal("a refused proposal moved the branch")
	}
}

// Paths onto a moved branch: only the named path's change goes on top of main as it is.
func TestProposePathsOntoAMovedBranch(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n", "docs/plan.md": "plan v1\n", "docs/old.md": "old\n"})
	cfg, checkout, base := checkoutOf(t, bare)
	handCommit(t, bare, "main", map[string]string{"t2.txt": "t2\n", "docs/other.md": "other\n"})
	writeIn(t, checkout, "docs/plan.md", "plan v2\n")
	writeIn(t, checkout, "README.md", "# changed outside the paths\n")
	writeIn(t, checkout, "stray.txt", "stray\n")
	must(t, os.Remove(filepath.Join(checkout, "docs", "old.md")))
	if _, err := cfg.ProposePaths(context.Background(), checkout, "yuno/p", Base{Commit: base}, "Plan", []string{"docs/plan.md", "docs/old.md"}); err != nil {
		t.Fatal(err)
	}
	if got := projecttest.Git(t, bare, "diff", "--name-status", "main", "yuno/p"); got != "D\tdocs/old.md\nM\tdocs/plan.md" {
		t.Fatalf("main..request = %q", got)
	}
	if projecttest.FileAt(t, bare, "yuno/p", "t2.txt") != "t2\n" || projecttest.FileAt(t, bare, "yuno/p", "docs/other.md") != "other\n" {
		t.Fatal("main's new files are missing from the request")
	}
}

// What the copy of the checkout cannot see keeps the base's version: a link (the copy skips
// links) and a tracked file under an Excluded directory, on an unmoved branch and on a
// moved one. A link the run deleted is deleted.
func TestProposeKeepsWhatTheCopyCannotSee(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n", "node_modules/pkg/index.js": "module.exports = 1\n"})
	seed := filepath.Join(t.TempDir(), "seed")
	projecttest.Git(t, filepath.Dir(seed), "clone", "--quiet", bare, seed)
	must(t, os.Symlink("README.md", filepath.Join(seed, "link")))
	projecttest.Git(t, seed, "add", "--all")
	projecttest.Git(t, seed, "commit", "--quiet", "-m", "a link")
	projecttest.Git(t, seed, "push", "--quiet", "origin", "HEAD:refs/heads/main")
	cfg, checkout, base := checkoutOf(t, bare)
	ctx := context.Background()
	kept := func(branch string) {
		t.Helper()
		if got := projecttest.Git(t, bare, "ls-tree", branch, "link"); !strings.HasPrefix(got, "120000 ") {
			t.Errorf("%s: the link is %q", branch, got)
		}
		if projecttest.FileAt(t, bare, branch, "node_modules/pkg/index.js") != "module.exports = 1\n" {
			t.Errorf("%s: the tracked file under node_modules was deleted", branch)
		}
	}

	writeIn(t, checkout, "README.md", "# demo\nchanged\n")
	if _, err := cfg.ProposePaths(ctx, checkout, "yuno/l1", Base{Commit: base}, "L1", nil); err != nil {
		t.Fatal(err)
	}
	kept("yuno/l1")
	if got := projecttest.Git(t, bare, "diff", "--name-status", "main", "yuno/l1"); got != "M\tREADME.md" {
		t.Fatalf("main..l1 = %q", got)
	}

	handCommit(t, bare, "main", map[string]string{"t2.txt": "t2\n"})
	if _, err := cfg.ProposePaths(ctx, checkout, "yuno/l2", Base{Commit: base}, "L2", nil); err != nil {
		t.Fatal(err)
	}
	kept("yuno/l2")

	must(t, os.Remove(filepath.Join(checkout, "link")))
	if _, err := cfg.ProposePaths(ctx, checkout, "yuno/l3", Base{Commit: base}, "L3", nil); err != nil {
		t.Fatal(err)
	}
	if got := projecttest.Git(t, bare, "diff", "--name-status", "main", "yuno/l3"); got != "M\tREADME.md\nD\tlink" {
		t.Fatalf("main..l3 = %q", got)
	}
}

// The guardrail: a commit that would delete or change a path outside the run's changes is
// refused, and the error names the paths.
func TestCheckTouched(t *testing.T) {
	projecttest.NeedGit(t)
	repo := t.TempDir()
	projecttest.Git(t, repo, "init", "--quiet")
	for name, body := range map[string]string{"a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n"} {
		must(t, os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644))
	}
	projecttest.Git(t, repo, "add", "--all")
	projecttest.Git(t, repo, "commit", "--quiet", "-m", "one")
	from := projecttest.Git(t, repo, "rev-parse", "HEAD")
	must(t, os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a2\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(repo, "c.txt"), []byte("c2\n"), 0o644))
	must(t, os.Remove(filepath.Join(repo, "b.txt")))
	projecttest.Git(t, repo, "commit", "--quiet", "-a", "-m", "two")
	to := projecttest.Git(t, repo, "rev-parse", "HEAD")
	ctx := context.Background()
	err := Config{}.checkTouched(ctx, repo, from, to, []treeChange{{path: "a.txt"}})
	if err == nil || err.Error() != "the commit would delete b.txt and change c.txt, which this run did not change; nothing was pushed" {
		t.Fatalf("stray paths: %v", err)
	}
	if err := (Config{}).checkTouched(ctx, repo, from, to, []treeChange{{path: "a.txt"}, {path: "b.txt"}, {path: "c.txt"}}); err != nil {
		t.Fatalf("the run's own paths: %v", err)
	}
}

// A conflict over many files names the first ones and counts the rest.
func TestConflictTextCountsTheRest(t *testing.T) {
	var paths []string
	for i := range maxNamedPaths + 3 {
		paths = append(paths, fmt.Sprintf("f%02d.txt", i))
	}
	text := (&ErrConflict{Branch: "main", Paths: paths}).Error()
	if !strings.Contains(text, "f19.txt and 3 more. Nothing was pushed") || strings.Contains(text, "f20.txt") {
		t.Fatalf("text = %q", text)
	}
}
