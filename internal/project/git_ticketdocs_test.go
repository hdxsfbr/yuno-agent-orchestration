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

// DESIGN 5.28 rule 9 (D-089) against local bare repositories: a ticket run's proposal
// carries none of its feature's plan documents but the ticket's own file; the run's changes
// to the others are left out and named, whatever else it pushes.

// planDocs is a repository with the four plan documents of the feature "search" and code.
func planDocs() map[string]string {
	return map[string]string{
		"README.md":                         "# demo\n",
		"app.txt":                           "app v1\n",
		"docs/plans/search/PRD.md":          "# PRD v1\n",
		"docs/plans/search/ARCHITECTURE.md": "# Architecture v1\n",
		"docs/plans/search/TASKS.md":        "# Tasks v1\n",
		"docs/plans/search/WAVES.md":        "# Waves v1\n",
		"docs/plans/search/tickets/T3.md":   "# T3 trace\n",
		"docs/plans/other/ARCHITECTURE.md":  "# Another feature\n",
	}
}

var ticketT8 = &TicketDocs{Feature: "search", Ticket: "T8"}

// The rule's paths: the feature's documents and other tickets' files are withheld, the
// ticket's own file (in any case) and everything outside the feature's directory are not,
// and a file in place of the directory or one above it is withheld too.
func TestTicketDocsWithholds(t *testing.T) {
	for p, want := range map[string]bool{
		"docs/plans/search/ARCHITECTURE.md":    true,
		"docs/plans/search/PRD.md":             true,
		"docs/plans/search/TASKS.md":           true,
		"docs/plans/search/WAVES.md":           true,
		"docs/plans/search/tickets/T3.md":      true,
		"docs/plans/search/notes/extra.md":     true,
		"docs/plans/search":                    true,
		"docs/plans":                           true,
		"docs":                                 true,
		"docs/plans/search/tickets/T8.md":      false,
		"docs/plans/search/tickets/t8.md":      false,
		"docs/plans/search/tickets/T80.md":     true,
		"docs/plans/searching/ARCHITECTURE.md": false,
		"docs/plans/other/ARCHITECTURE.md":     false,
		"docs/guide.md":                        false,
		"README.md":                            false,
		"internal/search/ARCHITECTURE.md":      false,
	} {
		if got := ticketT8.withholds(p); got != want {
			t.Errorf("withholds(%q) = %v, want %v", p, got, want)
		}
	}
	if ticketT8.Dir() != "docs/plans/search" || ticketT8.File() != "docs/plans/search/tickets/T8.md" {
		t.Fatalf("dir %q file %q", ticketT8.Dir(), ticketT8.File())
	}
}

