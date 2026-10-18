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
	if _, err := cfg.ProposeOnto(ctx, checkout, "yuno/abc", "Nothing new"); !errors.Is(err, ErrNoChanges) {
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

	second, err := cfg.ProposeOnto(ctx, checkout, "yuno/abc", "Second change")
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
	_, err = cfg.ProposeOnto(ctx, checkout, "yuno/missing", "Nowhere")
	if err == nil || !strings.Contains(err.Error(), "git clone") || strings.Contains(err.Error(), cfg.Token) {
		t.Fatalf("missing branch: %v", err)
	}
	if after := projecttest.Branches(t, bare); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("branches changed: %v -> %v", before, after)
	}
}
