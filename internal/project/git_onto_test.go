package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yuno/internal/project/projecttest"
)

// DESIGN 5.20 rule 2 (ProposeOnto) against a local bare repository: a later call of a
// run pushes one commit on top of the request's own branch, with the title as its
// message; a tree equal to the branch pushes nothing; a branch the remote lacks fails
// before anything is pushed, and git's message carries no token.
func TestProposeOnto(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n", "keep/old.txt": "old\n"})
	cfg := Config{RepoURL: bare, Token: "glpat-not-a-real-token-0", DefaultBranch: "main", Provider: ProviderGitLab}
	ctx := context.Background()
	ws := t.TempDir()
	checkout := filepath.Join(ws, "repo")
	must(t, cfg.Clone(ctx, checkout))

	// The first call opens the branch from the default one.
	must(t, os.WriteFile(filepath.Join(checkout, "README.md"), []byte("# demo\nfirst\n"), 0o644))
	first, err := cfg.Propose(ctx, checkout, "yuno/abc", "First change")
	if err != nil || first.Commit == "" {
		t.Fatalf("propose = %+v %v", first, err)
	}

	// The checkout is unchanged since: nothing to push, and the branch stays where it was.
	if _, err := cfg.ProposeOnto(ctx, checkout, "yuno/abc", "", "Nothing new"); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("clean tree: %v", err)
	}
	if got := projecttest.Git(t, bare, "rev-parse", "yuno/abc"); got != first.Commit {
		t.Fatalf("a clean tree moved the branch: %s, want %s", got, first.Commit)
	}

	// The agent works on: an edit, a new file, a deletion, a dependency directory.
	must(t, os.WriteFile(filepath.Join(checkout, "README.md"), []byte("# demo\nsecond\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(checkout, "extra.txt"), []byte("extra\n"), 0o644))
	must(t, os.RemoveAll(filepath.Join(checkout, "keep")))
	must(t, os.MkdirAll(filepath.Join(checkout, "node_modules", "x"), 0o755))
	must(t, os.WriteFile(filepath.Join(checkout, "node_modules", "x", "i.js"), []byte("x"), 0o644))
	// A hook the agent planted must not run on the api side here either.
	must(t, os.WriteFile(filepath.Join(checkout, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\necho HOOK > "+filepath.Join(ws, "hook-ran")+"\n"), 0o755))

	second, err := cfg.ProposeOnto(ctx, checkout, "yuno/abc", "", "Second change")
	if err != nil {
		t.Fatal(err)
	}
	if second.Branch != "yuno/abc" || second.Commit == "" || second.Commit == first.Commit {
		t.Fatalf("proposal = %+v", second)
	}
	if got := projecttest.Git(t, bare, "rev-parse", "yuno/abc"); got != second.Commit {
		t.Fatalf("branch tip = %s, want %s", got, second.Commit)
	}
	// One commit on top of the first, not a rewrite: the history is seed, first, second.
	if got := projecttest.Git(t, bare, "rev-parse", "yuno/abc~1"); got != first.Commit {
		t.Fatalf("the new commit's parent = %s, want the first commit %s", got, first.Commit)
	}
	if got := projecttest.Git(t, bare, "log", "--format=%s", "yuno/abc"); got != "Second change\nFirst change\nseed" {
		t.Fatalf("history = %q", got)
	}
	if got := projecttest.Author(t, bare, "yuno/abc"); got != "Yuno <yuno@localhost>" {
		t.Fatalf("author = %q", got)
	}
	if got := projecttest.FileAt(t, bare, "yuno/abc", "README.md"); got != "# demo\nsecond\n" {
		t.Fatalf("README = %q", got)
	}
	if got := projecttest.FileAt(t, bare, "yuno/abc", "extra.txt"); got != "extra\n" {
		t.Fatalf("extra.txt = %q", got)
	}
	if got := projecttest.FileAt(t, bare, "yuno/abc", "keep/old.txt"); got != "" {
		t.Fatal("a deleted file survived on the branch")
	}
	if got := projecttest.FileAt(t, bare, "yuno/abc", "node_modules/x/i.js"); got != "" {
		t.Fatal("node_modules was committed")
	}
	if got := projecttest.FileAt(t, bare, "main", "README.md"); got != "# demo\n" {
		t.Fatal("main moved")
	}
	if _, err := os.Stat(filepath.Join(ws, "hook-ran")); err == nil {
		t.Fatal("the checkout's pre-commit hook ran on the api side")
	}

	// A branch the remote lacks: git's message, redacted, and nothing pushed.
	before := projecttest.Branches(t, bare)
	_, err = cfg.ProposeOnto(ctx, checkout, "yuno/missing", "", "Nowhere")
	if err == nil || !strings.Contains(err.Error(), "git clone") || strings.Contains(err.Error(), cfg.Token) {
		t.Fatalf("missing branch: %v", err)
	}
	if after := projecttest.Branches(t, bare); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("branches changed: %v -> %v", before, after)
	}
}

// pushByHand puts one commit on branch of the bare repository from a clone of its own,
// the way a person pushing to the request's branch does, and returns the commit's id.
func pushByHand(t *testing.T, bare, branch, file, content string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "hand")
	projecttest.Git(t, filepath.Dir(work), "clone", "--quiet", "--branch", branch, bare, work)
	must(t, os.WriteFile(filepath.Join(work, file), []byte(content), 0o644))
	projecttest.Git(t, work, "add", "--all")
	projecttest.Git(t, work, "commit", "--quiet", "-m", "by hand: "+file)
	projecttest.Git(t, work, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
	return projecttest.Git(t, work, "rev-parse", "HEAD")
}

// DESIGN 5.22 rules 2 and 4 (CloneBranch, HeadSHA): a run that updates a request clones
// its source branch, not the default one, and HeadSHA names the commit the clone is at,
// 40 hex digits; Clone is CloneBranch of the default branch.
func TestCloneBranchAndHeadSHA(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n"})
	cfg := Config{RepoURL: bare, Token: "glpat-not-a-real-token-0", DefaultBranch: "main", Provider: ProviderGitLab}
	ctx := context.Background()
	ws := t.TempDir()

	// A fresh clone of the default branch is at the remote's main.
	fresh := filepath.Join(ws, "fresh")
	must(t, cfg.Clone(ctx, fresh))
	head, err := cfg.HeadSHA(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if want := projecttest.Git(t, bare, "rev-parse", "main"); head != want || !commitIDPattern.MatchString(head) {
		t.Fatalf("HeadSHA = %q, want main's %q", head, want)
	}

	// The branch of a request: its own content, its own head, one branch only, and the
	// local exclude file as a Clone writes it.
	must(t, os.WriteFile(filepath.Join(fresh, "README.md"), []byte("# demo\nbranch\n"), 0o644))
	proposal, err := cfg.Propose(ctx, fresh, "yuno/abc", "On the branch")
	if err != nil {
		t.Fatal(err)
	}
	onBranch := filepath.Join(ws, "onbranch")
	must(t, cfg.CloneBranch(ctx, onBranch, "yuno/abc"))
	if got, err := cfg.HeadSHA(ctx, onBranch); err != nil || got != proposal.Commit {
		t.Fatalf("HeadSHA of the branch clone = %q %v, want %s", got, err, proposal.Commit)
	}
	if got, _ := os.ReadFile(filepath.Join(onBranch, "README.md")); string(got) != "# demo\nbranch\n" {
		t.Fatalf("the branch clone holds %q", got)
	}
	if got := projecttest.Git(t, onBranch, "for-each-ref", "--format=%(refname:short)", "refs/remotes/"); got != "origin/yuno/abc" {
		t.Fatalf("remote branches in the clone: %q, want only origin/yuno/abc", got)
	}
	if got, _ := os.ReadFile(filepath.Join(onBranch, ".git", "info", "exclude")); !strings.Contains(string(got), "node_modules/") {
		t.Fatalf("exclude file = %q", got)
	}

	// Not a repository: an error, never an empty id.
	if got, err := cfg.HeadSHA(ctx, t.TempDir()); err == nil || got != "" {
		t.Fatalf("HeadSHA outside a repository = %q %v", got, err)
	}
	if err := cfg.CloneBranch(ctx, filepath.Join(ws, "missing"), "yuno/nope"); err == nil || strings.Contains(err.Error(), cfg.Token) {
		t.Fatalf("clone of a missing branch: %v", err)
	}
}

// DESIGN 5.22 rule 4 (the branch guard, D-050): ProposeOnto with the commit the run's
// checkout was cloned from refuses when the branch has moved since, copying and pushing
// nothing; it pushes when the branch is still at that commit; "" skips the guard.
func TestProposeOntoBranchGuard(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n"})
	cfg := Config{RepoURL: bare, Token: "glpat-not-a-real-token-0", DefaultBranch: "main", Provider: ProviderGitLab}
	ctx := context.Background()
	ws := t.TempDir()

	// The request's branch exists, opened by an earlier run.
	opener := filepath.Join(ws, "opener")
	must(t, cfg.Clone(ctx, opener))
	must(t, os.WriteFile(filepath.Join(opener, "README.md"), []byte("# demo\nopened\n"), 0o644))
	opened, err := cfg.Propose(ctx, opener, "yuno/abc", "Open the request")
	if err != nil {
		t.Fatal(err)
	}

	// A run that updates the request clones the branch and records its head as the base.
	checkout := filepath.Join(ws, "run")
	must(t, cfg.CloneBranch(ctx, checkout, "yuno/abc"))
	base, err := cfg.HeadSHA(ctx, checkout)
	if err != nil || base != opened.Commit {
		t.Fatalf("base = %q %v, want %s", base, err, opened.Commit)
	}

	// Equal: the branch is where the run cloned it, so the guard passes; one commit on top.
	must(t, os.WriteFile(filepath.Join(checkout, "README.md"), []byte("# demo\nfirst fix\n"), 0o644))
	first, err := cfg.ProposeOnto(ctx, checkout, "yuno/abc", base, "First fix")
	if err != nil {
		t.Fatalf("branch at the base: %v", err)
	}
	if first.Commit == "" || first.Commit == base || projecttest.Git(t, bare, "rev-parse", "yuno/abc") != first.Commit ||
		projecttest.Git(t, bare, "rev-parse", "yuno/abc~1") != base {
		t.Fatalf("proposal = %+v; branch = %s, parent = %s", first,
			projecttest.Git(t, bare, "rev-parse", "yuno/abc"), projecttest.Git(t, bare, "rev-parse", "yuno/abc~1"))
	}

	// The run records the pushed commit as its new base (rule 4), so a second call passes.
	must(t, os.WriteFile(filepath.Join(checkout, "README.md"), []byte("# demo\nsecond fix\n"), 0o644))
	second, err := cfg.ProposeOnto(ctx, checkout, "yuno/abc", first.Commit, "Second fix")
	if err != nil || projecttest.Git(t, bare, "rev-parse", "yuno/abc") != second.Commit {
		t.Fatalf("second call from the recorded base: %+v %v", second, err)
	}

	// Moved: a person pushes to the branch after the run's base. Nothing is pushed.
	handed := pushByHand(t, bare, "yuno/abc", "hand.txt", "mine\n")
	must(t, os.WriteFile(filepath.Join(checkout, "README.md"), []byte("# demo\nthird fix\n"), 0o644))
	branches := projecttest.Branches(t, bare)
	_, err = cfg.ProposeOnto(ctx, checkout, "yuno/abc", second.Commit, "Third fix")
	var moved *ErrBranchMoved
	if !errors.As(err, &moved) {
		t.Fatalf("branch moved: %v, want *ErrBranchMoved", err)
	}
	if moved.Branch != "yuno/abc" || moved.Head != handed || moved.Base != second.Commit {
		t.Fatalf("ErrBranchMoved = %+v, want branch yuno/abc, head %s, base %s", moved, handed, second.Commit)
	}
	const text = "the branch yuno/abc has new commits since this run cloned it; nothing was pushed. " +
		"Finish and report it; a new run on the request starts from the branch as it is now"
	if err.Error() != text {
		t.Fatalf("error text = %q", err.Error())
	}
	if errors.Is(err, ErrNoChanges) {
		t.Fatal("a moved branch reads as no changes")
	}
	if got := projecttest.Git(t, bare, "rev-parse", "yuno/abc"); got != handed {
		t.Fatalf("the refused call moved the branch: %s, want %s", got, handed)
	}
	if after := projecttest.Branches(t, bare); strings.Join(after, ",") != strings.Join(branches, ",") {
		t.Fatalf("branches changed: %v -> %v", branches, after)
	}
	if got := projecttest.FileAt(t, bare, "yuno/abc", "README.md"); got != "# demo\nsecond fix\n" {
		t.Fatalf("README on the branch = %q: the refused call pushed its tree", got)
	}

	// A branch the remote lacks fails on the clone, before the guard and with git's text.
	if _, err := cfg.ProposeOnto(ctx, checkout, "yuno/missing", base, "Nowhere"); err == nil || errors.As(err, &moved) ||
		strings.Contains(err.Error(), cfg.Token) {
		t.Fatalf("missing branch: %v", err)
	}

	// "": the guard is skipped (the same-run path, runs cloned before checkout_sha), and
	// the commit lands on top of the branch as it is now, the person's commit kept.
	third, err := cfg.ProposeOnto(ctx, checkout, "yuno/abc", "", "Third fix")
	if err != nil {
		t.Fatalf("guard skipped: %v", err)
	}
	if got := projecttest.Git(t, bare, "rev-parse", "yuno/abc"); got != third.Commit {
		t.Fatalf("branch = %s, want %s", got, third.Commit)
	}
	if got := projecttest.Git(t, bare, "rev-parse", "yuno/abc~1"); got != handed {
		t.Fatalf("parent of the new commit = %s, want the person's %s", got, handed)
	}
}
