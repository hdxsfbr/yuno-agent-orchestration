package project

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"yuno/internal/project/projecttest"
)

const runRepoRunID = "0199f0a2-0000-7000-8000-0000000000aa"

// runRepoFixture is a project whose remote is a local bare repository reached over
// file:// (so the clone is shallow at depth 1, as over https), a state directory, and the
// first step's clone in its workspace with the run repository holding its commit.
type runRepoFixture struct {
	cfg     Config
	state   string
	runRepo string
	base    string
}

func newRunRepoFixture(t *testing.T, files map[string]string) *runRepoFixture {
	t.Helper()
	bare := projecttest.BareRepo(t, files)
	f := &runRepoFixture{cfg: Config{RepoURL: "file://" + bare, Token: "glpat-not-a-real-token-0", DefaultBranch: "main", Provider: ProviderGitLab},
		state: t.TempDir()}
	f.runRepo = RunRepoPath(f.state, runRepoRunID)
	ctx := context.Background()
	repo := f.checkoutPath("ws1")
	must(t, os.MkdirAll(filepath.Dir(repo), 0o755))
	must(t, f.cfg.Clone(ctx, repo))
	var err error
	if f.base, err = f.cfg.HeadSHA(ctx, repo); err != nil {
		t.Fatal(err)
	}
	must(t, InitRunRepo(ctx, f.runRepo))
	must(t, SaveRunBase(ctx, f.runRepo, repo, BaseRef("visit-1")))
	return f
}

// checkoutPath is runs/<run>/repo of the workspace ws under the fixture's temporary root.
func (f *runRepoFixture) checkoutPath(ws string) string {
	return filepath.Join(f.state, ws, RepoRel(runRepoRunID))
}

func (f *runRepoFixture) commit(t *testing.T, ws, parent, visitID, message string) string {
	t.Helper()
	root, err := OpenCheckout(filepath.Join(f.state, ws), runRepoRunID)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	sha, err := CommitCheckout(context.Background(), f.runRepo, root, parent, VisitRef(visitID), message)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func writeAt(t *testing.T, path, content string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, []byte(content), 0o644))
}

