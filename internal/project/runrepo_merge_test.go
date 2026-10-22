package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"yuno/internal/project/projecttest"
)

// DESIGN 5.30 rule 10 and 5.22 rule 13 (D-088) against local bare repositories: a run's
// checkout gets its branch's and the default branch's history from the run repository, a
// run on a request starts with the default branch merged in, and its push is a real merge.

// withHistoryDepth lowers the fetch depths for one test.
func withHistoryDepth(t *testing.T, depth, limit int) {
	t.Helper()
	d, l := historyDepth, maxHistoryDepth
	historyDepth, maxHistoryDepth = depth, limit
	t.Cleanup(func() { historyDepth, maxHistoryDepth = d, l })
}

// mergeClone merges branch into into in a clone of the bare repository, as the provider does
// when the request is merged (a conflict fails the test), and returns the clone.
func mergeClone(t *testing.T, bare, into, branch string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "merge")
	projecttest.Git(t, filepath.Dir(work), "clone", "--quiet", "--branch", into, bare, work)
	projecttest.Git(t, work, "merge", "--quiet", "--no-ff", "--no-edit", "origin/"+branch)
	return work
}

// conflictingRequest is a bare repository whose request branch yuno/mr and main both
// changed app.txt since the branch was made, besides a file each; it returns main's head.
func conflictingRequest(t *testing.T, seed map[string]string) (string, string) {
	t.Helper()
	files := map[string]string{"README.md": "# demo\n", "app.txt": "line\n"}
	for k, v := range seed {
		files[k] = v
	}
	bare := projecttest.BareRepo(t, files)
	projecttest.Git(t, bare, "branch", "yuno/mr", "main")
	handCommit(t, bare, "yuno/mr", map[string]string{"app.txt": "the branch's line\n", "feature.txt": "feature\n"})
	return bare, handCommit(t, bare, "main", map[string]string{"app.txt": "main's line\n", "later.txt": "later\n"})
}

// The fetch deepens until the request's branch and main share history, and every checkout
// built from the run repository has origin/main with that history, the branch's own, and
// no credential; a run on main gets main's history.
func TestFetchRunGivesTheCheckoutHistory(t *testing.T) {
	withHistoryDepth(t, 2, 20)
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n"})
	for i := range 3 {
		handCommit(t, bare, "main", map[string]string{"m" + strconv.Itoa(i) + ".txt": "m\n"})
	}
	fork := projecttest.Git(t, bare, "rev-parse", "main")
	projecttest.Git(t, bare, "branch", "yuno/mr", "main")
	for i := range 3 {
		handCommit(t, bare, "yuno/mr", map[string]string{"b" + strconv.Itoa(i) + ".txt": "b\n"})
		handCommit(t, bare, "main", map[string]string{"later" + strconv.Itoa(i) + ".txt": "l\n"})
	}
	mainHead, branchHead := projecttest.Git(t, bare, "rev-parse", "main"), projecttest.Git(t, bare, "rev-parse", "yuno/mr")
	f := newRunRepoFixtureOn(t, bare, "yuno/mr")
	if f.fetched != (Fetched{Base: branchHead, Default: mainHead, MergeBase: fork}) {
		t.Fatalf("fetched = %+v, want base %s, default %s, merge base %s", f.fetched, branchHead, mainHead, fork)
	}
	ctx := context.Background()
	check := func(checkout string) {
		t.Helper()
		if got := projecttest.Git(t, checkout, "rev-parse", "origin/main"); got != mainHead {
			t.Errorf("origin/main = %s, want %s", got, mainHead)
		}
		if got := projecttest.Git(t, checkout, "merge-base", "HEAD", "origin/main"); got != fork {
			t.Errorf("merge base in the checkout = %s, want %s", got, fork)
		}
		if n, _ := strconv.Atoi(projecttest.Git(t, checkout, "rev-list", "--count", "origin/main")); n < 4 {
			t.Errorf("origin/main has %d commits of history", n)
		}
		if got := projecttest.Git(t, checkout, "for-each-ref", "--format=%(refname:short)", "refs/remotes/"); got != "origin/main\norigin/yuno/mr" {
			t.Errorf("remote branches = %q", got)
		}
		cfg, _ := os.ReadFile(filepath.Join(checkout, ".git", "config"))
		if strings.Contains(string(cfg), f.cfg.Token) || strings.Contains(string(cfg), "extraheader") || !strings.Contains(string(cfg), f.cfg.RepoURL) {
			t.Errorf(".git/config:\n%s", cfg)
		}
	}
	check(f.checkoutPath("ws1"))
	// A later step's checkout, built from a commit of the first one, has the same.
	writeIn(t, f.checkoutPath("ws1"), "work.txt", "work\n")
	one := f.commit(t, "ws1", f.base, "visit-1", "one visit 1")
	two := f.checkoutPath("ws2")
	must(t, os.MkdirAll(filepath.Dir(two), 0o755))
	must(t, f.cfg.CheckoutRun(ctx, f.runRepo, one, two, "yuno/mr"))
	check(two)
	if cfg, _ := os.ReadFile(filepath.Join(f.runRepo, "config")); strings.Contains(string(cfg), f.cfg.Token) || strings.Contains(string(cfg), "extraheader") {
		t.Fatalf("the run repository's config holds a credential:\n%s", cfg)
	}

	// A run on main: main's history, no second branch.
	onMain := newRunRepoFixtureOn(t, bare, "main")
	if onMain.fetched != (Fetched{Base: mainHead}) {
		t.Fatalf("fetched on main = %+v", onMain.fetched)
	}
	if n, _ := strconv.Atoi(projecttest.Git(t, onMain.checkoutPath("ws1"), "rev-list", "--count", "HEAD")); n != 2 {
		t.Fatalf("a run on main has %d commits of history, want the depth 2", n)
	}

	// Past the limit there is no merge base, and the branch is not merged.
	withHistoryDepth(t, 1, 2)
	capped := newRunRepoFixtureOn(t, bare, "yuno/mr")
	if capped.fetched.MergeBase != "" {
		t.Fatalf("merge base past the limit: %+v", capped.fetched)
	}
	if _, err := capped.cfg.MergeDefault(ctx, capped.checkoutPath("ws1"), capped.runRepo, "visit-1", capped.fetched); !errors.Is(err, ErrNoMergeBase) {
		t.Fatalf("merge without a merge base: %v", err)
	}
}

