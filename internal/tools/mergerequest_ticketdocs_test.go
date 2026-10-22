package tools

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"yuno/internal/project"
	"yuno/internal/project/projecttest"
	"yuno/internal/store"
)

// The tools half of DESIGN 5.28 rule 9 (D-089): open_merge_request in the run of a plan
// ticket carries, of the feature's plan documents, only the ticket's own file; a run that
// builds no ticket, such as the plan pipeline's, writes them all, and its documents go onto
// main as it is per path (D-087), so a revision meeting main's newer edits either merges or
// conflicts, and never overwrites.

var planDocNames = []string{"PRD.md", "ARCHITECTURE.md", "TASKS.md", "WAVES.md"}

// commitByHand commits files on branch of the bare repository from a clone of its own, the
// way another merge does, and returns the commit's id.
func commitByHand(t *testing.T, bare, branch string, files map[string]string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "hand")
	projecttest.Git(t, filepath.Dir(work), "clone", "--quiet", "--branch", branch, bare, work)
	for name, body := range files {
		p := filepath.Join(work, filepath.FromSlash(name))
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(body), 0o644))
	}
	projecttest.Git(t, work, "add", "--all")
	projecttest.Git(t, work, "commit", "--quiet", "-m", "by hand")
	projecttest.Git(t, work, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
	return projecttest.Git(t, work, "rev-parse", "HEAD")
}

// withPlanDocs puts the plan documents of the feature "search" on main and makes the run's
// checkout a fresh clone of main with them, recorded as its checkout_sha.
func (e *recordEnv) withPlanDocs() {
	e.t.Helper()
	files := map[string]string{}
	for _, doc := range planDocNames {
		files["docs/plans/search/"+doc] = "# " + doc + " v1\n"
	}
	head := commitByHand(e.t, e.bare, "main", files)
	must(e.t, os.RemoveAll(e.checkout))
	must(e.t, e.pc.Clone(context.Background(), e.checkout))
	must(e.t, store.SetRunCheckoutSHA(context.Background(), testPool, e.run.ID, head))
}

