package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"yuno/internal/project/projecttest"
)

// M3 of the wave 1 review: ProposePaths commits only the named files and directories, a
// path deleted in the checkout is a deletion, everything else left in the checkout stays
// out, a path in neither place and a path through a link are refused.
func TestProposePaths(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n", "docs/plans/x/OLD.md": "old\n", "docs/other.md": "o\n"})
	cfg := Config{RepoURL: bare, Token: "glpat-not-a-real-token-0", DefaultBranch: "main", Provider: ProviderGitLab}
	ctx := context.Background()
	checkout := filepath.Join(t.TempDir(), "repo")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(cfg.Clone(ctx, checkout))
	write := func(p, body string) {
		must(os.MkdirAll(filepath.Dir(filepath.Join(checkout, p)), 0o755))
		must(os.WriteFile(filepath.Join(checkout, p), []byte(body), 0o644))
	}
	write("docs/plans/x/PRD.md", "prd\n")
	write("docs/plans/x/sub/TASKS.md", "tasks\n")
	write("docs/plans/x/node_modules/skip.js", "x")
	write("README.md", "# changed\n")
	write("src/stray.go", "package stray\n")
	must(os.Remove(filepath.Join(checkout, "docs/plans/x/OLD.md")))
	must(os.Remove(filepath.Join(checkout, "docs/other.md")))

	prop, err := cfg.ProposePaths(ctx, checkout, "yuno/p1", Base{}, "Plan", []string{"docs/plans/x", "docs/other.md"})
	must(err)
	if prop.Commit == "" || prop.Files != 2 {
		t.Fatalf("proposal = %+v", prop)
	}
	at := func(p string) string { return projecttest.FileAt(t, bare, "yuno/p1", p) }
	if at("docs/plans/x/PRD.md") != "prd\n" || at("docs/plans/x/sub/TASKS.md") != "tasks\n" {
		t.Error("the named directory was not committed")
	}
	if at("docs/plans/x/OLD.md") != "" || at("docs/other.md") != "" {
		t.Error("a path deleted in the checkout survived")
	}
	if at("README.md") != "# demo\n" {
		t.Errorf("README outside the paths changed: %q", at("README.md"))
	}
	if at("src/stray.go") != "" || at("docs/plans/x/node_modules/skip.js") != "" {
		t.Error("a file outside the paths, or an excluded directory, was committed")
	}

	// A path in neither the checkout nor the repository.
	var missing *ErrPathMissing
	if _, err := cfg.ProposePaths(ctx, checkout, "yuno/p2", Base{}, "x", []string{"nope/none.md"}); !errors.As(err, &missing) {
		t.Fatalf("missing path: %v", err)
	}
	// Paths that change nothing.
	if _, err := cfg.ProposePaths(ctx, checkout, "yuno/p3", Base{}, "x", []string{"docs/plans/x"}); err != nil && !errors.Is(err, ErrNoChanges) {
		t.Fatalf("repeat: %v", err)
	}
	// A path through a link is refused and nothing is pushed.
	must(os.Symlink("/etc", filepath.Join(checkout, "evil")))
	if _, err := cfg.ProposePaths(ctx, checkout, "yuno/p4", Base{}, "x", []string{"evil/hostname"}); err == nil {
		t.Fatal("a path through a link was accepted")
	}
	if slices.Contains(projecttest.Branches(t, bare), "yuno/p4") || slices.Contains(projecttest.Branches(t, bare), "yuno/p2") {
		t.Fatal("a refused proposal pushed a branch")
	}

	// ProposeOntoPaths adds only its paths on top of the branch.
	write("docs/plans/x/PRD.md", "prd v2\n")
	write("README.md", "# changed again\n")
	if _, err := cfg.ProposeOntoPaths(ctx, checkout, "yuno/p1", Base{}, "Revise", []string{"docs/plans/x/PRD.md"}); err != nil {
		t.Fatal(err)
	}
	if at("docs/plans/x/PRD.md") != "prd v2\n" || at("README.md") != "# demo\n" {
		t.Errorf("onto: PRD=%q README=%q", at("docs/plans/x/PRD.md"), at("README.md"))
	}
}
