package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"testing"

	"yuno/internal/project"
	"yuno/internal/project/projecttest"
	"yuno/internal/store"
)

// The tools half of DESIGN 5.16 rule 10 (D-087): open_merge_request counts the run's
// changes from its checkout_sha, the commit its checkout was cloned at.

// T5.16.9: the default branch gains a file after the run's clone (another ticket merged);
// the run's new request keeps it, changes only what the run changed, and the run's own
// commit becomes its checkout_sha. A file both changed is a model error that names it:
// nothing is pushed, no request is opened and the key is free again.
func TestOpenMergeRequestKeepsWhatMainGained(t *testing.T) {
	for _, provider := range []project.Provider{project.ProviderGitLab, project.ProviderGitHub} {
		t.Run(string(provider), func(t *testing.T) {
			e := newRecordEnv(t, provider)
			ctx := context.Background()
			base := e.tip("main") // the environment's checkout is a clone of main
			must(t, store.SetRunCheckoutSHA(ctx, testPool, e.run.ID, base))
			t2 := pushByHand(t, e.bare, "main", "t2.txt", "ticket two\n")

			e.edit("README.md", "# demo\nT3\n")
			out, got, err := e.open(e.key('a'), "T3")
			if err != nil || out.Result.IsError {
				t.Fatalf("open: %+v %v", out, err)
			}
			at := func(p string) string { return projecttest.FileAt(t, e.bare, got.Branch, p) }
			if at("t2.txt") != "ticket two\n" || at("keep/old.txt") != "old\n" || at("README.md") != "# demo\nT3\n" {
				t.Fatalf("the request: t2.txt %q, keep/old.txt %q, README %q", at("t2.txt"), at("keep/old.txt"), at("README.md"))
			}
			if diff := projecttest.Git(t, e.bare, "diff", "--name-status", "main", got.Branch); diff != "M\tREADME.md" {
				t.Fatalf("main..request = %q", diff)
			}
			if projecttest.Git(t, e.bare, "rev-parse", got.Branch+"^1") != t2 || e.checkoutSHA(e.run) != projecttest.Git(t, e.bare, "rev-parse", got.Branch+"^2") {
				t.Fatalf("tip %s, checkout_sha %s", e.tip(got.Branch), e.checkoutSHA(e.run))
			}

			// Another run changes README.md while main changed it too.
			e2 := newRecordEnv(t, provider)
			must(t, store.SetRunCheckoutSHA(ctx, testPool, e2.run.ID, e2.tip("main")))
			pushByHand(t, e2.bare, "main", "README.md", "# demo\nfrom main\n")
			e2.edit("README.md", "# demo\nfrom the run\n")
			branches := projecttest.Branches(t, e2.bare)
			out, _, err = e2.open(e2.key('a'), "Clash")
			const text = "these files conflict with changes on main made since this run's checkout: README.md. Nothing was pushed. " +
				"Ask the operator how to resolve it, or finish and report the conflict"
			if err != nil || !out.Result.IsError || out.Result.Text != text {
				t.Fatalf("conflict: %+v %v", out, err)
			}
			if !slices.Equal(projecttest.Branches(t, e2.bare), branches) || e2.requests() != 0 || e2.records() != 0 {
				t.Fatal("a conflicting call pushed or opened something")
			}
			sum := sha256.Sum256([]byte(e2.run.ID + "a"))
			if _, err := store.GetSideEffect(ctx, testPool, hex.EncodeToString(sum[:])); err == nil {
				t.Fatal("a conflicting call left its side effect")
			}
		})
	}
}