// buildsTicket makes the environment's run the run of ticket T8 of an open plan for the
// feature "search", as intake links it.
func (e *recordEnv) buildsTicket() {
	e.t.Helper()
	ctx := context.Background()
	plan := &store.Plan{ProjectID: e.project.ID, Feature: "search", PlanRunID: e.newRun(e.t).ID, Status: store.PlanBuilding}
	must(e.t, store.CreatePlan(ctx, testPool, plan))
	e.t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM plans WHERE id=$1`, plan.ID) })
	n := int64(8)
	must(e.t, store.UpsertPlanTicket(ctx, testPool, &store.PlanTicket{PlanID: plan.ID, TicketID: "T8", Wave: 2, Title: "T8", IssueNumber: &n}))
	if moved, err := store.LinkTicketRun(ctx, testPool, plan.ID, "T8", e.run.ID); err != nil || !moved {
		e.t.Fatalf("link the ticket: %v %v", moved, err)
	}
}

// T5.28.8: a ticket run that edited ARCHITECTURE.md and its ticket file besides code pushes
// the code and the ticket file only, and its answer names ARCHITECTURE.md as left out and
// says where such notes go; the ready call with the feature's directory as paths pushes the
// ticket file and leaves TASKS.md out too; a later call whose only change is withheld
// pushes nothing and says so.
func TestOpenMergeRequestTicketRunCarriesOnlyItsTicketFile(t *testing.T) {
	e := newRecordEnv(t, project.ProviderGitLab)
	e.withPlanDocs()
	e.buildsTicket()
	e.write("search.go", "package search\n")
	e.write("docs/plans/search/ARCHITECTURE.md", "# ARCHITECTURE.md, as T8 built it\n")
	e.write("docs/plans/search/tickets/T8.md", "# T8 trace\n")

	out, got, err := e.openWith(e.key('a'), "T8: search", "the description", nil, nil)
	if err != nil || out.Result.IsError {
		t.Fatalf("draft call: %+v %v", out, err)
	}
	if !slices.Equal(got.LeftOut, []string{"docs/plans/search/ARCHITECTURE.md"}) {
		t.Fatalf("left out = %v", got.LeftOut)
	}
	for _, want := range []string{"Left out of the merge request: docs/plans/search/ARCHITECTURE.md.", "docs/plans/search/tickets/T8.md",
		`"## Notes for the plan documents"`, "the wave's ship run applies them"} {
		if !strings.Contains(got.Notice, want) {
			t.Errorf("notice lacks %q: %s", want, got.Notice)
		}
	}
	if diff := projecttest.Git(t, e.bare, "diff", "--name-status", "main", got.Branch); diff != "A\tdocs/plans/search/tickets/T8.md\nA\tsearch.go" {
		t.Fatalf("main..request = %q", diff)
	}

	// The ready call: the feature's directory as paths.
	e.write("docs/plans/search/tickets/T8.md", "# T8 trace\n\n## Notes for the plan documents\n- ARCHITECTURE.md §2: the index is built at start\n")
	e.write("docs/plans/search/TASKS.md", "# TASKS.md, T8 done\n")
	out, ready, err := e.openWith(e.key('b'), "T8: search", "the description", nil, []string{"docs/plans/search"})
	if err != nil || out.Result.IsError || !ready.Updated || !ready.Replayed {
		t.Fatalf("ready call: %+v %+v %v", out, ready, err)
	}
	if !slices.Equal(ready.LeftOut, []string{"docs/plans/search/ARCHITECTURE.md", "docs/plans/search/TASKS.md"}) {
		t.Fatalf("left out = %v", ready.LeftOut)
	}
	at := func(p string) string { return projecttest.FileAt(t, e.bare, got.Branch, p) }
	if !strings.Contains(at("docs/plans/search/tickets/T8.md"), "## Notes for the plan documents") ||
		at("docs/plans/search/ARCHITECTURE.md") != "# ARCHITECTURE.md v1\n" || at("docs/plans/search/TASKS.md") != "# TASKS.md v1\n" {
		t.Fatalf("the request: T8 %q, ARCHITECTURE %q, TASKS %q", at("docs/plans/search/tickets/T8.md"),
			at("docs/plans/search/ARCHITECTURE.md"), at("docs/plans/search/TASKS.md"))
	}

	// Only a withheld document changed since: nothing is pushed, and the answer still says so.
	tip := e.tip(got.Branch)
	e.write("docs/plans/search/ARCHITECTURE.md", "# ARCHITECTURE.md, edited again\n")
	out, again, err := e.openWith(e.key('c'), "T8: search", "the description", nil, []string{"docs/plans/search"})
	if err != nil || out.Result.IsError || again.Updated || !again.Replayed || e.tip(got.Branch) != tip ||
		!slices.Equal(again.LeftOut, []string{"docs/plans/search/ARCHITECTURE.md", "docs/plans/search/TASKS.md"}) {
		t.Fatalf("withheld only: %+v %+v %v", out, again, err)
	}
	if e.requests() != 1 || e.records() != 1 {
		t.Fatalf("requests %d, records %d", e.requests(), e.records())
	}
}

// A ticket run whose only changes are to plan documents it does not own opens nothing: the
// model error names them and where the notes go, and the call's key is free again.
func TestOpenMergeRequestTicketRunWithOnlyWithheldChanges(t *testing.T) {
	e := newRecordEnv(t, project.ProviderGitLab)
	e.withPlanDocs()
	e.buildsTicket()
	e.write("docs/plans/search/PRD.md", "# PRD.md, edited by a ticket\n")
	out, _, err := e.openWith(e.key('a'), "T8: search", "the description", nil, nil)
	if err != nil || !out.Result.IsError {
		t.Fatalf("call: %+v %v", out, err)
	}
	for _, want := range []string{"no changes to propose besides plan documents a ticket run does not change; nothing was pushed",
		"Left out of the merge request: docs/plans/search/PRD.md.", "docs/plans/search/tickets/T8.md"} {
		if !strings.Contains(out.Result.Text, want) {
			t.Errorf("text lacks %q: %s", want, out.Result.Text)
		}
	}
	if e.requests() != 0 || e.records() != 0 || slices.Contains(projecttest.Branches(t, e.bare), GitLabBranch(e.key('a'))) {
		t.Fatal("a call with only withheld changes pushed or opened something")
	}
}

// T5.28.8: a run that builds no ticket, as the plan pipeline's integrator, still writes all
// four plan documents.
func TestOpenMergeRequestPlanRunWritesEveryPlanDocument(t *testing.T) {
	e := newRecordEnv(t, project.ProviderGitLab)
	e.withPlanDocs()
	for _, doc := range planDocNames {
		e.write("docs/plans/search/"+doc, "# "+doc+" v2\n")
	}
	out, got, err := e.openWith(e.key('a'), "Plan: search", "the description", nil, []string{"docs/plans/search"})
	if err != nil || out.Result.IsError || len(got.LeftOut) != 0 || got.Notice != "" {
		t.Fatalf("call: %+v %+v %v", out, got, err)
	}
	if diff := projecttest.Git(t, e.bare, "diff", "--name-status", "main", got.Branch); diff != "M\tdocs/plans/search/ARCHITECTURE.md\n"+
		"M\tdocs/plans/search/PRD.md\nM\tdocs/plans/search/TASKS.md\nM\tdocs/plans/search/WAVES.md" {
		t.Fatalf("main..request = %q", diff)
	}
}

// T5.29.16: a plan revision whose PRD and ARCHITECTURE changes meet main's newer
// ARCHITECTURE.md (a wave's ship run applied its notes while the revision ran) is a conflict
// that names ARCHITECTURE.md and pushes nothing; once the revision leaves ARCHITECTURE.md
// alone, its PRD and WAVES changes merge onto main as it is and main's ARCHITECTURE.md stays.
func TestOpenMergeRequestRevisionMeetsMainsNewerDocuments(t *testing.T) {
	e := newRecordEnv(t, project.ProviderGitLab)
	e.withPlanDocs()
	mainHead := commitByHand(t, e.bare, "main", map[string]string{"docs/plans/search/ARCHITECTURE.md": "# ARCHITECTURE.md, wave 5 as built\n"})
	e.write("docs/plans/search/PRD.md", "# PRD.md, revised\n")
	e.write("docs/plans/search/ARCHITECTURE.md", "# ARCHITECTURE.md, revised\n")

	branches := projecttest.Branches(t, e.bare)
	out, _, err := e.openWith(e.key('a'), "Plan revision: search", "the description", nil, []string{"docs/plans/search"})
	const text = "these files conflict with changes on main made since this run's checkout: docs/plans/search/ARCHITECTURE.md. " +
		"Nothing was pushed. Ask the operator how to resolve it, or finish and report the conflict"
	if err != nil || !out.Result.IsError || out.Result.Text != text {
		t.Fatalf("conflict: %+v %v", out, err)
	}
	if !slices.Equal(projecttest.Branches(t, e.bare), branches) || e.requests() != 0 || e.tip("main") != mainHead {
		t.Fatal("a conflicting revision pushed or opened something")
	}

	e.write("docs/plans/search/ARCHITECTURE.md", "# ARCHITECTURE.md v1\n")
	e.write("docs/plans/search/WAVES.md", "# WAVES.md, revised\n")
	out, got, err := e.openWith(e.key('b'), "Plan revision: search", "the description", nil, []string{"docs/plans/search"})
	if err != nil || out.Result.IsError {
		t.Fatalf("revision: %+v %v", out, err)
	}
	if diff := projecttest.Git(t, e.bare, "diff", "--name-status", "main", got.Branch); diff != "M\tdocs/plans/search/PRD.md\nM\tdocs/plans/search/WAVES.md" {
		t.Fatalf("main..request = %q", diff)
	}
	if a := projecttest.FileAt(t, e.bare, got.Branch, "docs/plans/search/ARCHITECTURE.md"); a != "# ARCHITECTURE.md, wave 5 as built\n" {
		t.Fatalf("the revision overwrote main's ARCHITECTURE.md: %q", a)
	}
}