// The acceptance test of D-088: the request's branch and main changed the same line. The
// platform's merge leaves the markers and names the file; the push refuses while a marker
// is left and refuses paths; the agent's resolution, a file edit, is pushed as a merge
// commit of the branch and main, and the branch then merges into main without a conflict.
func TestMergeDefaultConflictThenThePush(t *testing.T) {
	bare, mainHead := conflictingRequest(t, nil)
	f := newRunRepoFixtureOn(t, bare, "yuno/mr")
	ctx := context.Background()
	checkout := f.checkoutPath("ws1")
	pm, err := f.cfg.MergeDefault(ctx, checkout, f.runRepo, "visit-1", f.fetched)
	if err != nil || pm.Merged != mainHead || pm.Commit != "" || !slices.Equal(pm.Conflicts, []string{"app.txt"}) {
		t.Fatalf("merge = %+v %v", pm, err)
	}
	app, _ := os.ReadFile(filepath.Join(checkout, "app.txt"))
	for _, want := range []string{"<<<<<<< ", "the branch's line", "main's line", ">>>>>>> "} {
		if !strings.Contains(string(app), want) {
			t.Fatalf("app.txt lacks %q:\n%s", want, app)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(checkout, "later.txt")); string(got) != "later\n" {
		t.Fatalf("main's other file was not merged: %q", got)
	}
	if got := projecttest.Git(t, checkout, "rev-parse", "MERGE_HEAD"); got != mainHead {
		t.Fatalf("MERGE_HEAD = %s", got)
	}

	base := Base{Commit: f.base, Merged: pm.Merged, Guard: true}
	_, err = f.cfg.ProposeOntoPaths(ctx, checkout, "yuno/mr", base, "Resolve", nil)
	var markers *ErrMarkers
	if !errors.As(err, &markers) || !slices.Equal(markers.Paths, []string{"app.txt"}) ||
		err.Error() != "these files still have conflict markers: app.txt. Resolve them, then propose again; nothing was pushed" {
		t.Fatalf("unresolved markers: %v", err)
	}
	if _, err := f.cfg.ProposeOntoPaths(ctx, checkout, "yuno/mr", base, "Resolve", []string{"app.txt"}); err == nil || !strings.Contains(err.Error(), "without paths") {
		t.Fatalf("paths with a pending merge: %v", err)
	}
	if got := projecttest.Git(t, bare, "rev-parse", "yuno/mr"); got != f.base {
		t.Fatal("a refused proposal moved the branch")
	}

	writeIn(t, checkout, "app.txt", "both lines\n")
	prop, err := f.cfg.ProposeOntoPaths(ctx, checkout, "yuno/mr", base, "Merge main and keep both lines", nil)
	if err != nil {
		t.Fatal(err)
	}
	if projecttest.Git(t, bare, "rev-parse", "yuno/mr") != prop.Commit || prop.Base != prop.Commit ||
		projecttest.Git(t, bare, "rev-parse", "yuno/mr^1") != f.base || projecttest.Git(t, bare, "rev-parse", "yuno/mr^2") != mainHead {
		t.Fatalf("proposal %+v: the branch is not a merge of its head %s and main %s", prop, f.base, mainHead)
	}
	at := func(p string) string { return projecttest.FileAt(t, bare, "yuno/mr", p) }
	if at("app.txt") != "both lines\n" || at("later.txt") != "later\n" || at("feature.txt") != "feature\n" {
		t.Fatalf("the branch: app %q, later %q, feature %q", at("app.txt"), at("later.txt"), at("feature.txt"))
	}
	if got := projecttest.Git(t, bare, "diff", "--name-status", "main", "yuno/mr"); got != "M\tapp.txt\nA\tfeature.txt" {
		t.Fatalf("main..request = %q", got)
	}
	work := mergeClone(t, bare, "main", "yuno/mr")
	if got, _ := os.ReadFile(filepath.Join(work, "app.txt")); string(got) != "both lines\n" {
		t.Fatalf("app.txt after merging the request = %q", got)
	}
}

// A clean merge is a merge commit in the checkout, recorded in the run repository, and a
// later step's checkout starts from it, so only the agents' changes show; a branch that has
// main already gets nothing merged.
func TestMergeDefaultCleanStartsLaterCheckoutsFromTheMerge(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n"})
	projecttest.Git(t, bare, "branch", "yuno/mr", "main")
	handCommit(t, bare, "yuno/mr", map[string]string{"feature.txt": "feature\n"})
	mainHead := handCommit(t, bare, "main", map[string]string{"later.txt": "later\n"})
	f := newRunRepoFixtureOn(t, bare, "yuno/mr")
	ctx := context.Background()
	checkout := f.checkoutPath("ws1")
	pm, err := f.cfg.MergeDefault(ctx, checkout, f.runRepo, "visit-1", f.fetched)
	if err != nil || pm.Merged != mainHead || pm.Commit == "" || len(pm.Conflicts) != 0 {
		t.Fatalf("merge = %+v %v", pm, err)
	}
	if projecttest.Git(t, checkout, "rev-parse", "HEAD") != pm.Commit || projecttest.Git(t, checkout, "rev-parse", "HEAD^1") != f.base ||
		projecttest.Git(t, checkout, "rev-parse", "HEAD^2") != mainHead || projecttest.Git(t, checkout, "status", "--porcelain") != "" {
		t.Fatal("the checkout is not at a clean merge of the branch and main")
	}
	if got := projecttest.Git(t, f.runRepo, "rev-parse", MergeRef("visit-1")); got != pm.Commit {
		t.Fatalf("merge ref = %s, want %s", got, pm.Commit)
	}

	writeIn(t, checkout, "feature.txt", "feature v2\n")
	one := f.commit(t, "ws1", pm.Commit, "visit-1", "one visit 1")
	two := f.checkoutPath("ws2")
	must(t, os.MkdirAll(filepath.Dir(two), 0o755))
	must(t, f.cfg.CheckoutRun(ctx, f.runRepo, one, two, "yuno/mr"))
	if projecttest.Git(t, two, "rev-parse", "HEAD") != pm.Commit || projecttest.Git(t, two, "rev-parse", "origin/yuno/mr") != f.base ||
		projecttest.Git(t, two, "rev-parse", "origin/main") != mainHead {
		t.Fatal("the later checkout does not start from the merge")
	}
	if got := projecttest.Git(t, two, "diff", "--name-only"); got != "feature.txt" {
		t.Fatalf("the later checkout's changes = %q, want the agent's edit only", got)
	}
	if got, err := ConflictMarkers(ctx, f.runRepo, one); err != nil || len(got) != 0 {
		t.Fatalf("markers after a clean merge: %v %v", got, err)
	}

	// The branch has main's head already: nothing to merge.
	ahead := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n"})
	projecttest.Git(t, ahead, "branch", "yuno/mr", "main")
	handCommit(t, ahead, "yuno/mr", map[string]string{"feature.txt": "feature\n"})
	g := newRunRepoFixtureOn(t, ahead, "yuno/mr")
	if pm, err := g.cfg.MergeDefault(ctx, g.checkoutPath("ws1"), g.runRepo, "visit-1", g.fetched); err != nil || pm.Merged != "" || pm.Commit != "" {
		t.Fatalf("a branch ahead of main: %+v %v", pm, err)
	}
}

// A later step learns which files still hold the merge's conflict markers from the run
// repository: the conflicted file until it is resolved, never a file the run did not
// change even when it holds marker-like lines.
func TestConflictMarkersOfALaterCommit(t *testing.T) {
	bare, _ := conflictingRequest(t, map[string]string{"docs/git.md": "<<<<<<< HEAD\nan example\n>>>>>>> other\n"})
	f := newRunRepoFixtureOn(t, bare, "yuno/mr")
	ctx := context.Background()
	if _, err := f.cfg.MergeDefault(ctx, f.checkoutPath("ws1"), f.runRepo, "visit-1", f.fetched); err != nil {
		t.Fatal(err)
	}
	one := f.commit(t, "ws1", f.base, "visit-1", "one visit 1")
	if got, err := ConflictMarkers(ctx, f.runRepo, one); err != nil || !slices.Equal(got, []string{"app.txt"}) {
		t.Fatalf("markers = %v %v", got, err)
	}
	two := f.checkoutPath("ws2")
	must(t, os.MkdirAll(filepath.Dir(two), 0o755))
	must(t, f.cfg.CheckoutRun(ctx, f.runRepo, one, two, "yuno/mr"))
	writeIn(t, two, "app.txt", "both lines\n")
	second := f.commit(t, "ws2", one, "visit-2", "two visit 1")
	if got, err := ConflictMarkers(ctx, f.runRepo, second); err != nil || len(got) != 0 {
		t.Fatalf("markers after the resolution = %v %v", got, err)
	}
}