// The acceptance test of D-089: a ticket run that edited ARCHITECTURE.md, TASKS.md, another
// ticket's file and its own ticket file, besides code, pushes the code and its ticket file
// only, whether it proposes the whole tree or the feature's directory, and names the rest.
// Main's edit of ARCHITECTURE.md since the run's checkout is no conflict and stays. A run
// whose only changes are withheld has nothing to push and still learns what was left out.
func TestProposeLeavesOutTheOtherPlanDocumentsOfATicketRun(t *testing.T) {
	bare := projecttest.BareRepo(t, planDocs())
	cfg, checkout, base := checkoutOf(t, bare)
	ctx := context.Background()
	// Another wave's ship run applied its notes while this run worked.
	handCommit(t, bare, "main", map[string]string{"docs/plans/search/ARCHITECTURE.md": "# Architecture v2 (as built)\n"})

	writeIn(t, checkout, "app.txt", "app v2\n")
	writeIn(t, checkout, "search.go", "package search\n")
	writeIn(t, checkout, "docs/plans/search/ARCHITECTURE.md", "# Architecture, edited by T8\n")
	writeIn(t, checkout, "docs/plans/search/TASKS.md", "# Tasks, T8 done\n")
	writeIn(t, checkout, "docs/plans/search/tickets/T3.md", "# T3 trace, touched by T8\n")
	writeIn(t, checkout, "docs/plans/search/tickets/T8.md", "# T8 trace\n\n## Notes for the plan documents\n- ARCHITECTURE.md §2: as built\n")
	must(t, os.Remove(filepath.Join(checkout, "docs/plans/search/WAVES.md")))
	writeIn(t, checkout, "docs/plans/other/ARCHITECTURE.md", "# Another feature, fixed\n")

	wantLeft := []string{"docs/plans/search/ARCHITECTURE.md", "docs/plans/search/TASKS.md", "docs/plans/search/WAVES.md",
		"docs/plans/search/tickets/T3.md"}
	prop, err := cfg.ProposePaths(ctx, checkout, "yuno/t8", Base{Commit: base, Ticket: ticketT8}, "T8", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(prop.LeftOut, wantLeft) {
		t.Fatalf("left out = %v, want %v", prop.LeftOut, wantLeft)
	}
	if got := projecttest.Git(t, bare, "diff", "--name-status", "main", "yuno/t8"); got !=
		"M\tapp.txt\nM\tdocs/plans/other/ARCHITECTURE.md\nA\tdocs/plans/search/tickets/T8.md\nA\tsearch.go" {
		t.Fatalf("main..request = %q", got)
	}
	at := func(branch, p string) string { return projecttest.FileAt(t, bare, branch, p) }
	if at("yuno/t8", "docs/plans/search/ARCHITECTURE.md") != "# Architecture v2 (as built)\n" || at("yuno/t8", "docs/plans/search/WAVES.md") != "# Waves v1\n" {
		t.Fatal("the request changed a plan document the ticket does not own")
	}
	// The run's own commit holds no withheld change either.
	if got := projecttest.Git(t, bare, "diff", "--name-only", base, prop.Base); got !=
		"app.txt\ndocs/plans/other/ARCHITECTURE.md\ndocs/plans/search/tickets/T8.md\nsearch.go" {
		t.Fatalf("base..own = %q", got)
	}

	// The feature's directory as paths, the ready call of the wave pipeline: the ticket file only.
	paths, err := cfg.ProposePaths(ctx, checkout, "yuno/t8-docs", Base{Commit: base, Ticket: ticketT8}, "T8 docs", []string{"docs/plans/search"})
	if err != nil {
		t.Fatal(err)
	}
	if got := projecttest.Git(t, bare, "diff", "--name-status", "main", "yuno/t8-docs"); got != "A\tdocs/plans/search/tickets/T8.md" || !slices.Equal(paths.LeftOut, wantLeft) {
		t.Fatalf("main..request = %q, left out %v", got, paths.LeftOut)
	}

	// Only withheld changes: nothing to push, and the answer says what was left out.
	_, only, onlyBase := checkoutOf(t, bare)
	writeIn(t, only, "docs/plans/search/PRD.md", "# PRD, edited by a ticket\n")
	before := projecttest.Branches(t, bare)
	none, err := cfg.ProposePaths(ctx, only, "yuno/t8-none", Base{Commit: onlyBase, Ticket: ticketT8}, "None", nil)
	if !errors.Is(err, ErrNoChanges) || !slices.Equal(none.LeftOut, []string{"docs/plans/search/PRD.md"}) {
		t.Fatalf("only withheld: %v, left out %v", err, none.LeftOut)
	}
	if !slices.Equal(projecttest.Branches(t, bare), before) {
		t.Fatal("a proposal with nothing to push pushed a branch")
	}
}

// A run that builds no ticket (the plan pipeline, a quick run) is not held: it writes all
// four plan documents.
func TestProposeOfAPlanRunCarriesEveryPlanDocument(t *testing.T) {
	bare := projecttest.BareRepo(t, planDocs())
	cfg, checkout, base := checkoutOf(t, bare)
	for _, doc := range []string{"PRD.md", "ARCHITECTURE.md", "TASKS.md", "WAVES.md"} {
		writeIn(t, checkout, "docs/plans/search/"+doc, "# "+doc+" v2\n")
	}
	prop, err := cfg.ProposePaths(context.Background(), checkout, "yuno/plan", Base{Commit: base}, "Plan revision: search", []string{"docs/plans/search"})
	if err != nil || len(prop.LeftOut) != 0 {
		t.Fatalf("proposal: %v, left out %v", err, prop.LeftOut)
	}
	if got := projecttest.Git(t, bare, "diff", "--name-status", "main", "yuno/plan"); got != "M\tdocs/plans/search/ARCHITECTURE.md\n"+
		"M\tdocs/plans/search/PRD.md\nM\tdocs/plans/search/TASKS.md\nM\tdocs/plans/search/WAVES.md" {
		t.Fatalf("main..request = %q", got)
	}
}

// D-089 with D-088: a ticket's request branch from before the rule edited ARCHITECTURE.md,
// and so did main since. The run on the request starts with main merged in and the markers
// left in ARCHITECTURE.md; it edits PRD.md and its ticket file and resolves nothing in the
// plan documents. The push is the merge of the branch and main without a marker refusal:
// ARCHITECTURE.md takes main's version, PRD.md keeps the branch's, both are named as left
// out, and the request then merges into main without reverting main's documents.
func TestProposeWithAPendingMergeLeavesOutPlanDocumentsAtMainsVersion(t *testing.T) {
	files := planDocs()
	bare := projecttest.BareRepo(t, files)
	projecttest.Git(t, bare, "branch", "yuno/mr", "main")
	handCommit(t, bare, "yuno/mr", map[string]string{"docs/plans/search/ARCHITECTURE.md": "# Architecture, the branch's edit\n",
		"feature.txt": "feature\n"})
	mainHead := handCommit(t, bare, "main", map[string]string{"docs/plans/search/ARCHITECTURE.md": "# Architecture v2 (as built)\n",
		"later.txt": "later\n"})
	f := newRunRepoFixtureOn(t, bare, "yuno/mr")
	ctx := context.Background()
	checkout := f.checkoutPath("ws1")
	pm, err := f.cfg.MergeDefault(ctx, checkout, f.runRepo, "visit-1", f.fetched)
	if err != nil || pm.Merged != mainHead || !slices.Equal(pm.Conflicts, []string{"docs/plans/search/ARCHITECTURE.md"}) {
		t.Fatalf("merge = %+v %v", pm, err)
	}
	writeIn(t, checkout, "docs/plans/search/PRD.md", "# PRD, edited by a ticket\n")
	writeIn(t, checkout, "docs/plans/search/tickets/T8.md", "# T8 trace\n")
	writeIn(t, checkout, "feature.txt", "feature v2\n")

	base := Base{Commit: f.base, Merged: pm.Merged, Guard: true, Ticket: ticketT8}
	prop, err := f.cfg.ProposeOntoPaths(ctx, checkout, "yuno/mr", base, "Address the review", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(prop.LeftOut, []string{"docs/plans/search/ARCHITECTURE.md", "docs/plans/search/PRD.md"}) {
		t.Fatalf("left out = %v", prop.LeftOut)
	}
	if projecttest.Git(t, bare, "rev-parse", "yuno/mr^1") != f.base || projecttest.Git(t, bare, "rev-parse", "yuno/mr^2") != mainHead {
		t.Fatal("the push is not a merge of the branch and main")
	}
	at := func(p string) string { return projecttest.FileAt(t, bare, "yuno/mr", p) }
	if at("docs/plans/search/ARCHITECTURE.md") != "# Architecture v2 (as built)\n" || at("docs/plans/search/PRD.md") != "# PRD v1\n" ||
		at("docs/plans/search/tickets/T8.md") != "# T8 trace\n" || at("feature.txt") != "feature v2\n" || at("later.txt") != "later\n" {
		t.Fatalf("the branch: ARCHITECTURE %q, PRD %q, T8 %q, feature %q", at("docs/plans/search/ARCHITECTURE.md"),
			at("docs/plans/search/PRD.md"), at("docs/plans/search/tickets/T8.md"), at("feature.txt"))
	}
	if got := projecttest.Git(t, bare, "diff", "--name-status", "main", "yuno/mr"); got != "A\tdocs/plans/search/tickets/T8.md\nA\tfeature.txt" {
		t.Fatalf("main..request = %q", got)
	}
	work := mergeClone(t, bare, "main", "yuno/mr")
	if got, _ := os.ReadFile(filepath.Join(work, "docs/plans/search/ARCHITECTURE.md")); string(got) != "# Architecture v2 (as built)\n" {
		t.Fatalf("ARCHITECTURE.md after merging the request = %q", got)
	}
}