// DESIGN 5.30 rule 4: a visit's checkout goes into the run repository as one commit on
// the commit it started from, made from a copy of its working tree (the excluded
// directories, links and .git left out, ignored files kept, nothing the agent planted in
// .git run), and the next visit's checkout built from that commit shows the change as
// changes against the branch, with origin the project's URL and no credential; it holds
// nothing of another visit's commit.
func TestRunRepositoryCarriesTheCheckout(t *testing.T) {
	f := newRunRepoFixture(t, map[string]string{"README.md": "# demo\n", "keep/old.txt": "old\n", ".gitignore": "build.out\n"})
	ctx := context.Background()
	if got := strings.TrimSpace(projecttest.Git(t, f.runRepo, "rev-parse", BaseRef("visit-1"))); got != f.base {
		t.Fatalf("base ref = %s, want %s", got, f.base)
	}
	one := f.checkoutPath("ws1")
	writeAt(t, filepath.Join(one, "README.md"), "# demo\nedited by one\n")
	writeAt(t, filepath.Join(one, "src", "new.txt"), "new\n")
	must(t, os.RemoveAll(filepath.Join(one, "keep")))
	writeAt(t, filepath.Join(one, "node_modules", "dep", "index.js"), "x\n")
	writeAt(t, filepath.Join(one, "build.out"), "built\n")
	writeAt(t, filepath.Join(one, "sub", ".git"), "gitdir: "+f.runRepo+"\n")
	must(t, os.Symlink("/etc/passwd", filepath.Join(one, "link")))
	marker := filepath.Join(t.TempDir(), "ran")
	must(t, os.WriteFile(filepath.Join(one, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\necho x > "+marker+"\n"), 0o755))
	must(t, os.WriteFile(filepath.Join(one, ".git", "config"),
		[]byte("[core]\n\tfsmonitor = echo x > "+marker+" ; true\n\thooksPath = "+filepath.Join(one, ".git", "hooks")+"\n"), 0o644))

	sha := f.commit(t, "ws1", f.base, "visit-1", "one visit 1")
	if sha == f.base {
		t.Fatal("no commit for a changed checkout")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("something planted in the checkout's .git ran")
	}
	if got := projecttest.Git(t, f.runRepo, "rev-parse", VisitRef("visit-1")); got != sha {
		t.Fatalf("visit ref = %s, want %s", got, sha)
	}
	if got := projecttest.Git(t, f.runRepo, "log", "-1", "--format=%an <%ae>|%s|%P", sha); got != "Yuno <yuno@localhost>|one visit 1|"+f.base {
		t.Fatalf("commit = %s", got)
	}
	files := strings.Split(projecttest.Git(t, f.runRepo, "ls-tree", "-r", "--name-only", sha), "\n")
	if want := []string{".gitignore", "README.md", "build.out", "src/new.txt"}; !slices.Equal(files, want) {
		t.Fatalf("committed files = %v, want %v", files, want)
	}

	// Nothing changed since: no new commit, the visit's ref names the parent.
	if again := f.commit(t, "ws1", sha, "visit-2", "two visit 1"); again != sha {
		t.Fatalf("an unchanged checkout committed %s", again)
	}
	if got := projecttest.Git(t, f.runRepo, "rev-parse", VisitRef("visit-2")); got != sha {
		t.Fatalf("visit-2 ref = %s", got)
	}

	// Another visit's commit, which the next checkout must not receive.
	other := f.checkoutPath("ws-other")
	must(t, os.MkdirAll(filepath.Dir(other), 0o755))
	must(t, f.cfg.CheckoutRun(ctx, f.runRepo, f.base, other, "main"))
	writeAt(t, filepath.Join(other, "sibling.txt"), "a sibling's work\n")
	sibling := f.commit(t, "ws-other", f.base, "visit-3", "b visit 1")

	two := f.checkoutPath("ws2")
	must(t, os.MkdirAll(filepath.Dir(two), 0o755))
	must(t, f.cfg.CheckoutRun(ctx, f.runRepo, sha, two, "main"))
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(two, rel))
		if err != nil {
			return "missing"
		}
		return string(b)
	}
	for rel, want := range map[string]string{"README.md": "# demo\nedited by one\n", "src/new.txt": "new\n", "build.out": "built\n",
		"keep/old.txt": "missing", "node_modules/dep/index.js": "missing", "sibling.txt": "missing", "link": "missing"} {
		if got := read(rel); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
	// The run's changes are in the working tree, not staged, not committed.
	if got := projecttest.Git(t, two, "diff", "--name-status"); got != "M\tREADME.md\nD\tkeep/old.txt" {
		t.Errorf("unstaged changes:\n%s", got)
	}
	if got := projecttest.Git(t, two, "diff", "--cached", "--name-only"); got != "" {
		t.Errorf("staged changes:\n%s", got)
	}
	if got := projecttest.Git(t, two, "ls-files", "--others", "--exclude-standard"); got != "src/new.txt" {
		t.Errorf("untracked files:\n%s", got)
	}
	if diff := projecttest.Git(t, two, "diff", "main", "--stat"); !strings.Contains(diff, "README.md") || !strings.Contains(diff, "keep/old.txt") {
		t.Errorf("git diff main:\n%s", diff)
	}
	if head := projecttest.Git(t, two, "symbolic-ref", "HEAD"); head != "refs/heads/main" {
		t.Errorf("HEAD = %s", head)
	}
	for _, ref := range []string{"main", "origin/main"} {
		if got := projecttest.Git(t, two, "rev-parse", ref); got != f.base {
			t.Errorf("%s = %s, want the cloned commit %s", ref, got, f.base)
		}
	}
	if url := projecttest.Git(t, two, "config", "remote.origin.url"); url != f.cfg.RepoURL {
		t.Errorf("origin = %s", url)
	}
	cfg, _ := os.ReadFile(filepath.Join(two, ".git", "config"))
	if strings.Contains(string(cfg), f.cfg.Token) || strings.Contains(string(cfg), "extraheader") {
		t.Errorf(".git/config:\n%s", cfg)
	}
	if ex, _ := os.ReadFile(filepath.Join(two, ".git", "info", "exclude")); !strings.Contains(string(ex), "node_modules/") {
		t.Errorf("exclude file:\n%s", ex)
	}
	if out, err := (Config{}).git(ctx, two, "cat-file", "-e", sibling); err == nil {
		t.Errorf("the checkout holds another visit's commit %s (%s)", sibling, out)
	}
}

// The run repository is made once, also when two visits of a run make it at once (the
// heads of a fork); a commit that is not a full id is refused before git runs.
func TestRunRepositoryInitAndRefusals(t *testing.T) {
	state := t.TempDir()
	dir := RunRepoPath(state, runRepoRunID)
	if want := filepath.Join(state, "yuno-runs", runRepoRunID+".git"); dir != want {
		t.Fatalf("RunRepoPath = %s", dir)
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = InitRunRepo(ctx, dir)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("init %d: %v", i, err)
		}
	}
	if got := projecttest.Git(t, dir, "config", "uploadpack.allowAnySHA1InWant"); got != "true" {
		t.Fatalf("allowAnySHA1InWant = %q", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(dir))
	if len(entries) != 1 {
		t.Fatalf("yuno-runs holds %d entries, want the repository only", len(entries))
	}
	if err := (Config{}).CheckoutRun(ctx, dir, "HEAD", filepath.Join(t.TempDir(), "repo"), "main"); err == nil {
		t.Fatal("a commit that is not a full id was accepted")
	}
	ws := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(ws, "runs"), 0o755))
	must(t, os.Symlink(t.TempDir(), filepath.Join(ws, "runs", runRepoRunID)))
	if _, err := OpenCheckout(ws, runRepoRunID); err == nil {
		t.Fatal("OpenCheckout went through a link")
	}
}
